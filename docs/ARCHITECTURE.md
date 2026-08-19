# Architecture

The README describes what the plugin does. This describes how it is put
together — enough to change it without discovering the constraints the
hard way.

## The shape of the thing

There is no daemon and no background process. Everything is one binary
invoked from three directions:

```
  agent CLI  ──hook──▶  tmux-agent-hub hook   ──▶  ~/.local/state/tmux-agent-hub/panes/*.json
                              │                          ▲          │
                              │                          │          │
  tmux status-right ─────▶  statusline ──────────────────┘          │
  prefix+e / prefix+o ───▶  sidebar (TUI) ──────────────────────────┘
                              │
                              └──▶ tmux send-keys / paste-buffer ──▶ another agent's pane
```

The JSON files are the only shared state. Writers are hook invocations
(one short-lived process per agent event); readers are the status line
(once per `status-interval`) and the sidebar (fsnotify + a refresh key).
Writes are atomic (temp file + rename), so a reader never sees half a
file. Concurrent writers are last-write-wins; the advisor is the only
place where two processes touch the same pane file, and it is ordered so
the reviewer's write lands after the worker's (see `apply` in
`internal/hookd/hookd.go`).

## Packages

| package | responsibility |
|---|---|
| `internal/state` | the `Pane` record and its JSON store. The vocabulary of the whole plugin. |
| `internal/hookd` | hook payload → pane → status. Pane resolution, live review, stuck detection wiring, hook installation. |
| `internal/transcript` | reading agent session transcripts. One `Reader` per agent CLI. |
| `internal/detect` | model-free stuck heuristics over `[]ToolCall`. Pure functions, no I/O. |
| `internal/statusline` | renders the status-bar segment (a tmux format string). Pure. |
| `internal/sidebar` | the sidebar TUI (bubbletea) plus `BuildRows`, which is pure and tested. |
| `internal/advisor` | prompt templates and the send wizard. |
| `internal/skills` | what an agent sees: memory chain, skills, commands, MCP servers. |
| `internal/tmuxctl` | every call to the `tmux` binary. Nothing else shells out to tmux. |
| `internal/config` | defaults, TOML overlay, themes. The commented template in `template.go` is the user-facing reference. |
| `internal/notify`, `internal/eventlog`, `internal/finder`, `internal/layout`, `internal/textutil` | one job each, named after it. |

The dependency direction is one-way: `state` and `transcript` know
nothing about tmux; `tmuxctl` knows nothing about agents.

## Pane resolution

Mapping a hook event to a tmux pane is the hardest part of the plugin,
because daemon-spawned sessions (agent teams, `--fork-session`) run
detached from any pane environment. `resolvePane` tries, in order:

1. `$TMUX_PANE` — but only when the pane exists *and* works in the
   event's directory. A daemon can carry a stale value that points at a
   live pane belonging to someone else.
2. the session id, matched against already-tracked panes (self-healing:
   a mapping that turns out to point at a foreign directory is dropped).
3. the hook's parent process tty → pane. For in-pane sessions this is
   exact, not a guess.
4. a viewer process whose command line carries the session id → its tty
   → pane.
5. working-directory match over agent-looking panes, with screen-content
   arbitration when several agents share one directory.

When nothing resolves, the event is dropped. Wrong data is worse than no
data — a misattributed status makes the sidebar lie. Turn on
`[debug] hooks_log` to see one line per event with its resolution.

Two more rules live here:

- **Sockets.** Hook processes inherit their caller's environment, and
  agent-team daemons run inside their own hidden tmux server. Trusting
  `$TMUX` would make the plugin act on the wrong server, so the user's
  socket is recorded once (`tmuxctl.RecordSocket`, called from
  `plugin.tmux`) and every tmux command pins it explicitly.
- **Teammates.** Sessions with no pane of their own (Claude Code agent
  teams) get the virtual id `%parent~sess8` and nest under the parent.

## Statuses

Hook events drive the status, but hooks are not reliable enough on their
own: a Ctrl+C fires no `Stop`, and a daemon-hosted turn can die without
one. Three signals reconcile:

- hook events (`internal/hookd/hookd.go`, `apply`);
- the user-interrupt marker in the transcript (`Reader.Interrupted`);
- transcript silence — a "working" agent whose transcript has not grown
  for `advisor.stale_after` (15 minutes by default) is not working.

A fourth signal answers a different question — not "what is this agent
doing" but "is it there at all". Panes outlive agents: `/exit`, a crash
or a `SIGKILL` leaves the pane running a shell, and a liveness check that
only asks tmux whether the pane exists keeps that entry for ever.
`ReconcileDeparted` therefore looks at tracked panes whose state has been
quiet for `advisor.gone_after` and whose command is a plain shell, and
drops them when the process table has no agent underneath. The two
filters exist for cost and for correctness: a live agent writes state on
every tool round, and a pane showing a shell mid-Bash-tool is not a
departed agent.

Both reconcilers run from the renderers, which is why statuses heal
without a daemon.

## The live reviewer

Two panes, both ordinary agent sessions: a **worker** and its
**reviewer** (`worker.ReviewerPane`). The link is state on both of them,
so it is made and broken in one place — `state.Store.LinkReviewer` and
`UnlinkReviewer`, used by the sidebar and the CLI alike. Unlinking clears
the worker's held advice and the reviewer's in-flight correlation and
queue entry; leaving the reviewer marked busy would queue every other
worker behind a review nobody will read. Re-assigning unlinks first, and
a mutual pair is two links, dropped one at a time. Everything is event-driven from the
same hooks:

```
worker PostToolUse        ─▶ liveReview(boundary=false)  mid-turn, rate-limited
worker Stop               ─▶ liveReview(boundary=true)   end of turn, always
worker UserPromptSubmit   ─▶ liveReview(boundary=true)   only while it is working
reviewer Stop             ─▶ forwardAdvice               grade, hold or deliver
```

The third line is not redundant. A prompt typed while the agent is still
working is absorbed into the running turn — no `Stop` fires at all — so
the conclusion the agent had just reached would wait for some later tool
round, or never travel. That prompt is the real boundary: whatever the
agent said before it is final. (Our own injected prompts are not.)

A review request carries the **delta**: what the worker's transcript
gained since `ReviewOffset`. The delta is not a summary — it is the
agent's own text, each tool call with its telling argument, each failed
call with the error, and the report of each subagent it delegated to. A
delta of bare tool names is worthless: the reviewer answers `OK` to
everything, which is how this was found (`TestDeltaSinceCarriesWork`).
How much of each part survives is `transcript.Budgets`, filled from
`[advisor]` — the readers take them as an argument rather than reading
config, so the package stays free of everything but transcripts.

Delivery is deliberately conservative, because a review that takes tens
of seconds may describe an already-fixed state:

| verdict | what happens |
|---|---|
| `OK` | nothing; a held high-severity note is considered resolved |
| `nit` | queued, delivered at the next turn boundary |
| `concern` / `blocker` | mid-turn: held, delivered only if the next review raises it again. End of turn: delivered at once — nothing comes after it to raise it again |

Correctness rests on a few invariants, all tested in
`internal/hookd/hookd_test.go`:

- a turn that only answers a review request is never fed onward
  (`SkipNextReview` set on the `ReqMarker` prompt) — this is what makes
  mutual review pairs safe;
- **a worker's turn is never hidden from its reviewer.** Whatever
  prompted it — the user, injected advice, a stuck nudge — the delta
  reaches the reviewer, and a reviewer busy with an earlier look at the
  same agent queues the boundary instead of dropping it. Hiding those
  turns is how the reviewer ended up seeing tool names and never a
  conclusion (`TestFinalReplyAlwaysReachesTheReviewer` walks every path
  that used to lose it). Loop protection is the `ReqMarker` rule above,
  `stripInjected` (the reviewer never reads its own advice back), and
  `advisor.advice_max` — a pair can only talk to itself when advice wakes an
  *idle* agent, so only those deliveries count toward the bound; after
  three of them with no user prompt the advisor holds what it has and
  logs `muted`. Muting advice that lands in a user-driven turn silenced
  the reviewer during exactly the work it was meant to watch;
- the user's own prompt to a reviewer cancels the pending correlation;
- one reviewer serves several workers through `ReviewQueue`, bounded by
  `advisor.queue`;
- a request that is never answered is dropped after `advisor.lost_after`,
  so a killed reviewer cannot mute the pair for the rest of the session;
- **a boundary that cannot be reviewed now is owed, never dropped.**
  Every path that leaves a turn boundary unfed — an empty or half-written
  delta, a reviewer busy with its own turn, a queue at its hard ceiling —
  goes through `oweBoundary`, which sets `BoundaryOwed` and logs the
  reason. The queue ceiling (`hardQueueFactor` × `advisor.queue`) is a
  bound on a state file, not a policy about reviews: reaching it delays a
  conclusion by one event, it does not lose it
  (`TestBoundarySurvivesAFullQueueCeiling`);
- **nothing is marked as said until it was said.** A failed send rewinds
  the worker's `ReviewOffset`, so the unread delta is fed again rather
  than skipped; a failed advice delivery puts the note back into pending
  and undoes `SkipNextReview`/`LastAdvice`, so the advice is re-raised
  instead of vanishing into state that claims it arrived. The `deliver`
  log entry is written after the send, never before it.

**Both sides race their transcripts.** A hook can fire before the words
that triggered it are on disk — the worker's closing message, the
reviewer's verdict. Each side waits out a short grace period
(`advisor.grace_ms`), and each also persists what it still owes; the grace is only the fast path, the
debt in the state file is the guarantee, and it survives a restart
because it is state rather than memory.

The subtle half is that a boundary delta is usually *not* empty when this
happens: the turn's tool calls are already written and only the closing
text is missing. Feeding that is worse than feeding nothing — the
reviewer judges a turn it has half seen. So a boundary waits for a delta
that ends with the agent's own words, and if the grace runs out it feeds
what it has and keeps `BoundaryOwed` set, so the conclusion follows as
its own boundary review the moment it lands — exactly once.

Whether a turn has concluded is metadata from the adapter
(`transcript.Delta.EndsWithReply`), never something inferred from the
rendered text. How a delta is rendered is not a contract: today both
adapters collapse a multiline answer onto one `agent:` line, and code
that read structure back out of that would break the day either of them
stops.

A verdict that cannot be read is logged as `no_verdict` and changes
nothing — treating it as `OK` would quietly resolve a concern the
reviewer is still holding. The correlation is kept and re-read at the
reviewer's next event (`VerdictOwed`); a second unreadable attempt frees
the reviewer so nothing queued behind it waits on words that never came.

Typing into a pane is two tmux commands — paste, then Enter — so there is
a third outcome between success and failure: `tmuxctl.ErrNotSubmitted`,
the text is in the agent's composer but was never submitted. Retrying
would paste it twice, so that outcome is kept rather than rolled back and
recorded as `unsubmitted`.

Be precise about what that costs. Delivery here is **at-most-once, not
guaranteed**: if the user clears the composer instead of submitting it,
that review request (and the offset that went with it) is gone — the
lost-request timeout frees the reviewer, but the delta is not fed again.
The alternative, rewinding on timeout, means a delta reviewed twice when
the user does submit the text later, and duplicated advice is worse than
a skipped mid-turn look. One thing is *not* assumed, though: an
unsubmitted advice must not set `SkipNextReview`. That flag belongs to a
turn that has not started, and the `AdvMarker` prompt sets it if the text
is ever really submitted — pre-setting it would swallow the delta of
whatever the user does next.

Every step is appended to `~/.local/state/tmux-agent-hub/advisor.log`
(`feed`, `queued`, `skipped`, `verdict`, `no_verdict`, `hold`, `deliver`,
`unsubmitted`, `send_failed`, `drop`, `muted`, `lost` — the README has
the table). Two rules make it an audit trail rather than a narration:
nothing is recorded as done before it is done, and every exception to the
"a conclusion always reaches the reviewer" invariant is recorded with its
reason. Both exist because the alternative was diagnosing a silent
reviewer by reading byte offsets out of a transcript.

## Adding another agent CLI

Support for an agent is two things: hooks that call us, and a transcript
reader.

**1. Hooks.** The agent must invoke `tmux-agent-hub hook <name>` on its
lifecycle events with a Claude-Code-shaped JSON payload on stdin:

```json
{
  "hook_event_name": "PostToolUse",
  "session_id": "…",
  "transcript_path": "/path/to/session.jsonl",
  "cwd": "/path/to/work",
  "tool_name": "Bash",
  "prompt": "…",
  "message": "…",
  "model": "…"
}
```

Events consumed: `SessionStart`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse`, `Notification` (or `PermissionRequest`), `Stop`,
`SessionEnd`. Unknown fields are ignored and unknown events are no-ops,
so a partial payload degrades instead of breaking. If the agent ships a
hooks file of its own, register it in `internal/hookd/install.go` next
to `codexEvents` — installation must stay idempotent and must never
touch entries that are not ours.

**2. A transcript reader.** Implement `transcript.Reader` (see
`internal/transcript/codex.go` for a full example) and register it in
`ForBudgets` (`For` is the same thing with default sizing):

```go
LastReply()  // last reply text + model — the advisor's source material
ToolCalls(n) // recent calls with args and results — detectors, timeline
DeltaSince(offset) // Delta{Text, End, EndsWithReply} — the review feed
Interrupted()      // is the tail a user abort?
AgentTitle()       // subagent name, when the session is a teammate
```

Everything else — status bar, sidebar, notifications, stuck detection,
the advisor — works off `state.Pane` and does not care which agent it is.
The one thing to get right is `DeltaSince`: it is the only thing another
agent ever reads, so it must carry the work, not a list of tool names.

Take the `Budgets` the constructor is handed and apply them in
`DeltaSince` — how much of a tool argument, an error or a subagent report
survives is the user's setting, not the adapter's opinion.

Finally, add a glyph in `[statusline.agent_glyphs]` and teach
`sidebar.UntrackedAgents` / `hookd.looksLikeAgent` what the pane command
looks like, so pre-existing sessions are still adopted.

## Testing rules

- **Never touch the user's live tmux server.** `TMUX_AGENT_HUB_TEST_SOCKET`
  pins tests to a throwaway socket; `TestMain` in `internal/hookd` sets it
  and unsets `$TMUX`. A test that sends keys to a real pane types into
  whatever the developer is running.
- **Never touch the user's real state.** Tests set `XDG_CONFIG_HOME` and
  `XDG_STATE_HOME` to temp dirs. The state store takes an explicit
  directory (`state.NewStore(t.TempDir())`); the event log does not — it
  reads `XDG_STATE_HOME`, which is exactly how fake reviews once ended up
  in a real `advisor.log`.
- **Typing into a pane is a seam.** `hookd.sendText` is a variable
  (`= tmuxctl.SendText`); the hook tests replace it, so the review state
  machine runs with no tmux server and the tests can assert what a
  reviewer actually received — including what happens when a send fails.
- Prefer testing the pure layer (`BuildRows`, `detect.Analyze`,
  `statusline.Render`, the transcript readers) — that is where the logic
  is, and it needs no tmux at all.
