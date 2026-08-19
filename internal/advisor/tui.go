package advisor

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/layout"
	"github.com/mrybas/tmux-agent-hub/internal/sidebar"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/textutil"
	"github.com/mrybas/tmux-agent-hub/internal/tmuxctl"
)

type step int

const (
	stepSource   step = iota // pick the source agent (when prefix+s was pressed elsewhere)
	stepTarget               // pick who receives the prompt
	stepTemplate             // pick the prompt template
	stepCustom               // typing a free-form prompt
	stepConfirm              // target is busy — send anyway?
)

type target struct {
	pane    *state.Pane
	session string
}

type wizard struct {
	cfg        config.Config
	agents     []target // every tracked alive agent
	src        *state.Pane
	pickSource bool // wizard started outside an agent pane
	targets    []target
	templates  []Template
	step       step
	srcCursor  int
	cursor     int
	tplCursor  int // 0 = custom prompt, 1..N = templates
	input      string
	pending    Template // chosen template awaiting busy-confirmation
	err        string

	width       int
	glyphs      map[state.Status]string
	glyphStyles map[state.Status]lipgloss.Style
	titleStyle  lipgloss.Style
	dimStyle    lipgloss.Style
}

// RunWizard opens the interactive picker and sends the message. When
// srcPane is a tracked agent, it is the source; otherwise the wizard
// starts by asking which agent's output to take. Designed for
// tmux display-popup -E.
func RunWizard(cfg config.Config, store *state.Store, srcPane string) error {
	panes, err := store.List()
	if err != nil {
		return err
	}
	locs, _ := tmuxctl.PaneLocations()
	tracked := map[string]bool{}
	var agents []target
	for _, p := range panes {
		if _, alive := locs[p.PaneID]; !alive {
			continue
		}
		tracked[p.PaneID] = true
		agents = append(agents, target{pane: p, session: locs[p.PaneID].Session})
	}
	// agent panes without hook events yet (pre-install sessions) are
	// still valid targets — typing into their TUI works fine
	if infos, err := tmuxctl.PanesFull(); err == nil {
		for _, p := range sidebar.UntrackedAgents(infos, tracked, "") {
			agents = append(agents, target{pane: p, session: locs[p.PaneID].Session})
		}
	}
	if len(agents) == 0 {
		return fmt.Errorf("no agents found")
	}
	w := &wizard{
		cfg:         cfg,
		agents:      agents,
		templates:   LoadTemplates(),
		glyphs:      cfg.Statusline.StatusGlyphs.ToMap(),
		titleStyle:  lipgloss.NewStyle().Foreground(lipgloss.Color(cfg.Theme.Accent)).Bold(true),
		dimStyle:    lipgloss.NewStyle().Foreground(lipgloss.Color(cfg.Theme.Dim)),
		glyphStyles: map[state.Status]lipgloss.Style{},
	}
	for st, hex := range cfg.Colors.ToMap() {
		w.glyphStyles[st] = lipgloss.NewStyle().Foreground(lipgloss.Color(hex))
	}
	if src, err := store.Load(srcPane); err == nil {
		w.setSource(src)
		w.step = stepTarget
	} else {
		w.pickSource = true
		w.step = stepSource
	}
	if w.step == stepTarget && len(w.targets) == 0 {
		return fmt.Errorf("no other agents to send to")
	}
	_, err = tea.NewProgram(w).Run()
	return err
}

// setSource fixes the source agent and rebuilds the target list without it.
func (w *wizard) setSource(src *state.Pane) {
	w.src = src
	w.targets = w.targets[:0]
	for _, t := range w.agents {
		if t.pane.PaneID != src.PaneID {
			w.targets = append(w.targets, t)
		}
	}
	w.cursor = 0
}

func (w *wizard) Init() tea.Cmd { return nil }

func (w *wizard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		w.width = msg.Width
	case tea.KeyMsg:
		w.err = ""
		if w.step == stepCustom {
			return w.updateCustom(msg)
		}
		switch layout.Normalize(msg.String()) {
		case "q", "ctrl+c":
			return w, tea.Quit
		case "esc":
			switch w.step {
			case stepTarget:
				if w.pickSource {
					w.step = stepSource
				} else {
					return w, tea.Quit
				}
			case stepTemplate:
				w.step = stepTarget
			case stepConfirm:
				w.step = stepTemplate
			default:
				return w, tea.Quit
			}
		case "j", "down":
			w.moveCursor(1)
		case "k", "up":
			w.moveCursor(-1)
		case "i":
			if w.step == stepTemplate {
				w.step = stepCustom
			}
		case "y":
			if w.step == stepConfirm {
				return w.send()
			}
		case "n":
			if w.step == stepConfirm {
				w.step = stepTemplate
			}
		case "enter":
			switch w.step {
			case stepSource:
				w.setSource(w.agents[w.srcCursor].pane)
				if len(w.targets) == 0 {
					w.err = "no other agents to send to"
					return w, nil
				}
				w.step = stepTarget
			case stepTarget:
				w.step = stepTemplate
			case stepTemplate:
				if w.tplCursor == 0 {
					w.step = stepCustom
					return w, nil
				}
				return w.proceed(w.templates[w.tplCursor-1])
			case stepConfirm:
				return w.send()
			}
		}
	}
	return w, nil
}

func (w *wizard) updateCustom(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEscape:
		w.step = stepTemplate
	case tea.KeyEnter:
		if strings.TrimSpace(w.input) == "" {
			return w, nil
		}
		return w.proceed(CustomTemplate(w.input))
	case tea.KeyBackspace:
		if len(w.input) > 0 {
			rs := []rune(w.input)
			w.input = string(rs[:len(rs)-1])
		}
	case tea.KeyCtrlC:
		return w, tea.Quit
	case tea.KeyRunes, tea.KeySpace:
		// KeySpace already carries the space in Runes — appending another
		// one would double every space typed
		if len(msg.Runes) > 0 {
			w.input += string(msg.Runes)
		} else if msg.Type == tea.KeySpace {
			w.input += " "
		}
	}
	return w, nil
}

// proceed runs the busy-target guard before sending tpl.
func (w *wizard) proceed(tpl Template) (tea.Model, tea.Cmd) {
	w.pending = tpl
	if w.targets[w.cursor].pane.Status == state.StatusWorking {
		w.step = stepConfirm
		return w, nil
	}
	return w.send()
}

func (w *wizard) moveCursor(dir int) {
	switch w.step {
	case stepSource:
		w.srcCursor = clamp(w.srcCursor+dir, len(w.agents))
	case stepTarget:
		w.cursor = clamp(w.cursor+dir, len(w.targets))
	case stepTemplate:
		w.tplCursor = clamp(w.tplCursor+dir, len(w.templates)+1)
	}
}

func clamp(v, n int) int {
	if v < 0 {
		return 0
	}
	if v >= n {
		return n - 1
	}
	return v
}

func (w *wizard) send() (tea.Model, tea.Cmd) {
	tgt := w.targets[w.cursor]
	output := SourceOutput(w.pending, w.src)
	msg := BuildMessage(w.pending, w.src, output, w.cfg.OutputBudget())
	if err := tmuxctl.SendText(tgt.pane.PaneID, msg); err != nil {
		w.err = err.Error()
		return w, nil
	}
	tmuxctl.DisplayMessage(fmt.Sprintf("tmux-agent-hub: sent %q to %s",
		tmuxctl.EscapeFormat(w.pending.Name), tgt.pane.PaneID))
	return w, tea.Quit
}

// lineWidth is the usable popup width (fallback before WindowSizeMsg).
func (w *wizard) lineWidth() int {
	if w.width > 10 {
		return w.width - 4
	}
	return 66
}

func (w *wizard) agentLine(t target) string {
	p := t.pane
	base, _ := sidebar.SplitFolder(sidebar.ShortPath(p.Cwd))
	glyph := w.glyphStyles[p.Status].Render(w.glyphs[p.Status])
	who := p.Agent
	if m := sidebar.ShortModel(p.Model); m != "" {
		who = m // the model is more informative than the agent name
	}
	budget := w.lineWidth() - len([]rune(base)) - len([]rune(who)) - 11
	label := truncate(sidebar.Label(p), max(budget, 8))
	if label == p.Agent || label == who {
		// nothing informative beyond the agent kind — don't repeat it
		return fmt.Sprintf("%s %s · %s", glyph, base, w.dimStyle.Render(who))
	}
	return fmt.Sprintf("%s %s · %s · %s", glyph, base, w.dimStyle.Render(who), label)
}

// fromLine reminds which agent's output is being forwarded. The newlines
// stay OUTSIDE the styled string: lipgloss pads multi-line blocks to
// their widest line, which shifts everything after them.
func (w *wizard) fromLine() string {
	if w.src == nil {
		return "\n"
	}
	base, _ := sidebar.SplitFolder(sidebar.ShortPath(w.src.Cwd))
	label := truncate(sidebar.Label(w.src), max(w.lineWidth()-len([]rune(base))-16, 8))
	return w.dimStyle.Render(fmt.Sprintf(" from: %s · %s (%s)", base, label, w.src.PaneID)) + "\n\n"
}

func (w *wizard) View() string {
	var b strings.Builder
	hints := " ↵ select · esc back · q quit"
	switch w.step {
	case stepSource:
		b.WriteString(w.titleStyle.Render(" Take the output of which agent?"))
		b.WriteString("\n")
		b.WriteString(w.dimStyle.Render(" (the pane you pressed the key in is not a tracked agent)"))
		b.WriteString("\n\n")
		for i, t := range w.agents {
			b.WriteString(w.renderItem(w.agentLine(t), i == w.srcCursor))
		}
	case stepTarget:
		b.WriteString(w.titleStyle.Render(" Send to which agent?"))
		b.WriteString("\n")
		b.WriteString(w.fromLine())
		for i, t := range w.targets {
			b.WriteString(w.renderItem(w.agentLine(t), i == w.cursor))
		}
	case stepTemplate:
		b.WriteString(w.titleStyle.Render(" Which prompt?"))
		b.WriteString("\n")
		b.WriteString(w.fromLine())
		b.WriteString(w.renderItem("✎ custom"+w.dimStyle.Render(" — write your own prompt"), w.tplCursor == 0))
		for i, t := range w.templates {
			firstLine, _, _ := strings.Cut(strings.TrimSpace(t.Body), "\n")
			line := t.Name + w.dimStyle.Render(" — "+truncate(firstLine, 46))
			b.WriteString(w.renderItem(line, i+1 == w.tplCursor))
		}
		hints = " ↵ select · i custom · esc back · q quit"
	case stepCustom:
		b.WriteString(w.titleStyle.Render(" Your prompt"))
		b.WriteString("\n\n")
		for _, line := range textutil.Wrap(w.input+"▏", w.lineWidth()) {
			b.WriteString(" " + line + "\n")
		}
		b.WriteString("\n")
		b.WriteString(w.dimStyle.Render(" The agent's last reply is appended automatically") + "\n" +
			w.dimStyle.Render(" (write {{output}} to place it yourself)."))
		hints = " ↵ send · esc back"
	case stepConfirm:
		tgt := w.targets[w.cursor].pane
		b.WriteString(w.titleStyle.Render(" Target is busy"))
		b.WriteString("\n\n")
		fmt.Fprintf(&b, " %s is still working — interrupt it with the prompt? (y/n)\n",
			tgt.PaneID)
		hints = " y send · n back"
	}
	b.WriteString("\n")
	if w.err != "" {
		b.WriteString(" " + w.err + "\n")
	}
	b.WriteString(w.dimStyle.Render(hints))
	return b.String()
}

// renderItem marks selection with the accent bar only: re-wrapping an
// already-styled line in another style garbles the inner ANSI sequences.
func (w *wizard) renderItem(line string, selected bool) string {
	if selected {
		return w.titleStyle.Render("▌") + " " + line + "\n"
	}
	return "  " + line + "\n"
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
