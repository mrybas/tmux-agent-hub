package hookd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/detect"
	"github.com/mrybas/tmux-agent-hub/internal/eventlog"
	"github.com/mrybas/tmux-agent-hub/internal/state"
	"github.com/mrybas/tmux-agent-hub/internal/tmuxctl"
	"github.com/mrybas/tmux-agent-hub/internal/transcript"
)

// Live review: a reviewer agent assigned to a worker receives the worker's
// transcript deltas — after tool rounds, not only at the end of a turn —
// and its advice is injected back into the worker's chat. Everything is
// event-driven from the hooks in this package; there is no daemon:
//
//	worker PostToolUse -> feed the delta (rate-limited) — mid-turn review
//	worker Stop        -> feed the final delta — turn boundary
//	reviewer Stop      -> grade the verdict, hold it or deliver it
//
// Advice is graded and delivered under a hold-and-reconfirm rule, because
// a mid-turn review that takes tens of seconds may describe an
// already-fixed state:
//
//	nit               queued, delivered at the next turn boundary
//	concern/blocker   mid-turn: held on first sight; delivered only when
//	                  the next review raises it again (silence = resolved)
//
// The end-of-turn review is the exception: it judges finished work, it
// cannot be stale, and no further review will raise it again — so its
// verdict goes out at once, together with anything queued.

// sendText types a message into an agent's pane. It is a variable so the
// hook tests can drive the state machine without a tmux server — every
// other tmux call in this package is a silent no-op when there is none,
// but a failed send changes what the reviewer does next.
var sendText = tmuxctl.SendText

// ReqMarker prefixes review requests typed into the reviewer, so hooks can
// tell them apart from the user's own prompts.
const ReqMarker = "[tmux-agent-hub review request]"

// AdvMarker prefixes advice injected into the worker, so its next turn is
// recognized as advice-triggered and not fed back to the reviewer.
const AdvMarker = "[tmux-agent-hub advisor"

// Severity levels of a review verdict, lowest first.
const (
	sevNone    = ""
	sevNit     = "nit"
	sevConcern = "concern"
	sevBlocker = "blocker"
)

func isHigh(severity string) bool {
	return severity == sevConcern || severity == sevBlocker
}

// liveReview feeds the worker's newest transcript delta to its reviewer.
// boundary marks the end of a turn, where a review always runs; mid-turn
// reviews are rate-limited and skipped while one is still in flight.
func liveReview(store *state.Store, p *state.Pane, cfg config.Config, boundary bool, now time.Time) {
	if p.ReviewerPane == "" || p.TranscriptPath == "" {
		return
	}
	if p.SkipNextReview {
		// this turn IS a review of someone else — never fed onward
		if boundary {
			p.SkipNextReview = false
			if d, err := transcript.ForBudgets(p.Agent, p.TranscriptPath, cfg.DeltaBudgets()).
				DeltaSince(p.ReviewOffset); err == nil {
				p.ReviewOffset = d.End
			}
			logSkip(cfg, p, "review turn, not fed onward")
		}
		return
	}
	if p.BoundaryOwed {
		// a turn ended with its last words still unwritten; this event is
		// the first chance to feed them, whatever kind of event it is
		boundary = true
	}
	reviewer, err := store.Load(p.ReviewerPane)
	if err != nil {
		p.ReviewerPane = "" // reviewer is gone, drop the link
		if boundary {
			logSkip(cfg, p, "reviewer is gone")
		}
		return
	}
	if !boundary {
		if cfg.Advisor.Mode != "live" {
			return
		}
		if now.Sub(p.LastReviewAt) < time.Duration(cfg.Advisor.MinInterval)*time.Second {
			return
		}
	}
	if reviewer.AwaitingReviewFor != "" {
		switch {
		case reviewer.AwaitingSince.IsZero():
			// a request from before this field existed — stamp it now and
			// give it the full grace period
			reviewer.AwaitingSince = now
			store.Save(reviewer)
		case now.Sub(reviewer.AwaitingSince) > cfg.Advisor.LostTimeout():
			// no verdict ever came back: the reviewer was interrupted,
			// killed, or its Stop hook was lost. Without this the pair goes
			// silent for the rest of the session — every later review is
			// "queued" behind a request nobody is working on.
			logAdvice(cfg, eventlog.Advice{Event: "lost", Worker: reviewer.AwaitingReviewFor,
				Reviewer: reviewer.PaneID, Reviewing: reviewer.Agent,
				LatencyMS: now.Sub(reviewer.AwaitingSince).Milliseconds()})
			reviewer.ClearAwaiting()
			if store.Save(reviewer) != nil {
				return
			}
		}
	}
	// A reviewer in the middle of its own turn is busy in every sense that
	// matters: injecting there mixes our request into work that is not ours
	// and costs that agent its own conclusion.
	if reviewer.AwaitingReviewFor == "" && reviewer.Status == state.StatusWorking {
		if enqueueReview(reviewer, p.PaneID, boundary, cfg) && store.Save(reviewer) == nil {
			logAdvice(cfg, eventlog.Advice{Event: "queued", Worker: p.PaneID,
				Reviewer: reviewer.PaneID, Agent: p.Agent, Boundary: boundary,
				Note: "reviewer busy with its own turn"})
		} else if boundary {
			oweBoundary(cfg, p, "reviewer busy with its own turn, not queued")
		}
		return
	}
	if reviewer.AwaitingReviewFor != "" {
		// the reviewer is busy — queue this worker instead of dropping it,
		// so one reviewer can serve several agents. A review already in
		// flight for THIS worker is not a reason to drop its turn boundary
		// either: the end of a turn carries the conclusions, and a mid-turn
		// review that is still running was looking at unfinished work.
		if enqueueReview(reviewer, p.PaneID, boundary, cfg) {
			if store.Save(reviewer) == nil {
				logAdvice(cfg, eventlog.Advice{Event: "queued", Worker: p.PaneID,
					Reviewer: reviewer.PaneID, Agent: p.Agent, Boundary: boundary})
			}
		} else if boundary {
			// the queue is at its hard ceiling. The delta is untouched, but
			// without the debt the retry would come back as a mid-turn look
			// (and be rate-limited): the boundary itself must survive.
			oweBoundary(cfg, p, "reviewer busy and its queue is full")
		}
		return
	}
	d, ok := nextDelta(p, cfg)
	if boundary && (!ok || !d.EndsWithReply) {
		// The Stop hook and the transcript write race, and the hook wins
		// often enough to matter: an agent's closing words land in the file
		// after the turn is reported as over. Feeding what is there right
		// then means feeding the tool calls without the conclusion they led
		// to — which is how a reviewer came to judge a turn it had only
		// half seen. Wait for the words; the agent has finished, nothing is
		// waiting on this.
		if waited, got := waitForConclusion(p, cfg, cfg.Advisor.BoundaryGrace()); got {
			d, ok = waited, true
		}
	}
	if !ok {
		if boundary {
			// still nothing: remember that this turn owes a review, so the
			// next event of any kind delivers it instead of rate-limiting it
			oweBoundary(cfg, p, "no delta yet")
		}
		return
	}
	{
		delta := stripInjected(d.Text)
		prevOffset, prevAt, prevOwed := p.ReviewOffset, p.LastReviewAt, p.BoundaryOwed
		// a boundary whose conclusion is still unwritten is only half paid:
		// the closing words are owed to the next event
		p.ReviewOffset, p.LastReviewAt = d.End, now
		p.BoundaryOwed = boundary && !d.EndsWithReply
		if sendReviewRequest(store, cfg, p, reviewer, delta, boundary, now) != nil {
			// the request never left: rewind, or this delta is never
			// reviewed — the offset would have skipped past it. The debt
			// stands too, so the next event retries as a boundary.
			p.ReviewOffset, p.LastReviewAt = prevOffset, prevAt
			p.BoundaryOwed = prevOwed || boundary
		}
	}
}

// hardQueueFactor bounds the queue relative to advisor.queue: it lives in
// a state file, so it cannot grow without end. It is a memory limit, not
// a policy about reviews — a boundary refused here is remembered on the
// worker (BoundaryOwed) and reviewed at its next event instead.
const hardQueueFactor = 2

// enqueueReview adds a worker to a busy reviewer's queue, reporting
// whether anything changed. A pane appears at most once: a second request
// for it only upgrades the queued entry to a turn boundary, which is the
// one thing that must not be skipped.
func enqueueReview(reviewer *state.Pane, pane string, boundary bool, cfg config.Config) bool {
	for i := range reviewer.ReviewQueue {
		if reviewer.ReviewQueue[i].Pane != pane {
			continue
		}
		if boundary && !reviewer.ReviewQueue[i].Boundary {
			reviewer.ReviewQueue[i].Boundary = true
			return true
		}
		return false
	}
	if reviewer.AwaitingReviewFor == pane && !boundary {
		return false // another mid-turn look while one is already in flight
	}
	if len(reviewer.ReviewQueue) >= cfg.Advisor.QueueLimit() {
		if !boundary {
			return false
		}
		// a finished turn outranks a mid-turn look: drop the oldest of
		// those to make room rather than lose a conclusion
		for i, q := range reviewer.ReviewQueue {
			if !q.Boundary {
				reviewer.ReviewQueue = append(reviewer.ReviewQueue[:i], reviewer.ReviewQueue[i+1:]...)
				break
			}
		}
		if len(reviewer.ReviewQueue) >= hardQueueFactor*cfg.Advisor.QueueLimit() {
			return false // every slot is a boundary already; state has a limit too
		}
	}
	reviewer.ReviewQueue = append(reviewer.ReviewQueue,
		state.QueuedReview{Pane: pane, Boundary: boundary})
	return true
}

// deltaPrefixes are how a delta line announces what it is. A line with
// none of them continues the one above it.
var deltaPrefixes = []string{"user: ", "agent: ", "tools: ", "failed ", "subagent report: "}

func startsDeltaLine(line string) bool {
	for _, p := range deltaPrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// stripInjected drops our own injected text from a delta — advice typed
// into a worker, review requests typed into a reviewer. Handing either
// back to the agent that wrote it invites it to raise the same point
// again. A marker swallows every following continuation line too: the
// renderers collapse a message into one line today, but a multi-line
// leak would quietly restart the loop this prevents.
func stripInjected(delta string) string {
	lines := strings.Split(delta, "\n")
	kept := lines[:0]
	dropping := false
	for _, line := range lines {
		switch {
		case strings.Contains(line, AdvMarker), strings.Contains(line, ReqMarker):
			dropping = true
		case dropping && !startsDeltaLine(line):
			// still inside the injected message
		default:
			dropping = false
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// waitForConclusion polls the transcript until the delta ends with the
// agent's own words, or the grace runs out. It reports whether it has
// anything at all to hand back. Whether a turn has concluded comes from
// the adapter, not from how the delta happens to be rendered.
func waitForConclusion(p *state.Pane, cfg config.Config, grace time.Duration) (transcript.Delta, bool) {
	const step = 100 * time.Millisecond
	last, got := transcript.Delta{End: p.ReviewOffset}, false
	for waited := time.Duration(0); waited < grace; waited += step {
		time.Sleep(step)
		d, ok := nextDelta(p, cfg)
		if !ok {
			continue
		}
		last, got = d, true
		if d.EndsWithReply {
			break
		}
	}
	return last, got
}

// waitForReply polls for a reviewer's verdict to reach its transcript.
func waitForReply(r *state.Pane, grace time.Duration) string {
	const step = 100 * time.Millisecond
	for waited := time.Duration(0); waited < grace; waited += step {
		time.Sleep(step)
		if reply, err := transcript.LastReplyText(r.Agent, r.TranscriptPath); err == nil &&
			strings.TrimSpace(reply) != "" {
			return reply
		}
	}
	return ""
}

// nextDelta reads what the worker did since its last review.
func nextDelta(p *state.Pane, cfg config.Config) (transcript.Delta, bool) {
	d, err := transcript.ForBudgets(p.Agent, p.TranscriptPath, cfg.DeltaBudgets()).
		DeltaSince(p.ReviewOffset)
	if err != nil || strings.TrimSpace(d.Text) == "" {
		return transcript.Delta{End: p.ReviewOffset}, false
	}
	return d, true
}

// sendReviewRequest marks the reviewer busy and types the request into it.
// The error is what tells the caller to rewind the worker's offset: a
// request that never arrived must not count as reviewed.
func sendReviewRequest(store *state.Store, cfg config.Config, worker, reviewer *state.Pane, delta string, boundary bool, now time.Time) error {
	reviewer.AwaitingReviewFor = worker.PaneID
	reviewer.AwaitingSince = now
	reviewer.ReviewBoundary = boundary
	if err := store.Save(reviewer); err != nil {
		return err
	}
	if err := sendText(reviewer.PaneID, buildReviewRequest(worker, delta, boundary)); err != nil {
		if errors.Is(err, tmuxctl.ErrNotSubmitted) {
			// the request is sitting in the reviewer's composer: rewinding
			// would paste it a second time. Treat it as fed — if no verdict
			// comes, the lost-request timeout picks it up.
			logAdvice(cfg, eventlog.Advice{Event: "unsubmitted", Worker: worker.PaneID,
				Reviewer: reviewer.PaneID, Agent: worker.Agent, Reviewing: reviewer.Agent,
				Boundary: boundary, Bytes: len(delta), Note: clip(err.Error(), 200)})
			return nil
		}
		// the request never reached the reviewer: releasing it now beats
		// leaving it "busy" with a review that was never asked for
		reviewer.ClearAwaiting()
		store.Save(reviewer)
		logAdvice(cfg, eventlog.Advice{Event: "send_failed", Worker: worker.PaneID,
			Reviewer: reviewer.PaneID, Agent: worker.Agent, Reviewing: reviewer.Agent,
			Boundary: boundary, Note: clip(err.Error(), 200)})
		return err
	}
	logAdvice(cfg, eventlog.Advice{Event: "feed", Worker: worker.PaneID,
		Reviewer: reviewer.PaneID, Agent: worker.Agent, Reviewing: reviewer.Agent,
		Boundary: boundary, Bytes: len(delta)})
	return nil
}

// popQueue starts the next queued review, if any.
func popQueue(store *state.Store, cfg config.Config, reviewer *state.Pane, now time.Time) {
	for len(reviewer.ReviewQueue) > 0 {
		next := reviewer.ReviewQueue[0]
		reviewer.ReviewQueue = reviewer.ReviewQueue[1:]
		worker, err := store.Load(next.Pane)
		if err != nil || worker.ReviewerPane != reviewer.PaneID {
			continue // gone or unlinked meanwhile
		}
		d, ok := nextDelta(worker, cfg)
		if !ok {
			continue
		}
		delta := stripInjected(d.Text)
		prevOffset, prevAt, prevOwed := worker.ReviewOffset, worker.LastReviewAt, worker.BoundaryOwed
		worker.ReviewOffset, worker.LastReviewAt = d.End, now
		worker.BoundaryOwed = next.Boundary && !d.EndsWithReply
		if store.Save(worker) != nil {
			continue
		}
		if sendReviewRequest(store, cfg, worker, reviewer, delta, next.Boundary, now) != nil {
			// rewind and try whoever else is queued: this delta still owes
			// the reviewer a look
			worker.ReviewOffset, worker.LastReviewAt = prevOffset, prevAt
			worker.BoundaryOwed = prevOwed || next.Boundary
			store.Save(worker)
			continue
		}
		return
	}
}

// forwardAdvice runs on the reviewer's Stop: it grades the verdict and
// decides whether the worker hears about it now, later, or never.
func forwardAdvice(store *state.Store, r *state.Pane, cfg config.Config, now time.Time) {
	if r.AwaitingReviewFor == "" {
		// not reviewing anything — but this agent may have workers waiting
		// for it to finish its own turn
		popQueue(store, cfg, r, now)
		return
	}
	workerPane := r.AwaitingReviewFor
	boundary := r.ReviewBoundary
	alreadyRetried := r.VerdictOwed
	r.ClearAwaiting()
	r.VerdictOwed = false

	worker, err := store.Load(workerPane)
	if err != nil || worker.ReviewerPane != r.PaneID {
		// worker gone or the link was dropped meanwhile — the reviewer is
		// free either way, so whoever is waiting for it must still be served
		popQueue(store, cfg, r, now)
		return
	}
	reply, err := transcript.LastReplyText(r.Agent, r.TranscriptPath)
	if err != nil {
		return
	}
	if strings.TrimSpace(reply) == "" {
		// the same race as on the worker's side: the reviewer's Stop can
		// beat the write of what it just said. An empty read is not an "OK"
		// — treating it as one would quietly resolve a held concern — so
		// wait a moment, and if there is still nothing, say nothing.
		reply = waitForReply(r, cfg.Advisor.BoundaryGrace())
		if strings.TrimSpace(reply) == "" {
			note := "unreadable — retrying at the next event"
			if !alreadyRetried {
				// keep the correlation and try again at the reviewer's next
				// event: the words are on their way, and an empty read must
				// neither be forwarded nor mistaken for an "OK"
				r.AwaitingReviewFor, r.ReviewBoundary = workerPane, boundary
				r.VerdictOwed = true
				store.Save(r)
			} else {
				// twice is enough: free the reviewer so whoever is queued
				// behind it is not stuck waiting for words that never came
				note = "unreadable twice — giving up on this review"
				popQueue(store, cfg, r, now)
			}
			logAdvice(cfg, eventlog.Advice{Event: "no_verdict", Worker: workerPane,
				Reviewer: r.PaneID, Reviewing: r.Agent, Boundary: boundary, Note: note})
			return
		}
	}
	severity, text := parseVerdict(reply)
	var latency int64
	if !worker.LastReviewAt.IsZero() {
		latency = now.Sub(worker.LastReviewAt).Milliseconds()
	}
	event := eventlog.Advice{Event: "verdict", Worker: worker.PaneID, Reviewer: r.PaneID,
		Agent: worker.Agent, Reviewing: r.Agent, Severity: severity, Boundary: boundary,
		Note: clip(text, 1500), LatencyMS: latency}
	logAdvice(cfg, event)

	// snapshot: an advice send that fails must leave the worker exactly as
	// it was, or the note is lost while the state claims it was delivered
	prevAdvice, prevAdviceAt := worker.LastAdvice, worker.LastAdviceAt
	prevSkip := worker.SkipNextReview

	deliver, delivered := "", ""
	switch {
	case severity == sevNone:
		// the reviewer is satisfied: a held high-severity note is resolved
		if isHigh(worker.PendingSeverity) {
			worker.PendingSeverity, worker.PendingNote = "", ""
		}
	case severity == sevNit:
		worker.PendingSeverity, worker.PendingNote = sevNit, text
		worker.PendingSince = time.Now()
	case isHigh(worker.PendingSeverity):
		// raised again — the issue survived the agent's next steps
		deliver, delivered = text, severity
		worker.PendingSeverity, worker.PendingNote = "", ""
	default:
		worker.PendingSeverity, worker.PendingNote = severity, text
		worker.PendingSince = time.Now()
	}

	// a turn boundary is the last chance to say anything queued
	if boundary && deliver == "" && worker.PendingNote != "" {
		deliver, delivered = worker.PendingNote, worker.PendingSeverity
		worker.PendingSeverity, worker.PendingNote = "", ""
	}

	// A pair can only talk to itself when the advice STARTS the worker's
	// turns: an idle agent that is woken by advice, answers, gets reviewed,
	// is woken again. Advice that lands in a turn the user set going cannot
	// sustain that loop — the turn ends when the work does — so it must
	// never be muted, however many rounds a long turn takes.
	startsTurn := worker.Status != state.StatusWorking

	if limit := cfg.Advisor.AdviceLimit(); deliver != "" && startsTurn &&
		limit > 0 && worker.AdviceStreak >= limit {
		worker.PendingSeverity, worker.PendingNote = delivered, deliver
		worker.PendingSince = now
		deliver, delivered = "", ""
		logAdvice(cfg, eventlog.Advice{Event: "muted", Worker: worker.PaneID,
			Reviewer: r.PaneID, Agent: worker.Agent, Reviewing: r.Agent,
			Severity: worker.PendingSeverity, Note: clip(worker.PendingNote, 1500)})
	}

	switch {
	case deliver != "":
		// SkipNextReview is set by the AdvMarker prompt when the advice
		// really starts a turn of its own — deciding it here would swallow
		// the work of an agent that was already mid-turn
		worker.LastAdvice, worker.LastAdviceAt = delivered, now
		event.Event, event.Severity, event.Note = "deliver", delivered, clip(deliver, 1500)
	case worker.PendingNote != "":
		event.Event = "hold"
	case severity != sevNone:
		event.Event = "drop"
	}
	// "hold" and "drop" are settled here; "deliver" is a claim about the
	// worker's chat, so it is logged only once the text is really in it
	if event.Event != "verdict" && event.Event != "deliver" {
		logAdvice(cfg, event)
	}
	if err := store.Save(worker); err != nil {
		return
	}
	if deliver != "" {
		// the worker is saved before the send on purpose: the injected
		// advice makes it fire its own hooks, which write the same file
		err := sendText(worker.PaneID, buildAdvice(r, delivered, deliver, cfg.AdviceBudget()))
		switch {
		case err == nil:
			if startsTurn {
				worker.AdviceStreak++
				store.Save(worker)
			}
			logAdvice(cfg, event)
		case errors.Is(err, tmuxctl.ErrNotSubmitted):
			// the advice is in the worker's composer but unsent: putting it
			// back into pending would paste it twice, so it counts as said.
			// The loop guard is a different question — it belongs to a turn
			// that has not started. If the text is ever submitted, the
			// AdvMarker prompt sets the flag then; if the user clears the
			// composer instead, setting it now would swallow the delta of
			// whatever they do next.
			worker.SkipNextReview = prevSkip
			store.Save(worker)
			event.Event = "unsubmitted"
			event.Note = clip(deliver, 1500) + " — " + clip(err.Error(), 200)
			logAdvice(cfg, event)
		default:
			// nothing was said, so nothing is resolved: hold the note again
			// (it can still be raised or dropped by the next review) and
			// undo the marks that claim the worker has heard it
			worker.PendingSeverity, worker.PendingNote = delivered, deliver
			worker.PendingSince = now
			worker.LastAdvice, worker.LastAdviceAt = prevAdvice, prevAdviceAt
			worker.SkipNextReview = prevSkip
			store.Save(worker)
			event.Event, event.Note = "send_failed", clip(err.Error(), 200)
			logAdvice(cfg, event)
		}
	}
	// the reviewer is free again — serve whoever was waiting
	popQueue(store, cfg, r, now)
}

// oweBoundary records that a turn boundary could not be reviewed now and
// must be reviewed at the next event, whatever kind it is. Every path
// that leaves a boundary unfed goes through here, so "the conclusion
// always reaches the reviewer" has no silent exceptions.
func oweBoundary(cfg config.Config, p *state.Pane, reason string) {
	p.BoundaryOwed = true
	logSkip(cfg, p, reason)
}

// logSkip records a turn boundary that produced no review, and why. The
// invariant is that a worker's conclusion always reaches its reviewer, so
// every exception to it has to be visible.
func logSkip(cfg config.Config, p *state.Pane, reason string) {
	logAdvice(cfg, eventlog.Advice{Event: "skipped", Worker: p.PaneID,
		Reviewer: p.ReviewerPane, Agent: p.Agent, Boundary: true, Note: reason})
}

func logAdvice(cfg config.Config, rec eventlog.Advice) {
	if !cfg.Debug.AdvisorLog {
		return
	}
	rec.Time = time.Now()
	eventlog.Append("advisor", cfg.Debug.LogMaxKB, rec)
}

func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// catchUp stalls the worker's next tool call while its reviewer is still
// forming a verdict on a held high-severity note. Hooks are synchronous,
// so waiting here pauses the agent; the user can always abort the tool
// call. Bounded by advisor.catch_up_max (0 disables it).
func catchUp(store *state.Store, p *state.Pane, cfg config.Config) {
	if cfg.Advisor.CatchUpMax <= 0 || p.ReviewerPane == "" || !isHigh(p.PendingSeverity) {
		return
	}
	reviewer, err := store.Load(p.ReviewerPane)
	if err != nil || reviewer.AwaitingReviewFor != p.PaneID {
		return // nothing in flight to wait for
	}
	deadline := time.Now().Add(time.Duration(cfg.Advisor.CatchUpMax) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		if r, err := store.Load(p.ReviewerPane); err == nil && r.AwaitingReviewFor != p.PaneID {
			return // the verdict landed
		}
	}
}

// verdictRe matches the severity prefix the reviewer is asked to use.
var verdictRe = regexp.MustCompile(`(?is)^\W*(ok|nit|concern|blocker)\b[:.\s-]*(.*)$`)

// parseVerdict grades a reviewer reply. Unlabeled advice counts as a
// concern, so it goes through the reconfirm cycle instead of interrupting
// the worker on a possibly stale observation.
func parseVerdict(reply string) (severity, text string) {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return sevNone, ""
	}
	if m := verdictRe.FindStringSubmatch(reply); m != nil {
		body := strings.TrimSpace(m[2])
		switch strings.ToLower(m[1]) {
		case "ok":
			return sevNone, ""
		case "nit":
			return sevNit, body
		case "concern":
			return sevConcern, body
		case "blocker":
			return sevBlocker, body
		}
	}
	return sevConcern, reply
}

func buildReviewRequest(worker *state.Pane, delta string, boundary bool) string {
	when := "The agent is still working — this is a mid-turn review."
	if boundary {
		when = "The agent just finished its turn — this is the final review of it."
	}
	rules := ""
	if data, err := os.ReadFile(filepath.Join(worker.Cwd, "WATCHDOG.md")); err == nil {
		rules = "\n--- project review rules (WATCHDOG.md) ---\n" +
			transcript.Tail(string(data), 4000) + "\n"
	}
	return fmt.Sprintf(`%s
You are a silent reviewer for another AI agent (%s) working in %s.
%s Below is what happened since your last review.

Reply with ONE severity prefix and, unless it is OK, a few concrete sentences:
  OK               nothing worth saying
  nit: ...         minor; the agent hears it when its turn ends
  concern: ...     real risk; you will see the next round and must repeat
                   it if it still stands (silence means resolved)
  blocker: ...     wrong direction, stuck in a loop, about to break things

Judge the direction, not the style. Do not edit files or run write
commands for this request — you are advice-only.
%s
--- delta ---
%s`, ReqMarker, worker.Agent, worker.Cwd, when, rules, delta)
}

func buildAdvice(reviewer *state.Pane, severity, text string, budget int) string {
	name := reviewer.Agent
	if base := filepath.Base(reviewer.Cwd); base != "" && base != "." && base != "/" {
		name += "@" + base
	}
	if severity == "" {
		severity = sevConcern
	}
	return fmt.Sprintf("%s · %s from %s] %s",
		AdvMarker, severity, name, transcript.Tail(text, budget))
}

// detectStuck runs the model-free detectors over the agent's recent tool
// calls and records what they saw. Findings are kept until the agent
// moves on; the same finding is only re-reported after a cooldown, so a
// long loop pings the user once, not forty times.
func detectStuck(p *state.Pane, cfg config.Config, now time.Time) {
	if !cfg.Detect.Enabled || p.TranscriptPath == "" {
		return
	}
	th := cfg.DetectThresholds()
	window := th.Window
	if window <= 0 {
		window = 20
	}
	calls, err := transcript.For(p.Agent, p.TranscriptPath).ToolCalls(window)
	if err != nil {
		return
	}
	f := detect.Analyze(calls, th)
	if f == nil {
		p.ClearStuck() // whatever it was, the agent is doing something else
		return
	}
	sig := f.Kind + "|" + f.Signature
	if p.StuckSignature == sig {
		cooldown := time.Duration(cfg.Detect.Cooldown) * time.Second
		if cooldown > 0 && now.Sub(p.StuckSince) < cooldown {
			return // same finding, still inside its cooldown
		}
	}
	p.StuckKind, p.StuckReason, p.StuckSignature, p.StuckSince = f.Kind, f.Reason, sig, now
	logAdvice(cfg, eventlog.Advice{Event: "detect", Worker: p.PaneID, Agent: p.Agent,
		Severity: f.Kind, Note: clip(f.Reason, 500), Bytes: f.Count})

	// arm B is notification-only; nudging the agent is opt-in
	if cfg.Detect.Nudge {
		// the nudge is stripped from the delta like any injected text; the
		// turn it lands in is still the agent's own work and still reviewed
		sendText(p.PaneID, fmt.Sprintf(
			"%s · stuck] %s — stop and reconsider the approach instead of repeating it.",
			AdvMarker, f.Reason))
	}
}
