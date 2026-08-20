// Package transcript reads agent session transcripts. Every agent CLI
// writes its own format, so callers go through a Reader chosen by agent
// kind — the screen is never scraped, the transcript is the only clean
// source of what an agent said and did.
package transcript

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	maxLineSize    = 10 * 1024 * 1024 // transcript lines can be huge
	maxInputRunes  = 6000
	maxResultRunes = 20000
)

// Budgets size a review delta. A delta is read by another agent, so it
// must carry what the work actually was — the tool argument, what failed,
// what a subagent came back with — and nothing beyond that. They come
// from the config; DefaultBudgets is what the plugin ships with.
type Budgets struct {
	Delta  int // the whole delta
	Arg    int // one tool argument
	Error  int // one failed tool's output
	Report int // one subagent's report
}

func DefaultBudgets() Budgets {
	return Budgets{Delta: 7000, Arg: 200, Error: 500, Report: 1200}
}

// resolved fills in anything a caller left at zero.
func (b Budgets) resolved() Budgets {
	def := DefaultBudgets()
	if b.Delta <= 0 {
		b.Delta = def.Delta
	}
	if b.Arg <= 0 {
		b.Arg = def.Arg
	}
	if b.Error <= 0 {
		b.Error = def.Error
	}
	if b.Report <= 0 {
		b.Report = def.Report
	}
	return b
}

// subagentTools delegate a whole task to another agent: their result is
// the delegated work, not a tool output.
var subagentTools = map[string]bool{"Agent": true, "Task": true}

func isSubagentTool(name string) bool { return subagentTools[name] }

// toolName names a tool in prose, for calls whose name was not seen (the
// delta may start after the tool_use line).
func toolName(name string) string {
	if name == "" {
		return "tool"
	}
	return name
}

// ToolCall is one tool invocation from the session history, with its
// arguments and (when found) the result the agent got back.
type ToolCall struct {
	Time    time.Time
	Tool    string
	Arg     string // the most representative argument (file path, command, …)
	ID      string
	Input   string // all arguments, rendered as "key: value" blocks
	Result  string // what the tool returned (trimmed)
	IsError bool
}

// Delta is what a session gained after a byte offset.
type Delta struct {
	Text string
	End  int64 // byte offset to continue from
	// EndsWithReply reports that the last thing in the session is the
	// agent's own words rather than a tool call or its result — that is,
	// the turn had said what it concluded by the time we read it. Callers
	// must not try to infer this from Text: how a delta is rendered is not
	// a contract.
	EndsWithReply bool
}

// Reader reads one session transcript. Implementations are per agent CLI.
type Reader interface {
	// LastReply is the agent's most recent reply plus the model that
	// produced it (the model may be unknown and come back empty).
	LastReply() (text, model string, err error)
	// ToolCalls returns at most max recent tool invocations, oldest first.
	ToolCalls(max int) ([]ToolCall, error)
	// DeltaSince summarizes what was appended after a byte offset — used
	// to feed a reviewer one chunk at a time.
	DeltaSince(offset int64) (Delta, error)
	// Interrupted reports that the tail of the session is a user interrupt
	// (Ctrl+C/Esc), which fires no completion hook.
	Interrupted() bool
	// AgentTitle is the subagent's name when the session is a teammate.
	AgentTitle() string
}

// For picks the reader for an agent kind ("claude", "codex", …) with the
// default delta budgets — everything except the review feed itself.
func For(agent, path string) Reader { return ForBudgets(agent, path, DefaultBudgets()) }

// ForBudgets is For with the caller's delta sizing.
//
// opencode has no transcript of its own that we can read — its sessions
// live in a SQLite database — so our plugin writes one for it, in the
// same JSONL shape Claude Code uses. That is why it reads with the same
// reader: the file is ours, and it is written to be read by this code.
func ForBudgets(agent, path string, b Budgets) Reader {
	b = b.resolved()
	if agent == "codex" {
		return codexReader{path: path, budget: b}
	}
	return claudeReader{path: path, budget: b}
}

// LastReplyText is the common shortcut: just the text of the last reply.
func LastReplyText(agent, path string) (string, error) {
	text, _, err := For(agent, path).LastReply()
	return text, err
}

// Tail keeps at most max runes from the end of s — the end of a reply
// carries the conclusions.
func Tail(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max:])
}

// clipMiddle shortens s but keeps both ends. A tool argument is usually a
// command, and its tail is where the consequence is ("… && rm tmp.go");
// clipping from the right alone hides exactly that — a reviewer then
// raises findings about work the command already undid.
func clipMiddle(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head := max * 2 / 3
	tail := max - head - 1
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

func clipRunes(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// argPriority: which tool input field best represents the call.
var argPriority = []string{"file_path", "command", "pattern", "path", "url", "query", "description", "prompt"}

func toolArg(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if json.Unmarshal(raw, &input) != nil {
		return clipRunes(string(raw), 80)
	}
	for _, key := range argPriority {
		if v, ok := input[key].(string); ok && v != "" {
			return clipRunes(v, 80)
		}
	}
	for _, v := range input {
		if s, ok := v.(string); ok && s != "" {
			return clipRunes(s, 80)
		}
	}
	return ""
}

// renderInput turns tool arguments into readable "key: value" blocks —
// JSON escaping would make commands and file contents unreadable.
func renderInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if json.Unmarshal(raw, &input) != nil {
		return clipRunes(string(raw), maxInputRunes)
	}
	keys := make([]string, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		var value string
		switch v := input[k].(type) {
		case string:
			value = v
		default:
			data, err := json.Marshal(v)
			if err != nil {
				continue
			}
			value = string(data)
		}
		if strings.Contains(value, "\n") {
			fmt.Fprintf(&b, "%s:\n%s\n", k, value)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", k, value)
		}
	}
	return clipRunes(b.String(), maxInputRunes)
}

// tailOffset picks a byte offset that comfortably holds `want` recent
// tool calls — scanning a 170 MB transcript on every hook is not an
// option, and only the tail matters for recency-based questions.
func tailOffset(size int64, want int) int64 {
	budget := int64(want) * 8 * 1024
	if budget < 1<<20 {
		budget = 1 << 20
	}
	if budget > 64<<20 {
		budget = 64 << 20
	}
	if size <= budget {
		return 0
	}
	return size - budget
}

// blocksText extracts text from a content field that is either a plain
// string or a list of {type, text} blocks — both agents use both shapes.
func blocksText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
