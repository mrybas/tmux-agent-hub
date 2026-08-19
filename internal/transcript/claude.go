package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// claudeReader reads Claude Code's JSONL transcripts.
type claudeReader struct {
	path   string
	budget Budgets
}

// entry is the subset of a Claude transcript line we care about. Unknown
// fields are ignored so format additions don't break us.
//
// Content is deliberately raw: a message is either a list of blocks or a
// plain string, and the user's own prompts are the plain-string kind.
// Declaring it as a list made every one of them fail to parse — the
// reviewer never saw a single thing the user asked for.
type entry struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Timestamp   string `json:"timestamp"`
	Message     struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// block is one element of a block-shaped message content.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"` // tool_use blocks
	ID        string          `json:"id"`   // tool_use id
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result payload
	IsError   bool            `json:"is_error"`
}

// blocks decodes the block list, or nothing when the content is a plain
// string (which carries text, never tool calls).
func (e entry) blocks() []block {
	var blocks []block
	if len(e.Message.Content) == 0 || e.Message.Content[0] != '[' {
		return nil
	}
	json.Unmarshal(e.Message.Content, &blocks)
	return blocks
}

// texts returns the message's text parts, whatever shape it has.
func (e entry) texts() []string {
	if len(e.Message.Content) == 0 {
		return nil
	}
	if e.Message.Content[0] != '[' {
		if t := strings.TrimSpace(blocksText(e.Message.Content)); t != "" {
			return []string{t}
		}
		return nil
	}
	var out []string
	for _, b := range e.blocks() {
		if b.Type == "text" {
			if t := strings.TrimSpace(b.Text); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

func (r claudeReader) scanner(f *os.File) *bufio.Scanner {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineSize)
	return sc
}

func (r claudeReader) LastReply() (text, model string, err error) {
	f, err := os.Open(r.path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	sc := r.scanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"assistant"`)) {
			continue // cheap pre-filter before JSON parsing
		}
		var e entry
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.IsSidechain {
			continue
		}
		if e.Message.Model != "" {
			model = e.Message.Model
		}
		if parts := e.texts(); len(parts) > 0 {
			text = strings.Join(parts, "\n\n")
		}
	}
	return text, model, sc.Err()
}

func (r claudeReader) ToolCalls(max int) ([]ToolCall, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if off := tailOffset(st.Size(), max); off > 0 {
		if _, err := f.Seek(off, 0); err != nil {
			return nil, err
		}
	}

	var calls []ToolCall
	type result struct {
		text    string
		isError bool
	}
	results := map[string]result{}
	sc := r.scanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		isUse := bytes.Contains(line, []byte(`"tool_use"`))
		isResult := bytes.Contains(line, []byte(`"tool_result"`))
		if !isUse && !isResult {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) != nil || e.IsSidechain {
			continue
		}
		ts, _ := time.Parse(time.RFC3339, e.Timestamp)
		for _, c := range e.blocks() {
			switch {
			case c.Type == "tool_use" && e.Type == "assistant" && c.Name != "":
				calls = append(calls, ToolCall{
					Time:  ts,
					Tool:  c.Name,
					Arg:   toolArg(c.Input),
					ID:    c.ID,
					Input: renderInput(c.Input),
				})
			case c.Type == "tool_result" && c.ToolUseID != "":
				results[c.ToolUseID] = result{text: blocksText(c.Content), isError: c.IsError}
			}
		}
	}
	if len(calls) > max {
		calls = calls[len(calls)-max:]
	}
	for i := range calls {
		if res, ok := results[calls[i].ID]; ok {
			calls[i].Result = clipRunes(res.text, maxResultRunes)
			calls[i].IsError = res.isError
		}
	}
	return calls, sc.Err()
}

func (r claudeReader) DeltaSince(offset int64) (Delta, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return Delta{End: offset}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Delta{End: offset}, err
	}
	if offset < 0 || offset > st.Size() {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return Delta{End: offset}, err
	}

	var b strings.Builder
	var tools []string
	endsWithReply := false
	names := map[string]string{} // tool_use id -> tool name, to attribute results
	flushTools := func() {
		if len(tools) > 0 {
			b.WriteString("tools: " + strings.Join(tools, ", ") + "\n")
			tools = nil
		}
	}
	sc := r.scanner(f)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.IsSidechain {
			continue
		}
		texts := e.texts()
		var calls, results []string
		for _, c := range e.blocks() {
			switch c.Type {
			case "tool_use":
				if c.Name == "" {
					continue
				}
				names[c.ID] = c.Name
				// the argument is what makes a call reviewable: which file,
				// which command, which subagent — a bare name says nothing
				call := c.Name
				if arg := toolArg(c.Input); arg != "" {
					call += "(" + clipMiddle(arg, r.budget.Arg) + ")"
				}
				calls = append(calls, call)
			case "tool_result":
				text := strings.TrimSpace(blocksText(c.Content))
				if c.ToolUseID == "" || text == "" {
					continue
				}
				name := names[c.ToolUseID]
				switch {
				case c.IsError:
					// failures are the highest-signal thing in a delta
					results = append(results, "failed "+toolName(name)+": "+
						clipRunes(text, r.budget.Error))
				case isSubagentTool(name):
					// a delegating agent does its real work inside subagents;
					// without their reports the reviewer only sees "Agent"
					results = append(results, "subagent report: "+
						clipRunes(text, r.budget.Report))
				}
			}
		}
		if len(texts) > 0 {
			flushTools()
			switch e.Type {
			case "user":
				b.WriteString("user: " + clipRunes(strings.Join(texts, "\n"), 400) + "\n")
				endsWithReply = false
			case "assistant":
				b.WriteString("agent: " + clipRunes(strings.Join(texts, "\n\n"), 1500) + "\n")
				endsWithReply = true
			}
		}
		if len(calls) > 0 {
			endsWithReply = false
		}
		tools = append(tools, calls...)
		if len(results) > 0 {
			endsWithReply = false
			flushTools()
			for _, line := range results {
				b.WriteString(line + "\n")
			}
		}
	}
	flushTools()
	if err := sc.Err(); err != nil {
		return Delta{End: offset}, err
	}
	return Delta{Text: Tail(b.String(), r.budget.Delta), End: st.Size(), EndsWithReply: endsWithReply}, nil
}

func (r claudeReader) Interrupted() bool {
	f, err := os.Open(r.path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	const tailSize = 256 * 1024
	offset := st.Size() - tailSize
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if offset > 0 && i == 0 {
			break // the first chunk line may be cut mid-JSON
		}
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "user":
			// tool_result entries are also type "user" but carry no marker
			return bytes.Contains(line, []byte("Request interrupted by user"))
		case "assistant":
			return false
		}
	}
	return false
}

func (r claudeReader) AgentTitle() string {
	f, err := os.Open(r.path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var title string
	sc := r.scanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"agent-name"`)) {
			continue
		}
		var e struct {
			Type      string `json:"type"`
			AgentName string `json:"agentName"`
		}
		if json.Unmarshal(line, &e) == nil && e.Type == "agent-name" && e.AgentName != "" {
			title = e.AgentName
		}
	}
	return title
}
