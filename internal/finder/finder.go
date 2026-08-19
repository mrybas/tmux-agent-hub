// Package finder is the fzf-backed agent search: one indexed line per
// agent (folder, session, status, model, prompt, tail of the last reply)
// plus a preview pane with the agent's details.
package finder

import (
	"fmt"
	"strings"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/sidebar"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/transcript"
)

// ansi renders a truecolor foreground escape from "#rrggbb".
func ansi(hex string) string {
	var r, g, b int
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b); err != nil {
		return ""
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
}

const reset = "\x1b[0m"

// Line builds one searchable fzf line: "<pane>\t<visible text>".
// Everything the user may remember is in the text: folder, path, session,
// agent, model, status word, title, prompt and a snippet of the reply.
func Line(cfg config.Config, p *state.Pane, session string) string {
	glyphs := cfg.Statusline.StatusGlyphs.ToMap()
	colors := cfg.Colors.ToMap()
	base, _ := sidebar.SplitFolder(sidebar.ShortPath(p.Cwd))

	parts := []string{
		ansi(colors[p.Display()]) + glyphs[p.Display()] + reset,
		base,
	}
	if session != "" {
		parts = append(parts, session)
	}
	agent := p.Agent
	if m := sidebar.ShortModel(p.Model); m != "" {
		agent = m
	}
	parts = append(parts, agent, string(p.Display()))
	if p.StuckReason != "" {
		parts = append(parts, "stuck: "+p.StuckReason)
	}
	if p.Title != "" {
		parts = append(parts, p.Title)
	}
	if p.AgentTitle != "" && p.ParentPane != "" {
		parts = append(parts, p.AgentTitle)
	}
	if p.LastPrompt != "" {
		parts = append(parts, clip(p.LastPrompt, 80))
	}
	if reply := replySnippet(p, 100); reply != "" {
		parts = append(parts, "\x1b[2m"+reply+reset)
	}
	return p.PaneID + "\t" + strings.Join(parts, " · ")
}

// replySnippet is a whitespace-collapsed tail of the agent's last reply.
func replySnippet(p *state.Pane, max int) string {
	if p.TranscriptPath == "" {
		return ""
	}
	text, err := transcript.LastReplyText(p.Agent, p.TranscriptPath)
	if err != nil {
		return ""
	}
	return clip(text, max)
}

func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// Preview renders the fzf preview pane for one agent.
func Preview(cfg config.Config, p *state.Pane, session string) string {
	glyphs := cfg.Statusline.StatusGlyphs.ToMap()
	colors := cfg.Colors.ToMap()
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s %s%s · %s\n", ansi(colors[p.Display()]), glyphs[p.Display()], p.Display(), reset, p.PaneID)
	fmt.Fprintf(&b, "%s\n", sidebar.ShortPath(p.Cwd))
	if session != "" {
		fmt.Fprintf(&b, "session: %s\n", session)
	}
	agent := p.Agent
	if m := sidebar.ShortModel(p.Model); m != "" {
		agent += " · " + m
	}
	fmt.Fprintf(&b, "agent: %s\n", agent)
	if p.CurrentTool != "" {
		fmt.Fprintf(&b, "tool: %s\n", p.CurrentTool)
	}
	if p.StuckReason != "" {
		fmt.Fprintf(&b, "stuck: %s\n", p.StuckReason)
	}
	if p.ReviewerPane != "" {
		fmt.Fprintf(&b, "reviewer: %s\n", p.ReviewerPane)
	}
	if p.LastPrompt != "" {
		fmt.Fprintf(&b, "\n\x1b[1mprompt\x1b[0m\n%s\n", p.LastPrompt)
	}
	if p.TranscriptPath != "" {
		if text, err := transcript.LastReplyText(p.Agent, p.TranscriptPath); err == nil && text != "" {
			fmt.Fprintf(&b, "\n\x1b[1mlast reply\x1b[0m\n%s\n", transcript.Tail(text, 2500))
		}
	}
	return b.String()
}
