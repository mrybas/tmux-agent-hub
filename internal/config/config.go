// Package config is the single source of truth for plugin settings:
// defaults live here, users override them in
// $XDG_CONFIG_HOME/tmux-agent-hub/config.toml (default ~/.config). tmux.conf only
// loads the plugin and optionally places the #{tmux_agent_hub_status} segment.
//
// A theme is not a separate format: it is a plain config fragment (TOML)
// overlaid between the defaults and the user's file:
//
//	defaults -> themes/<name>.toml -> config.toml
package config

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/mrybas/tmux-agent-hub/internal/detect"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/statusline"
	"github.com/mrybas/tmux-agent-hub/internal/transcript"
)

//go:embed themes/*.toml
var embeddedThemes embed.FS

type Keys struct {
	Sidebar string `toml:"sidebar"` // prefix+<key> toggles the sidebar pane
	Popup   string `toml:"popup"`   // prefix+<key> opens the sidebar as a popup
	All     string `toml:"all"`     // prefix+<key> toggles sidebars in every window
	Find    string `toml:"find"`    // prefix+<key> opens the fzf agent search
	Next    string `toml:"next"`    // prefix+<key> jumps to the next agent that needs you
	Advisor string `toml:"advisor"` // prefix+<key> opens the advisor popup
}

// StatusStrings holds one string per agent status — used for both glyphs
// and colors.
type StatusStrings struct {
	Working           string `toml:"working"`
	WaitingPermission string `toml:"waiting_permission"`
	WaitingInput      string `toml:"waiting_input"`
	Done              string `toml:"done"`
	Dead              string `toml:"dead"`
	Stuck             string `toml:"stuck"`
}

func (s StatusStrings) ToMap() map[state.Status]string {
	return map[state.Status]string{
		state.StatusWorking:           s.Working,
		state.StatusWaitingPermission: s.WaitingPermission,
		state.StatusWaitingInput:      s.WaitingInput,
		state.StatusDone:              s.Done,
		state.StatusDead:              s.Dead,
		state.StatusStuck:             s.Stuck,
	}
}

type Statusline struct {
	Style        string            `toml:"style"` // "status" or "agent"
	BG           bool              `toml:"bg"`
	Max          int               `toml:"max"`
	StatusGlyphs StatusStrings     `toml:"status_glyphs"`
	AgentGlyphs  map[string]string `toml:"agent_glyphs"` // "default" is the fallback
}

// Sidebar view knobs. A preset (view) sets all of them; individual knobs,
// when non-empty, override the preset.
type Sidebar struct {
	Width    int    `toml:"width"`
	Position string `toml:"position"` // "left" or "right"
	View     string `toml:"view"`     // preset: rich | compact | tree
	Row      string `toml:"row"`      // "" | rich | compact
	Group    string `toml:"group"`    // "" | folder | session | none
	Sort     string `toml:"sort"`     // "" | path | urgency
	Guides   string `toml:"guides"`   // "" | on | off
}

type Icons struct {
	Folder    string `toml:"folder"`
	GuideMid  string `toml:"guide_mid"`
	GuideLast string `toml:"guide_last"`
}

type Theme struct {
	Name      string `toml:"name"`      // optional: overlay themes/<name>.toml first
	Selection string `toml:"selection"` // "bar" or "reverse"
	Accent    string `toml:"accent"`
	Dim       string `toml:"dim"`
	Icons     Icons  `toml:"icons"`
}

// Notify configures event effects. Available effects: "message" (status
// line toast), "blink" (the glyph blinks until the cause is gone),
// "bell" (rings the agent's window — its name lights up), "desktop"
// (macOS notification), "popup" (auto-closing popup; steals keys while
// open, off by default).
type Notify struct {
	Done          []string `toml:"done"`
	Permission    []string `toml:"permission"`
	Stuck         []string `toml:"stuck"`
	Debounce      int      `toml:"debounce"`       // seconds between pings per agent
	PopupPosition string   `toml:"popup_position"` // "top-right" | "center"
}

// Debug settings — off by default.
type Debug struct {
	HooksLog   bool `toml:"hooks_log"`   // record every hook event's pane resolution
	AdvisorLog bool `toml:"advisor_log"` // record every review feed, verdict and delivery
	LogMaxKB   int  `toml:"log_max_kb"`  // rotate above this size (one .old kept)
}

// Advisor controls the live reviewer: how often a linked reviewer looks
// at the worker's work, how long the worker may be stalled for it, and
// the limits that keep the pair honest when something goes wrong.
type Advisor struct {
	Mode        string `toml:"mode"`         // "live" (review after tool rounds) or "turn" (end of turn only)
	MinInterval int    `toml:"min_interval"` // seconds between mid-turn reviews of one agent
	CatchUpMax  int    `toml:"catch_up_max"` // seconds the worker may wait for a held verdict (0 = never)
	AdviceMax   int    `toml:"advice_max"`   // deliveries into an idle agent before the advisor waits for you (0 = no limit)
	// How much text each part of a review is worth. These are shape, not
	// policy — the defaults are what a reviewer can read without drowning.
	DeltaRunes  int `toml:"delta_runes"`  // the whole delta handed to a reviewer
	ArgRunes    int `toml:"arg_runes"`    // one tool argument inside it
	ErrorRunes  int `toml:"error_runes"`  // one failed tool's output
	ReportRunes int `toml:"report_runes"` // one subagent's report
	AdviceRunes int `toml:"advice_runes"` // advice injected back into the worker
	OutputRunes int `toml:"output_runes"` // {{output}} in a one-shot advisor template
	LostAfter   int `toml:"lost_after"`   // seconds before an unanswered review request is written off
	Queue       int `toml:"queue"`        // workers that may wait for one busy reviewer
	GraceMS     int `toml:"grace_ms"`     // how long a turn boundary waits for the transcript to catch up
	StaleAfter  int `toml:"stale_after"`  // seconds of transcript silence before a "working" agent is not working
	GoneAfter   int `toml:"gone_after"`   // seconds of silence before checking whether the agent still runs in its pane
}

// Advisor limits, resolved: a value the user left out (or set to
// nonsense) falls back to the built-in default. AdviceMax is the one
// exception — an explicit 0 there means "never go quiet".
func (a Advisor) AdviceLimit() int {
	if a.AdviceMax < 0 {
		return Default().Advisor.AdviceMax
	}
	return a.AdviceMax
}

func (a Advisor) LostTimeout() time.Duration {
	return seconds(a.LostAfter, Default().Advisor.LostAfter)
}

func (a Advisor) QueueLimit() int {
	if a.Queue <= 0 {
		return Default().Advisor.Queue
	}
	return a.Queue
}

// BoundaryGrace may legitimately be zero: an agent whose transcript is
// always current has nothing to wait for.
func (a Advisor) BoundaryGrace() time.Duration {
	if a.GraceMS < 0 {
		a.GraceMS = Default().Advisor.GraceMS
	}
	return time.Duration(a.GraceMS) * time.Millisecond
}

func (a Advisor) StaleWorkingAfter() time.Duration {
	return seconds(a.StaleAfter, Default().Advisor.StaleAfter)
}

// GoneCheckAfter is how long an agent's state may sit untouched before we
// spend a process-table scan on "is this agent still there at all". A live
// agent writes state on every tool round, so the quiet ones are the only
// suspects worth the cost.
func (a Advisor) GoneCheckAfter() time.Duration {
	return seconds(a.GoneAfter, Default().Advisor.GoneAfter)
}

// TextBudget resolves one of the rune limits, falling back to the default
// when it is missing or nonsensical. Zero is never "unlimited" here: a
// delta with no ceiling would drown the reviewer it is meant to inform.
func (a Advisor) TextBudget(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

// DeltaBudgets are the review-feed limits, resolved.
func (c Config) DeltaBudgets() transcript.Budgets {
	def, a := Default().Advisor, c.Advisor
	return transcript.Budgets{
		Delta:  a.TextBudget(a.DeltaRunes, def.DeltaRunes),
		Arg:    a.TextBudget(a.ArgRunes, def.ArgRunes),
		Error:  a.TextBudget(a.ErrorRunes, def.ErrorRunes),
		Report: a.TextBudget(a.ReportRunes, def.ReportRunes),
	}
}

// AdviceBudget is how much advice may be typed into a worker's chat.
func (c Config) AdviceBudget() int {
	return c.Advisor.TextBudget(c.Advisor.AdviceRunes, Default().Advisor.AdviceRunes)
}

// OutputBudget is how much of a source agent's reply a one-shot advisor
// template carries.
func (c Config) OutputBudget() int {
	return c.Advisor.TextBudget(c.Advisor.OutputRunes, Default().Advisor.OutputRunes)
}

func seconds(value, fallback int) time.Duration {
	if value <= 0 {
		value = fallback
	}
	return time.Duration(value) * time.Second
}

// Detect tunes the stuck-detectors: cheap, model-free heuristics over an
// agent's recent tool calls. A zero threshold disables that detector.
type Detect struct {
	Enabled     bool `toml:"enabled"`
	Nudge       bool `toml:"nudge"`    // also tell the agent, not just the user
	Cooldown    int  `toml:"cooldown"` // seconds before the same finding fires again
	Window      int  `toml:"window"`
	Repeat      int  `toml:"repeat"`
	SameFailure int  `toml:"same_failure"`
	ErrorStreak int  `toml:"error_streak"`
	Oscillation int  `toml:"oscillation"`
	NoProgress  int  `toml:"no_progress"`
}

type Config struct {
	Editor     string        `toml:"editor"` // used by the skills inspector; $EDITOR/nvim fallback
	Keys       Keys          `toml:"keys"`
	Colors     StatusStrings `toml:"colors"` // shared by statusline and sidebar
	Theme      Theme         `toml:"theme"`
	Notify     Notify        `toml:"notify"`
	Advisor    Advisor       `toml:"advisor"`
	Detect     Detect        `toml:"detect"`
	Debug      Debug         `toml:"debug"`
	Statusline Statusline    `toml:"statusline"`
	Sidebar    Sidebar       `toml:"sidebar"`
}

// Colors are explicit hex values on purpose: named ANSI colors get
// remapped by terminal themes and stop being distinguishable.
func Default() Config {
	return Config{
		Editor: "nvim",
		Keys:   Keys{Sidebar: "e", Popup: "o", All: "E", Find: "f", Next: "u", Advisor: "s"},
		Colors: StatusStrings{
			Working:           "#f5c518", // amber
			WaitingPermission: "#f2508b", // pink-red, must scream
			WaitingInput:      "#56b6c2", // teal
			Done:              "#4fce62", // green
			Dead:              "#7f849c", // grey
			Stuck:             "#fab387", // orange: running, but in circles
		},
		Theme: Theme{
			Selection: "bar",
			Accent:    "#89b4fa",
			Dim:       "#7f849c",
			Icons:     Icons{Folder: "", GuideMid: "├", GuideLast: "╰"},
		},
		Statusline: Statusline{
			Style: statusline.StyleStatus,
			Max:   12,
			StatusGlyphs: StatusStrings{
				Working:           "✳",
				WaitingPermission: "?",
				WaitingInput:      "○",
				Done:              "✓",
				Dead:              "✗",
				Stuck:             "!",
			},
			AgentGlyphs: map[string]string{"claude": "✳", "codex": "⬢", "default": "●"},
		},
		Sidebar: Sidebar{Width: 42, Position: "left", View: "rich"},
		Debug:   Debug{AdvisorLog: true, LogMaxKB: 1024},
		Advisor: Advisor{
			Mode: "live", MinInterval: 45, CatchUpMax: 20,
			AdviceMax: 3, LostAfter: 600, Queue: 8, GraceMS: 2000,
			StaleAfter: 900, GoneAfter: 60,
			DeltaRunes: 7000, ArgRunes: 200, ErrorRunes: 500,
			ReportRunes: 1200, AdviceRunes: 2000, OutputRunes: 8000,
		},
		Detect: Detect{
			Enabled: true, Cooldown: 180, Window: 20,
			Repeat: 5, SameFailure: 2, ErrorStreak: 3,
			Oscillation: 4, NoProgress: 25,
		},
		Notify: Notify{
			Done:          []string{"message", "bell", "blink"},
			Permission:    []string{"message", "blink"},
			Stuck:         []string{"message", "blink"},
			Debounce:      5,
			PopupPosition: "top-right",
		},
	}
}

func Path() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "tmux-agent-hub", "config.toml")
}

// Load composes defaults -> named theme (if any) -> user config. On a
// broken file it returns pure defaults plus the error, so callers keep
// rendering.
func Load() (Config, error) {
	cfg := Default()
	path := Path()
	if path == "" {
		return cfg, nil
	}
	if _, err := os.Stat(path); err != nil {
		return cfg, nil
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Default(), err
	}
	if cfg.Theme.Name == "" {
		return cfg, nil
	}
	// Re-compose with the theme as the middle layer so the user's own
	// values still win over the theme's.
	themed := Default()
	if err := overlayTheme(&themed, cfg.Theme.Name); err != nil {
		return cfg, err
	}
	if _, err := toml.DecodeFile(path, &themed); err != nil {
		return Default(), err
	}
	return themed, nil
}

// overlayTheme decodes a theme file over cfg. User themes in
// ~/.config/tmux-agent-hub/themes/ shadow the embedded ones.
func overlayTheme(cfg *Config, name string) error {
	if userPath := Path(); userPath != "" {
		p := filepath.Join(filepath.Dir(userPath), "themes", name+".toml")
		if _, err := os.Stat(p); err == nil {
			_, err := toml.DecodeFile(p, cfg)
			return err
		}
	}
	data, err := embeddedThemes.ReadFile("themes/" + name + ".toml")
	if err != nil {
		return fmt.Errorf("unknown theme %q", name)
	}
	return toml.Unmarshal(data, cfg)
}

// ViewSpec is the resolved sidebar layout after applying the preset and
// the individual knob overrides.
type ViewSpec struct {
	Row    string // "rich" | "compact"
	Group  string // "folder" | "session" | "none"
	Sort   string // "path" | "urgency"
	Guides bool
}

var viewPresets = map[string]ViewSpec{
	"rich":    {Row: "rich", Group: "folder", Sort: "path"},
	"compact": {Row: "compact", Group: "folder", Sort: "path"},
	"tree":    {Row: "compact", Group: "folder", Sort: "path", Guides: true},
}

// ViewPresetNames is the cycle order for the sidebar's live view switch.
// "compact" stays available as a config value (tree without guides) but
// is not in the cycle — it reads almost identically to tree.
var ViewPresetNames = []string{"rich", "tree"}

func (c Config) SidebarView() ViewSpec {
	spec, ok := viewPresets[c.Sidebar.View]
	if !ok {
		spec = viewPresets["rich"]
	}
	return c.ApplyKnobs(spec)
}

// ApplyKnobs overlays the user's explicit knobs on a preset — so e.g.
// group = "session" survives live view cycling in the sidebar.
func (c Config) ApplyKnobs(spec ViewSpec) ViewSpec {
	if c.Sidebar.Row != "" {
		spec.Row = c.Sidebar.Row
	}
	if c.Sidebar.Group != "" {
		spec.Group = c.Sidebar.Group
	}
	if c.Sidebar.Sort != "" {
		spec.Sort = c.Sidebar.Sort
	}
	switch c.Sidebar.Guides {
	case "on":
		spec.Guides = true
	case "off":
		spec.Guides = false
	}
	return spec
}

// ViewPreset returns the resolved spec for a named preset (used by the
// sidebar's live view cycling).
func ViewPreset(name string) ViewSpec {
	if spec, ok := viewPresets[name]; ok {
		return spec
	}
	return viewPresets["rich"]
}

// EditorCommand resolves the editor: config value, then $EDITOR, then vi.
// BlinkConfigured reports whether any notify list uses the blink effect
// (the status line then needs a 1s refresh interval for the pulse).
func (c Config) BlinkConfigured() bool {
	for _, list := range [][]string{c.Notify.Done, c.Notify.Permission, c.Notify.Stuck} {
		for _, e := range list {
			if e == "blink" {
				return true
			}
		}
	}
	return false
}

func (c Config) EditorCommand() string {
	if c.Editor != "" {
		return c.Editor
	}
	if ed := os.Getenv("EDITOR"); ed != "" {
		return ed
	}
	return "vi"
}

// DetectThresholds maps the config onto the detector package.
func (c Config) DetectThresholds() detect.Thresholds {
	return detect.Thresholds{
		Window:      c.Detect.Window,
		Repeat:      c.Detect.Repeat,
		SameFailure: c.Detect.SameFailure,
		ErrorStreak: c.Detect.ErrorStreak,
		Oscillation: c.Detect.Oscillation,
		NoProgress:  c.Detect.NoProgress,
	}
}

func (c Config) StatuslineOptions() statusline.Options {
	agentGlyphs := make(map[string]string, len(c.Statusline.AgentGlyphs))
	for k, v := range c.Statusline.AgentGlyphs {
		agentGlyphs[k] = v
	}
	def := agentGlyphs["default"]
	delete(agentGlyphs, "default")
	if def == "" {
		def = "●"
	}
	contains := func(list []string, s string) bool {
		for _, e := range list {
			if e == s {
				return true
			}
		}
		return false
	}
	return statusline.Options{
		BlinkPermission: contains(c.Notify.Permission, "blink"),
		BlinkDone:       contains(c.Notify.Done, "blink"),
		BlinkStuck:      contains(c.Notify.Stuck, "blink"),
		BlinkAltColor:   c.Theme.Dim,
		Style:           c.Statusline.Style,
		BG:              c.Statusline.BG,
		Glyphs:          agentGlyphs,
		StatusGlyphs:    c.Statusline.StatusGlyphs.ToMap(),
		DefaultGlyph:    def,
		Colors:          c.Colors.ToMap(),
		Max:             c.Statusline.Max,
	}
}
