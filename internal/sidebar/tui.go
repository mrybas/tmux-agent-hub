package sidebar

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"
	"github.com/mattn/go-runewidth"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/hookd"
	"github.com/mrybas/tmux-agent-hub/internal/layout"
	"github.com/mrybas/tmux-agent-hub/internal/skills"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/textutil"
	"github.com/mrybas/tmux-agent-hub/internal/tmuxctl"
	"github.com/mrybas/tmux-agent-hub/internal/transcript"
)

type mode int

const (
	modeNormal mode = iota
	modeFilter
	modeRename
	modeConfirmKill
	modeHelp
	modeAssign         // picking a reviewer for the highlighted agent
	modeInspect        // skills/config inspector for the highlighted agent
	modeActivity       // tool-call timeline for the highlighted agent
	modeActivityDetail // one tool call: arguments and result
)

type refreshMsg struct{}

// editorClosedMsg arrives when the editor popup exits.
type editorClosedMsg struct{ err error }

// advisorClosedMsg arrives when the advisor popup (s key) exits.
type advisorClosedMsg struct{ err error }
type tickMsg time.Time

type model struct {
	cfg    config.Config
	store  *state.Store
	notify chan struct{}
	paneID string // the sidebar's own tmux pane

	spec        config.ViewSpec
	viewIdx     int // index into config.ViewPresetNames (v key cycling)
	rows        []Row
	cursor      int // index into rows; always on a non-header row when possible
	offset      int // first visible rendered line
	filter      string
	input       string // text being typed in filter/rename mode
	popup       bool   // running inside a tmux popup (prefix+o)
	prefixKey   string // tmux prefix in bubbletea notation
	prefixArmed bool   // prefix pressed, waiting for the follow-up key
	mode        mode
	status      string // transient footer message (errors etc.)
	glyphs      map[state.Status]string
	counts      map[state.Status]int
	byID        map[string]*state.Pane
	reviews     map[string][]string // reviewer pane -> workers it reviews

	assignFor    *state.Pane // agent a reviewer is being picked for
	assignOpts   []Row       // candidate reviewers
	assignCursor int

	inspectRows   []skills.Line // skills inspector content
	inspectCursor int           // index of the selected editable row
	inspectOffset int

	activityCalls  []transcript.ToolCall // tool timeline for modeActivity
	activityLabel  string
	activityCursor int // selected call
	activityOffset int // first visible call

	detailTitle  string // one call blown up: modeActivityDetail
	detailLines  []string
	detailOffset int
	syncWarn     bool // synchronize-panes is on in the sidebar's window

	width, height int

	glyphStyles map[state.Status]lipgloss.Style
	accentStyle lipgloss.Style
	headerStyle lipgloss.Style
	selStyle    lipgloss.Style
	dimStyle    lipgloss.Style
}

// vline is one rendered terminal line; row maps it back to the logical
// list entry (-1 for decorative lines).
type vline struct {
	text string
	row  int
}

// Run starts the sidebar TUI in the current pane.
func Run(cfg config.Config, store *state.Store) error {
	tmuxctl.RecordSocket() // the sidebar certainly runs on the user's server
	paneID := os.Getenv("TMUX_PANE")
	if paneID != "" {
		tmuxctl.MarkSidebarPane(paneID)
	}
	m := &model{
		cfg:         cfg,
		paneID:      paneID,
		store:       store,
		notify:      make(chan struct{}, 1),
		spec:        cfg.SidebarView(),
		glyphs:      cfg.Statusline.StatusGlyphs.ToMap(),
		accentStyle: lipgloss.NewStyle().Foreground(lipgloss.Color(cfg.Theme.Accent)),
		headerStyle: lipgloss.NewStyle().Foreground(lipgloss.Color(cfg.Theme.Accent)).Bold(true),
		selStyle:    lipgloss.NewStyle().Reverse(true),
		dimStyle:    lipgloss.NewStyle().Foreground(lipgloss.Color(cfg.Theme.Dim)),
		glyphStyles: map[state.Status]lipgloss.Style{},
	}
	for i, name := range config.ViewPresetNames {
		if name == cfg.Sidebar.View {
			m.viewIdx = i
		}
	}
	for st, hex := range cfg.Colors.ToMap() {
		m.glyphStyles[st] = lipgloss.NewStyle().Foreground(lipgloss.Color(hex))
	}
	m.reload()

	watcher, err := fsnotify.NewWatcher()
	if err == nil {
		os.MkdirAll(store.Dir(), 0o755)
		if watcher.Add(store.Dir()) == nil {
			go func() {
				for range watcher.Events {
					select {
					case m.notify <- struct{}{}:
					default:
					}
				}
			}()
			defer watcher.Close()
		}
	}

	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.waitNotify(), tick())
}

func (m *model) waitNotify() tea.Cmd {
	return func() tea.Msg {
		<-m.notify
		return refreshMsg{}
	}
}

func tick() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) reload() {
	panes, err := m.store.List()
	if err != nil {
		m.status = err.Error()
		return
	}
	locs, err := tmuxctl.PaneLocations()
	if err == nil {
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
		panes = kept
	}
	// agent panes without any hook events yet (pre-install sessions)
	tracked := map[string]bool{}
	for _, p := range panes {
		tracked[p.PaneID] = true
	}
	if infos, err := tmuxctl.PanesFull(); err == nil {
		if extra := UntrackedAgents(infos, tracked, m.paneID); len(extra) > 0 {
			panes = append(panes, extra...)
			sort.Slice(panes, func(i, j int) bool {
				if panes[i].Cwd != panes[j].Cwd {
					return panes[i].Cwd < panes[j].Cwd
				}
				return panes[i].PaneID < panes[j].PaneID
			})
		}
	}
	m.syncWarn = tmuxctl.PaneSyncOn(m.paneID)
	if hookd.ReconcileInterrupted(m.store, panes) {
		tmuxctl.RefreshStatus()
	}
	if hookd.ReconcileDeparted(m.store, panes) {
		// an agent exited but left its pane behind: re-read without it
		if fresh, err := m.store.List(); err == nil {
			kept := fresh[:0]
			for _, p := range fresh {
				if _, alive := locs[p.PaneID]; alive || p.ParentPane != "" {
					kept = append(kept, p)
				}
			}
			panes = kept
		}
		tmuxctl.RefreshStatus()
	}
	m.counts = map[state.Status]int{}
	m.byID = map[string]*state.Pane{}
	m.reviews = map[string][]string{}
	for _, p := range panes {
		m.counts[p.Display()]++
		m.byID[p.PaneID] = p
	}
	for _, p := range panes {
		if p.ReviewerPane != "" {
			if _, alive := m.byID[p.ReviewerPane]; alive {
				// a reviewer may serve several workers — keep them all
				m.reviews[p.ReviewerPane] = append(m.reviews[p.ReviewerPane], p.PaneID)
			}
		}
	}
	var selected string
	if r := m.current(); r != nil {
		selected = r.Pane.PaneID
	}
	m.rows = BuildRows(panes, locs, m.filter, m.spec, locs[m.paneID].Session)
	m.cursor = -1
	for i, r := range m.rows {
		if !r.Header && (m.cursor == -1 || r.Pane.PaneID == selected) {
			m.cursor = i
			if r.Pane.PaneID == selected {
				break
			}
		}
	}
	m.publishSelection()
}

func (m *model) current() *Row {
	if m.cursor >= 0 && m.cursor < len(m.rows) && !m.rows[m.cursor].Header {
		return &m.rows[m.cursor]
	}
	return nil
}

func (m *model) move(dir int) {
	for i := m.cursor + dir; i >= 0 && i < len(m.rows); i += dir {
		if !m.rows[i].Header {
			m.cursor = i
			m.publishSelection()
			return
		}
	}
}

// publishSelection exposes the highlighted agent via a pane option, so
// prefix+<advisor key> pressed on the sidebar picks it as the source.
func (m *model) publishSelection() {
	if m.paneID == "" {
		return
	}
	if r := m.current(); r != nil {
		tmuxctl.PublishSelection(m.paneID, r.Pane.PaneID)
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case refreshMsg:
		m.reload()
		return m, m.waitNotify()
	case tickMsg:
		m.reload()
		return m, tick()
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// keys can leak in while the pane is not focused (repeat-mode pane
	// hops, stray send-keys) — react only when the sidebar is really
	// the focused pane
	if m.paneID != "" && !tmuxctl.PaneIsFocused(m.paneID) {
		return m, nil
	}
	m.status = ""
	switch m.mode {
	case modeFilter, modeRename:
		switch msg.Type {
		case tea.KeyEscape:
			m.mode = modeNormal
			m.input = ""
		case tea.KeyEnter:
			if m.mode == modeFilter {
				m.filter = m.input
			} else if r := m.current(); r != nil {
				m.rename(r.Pane.PaneID, m.input)
			}
			m.mode = modeNormal
			m.input = ""
			m.reload()
		case tea.KeyBackspace:
			if len(m.input) > 0 {
				rs := []rune(m.input)
				m.input = string(rs[:len(rs)-1])
			}
		case tea.KeyRunes, tea.KeySpace:
			// KeySpace already carries the space in Runes — appending
			// another one would double every space typed
			if len(msg.Runes) > 0 {
				m.input += string(msg.Runes)
			} else if msg.Type == tea.KeySpace {
				m.input += " "
			}
		}
		return m, nil

	case modeHelp:
		switch layout.Normalize(msg.String()) {
		case "q", "esc", "?", "ctrl+c":
			m.mode = modeNormal
		}
		return m, nil

	case modeActivityDetail:
		switch layout.Normalize(msg.String()) {
		case "q", "esc", "ctrl+c":
			m.mode = modeActivity // back to the timeline
		case "j", "down":
			if m.detailOffset < len(m.detailLines)-1 {
				m.detailOffset++
			}
		case "k", "up":
			if m.detailOffset > 0 {
				m.detailOffset--
			}
		case "g":
			m.detailOffset = 0
		case "G":
			m.detailOffset = max(len(m.detailLines)-1, 0)
		case "ctrl+d":
			m.detailOffset = min(m.detailOffset+10, max(len(m.detailLines)-1, 0))
		case "ctrl+u":
			m.detailOffset = max(m.detailOffset-10, 0)
		}
		return m, nil

	case modeActivity:
		switch layout.Normalize(msg.String()) {
		case "enter":
			if m.activityCursor >= 0 && m.activityCursor < len(m.activityCalls) {
				m.openCallDetail(m.activityCalls[m.activityCursor])
			}
		case "q", "esc", "t", "ctrl+c":
			m.mode = modeNormal
		case "j", "down":
			m.activityCursor = min(m.activityCursor+1, max(len(m.activityCalls)-1, 0))
		case "k", "up":
			m.activityCursor = max(m.activityCursor-1, 0)
		case "g":
			m.activityCursor = 0
		case "G":
			m.activityCursor = max(len(m.activityCalls)-1, 0)
		case "ctrl+d":
			m.activityCursor = min(m.activityCursor+10, max(len(m.activityCalls)-1, 0))
		case "ctrl+u":
			m.activityCursor = max(m.activityCursor-10, 0)
		}
		return m, nil

	case modeInspect:
		switch layout.Normalize(msg.String()) {
		case "q", "esc", "i", "ctrl+c":
			m.mode = modeNormal
		case "j", "down":
			m.inspectMove(1)
		case "k", "up":
			m.inspectMove(-1)
		case "g":
			m.inspectCursor = -1
			m.inspectMove(1)
		case "enter":
			if m.inspectCursor >= 0 && m.inspectCursor < len(m.inspectRows) {
				if path := m.inspectRows[m.inspectCursor].Path; path != "" {
					if os.Getenv("TMUX_AGENT_HUB_POPUP") != "" {
						// popup mode: tmux allows one popup per client —
						// close ourselves and chain the editor popup
						if bin, err := os.Executable(); err == nil {
							tmuxctl.RunShellDetached(fmt.Sprintf("sleep 0.3; %q edit-popup %q", bin, path))
							return m, tea.Quit
						}
					}
					editor := m.cfg.EditorCommand()
					return m, func() tea.Msg {
						err := tmuxctl.OpenPopupLarge(fmt.Sprintf("%s %q", editor, path))
						return editorClosedMsg{err}
					}
				}
			}
		}
		return m, nil

	case modeAssign:
		switch layout.Normalize(msg.String()) {
		case "q", "esc", "ctrl+c":
			m.mode = modeNormal
		case "j", "down":
			if m.assignCursor < len(m.assignOpts) {
				m.assignCursor++
			}
		case "k", "up":
			if m.assignCursor > 0 {
				m.assignCursor--
			}
		case "enter":
			m.applyAssign()
			m.mode = modeNormal
			m.reload()
		}
		return m, nil

	case modeConfirmKill:
		if layout.Normalize(msg.String()) == "y" {
			if r := m.current(); r != nil {
				if r.Pane.ParentPane != "" {
					// a teammate has no pane — just forget its entry
					m.store.Delete(r.Pane.PaneID)
				} else if err := tmuxctl.KillPane(r.Pane.PaneID); err != nil {
					m.status = err.Error()
				} else {
					m.store.Delete(r.Pane.PaneID)
				}
				tmuxctl.RefreshStatus()
				m.reload()
			}
		}
		m.mode = modeNormal
		return m, nil
	}

	switch layout.Normalize(msg.String()) {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "j", "down":
		m.move(1)
	case "k", "up":
		m.move(-1)
	case "g":
		m.cursor = -1
		m.move(1)
	case "G":
		m.cursor = len(m.rows)
		m.move(-1)
	case "v":
		m.viewIdx = (m.viewIdx + 1) % len(config.ViewPresetNames)
		// user's config knobs (e.g. group = "session") survive cycling
		m.spec = m.cfg.ApplyKnobs(config.ViewPreset(config.ViewPresetNames[m.viewIdx]))
		m.reload()
	case "enter":
		if r := m.current(); r != nil {
			target := r.Pane.PaneID
			if r.Pane.ParentPane != "" {
				target = r.Pane.ParentPane // teammates have no pane of their own
			}
			if err := tmuxctl.JumpTo(target); err != nil {
				m.status = err.Error()
			} else if os.Getenv("TMUX_AGENT_HUB_POPUP") != "" {
				return m, tea.Quit // popup mode: jumping closes the popup
			}
		}
	case "x":
		if m.current() != nil {
			m.mode = modeConfirmKill
		}
	case "r":
		if r := m.current(); r != nil {
			m.mode = modeRename
			m.input = r.Pane.Title
		}
	case "/":
		m.mode = modeFilter
		m.input = m.filter
	case "esc":
		if m.filter != "" {
			m.filter = ""
			m.reload()
		}
	case "R":
		m.reload()
	case "a":
		if r := m.current(); r != nil {
			m.assignFor = r.Pane
			m.assignOpts = m.assignOpts[:0]
			for _, row := range m.rows {
				if !row.Header && row.Pane.PaneID != r.Pane.PaneID && row.Pane.ParentPane == "" {
					m.assignOpts = append(m.assignOpts, row)
				}
			}
			m.assignCursor = 0
			m.mode = modeAssign
		}
	case "i":
		if r := m.current(); r != nil {
			home, err := os.UserHomeDir()
			if err == nil {
				m.inspectRows = skills.Inspect(home, r.Pane.Cwd, r.Pane.Agent).Rows(home)
				m.inspectOffset = 0
				m.inspectCursor = -1
				m.inspectMove(1) // land on the first editable row
				m.mode = modeInspect
			}
		}
	case "s":
		// advisor with the highlighted agent as the source; the focus
		// guard above makes accidental triggers a non-issue now
		if r := m.current(); r != nil {
			pane := r.Pane.PaneID
			bin, err := os.Executable()
			if err != nil {
				m.status = err.Error()
				break
			}
			if os.Getenv("TMUX_AGENT_HUB_POPUP") != "" {
				// popup mode: tmux allows one popup per client — close
				// ourselves and chain the advisor popup
				tmuxctl.RunShellDetached(fmt.Sprintf("sleep 0.3; %q advisor-popup '%s'", bin, pane))
				return m, tea.Quit
			}
			return m, func() tea.Msg {
				return advisorClosedMsg{tmuxctl.OpenPopup(fmt.Sprintf("%q advisor '%s'", bin, pane), 72, 20)}
			}
		}
	case "t":
		// tool-call timeline (activity) for the highlighted agent
		if r := m.current(); r != nil {
			if r.Pane.TranscriptPath == "" {
				m.status = "no transcript for this agent yet"
				break
			}
			calls, err := transcript.For(r.Pane.Agent, r.Pane.TranscriptPath).ToolCalls(200)
			if err != nil || len(calls) == 0 {
				m.status = "no tool activity recorded"
				break
			}
			m.activityCalls = calls
			m.activityLabel = Label(r.Pane)
			m.activityCursor = max(len(calls)-1, 0) // start at the newest
			m.activityOffset = 0
			m.mode = modeActivity
		}
	case "?":
		m.mode = modeHelp
	}
	return m, nil
}

// applyAssign persists the reviewer choice; index 0 is "unassign".
func (m *model) applyAssign() {
	p, err := m.store.Load(m.assignFor.PaneID)
	if err != nil {
		m.status = err.Error()
		return
	}
	// linking and unlinking touch both agents' state — the store owns that
	if m.assignCursor == 0 {
		err = m.store.UnlinkReviewer(p.PaneID)
	} else {
		err = m.store.LinkReviewer(p.PaneID, m.assignOpts[m.assignCursor-1].Pane.PaneID)
	}
	if err != nil {
		m.status = err.Error()
	}
}

func (m *model) rename(paneID, title string) {
	p, err := m.store.Load(paneID)
	if err != nil {
		m.status = err.Error()
		return
	}
	p.Title = strings.TrimSpace(title)
	if err := m.store.Save(p); err != nil {
		m.status = err.Error()
	}
}

// --- rendering --------------------------------------------------------------

func (m *model) View() string {
	if m.mode == modeHelp {
		return m.helpView()
	}
	if m.mode == modeAssign {
		return m.assignView()
	}
	if m.mode == modeInspect {
		return m.inspectView()
	}
	if m.mode == modeActivity {
		return m.activityView()
	}
	if m.mode == modeActivityDetail {
		return m.activityDetailView()
	}
	var b strings.Builder
	b.WriteString(m.summaryLine())
	b.WriteString("\n")

	lines := m.buildLines()
	listHeight := m.height - 3
	if listHeight < 1 {
		listHeight = 1
	}
	maxOffset := len(lines) - listHeight
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.offset > maxOffset {
		m.offset = maxOffset
	}
	// keep every line of the selected row visible
	first, last := -1, -1
	for i, l := range lines {
		if l.row == m.cursor {
			if first == -1 {
				first = i
			}
			last = i
		}
	}
	if first >= 0 {
		if first < m.offset {
			m.offset = first
		}
		if last >= m.offset+listHeight {
			m.offset = last - listHeight + 1
		}
	}

	if len(lines) == 0 {
		b.WriteString("\n" + m.dimStyle.Render("  no agents"))
	}
	end := min(m.offset+listHeight, len(lines))
	for i := m.offset; i < end; i++ {
		b.WriteString(lines[i].text)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

// summaryLine: per-status counts (urgency order) + current view name.
func (m *model) summaryLine() string {
	var parts []string
	plain := 0
	for _, st := range []state.Status{
		state.StatusWaitingPermission, state.StatusStuck, state.StatusDone,
		state.StatusWorking, state.StatusWaitingInput, state.StatusDead,
	} {
		if n := m.counts[st]; n > 0 {
			seg := fmt.Sprintf("%s%d", m.glyphs[st], n)
			parts = append(parts, m.glyphStyles[st].Render(seg))
			plain += runewidth.StringWidth(seg) + 1
		}
	}
	left := " " + strings.Join(parts, " ")
	view := config.ViewPresetNames[m.viewIdx]
	pad := m.width - plain - runewidth.StringWidth(view) - 2
	if pad < 1 {
		return left
	}
	return left + strings.Repeat(" ", pad) + m.dimStyle.Render(view)
}

func (m *model) buildLines() []vline {
	var lines []vline
	for i, r := range m.rows {
		if r.Header {
			if len(lines) > 0 {
				lines = append(lines, vline{"", -1})
			}
			lines = append(lines, vline{m.renderHeader(r), i})
			continue
		}
		if m.spec.Group == "none" && m.spec.Row == "rich" && len(lines) > 0 {
			lines = append(lines, vline{"", -1})
		}
		lines = append(lines, m.renderAgent(r, i)...)
	}
	return lines
}

func (m *model) renderHeader(r Row) string {
	base, parent := SplitFolder(r.Folder)
	icon := m.cfg.Theme.Icons.Folder
	if icon != "" {
		icon += " "
	}
	text := " " + icon + base
	line := m.headerStyle.Render(truncate(text, m.width-1))
	rest := m.width - runewidth.StringWidth(text) - 2
	if parent != "" && rest > 4 {
		line += m.dimStyle.Render(" " + runewidth.Truncate(parent, rest, "…"))
	}
	return line
}

func (m *model) renderAgent(r Row, idx int) []vline {
	p := r.Pane
	selected := idx == m.cursor
	glyph := m.glyphs[p.Display()]
	if glyph == "" {
		glyph = "●"
	}
	prefix := "  "
	if m.spec.Guides && m.spec.Group != "none" && r.Depth == 0 {
		g := m.cfg.Theme.Icons.GuideMid
		if r.Last {
			g = m.cfg.Theme.Icons.GuideLast
		}
		prefix = m.dimStyle.Render(g) + " "
	}
	if selected && m.cfg.Theme.Selection != "reverse" {
		prefix = m.accentStyle.Render("▌") + " "
	}
	if r.Depth > 0 { // teammate: nest under the parent with its own guide
		g := m.cfg.Theme.Icons.GuideMid
		if r.Last {
			g = m.cfg.Theme.Icons.GuideLast
		}
		prefix += "  " + m.dimStyle.Render(g) + " "
	}

	var lines []vline
	if m.spec.Row == "rich" {
		lines = m.renderRich(r, prefix, glyph, selected)
	} else {
		lines = []vline{{m.renderCompact(r, prefix, glyph, selected), idx}}
	}
	return append(lines, m.linkLines(p, idx)...)
}

// linkLines makes reviewer pairs explicit in every view: the primary
// shows who reviews it, the reviewer shows whom it reviews.
func (m *model) linkLines(p *state.Pane, idx int) []vline {
	var lines []vline
	link := func(label string, panes ...string) {
		var names []string
		for _, pane := range panes {
			if other, ok := m.byID[pane]; ok {
				base, _ := SplitFolder(ShortPath(other.Cwd))
				names = append(names, base+" ("+other.Agent+")")
			}
		}
		if len(names) == 0 {
			return
		}
		// one reviewer can serve several workers — name the first and
		// count the rest, so the row stays one line
		text := names[0]
		if len(names) > 1 {
			text += fmt.Sprintf(" +%d", len(names)-1)
		}
		styled := m.accentStyle.Render("⇄") + m.dimStyle.Render(
			" "+label+": "+truncate(text, max(m.width-16, 8)))
		lines = append(lines, vline{"    " + styled, idx})
	}
	if p.ReviewerPane != "" {
		link("reviewer", p.ReviewerPane)
	}
	if workers := m.reviews[p.PaneID]; len(workers) > 0 {
		link("reviewing", workers...)
	}
	return lines
}

// renderCompact: `▌✳ label………………  7m` — one line, time right-aligned.
func (m *model) renderCompact(r Row, prefix, glyph string, selected bool) string {
	p := r.Pane
	indent := 4 * r.Depth
	dur := since(p.StatusSince)
	if p.Status == state.StatusWorking && p.CurrentTool != "" {
		dur = ShortTool(p.CurrentTool) + " " + dur
	}
	who := p.Agent
	if md := ShortModel(p.Model); md != "" {
		who = md // the model is more informative than the agent name
	}
	label := Label(p)
	if p.StuckReason != "" {
		label = "stuck: " + p.StuckReason
	}
	if label == p.Agent || label == who {
		label = who
	} else {
		label = who + " · " + label
	}
	if (m.spec.Group != "folder" || r.Inbox) && r.Depth == 0 {
		base, _ := SplitFolder(r.Folder)
		label = base + " · " + label
	}
	avail := m.width - 2 - 2 - indent - runewidth.StringWidth(dur) - 2
	label = runewidth.Truncate(label, max(avail, 4), "…")
	pad := m.width - 2 - 2 - indent - runewidth.StringWidth(label) - runewidth.StringWidth(dur) - 1
	if pad < 1 {
		pad = 1
	}
	if selected && m.cfg.Theme.Selection == "reverse" {
		return m.selStyle.Render(fmt.Sprintf("%s%s %s%s%s ", "  ", glyph, label, strings.Repeat(" ", pad), dur))
	}
	line := prefix + m.glyphStyles[p.Display()].Render(glyph) + " "
	if selected {
		line += lipgloss.NewStyle().Bold(true).Render(label)
	} else {
		line += label
	}
	return line + strings.Repeat(" ", pad) + m.dimStyle.Render(dur)
}

// renderRich: two lines — identity + right-aligned time, then a dim
// detail line (prompt, or what permission is being waited on).
func (m *model) renderRich(r Row, prefix, glyph string, selected bool) []vline {
	p := r.Pane
	dur := since(p.StatusSince)
	if p.Status == state.StatusWorking && p.CurrentTool != "" {
		dur = ShortTool(p.CurrentTool) + " " + dur
	}
	indent := 4 * r.Depth
	name := p.Agent
	if model := ShortModel(p.Model); model != "" {
		name = model // the model is more informative than the agent name
	}
	if p.ParentPane != "" && p.AgentTitle != "" {
		name = p.AgentTitle
	}
	if (m.spec.Group != "folder" || r.Inbox) && r.Depth == 0 {
		base, _ := SplitFolder(r.Folder)
		name = base + " · " + name
	}
	if r.Session != "" && m.spec.Group != "session" {
		name += " · " + r.Session
	}
	avail := m.width - 2 - 2 - indent - runewidth.StringWidth(dur) - 2
	name = runewidth.Truncate(name, max(avail, 4), "…")
	pad := m.width - 2 - 2 - indent - runewidth.StringWidth(name) - runewidth.StringWidth(dur) - 1
	if pad < 1 {
		pad = 1
	}

	detail := Label(p)
	detailStyle := m.dimStyle
	switch {
	case p.StuckReason != "":
		detail = "stuck: " + p.StuckReason
		detailStyle = m.glyphStyles[state.StatusStuck]
	case p.Status == state.StatusWaitingPermission && p.CurrentTool != "":
		detail = "waits: " + p.CurrentTool
		detailStyle = m.glyphStyles[p.Status]
	}
	detail = runewidth.Truncate(detail, max(m.width-5, 4), "…")

	rowIdx := m.rowIndexOf(p.PaneID)
	if selected && m.cfg.Theme.Selection == "reverse" {
		l1 := m.selStyle.Render(fmt.Sprintf("  %s %s%s%s ", glyph, name, strings.Repeat(" ", pad), dur))
		l2 := m.selStyle.Render(truncate("    "+detail, m.width))
		return []vline{{l1, rowIdx}, {l2, rowIdx}}
	}
	nameStyled := name
	if selected {
		nameStyled = lipgloss.NewStyle().Bold(true).Render(name)
	}
	l1 := prefix + m.glyphStyles[p.Display()].Render(glyph) + " " + nameStyled +
		strings.Repeat(" ", pad) + m.dimStyle.Render(dur)
	detailPrefix := "    "
	if selected && m.cfg.Theme.Selection != "reverse" {
		detailPrefix = m.accentStyle.Render("▌") + "   "
	}
	l2 := detailPrefix + detailStyle.Render(detail)
	return []vline{{l1, rowIdx}, {l2, rowIdx}}
}

func (m *model) rowIndexOf(paneID string) int {
	for i, r := range m.rows {
		if !r.Header && r.Pane.PaneID == paneID {
			return i
		}
	}
	return -1
}

// assignView is the reviewer picker overlay for the `a` key.
func (m *model) assignView() string {
	var b strings.Builder
	base, _ := SplitFolder(ShortPath(m.assignFor.Cwd))
	b.WriteString(m.headerStyle.Render(" Reviewer for "+base) + "\n\n")
	item := func(line string, idx int) {
		if idx == m.assignCursor {
			b.WriteString(m.accentStyle.Render("▌") + " " + line + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	none := "none — unassign"
	if m.assignFor.ReviewerPane == "" {
		none = "none (current)"
	}
	item(m.dimStyle.Render(none), 0)
	for i, row := range m.assignOpts {
		p := row.Pane
		rbase, _ := SplitFolder(ShortPath(p.Cwd))
		who := p.Agent
		if md := ShortModel(p.Model); md != "" {
			who = md
		}
		text := rbase + " · " + who
		if label := Label(p); label != p.Agent && label != who {
			text += " · " + label
		}
		line := m.glyphStyles[p.Status].Render(m.glyphs[p.Status]) + " " +
			truncate(text, max(m.width-6, 8))
		if m.assignFor.ReviewerPane == p.PaneID {
			line += m.accentStyle.Render(" ⇄")
		}
		item(line, i+1)
	}
	b.WriteString("\n" + m.dimStyle.Render(" ↵ assign · esc cancel"))
	return b.String()
}

// toolClassColor groups tools into stable colors: shell, file edits,
// reads, MCP, everything else.
func toolClassColor(tool string) string {
	switch {
	case strings.HasPrefix(tool, "mcp__"):
		return "#cba6f7" // mauve
	case tool == "Bash" || tool == "PowerShell" || strings.Contains(tool, "shell"):
		return "#a6e3a1" // green
	case tool == "Edit" || tool == "Write" || tool == "NotebookEdit" || tool == "MultiEdit":
		return "#f9e2af" // amber
	case tool == "Read" || tool == "Grep" || tool == "Glob" || tool == "LS" || tool == "WebFetch" || tool == "WebSearch":
		return "#89b4fa" // blue
	default:
		return "#f5c2e7" // pink (Task, agents, etc.)
	}
}

// activityView renders the tool-call timeline overlay.
func (m *model) activityView() string {
	var b strings.Builder
	b.WriteString(m.headerStyle.Render(" activity — "+truncate(m.activityLabel, max(m.width-12, 8))) + "\n\n")
	height := m.height - 4
	if height < 1 {
		height = 1
	}
	// scroll the viewport so the selected call stays visible
	if m.activityCursor < m.activityOffset {
		m.activityOffset = m.activityCursor
	}
	if m.activityCursor >= m.activityOffset+height {
		m.activityOffset = m.activityCursor - height + 1
	}
	if maxOffset := len(m.activityCalls) - height; m.activityOffset > maxOffset {
		m.activityOffset = max(maxOffset, 0)
	}
	end := min(m.activityOffset+height, len(m.activityCalls))
	for i := m.activityOffset; i < end; i++ {
		call := m.activityCalls[i]
		selected := i == m.activityCursor
		ts := "     "
		if !call.Time.IsZero() {
			ts = call.Time.Local().Format("15:04")
		}
		style := lipgloss.NewStyle().Foreground(lipgloss.Color(toolClassColor(call.Tool)))
		prefix := "  "
		if selected {
			prefix = m.accentStyle.Render("▌") + " "
		}
		arg := truncate(ShortPath(call.Arg), max(m.width-22, 8))
		line := prefix + m.dimStyle.Render(ts) + " " + style.Render(ShortTool(call.Tool))
		if call.Arg != "" {
			if selected {
				line += " " + arg // the selected row keeps its argument bright
			} else {
				line += " " + m.dimStyle.Render(arg)
			}
		}
		if call.IsError {
			line += " " + m.glyphStyles[state.StatusWaitingPermission].Render("✗")
		}
		b.WriteString(line + "\n")
	}
	pos := ""
	if len(m.activityCalls) > 0 {
		pos = fmt.Sprintf(" · %d/%d", m.activityCursor+1, len(m.activityCalls))
	}
	b.WriteString("\n" + m.dimStyle.Render(" ↵ open · j/k move"+pos+" · esc close"))
	return b.String()
}

// openCallDetail renders one tool call — its arguments and what it
// returned — into a scrollable buffer.
func (m *model) openCallDetail(call transcript.ToolCall) {
	width := max(m.width-2, 20)
	stamp := ""
	if !call.Time.IsZero() {
		stamp = call.Time.Local().Format("15:04:05")
	}
	m.detailTitle = strings.TrimSpace(call.Tool + " " + stamp)
	m.detailLines = nil

	section := func(title, body string) {
		m.detailLines = append(m.detailLines, m.headerStyle.Render(" "+title))
		if strings.TrimSpace(body) == "" {
			m.detailLines = append(m.detailLines, m.dimStyle.Render("   (empty)"))
			return
		}
		for _, line := range textutil.Wrap(body, width-3) {
			m.detailLines = append(m.detailLines, "  "+line)
		}
	}
	section("input", call.Input)
	m.detailLines = append(m.detailLines, "")
	label := "result"
	if call.IsError {
		label = "result (error)"
	}
	if call.Result == "" {
		label = "result (not recorded)"
	}
	section(label, call.Result)
	m.detailOffset = 0
	m.mode = modeActivityDetail
}

// activityDetailView renders the blown-up call, scrolled.
func (m *model) activityDetailView() string {
	var b strings.Builder
	b.WriteString(m.headerStyle.Render(" "+truncate(m.detailTitle, max(m.width-2, 8))) + "\n\n")
	height := m.height - 4
	if height < 1 {
		height = 1
	}
	end := min(m.detailOffset+height, len(m.detailLines))
	for _, line := range m.detailLines[m.detailOffset:end] {
		b.WriteString(line + "\n")
	}
	pos := ""
	if len(m.detailLines) > height {
		pos = fmt.Sprintf(" · %d/%d", min(m.detailOffset+height, len(m.detailLines)), len(m.detailLines))
	}
	b.WriteString("\n" + m.dimStyle.Render(" j/k scroll · ^d/^u page"+pos+" · esc back"))
	return b.String()
}

// inspectMove moves the inspector cursor to the next/previous editable row.
func (m *model) inspectMove(dir int) {
	for i := m.inspectCursor + dir; i >= 0 && i < len(m.inspectRows); i += dir {
		if m.inspectRows[i].Path != "" {
			m.inspectCursor = i
			return
		}
	}
}

// inspectView renders the skills inspector overlay: a cursor over editable
// rows, Enter opens the file in the configured editor.
func (m *model) inspectView() string {
	var b strings.Builder
	b.WriteString(m.headerStyle.Render(" what this agent sees") + "\n\n")
	height := m.height - 4
	if height < 1 {
		height = 1
	}
	if m.inspectCursor >= 0 {
		if m.inspectCursor < m.inspectOffset {
			m.inspectOffset = m.inspectCursor
		}
		if m.inspectCursor >= m.inspectOffset+height {
			m.inspectOffset = m.inspectCursor - height + 1
		}
	}
	end := min(m.inspectOffset+height, len(m.inspectRows))
	for i := m.inspectOffset; i < end; i++ {
		row := m.inspectRows[i]
		switch {
		case row.Header:
			b.WriteString(m.headerStyle.Render(" " + row.Text))
		case i == m.inspectCursor:
			b.WriteString(m.accentStyle.Render("▌") + truncate(row.Text, m.width-1))
		case row.Path != "":
			b.WriteString(" " + truncate(row.Text, m.width-1))
		default:
			b.WriteString(m.dimStyle.Render(truncate(" "+row.Text, m.width)))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n" + m.dimStyle.Render(" ↵ edit · j/k move · esc close"))
	return b.String()
}

// helpView lists every binding: the global prefix keys (from the config,
// so custom keys show correctly) and the in-app keys.
func (m *model) helpView() string {
	title := m.headerStyle.Render
	dim := m.dimStyle.Render
	row := func(key, desc string) string {
		// pad before styling: ANSI escapes must not count into the width
		return "  " + m.accentStyle.Render(fmt.Sprintf("%-10s", key)) + dim(desc) + "\n"
	}
	var b strings.Builder
	b.WriteString(title(" tmux-agent-hub help") + "\n\n")
	b.WriteString(title(" tmux (prefix + key)") + "\n")
	b.WriteString(row(m.cfg.Keys.Sidebar, "toggle this sidebar pane"))
	b.WriteString(row(m.cfg.Keys.Popup, "open the sidebar as a popup"))
	b.WriteString(row(m.cfg.Keys.All, "toggle sidebars in every window"))
	b.WriteString(row(m.cfg.Keys.Find, "fzf search across agents (with preview)"))
	b.WriteString(row(m.cfg.Keys.Next, "jump to the next agent that needs you"))
	b.WriteString(row(m.cfg.Keys.Advisor, "advisor: send agent's reply to another agent"))
	b.WriteString(dim("            (in the sidebar: highlighted agent is the source)") + "\n")
	b.WriteString("\n" + title(" sidebar") + "\n")
	b.WriteString(row("j / k", "move down / up"))
	b.WriteString(row("g / G", "first / last agent"))
	b.WriteString(row("ctrl+d/u", "page down / up (activity, inspector)"))
	b.WriteString(row("enter", "jump to the agent's pane"))
	b.WriteString(row("v", "cycle view: rich / tree"))
	b.WriteString(row("r", "rename the agent"))
	b.WriteString(row("x", "kill the agent's pane (asks y/n)"))
	b.WriteString(row("/", "filter; esc clears"))
	b.WriteString(row("s", "advisor: send this agent's reply to another"))
	b.WriteString(row("a", "assign a live reviewer (advice into chat)"))
	b.WriteString(row("i", "inspect: skills/commands/mcp the agent sees"))
	b.WriteString(row("t", "activity: tool-call timeline (↵ opens a call)"))
	b.WriteString(row("R", "force reload"))
	b.WriteString(row("q", "close the sidebar"))
	b.WriteString("\n" + title(" advisor popup") + "\n")
	b.WriteString(row("enter", "select target, then prompt template"))
	b.WriteString(row("i", "write a custom prompt"))
	b.WriteString("\n" + dim(" statuses: "))
	for _, st := range []state.Status{
		state.StatusWorking, state.StatusWaitingPermission,
		state.StatusWaitingInput, state.StatusDone, state.StatusStuck,
		state.StatusDead,
	} {
		b.WriteString(m.glyphStyles[st].Render(m.glyphs[st]) + dim(" "+string(st)+"  "))
	}
	b.WriteString("\n\n" + dim(" ? / esc / q — close help"))
	return b.String()
}

func (m *model) footer() string {
	switch m.mode {
	case modeFilter:
		return "/" + m.input + "▏"
	case modeRename:
		return "rename: " + m.input + "▏"
	case modeConfirmKill:
		if r := m.current(); r != nil {
			return fmt.Sprintf("kill pane %s? (y/n)", r.Pane.PaneID)
		}
	}
	if m.status != "" {
		return m.dimStyle.Render(truncate(m.status, m.width))
	}
	if m.syncWarn {
		return m.glyphStyles[state.StatusWaitingPermission].Render(
			truncate(" ⚠ synchronize-panes is ON in this window", m.width))
	}
	return m.dimStyle.Render(truncate(" ↵ jump · s send · v view · / filter · ? help · q quit", m.width))
}

func truncate(s string, max int) string {
	if max < 1 {
		return ""
	}
	return runewidth.Truncate(s, max, "…")
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
