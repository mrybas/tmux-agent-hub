package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mrybas/tmux-agent-hub/internal/advisor"
	"os/exec"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/detect"
	"github.com/mrybas/tmux-agent-hub/internal/eventlog"
	"github.com/mrybas/tmux-agent-hub/internal/finder"
	"github.com/mrybas/tmux-agent-hub/internal/hookd"
	"github.com/mrybas/tmux-agent-hub/internal/layout"
	"github.com/mrybas/tmux-agent-hub/internal/sidebar"
	"github.com/mrybas/tmux-agent-hub/internal/skills"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/statusline"
	"github.com/mrybas/tmux-agent-hub/internal/tmuxctl"
	"github.com/mrybas/tmux-agent-hub/internal/transcript"
)

const usage = `tmux-agent-hub — tmux plugin for working with AI agents

Setup:
  tmux-agent-hub install-hooks    register hooks in ~/.claude/settings.json
                                  (and ~/.codex/hooks.json when Codex is set up)
  tmux-agent-hub uninstall-hooks  remove every tmux-agent-hub hook entry
  tmux-agent-hub config init      write a commented default config.toml
  tmux-agent-hub bind             install prefix key bindings from the config

Looking at agents:
  tmux-agent-hub state list       show all tracked agent panes
  tmux-agent-hub sidebar          run the sidebar TUI (inside a tmux pane)
  tmux-agent-hub sidebar-toggle [window]  open/close the sidebar in a window
  tmux-agent-hub next             jump to the next agent that needs you
  tmux-agent-hub find             fzf search across agents (needs fzf)
  tmux-agent-hub skills [pane|dir]  what an agent sees (skills/commands/mcp/memory)

Agents talking to agents:
  tmux-agent-hub advisor-send <src> <dst> <template>  send src's last reply to dst
  tmux-agent-hub advisor-assign <worker> <reviewer|none>  link a live reviewer
  tmux-agent-hub send-text <pane> [file]  type a prompt into an agent (stdin by default)

Measuring and debugging:
  tmux-agent-hub replay [path]    run the stuck-detectors over recorded transcripts
  tmux-agent-hub metrics <pane>   one agent's run as JSON (work, findings, advisor)
  tmux-agent-hub cleanup          drop state for panes that no longer exist

Called by tmux and by the agents themselves:
  tmux-agent-hub hook [agent]     handle an agent hook event (payload on stdin)
  tmux-agent-hub statusline       render status-bar glyphs (used from status-right)
  tmux-agent-hub focus <pane>     reset "done" on pane focus (used from a tmux hook)

  tmux-agent-hub version          print the build version
`

// version is stamped at build time (-ldflags "-X main.version=…"); a
// plain "go build" leaves it "dev".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "hook":
		agent := "claude"
		if len(os.Args) > 2 {
			agent = os.Args[2]
		}
		err = hookd.Handle(os.Stdin, agent)
	case "install-hooks":
		err = installHooks()
	case "uninstall-hooks":
		err = hookd.Uninstall()
	case "state":
		err = stateList()
	case "cleanup":
		err = cleanup()
	case "statusline":
		err = statuslineCmd()
	case "focus":
		if len(os.Args) > 2 {
			err = focusCmd(os.Args[2])
		}
	case "sidebar":
		err = sidebarCmd()
	case "sidebar-all":
		err = sidebarAllCmd()
	case "sidebar-ensure":
		if len(os.Args) > 2 {
			err = sidebarEnsureCmd(os.Args[2])
		}
	case "sidebar-toggle":
		windowID := ""
		if len(os.Args) > 2 {
			windowID = os.Args[2]
		}
		err = sidebarToggleCmd(windowID)
	case "next":
		err = nextCmd()
	case "advisor":
		if len(os.Args) > 2 {
			err = advisorCmd(os.Args[2])
		}
	case "advisor-popup":
		// invoked via run-shell (which expands #{pane_id} correctly, unlike
		// display-popup, whose expansion sees the popup's own hidden pane)
		if len(os.Args) > 2 {
			err = advisorPopupCmd(os.Args[2])
		}
	case "advisor-assign":
		if len(os.Args) > 3 {
			err = advisorAssignCmd(os.Args[2], os.Args[3])
		} else {
			err = fmt.Errorf("usage: tmux-agent-hub advisor-assign <primary-pane> <reviewer-pane|none>")
		}
	case "advisor-send":
		if len(os.Args) > 4 {
			err = advisorSendCmd(os.Args[2], os.Args[3], os.Args[4])
		} else {
			err = fmt.Errorf("usage: tmux-agent-hub advisor-send <source-pane> <target-pane> <template>")
		}
	case "bind":
		err = bindCmd()
	case "find":
		err = findCmd()
	case "find-feed":
		err = findFeedCmd()
	case "preview":
		if len(os.Args) > 2 {
			err = previewCmd(os.Args[2])
		}
	case "send-text":
		if len(os.Args) > 2 {
			err = sendTextCmd(os.Args[2], os.Args[3:])
		} else {
			err = fmt.Errorf("usage: tmux-agent-hub send-text <pane> [file]  (stdin when no file)")
		}
	case "metrics":
		if len(os.Args) > 2 {
			err = metricsCmd(os.Args[2])
		} else {
			err = fmt.Errorf("usage: tmux-agent-hub metrics <pane>")
		}
	case "replay":
		err = replayCmd(os.Args[2:])
	case "skills":
		arg := ""
		if len(os.Args) > 2 {
			arg = os.Args[2]
		}
		err = skillsCmd(arg)
	case "edit-popup":
		// opens the configured editor in a popup (chained from the sidebar
		// popup, which must close first — tmux allows one popup per client)
		if len(os.Args) > 2 {
			cfg, _ := config.Load()
			err = tmuxctl.OpenPopupLarge(fmt.Sprintf("%s %q", cfg.EditorCommand(), os.Args[2]))
		}
	case "version", "--version", "-v":
		fmt.Println("tmux-agent-hub", version)
	case "config":
		if len(os.Args) > 2 && os.Args[2] == "init" {
			err = config.Init()
		} else {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tmux-agent-hub: %v\n", err)
		os.Exit(1)
	}
}

func installHooks() error {
	bin, err := executablePath()
	if err != nil {
		return err
	}
	if err := hookd.Install(bin); err != nil {
		return err
	}
	fmt.Printf("Hooks installed in ~/.claude/settings.json (command: %s hook)\n", bin)
	fmt.Println("Note: already-running Claude sessions pick this up only after restart.")
	return nil
}

func stateList() error {
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	panes, err := store.List()
	if err != nil {
		return err
	}
	if len(panes) == 0 {
		fmt.Println("no tracked agents")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PANE\tAGENT\tSTATUS\tSINCE\tCWD\tTOOL\tLAST PROMPT")
	for _, p := range panes {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.PaneID, p.Agent, p.Status, since(p.StatusSince),
			shortPath(p.Cwd), p.CurrentTool, clip(p.LastPrompt, 40))
	}
	return w.Flush()
}

func cleanup() error {
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	alive, err := tmuxctl.ListPaneIDs()
	if err != nil {
		return fmt.Errorf("tmux not running? %w", err)
	}
	panes, err := store.List()
	if err != nil {
		return err
	}
	removed := 0
	for _, p := range panes {
		if !p.AliveIn(alive) {
			if err := store.Delete(p.PaneID); err != nil {
				return err
			}
			removed++
		}
	}
	fmt.Printf("removed %d stale entries\n", removed)
	return nil
}

// claudeProjectDir encodes a cwd the way Claude Code names its project
// transcript folders: every non-alphanumeric rune becomes "-".
func claudeProjectDir(cwd string) string {
	enc := []rune(cwd)
	for i, r := range enc {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			enc[i] = '-'
		}
	}
	return string(enc)
}

var sessionIDRe = regexp.MustCompile(`--session-id[= ]([0-9a-f-]{36})`)
var resumeRe = regexp.MustCompile(`--resume[= ](\S+\.jsonl)`)

// paneSessionHints extracts the agent's session id and transcript from
// the pane's process command lines — deterministic even when several
// agents share one directory.
func paneSessionHints(panePID int) (sessionID, transcriptPath string) {
	for _, cmd := range tmuxctl.PaneProcessCmdlines(panePID) {
		if m := sessionIDRe.FindStringSubmatch(cmd); m != nil && sessionID == "" {
			sessionID = m[1]
		}
		if m := resumeRe.FindStringSubmatch(cmd); m != nil && transcriptPath == "" {
			transcriptPath = m[1]
		}
	}
	return
}

// findTranscript locates a session's transcript: first in the cwd's own
// project folder, then anywhere under ~/.claude/projects (sessions can
// start from a different directory).
func findTranscript(home, cwd, sessionID string) string {
	if home == "" || sessionID == "" {
		return ""
	}
	direct := filepath.Join(home, ".claude", "projects", claudeProjectDir(cwd), sessionID+".jsonl")
	if _, err := os.Stat(direct); err == nil {
		return direct
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// adoptUntracked persists state for agent panes that predate the hook
// install: they become normal sidebar entries, and their transcript (when
// unambiguous) makes the advisor work for them right away. Their status
// stays a best-effort "waiting_input" until the session is restarted with
// hooks.
func adoptUntracked(store *state.Store, panes []*state.Pane) bool {
	infos, err := tmuxctl.PanesFull()
	if err != nil {
		return false
	}
	cfg, _ := config.Load()
	tracked := map[string]bool{}
	cwdCount := map[string]int{}
	for _, p := range panes {
		tracked[p.PaneID] = true
	}
	untracked := sidebar.UntrackedAgents(infos, tracked, "")
	for _, u := range untracked {
		cwdCount[u.Cwd]++
	}
	adopted := false
	home, _ := os.UserHomeDir()
	pidByPane := map[string]int{}
	for _, info := range infos {
		pidByPane[info.ID] = info.PID
	}
	// enrich already-tracked entries that miss their transcript: process
	// command lines can pin the session deterministically
	for _, p := range panes {
		if p.TranscriptPath != "" || p.ParentPane != "" {
			continue
		}
		sid, tr := paneSessionHints(pidByPane[p.PaneID])
		if sid == "" && tr == "" {
			continue
		}
		if sid != "" && p.SessionID == "" {
			p.SessionID = sid
			p.TranscriptPath = findTranscript(home, p.Cwd, sid)
		}
		if tr != "" && p.TranscriptPath == "" {
			p.TranscriptPath = tr
		}
		if p.TranscriptPath != "" || p.SessionID != "" {
			if store.Save(p) == nil {
				adopted = true
			}
		}
	}
	for _, u := range untracked {
		u.Title = "" // a tracked entry now — no "untracked" label
		u.StatusSince = time.Now()
		u.UpdatedAt = time.Now()
		if !attachSession(u, store, home, cwdCount[u.Cwd] == 1, cfg.Advisor.StaleWorkingAfter()) {
			continue
		}
		if store.Save(u) == nil {
			adopted = true
		}
	}
	return adopted
}

// attachSession decides whether an untracked agent pane may be adopted,
// and fills in the session it belongs to. Both halves are one question: a
// pane is only worth tracking when we can say which session is in it.
func attachSession(u *state.Pane, store *state.Store, home string, alone bool, fresh time.Duration) bool {
	if store.Ended(u.PaneID) {
		// the session in this pane ended; whatever is still running there
		// is an agent at its exit prompt, not an agent at work
		return false
	}
	// the process command line pins the session deterministically — but
	// only for a pid we actually have: asking about pid 0 walks whatever
	// the process table starts with and hands back a stranger's session
	pid := paneProcessID(u.PaneID)
	if sid, tr := hintsFor(pid); sid != "" || tr != "" {
		u.SessionID, u.TranscriptPath = sid, tr
		if tr == "" && sid != "" {
			u.TranscriptPath = findTranscript(home, u.Cwd, sid)
		}
	}
	if u.TranscriptPath == "" && u.Agent == "claude" && home != "" && alone {
		// the only agent in this directory: the newest transcript of the
		// matching project is its — but only if someone is still writing to
		// it. An older one belongs to a session that has finished, and
		// attaching it dresses a pane in the last words of the dead.
		glob := filepath.Join(home, ".claude", "projects", claudeProjectDir(u.Cwd), "*.jsonl")
		if matches, err := filepath.Glob(glob); err == nil {
			newest, newestAt := "", time.Time{}
			for _, m := range matches {
				if st, err := os.Stat(m); err == nil && st.ModTime().After(newestAt) {
					newest, newestAt = m, st.ModTime()
				}
			}
			if newest != "" && time.Since(newestAt) < fresh {
				u.TranscriptPath = newest
				u.SessionID = strings.TrimSuffix(filepath.Base(newest), ".jsonl")
			}
		}
	}
	// Codex has no per-directory transcripts: its rollouts live in one
	// dated tree, so "the newest one" is as likely to belong to another
	// pane as to this one. Guessing there put a stranger's session in the
	// sidebar, so untracked Codex panes are only adopted when their own
	// process says which session they are — with hooks installed they are
	// tracked the moment they do anything anyway.
	return u.TranscriptPath != ""
}

// hintsFor is paneSessionHints for a pid we are sure of.
func hintsFor(pid int) (sessionID, transcriptPath string) {
	if pid <= 0 {
		return "", ""
	}
	return paneSessionHints(pid)
}

// paneProcessID is the pid of the process running in a pane ("" pane or a
// tmux hiccup gives 0, which no process tree matches).
func paneProcessID(paneID string) int {
	if info, ok := tmuxctl.PaneInfoFor(paneID); ok {
		return info.PID
	}
	return 0
}

// statuslineCmd prints the glyph segment. It also self-cleans: state for
// panes that disappeared is dropped on the spot, so a separate reaper
// isn't needed for the common case.
func statuslineCmd() error {
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	panes, err := store.List()
	if err != nil {
		return err
	}
	if alive, err := tmuxctl.ListPaneIDs(); err == nil {
		kept := panes[:0]
		for _, p := range panes {
			if p.AliveIn(alive) {
				kept = append(kept, p)
			} else {
				store.Delete(p.PaneID)
			}
		}
		panes = kept
	}
	hookd.ReconcileInterrupted(store, panes)
	if hookd.ReconcileDeparted(store, panes) {
		panes, _ = store.List() // an agent exited without a SessionEnd hook
	}
	if adoptUntracked(store, panes) {
		panes, _ = store.List() // re-read to include the adopted agents
	}
	cfg, _ := config.Load() // broken config → defaults, keep rendering
	opts := cfg.StatuslineOptions()
	opts.BlinkPhase = time.Now().Unix()%2 == 1
	fmt.Print(statusline.Render(panes, opts))
	return nil
}

func sidebarCmd() error {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tmux-agent-hub: config: %v (using defaults)\n", err)
	}
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	return sidebar.Run(cfg, store)
}

func sidebarToggleCmd(windowID string) error {
	if id, err := tmuxctl.FindSidebarPane(windowID); err == nil && id != "" {
		return tmuxctl.KillPane(id)
	}
	cfg, _ := config.Load()
	bin, err := executablePath()
	if err != nil {
		return err
	}
	return tmuxctl.OpenSidebar(windowID, cfg.Sidebar.Position, cfg.Sidebar.Width,
		fmt.Sprintf("%q sidebar", bin))
}

// sidebarAllCmd toggles "sidebar in every window": opens one per window
// (wide enough), and tmux hooks keep new windows covered while enabled.
func sidebarAllCmd() error {
	if tmuxctl.SidebarAllEnabled() {
		tmuxctl.SetSidebarAll(false)
		for _, id := range tmuxctl.AllSidebarPanes() {
			tmuxctl.KillPane(id)
		}
		tmuxctl.DisplayMessage("tmux-agent-hub: sidebars everywhere OFF")
		return nil
	}
	tmuxctl.SetSidebarAll(true)
	windows, err := tmuxctl.ListWindows()
	if err != nil {
		return err
	}
	for id := range windows {
		sidebarEnsureCmd(id)
	}
	tmuxctl.DisplayMessage("tmux-agent-hub: sidebars everywhere ON")
	return nil
}

// sidebarEnsureCmd opens a sidebar in the window when the global mode is
// on, the window has none yet, and it is wide enough to lose a column.
func sidebarEnsureCmd(windowID string) error {
	if !tmuxctl.SidebarAllEnabled() {
		return nil
	}
	if id, err := tmuxctl.FindSidebarPane(windowID); err != nil || id != "" {
		return nil
	}
	cfg, _ := config.Load()
	windows, err := tmuxctl.ListWindows()
	if err != nil {
		return err
	}
	if width, ok := windows[windowID]; !ok || width < cfg.Sidebar.Width+40 {
		return nil // window too narrow (or gone) — skip it
	}
	bin, err := executablePath()
	if err != nil {
		return err
	}
	return tmuxctl.OpenSidebar(windowID, cfg.Sidebar.Position, cfg.Sidebar.Width,
		fmt.Sprintf("%q sidebar", bin))
}

// nextCmd jumps to the agent most in need of attention.
func nextCmd() error {
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	panes, err := store.List()
	if err != nil {
		return err
	}
	if alive, err := tmuxctl.ListPaneIDs(); err == nil {
		kept := panes[:0]
		for _, p := range panes {
			if p.AliveIn(alive) {
				kept = append(kept, p)
			}
		}
		panes = kept
	}
	next := state.ChooseNext(panes)
	if next == nil {
		tmuxctl.DisplayMessage("tmux-agent-hub: nobody is waiting for you")
		return nil
	}
	if next.ParentPane != "" { // teammates have no pane — go to the parent
		return tmuxctl.JumpTo(next.ParentPane)
	}
	return tmuxctl.JumpTo(next.PaneID)
}

// alivePanes returns tracked agents that are still meaningful, with
// their locations.
func alivePanes() ([]*state.Pane, map[string]tmuxctl.Location, error) {
	store, err := state.DefaultStore()
	if err != nil {
		return nil, nil, err
	}
	panes, err := store.List()
	if err != nil {
		return nil, nil, err
	}
	locs, err := tmuxctl.PaneLocations()
	if err != nil {
		return panes, map[string]tmuxctl.Location{}, nil
	}
	alive := map[string]bool{}
	for id := range locs {
		alive[id] = true
	}
	kept := panes[:0]
	for _, p := range panes {
		if p.AliveIn(alive) {
			kept = append(kept, p)
		}
	}
	return kept, locs, nil
}

// findFeedCmd prints the searchable line for every agent (fzf input).
func findFeedCmd() error {
	panes, locs, err := alivePanes()
	if err != nil {
		return err
	}
	cfg, _ := config.Load()
	for _, p := range panes {
		fmt.Println(finder.Line(cfg, p, locs[p.PaneID].Session))
	}
	return nil
}

// previewCmd renders the fzf preview pane for one agent.
func previewCmd(paneID string) error {
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	p, err := store.Load(paneID)
	if err != nil {
		return fmt.Errorf("unknown agent %s", paneID)
	}
	locs, _ := tmuxctl.PaneLocations()
	cfg, _ := config.Load()
	fmt.Print(finder.Preview(cfg, p, locs[p.PaneID].Session))
	return nil
}

// findCmd runs fzf over the agents (designed for a tmux popup):
// Enter jumps to the agent, Ctrl-S opens the advisor with it as source.
func findCmd() error {
	if _, err := exec.LookPath("fzf"); err != nil {
		return fmt.Errorf("fzf not found in PATH")
	}
	bin, err := executablePath()
	if err != nil {
		return err
	}
	fzf := exec.Command("fzf",
		"--ansi", "--delimiter=\t", "--with-nth=2..",
		"--no-sort", "--layout=reverse",
		"--prompt=agent> ",
		"--expect=ctrl-s",
		"--preview", fmt.Sprintf("%q preview {1}", bin),
		"--preview-window=right,55%,wrap",
		"--header=enter jump · ctrl-s advisor")
	feed, err := exec.Command(bin, "find-feed").Output()
	if err != nil {
		return err
	}
	fzf.Stdin = strings.NewReader(string(feed))
	fzf.Stderr = os.Stderr
	out, err := fzf.Output()
	if err != nil {
		return nil // cancelled (esc) — not an error
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 2 {
		return nil
	}
	key := lines[0]
	paneID, _, _ := strings.Cut(lines[1], "\t")
	if paneID == "" {
		return nil
	}
	if key == "ctrl-s" {
		// this process lives inside a popup — chain the advisor popup
		// after ours closes
		return exec.Command("tmux", "run-shell", "-b",
			fmt.Sprintf("sleep 0.3; %q advisor-popup '%s'", bin, paneID)).Run()
	}
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	if p, err := store.Load(paneID); err == nil && p.ParentPane != "" {
		paneID = p.ParentPane // teammates have no pane of their own
	}
	return tmuxctl.JumpTo(paneID)
}

// sendTextCmd types a prompt into an agent's pane — the scripting entry
// point for driving an agent from outside its terminal.
func sendTextCmd(pane string, rest []string) error {
	var data []byte
	var err error
	if len(rest) > 0 && rest[0] != "-" {
		data, err = os.ReadFile(rest[0])
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("nothing to send")
	}
	return tmuxctl.SendText(pane, string(data))
}

// metricsCmd prints one agent's run as JSON: how much work it did, what
// the detectors saw and what the advisor did about it — the counterpart
// of send-text when a run is scripted rather than watched.
func metricsCmd(pane string) error {
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	p, err := store.Load(pane)
	if err != nil {
		return fmt.Errorf("%s is not a tracked agent", pane)
	}
	cfg, _ := config.Load()

	out := map[string]any{
		"pane": p.PaneID, "agent": p.Agent, "cwd": p.Cwd,
		"model": p.Model, "status": string(p.Display()),
	}
	if p.TranscriptPath != "" {
		calls, err := transcript.For(p.Agent, p.TranscriptPath).ToolCalls(5000)
		if err == nil {
			edits, failures := 0, 0
			for _, c := range calls {
				switch c.Tool {
				case "Edit", "Write", "MultiEdit", "NotebookEdit", "apply_patch":
					edits++
				}
				if c.IsError {
					failures++
				}
			}
			findings := map[string]int{}
			seen := map[string]bool{}
			for i := 1; i <= len(calls); i++ {
				f := detect.Analyze(calls[:i], cfg.DetectThresholds())
				if f == nil {
					continue
				}
				key := f.Kind + "|" + f.Signature
				if !seen[key] {
					seen[key] = true
					findings[f.Kind]++
				}
			}
			out["tool_calls"] = len(calls)
			out["edits"] = edits
			out["failed_calls"] = failures
			out["findings"] = findings
			if len(calls) > 0 && !calls[0].Time.IsZero() && !calls[len(calls)-1].Time.IsZero() {
				out["span_seconds"] = int(calls[len(calls)-1].Time.Sub(calls[0].Time).Seconds())
			}
		}
	}
	out["advisor"] = advisorCounts(p.PaneID)
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// advisorCounts summarizes advisor.log for one worker.
func advisorCounts(pane string) map[string]int {
	counts := map[string]int{}
	path := eventlog.Path("advisor")
	if path == "" {
		return counts
	}
	f, err := os.Open(path)
	if err != nil {
		return counts
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var rec eventlog.Advice
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Worker != pane {
			continue
		}
		counts[rec.Event]++
	}
	return counts
}

// replayCmd runs the stuck-detectors over recorded transcripts without
// touching any agent: the offline half of the measurement harness. With
// no arguments it replays every tracked agent; a path replays one
// transcript (or a directory of them).
func replayCmd(args []string) error {
	cfg, _ := config.Load()
	th := cfg.DetectThresholds()
	stats, only := false, ""
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch {
		case args[0] == "--stats":
			stats = true
		case strings.HasPrefix(args[0], "--only="):
			only = strings.TrimPrefix(args[0], "--only=")
		case strings.Contains(args[0], "="):
			// threshold override, e.g. --repeat=4 — the tuning knob
			name, value, _ := strings.Cut(strings.TrimPrefix(args[0], "--"), "=")
			var n int
			fmt.Sscanf(value, "%d", &n)
			switch name {
			case "window":
				th.Window = n
			case "repeat":
				th.Repeat = n
			case "same-failure":
				th.SameFailure = n
			case "error-streak":
				th.ErrorStreak = n
			case "oscillation":
				th.Oscillation = n
			case "no-progress":
				th.NoProgress = n
			}
		}
		args = args[1:]
	}

	type target struct{ agent, path, label string }
	var targets []target
	if len(args) > 0 {
		root := args[0]
		info, err := os.Stat(root)
		if err != nil {
			return err
		}
		if info.IsDir() {
			matches, _ := filepath.Glob(filepath.Join(root, "*.jsonl"))
			for _, m := range matches {
				targets = append(targets, target{"claude", m, filepath.Base(m)})
			}
		} else {
			agent := "claude"
			if strings.Contains(filepath.Base(root), "rollout-") {
				agent = "codex"
			}
			targets = append(targets, target{agent, root, filepath.Base(root)})
		}
	} else {
		store, err := state.DefaultStore()
		if err != nil {
			return err
		}
		panes, err := store.List()
		if err != nil {
			return err
		}
		for _, p := range panes {
			if p.TranscriptPath == "" {
				continue
			}
			targets = append(targets, target{p.Agent, p.TranscriptPath,
				p.PaneID + " " + shortPath(p.Cwd)})
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("nothing to replay")
	}
	fmt.Printf("thresholds: %+v (enabled=%v)\n", th, cfg.Detect.Enabled)

	kinds := map[string]int{}
	sessions, flagged, totalCalls := 0, 0, 0
	for _, t := range targets {
		calls, err := transcript.For(t.agent, t.path).ToolCalls(5000)
		if err != nil || len(calls) == 0 {
			continue
		}
		sessions++
		totalCalls += len(calls)
		if stats {
			printReplayStats(t.label, calls)
			continue
		}
		// walk the session as it happened, reporting each new finding once
		seen := map[string]bool{}
		hits := 0
		for i := 1; i <= len(calls); i++ {
			f := detect.Analyze(calls[:i], th)
			if f == nil {
				continue
			}
			if only != "" && f.Kind != only {
				continue
			}
			key := f.Kind + "|" + f.Signature
			if seen[key] {
				continue
			}
			seen[key] = true
			kinds[f.Kind]++
			hits++
			if hits <= 3 || only != "" {
				fmt.Printf("  %s\n    call %d/%d · %s · %s\n", t.label, i, len(calls), f.Kind, f.Reason)
			}
		}
		if hits > 0 {
			flagged++
			if hits > 3 && only == "" {
				fmt.Printf("    … %d more findings in this session\n", hits-3)
			}
		}
	}
	fmt.Printf("\n%d sessions, %d tool calls: %d sessions flagged\n", sessions, totalCalls, flagged)
	for _, kind := range []string{"same_failure", "error_streak", "repeat", "oscillation", "no_progress"} {
		if n := kinds[kind]; n > 0 {
			fmt.Printf("  %-14s %d\n", kind, n)
		}
	}
	return nil
}

// printReplayStats describes what a session's tool calls look like — the
// raw material thresholds are tuned against.
func printReplayStats(label string, calls []transcript.ToolCall) {
	withResult, failures := 0, 0
	maxRepeat, maxNoEdit, maxErrStreak := 1, 0, 0
	repeat, noEdit, errStreak := 1, 0, 0
	mutating := map[string]bool{"Edit": true, "Write": true, "MultiEdit": true,
		"NotebookEdit": true, "apply_patch": true}
	for i, c := range calls {
		if c.Result != "" {
			withResult++
		}
		failed := c.IsError || (c.Tool == "Bash" || c.Tool == "shell" || c.Tool == "exec") &&
			(strings.Contains(strings.ToLower(c.Result), "error:") ||
				strings.Contains(strings.ToLower(c.Result), "fail") ||
				strings.Contains(c.Result, "exit code 1"))
		if failed {
			failures++
			errStreak++
		} else {
			errStreak = 0
		}
		maxErrStreak = max(maxErrStreak, errStreak)
		if mutating[c.Tool] {
			noEdit = 0
		} else {
			noEdit++
		}
		maxNoEdit = max(maxNoEdit, noEdit)
		if i > 0 && detect.Signature(calls[i-1]) == detect.Signature(c) {
			repeat++
		} else {
			repeat = 1
		}
		maxRepeat = max(maxRepeat, repeat)
	}
	fmt.Printf("%-42s calls %4d  results %3d%%  failures %3d  max: repeat %d, err-streak %d, no-edit %d\n",
		clip(label, 42), len(calls), withResult*100/max(len(calls), 1), failures,
		maxRepeat, maxErrStreak, maxNoEdit)
}

// skillsCmd prints what an agent sees: by pane id, by path, or for the
// current directory.
func skillsCmd(arg string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cwd, agent := "", "claude"
	switch {
	case strings.HasPrefix(arg, "%"):
		store, err := state.DefaultStore()
		if err != nil {
			return err
		}
		p, err := store.Load(arg)
		if err != nil {
			return fmt.Errorf("%s is not a tracked agent", arg)
		}
		cwd, agent = p.Cwd, p.Agent
	case arg != "":
		cwd = arg
	default:
		if cwd, err = os.Getwd(); err != nil {
			return err
		}
	}
	for _, line := range skills.Inspect(home, cwd, agent).Lines(home) {
		fmt.Println(line)
	}
	return nil
}

// advisorCmd runs the interactive picker (inside a tmux popup).
func advisorCmd(srcPane string) error {
	cfg, _ := config.Load()
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	return advisor.RunWizard(cfg, store, srcPane)
}

// advisorPopupCmd opens the wizard in a tmux popup for a concrete source
// pane (already expanded by run-shell).
func advisorPopupCmd(srcPane string) error {
	// pressed with the sidebar focused: the highlighted agent is the source
	if sel := tmuxctl.SidebarSelection(srcPane); sel != "" {
		srcPane = sel
	}
	bin, err := executablePath()
	if err != nil {
		return err
	}
	return tmuxctl.OpenPopup(fmt.Sprintf("%q advisor '%s'", bin, srcPane), 72, 20)
}

// advisorAssignCmd links a live reviewer to a primary agent (or "none" to
// unlink). From then on the primary's turn-deltas are fed to the reviewer
// and non-"OK" replies come back as advice — see internal/hookd/live.go.
func advisorAssignCmd(primaryPane, reviewerPane string) error {
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	if reviewerPane == "none" {
		return store.UnlinkReviewer(primaryPane)
	}
	return store.LinkReviewer(primaryPane, reviewerPane)
}

// advisorSendCmd is the non-interactive path (scripting and tests).
func advisorSendCmd(srcPane, dstPane, tplName string) error {
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	tpl, ok := advisor.FindTemplate(tplName)
	if !ok {
		return fmt.Errorf("unknown template %q", tplName)
	}
	src, _ := store.Load(srcPane)
	cfg, _ := config.Load()
	msg := advisor.BuildMessage(tpl, src, advisor.SourceOutput(tpl, src), cfg.OutputBudget())
	return tmuxctl.SendText(dstPane, msg)
}

func bindCmd() error {
	tmuxctl.RecordSocket() // bind runs from plugin.tmux on the user's server
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.BlinkConfigured() {
		// the software pulse needs the status line re-rendered every second
		tmuxctl.SetStatusInterval(1)
	}
	// every binding also fires from Ukrainian/Russian layouts: bind the
	// Cyrillic characters living on the same physical keys
	bindAll := func(key string, bind func(k string) error) error {
		if err := bind(key); err != nil {
			return err
		}
		for _, alt := range layout.Alts(key) {
			bind(alt) // best-effort: an exotic terminal may reject unicode keys
		}
		return nil
	}
	bin, err := executablePath()
	if err != nil {
		return err
	}
	if err := bindAll(cfg.Keys.Sidebar, func(k string) error {
		return tmuxctl.Bind(k, fmt.Sprintf("%q sidebar-toggle '#{window_id}'", bin))
	}); err != nil {
		return err
	}
	if err := bindAll(cfg.Keys.Next, func(k string) error {
		return tmuxctl.Bind(k, fmt.Sprintf("%q next", bin))
	}); err != nil {
		return err
	}
	if err := bindAll(cfg.Keys.Popup, func(k string) error {
		return tmuxctl.BindSidebarPopup(k, fmt.Sprintf("%q sidebar", bin))
	}); err != nil {
		return err
	}
	if err := bindAll(cfg.Keys.Find, func(k string) error {
		return tmuxctl.BindWidePopup(k, fmt.Sprintf("%q find", bin))
	}); err != nil {
		return err
	}
	if err := bindAll(cfg.Keys.All, func(k string) error {
		return tmuxctl.Bind(k, fmt.Sprintf("%q sidebar-all", bin))
	}); err != nil {
		return err
	}
	return bindAll(cfg.Keys.Advisor, func(k string) error {
		return tmuxctl.Bind(k, fmt.Sprintf("%q advisor-popup '#{pane_id}'", bin))
	})
}

func executablePath() (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(bin)
}

// focusCmd clears the "done" flag when the user visits the pane: the
// result has been seen, the agent is simply idle again. Untracked panes
// are a silent no-op.
func focusCmd(paneID string) error {
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	panes, err := store.List()
	if err != nil {
		return nil
	}
	changed := false
	for _, p := range panes {
		if p.Status != state.StatusDone {
			continue
		}
		// visiting a pane acknowledges the agent AND its teammates —
		// their results are shown in that same pane
		if p.PaneID == paneID || p.ParentPane == paneID {
			p.SetStatus(state.StatusWaitingInput, time.Now())
			if store.Save(p) == nil {
				changed = true
			}
		}
	}
	if changed {
		tmuxctl.RefreshStatus()
	}
	return nil
}

func since(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, ok := trimPrefix(p, home); ok {
			return "~" + rel
		}
	}
	return p
}

func trimPrefix(p, prefix string) (string, bool) {
	if p == prefix {
		return "", true
	}
	if len(p) > len(prefix) && p[:len(prefix)] == prefix && p[len(prefix)] == '/' {
		return p[len(prefix):], true
	}
	return p, false
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
