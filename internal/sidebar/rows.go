// Package sidebar is the bubbletea TUI listing all agents grouped by the
// folder they work in, with jump/kill/rename actions.
package sidebar

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/hookd"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/tmuxctl"
)

// Row is one logical list entry: either a folder header or an agent.
type Row struct {
	Header  bool
	Folder  string // header rows: shortened path; agent rows: their folder
	Pane    *state.Pane
	Session string
	Last    bool // last agent in its group / last child (tree guides)
	Depth   int  // 0 = pane agent, 1 = teammate nested under it
	Inbox   bool // pinned in the inbox section (needs the user)
}

// UntrackedAgents synthesizes entries for panes that run an agent but have
// produced no hook events yet (sessions started before install-hooks).
func UntrackedAgents(infos []tmuxctl.PaneInfo, tracked map[string]bool, selfPane string) []*state.Pane {
	var panes []*state.Pane
	for _, info := range infos {
		if tracked[info.ID] || info.ID == selfPane {
			continue
		}
		agent := hookd.AgentKindInPane(info.Command)
		if agent == "" {
			continue
		}
		panes = append(panes, &state.Pane{
			PaneID: info.ID,
			Agent:  agent,
			Status: state.StatusWaitingInput,
			Cwd:    info.Path,
			Title:  "untracked — no hook events yet",
		})
	}
	return panes
}

// sortWorkingFirst puts the agents that are doing something at the top of
// their own group, leaving the groups themselves — and everything inside
// them — in the order they were already in. What the user scans a session
// for is what is running in it right now.
func sortWorkingFirst(tops []*state.Pane, locs map[string]tmuxctl.Location, group string) {
	key := func(p *state.Pane) string {
		if group == "session" {
			return locs[p.PaneID].Session
		}
		return ShortPath(p.Cwd) // "folder", and a single group for "none"
	}
	// the group order is whatever brought us here; rank preserves it
	rank := map[string]int{}
	for _, p := range tops {
		if k := key(p); rank[k] == 0 {
			rank[k] = len(rank) + 1
		}
	}
	working := func(p *state.Pane) bool { return p.Display() == state.StatusWorking }
	sort.SliceStable(tops, func(i, j int) bool {
		if ri, rj := rank[key(tops[i])], rank[key(tops[j])]; ri != rj {
			return ri < rj
		}
		wi, wj := working(tops[i]), working(tops[j])
		return wi && !wj
	})
}

// urgencyRank orders statuses by how much they need the user.
var urgencyRank = map[state.Status]int{
	state.StatusWaitingPermission: 0,
	state.StatusStuck:             1,
	state.StatusDone:              2,
	state.StatusWorking:           3,
	state.StatusWaitingInput:      4,
	state.StatusDead:              5,
}

// BuildRows prepares the visible list: filter, sort, group. Input is
// sorted by cwd (store.List guarantees that); filter is a
// case-insensitive substring match over folder, title, prompt, agent and
// session name.
// currentSession (may be empty) sorts first in session grouping.
func BuildRows(panes []*state.Pane, locs map[string]tmuxctl.Location, filter string, spec config.ViewSpec, currentSession string) []Row {
	filter = strings.ToLower(filter)
	var kept []*state.Pane
	for _, p := range panes {
		if filter == "" || matches(p, locs[p.PaneID].Session, filter) {
			kept = append(kept, p)
		}
	}
	if spec.Sort == "urgency" {
		sort.SliceStable(kept, func(i, j int) bool {
			ri, rj := urgencyRank[kept[i].Display()], urgencyRank[kept[j].Display()]
			if ri != rj {
				return ri < rj
			}
			return kept[i].StatusSince.Before(kept[j].StatusSince)
		})
	}

	// teammates nest under their parent's row
	childrenOf := map[string][]*state.Pane{}
	tracked := map[string]bool{}
	for _, p := range kept {
		tracked[p.PaneID] = true
	}
	var tops []*state.Pane
	for _, p := range kept {
		if p.ParentPane != "" && tracked[p.ParentPane] {
			childrenOf[p.ParentPane] = append(childrenOf[p.ParentPane], p)
		} else {
			tops = append(tops, p)
		}
	}

	if spec.Sort == "activity" {
		sortWorkingFirst(tops, locs, spec.Group)
	}

	sessionCount := map[string]int{}
	if spec.Group == "session" {
		for _, p := range tops {
			s := locs[p.PaneID].Session
			sessionCount[s] += 1 + len(childrenOf[p.PaneID])
		}
		// session name, current one first; then keep the cwd order
		sess := func(p *state.Pane) string { return locs[p.PaneID].Session }
		sort.SliceStable(tops, func(i, j int) bool {
			si, sj := sess(tops[i]), sess(tops[j])
			if si != sj {
				if si == currentSession {
					return true
				}
				if sj == currentSession {
					return false
				}
				return si < sj
			}
			return false
		})
	}

	// agents that need the user are pinned in an inbox section on top
	needsUser := func(p *state.Pane) bool {
		switch p.Display() {
		case state.StatusWaitingPermission, state.StatusDone, state.StatusStuck:
			return true
		}
		return false
	}
	var inboxTops, restTops []*state.Pane
	for _, p := range tops {
		if needsUser(p) {
			inboxTops = append(inboxTops, p)
		} else {
			restTops = append(restTops, p)
		}
	}
	sort.SliceStable(inboxTops, func(i, j int) bool {
		ri, rj := urgencyRank[inboxTops[i].Display()], urgencyRank[inboxTops[j].Display()]
		if ri != rj {
			return ri < rj
		}
		return inboxTops[i].StatusSince.Before(inboxTops[j].StatusSince)
	})

	var rows []Row
	if len(inboxTops) > 0 {
		rows = append(rows, Row{Header: true, Folder: fmt.Sprintf("inbox · %d", len(inboxTops))})
		for _, p := range inboxTops {
			folder := ShortPath(p.Cwd)
			rows = append(rows, Row{Pane: p, Session: locs[p.PaneID].Session, Folder: folder, Inbox: true})
			kids := childrenOf[p.PaneID]
			for i, kid := range kids {
				rows = append(rows, Row{Pane: kid, Folder: folder, Depth: 1, Last: i == len(kids)-1, Inbox: true})
			}
		}
	}
	tops = restTops
	lastGroup := "\x00"
	for _, p := range tops {
		folder := ShortPath(p.Cwd)
		if folder == "" {
			folder = "(no folder)"
		}
		session := locs[p.PaneID].Session
		var group string
		switch spec.Group {
		case "folder":
			group = folder
		case "session":
			group = session
			if group == "" {
				group = "(no session)"
			}
			group = fmt.Sprintf("%s · %d", group, sessionCount[session])
		}
		if group != "" && group != lastGroup {
			rows = append(rows, Row{Header: true, Folder: group})
			lastGroup = group
		}
		rows = append(rows, Row{Pane: p, Session: session, Folder: folder})
		kids := childrenOf[p.PaneID]
		for i, kid := range kids {
			rows = append(rows, Row{Pane: kid, Folder: folder, Depth: 1, Last: i == len(kids)-1})
		}
	}
	// mark the last top-level agent of every group for tree guides
	for i := range rows {
		if rows[i].Header || rows[i].Depth > 0 {
			continue
		}
		next := i + 1
		for next < len(rows) && rows[next].Depth > 0 {
			next++
		}
		if next == len(rows) || rows[next].Header {
			rows[i].Last = true
		}
	}
	return rows
}

func matches(p *state.Pane, session, filter string) bool {
	for _, s := range []string{p.Cwd, p.Title, p.LastPrompt, p.Agent, session} {
		if strings.Contains(strings.ToLower(s), filter) {
			return true
		}
	}
	return false
}

// Label is the agent line text: the user-given title if any, otherwise
// the last prompt, otherwise the agent name.
func Label(p *state.Pane) string {
	if p.Title != "" {
		return p.Title
	}
	if p.ParentPane != "" && p.AgentTitle != "" {
		return p.AgentTitle // teammates are best known by their agent name
	}
	if p.LastPrompt != "" {
		return p.LastPrompt
	}
	return p.Agent
}

// SplitFolder returns the base name and the dimmed parent part of a
// shortened path: "~/repo/tmux_agent_hub_plugin" -> ("tmux_agent_hub_plugin", "~/repo").
func SplitFolder(short string) (base, parent string) {
	i := strings.LastIndex(short, "/")
	if i < 0 {
		return short, ""
	}
	return short[i+1:], short[:i]
}

// ShortTool compresses a tool name for display: MCP tools like
// "mcp__claude-in-chrome__computer" become "computer".
func ShortTool(tool string) string {
	if i := strings.LastIndex(tool, "__"); i >= 0 {
		tool = tool[i+2:]
	}
	if r := []rune(tool); len(r) > 12 {
		tool = string(r[:11]) + "…"
	}
	return tool
}

// ShortModel compresses a model id for display: "claude-fable-5" ->
// "fable-5". Empty stays empty.
func ShortModel(model string) string {
	return strings.TrimPrefix(model, "claude-")
}

func ShortPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}
