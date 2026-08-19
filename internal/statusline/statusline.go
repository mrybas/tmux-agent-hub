// Package statusline renders agent glyphs for the tmux status bar.
// Output is a tmux format string with #[fg=...] style sequences; tmux
// expands them when drawing status-right. All settings come from the
// config package.
package statusline

import (
	"fmt"
	"strings"

	"github.com/mrybas/tmux-agent-hub/internal/state"
)

const (
	// StyleStatus: the glyph's shape encodes the status (color is
	// redundant), agent identity is not shown. Default — survives any
	// terminal color scheme.
	StyleStatus = "status"
	// StyleAgent: the glyph encodes the agent (✳ claude, ⬢ codex),
	// only color encodes the status.
	StyleAgent = "agent"
)

type Options struct {
	BlinkPermission bool   // waiting_permission glyphs blink until resolved
	BlinkDone       bool   // done glyphs blink until the pane is visited
	BlinkStuck      bool   // stuck glyphs blink until the agent moves on
	BlinkPhase      bool   // "off" phase of the software pulse (set from wall clock)
	BlinkAltColor   string // color of the off phase (dim)
	Style           string
	BG              bool              // render as background blocks instead of colored glyphs
	Glyphs          map[string]string // agent name -> glyph (StyleAgent)
	StatusGlyphs    map[state.Status]string
	DefaultGlyph    string
	Colors          map[state.Status]string
	Max             int // above this many agents, collapse to per-status counts
}

// summaryOrder is the display order when collapsed to counts: things that
// need the user first.
var summaryOrder = []state.Status{
	state.StatusWaitingPermission,
	state.StatusStuck,
	state.StatusDone,
	state.StatusWorking,
	state.StatusWaitingInput,
	state.StatusDead,
}

// Render returns "" when there are no agents; otherwise one colored glyph
// per pane (stable order — caller passes them sorted), or per-status
// counts when there are more than o.Max.
func Render(panes []*state.Pane, o Options) string {
	if len(panes) == 0 {
		return ""
	}
	var b strings.Builder
	if len(panes) > o.Max {
		counts := map[state.Status]int{}
		for _, p := range panes {
			counts[p.Display()]++
		}
		for _, st := range summaryOrder {
			if n := counts[st]; n > 0 {
				b.WriteString(o.seg(st, fmt.Sprintf("%s%d", o.statusGlyph(st), n)))
			}
		}
	} else {
		for _, p := range panes {
			b.WriteString(o.seg(p.Display(), o.glyphFor(p)))
		}
	}
	b.WriteString("#[default]")
	return b.String()
}

func (o Options) glyphFor(p *state.Pane) string {
	if o.Style == StyleAgent {
		if g, ok := o.Glyphs[p.Agent]; ok {
			return g
		}
		return o.DefaultGlyph
	}
	return o.statusGlyph(p.Display())
}

func (o Options) statusGlyph(st state.Status) string {
	if g, ok := o.StatusGlyphs[st]; ok && g != "" {
		return g
	}
	return o.DefaultGlyph
}

func (o Options) seg(st state.Status, text string) string {
	color := o.Colors[st]
	attr := ""
	if (o.BlinkPermission && st == state.StatusWaitingPermission) ||
		(o.BlinkDone && st == state.StatusDone) ||
		(o.BlinkStuck && st == state.StatusStuck) {
		// terminal blink for terminals that render it, plus a software
		// pulse (color alternates every refresh) for those that don't
		attr = "blink,"
		if o.BlinkPhase && o.BlinkAltColor != "" {
			color = o.BlinkAltColor
		}
	}
	if o.BG {
		return fmt.Sprintf("#[%sfg=black,bg=%s] %s ", attr, color, text)
	}
	return fmt.Sprintf("#[%sfg=%s]%s", attr, color, text)
}
