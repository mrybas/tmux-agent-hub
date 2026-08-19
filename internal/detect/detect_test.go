package detect

import (
	"strings"
	"testing"

	"github.com/mrybas/tmux-agent-hub/internal/transcript"
)

func call(tool, arg, result string) transcript.ToolCall {
	// Input carries the full arguments in real transcripts; the detectors
	// hash it, so tests must populate it too.
	return transcript.ToolCall{Tool: tool, Arg: arg, Input: arg, Result: result}
}

// failing is a call the transcript reported as failed. Whether a call
// failed is decided by the reader, never by scanning its output.
func failing(tool, arg, result string) transcript.ToolCall {
	c := call(tool, arg, result)
	c.IsError = true
	return c
}

func kinds(t *testing.T, calls []transcript.ToolCall) string {
	t.Helper()
	if f := Analyze(calls, DefaultThresholds()); f != nil {
		return f.Kind
	}
	return ""
}

// The healthy cases matter most: a false positive costs the user's trust.
func TestHealthySessionsAreSilent(t *testing.T) {
	cases := map[string][]transcript.ToolCall{
		"edit then test, passing": {
			call("Read", "/src/app.go", "package app"),
			call("Edit", "/src/app.go", "ok"),
			call("Bash", "go test ./...", "ok\nPASS"),
			call("Edit", "/src/app_test.go", "ok"),
			call("Bash", "go test ./...", "ok\nPASS"),
		},
		"exploring a new repo": {
			call("Glob", "**/*.go", "50 files"),
			call("Read", "/a.go", "package a"),
			call("Read", "/b.go", "package b"),
			call("Grep", "func main", "2 matches"),
			call("Read", "/c.go", "package c"),
		},
		"one failing test then a fix": {
			failing("Bash", "go test ./...", "FAIL: TestFoo"),
			call("Read", "/foo.go", "package foo"),
			call("Edit", "/foo.go", "ok"),
			call("Bash", "go test ./...", "ok\nPASS"),
		},
	}
	for name, calls := range cases {
		if k := kinds(t, calls); k != "" {
			t.Errorf("%s: expected silence, got %q", name, k)
		}
	}
}

func TestSameFailure(t *testing.T) {
	calls := []transcript.ToolCall{
		call("Edit", "/foo.go", "ok"),
		failing("Bash", "go test ./...", "FAIL: TestFoo expected 3 got 4"),
		failing("Bash", "go test ./...", "FAIL: TestFoo expected 3 got 4"),
	}
	f := Analyze(calls, DefaultThresholds())
	if f == nil || f.Kind != "same_failure" {
		t.Fatalf("got %+v, want same_failure", f)
	}
	if f.Count != 2 {
		t.Errorf("count = %d, want 2", f.Count)
	}
	if f.Reason == "" {
		t.Error("a finding must explain itself")
	}
}

// Numbers that change every run (durations, pids) must not hide a loop.
func TestSameFailureIgnoresVolatileNumbers(t *testing.T) {
	calls := []transcript.ToolCall{
		failing("Bash", "pytest -k auth", "FAILED in 1.31s: assert None"),
		failing("Bash", "pytest -k auth", "FAILED in 2.07s: assert None"),
		failing("Bash", "pytest -k auth", "FAILED in 1.98s: assert None"),
	}
	if k := kinds(t, calls); k != "same_failure" {
		t.Errorf("got %q, want same_failure", k)
	}
}

// An edit that changes nothing about the failure is the loop we care
// about most — editing in between must NOT hide it.
func TestEditDoesNotHideAnIdenticalFailure(t *testing.T) {
	calls := []transcript.ToolCall{
		failing("Bash", "go build ./...", "undefined: foo"),
		call("Edit", "/foo.go", "ok"),
		failing("Bash", "go build ./...", "undefined: foo"),
	}
	if k := kinds(t, calls); k != "same_failure" {
		t.Errorf("got %q, want same_failure", k)
	}
}

// But an edit that DOES change the failure means progress.
func TestChangedFailureIsProgress(t *testing.T) {
	calls := []transcript.ToolCall{
		failing("Bash", "go build ./...", "undefined: foo"),
		call("Edit", "/foo.go", "ok"),
		failing("Bash", "go build ./...", "undefined: bar"),
	}
	if k := kinds(t, calls); k != "" {
		t.Errorf("got %q, want silence — the error moved", k)
	}
}

func TestErrorStreak(t *testing.T) {
	calls := []transcript.ToolCall{
		failing("Bash", "npm ci", "npm ERR! network"),
		failing("Bash", "yarn install", "error: registry unreachable"),
		failing("Bash", "pnpm i", "ERR_PNPM_FETCH failed"),
	}
	if k := kinds(t, calls); k != "error_streak" {
		t.Errorf("got %q, want error_streak", k)
	}
}

func TestRepeatWithoutFailure(t *testing.T) {
	var calls []transcript.ToolCall
	for i := 0; i < 5; i++ { // the tuned threshold: 3 repeats are normal work
		calls = append(calls, call("Read", "/big/file.go", "…contents…"))
	}
	f := Analyze(calls, DefaultThresholds())
	if f == nil || f.Kind != "repeat" {
		t.Fatalf("got %+v, want repeat", f)
	}
}

// Oscillation only counts when the loop is getting nowhere: at least one
// side keeps failing.
func TestOscillationNeedsFailures(t *testing.T) {
	stuck := []transcript.ToolCall{
		failing("Bash", "make check", "error: still broken"),
		call("Bash", "git status", "clean"),
		failing("Bash", "make check", "error: still broken"),
		call("Bash", "git status", "clean"),
	}
	if k := kinds(t, stuck); k != "oscillation" {
		t.Errorf("failing oscillation: got %q, want oscillation", k)
	}

	healthy := []transcript.ToolCall{
		call("Grep", "handler", "3 matches"),
		call("Read", "/a.go", "…"),
		call("Grep", "handler", "3 matches"),
		call("Read", "/a.go", "…"),
	}
	if k := kinds(t, healthy); k != "" {
		t.Errorf("healthy browse rhythm: got %q, want silence", k)
	}

	editing := []transcript.ToolCall{
		call("Edit", "/a.go", "ok"),
		failing("Bash", "go test", "FAIL x"),
		call("Edit", "/a.go", "ok"),
		failing("Bash", "go test", "FAIL x"),
	}
	if k := kinds(t, editing); k == "oscillation" {
		t.Error("edit/test cycles are covered by same_failure, not oscillation")
	}
}

func TestNoProgress(t *testing.T) {
	// an agent that was editing and then went quiet for a long stretch
	calls := []transcript.ToolCall{call("Edit", "/start.go", "ok")}
	for i := 0; i < 25; i++ {
		calls = append(calls, call("Read", "/file"+string(rune('a'+i))+".go", "…"))
	}
	th := DefaultThresholds()
	th.Window = 40 // the earlier edit must stay inside the window
	if f := Analyze(calls, th); f == nil || f.Kind != "no_progress" {
		t.Errorf("got %+v, want no_progress", f)
	}
	// one edit resets the counter
	calls = append(calls, call("Edit", "/x.go", "ok"))
	if f := Analyze(calls, th); f != nil {
		t.Errorf("after an edit: got %+v, want silence", f)
	}
}

// Research at the start of a session is not a stall.
func TestNoProgressIgnoresPureResearch(t *testing.T) {
	var calls []transcript.ToolCall
	for i := 0; i < 30; i++ {
		calls = append(calls, call("Read", "/file"+string(rune('a'+i%26))+".go", "…"))
	}
	th := DefaultThresholds()
	th.Window = 40
	if f := Analyze(calls, th); f != nil && f.Kind == "no_progress" {
		t.Errorf("reading before any edit must stay silent, got %+v", f)
	}
}

// The classic loop: edit, re-run, identical failure, edit, re-run…
func TestSameFailureSurvivesEditsInBetween(t *testing.T) {
	calls := []transcript.ToolCall{
		failing("Bash", "go test ./...", "FAIL: TestFoo want 3 got 4"),
		call("Edit", "/foo.go", "ok"),
		failing("Bash", "go test ./...", "FAIL: TestFoo want 3 got 4"),
		call("Edit", "/foo.go", "ok"),
		failing("Bash", "go test ./...", "FAIL: TestFoo want 3 got 4"),
	}
	f := Analyze(calls, DefaultThresholds())
	if f == nil || f.Kind != "same_failure" {
		t.Fatalf("got %+v, want same_failure", f)
	}
	if !strings.Contains(f.Reason, "edits") {
		t.Errorf("the reason should mention the wasted edits: %q", f.Reason)
	}
}

// Long shell commands that merely start alike are different calls.
func TestSignatureUsesFullArguments(t *testing.T) {
	prefix := "cd /very/long/path/that/repeats/everywhere && python3 - <<EOF "
	a := transcript.ToolCall{Tool: "Bash", Arg: prefix, Input: "command: " + prefix + "print(alpha)"}
	b := transcript.ToolCall{Tool: "Bash", Arg: prefix, Input: "command: " + prefix + "print(beta)"}
	if Signature(a) == Signature(b) {
		t.Error("different commands with a shared prefix must not share a signature")
	}
}

func TestWindowLimitsHistory(t *testing.T) {
	var calls []transcript.ToolCall
	for i := 0; i < 30; i++ {
		calls = append(calls, call("Bash", "make", "ok"))
	}
	th := DefaultThresholds()
	th.Window = 20
	f := Analyze(calls, th)
	if f == nil || f.Count > 20 {
		t.Fatalf("count must respect the window: %+v", f)
	}
}

func TestDisabledDetectors(t *testing.T) {
	calls := []transcript.ToolCall{
		failing("Bash", "go test", "FAIL x"),
		failing("Bash", "go test", "FAIL x"),
		failing("Bash", "go test", "FAIL x"),
	}
	th := Thresholds{Window: 20} // everything disabled
	if f := Analyze(calls, th); f != nil {
		t.Errorf("disabled detectors must stay silent, got %+v", f)
	}
}

// Reading source code that talks about errors is not a failure.
func TestFailureWordsOnlyCountForCommands(t *testing.T) {
	var calls []transcript.ToolCall
	for i := 0; i < 5; i++ {
		calls = append(calls, call("Read", "/src/errors.go",
			`func wrap() error { return fmt.Errorf("error: %w", err) }`))
	}
	f := Analyze(calls, DefaultThresholds())
	if f == nil || f.Kind != "repeat" {
		t.Fatalf("got %+v, want plain repeat (not a failure loop)", f)
	}
}

// Agents often edit through the shell; measured runs finished whole tasks
// without a single Edit call. Those commands must count as progress.
func TestShellWritesCountAsProgress(t *testing.T) {
	writes := []string{
		"sed -i '' 's/a/b/' src/calc.py",
		"cat > /tmp/patch.py <<'PY'\nprint(1)\nPY",
		"python3 - <<'PY' > out.json\nprint(1)\nPY",
		"cp build/x src/x",
		"git apply fix.patch",
	}
	for _, cmd := range writes {
		c := transcript.ToolCall{Tool: "Bash", Arg: cmd, Input: "command: " + cmd}
		if !mutating(c) {
			t.Errorf("must count as progress: %q", cmd)
		}
	}
	reads := []string{
		"go test ./...",
		"grep -rn handler src/",
		"cat src/calc.py",
		"ls -la",
	}
	for _, cmd := range reads {
		c := transcript.ToolCall{Tool: "Bash", Arg: cmd, Input: "command: " + cmd}
		if mutating(c) {
			t.Errorf("must NOT count as progress: %q", cmd)
		}
	}

	// a long read-only stretch that ends with a shell edit is progress
	var calls []transcript.ToolCall
	for i := 0; i < 25; i++ {
		calls = append(calls, call("Bash", "grep -rn thing"+string(rune('a'+i))+" .", "no match"))
	}
	calls = append(calls, call("Bash", "sed -i '' 's/x/y/' a.py", "ok"))
	th := DefaultThresholds()
	th.Window = 40
	if f := Analyze(calls, th); f != nil && f.Kind == "no_progress" {
		t.Errorf("a shell edit resets the stall counter, got %+v", f)
	}

	// …but the same stretch after an edit, with nothing written since, is
	// exactly what a stall looks like
	stalled := append([]transcript.ToolCall{call("Bash", "sed -i '' 's/x/y/' a.py", "ok")}, calls[:25]...)
	if f := Analyze(stalled, th); f == nil || f.Kind != "no_progress" {
		t.Errorf("expected a stall after the edits stopped, got %+v", f)
	}
}

// Reading a design document that talks about failures is not a failure —
// this cost eleven false alarms in a real session before it was fixed.
func TestDocumentationIsNotAFailure(t *testing.T) {
	doc := "## Error handling\nWhen the reconcile fails, the controller retries. error: is logged."
	var calls []transcript.ToolCall
	for i := 0; i < 8; i++ {
		calls = append(calls, call("Bash", "sed -n '1,80p' docs/FOLDERS"+string(rune('a'+i))+".md", doc))
	}
	if f := Analyze(calls, DefaultThresholds()); f != nil && f.Kind == "error_streak" {
		t.Errorf("reading docs must not look like failing calls: %+v", f)
	}
}
