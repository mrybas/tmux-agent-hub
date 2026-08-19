package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/statusline"
)

func TestLoadWithoutFileReturnsDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keys.Sidebar != "e" || cfg.Statusline.Max != 12 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoadOverlaysPartialFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "tmux-agent-hub", "config.toml")
	os.MkdirAll(filepath.Dir(path), 0o755)
	partial := `
[colors]
working = "#ff0000"
[statusline.status_glyphs]
waiting_input = "z"
[statusline.agent_glyphs]
gemini = "G"
`
	if err := os.WriteFile(path, []byte(partial), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Colors.Working != "#ff0000" {
		t.Errorf("override lost: %+v", cfg.Colors)
	}
	if cfg.Colors.Done != "#4fce62" {
		t.Errorf("untouched default lost: %+v", cfg.Colors)
	}
	if cfg.Statusline.StatusGlyphs.WaitingInput != "z" || cfg.Statusline.StatusGlyphs.Working != "✳" {
		t.Errorf("glyph overlay wrong: %+v", cfg.Statusline.StatusGlyphs)
	}
	if cfg.Statusline.AgentGlyphs["gemini"] != "G" || cfg.Statusline.AgentGlyphs["claude"] != "✳" {
		t.Errorf("agent glyph map not merged: %v", cfg.Statusline.AgentGlyphs)
	}
}

func TestLoadBrokenFileFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "tmux-agent-hub", "config.toml")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("[[[not toml"), 0o644)
	cfg, err := Load()
	if err == nil {
		t.Error("expected an error for broken file")
	}
	if cfg.Keys.Sidebar != "e" {
		t.Errorf("broken file must fall back to defaults: %+v", cfg)
	}
}

func TestStatuslineOptions(t *testing.T) {
	o := Default().StatuslineOptions()
	if o.Style != statusline.StyleStatus || o.Max != 12 {
		t.Errorf("options mapping wrong: %+v", o)
	}
	if o.Colors[state.StatusWaitingPermission] != "#f2508b" {
		t.Errorf("colors mapping wrong: %v", o.Colors)
	}
	if o.StatusGlyphs[state.StatusWaitingInput] != "○" {
		t.Errorf("glyph mapping wrong: %v", o.StatusGlyphs)
	}
	if o.DefaultGlyph != "●" {
		t.Errorf("default glyph = %q", o.DefaultGlyph)
	}
	if _, ok := o.Glyphs["default"]; ok {
		t.Error("'default' must be extracted from agent glyph map")
	}
}

func TestSidebarViewResolution(t *testing.T) {
	c := Default() // view = rich
	v := c.SidebarView()
	if v.Row != "rich" || v.Group != "folder" || v.Sort != "activity" || v.Guides {
		t.Errorf("rich preset wrong: %+v", v)
	}
	c.Sidebar.View = "tree"
	if v := c.SidebarView(); !v.Guides || v.Row != "compact" {
		t.Errorf("tree preset wrong: %+v", v)
	}
	// knobs override the preset
	c.Sidebar.View = "rich"
	c.Sidebar.Sort = "urgency"
	c.Sidebar.Guides = "on"
	if v := c.SidebarView(); v.Sort != "urgency" || !v.Guides || v.Row != "rich" {
		t.Errorf("knob overrides wrong: %+v", v)
	}
	c.Sidebar.View = "nonsense"
	c.Sidebar.Sort = ""
	c.Sidebar.Guides = ""
	if v := c.SidebarView(); v.Row != "rich" {
		t.Errorf("unknown preset must fall back to rich: %+v", v)
	}
}

func TestThemeOverlay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "tmux-agent-hub", "config.toml")
	os.MkdirAll(filepath.Dir(path), 0o755)
	// user sets a built-in theme and overrides one of its colors
	userCfg := `
[theme]
name = "catppuccin-mocha"
[colors]
done = "#123456"
`
	os.WriteFile(path, []byte(userCfg), 0o644)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Colors.Working != "#f9e2af" {
		t.Errorf("theme color not applied: %+v", cfg.Colors)
	}
	if cfg.Colors.Done != "#123456" {
		t.Errorf("user value must win over theme: %+v", cfg.Colors)
	}
	if cfg.Theme.Accent != "#b4befe" {
		t.Errorf("theme accent not applied: %+v", cfg.Theme)
	}

	// user theme file shadows the embedded one
	themesDir := filepath.Join(dir, "tmux-agent-hub", "themes")
	os.MkdirAll(themesDir, 0o755)
	os.WriteFile(filepath.Join(themesDir, "catppuccin-mocha.toml"),
		[]byte("[colors]\nworking = \"#ffffff\"\n"), 0o644)
	cfg, _ = Load()
	if cfg.Colors.Working != "#ffffff" {
		t.Errorf("user theme file must shadow embedded: %+v", cfg.Colors)
	}

	// unknown theme: keep rendering, report error
	os.WriteFile(path, []byte("[theme]\nname = \"nope\"\n"), 0o644)
	if _, err := Load(); err == nil {
		t.Error("unknown theme must return an error")
	}
}

func TestDefaultTemplateMatchesDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := Init(); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("generated template does not parse: %v", err)
	}
	// The commented template must be a faithful copy of the built-ins —
	// every section, because a missing default silently disables a whole
	// feature (this test exists because that happened).
	def := Default()
	if cfg.Colors != def.Colors || cfg.Statusline.StatusGlyphs != def.Statusline.StatusGlyphs ||
		cfg.Keys != def.Keys || cfg.Sidebar != def.Sidebar ||
		cfg.Advisor != def.Advisor || cfg.Detect != def.Detect ||
		cfg.Debug != def.Debug || cfg.Theme != def.Theme ||
		cfg.Statusline.Style != def.Statusline.Style || cfg.Statusline.Max != def.Statusline.Max ||
		cfg.Editor != def.Editor {
		t.Errorf("template drifted from Default():\n got %+v\nwant %+v", cfg, def)
	}
	if !reflect.DeepEqual(cfg.Notify, def.Notify) {
		t.Errorf("notify drifted: got %+v want %+v", cfg.Notify, def.Notify)
	}
	if err := Init(); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("Init must refuse to overwrite, got %v", err)
	}
}

// The advisor's limits are configuration, not constants in the code, so
// a missing or nonsensical value must still resolve to something sane.
func TestAdvisorLimitsResolve(t *testing.T) {
	def := Default().Advisor
	if got := (Advisor{}).LostTimeout(); got != time.Duration(def.LostAfter)*time.Second {
		t.Errorf("LostTimeout on an empty config = %v, want the default", got)
	}
	if got := (Advisor{Queue: -1}).QueueLimit(); got != def.Queue {
		t.Errorf("QueueLimit(-1) = %d, want the default %d", got, def.Queue)
	}
	if got := (Advisor{StaleAfter: 0}).StaleWorkingAfter(); got != time.Duration(def.StaleAfter)*time.Second {
		t.Errorf("StaleWorkingAfter(0) = %v, want the default", got)
	}
	// an explicit zero is meaningful for these two
	if got := (Advisor{AdviceMax: 0}).AdviceLimit(); got != 0 {
		t.Errorf("AdviceLimit(0) = %d, want 0 — an explicit \"never go quiet\"", got)
	}
	if got := (Advisor{GraceMS: 0}).BoundaryGrace(); got != 0 {
		t.Errorf("BoundaryGrace(0) = %v, want 0 — an explicit \"do not wait\"", got)
	}
	// and a real value is honoured
	if got := (Advisor{Queue: 3}).QueueLimit(); got != 3 {
		t.Errorf("QueueLimit(3) = %d", got)
	}
	if got := (Advisor{GraceMS: 250}).BoundaryGrace(); got != 250*time.Millisecond {
		t.Errorf("BoundaryGrace(250) = %v", got)
	}
}

// The delta budgets are configuration, but zero is not a meaningful
// value for any of them: a review with no ceiling drowns the reviewer.
func TestDeltaBudgetsResolve(t *testing.T) {
	def := Default().Advisor
	empty := Config{}.DeltaBudgets()
	if empty.Delta != def.DeltaRunes || empty.Arg != def.ArgRunes ||
		empty.Error != def.ErrorRunes || empty.Report != def.ReportRunes {
		t.Errorf("an empty config must yield the defaults, got %+v", empty)
	}
	if got := (Config{}).AdviceBudget(); got != def.AdviceRunes {
		t.Errorf("AdviceBudget = %d, want the default %d", got, def.AdviceRunes)
	}
	if got := (Config{}).OutputBudget(); got != def.OutputRunes {
		t.Errorf("OutputBudget = %d, want the default %d", got, def.OutputRunes)
	}
	mine := Config{Advisor: Advisor{ArgRunes: 400, DeltaRunes: -1}}.DeltaBudgets()
	if mine.Arg != 400 {
		t.Errorf("a real value must be honoured, got %d", mine.Arg)
	}
	if mine.Delta != def.DeltaRunes {
		t.Errorf("nonsense must fall back, got %d", mine.Delta)
	}
}

func TestGroupKnobSurvivesCycling(t *testing.T) {
	c := Default()
	c.Sidebar.Group = "session"
	tree := c.ApplyKnobs(ViewPreset("tree"))
	if tree.Group != "session" {
		t.Errorf("tree must take the session knob, got %q", tree.Group)
	}
}
