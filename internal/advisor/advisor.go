// Package advisor forwards one agent's last reply to another agent with a
// prompt template ("review this", "check the diff", ...). Templates are
// markdown files with {{output}}, {{cwd}} and {{agent}} placeholders;
// user templates in ~/.config/tmux-agent-hub/templates/ shadow the embedded ones.
package advisor

import (
	"embed"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/transcript"
)

//go:embed templates/*.md
var embeddedTemplates embed.FS

type Template struct {
	Name string
	Body string
}

// NeedsOutput reports whether the template wants the agent's reply text
// (so callers can skip transcript reading otherwise).
func (t Template) NeedsOutput() bool {
	return strings.Contains(t.Body, "{{output}}")
}

// LoadTemplates merges embedded templates with the user's
// ~/.config/tmux-agent-hub/templates/*.md; same-name user files win.
func LoadTemplates() []Template {
	byName := map[string]string{}
	if entries, err := embeddedTemplates.ReadDir("templates"); err == nil {
		for _, e := range entries {
			data, err := embeddedTemplates.ReadFile("templates/" + e.Name())
			if err == nil {
				byName[strings.TrimSuffix(e.Name(), ".md")] = string(data)
			}
		}
	}
	if cfgPath := config.Path(); cfgPath != "" {
		dir := filepath.Join(filepath.Dir(cfgPath), "templates")
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err == nil {
					byName[strings.TrimSuffix(e.Name(), ".md")] = string(data)
				}
			}
		}
	}
	var templates []Template
	for name, body := range byName {
		templates = append(templates, Template{Name: name, Body: body})
	}
	sort.Slice(templates, func(i, j int) bool { return templates[i].Name < templates[j].Name })
	return templates
}

func FindTemplate(name string) (Template, bool) {
	for _, t := range LoadTemplates() {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// CustomTemplate wraps a user-typed prompt. Placeholders work as in
// files; when {{output}} is absent, the source agent's reply is appended
// so "review this" just works.
func CustomTemplate(prompt string) Template {
	body := strings.TrimSpace(prompt)
	if !strings.Contains(body, "{{output}}") {
		body += "\n\n--- output of {{agent}} working in {{cwd}} ---\n{{output}}"
	}
	return Template{Name: "custom", Body: body}
}

// BuildMessage renders the template for a source agent. output may be
// empty (e.g. no transcript yet); budget caps how much of it is pasted
// into the target agent's prompt (advisor.output_runes).
func BuildMessage(tpl Template, src *state.Pane, output string, budget int) string {
	if output == "" {
		output = "(no output captured)"
	}
	cwd := "(unknown)"
	agent := "unknown"
	if src != nil {
		if src.Cwd != "" {
			cwd = src.Cwd
		}
		if src.Agent != "" {
			agent = src.Agent
		}
	}
	msg := tpl.Body
	msg = strings.ReplaceAll(msg, "{{output}}", transcript.Tail(output, budget))
	msg = strings.ReplaceAll(msg, "{{cwd}}", cwd)
	msg = strings.ReplaceAll(msg, "{{agent}}", agent)
	return strings.TrimSpace(msg) + "\n"
}

// SourceOutput extracts the source agent's last reply when the template
// needs it. Missing transcripts are not an error — the placeholder text
// explains it.
func SourceOutput(tpl Template, src *state.Pane) string {
	if src == nil || !tpl.NeedsOutput() || src.TranscriptPath == "" {
		return ""
	}
	out, err := transcript.LastReplyText(src.Agent, src.TranscriptPath)
	if err != nil {
		return ""
	}
	return out
}
