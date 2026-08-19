// Package state is the core of the plugin: a per-pane JSON store under
// ~/.local/state/tmux-agent-hub/panes/ written by agent hooks and read by the status
// line and the sidebar.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Status string

const (
	StatusWorking           Status = "working"
	StatusWaitingPermission Status = "waiting_permission"
	StatusWaitingInput      Status = "waiting_input"
	StatusDone              Status = "done"
	StatusDead              Status = "dead"
	// StatusStuck is not a lifecycle state but a flag on top of one: the
	// agent is running yet the detectors say it is going in circles.
	StatusStuck Status = "stuck"
)

// Pane describes one agent running inside one tmux pane.
type Pane struct {
	PaneID         string    `json:"pane_id"` // tmux pane id, e.g. "%12"
	Agent          string    `json:"agent"`   // "claude", "codex", ...
	Status         Status    `json:"status"`
	Cwd            string    `json:"cwd"`
	SessionID      string    `json:"session_id"` // agent's own session id
	TranscriptPath string    `json:"transcript_path,omitempty"`
	LastPrompt     string    `json:"last_prompt,omitempty"` // truncated
	CurrentTool    string    `json:"current_tool,omitempty"`
	Model          string    `json:"model,omitempty"` // from the transcript, e.g. "claude-fable-5"
	Title          string    `json:"title,omitempty"` // user-assigned name
	StatusSince    time.Time `json:"status_since"`
	UpdatedAt      time.Time `json:"updated_at"`

	// Teammate/subagent sessions have no pane of their own: they get a
	// virtual id "<parent>~<sess8>" and hang off ParentPane in the tree.
	ParentPane string `json:"parent_pane,omitempty"`
	AgentTitle string `json:"agent_title,omitempty"` // from transcript agent-name

	// Stuck detection: what the heuristics saw, kept until the agent moves
	// on or the user gives it a new instruction.
	StuckKind      string    `json:"stuck_kind,omitempty"`
	StuckReason    string    `json:"stuck_reason,omitempty"`
	StuckSignature string    `json:"stuck_signature,omitempty"`
	StuckSince     time.Time `json:"stuck_since,omitempty"`

	// NotifiedAt debounces notification effects per agent.
	NotifiedAt time.Time `json:"notified_at,omitempty"`

	// Live-review link (advisor): this agent's turns are fed to ReviewerPane.
	ReviewerPane string `json:"reviewer_pane,omitempty"`
	ReviewOffset int64  `json:"review_offset,omitempty"` // transcript bytes already fed
	// Set on the reviewer while a review request is pending; its next reply
	// is forwarded to that pane as advice. ReviewBoundary marks that the
	// in-flight review covers the end of a turn.
	AwaitingReviewFor string         `json:"awaiting_review_for,omitempty"`
	AwaitingSince     time.Time      `json:"awaiting_since,omitempty"` // to notice a verdict that never came
	ReviewBoundary    bool           `json:"review_boundary,omitempty"`
	ReviewQueue       []QueuedReview `json:"review_queue,omitempty"` // workers waiting for this reviewer
	// Advice waiting on the worker: low severity waits for a turn
	// boundary, high severity waits to be raised a second time.
	PendingSeverity string    `json:"pending_severity,omitempty"`
	PendingNote     string    `json:"pending_note,omitempty"`
	PendingSince    time.Time `json:"pending_since,omitempty"`
	LastReviewAt    time.Time `json:"last_review_at,omitempty"`
	LastAdvice      string    `json:"last_advice,omitempty"` // severity of the last delivered advice
	LastAdviceAt    time.Time `json:"last_advice_at,omitempty"`
	// VerdictOwed marks a reviewer whose verdict could not be read yet:
	// its next event re-reads instead of losing the review.
	VerdictOwed bool `json:"verdict_owed,omitempty"`
	// BoundaryOwed marks a turn that ended before its last words were in
	// the transcript: the review is owed and runs at the next event.
	BoundaryOwed bool `json:"boundary_owed,omitempty"`
	// AdviceStreak counts deliveries since the user last said anything —
	// the bound on a reviewer and a worker talking to each other.
	AdviceStreak int `json:"advice_streak,omitempty"`
	// Set on a reviewer whose current turn is answering a review request:
	// that turn IS the review, and feeding it onward is what would make a
	// mutual pair loop. A worker's turn is never hidden this way.
	SkipNextReview bool `json:"skip_next_review,omitempty"`
}

// QueuedReview is one worker waiting for a busy reviewer.
type QueuedReview struct {
	Pane     string `json:"pane"`
	Boundary bool   `json:"boundary"`
}

// Display is the status to show: a stuck agent is reported as stuck even
// though its lifecycle status is still "working".
func (p *Pane) Display() Status {
	if p.StuckReason != "" && (p.Status == StatusWorking || p.Status == StatusWaitingInput) {
		return StatusStuck
	}
	return p.Status
}

// ClearAwaiting frees a reviewer: no review request is in flight on it.
func (p *Pane) ClearAwaiting() {
	p.AwaitingReviewFor = ""
	p.ReviewBoundary = false
	p.AwaitingSince = time.Time{}
}

// ClearStuck drops a stuck finding (the agent moved on).
func (p *Pane) ClearStuck() {
	p.StuckKind, p.StuckReason, p.StuckSignature = "", "", ""
	p.StuckSince = time.Time{}
}

// AliveIn reports whether this entry is still meaningful given the set of
// existing tmux panes: real entries need their own pane, virtual teammate
// entries live as long as their parent's pane does.
func (p *Pane) AliveIn(panes map[string]bool) bool {
	if p.ParentPane != "" {
		return panes[p.ParentPane]
	}
	return panes[p.PaneID]
}

// SetStatus updates the status and resets StatusSince only on transitions.
func (p *Pane) SetStatus(s Status, now time.Time) {
	if p.Status != s {
		p.Status = s
		p.StatusSince = now
	}
	p.UpdatedAt = now
}

// LinkReviewer makes reviewer the live reviewer of worker, starting from
// whatever the worker does next. Any previous pairing is undone first, so
// re-assigning cannot strand the old reviewer.
func (s *Store) LinkReviewer(workerPane, reviewerPane string) error {
	if workerPane == reviewerPane {
		return fmt.Errorf("an agent cannot review itself")
	}
	worker, err := s.Load(workerPane)
	if err != nil {
		return fmt.Errorf("%s is not a tracked agent", workerPane)
	}
	if _, err := s.Load(reviewerPane); err != nil {
		return fmt.Errorf("%s is not a tracked agent", reviewerPane)
	}
	s.unlink(worker)
	worker.ReviewerPane = reviewerPane
	// feed only what happens from now on
	if st, err := os.Stat(worker.TranscriptPath); err == nil {
		worker.ReviewOffset = st.Size()
	}
	return s.Save(worker)
}

// UnlinkReviewer ends a pairing from the worker's side. Both halves are
// cleaned: the worker keeps no advice it will never hear, and the
// reviewer is not left marked busy with a review nobody will read — that
// would queue everyone else behind it until the lost-request timeout.
// A mutual pairing is two links; this drops only this one.
func (s *Store) UnlinkReviewer(workerPane string) error {
	worker, err := s.Load(workerPane)
	if err != nil {
		return fmt.Errorf("%s is not a tracked agent", workerPane)
	}
	s.unlink(worker)
	return s.Save(worker)
}

// unlink clears the worker's half in memory (the caller saves it) and the
// reviewer's half on disk.
func (s *Store) unlink(worker *Pane) {
	reviewerPane := worker.ReviewerPane
	worker.ReviewerPane = ""
	worker.PendingSeverity, worker.PendingNote = "", ""
	worker.PendingSince = time.Time{}
	worker.LastAdvice, worker.LastAdviceAt = "", time.Time{}
	worker.BoundaryOwed = false
	worker.AdviceStreak = 0
	if reviewerPane == "" {
		return
	}
	reviewer, err := s.Load(reviewerPane)
	if err != nil {
		return
	}
	changed := false
	if reviewer.AwaitingReviewFor == worker.PaneID {
		reviewer.ClearAwaiting()
		reviewer.VerdictOwed = false
		changed = true
	}
	kept := reviewer.ReviewQueue[:0]
	for _, q := range reviewer.ReviewQueue {
		if q.Pane == worker.PaneID {
			changed = true
			continue
		}
		kept = append(kept, q)
	}
	reviewer.ReviewQueue = kept
	if changed {
		s.Save(reviewer)
	}
}

type Store struct {
	dir string
}

// DefaultStore lives in $XDG_STATE_HOME/tmux-agent-hub/panes (default
// ~/.local/state) — runtime state per the XDG base directory spec.
func DefaultStore() (*Store, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return &Store{dir: filepath.Join(base, "tmux-agent-hub", "panes")}, nil
}

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Dir() string { return s.dir }

// fileFor maps a pane id like "%12" to "pane-12.json".
func (s *Store) fileFor(paneID string) string {
	return filepath.Join(s.dir, "pane-"+strings.TrimPrefix(paneID, "%")+".json")
}

func (s *Store) Load(paneID string) (*Pane, error) {
	data, err := os.ReadFile(s.fileFor(paneID))
	if err != nil {
		return nil, err
	}
	var p Pane
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.fileFor(paneID), err)
	}
	return &p, nil
}

// LoadOrNew returns the stored pane or a fresh one if none exists yet
// (an agent may have started before the plugin was installed).
func (s *Store) LoadOrNew(paneID string) *Pane {
	if p, err := s.Load(paneID); err == nil {
		return p
	}
	return &Pane{PaneID: paneID}
}

// Save writes atomically: tmp file in the same dir, then rename.
func (s *Store) Save(p *Pane) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".pane-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.fileFor(p.PaneID))
}

func (s *Store) Delete(paneID string) error {
	err := os.Remove(s.fileFor(paneID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// List returns all tracked panes sorted by cwd, then pane id.
// Unreadable or corrupt files are skipped.
func (s *Store) List() ([]*Pane, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var panes []*Pane
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var p Pane
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}
		panes = append(panes, &p)
	}
	sort.Slice(panes, func(i, j int) bool {
		if panes[i].Cwd != panes[j].Cwd {
			return panes[i].Cwd < panes[j].Cwd
		}
		return panes[i].PaneID < panes[j].PaneID
	})
	return panes, nil
}
