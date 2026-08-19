package hookd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// claudeEvents are the Claude Code hook events tmux-agent-hub subscribes to
// (~/.claude/settings.json).
var claudeEvents = []string{
	"SessionStart",
	"UserPromptSubmit",
	"PreToolUse",
	"PostToolUse",
	"Notification",
	"Stop",
	"SessionEnd",
}

// codexEvents are the Codex CLI hooks-engine events (~/.codex/hooks.json —
// same JSON shape as Claude's, with a dedicated PermissionRequest event).
var codexEvents = []string{
	"SessionStart",
	"UserPromptSubmit",
	"PreToolUse",
	"PostToolUse",
	"PermissionRequest",
	"Stop",
	"SessionEnd",
}

// commandMarker identifies our entries in an agent's hook file.
const commandMarker = `tmux-agent-hub" hook`

func claudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func codexHooksPath() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	// only offer Codex integration when Codex itself is set up
	if _, err := os.Stat(filepath.Join(home, ".codex")); err != nil {
		return "", false
	}
	return filepath.Join(home, ".codex", "hooks.json"), true
}

// Install registers tmux-agent-hub hooks for Claude Code and, when ~/.codex
// exists, for Codex. Idempotent: previous tmux-agent-hub entries (e.g. with a
// stale binary path) are replaced; everything else is preserved.
func Install(binPath string) error {
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	if err := installInto(path, claudeEvents, fmt.Sprintf("%q hook", binPath)); err != nil {
		return err
	}
	if codexPath, ok := codexHooksPath(); ok {
		if err := installInto(codexPath, codexEvents, fmt.Sprintf("%q hook codex", binPath)); err != nil {
			return err
		}
		fmt.Println("Codex hooks written to", codexPath)
		fmt.Println("NOTE: Codex requires trusting them once — run /hooks inside Codex and approve.")
	}
	return nil
}

// Uninstall removes every tmux-agent-hub hook entry from both files.
func Uninstall() error {
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	if err := uninstallFrom(path); err != nil {
		return err
	}
	if codexPath, ok := codexHooksPath(); ok {
		return uninstallFrom(codexPath)
	}
	return nil
}

func installInto(path string, events []string, cmd string) error {
	settings, existed, err := readSettings(path)
	if err != nil {
		return err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	removeOurs(hooks)
	for _, event := range events {
		groups, _ := hooks[event].([]any)
		groups = append(groups, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": cmd}},
		})
		hooks[event] = groups
	}
	if existed {
		orig, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".bak-tmux-agent-hub", orig, 0o644); err != nil {
			return err
		}
	}
	return writeSettings(path, settings)
}

func uninstallFrom(path string) error {
	settings, existed, err := readSettings(path)
	if err != nil || !existed {
		return err
	}
	if hooks, _ := settings["hooks"].(map[string]any); hooks != nil {
		removeOurs(hooks)
	}
	return writeSettings(path, settings)
}

// removeOurs drops every matcher group that contains a tmux-agent-hub command.
// Groups are ours entirely (we never append into foreign groups), so
// dropping whole groups is safe.
func removeOurs(hooks map[string]any) {
	for event, v := range hooks {
		groups, ok := v.([]any)
		if !ok {
			continue
		}
		var kept []any
		for _, g := range groups {
			if !isOurs(g) {
				kept = append(kept, g)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
}

func isOurs(group any) bool {
	m, ok := group.(map[string]any)
	if !ok {
		return false
	}
	inner, _ := m["hooks"].([]any)
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		if strings.Contains(cmd, commandMarker) {
			return true
		}
	}
	return false
}

func readSettings(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return settings, true, nil
}

func writeSettings(path string, settings map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
