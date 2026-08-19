// Package tmuxctl wraps the tmux CLI. Everything here must be safe to call
// from hook handlers: fast, and silent no-ops outside of tmux.
package tmuxctl

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func insideTmux() bool { return os.Getenv("TMUX") != "" }

// Hook processes inherit their caller's environment, and agent-teams
// daemons run inside a hidden tmux server (claude-swarm-*): trusting the
// inherited $TMUX would make us operate on the wrong server. The plugin
// records the user's server socket (RecordSocket) and every command pins
// it explicitly. TMUX_AGENT_HUB_TEST_SOCKET (tests) takes precedence.
func tmuxArgs(args []string) []string {
	if sock := os.Getenv("TMUX_AGENT_HUB_TEST_SOCKET"); sock != "" {
		return append([]string{"-L", sock}, args...)
	}
	if sock := recordedSocket(); sock != "" {
		return append([]string{"-S", sock}, args...)
	}
	return args
}

func socketFile() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "tmux-agent-hub", "server-socket")
}

var recordedSocketOnce sync.Once
var recordedSocketPath string

func recordedSocket() string {
	recordedSocketOnce.Do(func() {
		if f := socketFile(); f != "" {
			if data, err := os.ReadFile(f); err == nil {
				recordedSocketPath = strings.TrimSpace(string(data))
			}
		}
	})
	return recordedSocketPath
}

// RecordSocket remembers the current server's socket path. Called from
// contexts that certainly run on the user's server (plugin.tmux via bind,
// the sidebar) — from then on hooks reach the right server no matter
// which environment spawned them.
func RecordSocket() {
	out, err := exec.Command("tmux", "display-message", "-p", "#{socket_path}").Output()
	if err != nil {
		return
	}
	sock := strings.TrimSpace(string(out))
	if sock == "" {
		return
	}
	f := socketFile()
	if f == "" {
		return
	}
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte(sock+"\n"), 0o644)
}

func run(args ...string) error {
	return exec.Command("tmux", tmuxArgs(args)...).Run()
}

func output(args ...string) (string, error) {
	out, err := exec.Command("tmux", tmuxArgs(args)...).Output()
	return string(out), err
}

// RefreshStatus asks tmux to redraw the status line. Errors are ignored:
// a failed redraw must never break a hook.
func RefreshStatus() {
	if !insideTmux() {
		return
	}
	run("refresh-client", "-S")
}

// ListPaneIDs returns the ids ("%12") of all panes in all sessions.
func ListPaneIDs() (map[string]bool, error) {
	out, err := output("list-panes", "-a", "-F", "#{pane_id}")
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			ids[line] = true
		}
	}
	return ids, nil
}

type Location struct {
	Session  string
	WindowID string
}

// PaneLocations maps every pane id to its session and window.
func PaneLocations() (map[string]Location, error) {
	out, err := output("list-panes", "-a", "-F", "#{pane_id}\t#{session_name}\t#{window_id}")
	if err != nil {
		return nil, err
	}
	locs := make(map[string]Location)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) == 3 {
			locs[parts[0]] = Location{Session: parts[1], WindowID: parts[2]}
		}
	}
	return locs, nil
}

// JumpTo focuses the given pane: session, window, pane. "=" pins the
// session name to an exact match (no fuzzy prefix matching).
func JumpTo(paneID string) error {
	locs, err := PaneLocations()
	if err != nil {
		return err
	}
	loc, ok := locs[paneID]
	if !ok {
		return fmt.Errorf("pane %s not found", paneID)
	}
	if err := run("switch-client", "-t", "="+loc.Session); err != nil {
		return err
	}
	if err := run("select-window", "-t", loc.WindowID); err != nil {
		return err
	}
	return run("select-pane", "-t", paneID)
}

func KillPane(paneID string) error { return run("kill-pane", "-t", paneID) }

// DisplayMessage shows a transient message in the tmux status line.
func DisplayMessage(msg string) { run("display-message", msg) }

// EscapeFormat neutralizes tmux format expansion in text we did not write
// — directory names, tool arguments, an agent's own words. tmux expands
// "#{...}", "#[...]" and (where jobs are allowed) "#(...)" in messages, so
// raw text could rewrite the toast around it. Doubling "#" is the escape
// tmux itself documents.
func EscapeFormat(s string) string { return strings.ReplaceAll(s, "#", "##") }

const (
	sidebarFlag  = "@tmux_agent_hub_sidebar"
	selectedFlag = "@tmux_agent_hub_selected"
)

// MarkSidebarPane tags the calling pane so ToggleSidebar can find it.
func MarkSidebarPane(paneID string) {
	run("set-option", "-p", "-t", paneID, sidebarFlag, "1")
}

// PublishSelection records which agent pane is highlighted in the sidebar,
// so prefix+<advisor key> pressed with the sidebar focused knows the source.
func PublishSelection(sidebarPane, selectedPane string) {
	run("set-option", "-p", "-t", sidebarPane, selectedFlag, selectedPane)
}

// SidebarSelection returns the highlighted agent pane if paneID is a
// sidebar pane ("" otherwise).
func SidebarSelection(paneID string) string {
	out, err := output("show-options", "-p", "-t", paneID, "-v", sidebarFlag)
	if err != nil || strings.TrimSpace(out) != "1" {
		return ""
	}
	out, err = output("show-options", "-p", "-t", paneID, "-v", selectedFlag)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// FindSidebarPane returns the sidebar pane id in the given window ("" if
// none). windowID may be empty for the current window.
func FindSidebarPane(windowID string) (string, error) {
	args := []string{"list-panes", "-F", "#{pane_id}\t#{" + sidebarFlag + "}"}
	if windowID != "" {
		args = append(args, "-t", windowID)
	}
	out, err := output(args...)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if id, flag, ok := strings.Cut(line, "\t"); ok && flag == "1" {
			return id, nil
		}
	}
	return "", nil
}

// OpenSidebar splits a full-height pane running cmd at the window edge.
func OpenSidebar(windowID, position string, width int, cmd string) error {
	args := []string{"split-window", "-h", "-f", "-l", fmt.Sprint(width)}
	if position != "right" {
		args = append(args, "-b")
	}
	if windowID != "" {
		args = append(args, "-t", windowID)
	}
	args = append(args, cmd)
	return run(args...)
}

// Bind installs a prefix binding running cmd. bind-key overwrites, so
// re-running is idempotent.
func Bind(key, cmd string) error {
	return run("bind-key", key, "run-shell", cmd)
}

// OpenPopup runs cmd in a centered popup on the current client.
func OpenPopup(cmd string, width, height int) error {
	return run("display-popup", "-E",
		"-w", fmt.Sprint(width), "-h", fmt.Sprint(height), cmd)
}

// ListWindows lists every window with its width.
func ListWindows() (map[string]int, error) {
	out, err := output("list-windows", "-a", "-F", "#{window_id}\t#{window_width}")
	if err != nil {
		return nil, err
	}
	windows := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if id, w, ok := strings.Cut(line, "\t"); ok {
			var width int
			fmt.Sscanf(w, "%d", &width)
			windows[id] = width
		}
	}
	return windows, nil
}

const sidebarAllFlag = "@tmux_agent_hub_sidebar_all"

// SidebarAllEnabled reports the global "sidebar everywhere" toggle.
func SidebarAllEnabled() bool {
	out, err := output("show-options", "-gqv", sidebarAllFlag)
	return err == nil && strings.TrimSpace(out) == "1"
}

// SetSidebarAll flips the global "sidebar everywhere" toggle.
func SetSidebarAll(on bool) {
	if on {
		run("set-option", "-g", sidebarAllFlag, "1")
	} else {
		run("set-option", "-gu", sidebarAllFlag)
	}
}

// AllSidebarPanes lists every sidebar pane across all sessions.
func AllSidebarPanes() []string {
	out, err := output("list-panes", "-a", "-F", "#{pane_id}\t#{"+sidebarFlag+"}")
	if err != nil {
		return nil
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if id, flag, ok := strings.Cut(line, "\t"); ok && flag == "1" {
			ids = append(ids, id)
		}
	}
	return ids
}

// BindSidebarPopup binds a key to open cmd in a popup with an env marker
// so the app knows to close itself after jumping.
func BindSidebarPopup(key, cmd string) error {
	return run("bind-key", key, "display-popup", "-E", "-e", "TMUX_AGENT_HUB_POPUP=1",
		"-w", "72%", "-h", "75%", cmd)
}

// BindWidePopup binds a key to open cmd in a wide popup (fzf search).
func BindWidePopup(key, cmd string) error {
	return run("bind-key", key, "display-popup", "-E", "-w", "90%", "-h", "80%", cmd)
}

// SetStatusInterval sets the status refresh period (seconds).
func SetStatusInterval(sec int) {
	run("set-option", "-g", "status-interval", fmt.Sprint(sec))
}

// RunShellDetached queues a shell command on the tmux server without
// waiting (used to chain popups: the next one opens after ours closes).
func RunShellDetached(cmd string) {
	run("run-shell", "-b", cmd)
}

// OpenPopupLarge runs cmd in a large popup (e.g. an editor).
func OpenPopupLarge(cmd string) error {
	return run("display-popup", "-E", "-w", "85%", "-h", "85%", cmd)
}

// sendSeq numbers paste buffers within a process; the pid keeps them
// unique across the concurrent hook processes that share one tmux server.
var sendSeq atomic.Uint64

// SendText types text into a pane via a paste buffer (bracketed paste, so
// TUIs treat it as one paste), waits for the app to settle, then submits
// with Enter.
//
// The buffer name is unique per call: two agents can be fed at the same
// moment (one reviewer serving several workers), and a shared name would
// let one paste consume the other's text — paste-buffer -d deletes it.
func SendText(paneID, text string) error {
	buffer := fmt.Sprintf("tmux-agent-hub-%d-%d", os.Getpid(), sendSeq.Add(1))
	// load-buffer must be pinned to the same server as the paste below,
	// or the buffer lands on whatever $TMUX happens to point at
	load := exec.Command("tmux", tmuxArgs([]string{"load-buffer", "-b", buffer, "-"})...)
	load.Stdin = strings.NewReader(text)
	if err := load.Run(); err != nil {
		return err
	}
	if err := run("paste-buffer", "-d", "-p", "-b", buffer, "-t", paneID); err != nil {
		run("delete-buffer", "-b", buffer) // paste-buffer -d never ran
		return err
	}
	time.Sleep(400 * time.Millisecond)
	if err := run("send-keys", "-t", paneID, "Enter"); err != nil {
		return fmt.Errorf("%w: %v", ErrNotSubmitted, err)
	}
	return nil
}

// ErrNotSubmitted reports the in-between outcome of SendText: the text is
// in the pane, but the Enter that submits it did not go through. Callers
// must not retry on this — the agent already has the text, and a second
// paste would duplicate it — but they cannot expect a reply either.
var ErrNotSubmitted = errors.New("pasted but not submitted")

// PaneTTY returns the pane's tty device ("" on failure).
func PaneTTY(paneID string) string {
	out, err := output("display-message", "-p", "-t", paneID, "#{pane_tty}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// StartDetached fires a tmux command without waiting for it (popups).
func StartDetached(args ...string) {
	exec.Command("tmux", tmuxArgs(args)...).Start()
}

// DisplayMessageFor shows a status-line toast on EVERY attached client,
// so it is visible no matter which session the user is looking at.
func DisplayMessageFor(msg string, ms int) {
	out, err := output("list-clients", "-F", "#{client_name}")
	clients := strings.Fields(strings.TrimSpace(out))
	if err != nil || len(clients) == 0 {
		run("display-message", "-d", fmt.Sprint(ms), msg)
		return
	}
	for _, c := range clients {
		run("display-message", "-c", c, "-d", fmt.Sprint(ms), msg)
	}
}

// PaneCwd returns the pane's current working directory ("" on failure).
func PaneCwd(paneID string) string {
	out, err := output("display-message", "-p", "-t", paneID, "#{pane_current_path}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// PaneInfo describes a pane for agent-to-pane resolution.
type PaneInfo struct {
	ID      string
	Path    string
	Command string
	PID     int
}

// PanesFull lists all panes with their working directory and command.
func PanesFull() ([]PaneInfo, error) {
	out, err := output("list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_path}\t#{pane_current_command}\t#{pane_pid}")
	if err != nil {
		return nil, err
	}
	var infos []PaneInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) == 4 {
			info := PaneInfo{ID: parts[0], Path: parts[1], Command: parts[2]}
			fmt.Sscanf(parts[3], "%d", &info.PID)
			infos = append(infos, info)
		}
	}
	return infos, nil
}

// PaneByTTY maps a tty device (e.g. "/dev/ttys013" or "ttys013") to the
// pane running on it.
func PaneByTTY(tty string) string {
	if tty == "" || strings.Contains(tty, "?") {
		return ""
	}
	if !strings.HasPrefix(tty, "/dev/") {
		tty = "/dev/" + tty
	}
	out, err := output("list-panes", "-a", "-F", "#{pane_id}\t#{pane_tty}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if id, paneTTY, ok := strings.Cut(line, "\t"); ok && paneTTY == tty {
			return id
		}
	}
	return ""
}

// ProcessTTY returns the controlling tty of a process ("" when detached).
func ProcessTTY(pid int) string {
	out, err := exec.Command("ps", "-o", "tty=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return ""
	}
	tty := strings.TrimSpace(string(out))
	if tty == "??" || tty == "-" {
		return ""
	}
	return tty
}

// TTYOfProcessWithArg finds the tty of a process whose command line
// contains needle (e.g. a session id in a daemon viewer client).
func TTYOfProcessWithArg(needle string) string {
	if needle == "" {
		return ""
	}
	out, err := exec.Command("ps", "-axo", "tty=,command=").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		tty, cmd, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || tty == "??" || !strings.Contains(cmd, needle) {
			continue
		}
		return tty
	}
	return ""
}

// PaneProcessCmdlines returns the command lines of every process in the
// pane's tree (for extracting --session-id / --resume hints).
func PaneProcessCmdlines(panePID int) []string {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	cmdline := map[int]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		var pid, ppid int
		fmt.Sscanf(fields[0], "%d", &pid)
		fmt.Sscanf(fields[1], "%d", &ppid)
		children[ppid] = append(children[ppid], pid)
		cmdline[pid] = strings.Join(fields[2:], " ")
	}
	var cmds []string
	var walk func(pid, depth int)
	walk = func(pid, depth int) {
		if depth > 6 {
			return
		}
		if c := cmdline[pid]; c != "" {
			cmds = append(cmds, c)
		}
		for _, ch := range children[pid] {
			walk(ch, depth+1)
		}
	}
	walk(panePID, 0)
	return cmds
}

// CaptureTail returns the last lines of a pane's visible content plus
// scrollback (whitespace-collapsed) for screen-content matching.
func CaptureTail(paneID string, lines int) string {
	out, err := output("capture-pane", "-p", "-t", paneID, "-S", fmt.Sprint(-lines))
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(out), " ")
}

// PaneHasAgentProcess reports whether the pane's process tree contains an
// agent binary — a pane can show "zsh" while an agent runs a Bash tool
// inside it.
// ProcessTree is one snapshot of the process table. Answering "is there
// an agent under this pane" costs a full ps, so callers that ask about
// many panes take a snapshot once and query it.
type ProcessTree struct {
	children map[int][]int
	agentish map[int]bool
}

// Snapshot reads the process table once.
func Snapshot() (ProcessTree, error) {
	t := ProcessTree{children: map[int][]int{}, agentish: map[int]bool{}}
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return t, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var pid, ppid int
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		fmt.Sscanf(fields[0], "%d", &pid)
		fmt.Sscanf(fields[1], "%d", &ppid)
		cmd := strings.Join(fields[2:], " ")
		t.children[ppid] = append(t.children[ppid], pid)
		if strings.Contains(cmd, "claude") || strings.Contains(cmd, "codex") ||
			strings.Contains(cmd, "/versions/") {
			t.agentish[pid] = true
		}
	}
	return t, nil
}

// HasAgent reports whether an agent process runs under this pane — the
// pane's own command may be a shell while the agent runs a Bash tool.
func (t ProcessTree) HasAgent(panePID int) bool {
	var walk func(pid, depth int) bool
	walk = func(pid, depth int) bool {
		if t.agentish[pid] {
			return true
		}
		if depth > 6 {
			return false
		}
		for _, c := range t.children[pid] {
			if walk(c, depth+1) {
				return true
			}
		}
		return false
	}
	return walk(panePID, 0)
}

func PaneHasAgentProcess(panePID int) bool {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return false
	}
	children := map[int][]int{}
	agentish := map[int]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var pid, ppid int
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		fmt.Sscanf(fields[0], "%d", &pid)
		fmt.Sscanf(fields[1], "%d", &ppid)
		cmd := strings.Join(fields[2:], " ")
		children[ppid] = append(children[ppid], pid)
		if strings.Contains(cmd, "claude") || strings.Contains(cmd, "codex") ||
			strings.Contains(cmd, "/versions/") {
			agentish[pid] = true
		}
	}
	var walk func(pid, depth int) bool
	walk = func(pid, depth int) bool {
		if agentish[pid] {
			return true
		}
		if depth > 6 {
			return false
		}
		for _, c := range children[pid] {
			if walk(c, depth+1) {
				return true
			}
		}
		return false
	}
	return walk(panePID, 0)
}

// PaneIsFocused reports whether the pane is the active pane of the active
// window of an attached session — i.e. keystrokes reaching it were really
// meant for it. Fail-open on errors.
func PaneIsFocused(paneID string) bool {
	out, err := output("display-message", "-p", "-t", paneID,
		"#{pane_active} #{window_active} #{session_attached}")
	if err != nil {
		return true
	}
	parts := strings.Fields(strings.TrimSpace(out))
	return len(parts) == 3 && parts[0] == "1" && parts[1] == "1" && parts[2] != "0"
}

// PaneSyncOn reports whether the pane's window has synchronize-panes
// enabled — keystrokes then go to every pane at once.
func PaneSyncOn(paneID string) bool {
	out, err := output("display-message", "-p", "-t", paneID, "#{?synchronize-panes,1,0}")
	return err == nil && strings.TrimSpace(out) == "1"
}
