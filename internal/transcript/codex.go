package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"time"
)

// codexReader reads Codex CLI rollout logs
// (~/.codex/sessions/<date>/rollout-*.jsonl).
//
// The format differs from Claude's in three ways that matter here: the
// final text of a turn is handed to us directly in a task_complete event,
// tool calls come in two shapes (function_call and custom_tool_call), and
// reasoning is stored encrypted — so reasoning-level analysis is not
// possible for Codex sessions.
type codexReader struct {
	path   string
	budget Budgets
}

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"` // response_item | event_msg | session_meta | ...
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type             string          `json:"type"`
	Role             string          `json:"role"`
	Name             string          `json:"name"`
	CallID           string          `json:"call_id"`
	Arguments        string          `json:"arguments"` // function_call: JSON string
	Input            json.RawMessage `json:"input"`     // custom_tool_call: script string
	Output           json.RawMessage `json:"output"`    // *_output blocks
	Content          json.RawMessage `json:"content"`   // message blocks
	LastAgentMessage string          `json:"last_agent_message"`
	State            struct {
		CollaborationMode struct {
			Model string `json:"model"`
		} `json:"collaboration_mode"`
	} `json:"state"` // world_state
}

func (r codexReader) scanner(f *os.File) *bufio.Scanner {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineSize)
	return sc
}

// each walks the rollout, handing decoded lines to fn.
func (r codexReader) each(from int64, fn func(codexLine, codexPayload)) (int64, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return from, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return from, err
	}
	if from < 0 || from > st.Size() {
		from = 0
	}
	if _, err := f.Seek(from, 0); err != nil {
		return from, err
	}
	sc := r.scanner(f)
	for sc.Scan() {
		var line codexLine
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		var p codexPayload
		if len(line.Payload) > 0 {
			json.Unmarshal(line.Payload, &p)
		}
		fn(line, p)
	}
	return st.Size(), sc.Err()
}

func (r codexReader) LastReply() (text, model string, err error) {
	_, err = r.each(0, func(line codexLine, p codexPayload) {
		switch {
		case line.Type == "event_msg" && p.Type == "task_complete" && p.LastAgentMessage != "":
			text = p.LastAgentMessage
		case line.Type == "response_item" && p.Type == "message" && p.Role == "assistant":
			if t := strings.TrimSpace(blocksText(p.Content)); t != "" {
				text = t
			}
		case line.Type == "world_state" && p.State.CollaborationMode.Model != "":
			model = p.State.CollaborationMode.Model
		}
	})
	return text, model, err
}

func (r codexReader) ToolCalls(max int) ([]ToolCall, error) {
	var from int64
	if st, err := os.Stat(r.path); err == nil {
		from = tailOffset(st.Size(), max)
	}
	var calls []ToolCall
	results := map[string]string{}
	_, err := r.each(from, func(line codexLine, p codexPayload) {
		if line.Type != "response_item" {
			return
		}
		ts, _ := time.Parse(time.RFC3339, line.Timestamp)
		switch p.Type {
		case "function_call":
			raw := json.RawMessage(p.Arguments)
			calls = append(calls, ToolCall{
				Time: ts, Tool: p.Name, ID: p.CallID,
				Arg: toolArg(raw), Input: renderInput(raw),
			})
		case "custom_tool_call":
			script := blocksText(p.Input)
			calls = append(calls, ToolCall{
				Time: ts, Tool: p.Name, ID: p.CallID,
				Arg: clipRunes(script, 80), Input: clipRunes(script, maxInputRunes),
			})
		case "function_call_output", "custom_tool_call_output":
			if p.CallID != "" {
				results[p.CallID] = blocksText(p.Output)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if len(calls) > max {
		calls = calls[len(calls)-max:]
	}
	for i := range calls {
		if out, ok := results[calls[i].ID]; ok {
			calls[i].Result = clipRunes(out, maxResultRunes)
			// Codex has no explicit error flag; the runner reports failures
			// in the output text itself
			calls[i].IsError = isFailureOutput(out)
		}
	}
	return calls, nil
}

func (r codexReader) DeltaSince(offset int64) (Delta, error) {
	var b strings.Builder
	var tools []string
	endsWithReply := false
	names := map[string]string{} // call_id -> tool name, to attribute outputs
	flushTools := func() {
		if len(tools) > 0 {
			b.WriteString("tools: " + strings.Join(tools, ", ") + "\n")
			tools = nil
		}
	}
	end, err := r.each(offset, func(line codexLine, p codexPayload) {
		if line.Type != "response_item" {
			return
		}
		switch p.Type {
		case "message":
			text := strings.TrimSpace(blocksText(p.Content))
			// developer messages and environment blocks are boilerplate
			if text == "" || p.Role == "developer" || strings.HasPrefix(text, "<") {
				return
			}
			flushTools()
			switch p.Role {
			case "user":
				b.WriteString("user: " + clipRunes(text, 400) + "\n")
				endsWithReply = false
			case "assistant":
				b.WriteString("agent: " + clipRunes(text, 1500) + "\n")
				endsWithReply = true
			}
		case "function_call", "custom_tool_call":
			if p.Name == "" {
				return
			}
			names[p.CallID] = p.Name
			endsWithReply = false
			// the argument is what makes a call reviewable: which file,
			// which command — a bare name says nothing
			arg := toolArg(json.RawMessage(p.Arguments))
			if p.Type == "custom_tool_call" {
				arg = blocksText(p.Input)
			}
			call := p.Name
			if arg = strings.TrimSpace(arg); arg != "" {
				call += "(" + clipMiddle(arg, r.budget.Arg) + ")"
			}
			tools = append(tools, call)
		case "function_call_output", "custom_tool_call_output":
			// Codex has no error flag; the runner reports failures in the
			// output text, and a failure is what a reviewer must see
			endsWithReply = false
			out := blocksText(p.Output)
			if !isFailureOutput(out) {
				return
			}
			flushTools()
			b.WriteString("failed " + toolName(names[p.CallID]) + ": " +
				clipRunes(out, r.budget.Error) + "\n")
		}
	})
	if err != nil {
		return Delta{End: offset}, err
	}
	flushTools()
	return Delta{Text: Tail(b.String(), r.budget.Delta), End: end, EndsWithReply: endsWithReply}, nil
}

// exitCodeRe matches the runner's exit-code line; anything non-zero is a
// failed call.
var exitCodeRe = regexp.MustCompile(`exit code ([0-9]+)`)

// isFailureOutput guesses whether a Codex tool output is a failure — the
// rollout carries no explicit error flag, so the runner's own words are
// all there is to go on.
func isFailureOutput(out string) bool {
	if m := exitCodeRe.FindStringSubmatch(out); m != nil && m[1] != "0" {
		return true
	}
	for _, marker := range []string{
		"command not found", "No such file or directory",
		"Permission denied", "timed out", "aborted:",
	} {
		if strings.Contains(out, marker) {
			return true
		}
	}
	return false
}

// Interrupted always reports false: Codex rollouts carry no abort marker,
// so a stuck "working" status is reconciled by transcript silence instead.
func (r codexReader) Interrupted() bool { return false }

// AgentTitle has no equivalent in Codex rollouts.
func (r codexReader) AgentTitle() string { return "" }
