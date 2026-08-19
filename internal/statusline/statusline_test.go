package statusline

import (
	"testing"

	"github.com/mrybas/tmux-agent-hub/internal/state"
)

// testOptions mirrors config.Default() without importing config (cycle).
func testOptions() Options {
	return Options{
		Style:        StyleStatus,
		Glyphs:       map[string]string{"claude": "✳", "codex": "⬢"},
		DefaultGlyph: "●",
		StatusGlyphs: map[state.Status]string{
			state.StatusWorking:           "✳",
			state.StatusWaitingPermission: "?",
			state.StatusWaitingInput:      "○",
			state.StatusDone:              "✓",
			state.StatusDead:              "✗",
		},
		Colors: map[state.Status]string{
			state.StatusWorking:           "#f5c518",
			state.StatusWaitingPermission: "#f2508b",
			state.StatusWaitingInput:      "#56b6c2",
			state.StatusDone:              "#4fce62",
			state.StatusDead:              "#7f849c",
		},
		Max: 12,
	}
}

func panes(statuses ...state.Status) []*state.Pane {
	var ps []*state.Pane
	for i, s := range statuses {
		ps = append(ps, &state.Pane{PaneID: string(rune('a' + i)), Agent: "claude", Status: s})
	}
	return ps
}

func TestRenderStatusStyle(t *testing.T) {
	out := Render(panes(state.StatusWorking, state.StatusWaitingPermission, state.StatusDone), testOptions())
	want := "#[fg=#f5c518]✳#[fg=#f2508b]?#[fg=#4fce62]✓#[default]"
	if out != want {
		t.Errorf("Render = %q, want %q", out, want)
	}
}

func TestRenderAgentStyle(t *testing.T) {
	o := testOptions()
	o.Style = StyleAgent
	ps := panes(state.StatusWorking)
	ps = append(ps, &state.Pane{PaneID: "x", Agent: "codex", Status: state.StatusDone})
	ps = append(ps, &state.Pane{PaneID: "y", Agent: "unknown", Status: state.StatusDone})
	out := Render(ps, o)
	want := "#[fg=#f5c518]✳#[fg=#4fce62]⬢#[fg=#4fce62]●#[default]"
	if out != want {
		t.Errorf("Render = %q, want %q", out, want)
	}
}

func TestRenderBGBlocks(t *testing.T) {
	o := testOptions()
	o.BG = true
	out := Render(panes(state.StatusWaitingPermission), o)
	want := "#[fg=black,bg=#f2508b] ? #[default]"
	if out != want {
		t.Errorf("Render = %q, want %q", out, want)
	}
}

func TestRenderEmpty(t *testing.T) {
	if out := Render(nil, testOptions()); out != "" {
		t.Errorf("Render(nil) = %q, want empty", out)
	}
}

func TestRenderCollapsesToCounts(t *testing.T) {
	o := testOptions()
	o.Max = 2
	out := Render(panes(state.StatusWorking, state.StatusWorking, state.StatusWaitingPermission), o)
	// permission count first (needs the user), then working
	want := "#[fg=#f2508b]?1#[fg=#f5c518]✳2#[default]"
	if out != want {
		t.Errorf("Render = %q, want %q", out, want)
	}
}
