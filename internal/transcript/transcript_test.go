package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastAssistantText(t *testing.T) {
	lines := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"do it"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}
not json at all
{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"subagent noise"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","text":""}]}}
{"type":"assistant","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"final"},{"type":"text","text":"reply"}]}}
`
	path := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(path, []byte(lines), 0o644)

	got, model, err := For("claude", path).LastReply()
	if err != nil {
		t.Fatal(err)
	}
	if got != "final\n\nreply" {
		t.Errorf("got %q, want the last non-sidechain text entry", got)
	}
	if model != "claude-fable-5" {
		t.Errorf("model = %q, want claude-fable-5", model)
	}
}

func TestLastAssistantTextMissingFile(t *testing.T) {
	if _, err := LastReplyText("claude", "/nonexistent"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestTail(t *testing.T) {
	if got := Tail("hello", 10); got != "hello" {
		t.Errorf("short string must be unchanged, got %q", got)
	}
	if got := Tail("abcdefgh", 3); got != "…fgh" {
		t.Errorf("got %q, want tail with ellipsis", got)
	}
}

func TestToolCalls(t *testing.T) {
	lines := `{"type":"assistant","timestamp":"2026-08-18T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"npm run build"}}]}}
{"type":"assistant","timestamp":"2026-08-18T10:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/src/app.go","old_string":"x"}},{"type":"text","text":"done"}]}}
{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"/noise"}}]}}
`
	path := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(path, []byte(lines), 0o644)
	calls, err := For("claude", path).ToolCalls(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2 (sidechain excluded): %+v", len(calls), calls)
	}
	if calls[0].Tool != "Bash" || calls[0].Arg != "npm run build" {
		t.Errorf("call 0 = %+v", calls[0])
	}
	if calls[1].Tool != "Edit" || calls[1].Arg != "/src/app.go" {
		t.Errorf("call 1 = %+v (file_path must win over old_string)", calls[1])
	}
	if calls[0].Time.IsZero() {
		t.Error("timestamp not parsed")
	}
}

func TestToolCallsLinkResults(t *testing.T) {
	lines := `{"type":"assistant","timestamp":"2026-08-18T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./...","description":"run tests"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"FAIL: TestFoo"}],"is_error":true}]}}
{"type":"assistant","timestamp":"2026-08-18T10:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/src/foo.go"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"package foo"}]}}
`
	path := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(path, []byte(lines), 0o644)
	calls, err := For("claude", path).ToolCalls(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d calls: %+v", len(calls), calls)
	}
	if calls[0].Result != "FAIL: TestFoo" || !calls[0].IsError {
		t.Errorf("call 0 result wrong: %+v", calls[0])
	}
	if !strings.Contains(calls[0].Input, "command: go test ./...") ||
		!strings.Contains(calls[0].Input, "description: run tests") {
		t.Errorf("call 0 input not rendered: %q", calls[0].Input)
	}
	if calls[1].Result != "package foo" { // plain-string tool_result
		t.Errorf("call 1 result wrong: %+v", calls[1])
	}
	if calls[1].IsError {
		t.Error("call 1 must not be an error")
	}
}

// A delta is the only thing a reviewer agent sees. A delegating agent
// produces almost no top-level text, so a delta of bare tool names told the
// reviewer nothing and it answered OK to everything.
func TestDeltaSinceCarriesWork(t *testing.T) {
	lines := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"research the code"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"starting"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./...","description":"run tests"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"FAIL: TestFoo"}],"is_error":true}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Agent","input":{"description":"map the VM path","prompt":"long prompt"}}]}}
{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"subagent chatter"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"the VM path goes through vmController"}]}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Read","input":{"file_path":"/src/vm.go"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":"package vm"}]}}
`
	path := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(path, []byte(lines), 0o644)

	d, err := For("claude", path).DeltaSince(0)
	if err != nil {
		t.Fatal(err)
	}
	delta, off := d.Text, d.End
	if off != int64(len(lines)) {
		t.Errorf("offset = %d, want the file size %d", off, len(lines))
	}
	for _, want := range []string{
		"user: research the code",
		"agent: starting",
		"Bash(go test ./...)",        // which command, not just "Bash"
		"failed Bash: FAIL: TestFoo", // failures are the point of a review
		"Agent(map the VM path)",     // which subagent
		"subagent report: the VM path goes through vmController",
		"Read(/src/vm.go)",
	} {
		if !strings.Contains(delta, want) {
			t.Errorf("delta missing %q:\n%s", want, delta)
		}
	}
	// ordinary results and subagent internals stay out — the delta is a
	// summary, not a transcript copy
	if strings.Contains(delta, "package vm") {
		t.Errorf("plain tool output must not be inlined:\n%s", delta)
	}
	if strings.Contains(delta, "subagent chatter") {
		t.Errorf("sidechain entries must stay out:\n%s", delta)
	}
	// the text of a turn comes before the calls it made
	if strings.Index(delta, "agent: starting") > strings.Index(delta, "Bash(") {
		t.Errorf("delta out of order:\n%s", delta)
	}
}

// Claude writes messages in two shapes — a plain string and a list of
// blocks — and uses the string shape for the user's own prompts. Both
// must produce the same delta; parsing only the list shape dropped every
// user prompt, so a reviewer never learned what had been asked for.
func TestDeltaIsTheSameInBothContentShapes(t *testing.T) {
	shapes := map[string]string{
		"string": `{"type":"user","message":{"role":"user","content":"rewrite the parser"}}
{"type":"assistant","message":{"role":"assistant","content":"starting"}}
`,
		"blocks": `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"rewrite the parser"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"starting"}]}}
`,
	}
	want := "user: rewrite the parser\nagent: starting\n"
	for name, lines := range shapes {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.jsonl")
			os.WriteFile(path, []byte(lines), 0o644)
			d, err := For("claude", path).DeltaSince(0)
			if err != nil {
				t.Fatal(err)
			}
			if d.Text != want {
				t.Errorf("delta = %q, want %q", d.Text, want)
			}
			if !d.EndsWithReply {
				t.Error("the session ends with the agent's own words")
			}
			text, _, err := For("claude", path).LastReply()
			if err != nil || text != "starting" {
				t.Errorf("LastReply = %q, %v", text, err)
			}
		})
	}
}

func TestDeltaCarriesPlainStringUserPrompts(t *testing.T) {
	lines := `{"type":"user","message":{"role":"user","content":"rewrite the parser, keep the API"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"starting"},{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/src/p.go"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"user","message":{"role":"user","content":"and add a test"}}
`
	path := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(path, []byte(lines), 0o644)

	d, err := For("claude", path).DeltaSince(0)
	if err != nil {
		t.Fatal(err)
	}
	delta := d.Text
	for _, want := range []string{
		"user: rewrite the parser, keep the API",
		"user: and add a test",
		"agent: starting",
		"Edit(/src/p.go)",
	} {
		if !strings.Contains(delta, want) {
			t.Errorf("delta missing %q:\n%s", want, delta)
		}
	}

	// the same shape must not confuse the other readers
	text, _, err := For("claude", path).LastReply()
	if err != nil || text != "starting" {
		t.Errorf("LastReply = %q, %v", text, err)
	}
	calls, err := For("claude", path).ToolCalls(10)
	if err != nil || len(calls) != 1 || calls[0].Tool != "Edit" || calls[0].Result != "ok" {
		t.Errorf("ToolCalls = %+v, %v", calls, err)
	}
}

// Whether a turn has said its conclusion is metadata, not something to
// be read off the rendered delta: a multiline final answer, a turn that
// stops on a tool call, and the same cases in the Codex rollout shape.
func TestDeltaReportsWhetherTheTurnConcluded(t *testing.T) {
	multiline := "here is the summary\n\n- first point\n- second point\nand the conclusion"
	cases := []struct {
		name  string
		agent string
		lines string
		want  bool
	}{
		{"claude multiline final answer", "claude",
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` +
				strings.ReplaceAll(multiline, "\n", "\\n") + `"}]}}
`, true},
		{"claude ends on a tool call", "claude",
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}
`, false},
		{"claude ends on a tool result", "claude",
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go build ./..."}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"exit 1","is_error":true}]}}
`, false},
		{"claude ends on the user's prompt", "claude",
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
{"type":"user","message":{"role":"user","content":"and one more thing"}}
`, false},
		{"codex multiline final answer", "codex",
			`{"timestamp":"2026-08-18T10:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` +
				strings.ReplaceAll(multiline, "\n", "\\n") + `"}]}}
`, true},
		{"codex ends on a tool call", "codex",
			`{"timestamp":"2026-08-18T10:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"looking"}]}}
{"timestamp":"2026-08-18T10:00:01Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{\"command\":\"ls\"}"}}
`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.jsonl")
			os.WriteFile(path, []byte(c.lines), 0o644)
			d, err := For(c.agent, path).DeltaSince(0)
			if err != nil {
				t.Fatal(err)
			}
			if d.EndsWithReply != c.want {
				t.Errorf("EndsWithReply = %v, want %v (delta %q)", d.EndsWithReply, c.want, d.Text)
			}
			if c.want && !strings.Contains(d.Text, "and the conclusion") {
				t.Errorf("the whole final answer must be in the delta: %q", d.Text)
			}
		})
	}
}

// The delta budgets come from the config, and the one that matters most
// is the tool argument: a long command clipped from the right hides how
// it ended, which is exactly where the consequence lives.
func TestBudgetsSizeTheDelta(t *testing.T) {
	long := "go test ./... && echo done && rm internal/textutil/zz_tmp_test.go"
	lines := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"` + long + `"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"boom, a very long error message indeed","is_error":true}]}}
`
	path := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(path, []byte(lines), 0o644)

	// generous budgets keep the whole command
	wide, err := ForBudgets("claude", path, Budgets{Arg: 200, Error: 500}).DeltaSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wide.Text, long) {
		t.Errorf("a 200-rune budget must hold this command:\n%s", wide.Text)
	}

	// a tight one still keeps both ends of it
	tight, err := ForBudgets("claude", path, Budgets{Arg: 30, Error: 20}).DeltaSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tight.Text, "go test") || !strings.Contains(tight.Text, "_test.go)") {
		t.Errorf("both ends of the command must survive clipping:\n%s", tight.Text)
	}
	if !strings.Contains(tight.Text, "…") {
		t.Errorf("a tight budget must actually clip:\n%s", tight.Text)
	}
	if strings.Contains(tight.Text, "a very long error message indeed") {
		t.Errorf("the error budget was not applied:\n%s", tight.Text)
	}

	// zero means "use the default", never "no limit"
	def, err := ForBudgets("claude", path, Budgets{}).DeltaSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if def.Text != wide.Text {
		t.Errorf("an empty budget must behave like the default:\n%s", def.Text)
	}
}

func TestCodexReader(t *testing.T) {
	lines := `{"timestamp":"2026-08-18T10:00:00Z","type":"session_meta","payload":{"session_id":"s1"}}
{"timestamp":"2026-08-18T10:00:01Z","type":"world_state","payload":{"state":{"collaboration_mode":{"model":"gpt-5.6-sol"}}}}
{"timestamp":"2026-08-18T10:00:02Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<skills_instructions>noise"}]}}
{"timestamp":"2026-08-18T10:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]}}
{"timestamp":"2026-08-18T10:00:04Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{\"command\":\"go build ./...\"}"}}
{"timestamp":"2026-08-18T10:00:05Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"exit code 1\nundefined: foo"}]}}
{"timestamp":"2026-08-18T10:00:06Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"c2","input":"const r = await tools.read({path:'main.go'})"}}
{"timestamp":"2026-08-18T10:00:07Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":"concern: the build is broken"}}
`
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	os.WriteFile(path, []byte(lines), 0o644)
	r := For("codex", path)

	text, model, err := r.LastReply()
	if err != nil {
		t.Fatal(err)
	}
	if text != "concern: the build is broken" {
		t.Errorf("LastReply text = %q", text)
	}
	if model != "gpt-5.6-sol" {
		t.Errorf("model = %q", model)
	}

	calls, err := r.ToolCalls(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d calls: %+v", len(calls), calls)
	}
	if calls[0].Tool != "shell" || calls[0].Arg != "go build ./..." {
		t.Errorf("call 0 = %+v", calls[0])
	}
	if !strings.Contains(calls[0].Result, "undefined: foo") || !calls[0].IsError {
		t.Errorf("call 0 result/error wrong: %+v", calls[0])
	}
	if calls[1].Tool != "exec" || !strings.Contains(calls[1].Input, "tools.read") {
		t.Errorf("call 1 = %+v", calls[1])
	}

	d, err := r.DeltaSince(0)
	if err != nil {
		t.Fatal(err)
	}
	delta, off := d.Text, d.End
	if off == 0 {
		t.Error("offset must advance")
	}
	if !strings.Contains(delta, "user: fix the build") {
		t.Errorf("delta = %q", delta)
	}
	// the reviewer must see what was run and what failed, not bare names
	if !strings.Contains(delta, "shell(go build ./...)") {
		t.Errorf("delta lost the tool argument: %q", delta)
	}
	if !strings.Contains(delta, "failed shell: exit code 1 undefined: foo") {
		t.Errorf("delta lost the failure: %q", delta)
	}
	if !strings.Contains(delta, "exec(const r = await tools.read") {
		t.Errorf("delta lost the custom tool call: %q", delta)
	}
	if strings.Contains(delta, "skills_instructions") {
		t.Error("developer boilerplate must be filtered out of the delta")
	}
}

// A long shell command says what it did at the front and what it undid at
// the end. Clipping only the right hid the tail, and a reviewer once
// raised a blocker about a file the same command had already deleted.
func TestClipMiddleKeepsBothEnds(t *testing.T) {
	cmd := "cat > internal/textutil/zz_tmp_test.go <<EOF ... EOF && go test ./internal/textutil/ -run WrapWide -v && rm internal/textutil/zz_tmp_test.go"
	got := clipMiddle(cmd, 60)
	if len([]rune(got)) > 60 {
		t.Errorf("clip too long: %d runes", len([]rune(got)))
	}
	if !strings.HasPrefix(got, "cat > internal/textutil") {
		t.Errorf("lost the head: %q", got)
	}
	if !strings.HasSuffix(got, "zz_tmp_test.go") {
		t.Errorf("lost the tail — the whole point: %q", got)
	}
	if short := clipMiddle("go build ./...", 60); short != "go build ./..." {
		t.Errorf("a short argument must pass through untouched, got %q", short)
	}
}

func TestCodexFailureDetection(t *testing.T) {
	fail := []string{
		"exit code 1",
		"…\nexit code 127",
		"zsh: command not found: gofmtx",
		"cat: nope.txt: No such file or directory",
		"the request timed out",
	}
	pass := []string{
		"exit code 0",
		"all tests passed",
		"", // no output at all is not a failure
	}
	for _, out := range fail {
		if !isFailureOutput(out) {
			t.Errorf("%q must count as a failure", out)
		}
	}
	for _, out := range pass {
		if isFailureOutput(out) {
			t.Errorf("%q must not count as a failure", out)
		}
	}
}

// The adapter must survive a real rollout from the user's machine when one
// is present — formats drift, and a silent parse failure would be worse
// than a failing test.
func TestCodexReaderOnRealRollout(t *testing.T) {
	home, _ := os.UserHomeDir()
	matches, _ := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*.jsonl"))
	if len(matches) == 0 {
		t.Skip("no local Codex rollouts")
	}
	var newest string
	var newestSize int64
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.Size() > newestSize {
			newest, newestSize = m, st.Size()
		}
	}
	r := For("codex", newest)
	if _, _, err := r.LastReply(); err != nil {
		t.Fatalf("LastReply on %s: %v", newest, err)
	}
	calls, err := r.ToolCalls(20)
	if err != nil {
		t.Fatalf("ToolCalls: %v", err)
	}
	t.Logf("%s: %d calls parsed", filepath.Base(newest), len(calls))
}
