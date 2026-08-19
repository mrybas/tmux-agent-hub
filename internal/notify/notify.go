// Package notify turns agent state transitions into user-visible pings:
// status-line toasts, window bells, desktop notifications and (optional)
// popups. Which effects fire for which event is configured in [notify].
package notify

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/tmuxctl"
)

// Fire applies the configured effects for an event ("done" or
// "permission"). Best-effort: notifications must never break a hook.
// The "blink" effect is passive (rendered by the status line) and is
// intentionally not handled here.
func Fire(cfg config.Config, p *state.Pane, event string) {
	var effects []string
	switch event {
	case "done":
		effects = cfg.Notify.Done
	case "permission":
		effects = cfg.Notify.Permission
	case "stuck":
		effects = cfg.Notify.Stuck
	default:
		return
	}
	base := filepath.Base(p.Cwd)
	var text, color string
	switch event {
	case "done":
		text = fmt.Sprintf("✓ %s · done", base)
		color = cfg.Colors.Done
	case "permission":
		text = fmt.Sprintf("? %s · wants %s", base, orUnknown(p.CurrentTool))
		color = cfg.Colors.WaitingPermission
	case "stuck":
		text = fmt.Sprintf("! %s · %s", base, p.StuckReason)
		color = cfg.Colors.Stuck
	}

	for _, effect := range effects {
		switch effect {
		case "message":
			// the styling is ours, the text is not: a directory name, a tool
			// name, or a stuck reason quoting the agent's own command line
			tmuxctl.DisplayMessageFor(fmt.Sprintf("#[fg=%s,bold]%s#[default]",
				color, tmuxctl.EscapeFormat(text)), 4000)
		case "bell":
			ringBell(p)
		case "desktop":
			desktopNotify(text)
		case "popup":
			openPopup(cfg, color, text)
		}
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "your attention"
	}
	return s
}

// desktopNotify sends an OS notification: osascript on macOS,
// notify-send (freedesktop) on Linux and the BSDs. Silently skipped when
// no known notifier exists.
func desktopNotify(text string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("osascript", "-e", fmt.Sprintf(
			"display notification %q with title \"tmux-agent-hub\"", text)).Start()
	default:
		if _, err := exec.LookPath("notify-send"); err == nil {
			exec.Command("notify-send", "--app-name=tmux-agent-hub", "tmux-agent-hub", text).Start()
		}
	}
}

// ringBell writes BEL to the agent's pane tty: tmux flags that window in
// the status bar and the terminal reacts per its bell settings.
func ringBell(p *state.Pane) {
	pane := p.PaneID
	if p.ParentPane != "" {
		pane = p.ParentPane
	}
	tty := tmuxctl.PaneTTY(pane)
	if tty == "" {
		return
	}
	if f, err := os.OpenFile(tty, os.O_WRONLY, 0); err == nil {
		f.WriteString("\a")
		f.Close()
	}
}

// openPopup shows a small auto-closing popup. tmux popups always grab the
// keyboard while open — that is why this effect is off by default and the
// popup dismisses itself quickly.
func openPopup(cfg config.Config, color, text string) {
	safe := strings.NewReplacer("'", "’", "\\", "", "%", "%%").Replace(text)
	shell := fmt.Sprintf("printf '\\n  \\033[1m%s\\033[0m\\n'; read -t 5 -n 1", safe)
	args := []string{"display-popup", "-E", "-w", "44", "-h", "5",
		"-S", "fg=" + color, "-T", " tmux-agent-hub "}
	if cfg.Notify.PopupPosition != "center" {
		args = append(args, "-x", "R", "-y", "1")
	}
	tmuxctl.StartDetached(append(args, shell)...)
}
