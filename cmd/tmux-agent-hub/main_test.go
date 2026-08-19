package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrybas/tmux-agent-hub/internal/state"
)

// TestMain keeps these tests off the machine they run on: adoption asks
// tmux about panes, and a throwaway socket has no server to answer.
func TestMain(m *testing.M) {
	os.Setenv("TMUX_AGENT_HUB_TEST_SOCKET", "tmux-agent-hub-cmd-test")
	os.Unsetenv("TMUX")
	os.Exit(m.Run())
}

// The reported bug in its exact shape: an agent asked to exit keeps its
// process (and its confirmation prompt) alive, and its transcript is as
// fresh as the moment the user closed it. Freshness alone therefore
// re-adopts it immediately; only the SessionEnd we received says the
// session is over.
func TestEndedPaneIsNotReadoptedDespiteAFreshTranscript(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, "panes"))
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(home, "repo", "jellifin_plugin")
	project := filepath.Join(home, ".claude", "projects", claudeProjectDir(cwd))
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(project, "ee636f6e.jsonl")
	// written seconds ago: the session ended, the file did not age
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pane := &state.Pane{PaneID: "%34", Agent: "claude", Cwd: cwd}

	// before the session ends, this pane is exactly what adoption is for
	if got := attachSession(pane, store, home, true, time.Hour); !got {
		t.Fatal("a pane with a live-looking session must be adoptable")
	}
	if pane.TranscriptPath != transcript {
		t.Errorf("transcript = %q, want the project's own", pane.TranscriptPath)
	}

	// the user presses Ctrl+C: the hook fires, the process stays
	if err := store.MarkEnded("%34"); err != nil {
		t.Fatal(err)
	}
	after := &state.Pane{PaneID: "%34", Agent: "claude", Cwd: cwd}
	if attachSession(after, store, home, true, time.Hour) {
		t.Error("a pane whose session ended must not be adopted, however fresh its transcript")
	}

	// a new session in the same pane clears the tombstone
	store.ClearEnded("%34")
	again := &state.Pane{PaneID: "%34", Agent: "claude", Cwd: cwd}
	if !attachSession(again, store, home, true, time.Hour) {
		t.Error("a new session in that pane must be tracked again")
	}
}

// A transcript nobody has written to for a long time belongs to a session
// that is over, whatever process is still in the pane.
func TestStaleTranscriptIsNotAdopted(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, "panes"))
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(home, "old")
	project := filepath.Join(home, ".claude", "projects", claudeProjectDir(cwd))
	os.MkdirAll(project, 0o755)
	transcript := filepath.Join(project, "old.jsonl")
	os.WriteFile(transcript, []byte("{}\n"), 0o644)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(transcript, old, old)

	pane := &state.Pane{PaneID: "%7", Agent: "claude", Cwd: cwd}
	if attachSession(pane, store, home, true, time.Minute) {
		t.Error("an hours-old transcript is not this pane's live session")
	}
}
