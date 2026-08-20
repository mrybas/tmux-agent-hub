// Package hookd handles agent lifecycle hooks. Claude Code invokes
// "tmux-agent-hub hook" on each event with a JSON payload on stdin; we map the
// event to a pane status and persist it via the state store.
package hookd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/notify"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/tmuxctl"
	"github.com/mrybas/tmux-agent-hub/internal/transcript"
)

// payload is the superset of fields we care about across Claude Code hook
// events. Unknown fields are ignored so format additions don't break us.
type payload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Prompt         string `json:"prompt"`    // UserPromptSubmit
	ToolName       string `json:"tool_name"` // PreToolUse / PermissionRequest
	Message        string `json:"message"`   // Notification
	Model          string `json:"model"`     // Codex sends it on every event

	agent string // which agent CLI invoked us ("claude" when empty)
}

// Handle processes one hook invocation for the given agent kind. It exits
// silently when the pane cannot be determined — hooks must never disturb
// the agent.
func Handle(r io.Reader, agent string) error {
	var pl payload
	if err := json.NewDecoder(r).Decode(&pl); err != nil {
		return nil // tolerate format changes rather than fail the hook
	}
	pl.agent = agent
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	paneID := resolvePane(store, &pl)
	if cfg, err := config.Load(); err == nil && cfg.Debug.HooksLog {
		debugLog(store, &pl, paneID, cfg.Debug.LogMaxKB)
	}
	if paneID == "" {
		return nil
	}
	return apply(store, paneID, &pl, time.Now())
}

// debugLog is the flight recorder for pane resolution (enabled via
// [debug] hooks_log in the config): one line per hook event. Above the
// configured size the log rotates — the current file becomes
// hooks.log.old (one generation kept) and a fresh one starts.
func debugLog(store *state.Store, pl *payload, resolved string, maxKB int) {
	logPath := filepath.Join(filepath.Dir(store.Dir()), "hooks.log")
	if maxKB <= 0 {
		maxKB = 1024
	}
	if st, err := os.Stat(logPath); err == nil && st.Size() > int64(maxKB)*1024 {
		os.Rename(logPath, logPath+".old")
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	sid := pl.SessionID
	if len(sid) > 8 {
		sid = sid[:8]
	}
	if resolved == "" {
		resolved = "DROPPED"
	}
	fmt.Fprintf(f, "%s %-18s agent=%s sid=%s env_pane=%q ppid=%d cwd=%s -> %s\n",
		time.Now().Format("15:04:05"), pl.HookEventName, pl.agent, sid,
		os.Getenv("TMUX_PANE"), os.Getppid(), pl.Cwd, resolved)
}

// resolvePane finds the tmux pane this event belongs to. The simple case
// is $TMUX_PANE, but daemon-spawned sessions (agent teams, --fork-session)
// run detached from the pane's environment — for those we match by the
// session id already tracked, then by the pane's working directory.
func resolvePane(store *state.Store, pl *payload) string {
	alive, aliveErr := tmuxctl.ListPaneIDs()
	paneOK := func(id string) bool {
		if aliveErr != nil {
			return true // cannot verify (tests, no server) — trust it
		}
		return alive[id]
	}
	// daemon processes may carry a stale TMUX_PANE from long ago — trust
	// it only when the pane exists AND works in the event's directory
	// (a stale value can point at a live pane belonging to someone else)
	if pane := os.Getenv("TMUX_PANE"); pane != "" && paneOK(pane) {
		if aliveErr != nil || pl.Cwd == "" || pathsRelated(tmuxctl.PaneCwd(pane), pl.Cwd) {
			return pane
		}
	}
	panes, err := store.List()
	if err != nil {
		return ""
	}
	if pl.SessionID != "" {
		for _, p := range panes {
			if p.SessionID != pl.SessionID {
				continue
			}
			if p.ParentPane != "" && paneOK(p.ParentPane) {
				return p.PaneID // virtual teammate entry
			}
			if paneOK(p.PaneID) {
				// a mapping created by a stale TMUX_PANE can point at a
				// foreign pane — heal only on a DEFINITE mismatch: an empty
				// PaneCwd is a transient tmux failure, not evidence
				paneCwd := tmuxctl.PaneCwd(p.PaneID)
				if aliveErr != nil || p.Cwd == "" || paneCwd == "" || pathsRelated(paneCwd, p.Cwd) {
					return p.PaneID
				}
				store.Delete(p.PaneID) // truly foreign pane — heal and re-resolve
			}
		}
	}
	// the hook runs as a child of the agent: for in-pane sessions the
	// parent's tty IS the pane tty — an exact match, no guessing
	if tty := tmuxctl.ProcessTTY(os.Getppid()); tty != "" {
		if pane := tmuxctl.PaneByTTY(tty); pane != "" && paneOK(pane) {
			return pane
		}
	}
	// daemon-hosted sessions are displayed by a viewer client whose
	// command line carries the session id — find its pane by tty
	if pl.SessionID != "" {
		if tty := tmuxctl.TTYOfProcessWithArg(pl.SessionID); tty != "" {
			if pane := tmuxctl.PaneByTTY(tty); pane != "" && paneOK(pane) {
				return pane
			}
		}
	}
	if pl.Cwd == "" {
		return ""
	}
	// bootstrap: an agent pane in this directory
	infos, err := tmuxctl.PanesFull()
	if err != nil {
		return ""
	}
	agent := pl.agent
	if agent == "" {
		agent = "claude"
	}
	var candidates []string
	for _, info := range infos {
		if !samePath(info.Path, pl.Cwd) {
			continue
		}
		// a pane can show its shell while the agent runs a Bash tool —
		// the process tree is the reliable signal then
		if !looksLikeAgent(info.Command, agent) &&
			!(info.PID > 0 && tmuxctl.PaneHasAgentProcess(info.PID)) {
			continue
		}
		candidates = append(candidates, info.ID)
	}
	var match string
	switch len(candidates) {
	case 0:
		return ""
	case 1:
		match = candidates[0]
	default:
		// several agents share this directory: the pane that DISPLAYS this
		// session contains its latest text — match by screen content
		match = resolveByScreen(pl, candidates)
		if match == "" {
			return "" // still ambiguous — better no data than the wrong pane
		}
	}
	// The pane may already be owned by another session. A FRESH owner
	// means this event is a teammate spawned inside it (nest as a virtual
	// child); a stale owner means the session was forked/replaced (daemon
	// restarts) — take the pane over.
	if owner, err := store.Load(match); err == nil && owner.SessionID != "" &&
		pl.SessionID != "" && owner.SessionID != pl.SessionID {
		if time.Since(owner.UpdatedAt) < 10*time.Minute {
			return virtualID(match, pl.SessionID)
		}
	}
	return match
}

// resolveByScreen finds which candidate pane renders this session by
// searching the panes' visible content for the session's latest text.
// Returns "" unless exactly one pane matches.
func resolveByScreen(pl *payload, candidates []string) string {
	var needles []string
	if pl.TranscriptPath != "" {
		if text, err := transcript.LastReplyText(pl.agent, pl.TranscriptPath); err == nil {
			if n := screenNeedle(text); n != "" {
				needles = append(needles, n)
			}
		}
	}
	if n := screenNeedle(pl.Prompt); n != "" {
		needles = append(needles, n)
	}
	if len(needles) == 0 {
		return ""
	}
	match := ""
	for _, pane := range candidates {
		screen := tmuxctl.CaptureTail(pane, 120)
		if screen == "" {
			continue
		}
		for _, n := range needles {
			if strings.Contains(screen, n) {
				if match != "" && match != pane {
					return "" // both show it — give up
				}
				match = pane
				break
			}
		}
	}
	return match
}

// screenNeedle turns text into a short whitespace-collapsed tail suitable
// for matching against captured pane content.
func screenNeedle(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	r := []rune(collapsed)
	if len(r) < 12 {
		return "" // too short to be distinctive
	}
	if len(r) > 48 {
		r = r[len(r)-48:]
	}
	return string(r)
}

// pathsRelated reports whether two directories belong to the same tree —
// equal, or one inside the other. Agents routinely cd into a subdirectory
// of their pane, so demanding equality would call every such session a
// hijacked pane and wipe its state (it did, on a real run).
func pathsRelated(a, b string) bool {
	if samePath(a, b) {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		ra, rb = a, b
	}
	return strings.HasPrefix(ra, rb+string(filepath.Separator)) ||
		strings.HasPrefix(rb, ra+string(filepath.Separator))
}

// samePath compares directories with symlink normalization (macOS /tmp
// vs /private/tmp).
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// virtualID builds the id for a pane-less teammate session.
func virtualID(parentPane, sessionID string) string {
	s := sessionID
	if len(s) > 8 {
		s = s[:8]
	}
	return parentPane + "~" + s
}

// looksLikeAgent accepts a pane command as a plausible agent TUI. The
// command is often not the agent name (Claude's versioned binaries show up
// as e.g. "2.1.234"), so anything that is not a plain shell qualifies.
func looksLikeAgent(command, agent string) bool {
	if strings.Contains(command, agent) {
		return true
	}
	switch command {
	case "zsh", "bash", "fish", "sh", "-zsh", "-bash", "tmux", "ssh":
		return false
	}
	return true
}

func apply(store *state.Store, paneID string, pl *payload, now time.Time) error {
	if pl.HookEventName == "SessionEnd" {
		if err := store.Delete(paneID); err != nil {
			return err
		}
		// the process may outlive the session — an agent asked to exit sits
		// at its confirmation prompt — so remember that this pane's session
		// is over and nothing should adopt what is still running there
		store.MarkEnded(paneID)
		tmuxctl.RefreshStatus()
		return nil
	}

	cfg, _ := config.Load()  // broken config → defaults, never break a hook
	store.ClearEnded(paneID) // an event other than SessionEnd: a session lives here
	if _, known := store.Load(paneID); known != nil && !paneRunsAgent(paneID, pl.agent) {
		// A pane we do not track, with no agent in it: a late event from a
		// session whose pane is gone, resolved onto some shell that happens
		// to sit in the same directory. Tracking it would put an agent in
		// the sidebar where the user can plainly see there is none.
		return nil
	}
	p := store.LoadOrNew(paneID)
	prevDisplay := p.Display()
	if parent, _, ok := strings.Cut(paneID, "~"); ok && parent != paneID {
		p.ParentPane = parent
	}
	if pl.agent != "" {
		p.Agent = pl.agent
	} else if p.Agent == "" {
		p.Agent = "claude"
	}
	if pl.Cwd != "" {
		p.Cwd = pl.Cwd
	}
	if p.Cwd == "" {
		// Codex payloads may lack cwd — fall back to the pane's directory
		p.Cwd = tmuxctl.PaneCwd(paneID)
	}
	if pl.SessionID != "" {
		p.SessionID = pl.SessionID
	}
	if pl.TranscriptPath != "" {
		p.TranscriptPath = pl.TranscriptPath
	}
	if pl.Model != "" {
		p.Model = pl.Model
	}

	switch pl.HookEventName {
	case "SessionStart":
		p.AdviceStreak = 0 // a fresh session owes the user nothing
		p.SetStatus(state.StatusWaitingInput, now)
	case "UserPromptSubmit":
		// A prompt typed while the agent is working is absorbed into the
		// running turn: no Stop fires, so the conclusion the agent had just
		// reached would sit unread until some later tool round. The prompt
		// itself is the boundary — whatever the agent said is final now.
		if p.Status == state.StatusWorking && isUserPrompt(pl.Prompt) {
			liveReview(store, p, cfg, true, now)
		}
		switch {
		case strings.HasPrefix(pl.Prompt, ReqMarker):
			// Our injected review request: keep AwaitingReviewFor and do not
			// overwrite the reviewer's own last prompt with boilerplate. The
			// turn it starts is the review itself and must never be fed to
			// ITS reviewer — that is what makes mutual pairs safe. Requests
			// are only sent to an idle reviewer (see liveReview), but if one
			// still lands mid-turn, that turn is the agent's own work and is
			// not hidden; the request text is stripped from the delta.
			if p.Status != state.StatusWorking {
				p.SkipNextReview = true
			}
		case strings.HasPrefix(pl.Prompt, AdvMarker):
			// Injected advice does NOT hide the turn it belongs to. What the
			// agent replies is exactly what its reviewer needs to read —
			// whether the advice started the turn or arrived inside one that
			// was already running. The advice text itself is stripped from
			// the delta, and a runaway advisor↔worker exchange is bounded by
			// AdviceStreak instead of by hiding the agent's work.
		default:
			// a real user prompt cancels any pending review correlation and
			// starts the agent fresh — old stuck findings no longer apply
			p.ClearAwaiting()
			p.ClearStuck()
			// internal traffic (<task-notification> etc.) is neither a label
			// nor a word from the user, so it does not unmute the advisor
			if isUserPrompt(pl.Prompt) {
				p.LastPrompt = truncate(pl.Prompt, 160)
				p.AdviceStreak = 0 // the user is back in the loop
			}
		}
		p.CurrentTool = ""
		p.SetStatus(state.StatusWorking, now)
	case "PreToolUse":
		p.CurrentTool = pl.ToolName
		p.SetStatus(state.StatusWorking, now)
	case "PostToolUse":
		// a tool round just closed: the finest granularity at which the
		// detectors and the reviewer can look at work in progress
		p.SetStatus(state.StatusWorking, now)
		detectStuck(p, cfg, now)
		liveReview(store, p, cfg, false, now)
	case "Notification":
		if strings.Contains(strings.ToLower(pl.Message), "permission") {
			p.SetStatus(state.StatusWaitingPermission, now)
		} else {
			p.SetStatus(state.StatusWaitingInput, now)
		}
	case "PermissionRequest": // Codex has a dedicated event for this
		if pl.ToolName != "" {
			p.CurrentTool = pl.ToolName
		}
		p.SetStatus(state.StatusWaitingPermission, now)
	case "Stop":
		p.CurrentTool = ""
		p.SetStatus(state.StatusDone, now)
		// the reply is complete — a good moment to learn the model and name
		if p.TranscriptPath != "" {
			tr := transcript.For(p.Agent, p.TranscriptPath)
			if _, model, err := tr.LastReply(); err == nil && model != "" {
				p.Model = model
			}
			if title := tr.AgentTitle(); title != "" {
				p.AgentTitle = title
			}
		}
		// live-review duties: as a worker — feed the final delta to the
		// reviewer; as a reviewer — grade and deliver its own verdict
		liveReview(store, p, cfg, true, now)
		forwardAdvice(store, p, cfg, now)
	default:
		return nil // SubagentStop, PreCompact, future events — not tracked
	}

	// notification effects on meaningful transitions, debounced per agent
	if p.Display() != prevDisplay {
		event := ""
		switch p.Display() {
		case state.StatusDone:
			event = "done"
		case state.StatusWaitingPermission:
			event = "permission"
		case state.StatusStuck:
			event = "stuck"
		}
		if event != "" {
			if cfg.Notify.Debounce <= 0 || now.Sub(p.NotifiedAt) >= time.Duration(cfg.Notify.Debounce)*time.Second {
				p.NotifiedAt = now
				notify.Fire(cfg, p, event)
			}
		}
	}

	// a verdict we could not read last time is re-read at the next event of
	// any kind — by then the reviewer's words are certainly on disk
	if p.VerdictOwed && p.AwaitingReviewFor != "" && pl.HookEventName != "Stop" {
		forwardAdvice(store, p, cfg, now)
	}

	if err := store.Save(p); err != nil {
		return err
	}
	tmuxctl.RefreshStatus()

	// Stall the next tool call while the reviewer forms a verdict on a
	// held note. Done after the save so the reviewer's concurrent write
	// (which lands during the wait) is not overwritten by ours.
	if pl.HookEventName == "PreToolUse" {
		catchUp(store, p, cfg)
	}
	return nil
}

// ReconcileInterrupted fixes panes stuck in "working": after a user
// interrupt (Ctrl+C/Esc — Claude Code fires no hook then, but writes an
// interrupt marker), or when the transcript has been silent for so long
// that the turn clearly ended (advisor.stale_after — a real turn appends
// entries every few seconds, so a long silence means the turn died
// without a Stop hook: an error, a daemon session, a killed process).
// Called from the periodic renderers (statusline, sidebar). Returns true
// when anything changed.
func ReconcileInterrupted(store *state.Store, panes []*state.Pane) bool {
	cfg, _ := config.Load()
	staleWorkingAfter := cfg.Advisor.StaleWorkingAfter()
	changed := false
	for _, p := range panes {
		if p.Status != state.StatusWorking || p.TranscriptPath == "" {
			continue
		}
		stale := false
		if st, err := os.Stat(p.TranscriptPath); err == nil &&
			time.Since(st.ModTime()) > staleWorkingAfter {
			stale = true
		}
		if stale || transcript.For(p.Agent, p.TranscriptPath).Interrupted() {
			p.CurrentTool = ""
			// a turn that died answers no review request: free the reviewer
			// now instead of waiting for the lost-verdict timeout
			p.ClearAwaiting()
			p.SetStatus(state.StatusWaitingInput, time.Now())
			if store.Save(p) == nil {
				changed = true
			}
		}
	}
	return changed
}

// isUserPrompt reports whether a prompt is a human talking, as opposed to
// our own injections or the harness's internal traffic ("<task-notification>").
func isUserPrompt(prompt string) bool {
	prompt = strings.TrimSpace(prompt)
	return prompt != "" && !strings.HasPrefix(prompt, "<") &&
		!strings.HasPrefix(prompt, ReqMarker) && !strings.HasPrefix(prompt, AdvMarker)
}

// agentPaneRe matches pane commands that are an agent's own TUI: Claude's
// versioned binaries ("2.1.234"), or the agent names themselves.
var agentPaneRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)

// AgentKindInPane names the agent a pane command belongs to, or "" when
// the pane is running something else.
func AgentKindInPane(command string) string {
	switch {
	case agentPaneRe.MatchString(command), strings.HasPrefix(command, "claude"):
		return "claude"
	case strings.HasPrefix(command, "codex"):
		return "codex"
	}
	return ""
}

// TrackPane starts tracking a pane because the user pointed at it — to
// assign it as a reviewer, say. That is better evidence than any of our
// heuristics, so it also clears a tombstone: the user can see what is in
// the pane. What we cannot know yet (the session, its transcript) arrives
// with the agent's next hook event.
func TrackPane(store *state.Store, paneID string, now time.Time) error {
	if _, err := store.Load(paneID); err == nil {
		return nil // already tracked
	}
	info, ok := tmuxctl.PaneInfoFor(paneID)
	if !ok {
		return fmt.Errorf("%s is not a pane", paneID)
	}
	agent := AgentKindInPane(info.Command)
	if agent == "" {
		return fmt.Errorf("%s is not running an agent", paneID)
	}
	store.ClearEnded(paneID)
	p := &state.Pane{PaneID: paneID, Agent: agent, Cwd: info.Path}
	p.SetStatus(state.StatusWaitingInput, now)
	return store.Save(p)
}

// paneRunsAgent reports whether an agent is actually running in a pane.
// The pane's own command is the cheap answer; the process tree is the
// reliable one, because a pane shows its shell while the agent runs a
// Bash tool. When tmux cannot say, the answer is yes: a hook must never
// be dropped on a guess.
func paneRunsAgent(paneID, agent string) bool {
	if parent, _, ok := strings.Cut(paneID, "~"); ok {
		paneID = parent // teammates live in their parent's pane
	}
	info, ok := tmuxctl.PaneInfoFor(paneID)
	if !ok {
		return true
	}
	if looksLikeAgent(info.Command, agent) {
		return true
	}
	return tmuxctl.PaneHasAgentProcess(info.PID)
}

// ReconcileDeparted drops agents whose pane outlived them. A pane that is
// a shell again means the agent exited without a SessionEnd hook — killed,
// crashed, or a CLI that never fires one — and the entry would otherwise
// sit in the sidebar for ever, claiming an agent that is not there.
// Called from the renderers, like ReconcileInterrupted.
func ReconcileDeparted(store *state.Store, panes []*state.Pane) bool {
	infos, err := tmuxctl.PanesFull()
	if err != nil {
		return false
	}
	cfg, _ := config.Load()
	return reconcileDeparted(store, panes, infos, nil, cfg.Advisor.GoneCheckAfter(), time.Now())
}

// reconcileDeparted is the testable half: tree is read lazily, and only
// when some pane actually looks like it lost its agent, because a process
// snapshot is expensive and this runs on every render.
func reconcileDeparted(store *state.Store, panes []*state.Pane,
	infos []tmuxctl.PaneInfo, hasAgent func(pid int) bool,
	quiet time.Duration, now time.Time) bool {
	byID := make(map[string]tmuxctl.PaneInfo, len(infos))
	for _, info := range infos {
		byID[info.ID] = info
	}
	var suspects []*state.Pane
	for _, p := range panes {
		if p.ParentPane != "" {
			continue // teammates live and die with their parent
		}
		info, ok := byID[p.PaneID]
		if !ok {
			continue // the pane itself is gone: someone else's cleanup
		}
		if looksLikeAgent(info.Command, p.Agent) {
			continue // still the agent's own TUI
		}
		if now.Sub(p.UpdatedAt) < quiet {
			// a live agent writes state on every tool round; a pane showing
			// a shell right after one is an agent running a Bash tool, not a
			// departed one. Only the quiet ones are worth a process scan.
			continue
		}
		suspects = append(suspects, p)
	}
	if len(suspects) == 0 {
		return false
	}
	if hasAgent == nil {
		tree, err := tmuxctl.Snapshot()
		if err != nil {
			return false // cannot tell — leave everything alone
		}
		hasAgent = tree.HasAgent
	}

	changed := false
	for _, p := range suspects {
		// a shell in the pane is normal while the agent runs a Bash tool;
		// only an empty process tree means the agent is really gone
		if hasAgent(byID[p.PaneID].PID) {
			continue
		}
		// Claude Code can host a session in a daemon, where the agent is
		// not under the pane at all. Its transcript is then the only proof
		// of life — and a fresh one outranks an empty process tree.
		if p.TranscriptPath != "" {
			if st, err := os.Stat(p.TranscriptPath); err == nil && now.Sub(st.ModTime()) < quiet {
				continue
			}
		}
		if store.Delete(p.PaneID) != nil {
			continue
		}
		changed = true
		for _, other := range panes {
			if other.ParentPane == p.PaneID {
				store.Delete(other.PaneID)
			}
		}
	}
	return changed
}

func truncate(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ") // collapse newlines/whitespace
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
