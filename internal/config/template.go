package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// defaultTOML is written by `tmux-agent-hub config init` — a fully commented
// config that doubles as the documentation of every setting.
const defaultTOML = `# tmux-agent-hub configuration.
# Every value here equals the built-in default — delete anything you don't
# change, the plugin works without this file at all.

editor = "nvim" # opens files from the skills inspector (falls back to $EDITOR)

[keys]
sidebar = "e"   # prefix+e — toggle the agents sidebar pane
popup = "o"     # prefix+o — open the sidebar as a popup (any window, no layout change)
all = "E"       # prefix+E — toggle sidebars in EVERY window (auto-opens in new ones)
find = "f"      # prefix+f — fzf search across agents (folder/status/prompt/reply)
next = "u"      # prefix+u — jump to the next agent that needs you (u = urgent)
advisor = "s"   # prefix+s — send this agent's reply to another agent (review etc.)

# Shared by the status bar and the sidebar. Explicit hex on purpose:
# named ANSI colors get remapped by terminal themes.
[colors]
working = "#f5c518"            # agent is thinking / running tools
waiting_permission = "#f2508b" # agent asks for a permission — needs YOU
waiting_input = "#56b6c2"      # idle at the prompt, result already seen
done = "#4fce62"               # finished, you haven't looked yet
dead = "#7f849c"               # process gone
stuck = "#fab387"              # running, but the detectors see it going in circles

[theme]
# name = "catppuccin-mocha"  # overlay a theme file: built-in themes
#                            # (catppuccin-mocha, gruvbox-dark) or your own
#                            # ~/.config/tmux-agent-hub/themes/<name>.toml.
#                            # A theme is a plain config fragment; your
#                            # values below still win over the theme's.
selection = "bar"  # "bar": accent bar on the left; "reverse": inverted row
accent = "#89b4fa" # selection bar, folder headers
dim = "#7f849c"    # timings, prompts, hints

[theme.icons]
folder = ""       # nerd font; set to "" if your font lacks it
guide_mid = "├"    # tree view branch guides
guide_last = "╰"

# What happens when an agent finishes, asks for a permission, or is caught
# going in circles. Effects:
#   message — status-line toast on every attached client
#   blink   — the status glyph blinks until the cause is gone (permission:
#             until you answer; done: until you visit the pane)
#   bell    — rings the agent's window (name lights up, terminal badge)
#   desktop — system notification (osascript / notify-send)
#   popup   — top-right auto-closing popup (steals keys while open)
[notify]
done = ["message", "bell", "blink"]
permission = ["message", "blink"]
stuck = ["message", "blink"]
debounce = 5              # seconds between pings per agent
popup_position = "top-right" # or "center"

# Stuck detection: model-free heuristics over the agent's recent tool
# calls. They cost nothing and fire in milliseconds — the agent is marked
# stuck in the sidebar and you get a notification. With nudge = true the
# agent is also told, in its own chat, what pattern it is repeating.
# "tmux-agent-hub replay" reports what these thresholds would have said on
# your own recorded sessions.
[detect]
enabled = true
nudge = false
cooldown = 180     # seconds before the same finding is reported again
window = 20        # how many recent tool calls are considered
repeat = 5         # identical call repeated this many times (tuned on real sessions)
same_failure = 2   # identical failing result this many times
error_streak = 3   # consecutive failing calls
oscillation = 4    # length of an A-B-A-B run
no_progress = 25   # calls in a row without editing anything (after edits started)

# The live reviewer (assigned with "a" in the sidebar). "live" lets the
# reviewer look at the work after tool rounds, not only when the turn
# ends; min_interval keeps that from flooding it. catch_up_max is how long
# a worker may be stalled waiting for a verdict it is about to receive
# (0 disables stalling).
[advisor]
mode = "live"      # or "turn"
min_interval = 45  # seconds between mid-turn reviews of one agent
catch_up_max = 20  # seconds a worker may wait for a verdict (0 = never)
# The limits that keep a pair honest when something goes wrong.
advice_max = 3     # deliveries into an IDLE agent before the advisor waits for
                   # you — the bound on a reviewer and a worker talking to each
                   # other (0 = no limit). Advice landing in a turn you set
                   # going never counts and is never withheld.
lost_after = 600   # seconds before a review request nobody answered is written
                   # off, so a killed reviewer cannot mute the pair for good
queue = 8          # workers that may wait for one busy reviewer; a finished
                   # turn always finds room, a mid-turn look may not
grace_ms = 2000    # a hook can fire before the words that triggered it are on
                   # disk: how long a turn boundary waits for the transcript to
                   # catch up before feeding what it has (0 = do not wait)
stale_after = 900  # seconds of transcript silence after which a "working"
                   # agent is not working (its turn died without a Stop hook)
# How much text each part of a review is worth, in runes. Most people never
# touch these: the defaults are what a reviewer can read without drowning.
# Raise arg_runes if long shell commands lose their tails in the delta.
delta_runes = 7000   # the whole delta handed to a reviewer
arg_runes = 200      # one tool argument inside it (clipped in the middle,
                     # so the end of a long command survives)
error_runes = 500    # one failed tool's output
report_runes = 1200  # one subagent's report
advice_runes = 2000  # advice injected back into the worker's chat
output_runes = 8000  # {{output}} in a one-shot advisor template (prefix+s)

# The flight recorders, under ~/.local/state/tmux-agent-hub/. hooks_log
# records every agent hook event and how it resolved to a pane — the first
# thing to enable when statuses look wrong. Above log_max_kb a log rotates:
# the current file becomes <name>.log.old (one generation kept).
[debug]
hooks_log = false   # every hook event and how it resolved to a pane
advisor_log = true  # every review feed, verdict and delivery (advisor.log)
log_max_kb = 1024   # both logs rotate above this size, one .old kept

[statusline]
style = "status" # "status": glyph shape = agent status (readable on any theme)
                 # "agent":  glyph = agent kind (see agent_glyphs), status is color-only
bg = false       # true: colored background blocks — maximum visibility
max = 12         # above this many agents, collapse to per-status counts

[statusline.status_glyphs]
working = "✳"
waiting_permission = "?"
waiting_input = "○"
done = "✓"
dead = "✗"
stuck = "!"

[statusline.agent_glyphs] # used with style = "agent"
claude = "✳"
codex = "⬢"
default = "●"

[sidebar]
width = 42
position = "left" # or "right"
view = "rich"     # preset: rich — two-line rows with prompts
                  #         tree — one-line rows with branch guides
                  #         compact — tree without guides (config-only)
                  # (the v key cycles rich/tree live in the sidebar; agents
                  #  that finished or wait for a permission are pinned in an
                  #  "inbox" section on top of every view)
# Fine-tuning: any non-empty knob overrides the preset.
# row = "rich"     # rich | compact
# group = "folder" # folder | session | none
# sort = "path"    # path | urgency
# guides = "off"   # on | off
`

// Init writes the commented default config. Refuses to overwrite.
func Init() error {
	path := Path()
	if path == "" {
		return fmt.Errorf("cannot determine config directory")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(defaultTOML), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}
