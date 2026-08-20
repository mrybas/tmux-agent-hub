package hookd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/tmuxctl"
)

func TestApplyLifecycle(t *testing.T) {
	store := state.NewStore(t.TempDir())
	now := time.Now()
	pane := "%7"

	step := func(pl payload, want state.Status) {
		t.Helper()
		if err := apply(store, pane, &pl, now); err != nil {
			t.Fatal(err)
		}
		p, err := store.Load(pane)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != want {
			t.Fatalf("after %s: status = %s, want %s", pl.HookEventName, p.Status, want)
		}
	}

	step(payload{HookEventName: "SessionStart", SessionID: "s1", Cwd: "/tmp/proj"}, state.StatusWaitingInput)
	step(payload{HookEventName: "UserPromptSubmit", Prompt: "fix the bug\nplease"}, state.StatusWorking)
	step(payload{HookEventName: "PreToolUse", ToolName: "Bash"}, state.StatusWorking)
	step(payload{HookEventName: "Notification", Message: "Claude needs your permission to use Bash"}, state.StatusWaitingPermission)
	step(payload{HookEventName: "Stop"}, state.StatusDone)

	p, _ := store.Load(pane)
	if p.LastPrompt != "fix the bug please" {
		t.Errorf("LastPrompt = %q, newlines not collapsed", p.LastPrompt)
	}
	if p.CurrentTool != "" {
		t.Errorf("CurrentTool = %q after Stop, want empty", p.CurrentTool)
	}

	if err := apply(store, pane, &payload{HookEventName: "SessionEnd"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(pane); err == nil {
		t.Error("state file still exists after SessionEnd")
	}
}

func TestIdleNotificationIsWaitingInput(t *testing.T) {
	store := state.NewStore(t.TempDir())
	pl := payload{HookEventName: "Notification", Message: "Claude is waiting for your input"}
	if err := apply(store, "%1", &pl, time.Now()); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Load("%1")
	if p.Status != state.StatusWaitingInput {
		t.Fatalf("status = %s, want waiting_input", p.Status)
	}
}

func TestInstallIsIdempotentAndPreservesForeignHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{
	  "model": "opus",
	  "hooks": {
	    "Stop": [{"hooks": [{"type": "command", "command": "afplay /System/Library/Sounds/Glass.aiff"}]}]
	  }
	}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Install("/old/path/tmux-agent-hub"); err != nil {
		t.Fatal(err)
	}
	if err := Install("/new/path/tmux-agent-hub"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["model"] != "opus" {
		t.Error("unrelated top-level setting lost")
	}
	if strings.Contains(string(data), "/old/path") {
		t.Error("stale binary path not removed on reinstall")
	}
	if !strings.Contains(string(data), "afplay") {
		t.Error("foreign Stop hook lost")
	}
	hooks := settings["hooks"].(map[string]any)
	for _, event := range claudeEvents {
		groups, _ := hooks[event].([]any)
		ours := 0
		for _, g := range groups {
			if isOurs(g) {
				ours++
			}
		}
		if ours != 1 {
			t.Errorf("event %s: %d tmux-agent-hub entries, want exactly 1", event, ours)
		}
	}

	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "tmux-agent-hub") {
		t.Error("tmux-agent-hub entries remain after uninstall")
	}
	if !strings.Contains(string(data), "afplay") {
		t.Error("foreign Stop hook lost on uninstall")
	}
}

// opencode has no hooks file to write into: it loads plugins from a
// directory, and ours is what reports events and writes the transcript.
// Installing must drop it in place, and uninstalling must take it away.
func TestOpencodePluginInstallAndRemove(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)

	// no opencode on this machine: nothing to install, and no complaint
	if err := installOpencodePlugin("/bin/hub"); err != nil {
		t.Fatalf("a machine without opencode must not be an error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg, "opencode", "plugins")); err == nil {
		t.Error("nothing may be created for an agent that is not installed")
	}

	// opencode is set up: the plugin lands in its plugin directory
	if err := os.MkdirAll(filepath.Join(cfg, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installOpencodePlugin("/bin/hub"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "opencode", "plugins", "tmux-agent-hub.js")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("plugin not installed: %v", err)
	}
	if !strings.Contains(string(body), `"/bin/hub"`) {
		t.Error("the plugin must call the binary it was installed from")
	}
	if strings.Contains(string(body), "__TMUX_AGENT_HUB_BIN__") {
		t.Error("the placeholder must be replaced")
	}

	// installing twice is the same as installing once
	if err := installOpencodePlugin("/bin/hub"); err != nil {
		t.Fatal(err)
	}
	if err := uninstallOpencodePlugin(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("uninstall must remove the plugin")
	}
	if err := uninstallOpencodePlugin(); err != nil {
		t.Errorf("removing what is already gone is not an error: %v", err)
	}
}

func TestStopLearnsModelFromTranscript(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	line := `{"type":"assistant","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"done"}]}}`
	os.WriteFile(tr, []byte(line+"\n"), 0o644)
	store := state.NewStore(t.TempDir())
	now := time.Now()
	if err := apply(store, "%9", &payload{HookEventName: "SessionStart", TranscriptPath: tr}, now); err != nil {
		t.Fatal(err)
	}
	if err := apply(store, "%9", &payload{HookEventName: "Stop"}, now); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Load("%9")
	if p.Model != "claude-fable-5" {
		t.Errorf("Model = %q, want claude-fable-5", p.Model)
	}
}

func TestReconcileInterrupted(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	interrupted := filepath.Join(dir, "int.jsonl")
	os.WriteFile(interrupted, []byte(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}
`), 0o644)
	busy := filepath.Join(dir, "busy.jsonl")
	os.WriteFile(busy, []byte(
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","text":"ok"}]}}
`), 0o644)

	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "x", TranscriptPath: interrupted}, now)
	apply(store, "%2", &payload{HookEventName: "UserPromptSubmit", Prompt: "y", TranscriptPath: busy}, now)

	panes, _ := store.List()
	if !ReconcileInterrupted(store, panes) {
		t.Fatal("expected a change for the interrupted pane")
	}
	p1, _ := store.Load("%1")
	if p1.Status != state.StatusWaitingInput {
		t.Errorf("interrupted pane = %s, want waiting_input", p1.Status)
	}
	p2, _ := store.Load("%2")
	if p2.Status != state.StatusWorking {
		t.Errorf("busy pane = %s, must stay working (tool_result is not an interrupt)", p2.Status)
	}
}

func writeTranscript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendTranscript(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func TestLiveReviewCycle(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	primaryTr := writeTranscript(t, dir, "p.jsonl",
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"fix the bug"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I changed foo.go and it works"}]}}
`)
	reviewerTr := writeTranscript(t, dir, "r.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"You forgot to handle the error in foo.go"}]}}
`)

	// primary %1 with reviewer %2 assigned
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: "/proj", TranscriptPath: primaryTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: "/rev", TranscriptPath: reviewerTr}, now)
	p, _ := store.Load("%1")
	p.ReviewerPane = "%2"
	store.Save(p)

	// primary finishes a turn -> reviewer must be marked awaiting, offset advances
	apply(store, "%1", &payload{HookEventName: "Stop"}, now)
	r, _ := store.Load("%2")
	if r.AwaitingReviewFor != "%1" {
		t.Fatalf("reviewer AwaitingReviewFor = %q, want %%1", r.AwaitingReviewFor)
	}
	p, _ = store.Load("%1")
	if p.ReviewOffset == 0 {
		t.Error("primary ReviewOffset not advanced")
	}

	// our review request lands in the reviewer -> must keep the correlation
	apply(store, "%2", &payload{HookEventName: "UserPromptSubmit", Prompt: ReqMarker + " delta..."}, now)
	r, _ = store.Load("%2")
	if r.AwaitingReviewFor != "%1" {
		t.Error("review request prompt must not clear AwaitingReviewFor")
	}

	// reviewer finishes with real advice -> primary marked SkipNextReview
	apply(store, "%2", &payload{HookEventName: "Stop"}, now)
	r, _ = store.Load("%2")
	if r.AwaitingReviewFor != "" {
		t.Error("AwaitingReviewFor must clear after forwarding")
	}
	p, _ = store.Load("%1")
	if p.SkipNextReview {
		t.Error("the flag belongs to the turn the advice starts, not to the delivery")
	}

	// the advice-triggered turn is reviewed like any other: the reviewer
	// has to see what came of its advice
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: AdvMarker + " from x] do it"}, now)
	p, _ = store.Load("%1")
	if p.SkipNextReview {
		t.Error("a worker's turn must never be hidden from its reviewer")
	}

	// a real user prompt in the reviewer cancels a stale correlation
	r, _ = store.Load("%2")
	r.AwaitingReviewFor = "%1"
	store.Save(r)
	apply(store, "%2", &payload{HookEventName: "UserPromptSubmit", Prompt: "unrelated user question"}, now)
	r, _ = store.Load("%2")
	if r.AwaitingReviewFor != "" {
		t.Error("real user prompt must cancel review correlation")
	}
}

// A reviewer that never answers (interrupted, killed, hook lost) used to
// mute the pair for the rest of the session: every later review found it
// "busy" with a request nobody was working on.
func TestLostVerdictFreesTheReviewer(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first step done"}]}}
`)
	reviewerTr := writeTranscript(t, dir, "r.jsonl", "")

	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: "/proj", TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: "/rev", TranscriptPath: reviewerTr}, now)
	p, _ := store.Load("%1")
	p.ReviewerPane = "%2"
	store.Save(p)

	apply(store, "%1", &payload{HookEventName: "Stop"}, now)
	r, _ := store.Load("%2")
	if r.AwaitingReviewFor != "%1" || r.AwaitingSince.IsZero() {
		t.Fatalf("reviewer not marked busy: %+v", r)
	}

	// the reviewer dies without ever replying, and the worker keeps working
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second step done"}]}}
`)
	later := now.Add(advisorLimits().LostTimeout() + time.Minute)
	apply(store, "%1", &payload{HookEventName: "Stop"}, later)

	r, _ = store.Load("%2")
	if r.AwaitingReviewFor != "%1" {
		t.Fatalf("the stale request must be dropped and the new one sent, got %q", r.AwaitingReviewFor)
	}
	if !r.AwaitingSince.After(now.Add(advisorLimits().LostTimeout())) {
		t.Error("the new request must carry a fresh timestamp")
	}
}

// A review request that never reaches the reviewer (pane gone, tmux
// hiccup) must not leave it marked busy: the verdict it would answer with
// is never coming.
func TestFailedSendFreesTheReviewer(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	failSends(t, errors.New("pane not found"))

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"did a thing"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: "/proj", TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: "/rev",
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	p, _ := store.Load("%1")
	p.ReviewerPane = "%2"
	store.Save(p)

	apply(store, "%1", &payload{HookEventName: "Stop"}, now)

	r, _ := store.Load("%2")
	if r.AwaitingReviewFor != "" {
		t.Errorf("a failed send must not mark the reviewer busy, got %q", r.AwaitingReviewFor)
	}
	if !r.AwaitingSince.IsZero() {
		t.Error("AwaitingSince must be cleared with the correlation")
	}
}

// What the reviewer actually receives is the product of this package:
// the marker hooks recognize, plus the worker's delta.
func TestReviewRequestReachesTheReviewer(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"rewrote the parser"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}
`)
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: "/proj", TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: "/rev",
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	p, _ := store.Load("%1")
	p.ReviewerPane = "%2"
	store.Save(p)

	apply(store, "%1", &payload{HookEventName: "Stop"}, now)

	msgs := sentTo("%2")
	if len(msgs) != 1 {
		t.Fatalf("reviewer got %d messages, want 1", len(msgs))
	}
	for _, want := range []string{ReqMarker, "rewrote the parser", "Bash(go test ./...)"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("review request missing %q:\n%s", want, msgs[0])
		}
	}
}

// A request that never reached the reviewer must leave the delta unread:
// the offset is what decides whether that work is ever reviewed.
func TestFailedRequestKeepsTheDeltaForRetry(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	failSends(t, errors.New("pane not found"))

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"rewrote the parser"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: "/proj", TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: "/rev",
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	apply(store, "%1", &payload{HookEventName: "Stop"}, now)

	w, _ = store.Load("%1")
	if w.ReviewOffset != 0 {
		t.Errorf("ReviewOffset = %d after a failed send, want 0 (the delta was never read)", w.ReviewOffset)
	}
	if !w.LastReviewAt.IsZero() {
		t.Error("LastReviewAt must not advance for a review that never happened")
	}

	// the pane comes back: the same delta must still be waiting
	resetSent(t)
	apply(store, "%1", &payload{HookEventName: "Stop"}, now.Add(time.Minute))

	msgs := sentTo("%2")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "rewrote the parser") {
		t.Fatalf("the retry must carry the same delta, got %q", msgs)
	}
	w, _ = store.Load("%1")
	if w.ReviewOffset == 0 {
		t.Error("a successful send must advance the offset")
	}
}

// Advice that never reached the worker must stay pending. Recording it as
// delivered loses the note twice: the text is gone, and SkipNextReview
// swallows the worker's next delta.
func TestFailedDeliveryHoldsTheAdvice(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"step one"}]}}
`)
	reviewerTr := filepath.Join(dir, "r.jsonl")
	os.WriteFile(reviewerTr, []byte(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"blocker: you are rewriting the same file in a loop"}]}}`+"\n"), 0o644)

	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: reviewerTr}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	// end-of-turn review: its verdict is delivered at once — but the send
	// fails, so the worker never hears it
	apply(store, "%1", &payload{HookEventName: "Stop"}, now)
	failSends(t, errors.New("pane not found"))
	resetAdvisorLog(t)
	apply(store, "%2", &payload{HookEventName: "Stop"}, now)

	// the log is an audit trail: it must not claim a delivery that failed
	if events := advisorEvents(t); countEvent(events, "deliver") != 0 ||
		countEvent(events, "send_failed") != 1 {
		t.Errorf("events = %v, want a send_failed and no deliver", events)
	}

	w, _ = store.Load("%1")
	if w.PendingNote == "" || w.PendingSeverity != sevBlocker {
		t.Fatalf("undelivered advice must stay pending, got %+v", w)
	}
	if w.SkipNextReview {
		t.Error("SkipNextReview must not be set for a turn the worker never got")
	}
	if w.LastAdvice != "" {
		t.Errorf("LastAdvice = %q, want empty — nothing was delivered", w.LastAdvice)
	}

	// the pane comes back and the reviewer raises it again: now it lands
	resetSent(t)
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"step two"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "Stop"}, now.Add(time.Minute))
	apply(store, "%2", &payload{HookEventName: "Stop"}, now.Add(time.Minute))

	msgs := sentTo("%1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "rewriting the same file") {
		t.Fatalf("the held advice must reach the worker on the retry, got %q", msgs)
	}
	if events := advisorEvents(t); countEvent(events, "deliver") != 1 {
		t.Errorf("events = %v, want exactly one deliver — the one that landed", events)
	}
	w, _ = store.Load("%1")
	if w.PendingNote != "" {
		t.Error("delivered advice must stop being pending")
	}
	// the worker submits the injected advice: that turn is fed onward like
	// any other, so the reviewer learns what its advice produced
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: msgs[0]}, now.Add(2*time.Minute))
	w, _ = store.Load("%1")
	if w.SkipNextReview {
		t.Error("a worker's turn must never be hidden from its reviewer")
	}
	if w.AdviceStreak != 1 {
		t.Errorf("AdviceStreak = %d after one delivery, want 1", w.AdviceStreak)
	}
}

// Pasting the text and pressing Enter are two tmux commands. When only
// the Enter fails the agent already has the text, so a retry would paste
// it twice — that outcome must be kept, not rolled back.
func TestUnsubmittedSendIsNotRetried(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"step one"}]}}
`)
	reviewerTr := filepath.Join(dir, "r.jsonl")
	os.WriteFile(reviewerTr, []byte(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"blocker: stop rewriting that file"}]}}`+"\n"), 0o644)

	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: reviewerTr}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	failSends(t, fmt.Errorf("%w: send-keys failed", tmuxctl.ErrNotSubmitted))
	resetAdvisorLog(t)

	// the review request lands in the composer but is never submitted
	apply(store, "%1", &payload{HookEventName: "Stop"}, now)
	w, _ = store.Load("%1")
	if w.ReviewOffset == 0 {
		t.Error("the delta is in the reviewer's pane — rewinding would paste it twice")
	}
	r, _ := store.Load("%2")
	if r.AwaitingReviewFor != "%1" {
		t.Error("the reviewer holds the request; the lost-request timeout covers it from here")
	}

	// same for the advice going back to the worker
	apply(store, "%2", &payload{HookEventName: "Stop"}, now)
	w, _ = store.Load("%1")
	if w.PendingNote != "" {
		t.Errorf("advice already in the worker's pane must not be re-queued, got %q", w.PendingNote)
	}
	if w.SkipNextReview {
		t.Error("the advice was never submitted: no turn of the worker's may be swallowed")
	}
	// the user clears the composer and works on: that work must still be
	// reviewed, and the AdvMarker prompt is what marks a real advice turn
	resetSent(t)
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"step two"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "Stop"}, now.Add(time.Minute))
	if msgs := sentTo("%2"); len(msgs) != 1 || !strings.Contains(msgs[0], "step two") {
		t.Errorf("the next real turn must still reach the reviewer, got %q", msgs)
	}
	events := advisorEvents(t)
	if countEvent(events, "unsubmitted") != 2 {
		t.Errorf("events = %v, want both sends recorded as unsubmitted", events)
	}
	if countEvent(events, "deliver") != 0 || countEvent(events, "send_failed") != 0 {
		t.Errorf("events = %v, want neither a deliver nor a send_failed", events)
	}
}

// The other half of the unsubmitted case, spelled out so a later change
// cannot "fix" it by accident: a request left sitting in the reviewer's
// composer takes its delta with it. The lost-request timeout frees the
// reviewer, but the offset stays advanced — delivery is at-most-once,
// because rewinding would review that delta twice if the user submits
// the pasted text later, and duplicated advice is the worse failure.
func TestUnsubmittedRequestIsLostByDesign(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"step one"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	failSends(t, fmt.Errorf("%w: send-keys failed", tmuxctl.ErrNotSubmitted))
	apply(store, "%1", &payload{HookEventName: "Stop"}, now)

	// the user clears the composer; nobody will ever review "step one"
	resetSent(t)
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"step two"}]}}
`)
	later := now.Add(advisorLimits().LostTimeout() + time.Minute)
	apply(store, "%1", &payload{HookEventName: "Stop"}, later)

	msgs := sentTo("%2")
	if len(msgs) != 1 {
		t.Fatalf("the timeout must free the reviewer and let the next review through, got %d messages", len(msgs))
	}
	if !strings.Contains(msgs[0], "step two") {
		t.Errorf("the new delta must be reviewed:\n%s", msgs[0])
	}
	if strings.Contains(msgs[0], "step one") {
		t.Error("the lost delta must NOT be replayed — that is the at-most-once tradeoff")
	}
}

// Advice reaches a working agent as a message inside the turn it is
// already running. Treating that turn as "the advice turn" swallowed the
// agent's own work — including the reply it ended with, which is the part
// a reviewer most needs to see. Only advice that starts a fresh turn is
// skipped.
func TestAdviceMidTurnDoesNotSwallowTheTurn(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl", "")
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	// the worker is working when the advice arrives
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "fix the parser"}, now)
	apply(store, "%1", &payload{HookEventName: "PreToolUse", ToolName: "Edit"}, now)
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit",
		Prompt: AdvMarker + " · concern from codex] the retry loop has no cap"}, now)

	w, _ = store.Load("%1")
	if w.SkipNextReview {
		t.Fatal("advice absorbed into a running turn must not mark it as an advice turn")
	}

	// the turn ends with a reply: it must reach the reviewer, and without
	// our own advice quoted back at it
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"capped the retry loop and rebuilt"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "Stop"}, now)

	msgs := sentTo("%2")
	if len(msgs) != 1 {
		t.Fatalf("reviewer got %d messages, want the end-of-turn review", len(msgs))
	}
	if !strings.Contains(msgs[0], "capped the retry loop and rebuilt") {
		t.Errorf("the turn's final reply is missing from the delta:\n%s", msgs[0])
	}
	if strings.Contains(msgs[0], AdvMarker) {
		t.Errorf("the reviewer must not read its own advice back:\n%s", msgs[0])
	}
}

// The worker's conclusions must reach the reviewer whatever prompted the
// turn — including a turn the advisor itself started. Hiding those turns
// was how the reviewer ended up seeing tool names and never an answer.
func TestAdviceStartedTurnIsStillReviewed(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl", "")
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	apply(store, "%1", &payload{HookEventName: "Stop"}, now) // idle
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit",
		Prompt: AdvMarker + " · concern from codex] the retry loop has no cap"}, now)
	if w, _ = store.Load("%1"); w.SkipNextReview {
		t.Fatal("a worker's turn must never be hidden from its reviewer")
	}

	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"capped it at five retries"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "Stop"}, now)

	msgs := sentTo("%2")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "capped it at five retries") {
		t.Fatalf("the answer to the advice must reach the reviewer, got %q", msgs)
	}
	if strings.Contains(msgs[0], AdvMarker) {
		t.Errorf("the reviewer must not read its own advice back:\n%s", msgs[0])
	}
}

// Feeding every turn back means the pair could talk to each other for
// ever. The bound is a streak of deliveries with no word from the user.
func TestAdviceStreakMutesAPairTalkingToItself(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl", "")
	reviewerTr := filepath.Join(dir, "r.jsonl")
	os.WriteFile(reviewerTr, []byte(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"blocker: still wrong"}]}}`+"\n"), 0o644)

	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: reviewerTr}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	// each round: the worker (idle between turns, so the advice is what
	// wakes it) says something, the reviewer answers, the advice goes back
	// — with no human anywhere in the loop
	round := func(n int) {
		appendTranscript(t, workerTr, fmt.Sprintf(
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"round %d"}]}}`+"\n", n))
		at := now.Add(time.Duration(n) * time.Minute)
		apply(store, "%1", &payload{HookEventName: "Stop"}, at) // worker goes idle
		apply(store, "%2", &payload{HookEventName: "Stop"}, at) // verdict -> advice
	}
	for i := 1; i <= adviceMax; i++ {
		round(i)
	}
	if got := len(sentTo("%1")); got != adviceMax {
		t.Fatalf("worker heard %d pieces of advice, want %d", got, adviceMax)
	}

	resetAdvisorLog(t)
	round(adviceMax + 1)
	if got := len(sentTo("%1")); got != adviceMax {
		t.Errorf("advice past the streak must be held, worker heard %d", got)
	}
	if events := advisorEvents(t); countEvent(events, "muted") != 1 {
		t.Errorf("events = %v, want the delivery recorded as muted", events)
	}
	w, _ = store.Load("%1")
	if w.PendingNote == "" {
		t.Error("muted advice must stay pending, not vanish")
	}

	// the user says something: the pair is unmuted and the held note lands
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "carry on"}, now.Add(time.Hour))
	if w, _ = store.Load("%1"); w.AdviceStreak != 0 {
		t.Fatalf("a real prompt must reset the streak, got %d", w.AdviceStreak)
	}
	round(adviceMax + 2)
	if got := len(sentTo("%1")); got != adviceMax+1 {
		t.Errorf("after the user spoke the advisor must deliver again, worker heard %d", got)
	}
}

// The rule the whole live-review design answers to: whatever else
// happens, the conclusion the worker ends a turn with reaches its
// reviewer. Each case here used to lose it.
func TestFinalReplyAlwaysReachesTheReviewer(t *testing.T) {
	cases := []struct {
		name  string
		setUp func(t *testing.T, store *state.Store, dir, workerTr string, now time.Time)
	}{
		{"plain turn", func(*testing.T, *state.Store, string, string, time.Time) {}},
		{"reviewer busy with someone else", func(t *testing.T, store *state.Store, dir, _ string, now time.Time) {
			other := writeTranscript(t, dir, "other.jsonl", "")
			apply(store, "%9", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: other}, now)
			r, _ := store.Load("%2")
			r.AwaitingReviewFor, r.AwaitingSince = "%9", now
			store.Save(r)
		}},
		{"reviewer busy with this very worker", func(t *testing.T, store *state.Store, dir, _ string, now time.Time) {
			r, _ := store.Load("%2")
			r.AwaitingReviewFor, r.AwaitingSince = "%1", now
			store.Save(r)
			appendTranscript(t, filepath.Join(dir, "r.jsonl"),
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"OK"}]}}
`)
		}},
		{"turn started by injected advice", func(t *testing.T, store *state.Store, _, _ string, now time.Time) {
			apply(store, "%1", &payload{HookEventName: "UserPromptSubmit",
				Prompt: AdvMarker + " · concern from codex] cap the retry loop"}, now)
		}},
		{"turn interrupted by injected advice", func(t *testing.T, store *state.Store, _, _ string, now time.Time) {
			apply(store, "%1", &payload{HookEventName: "PreToolUse", ToolName: "Edit"}, now)
			apply(store, "%1", &payload{HookEventName: "UserPromptSubmit",
				Prompt: AdvMarker + " · concern from codex] cap the retry loop"}, now)
		}},
		{"agent was nudged as stuck", func(t *testing.T, store *state.Store, _, _ string, now time.Time) {
			apply(store, "%1", &payload{HookEventName: "UserPromptSubmit",
				Prompt: AdvMarker + " · stuck] Bash ran 5 times with the same failure"}, now)
		}},
	}
	const conclusion = "the migration is safe to run twice"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			store := state.NewStore(t.TempDir())
			now := time.Now()
			resetSent(t)

			workerTr := writeTranscript(t, dir, "w.jsonl", "")
			apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
			apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
				TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
			w, _ := store.Load("%1")
			w.ReviewerPane = "%2"
			store.Save(w)
			c.setUp(t, store, dir, workerTr, now)

			appendTranscript(t, workerTr,
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"`+conclusion+`"}]}}
`)
			apply(store, "%1", &payload{HookEventName: "Stop"}, now)

			// either it went out now, or it is queued behind a review that
			// is still running — in which case the verdict releases it
			if !strings.Contains(strings.Join(sentTo("%2"), "\n"), conclusion) {
				apply(store, "%2", &payload{HookEventName: "Stop"}, now.Add(time.Second))
			}
			delivered := strings.Join(sentTo("%2"), "\n")
			if !strings.Contains(delivered, conclusion) {
				t.Fatalf("the worker's conclusion never reached the reviewer:\n%s", delivered)
			}
			if strings.Contains(delivered, AdvMarker) {
				t.Errorf("injected text must be stripped from the delta:\n%s", delivered)
			}
		})
	}
}

// The way this session actually runs: the user types while the agent is
// still working, so the prompt is absorbed into the running turn and no
// Stop ever fires. The agent's conclusion would then sit unread until
// some later tool round — which is exactly how a finished answer failed
// to reach the reviewer in real use.
func TestPromptDuringWorkFeedsTheConclusion(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl", "")
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "do the thing"}, now)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now)
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done: the migration is idempotent"}]}}
`)
	resetSent(t)

	// no Stop — the user simply types again while the agent is working
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "and now the other one"}, now.Add(time.Minute))

	msgs := sentTo("%2")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "the migration is idempotent") {
		t.Fatalf("the conclusion must be fed when the next prompt closes it, got %q", msgs)
	}

	// our own injected prompts must not trigger that feed
	resetSent(t)
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"more work"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit",
		Prompt: AdvMarker + " · nit from codex] name it better"}, now.Add(2*time.Minute))
	if msgs := sentTo("%2"); len(msgs) != 0 {
		t.Errorf("injected advice is not a boundary, got %q", msgs)
	}
}

// A review request must never be typed into a reviewer that is busy with
// its own turn: that mixes our request into someone else's work and used
// to cost that agent its own conclusion.
func TestRequestWaitsForABusyReviewersOwnTurn(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"worker conclusion"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	// the reviewer is working on something of its own
	apply(store, "%2", &payload{HookEventName: "UserPromptSubmit", Prompt: "read this file for me"}, now)

	apply(store, "%1", &payload{HookEventName: "Stop"}, now)
	if msgs := sentTo("%2"); len(msgs) != 0 {
		t.Fatalf("nothing may be typed into a working reviewer, got %q", msgs)
	}
	r, _ := store.Load("%2")
	if len(r.ReviewQueue) != 1 || !r.ReviewQueue[0].Boundary {
		t.Fatalf("the boundary must be queued, got %+v", r.ReviewQueue)
	}
	w, _ = store.Load("%1")
	if w.ReviewOffset != 0 {
		t.Error("a queued review has not read anything yet: the offset must not move")
	}

	// the reviewer finishes its own turn -> the queue is served
	apply(store, "%2", &payload{HookEventName: "Stop"}, now.Add(time.Second))
	msgs := sentTo("%2")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "worker conclusion") {
		t.Fatalf("the queued review must run once the reviewer is free, got %q", msgs)
	}
}

// A mid-turn request that still slips in (a pasted one, a race) must not
// hide the mixed turn it landed in.
func TestRequestMidTurnDoesNotSwallowTheReviewersTurn(t *testing.T) {
	store := state.NewStore(t.TempDir())
	dir := t.TempDir()
	now := time.Now()

	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	apply(store, "%2", &payload{HookEventName: "UserPromptSubmit", Prompt: "my own work"}, now)
	apply(store, "%2", &payload{HookEventName: "UserPromptSubmit", Prompt: ReqMarker + " delta"}, now)

	r, _ := store.Load("%2")
	if r.SkipNextReview {
		t.Error("the reviewer was mid-turn: its own work must not be swallowed")
	}
}

// The queue is bounded, but a bound must not eat a finished turn.
func TestFullQueueStillTakesABoundary(t *testing.T) {
	reviewer := &state.Pane{PaneID: "%2", AwaitingReviewFor: "%99"}
	for i := 0; i < queueLimit; i++ {
		reviewer.ReviewQueue = append(reviewer.ReviewQueue,
			state.QueuedReview{Pane: fmt.Sprintf("%%%d", 100+i)})
	}
	cfg, _ := config.Load()
	if enqueueReview(reviewer, "%1", false, cfg) {
		t.Error("a mid-turn look may be dropped when the queue is full")
	}
	if !enqueueReview(reviewer, "%1", true, cfg) {
		t.Fatal("a turn boundary must find room in a full queue")
	}
	if len(reviewer.ReviewQueue) != queueLimit {
		t.Errorf("queue length = %d, want the oldest mid-turn entry evicted", len(reviewer.ReviewQueue))
	}
	last := reviewer.ReviewQueue[len(reviewer.ReviewQueue)-1]
	if last.Pane != "%1" || !last.Boundary {
		t.Errorf("last entry = %+v, want the boundary for %%1", last)
	}
	// an existing entry is upgraded, never duplicated
	if !enqueueReview(reviewer, "%101", true, cfg) {
		t.Error("a queued mid-turn entry must be upgradable to a boundary")
	}
	seen := 0
	for _, q := range reviewer.ReviewQueue {
		if q.Pane == "%101" {
			seen++
			if !q.Boundary {
				t.Error("the entry was not upgraded")
			}
		}
	}
	if seen != 1 {
		t.Errorf("%%101 appears %d times, want exactly one entry", seen)
	}
}

func TestStripInjectedRemovesWholeBlocks(t *testing.T) {
	delta := "agent: first\n" +
		"user: " + AdvMarker + " · concern from codex] line one\n" +
		"line two of the same advice\n" +
		"agent: my answer\n" +
		"user: " + ReqMarker + "\n" +
		"review boilerplate\n" +
		"tools: Bash(go test ./...)\n"
	got := stripInjected(delta)
	for _, gone := range []string{AdvMarker, ReqMarker, "line two of the same advice", "review boilerplate"} {
		if strings.Contains(got, gone) {
			t.Errorf("injected text survived (%q):\n%s", gone, got)
		}
	}
	for _, kept := range []string{"agent: first", "agent: my answer", "tools: Bash(go test ./...)"} {
		if !strings.Contains(got, kept) {
			t.Errorf("real work was stripped (%q):\n%s", kept, got)
		}
	}
}

// Internal harness traffic is not the user speaking, so it must not
// unmute an advisor that has been talking to itself.
func TestInternalPromptsDoNotResetTheAdviceStreak(t *testing.T) {
	store := state.NewStore(t.TempDir())
	now := time.Now()
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: "/proj"}, now)
	p, _ := store.Load("%1")
	p.AdviceStreak = adviceMax
	store.Save(p)

	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "<task-notification>agent x finished"}, now)
	if p, _ = store.Load("%1"); p.AdviceStreak != adviceMax {
		t.Errorf("AdviceStreak = %d, internal traffic must not reset it", p.AdviceStreak)
	}
	if p.LastPrompt != "" {
		t.Errorf("LastPrompt = %q, internal traffic is not a label", p.LastPrompt)
	}

	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "carry on"}, now)
	if p, _ = store.Load("%1"); p.AdviceStreak != 0 {
		t.Error("a human prompt must reset the streak")
	}
}

// The measured cause of a conclusion never reaching the reviewer: the
// Stop hook and the transcript write race, and the hook can win by
// milliseconds. The turn is then over with nothing to review, and no
// later event is obliged to look again.
func TestBoundaryOwedWhenTheTranscriptLags(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)
	resetAdvisorLog(t)

	workerTr := writeTranscript(t, dir, "w.jsonl", "")
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	// the turn ends before its last words are on disk
	apply(store, "%1", &payload{HookEventName: "Stop"}, now)
	if msgs := sentTo("%2"); len(msgs) != 0 {
		t.Fatalf("there was nothing to send yet, got %q", msgs)
	}
	w, _ = store.Load("%1")
	if !w.BoundaryOwed {
		t.Fatal("the turn owes a review and must say so")
	}
	if events := advisorEvents(t); countEvent(events, "skipped") != 1 {
		t.Errorf("events = %v, want the skipped boundary recorded", events)
	}

	// the words land a moment later; the next event of any kind — even a
	// rate-limited mid-turn one — must carry them
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"and that is the conclusion"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now.Add(time.Second))

	msgs := sentTo("%2")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "and that is the conclusion") {
		t.Fatalf("the owed review must run at the next event, got %q", msgs)
	}
	if !strings.Contains(msgs[0], "just finished its turn") {
		t.Errorf("the owed review is still a turn boundary:\n%s", msgs[0])
	}
	w, _ = store.Load("%1")
	if w.BoundaryOwed {
		t.Error("the debt must be cleared once it is paid")
	}
}

// The grace period covers the common case without waiting for a later
// event at all: the words land while the hook is still running.
func TestBoundaryWaitsBrieflyForTheTranscript(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl", "")
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	withBoundaryGrace(t, 2000)

	go func() {
		time.Sleep(150 * time.Millisecond)
		appendTranscript(t, workerTr,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"written just after the hook"}]}}
`)
	}()
	apply(store, "%1", &payload{HookEventName: "Stop"}, now)

	msgs := sentTo("%2")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "written just after the hook") {
		t.Fatalf("the boundary must wait out a late write, got %q", msgs)
	}
	if w, _ = store.Load("%1"); w.BoundaryOwed {
		t.Error("nothing is owed when the wait succeeded")
	}
}

// The exact live sequence, end to end: the user types while the agent is
// working (so the prompt is the boundary), the agent's conclusion is
// still unwritten at that instant, and it lands a moment later. It must
// be reviewed — once, not twice — and the debt must survive a failed send.
func TestOwedBoundaryDeliversExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl", "")
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "do the thing"}, now)

	// the user types again before the conclusion is on disk
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "and one more thing"}, now.Add(time.Second))
	if msgs := sentTo("%2"); len(msgs) != 0 {
		t.Fatalf("nothing was written yet, so nothing can be reviewed: %q", msgs)
	}
	if w, _ = store.Load("%1"); !w.BoundaryOwed {
		t.Fatal("the boundary must be remembered")
	}

	// it lands — but the reviewer's pane is momentarily unreachable
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"the conclusion"}]}}
`)
	failSends(t, errors.New("pane not found"))
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now.Add(2*time.Second))
	w, _ = store.Load("%1")
	if !w.BoundaryOwed {
		t.Error("a failed send must not pay off the debt")
	}
	if w.ReviewOffset != 0 {
		t.Error("nor may it consume the delta")
	}

	// the debt lives in the state file, so a restart of the plugin (a new
	// process, a fresh store over the same directory) still owes it
	reopened := state.NewStore(store.Dir())
	if w, _ = reopened.Load("%1"); !w.BoundaryOwed {
		t.Error("the debt must survive a restart — it is state, not memory")
	}

	// the pane is back: the conclusion is reviewed, as a boundary, once
	resetSent(t)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now.Add(3*time.Second))
	msgs := sentTo("%2")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "the conclusion") {
		t.Fatalf("the owed boundary must be delivered, got %q", msgs)
	}
	if !strings.Contains(msgs[0], "just finished its turn") {
		t.Errorf("it is still a boundary review:\n%s", msgs[0])
	}
	if w, _ = store.Load("%1"); w.BoundaryOwed {
		t.Error("paid debts must be cleared")
	}

	// and never a second time
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now.Add(4*time.Second))
	apply(store, "%1", &payload{HookEventName: "Stop"}, now.Add(5*time.Second))
	if msgs := sentTo("%2"); len(msgs) != 1 {
		t.Errorf("the same conclusion was reviewed %d times, want once", len(msgs))
	}
}

// The mute rule exists for a pair left alone together. It must never
// silence review of work the user set going: a long turn can take many
// review rounds, and every one of them is the reviewer doing its job.
func TestAdviceIntoAWorkingAgentIsNeverMuted(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl", "")
	reviewerTr := filepath.Join(dir, "r.jsonl")
	os.WriteFile(reviewerTr, []byte(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"blocker: still wrong"}]}}`+"\n"), 0o644)

	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: reviewerTr}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	// one long user-driven turn, reviewed round after round
	resetAdvisorLog(t)
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "do the big thing"}, now)
	for i := 1; i <= adviceMax+3; i++ {
		appendTranscript(t, workerTr, fmt.Sprintf(
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"step %d"}]}}`+"\n", i))
		at := now.Add(time.Duration(i) * time.Minute)
		apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, at)
		apply(store, "%2", &payload{HookEventName: "Stop"}, at)
	}

	// hold-and-reconfirm still paces WHAT is said (every other round here);
	// what must not happen is the advisor falling silent altogether
	events := advisorEvents(t)
	if n := countEvent(events, "muted"); n != 0 {
		t.Errorf("%d deliveries muted — advice into a working agent must never be", n)
	}
	delivered := len(sentTo("%1"))
	if delivered <= adviceMax-1 {
		t.Errorf("the worker heard %d pieces of advice over %d rounds: the advisor went quiet",
			delivered, adviceMax+3)
	}
	if w, _ = store.Load("%1"); w.AdviceStreak != 0 {
		t.Errorf("AdviceStreak = %d: only advice that starts a turn counts toward the bound", w.AdviceStreak)
	}
}

// The reviewer's Stop races its own transcript write exactly as the
// worker's does. An empty read must not pass for "OK" — that would
// silently resolve a concern the reviewer is still holding.
func TestEmptyVerdictIsNotAnOK(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)
	resetAdvisorLog(t)

	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "w.jsonl", "")}, now)
	reviewerTr := writeTranscript(t, dir, "r.jsonl", "")
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: reviewerTr}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	w.PendingSeverity, w.PendingNote = sevBlocker, "the retry loop has no cap"
	store.Save(w)
	r, _ := store.Load("%2")
	r.AwaitingReviewFor, r.AwaitingSince = "%1", now
	store.Save(r)

	// the reviewer stops with nothing written yet
	apply(store, "%2", &payload{HookEventName: "Stop"}, now)

	w, _ = store.Load("%1")
	if w.PendingSeverity != sevBlocker || w.PendingNote == "" {
		t.Errorf("a held note must survive an unreadable verdict, got %q/%q", w.PendingSeverity, w.PendingNote)
	}
	if events := advisorEvents(t); countEvent(events, "no_verdict") != 1 {
		t.Errorf("events = %v, want the empty read recorded", events)
	}
	if r, _ = store.Load("%2"); r.AwaitingReviewFor != "%1" || !r.VerdictOwed {
		t.Fatalf("the review must stay owed for a retry, got %+v", r)
	}

	// the words land, and the reviewer's next event of any kind pays it —
	// the concern the user can see in its pane reaches the worker
	appendTranscript(t, reviewerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"blocker: the retry loop still has no cap"}]}}
`)
	resetSent(t)
	apply(store, "%2", &payload{HookEventName: "PostToolUse", ToolName: "Read"}, now.Add(time.Second))

	msgs := sentTo("%1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "the retry loop still has no cap") {
		t.Fatalf("the retried verdict must reach the worker, got %q", msgs)
	}
	if r, _ = store.Load("%2"); r.AwaitingReviewFor != "" || r.VerdictOwed {
		t.Errorf("a paid review leaves nothing owed, got %+v", r)
	}
}

// The sequence measured in real use: at the Stop hook the turn's tool
// calls are already in the transcript but its closing words are not. The
// delta is therefore not empty, so the boundary looked paid — and the
// reviewer judged a turn it had only half seen, while the conclusion
// travelled with the NEXT turn.
func TestBoundaryOwedUntilTheFinalTextLands(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	// tools recorded, no closing text yet
	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl", "")}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	apply(store, "%1", &payload{HookEventName: "Stop"}, now)

	// what went out is fine; what matters is that the turn is not settled
	if msgs := sentTo("%2"); len(msgs) != 1 || !strings.Contains(msgs[0], "Bash(go test ./...)") {
		t.Fatalf("the work so far should be reviewed, got %q", msgs)
	}
	w, _ = store.Load("%1")
	if !w.BoundaryOwed {
		t.Fatal("a boundary without the turn's final words is only half paid")
	}

	// the reviewer answers the half-turn, freeing itself
	os.WriteFile(filepath.Join(dir, "r.jsonl"), []byte(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"OK"}]}}`+"\n"), 0o644)
	apply(store, "%2", &payload{HookEventName: "Stop"}, now.Add(time.Second))

	// the closing words land a moment later
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"all green, the migration is idempotent"}]}}
`)
	resetSent(t)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now.Add(time.Second))

	msgs := sentTo("%2")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "all green, the migration is idempotent") {
		t.Fatalf("the conclusion must follow as its own boundary review, got %q", msgs)
	}
	if !strings.Contains(msgs[0], "just finished its turn") {
		t.Errorf("it is the turn's boundary, not a mid-turn look:\n%s", msgs[0])
	}
	w, _ = store.Load("%1")
	if w.BoundaryOwed {
		t.Error("the debt is paid once the conclusion is in")
	}

	// and never twice
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now.Add(2*time.Second))
	apply(store, "%1", &payload{HookEventName: "Stop"}, now.Add(3*time.Second))
	if got := len(sentTo("%2")); got != 1 {
		t.Errorf("the conclusion was reviewed %d times, want once", got)
	}
}

// The whole chain in one place, with the worker's state at each step:
// feed while working -> verdict -> hold -> reconfirm -> delivery, and the
// same delivery landing on an idle agent counts toward the self-talk bound.
func TestFeedVerdictDeliveryEndToEnd(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)
	resetAdvisorLog(t)

	workerTr := writeTranscript(t, dir, "w.jsonl", "")
	reviewerTr := filepath.Join(dir, "r.jsonl")
	verdict := func(text string) {
		os.WriteFile(reviewerTr, []byte(
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"`+text+`"}]}}`+"\n"), 0o644)
	}
	verdict("concern: the retry loop has no cap")

	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: reviewerTr}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	// 1. the user sets the worker going; a tool round feeds the reviewer
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "add retries"}, now)
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"added a retry loop"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Edit"}, now)
	if r, _ := store.Load("%2"); r.AwaitingReviewFor != "%1" {
		t.Fatal("the reviewer must be reviewing the worker")
	}

	// 2. first sighting of a concern, mid-turn: held, the worker hears nothing
	apply(store, "%2", &payload{HookEventName: "Stop"}, now.Add(time.Second))
	w, _ = store.Load("%1")
	if w.PendingSeverity != sevConcern || len(sentTo("%1")) != 0 {
		t.Fatalf("a fresh mid-turn concern is held, got %+v / %q", w.PendingSeverity, sentTo("%1"))
	}
	if w.Status != state.StatusWorking {
		t.Fatal("the worker is still working — this is the classification the bound depends on")
	}

	// 3. raised again while the worker still works: delivered, never muted
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"still working on it"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Edit"}, now.Add(2*time.Second))
	verdict("concern: the retry loop still has no cap")
	apply(store, "%2", &payload{HookEventName: "Stop"}, now.Add(3*time.Second))

	msgs := sentTo("%1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "still has no cap") {
		t.Fatalf("a reconfirmed concern must reach the worker, got %q", msgs)
	}
	w, _ = store.Load("%1")
	if w.AdviceStreak != 0 {
		t.Errorf("AdviceStreak = %d: advice into user-driven work is not self-talk", w.AdviceStreak)
	}
	events := advisorEvents(t)
	for _, want := range []string{"feed", "verdict", "hold", "deliver"} {
		if countEvent(events, want) == 0 {
			t.Errorf("events = %v, missing %q", events, want)
		}
	}
	if countEvent(events, "muted") != 0 {
		t.Errorf("events = %v, nothing here is self-talk", events)
	}

	// 4. the same delivery to an idle worker does count toward the bound:
	// its turn ends, the boundary review goes out, and the verdict comes
	// back to an agent that is waiting — the advice is what wakes it
	appendTranscript(t, workerTr,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"idle answer"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "Stop"}, now.Add(4*time.Second))
	if w, _ = store.Load("%1"); w.Status == state.StatusWorking {
		t.Fatal("the worker must be idle for this half of the check")
	}
	verdict("concern: the retry loop still has no cap")
	apply(store, "%2", &payload{HookEventName: "Stop"}, now.Add(6*time.Second))
	if w, _ = store.Load("%1"); w.AdviceStreak != 1 {
		t.Errorf("AdviceStreak = %d after waking an idle agent, want 1", w.AdviceStreak)
	}
}

// The queue has a hard ceiling so a state file cannot grow without end.
// Reaching it must delay a conclusion, never lose one.
func TestBoundarySurvivesAFullQueueCeiling(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	resetSent(t)

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"the conclusion nobody may lose"}]}}
`)
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir,
		TranscriptPath: writeTranscript(t, dir, "r.jsonl",
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"OK"}]}}
`)}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	// the reviewer is busy and its queue is full to the hard ceiling, all
	// of it boundaries — nothing can be evicted to make room
	r, _ := store.Load("%2")
	r.AwaitingReviewFor, r.AwaitingSince = "%9", now
	for i := 0; i < hardQueueFactor*queueLimit; i++ {
		r.ReviewQueue = append(r.ReviewQueue,
			state.QueuedReview{Pane: fmt.Sprintf("%%%d", 100+i), Boundary: true})
	}
	store.Save(r)

	apply(store, "%1", &payload{HookEventName: "Stop"}, now)

	if msgs := sentTo("%2"); len(msgs) != 0 {
		t.Fatalf("there was no room to send anything, got %q", msgs)
	}
	w, _ = store.Load("%1")
	if !w.BoundaryOwed {
		t.Fatal("a boundary refused by the ceiling must be remembered, not dropped")
	}
	if w.ReviewOffset != 0 {
		t.Error("nothing was reviewed, so nothing may be consumed")
	}
	// the debt is state, not memory: a restarted plugin still owes it
	if reopened, _ := state.NewStore(store.Dir()).Load("%1"); !reopened.BoundaryOwed {
		t.Error("the debt must survive a restart")
	}

	// the reviewer drains and comes free; the conclusion is reviewed as
	// the boundary it always was
	r, _ = store.Load("%2")
	r.ClearAwaiting()
	r.ReviewQueue = nil
	store.Save(r)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now.Add(time.Second))

	msgs := sentTo("%2")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "the conclusion nobody may lose") {
		t.Fatalf("the delayed conclusion must still arrive, got %q", msgs)
	}
	if !strings.Contains(msgs[0], "just finished its turn") {
		t.Errorf("and still as a turn boundary:\n%s", msgs[0])
	}
	if w, _ = store.Load("%1"); w.BoundaryOwed {
		t.Error("the debt is paid once")
	}

	// and paid exactly once: no later event repeats it
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now.Add(2*time.Second))
	apply(store, "%1", &payload{HookEventName: "Stop"}, now.Add(3*time.Second))
	apply(store, "%2", &payload{HookEventName: "Stop"}, now.Add(4*time.Second))
	if got := len(sentTo("%2")); got != 1 {
		t.Errorf("the conclusion was reviewed %d times, want once", got)
	}
}

// An agent can exit and leave its pane behind — /exit, a crash, Ctrl+C
// on a CLI that fires no SessionEnd. The pane is still alive, so a
// pane-based liveness check kept the entry for ever: the sidebar and the
// popups went on offering an agent that was not there.
func TestReconcileDepartedDropsAgentsThatLeftTheirPane(t *testing.T) {
	store := state.NewStore(t.TempDir())
	now := time.Now()
	for _, p := range []*state.Pane{
		{PaneID: "%1", Agent: "claude", Status: state.StatusWaitingInput, Cwd: "/gone", UpdatedAt: now},
		{PaneID: "%1~abc", Agent: "claude", ParentPane: "%1", Status: state.StatusDone, UpdatedAt: now},
		{PaneID: "%2", Agent: "claude", Status: state.StatusWorking, Cwd: "/busy", UpdatedAt: now},
		{PaneID: "%3", Agent: "claude", Status: state.StatusWorking, Cwd: "/tool", UpdatedAt: now},
	} {
		if err := store.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	panes, _ := store.List()
	store.Save(&state.Pane{PaneID: "%5", Agent: "claude", Status: state.StatusWaitingInput,
		Cwd: "/editor", UpdatedAt: now})
	panes, _ = store.List()
	infos := []tmuxctl.PaneInfo{
		{ID: "%1", Command: "zsh", PID: 111},     // the agent left, the shell stayed
		{ID: "%2", Command: "2.1.234", PID: 222}, // still the agent's own TUI
		{ID: "%3", Command: "zsh", PID: 333},     // a shell, but the agent runs a Bash tool
		{ID: "%5", Command: "nvim", PID: 555},    // the user took the pane over
	}
	hasAgent := func(pid int) bool { return pid == 333 }
	const quiet = time.Minute

	if !reconcileDeparted(store, panes, infos, hasAgent, quiet, now.Add(2*time.Minute)) {
		t.Fatal("the departed agent must be noticed")
	}
	left := map[string]bool{}
	fresh, _ := store.List()
	for _, p := range fresh {
		left[p.PaneID] = true
	}
	if left["%1"] {
		t.Error("an agent whose pane is a shell again must be dropped")
	}
	if left["%1~abc"] {
		t.Error("its teammates go with it")
	}
	if !left["%2"] {
		t.Error("a running agent must be left alone")
	}
	if !left["%3"] {
		t.Error("a shell pane with the agent still in its process tree is working, not gone")
	}
	if left["%5"] {
		t.Error("a pane the user gave to an editor holds no agent either")
	}

	// a daemon-hosted session runs outside the pane's process tree: a
	// transcript still being written is what says it is alive
	daemon := filepath.Join(t.TempDir(), "d.jsonl")
	os.WriteFile(daemon, []byte("{}\n"), 0o644)
	// written "just now" as of the moment the check runs
	written := now.Add(2 * time.Minute)
	os.Chtimes(daemon, written, written)
	store.Save(&state.Pane{PaneID: "%4", Agent: "claude", Status: state.StatusWaitingInput,
		Cwd: "/daemon", TranscriptPath: daemon, UpdatedAt: now})
	panes, _ = store.List()
	infos = append(infos, tmuxctl.PaneInfo{ID: "%4", Command: "zsh", PID: 444})
	reconcileDeparted(store, panes, infos, func(int) bool { return false }, quiet, now.Add(2*time.Minute))
	if _, err := store.Load("%4"); err != nil {
		t.Error("an agent whose transcript is still being written is alive, wherever its process lives")
	}

	// an agent that wrote state a moment ago is not a suspect at all: a
	// shell in its pane is a Bash tool, and no process scan is spent on it
	fresh, _ = store.List()
	if reconcileDeparted(store, fresh, infos,
		func(int) bool { t.Fatal("a recently active agent must not cost a process scan"); return false },
		quiet, now) {
		t.Error("nothing is departed while the state is fresh")
	}
}

// A late event from an agent whose pane is gone can resolve onto some
// unrelated shell that happens to sit in the same directory. Tracking it
// puts an agent in the sidebar where the user can see there is none — and
// it comes back every time such an event lands, so cleanup alone is not
// an answer.
func TestUnknownPaneWithoutAnAgentIsNotTracked(t *testing.T) {
	store := state.NewStore(t.TempDir())
	now := time.Now()

	// tmux is unreachable in tests, so paneRunsAgent cannot tell and the
	// event is kept — a hook is never dropped on a guess
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: "/proj"}, now)
	if _, err := store.Load("%1"); err != nil {
		t.Fatal("an undecidable pane must still be tracked")
	}

	// an already-tracked pane is updated whatever its command is now: the
	// agent may simply be running a Bash tool
	apply(store, "%1", &payload{HookEventName: "Stop"}, now)
	if p, _ := store.Load("%1"); p.Status != state.StatusDone {
		t.Error("a tracked pane must keep receiving its events")
	}
}

// Picking a reviewer must work for a pane whose agent has not spoken to
// us yet: its hooks may be minutes old, or its last session ended and the
// new one has fired nothing so far. The user can see what is in the pane;
// that beats waiting for an event.
func TestTrackPaneAdoptsWhatTheUserPointsAt(t *testing.T) {
	store := state.NewStore(t.TempDir())
	now := time.Now()

	// tmux is unreachable in tests, so the pane cannot be inspected
	if err := TrackPane(store, "%354", now); err == nil {
		t.Error("without tmux there is nothing to adopt")
	}

	// an already-tracked pane is left exactly as it was
	store.Save(&state.Pane{PaneID: "%9", Agent: "codex", Cwd: "/proj", SessionID: "s1"})
	if err := TrackPane(store, "%9", now); err != nil {
		t.Fatal(err)
	}
	if p, _ := store.Load("%9"); p.SessionID != "s1" {
		t.Error("tracking must not overwrite what the hooks already know")
	}
}

func TestAgentKindInPane(t *testing.T) {
	cases := map[string]string{
		"2.1.235": "claude", "claude": "claude", "codex": "codex",
		"opencode": "opencode",
		"zsh":      "", "nvim": "", "": "", "node": "",
	}
	for command, want := range cases {
		if got := AgentKindInPane(command); got != want {
			t.Errorf("AgentKindInPane(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		reply    string
		severity string
		text     string
	}{
		{"OK", sevNone, ""},
		{"ok — nothing to add", sevNone, ""},
		{`"OK"`, sevNone, ""},
		{"nit: rename the helper", sevNit, "rename the helper"},
		{"concern: the retry loop has no cap", sevConcern, "the retry loop has no cap"},
		{"Blocker - you are rerunning the same test", sevBlocker, "you are rerunning the same test"},
		{"You forgot to handle the error", sevConcern, "You forgot to handle the error"},
		{"", sevNone, ""},
	}
	for _, c := range cases {
		sev, text := parseVerdict(c.reply)
		if sev != c.severity || text != c.text {
			t.Errorf("parseVerdict(%q) = (%q, %q), want (%q, %q)", c.reply, sev, text, c.severity, c.text)
		}
	}
}

// A mid-turn concern must be held on first sight and delivered only when
// the next review raises it again; silence in between means it is gone.
func TestHoldAndReconfirm(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"step one"}]}}
`)
	reviewerTr := filepath.Join(dir, "r.jsonl")
	setVerdict := func(text string) {
		line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
		os.WriteFile(reviewerTr, []byte(line), 0o644)
	}
	setVerdict("concern: the retry loop has no cap")

	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: reviewerTr}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	// mid-turn tool round -> review is requested
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now)
	r, _ := store.Load("%2")
	if r.AwaitingReviewFor != "%1" {
		t.Fatal("PostToolUse must trigger a mid-turn review")
	}

	// first concern: held, worker hears nothing
	apply(store, "%2", &payload{HookEventName: "Stop"}, now)
	w, _ = store.Load("%1")
	if w.PendingSeverity != sevConcern || w.PendingNote == "" {
		t.Fatalf("first concern must be held: %+v", w)
	}
	if w.SkipNextReview {
		t.Error("nothing was delivered, so the next turn must still be reviewed")
	}

	// worker keeps working, the reviewer raises it again -> delivered now
	appendLine(t, workerTr, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"step two"}]}}`)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now)
	setVerdict("concern: still no cap on the retry loop")
	apply(store, "%2", &payload{HookEventName: "Stop"}, now)
	w, _ = store.Load("%1")
	if w.PendingNote != "" {
		t.Errorf("reconfirmed note must be cleared after delivery: %+v", w)
	}
	if w.SkipNextReview {
		t.Error("the worker was mid-turn: its own work must not be swallowed by the advice")
	}

	// a held note that the reviewer no longer raises is dropped
	w.PendingSeverity, w.PendingNote, w.SkipNextReview = sevBlocker, "old worry", false
	store.Save(w)
	appendLine(t, workerTr, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"step three"}]}}`)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now)
	setVerdict("OK")
	apply(store, "%2", &payload{HookEventName: "Stop"}, now)
	w, _ = store.Load("%1")
	if w.PendingNote != "" || w.PendingSeverity != "" {
		t.Errorf("an OK verdict must resolve the held note: %+v", w)
	}
	if w.SkipNextReview {
		t.Error("nothing delivered — no skip expected")
	}
}

// Nits wait for the end of the turn.
func TestNitWaitsForBoundary(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	workerTr := writeTranscript(t, dir, "w.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"work"}]}}
`)
	reviewerTr := filepath.Join(dir, "r.jsonl")
	os.WriteFile(reviewerTr, []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"nit: name it better"}]}}`+"\n"), 0o644)

	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: workerTr}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: reviewerTr}, now)
	w, _ := store.Load("%1")
	w.ReviewerPane = "%2"
	store.Save(w)

	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Read"}, now)
	apply(store, "%2", &payload{HookEventName: "Stop"}, now)
	w, _ = store.Load("%1")
	if w.PendingSeverity != sevNit {
		t.Fatalf("nit must be queued, got %+v", w)
	}
	if w.SkipNextReview {
		t.Error("a queued nit must not be delivered mid-turn")
	}

	// end of the worker's turn -> the queued nit is flushed
	appendLine(t, workerTr, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
	apply(store, "%1", &payload{HookEventName: "Stop"}, now)
	apply(store, "%2", &payload{HookEventName: "Stop"}, now)
	w, _ = store.Load("%1")
	if w.PendingNote != "" {
		t.Errorf("nit must be flushed at the boundary: %+v", w)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePaneBySessionID(t *testing.T) {
	t.Setenv("TMUX_PANE", "")
	store := state.NewStore(t.TempDir())
	now := time.Now()
	apply(store, "%7", &payload{HookEventName: "SessionStart", SessionID: "sess-42", Cwd: "/x"}, now)

	pane := resolvePane(store, &payload{SessionID: "sess-42"})
	if pane != "%7" {
		t.Errorf("resolvePane = %q, want %%7 (matched by session id)", pane)
	}
	if p := resolvePane(store, &payload{SessionID: "unknown"}); p != "" {
		t.Errorf("unknown session without cwd must not resolve, got %q", p)
	}
}

func TestReconcileStaleWorking(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	tr := writeTranscript(t, dir, "stale.jsonl",
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"do it"}]}}
`)
	old := now.Add(-30 * time.Minute)
	os.Chtimes(tr, old, old)
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "do it", TranscriptPath: tr}, now)

	fresh := writeTranscript(t, dir, "fresh.jsonl",
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"busy"}]}}
`)
	apply(store, "%2", &payload{HookEventName: "UserPromptSubmit", Prompt: "busy", TranscriptPath: fresh}, now)

	if !ReconcileInterrupted(store, mustList(t, store)) {
		t.Fatal("expected the stale pane to change")
	}
	p1, _ := store.Load("%1")
	if p1.Status != state.StatusWaitingInput {
		t.Errorf("stale-transcript pane = %s, want waiting_input", p1.Status)
	}
	p2, _ := store.Load("%2")
	if p2.Status != state.StatusWorking {
		t.Errorf("fresh pane = %s, must stay working", p2.Status)
	}
}

func mustList(t *testing.T, store *state.Store) []*state.Pane {
	t.Helper()
	panes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	return panes
}

// One reviewer serving two workers: the second is queued while the first
// is being reviewed, and starts as soon as the reviewer is free.
func TestReviewQueue(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	trA := writeTranscript(t, dir, "a.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"worker A step"}]}}
`)
	trB := writeTranscript(t, dir, "b.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"worker B step"}]}}
`)
	trRev := writeTranscript(t, dir, "rev.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"OK"}]}}
`)

	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: trA}, now)
	apply(store, "%3", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: trB}, now)
	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: trRev}, now)
	for _, w := range []string{"%1", "%3"} {
		p, _ := store.Load(w)
		p.ReviewerPane = "%2"
		store.Save(p)
	}

	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now)
	r, _ := store.Load("%2")
	if r.AwaitingReviewFor != "%1" {
		t.Fatalf("first worker must be in review, got %q", r.AwaitingReviewFor)
	}

	apply(store, "%3", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now)
	r, _ = store.Load("%2")
	if len(r.ReviewQueue) != 1 || r.ReviewQueue[0].Pane != "%3" {
		t.Fatalf("second worker must be queued: %+v", r.ReviewQueue)
	}

	// the reviewer answers -> the queued worker is picked up automatically
	apply(store, "%2", &payload{HookEventName: "Stop"}, now)
	r, _ = store.Load("%2")
	if r.AwaitingReviewFor != "%3" {
		t.Errorf("queued worker must start reviewing, got %q", r.AwaitingReviewFor)
	}
	if len(r.ReviewQueue) != 0 {
		t.Errorf("queue must be drained: %+v", r.ReviewQueue)
	}
}

// Mutual pairs: a turn that only answers a review request must never be
// fed to that reviewer's own reviewer, or the pair cascades forever.
func TestReviewTurnIsNotFedOnward(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	trB := writeTranscript(t, dir, "b.jsonl",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"OK"}]}}
`)
	trC := writeTranscript(t, dir, "c.jsonl", "")

	apply(store, "%2", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: trB}, now)
	apply(store, "%3", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: trC}, now)
	b, _ := store.Load("%2")
	b.ReviewerPane = "%3" // B is reviewed by C
	store.Save(b)

	// B receives a review request (it reviews someone else)
	apply(store, "%2", &payload{HookEventName: "UserPromptSubmit", Prompt: ReqMarker + " delta"}, now)
	b, _ = store.Load("%2")
	if !b.SkipNextReview {
		t.Fatal("a review request must mark the coming turn as not-to-be-reviewed")
	}

	apply(store, "%2", &payload{HookEventName: "Stop"}, now)
	c, _ := store.Load("%3")
	if c.AwaitingReviewFor != "" {
		t.Errorf("C must not be asked to review B's review turn, got %q", c.AwaitingReviewFor)
	}

	// the flag covers exactly one turn: it clears at that turn's boundary,
	// and the offset moves past the swallowed review so it is not replayed
	b, _ = store.Load("%2")
	if b.SkipNextReview {
		t.Error("the flag must clear at the boundary of the turn it covered")
	}
	if b.ReviewOffset == 0 {
		t.Error("the swallowed review turn must be consumed, not left to be fed later")
	}

	// B's next turn is ordinary work again — and it does get reviewed
	resetSent(t)
	appendTranscript(t, trB,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"back to my own work"}]}}
`)
	apply(store, "%2", &payload{HookEventName: "Stop"}, now.Add(time.Minute))
	if msgs := sentTo("%3"); len(msgs) != 1 || !strings.Contains(msgs[0], "back to my own work") {
		t.Errorf("the turn after a review turn must be fed onward, got %q", msgs)
	}
}

// The detectors mark an agent stuck from its own tool history, and let go
// as soon as it does something else.
func TestDetectStuckLifecycle(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(t.TempDir())
	now := time.Now()

	loop := `{"type":"assistant","timestamp":"2026-08-18T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"FAIL: TestFoo","is_error":true}]}}
{"type":"assistant","timestamp":"2026-08-18T10:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"FAIL: TestFoo","is_error":true}]}}
`
	tr := writeTranscript(t, dir, "w.jsonl", loop)
	apply(store, "%1", &payload{HookEventName: "SessionStart", Cwd: dir, TranscriptPath: tr}, now)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Bash"}, now)

	p, _ := store.Load("%1")
	if p.StuckKind != "same_failure" {
		t.Fatalf("expected a same_failure finding, got %+v", p)
	}
	if p.Display() != state.StatusStuck {
		t.Errorf("display status = %s, want stuck", p.Display())
	}
	if p.Status != state.StatusWorking {
		t.Errorf("the underlying lifecycle status must stay working, got %s", p.Status)
	}

	// the agent tries something different -> the finding is dropped
	appendLine(t, tr, `{"type":"assistant","timestamp":"2026-08-18T10:02:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Read","input":{"file_path":"/foo.go"}}]}}`)
	appendLine(t, tr, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":"package foo"}]}}`)
	apply(store, "%1", &payload{HookEventName: "PostToolUse", ToolName: "Read"}, now)
	p, _ = store.Load("%1")
	if p.StuckReason != "" {
		t.Errorf("finding must clear once the agent moves on: %+v", p)
	}

	// a fresh user instruction also clears any finding
	p.StuckKind, p.StuckReason, p.StuckSignature = "same_failure", "old", "sig"
	store.Save(p)
	apply(store, "%1", &payload{HookEventName: "UserPromptSubmit", Prompt: "try another way"}, now)
	p, _ = store.Load("%1")
	if p.StuckReason != "" {
		t.Errorf("a new user prompt must clear the finding: %+v", p)
	}
}
