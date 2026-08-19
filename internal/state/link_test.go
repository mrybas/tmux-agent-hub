package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o644) }

func mustSave(t *testing.T, s *Store, p *Pane) {
	t.Helper()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
}

// Unlinking is not just "forget the reviewer": a live pairing has state on
// both agents, and leaving the reviewer marked busy would queue every
// other worker behind a review nobody will ever read.
func TestUnlinkReviewerCleansBothSides(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	mustSave(t, s, &Pane{PaneID: "%1", ReviewerPane: "%2",
		PendingSeverity: "concern", PendingNote: "the retry loop has no cap", PendingSince: now,
		LastAdvice: "concern", LastAdviceAt: now, BoundaryOwed: true, AdviceStreak: 2})
	mustSave(t, s, &Pane{PaneID: "%2",
		AwaitingReviewFor: "%1", AwaitingSince: now, ReviewBoundary: true, VerdictOwed: true,
		ReviewQueue: []QueuedReview{{Pane: "%1", Boundary: true}, {Pane: "%3"}}})

	if err := s.UnlinkReviewer("%1"); err != nil {
		t.Fatal(err)
	}

	w, _ := s.Load("%1")
	if w.ReviewerPane != "" {
		t.Error("the link must be gone")
	}
	if w.PendingNote != "" || w.PendingSeverity != "" {
		t.Error("advice the worker will never hear must not linger")
	}
	if w.BoundaryOwed || w.AdviceStreak != 0 || w.LastAdvice != "" {
		t.Errorf("the pairing's leftovers must be cleared: %+v", w)
	}

	r, _ := s.Load("%2")
	if r.AwaitingReviewFor != "" || r.VerdictOwed || r.ReviewBoundary {
		t.Errorf("the reviewer must not stay busy with a review nobody will read: %+v", r)
	}
	if len(r.ReviewQueue) != 1 || r.ReviewQueue[0].Pane != "%3" {
		t.Errorf("only this worker leaves the queue, got %+v", r.ReviewQueue)
	}
}

// A mutual pairing is two independent links; dropping one keeps the other.
func TestUnlinkKeepsTheOppositeLink(t *testing.T) {
	s := NewStore(t.TempDir())
	mustSave(t, s, &Pane{PaneID: "%1", ReviewerPane: "%2"})
	mustSave(t, s, &Pane{PaneID: "%2", ReviewerPane: "%1"})

	if err := s.UnlinkReviewer("%1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Load("%2"); r.ReviewerPane != "%1" {
		t.Errorf("%%2 is still reviewed by %%1, got %q", r.ReviewerPane)
	}
}

// Re-assigning must not strand the previous reviewer either.
func TestLinkReviewerReplacesTheOldPairing(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	transcript := filepath.Join(dir, "w.jsonl")
	if err := writeFile(transcript, "some earlier work\n"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s, &Pane{PaneID: "%1", ReviewerPane: "%2", TranscriptPath: transcript})
	mustSave(t, s, &Pane{PaneID: "%2", AwaitingReviewFor: "%1", AwaitingSince: time.Now()})
	mustSave(t, s, &Pane{PaneID: "%3"})

	if err := s.LinkReviewer("%1", "%3"); err != nil {
		t.Fatal(err)
	}
	w, _ := s.Load("%1")
	if w.ReviewerPane != "%3" {
		t.Errorf("reviewer = %q, want %%3", w.ReviewerPane)
	}
	if w.ReviewOffset == 0 {
		t.Error("a new pairing starts from what the worker does next, not from its history")
	}
	if r, _ := s.Load("%2"); r.AwaitingReviewFor != "" {
		t.Error("the previous reviewer must be released")
	}
}

func TestLinkReviewerRejectsSelfAndStrangers(t *testing.T) {
	s := NewStore(t.TempDir())
	mustSave(t, s, &Pane{PaneID: "%1"})
	if err := s.LinkReviewer("%1", "%1"); err == nil {
		t.Error("an agent cannot review itself")
	}
	if err := s.LinkReviewer("%1", "%404"); err == nil {
		t.Error("an untracked pane is not a reviewer")
	}
	if err := s.UnlinkReviewer("%404"); err == nil {
		t.Error("unlinking an untracked pane must say so")
	}
}
