// Package detect finds agents that are stuck, from their tool calls
// alone: no model, no network, microseconds. It is deliberately pure —
// a slice of tool calls in, a finding out — so the same code runs live
// from hooks and offline over recorded transcripts when tuning.
package detect

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"

	"github.com/mrybas/tmux-agent-hub/internal/transcript"
)

// Thresholds tune how eager the detectors are. Zero disables a detector.
type Thresholds struct {
	Window      int // how many recent calls are considered
	Repeat      int // identical call repeated this many times
	SameFailure int // identical failing result this many times
	ErrorStreak int // consecutive failing calls
	Oscillation int // length of an A-B-A-B run
	NoProgress  int // calls in a row without editing anything
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		Window:      20,
		Repeat:      5,
		SameFailure: 2,
		ErrorStreak: 3,
		Oscillation: 4,
		NoProgress:  25,
	}
}

// Finding is what a detector saw. Kind is stable (config, metrics),
// Reason is what a human — or the agent itself — gets told.
type Finding struct {
	Kind      string // repeat | same_failure | error_streak | oscillation | no_progress
	Reason    string
	Signature string // what repeated, for cooldown bookkeeping
	Count     int
}

// mutatingTools are the obvious evidence that work is happening.
var mutatingTools = map[string]bool{
	"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true,
	"apply_patch": true, "write_file": true, "edit_file": true,
}

// writingShell spots a shell command that changes files. Agents edit
// through the shell more often than one would think — measured on real
// runs, whole sessions can finish without a single Edit call — and
// counting those as "no progress" would be plain wrong.
var writingShell = regexp.MustCompile(
	`(^|[\s;&|(])(sed\s+-i|tee\b|cp\b|mv\b|rm\b|mkdir\b|touch\b|patch\b|git\s+(apply|checkout|commit|mv|rm)\b)|>>?\s*[^\s|&]`)

// mutating reports whether a call changed the workspace.
func mutating(c transcript.ToolCall) bool {
	if mutatingTools[c.Tool] {
		return true
	}
	if !commandTools[c.Tool] {
		return false
	}
	text := c.Input
	if strings.TrimSpace(text) == "" {
		text = c.Arg
	}
	return writingShell.MatchString(text)
}

var digits = regexp.MustCompile(`\d+`)

// Signature identifies "the same call again": the tool plus a hash of its
// FULL arguments with volatile parts (numbers, whitespace) normalized
// away. Hashing the whole input matters — two long shell commands often
// share their first hundred characters, and comparing prefixes would call
// them the same call.
func Signature(c transcript.ToolCall) string {
	full := c.Input
	if strings.TrimSpace(full) == "" {
		full = c.Arg
	}
	norm := digits.ReplaceAllString(strings.Join(strings.Fields(full), " "), "#")
	h := fnv.New64a()
	h.Write([]byte(norm))
	return fmt.Sprintf("%s|%x", c.Tool, h.Sum64())
}

// resultKey normalizes a tool result so that "the same failure" survives
// changing durations, line numbers and paths of temp files.
func resultKey(c transcript.ToolCall) string {
	text := digits.ReplaceAllString(strings.Join(strings.Fields(c.Result), " "), "#")
	if r := []rune(text); len(r) > 400 {
		text = string(r[:400])
	}
	return text
}

// failed asks the transcript, not the text: the reader knows how its
// agent reports a failed call. Scanning output for words like "error:"
// was measured on a real session to mark eight healthy documentation
// reads as failures — a design document simply contains those words.
func failed(c transcript.ToolCall) bool {
	return c.IsError
}

// commandTools run something; only their calls can be shell writes.
var commandTools = map[string]bool{
	"Bash": true, "shell": true, "exec": true, "PowerShell": true,
	"run_command": true, "local_shell": true,
}

// Analyze returns the most telling finding about the tail of a session,
// or nil when the agent looks healthy. Detectors are ordered by how
// specific their evidence is: a repeated identical failure says much more
// than a long stretch without edits.
func Analyze(calls []transcript.ToolCall, th Thresholds) *Finding {
	if len(calls) == 0 {
		return nil
	}
	if th.Window > 0 && len(calls) > th.Window {
		calls = calls[len(calls)-th.Window:]
	}
	for _, check := range []func([]transcript.ToolCall, Thresholds) *Finding{
		sameFailure, errorStreak, repeated, oscillation, noProgress,
	} {
		if f := check(calls, th); f != nil {
			return f
		}
	}
	return nil
}

// sameFailure: the same call keeps coming back with the same failure —
// the strongest evidence of a loop, because nothing is changing.
func sameFailure(calls []transcript.ToolCall, th Thresholds) *Finding {
	if th.SameFailure <= 0 {
		return nil
	}
	last := calls[len(calls)-1]
	if !failed(last) {
		return nil
	}
	sig, key := Signature(last), resultKey(last)
	if key == "" {
		return nil
	}
	// Work in between does not clear the finding: an identical failure
	// after edits is exactly the loop worth reporting. Only the same call
	// producing a DIFFERENT result means the agent moved.
	count, edits := 0, 0
	for i := len(calls) - 1; i >= 0; i-- {
		c := calls[i]
		if Signature(c) != sig {
			if mutating(c) {
				edits++
			}
			continue
		}
		if !failed(c) || resultKey(c) != key {
			break
		}
		count++
	}
	if count < th.SameFailure {
		return nil
	}
	reason := fmt.Sprintf("%s ran %d times with the same failure — %s",
		last.Tool, count, shorten(last.Arg, 60))
	if edits > 0 {
		reason = fmt.Sprintf("%s ran %d times with the same failure after %d edits — %s",
			last.Tool, count, edits, shorten(last.Arg, 60))
	}
	return &Finding{Kind: "same_failure", Signature: sig, Count: count, Reason: reason}
}

// errorStreak: everything is failing, whatever it is.
func errorStreak(calls []transcript.ToolCall, th Thresholds) *Finding {
	if th.ErrorStreak <= 0 {
		return nil
	}
	count := 0
	for i := len(calls) - 1; i >= 0 && failed(calls[i]); i-- {
		count++
	}
	if count < th.ErrorStreak {
		return nil
	}
	return &Finding{
		Kind:      "error_streak",
		Signature: "streak",
		Count:     count,
		Reason:    fmt.Sprintf("the last %d tool calls all failed", count),
	}
}

// repeated: the same call over and over, failing or not.
func repeated(calls []transcript.ToolCall, th Thresholds) *Finding {
	last := calls[len(calls)-1]
	// editing the same file again and again is how work looks, not a loop
	if th.Repeat <= 0 || mutating(last) {
		return nil
	}
	sig := Signature(last)
	count := 0
	for i := len(calls) - 1; i >= 0; i-- {
		if Signature(calls[i]) != sig {
			if mutating(calls[i]) {
				break // an edit in between is progress, not a loop
			}
			continue
		}
		count++
	}
	if count < th.Repeat {
		return nil
	}
	return &Finding{
		Kind:      "repeat",
		Signature: sig,
		Count:     count,
		Reason: fmt.Sprintf("%s repeated %d times — %s",
			last.Tool, count, shorten(last.Arg, 60)),
	}
}

// oscillation: A-B-A-B, the classic "edit, test, edit, test" spin where
// the test result never moves.
func oscillation(calls []transcript.ToolCall, th Thresholds) *Finding {
	if th.Oscillation <= 0 || len(calls) < th.Oscillation {
		return nil
	}
	prev, last := calls[len(calls)-2], calls[len(calls)-1]
	// alternating edits, or a healthy browse/click rhythm, are not loops;
	// an oscillation only means something when it is getting nowhere
	if mutating(prev) || mutating(last) {
		return nil
	}
	a, b := Signature(prev), Signature(last)
	if a == b {
		return nil
	}
	count, failures := 0, 0
	for i := len(calls) - 1; i >= 0; i-- {
		want := b
		if (len(calls)-1-i)%2 == 1 {
			want = a
		}
		if Signature(calls[i]) != want {
			break
		}
		if failed(calls[i]) {
			failures++
		}
		count++
	}
	if count < th.Oscillation || failures == 0 {
		return nil
	}
	return &Finding{
		Kind:      "oscillation",
		Signature: a + " <-> " + b,
		Count:     count,
		Reason: fmt.Sprintf("alternating between %s and %s for %d calls",
			shorten(calls[len(calls)-2].Tool+" "+calls[len(calls)-2].Arg, 40),
			shorten(calls[len(calls)-1].Tool+" "+calls[len(calls)-1].Arg, 40), count),
	}
}

// noProgress: plenty of activity, nothing written.
func noProgress(calls []transcript.ToolCall, th Thresholds) *Finding {
	if th.NoProgress <= 0 {
		return nil
	}
	count, sawEdit := 0, false
	for i := len(calls) - 1; i >= 0; i-- {
		if mutating(calls[i]) {
			sawEdit = true
			break
		}
		count++
	}
	// reading a lot at the start of a session is research, not a stall:
	// only report a stall for an agent that HAD been changing things
	if count < th.NoProgress || !sawEdit {
		return nil
	}
	return &Finding{
		Kind:      "no_progress",
		Signature: "no_progress",
		Count:     count,
		Reason:    fmt.Sprintf("%d tool calls without changing a single file", count),
	}
}

func shorten(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
