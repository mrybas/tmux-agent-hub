package sidebar

import (
	"testing"
	"time"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/tmuxctl"
)

var folderView = config.ViewPreset("rich") // group=folder, sort=path

func TestBuildRowsGroupsByFolder(t *testing.T) {
	panes := []*state.Pane{ // sorted by cwd, as store.List guarantees
		{PaneID: "%1", Agent: "claude", Cwd: "/a", LastPrompt: "fix tests"},
		{PaneID: "%2", Agent: "claude", Cwd: "/a"},
		{PaneID: "%3", Agent: "codex", Cwd: "/b"},
	}
	locs := map[string]tmuxctl.Location{
		"%1": {Session: "web"}, "%2": {Session: "web"}, "%3": {Session: "api"},
	}
	rows := BuildRows(panes, locs, "", folderView, "")
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5 (2 headers + 3 agents)", len(rows))
	}
	if !rows[0].Header || rows[0].Folder != "/a" {
		t.Errorf("row 0 = %+v, want header /a", rows[0])
	}
	if rows[1].Header || rows[1].Pane.PaneID != "%1" || rows[1].Session != "web" {
		t.Errorf("row 1 = %+v", rows[1])
	}
	if rows[1].Last || !rows[2].Last {
		t.Errorf("Last flags wrong: %+v %+v", rows[1], rows[2])
	}
	if !rows[3].Header || rows[3].Folder != "/b" {
		t.Errorf("row 3 = %+v, want header /b", rows[3])
	}
	if !rows[4].Last {
		t.Errorf("single agent in group must be Last: %+v", rows[4])
	}
}

func TestBuildRowsInboxSection(t *testing.T) {
	t0 := time.Now()
	panes := []*state.Pane{
		{PaneID: "%1", Cwd: "/a", Status: state.StatusWorking, StatusSince: t0},
		{PaneID: "%2", Cwd: "/b", Status: state.StatusDone, StatusSince: t0},
		{PaneID: "%3", Cwd: "/c", Status: state.StatusWaitingPermission, StatusSince: t0},
		{PaneID: "%4", Cwd: "/d", Status: state.StatusDone, StatusSince: t0.Add(-time.Hour)},
	}
	rows := BuildRows(panes, nil, "", folderView, "")
	// inbox header + 3 pinned (perm, older done, done), then /a group + worker
	if !rows[0].Header || rows[0].Folder != "inbox · 3" {
		t.Fatalf("row 0 = %+v, want inbox header", rows[0])
	}
	order := []string{"%3", "%4", "%2"}
	for i, want := range order {
		r := rows[i+1]
		if r.Pane.PaneID != want || !r.Inbox {
			t.Fatalf("inbox row %d = %+v, want %s pinned", i, r, want)
		}
	}
	if !rows[4].Header || rows[4].Folder != "/a" {
		t.Errorf("row 4 = %+v, want /a group header", rows[4])
	}
	if rows[5].Pane.PaneID != "%1" || rows[5].Inbox {
		t.Errorf("row 5 = %+v, want the working agent outside inbox", rows[5])
	}
}

func TestBuildRowsFilter(t *testing.T) {
	panes := []*state.Pane{
		{PaneID: "%1", Agent: "claude", Cwd: "/a", LastPrompt: "Fix Tests"},
		{PaneID: "%2", Agent: "claude", Cwd: "/b", LastPrompt: "deploy"},
	}
	rows := BuildRows(panes, nil, "fix te", folderView, "")
	if len(rows) != 2 || rows[1].Pane.PaneID != "%1" {
		t.Fatalf("filter failed: %+v", rows)
	}
	if rows := BuildRows(panes, nil, "zzz", folderView, ""); len(rows) != 0 {
		t.Fatalf("filter 'zzz' must drop everything, got %+v", rows)
	}
}

func TestLabel(t *testing.T) {
	if l := Label(&state.Pane{Agent: "claude", LastPrompt: "hi", Title: "my task"}); l != "my task" {
		t.Errorf("title must win, got %q", l)
	}
	if l := Label(&state.Pane{Agent: "claude", LastPrompt: "hi"}); l != "hi" {
		t.Errorf("prompt must be second, got %q", l)
	}
	if l := Label(&state.Pane{Agent: "claude"}); l != "claude" {
		t.Errorf("agent is the fallback, got %q", l)
	}
}

func TestSplitFolder(t *testing.T) {
	base, parent := SplitFolder("~/repo/tmux_agent_hub_plugin")
	if base != "tmux_agent_hub_plugin" || parent != "~/repo" {
		t.Errorf("got %q %q", base, parent)
	}
	base, parent = SplitFolder("plain")
	if base != "plain" || parent != "" {
		t.Errorf("got %q %q", base, parent)
	}
}

func TestBuildRowsNestsTeammates(t *testing.T) {
	panes := []*state.Pane{
		{PaneID: "%1", Agent: "claude", Cwd: "/a", LastPrompt: "main work"},
		{PaneID: "%1~abc", Agent: "claude", Cwd: "/a", ParentPane: "%1", AgentTitle: "sleeper"},
		{PaneID: "%1~def", Agent: "claude", Cwd: "/a", ParentPane: "%1", AgentTitle: "worker-2"},
		{PaneID: "%2", Agent: "claude", Cwd: "/b"},
	}
	rows := BuildRows(panes, nil, "", folderView, "")
	// header /a, %1, two children, header /b, %2
	if len(rows) != 6 {
		t.Fatalf("got %d rows, want 6", len(rows))
	}
	if rows[2].Depth != 1 || rows[2].Pane.PaneID != "%1~abc" || rows[2].Last {
		t.Errorf("row 2 = %+v, want first child", rows[2])
	}
	if rows[3].Depth != 1 || !rows[3].Last {
		t.Errorf("row 3 = %+v, want last child", rows[3])
	}
	if !rows[1].Last {
		t.Errorf("parent %%1 is the last top-level agent of its group: %+v", rows[1])
	}
	if l := Label(rows[2].Pane); l != "sleeper" {
		t.Errorf("teammate label = %q, want its agent name", l)
	}
	// orphan child (parent not tracked) surfaces at top level
	orphan := []*state.Pane{{PaneID: "%9~zzz", Cwd: "/c", ParentPane: "%9", AgentTitle: "lost"}}
	rows = BuildRows(orphan, nil, "", folderView, "")
	if len(rows) != 2 || rows[1].Depth != 0 {
		t.Fatalf("orphan must be top-level: %+v", rows)
	}
}

func TestShortTool(t *testing.T) {
	if got := ShortTool("mcp__claude-in-chrome__computer"); got != "computer" {
		t.Errorf("got %q", got)
	}
	if got := ShortTool("Bash"); got != "Bash" {
		t.Errorf("got %q", got)
	}
	if got := ShortTool("SomeVeryLongToolName"); len([]rune(got)) > 12 {
		t.Errorf("not shortened: %q", got)
	}
}

func TestUntrackedAgents(t *testing.T) {
	infos := []tmuxctl.PaneInfo{
		{ID: "%1", Path: "/a", Command: "2.1.226"}, // old claude client
		{ID: "%2", Path: "/b", Command: "codex"},
		{ID: "%3", Path: "/c", Command: "nvim"},    // not an agent
		{ID: "%4", Path: "/d", Command: "claude"},  // tracked already
		{ID: "%5", Path: "/e", Command: "2.1.234"}, // the sidebar itself? no — selfPane below
	}
	got := UntrackedAgents(infos, map[string]bool{"%4": true}, "%5")
	if len(got) != 2 {
		t.Fatalf("got %d untracked, want 2: %+v", len(got), got)
	}
	if got[0].Agent != "claude" || got[1].Agent != "codex" {
		t.Errorf("agents wrong: %+v %+v", got[0], got[1])
	}
	if got[0].Title == "" {
		t.Error("untracked entries must be labeled")
	}
}

func TestBuildRowsGroupsBySession(t *testing.T) {
	sessView := config.ViewPreset("tree")
	sessView.Group = "session"
	panes := []*state.Pane{
		{PaneID: "%1", Cwd: "/a"},
		{PaneID: "%2", Cwd: "/b"},
		{PaneID: "%3", Cwd: "/c"},
	}
	locs := map[string]tmuxctl.Location{
		"%1": {Session: "zeta"}, "%2": {Session: "alpha"}, "%3": {Session: "zeta"},
	}
	rows := BuildRows(panes, locs, "", sessView, "zeta")
	// current session (zeta) first: hdr zeta, %1, %3, hdr alpha, %2
	if len(rows) != 5 {
		t.Fatalf("got %d rows: %+v", len(rows), rows)
	}
	if !rows[0].Header || rows[0].Folder != "zeta · 2" {
		t.Errorf("row 0 = %+v, want zeta·2 header first", rows[0])
	}
	if rows[1].Pane.PaneID != "%1" || rows[2].Pane.PaneID != "%3" {
		t.Errorf("zeta members wrong: %+v %+v", rows[1], rows[2])
	}
	if !rows[3].Header || rows[3].Folder != "alpha · 1" {
		t.Errorf("row 3 = %+v, want alpha·1 header", rows[3])
	}
}
