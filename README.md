# tmux-agent-hub

A tmux plugin for people who run many AI coding agents at once.

Run many Claude Code / Codex sessions across tmux windows and sessions and
you quickly lose track of who is working, who finished, and who has been
silently waiting for a permission for the last twenty minutes. tmux-agent-hub
gives you:

- **Status bar glyphs** — one colored glyph per agent: working / waiting
  for permission / finished / idle, visible from any session.
- **Sidebar** — every agent across all sessions in one tree: status, last
  prompt, model, time in state; jump to any agent with Enter. Teammates
  (subagents) nest under their parent.
- **Advisor** — send one agent's last reply to another agent for review
  with one keystroke, or link two agents so every turn is reviewed
  automatically ("Claude reviews Codex and vice versa").
- **Skills inspector** — see exactly which skills, commands, subagents,
  MCP servers and memory files an agent sees, and which level each comes
  from (global / project / plugin). Open any of them in your editor.
- **Agent search** — fzf across everything you remember: folder, status,
  prompt, even the text of the agent's last reply, with a live preview.
- **Notifications** — status-line toasts, blinking glyphs until you react,
  a bell in the agent's window, desktop notifications.

No daemons. State is driven by agent lifecycle hooks; the UI reads a few
small JSON files. When nothing happens, tmux-agent-hub costs nothing.

## Requirements

- tmux ≥ 3.1 for the status bar and the sidebar; ≥ 3.2 for everything
  that opens in a popup (advisor, fzf search, skills inspector)
- `curl` or `wget` — to fetch the release binary on first run
- Go ≥ 1.24 only if you build it yourself (hacking on the plugin)
- [fzf](https://github.com/junegunn/fzf) — optional, for `prefix+f` search
- Claude Code and/or Codex CLI

## Installation

With [TPM](https://github.com/tmux-plugins/tpm) — add to `~/.tmux.conf`
and press `prefix+I`:

```tmux
set -g @plugin 'mrybas/tmux-agent-hub'
```

Or manually:

```bash
git clone https://github.com/mrybas/tmux-agent-hub ~/.tmux/plugins/tmux-agent-hub
```

```tmux
run-shell ~/.tmux/plugins/tmux-agent-hub/plugin.tmux
```

Either way the plugin gets its binary on first run — it downloads the
release for your platform (verified against the release's `SHA256SUMS`)
and needs no toolchain. Only a checkout whose sources are newer than its
binary is built locally, which is what a development tree looks like; if
you want that, install Go. Then register the agent hooks once — this is
the part that makes agents report to the plugin:

```bash
~/.tmux/plugins/tmux-agent-hub/bin/tmux-agent-hub install-hooks
```

It registers hooks in:

- `~/.claude/settings.json` — Claude Code (existing settings preserved,
  a `.bak-tmux-agent-hub` backup is written);
- `~/.codex/hooks.json` — Codex CLI, when `~/.codex` exists. Codex
  requires trusting hooks once: run `/hooks` inside Codex and approve.

Agent sessions pick hooks up on start; sessions that were already running
appear in the sidebar immediately (adopted with limited detail) and gain
full tracking after their next restart. `tmux-agent-hub uninstall-hooks` removes
every hook entry and touches nothing else.

## Default keys

| Key | Action |
|-----|--------|
| `prefix+e` | toggle the sidebar pane in the current window |
| `prefix+o` | open the sidebar as a popup (any window, no layout change) |
| `prefix+E` | toggle sidebars in **every** window (new windows auto-covered) |
| `prefix+f` | fzf search across agents (needs fzf) |
| `prefix+u` | jump to the agent that needs you most |
| `prefix+s` | advisor: send an agent's reply to another agent |

All keys are configurable (`[keys]` in the config) and every binding also
works from Ukrainian/Russian keyboard layouts (the Cyrillic character on
the same physical key is bound automatically).

## Status bar

The plugin inserts a segment into `status-right` (or replaces the
`#{tmux_agent_hub_status}` placeholder if you put one there). One glyph per
agent; glyph shape encodes the status so it stays readable under any
terminal color scheme:

- `✳` working (amber)
- `?` waiting for permission (pink; blinks until you respond)
- `✓` finished (green; blinks until you visit the pane)
- `○` idle at the prompt (teal)
- `✗` dead (grey)

Colors are explicit hex values on purpose — terminal themes remap named
ANSI colors. Above `statusline.max` agents the segment collapses into
per-status counts, most urgent first. Alternative looks: `style = "agent"`
(glyph = agent kind: `✳` Claude, `⬢` Codex; status by color only) and
`bg = true` (colored background blocks).

## Sidebar

Agents that need you — waiting for a permission or finished with unseen
results — are pinned in an **inbox** section at the very top (most
urgent first, folder shown per row); everything else is grouped below.

Two views, cycled live with `v`:

- **rich** (default) — two-line rows: agent/model/session + the last
  prompt; permission waits show which tool is being requested;
- **tree** — one-line rows with `├ ╰` branch guides
  (`view = "compact"` in the config is tree without guides).

Grouping is a separate knob that survives view switching:
`group = "folder"` (default), `"session"` (group by tmux session,
current session first, agent count in the header), or `"none"`.

Inside every group the agents that are **working right now** come first —
that is what you scan a session for. The rest keep their stable order, and
the groups themselves do not move. Set `sort = "path"` for a plain
directory order, or `sort = "urgency"` to put whoever needs you first
everywhere, not just in the inbox.

Keys inside the sidebar:

| Key | Action |
|-----|--------|
| `j`/`k`, `g`/`G` | move / first / last |
| `Enter` | jump to the agent's pane (popup closes after the jump) |
| `v` | cycle view |
| `s` | advisor: send this agent's reply to another agent |
| `a` | assign a live reviewer, or the first entry to unassign |
| `i` | skills inspector for this agent |
| `t` | activity: the agent's tool-call timeline |
| `r` | rename the agent (your label wins over the prompt) |
| `x` | kill the agent's pane (asks y/n) |
| `/` | filter (folder, prompt, title, session); `Esc` clears |
| `R` | force reload |
| `?` | help overlay with every binding |
| `q` | close |

**Teammates / subagents** (Claude Code agent teams) have no pane of their
own; they nest under their parent with their agent name and their own
status glyph. `Enter` jumps to the parent's pane; visiting the parent
acknowledges the whole family's "done" state.

Working agents show their current tool next to the duration
(`✳ deploy fix · Bash 2m`). Long MCP tool names are shortened.

**Untracked agents**: agent panes that predate the hook install are
detected and adopted automatically, so nothing on screen is invisible in
the sidebar. Their status stays best-effort until the session restarts
with hooks.

**Departed agents**: an agent that exits without a `SessionEnd` hook —
killed, crashed, or a CLI that has none — leaves its pane behind, and
the pane being alive is not the same as the agent being alive. A tracked
pane that has gone quiet and now shows a plain shell is checked against
the process table, and the entry is dropped when the agent is really
gone (`advisor.gone_after` is how long "quiet" is; a shell showing while
the agent runs a Bash tool is not it).

## Stuck detection

Model-free heuristics run over every agent's recent tool calls, on each
tool round — no API calls, microseconds. When an agent starts going in
circles it is marked **stuck** (`!` glyph, orange) in the status bar and
pinned to the top of the sidebar with the reason:

```
 inbox · 1
 ! kubevirt · fable-5 · stuck: Bash ran 4 times with the same failure
                        after 3 edits — go test ./...
```

Detectors, most specific first:

| kind | fires when |
|---|---|
| `same_failure` | the same call keeps returning the same failure — edits in between do not clear it, which is exactly the loop worth catching |
| `error_streak` | N tool calls in a row failed, whatever they were |
| `repeat` | the same call repeated N times (excluding edits — editing one file repeatedly is normal work) |
| `oscillation` | A-B-A-B with at least one side failing (healthy browse rhythms are ignored) |
| `no_progress` | a long stretch without touching a file, *after* the agent had started editing (research at the start of a session is not a stall) |

Thresholds live in `[detect]`. The defaults were tuned by replaying
recorded sessions, not guessed — see below. With `nudge = true` the agent
is also told in its own chat what pattern it is repeating; by default the
detectors only tell **you**.

### Tuning against your own sessions

```bash
tmux-agent-hub replay                 # replay every tracked agent
tmux-agent-hub replay --stats         # corpus statistics per session
tmux-agent-hub replay --only=repeat   # one detector, every finding
tmux-agent-hub replay --repeat=6      # try a threshold without editing the config
tmux-agent-hub replay ~/.claude/projects/<dir>   # a directory of transcripts
```

Replay walks each recorded session call by call and reports what the
detectors would have said at the time — the cheap way to see false
positives before they ever interrupt anyone. The shipped defaults produce
about 10 findings per 4400 tool calls of real work.

## Advisor

`prefix+s` opens the advisor popup. The source is context-dependent: the
current pane's agent; the highlighted agent when pressed with the sidebar
focused; otherwise the popup asks whose output to take.

Pick a target agent and a prompt template — the source's **last reply**
(taken from the session transcript, never scraped off the screen) is
typed into the target's chat via bracketed paste and submitted. If the
target is mid-turn, the popup asks before interrupting.

Built-in templates: `review`, `check-diff` ("don't trust the summary —
run `git diff` yourself"), `write-tests`. The first entry in the list is
`✎ custom` (key `i`) — type a prompt right in the popup; the source's
reply is appended automatically. Your own templates are markdown files —
see [Prompt templates](#prompt-templates).

Scripting: `tmux-agent-hub advisor-send <source-pane> <target-pane> <template>`.

### Prompt templates

A template is a markdown file in `~/.config/tmux-agent-hub/templates/`.
Its name is what the advisor popup shows, and a file named like a
built-in (`review.md`, `check-diff.md`, `write-tests.md`) replaces it.

`~/.config/tmux-agent-hub/templates/security.md`:

```markdown
Another agent ({{agent}}) just finished work in {{cwd}}.
Review it for security problems only: injection, secrets in code or
logs, unsafe file permissions, missing authorization checks.
Ignore style and naming. If you find nothing, say so in one line.

--- what it reported ---
{{output}}
```

| placeholder | expands to |
|---|---|
| `{{output}}` | the source agent's last reply, taken from its session transcript (tail-trimmed to `advisor.output_runes`) |
| `{{cwd}}` | the source agent's working directory |
| `{{agent}}` | the source agent's kind (`claude`, `codex`, …) |

If a template contains no `{{output}}`, the reply is not read at all —
useful for prompts that should send the reviewer to the real artifacts
instead of a summary (that is how `check-diff` works). In the `✎ custom`
prompt the same rule applies in reverse: without `{{output}}` the reply
is appended at the end automatically.

Templates are read on every invocation, so a newly saved file shows up
in the popup immediately.

### Live reviewer

Press `a` on an agent in the sidebar and pick another agent as its
reviewer — Claude reviewing Codex or the other way round is fine. From
then on the reviewer watches the work as it happens:

1. **after every tool round**, at the end of a turn, and when you type
   into an agent that is still working (that prompt closes whatever it
   had just concluded), the reviewer
   receives the worker's newest transcript delta — not a summary, the
   actual steps: what you asked for, what the agent said, every tool call
   with its telling argument (`Bash(go test ./...)`, `Read(internal/vm.go)`),
   every failed call with the error, and the report of every subagent it
   delegated to;
2. it answers with a severity prefix: `OK`, `nit: …`, `concern: …` or
   `blocker: …`;
3. what the worker hears depends on that severity.

**Hold and reconfirm.** A review takes tens of seconds, so a *mid-turn*
verdict may describe an already-fixed state. High-severity advice found
mid-turn is therefore not delivered on first sight: it is held, and only
reaches the worker if the next review raises it again — silence in
between means the issue resolved itself, and nothing is said. Nits are
queued and flushed when the turn ends, so they never interrupt work in
progress.

A verdict from the **end-of-turn** review is different: it describes the
finished work, it cannot be stale, and there is no next review to raise
it again — so it is delivered right away, together with anything queued.
The turn boundary is the last moment anything can be said at all.

Delivered advice lands in the worker's chat as
`[tmux-agent-hub advisor · concern from claude@dir] …`.

**Catch-up.** When the worker is about to run another tool while a held
verdict is still being formed, its next tool call is briefly stalled
(`advisor.catch_up_max`, default 20 s) so the advice can arrive before
the next step instead of after it. Hooks are synchronous, so this is a
plain wait — pressing Esc in the agent aborts it like any other tool
call.

**Project rules.** If the worker's directory contains a `WATCHDOG.md`,
its content is appended to every review request — a place to write what
matters in this repo ("never touch migrations without a dry run").

**Any pairing works**: Claude reviewing Codex, Codex reviewing Claude, or
same-kind pairs — the two roles are always two separate sessions. Each
agent's transcript is read through its own adapter (Claude JSONL, Codex
rollout), so worker and reviewer can be different tools. One reviewer can
also serve several workers: requests queue up and run one at a time.

**When a reviewer dies.** A verdict that never arrives (the reviewer was
interrupted, killed, or its Stop hook was lost) would otherwise leave the
pair silent forever: every later review would queue behind a request
nobody is working on. An unanswered request is therefore dropped after 10
minutes — logged as `lost` — and reviewing continues with the next delta.

**What the reviewer always sees.** A worker's turn is never hidden from
its reviewer — not when the advisor started it, not when advice arrived
in the middle of it, not when the reviewer is still busy with the
previous look at the same agent (that one is queued and released by the
verdict). The conclusion an agent ends a turn with is the part a reviewer
most needs, so nothing in the pipeline may consume it silently.

Loop protection therefore lives elsewhere:

- a turn that only answers a review request is never fed onward — this is
  what makes mutual pairs (A reviews B, B reviews A) safe;
- our own injected text is stripped from the delta, so a reviewer never
  reads its own advice back;
- a pair can only talk to itself when advice *wakes* an idle agent, so
  that is the only case that counts: after `advisor.advice_max` such
  deliveries with no word from you the advisor goes quiet and holds what
  it has (`muted` in the log). Advice landing in a turn you set going is
  never muted, however many review rounds a long turn takes;
- your own prompt to the reviewer cancels a pending correlation, and `OK`
  verdicts are swallowed. Pairs are shown in the sidebar
under both ends (`⇄ reviewer: …` / `⇄ reviewing: …`).

Everything the advisor does is recorded in
`~/.local/state/tmux-agent-hub/advisor.log` (JSONL, rotated) — both an
audit trail for "why did I get this advice" and the raw data for tuning.
It is an audit trail in the strict sense: nothing is logged as done until
it is done.

| event | what it means |
|---|---|
| `feed` | a delta went to the reviewer |
| `queued` | the reviewer was busy; this worker is waiting its turn |
| `skipped` | a turn boundary produced no review, with the reason |
| `verdict` | the reviewer answered, with its severity |
| `no_verdict` | the answer could not be read yet — retried, never read as "OK" |
| `hold` | high-severity advice held until the next review raises it again |
| `deliver` | the advice is in the worker's chat (written *after* the send) |
| `unsubmitted` | it was pasted but never submitted — not retried, see below |
| `send_failed` | it never arrived; the delta and the note are put back |
| `drop` | the reviewer's point resolved itself before it was ever delivered |
| `muted` | a reviewer and a worker talking to each other, held until you speak |
| `lost` | a review request that was never answered, released after `lost_after` |

Two of these deserve a note. `skipped` and `no_verdict` exist because a
hook can fire before the words that triggered it are on disk — an agent's
closing sentence lands after its turn is reported as over. A boundary
review therefore waits for the turn's final words, and if they are still
missing it remembers the debt and sends them as their own review the
moment they arrive, so a reviewer is never left judging half a turn. The
same debt covers a boundary the reviewer had no room for (busy with its
own turn, or a full `queue`): every `skipped` line names its reason, and
none of them means the conclusion was thrown away.

`unsubmitted` is the one place delivery is at-most-once: the text is
already sitting in the agent's prompt, so retrying would paste it twice.
A `send_failed` is retried at the next event; an `unsubmitted` is not.

```toml
[advisor]
mode = "live"      # "turn" reviews only at the end of a turn
min_interval = 45  # seconds between mid-turn reviews of one agent
catch_up_max = 20  # 0 disables stalling
advice_max = 3     # deliveries into an idle agent before the advisor waits
                   # for you (0 = no limit); advice landing in work you
                   # started never counts and is never withheld
lost_after = 600   # seconds before an unanswered review request is written off
queue = 8          # workers that may wait for one busy reviewer
grace_ms = 2000    # how long a turn boundary waits for the transcript to
                   # catch up with it (0 = do not wait)
stale_after = 900  # seconds of silence after which a "working" agent is not

# how much text each part of a review is worth, in runes
delta_runes = 7000   # the whole delta handed to a reviewer
arg_runes = 200      # one tool argument in it — clipped in the middle, so
                     # the end of a long command survives
error_runes = 500    # one failed tool's output
report_runes = 1200  # one subagent's report
advice_runes = 2000  # advice injected back into the worker's chat
output_runes = 8000  # {{output}} in a one-shot advisor template
```

**Unassigning** (`a` → the first entry, or
`tmux-agent-hub advisor-assign <worker> none`) ends the pairing on both
sides: the worker keeps no held advice it will never hear, and the
reviewer is released from a review nobody will read — otherwise every
other worker would queue behind it until the request timed out.
Re-assigning does the same to the previous pairing first. A mutual pair
is two links, and each is dropped on its own.

Everything is event-driven — no daemon. Scripting:
`tmux-agent-hub advisor-assign <worker> <reviewer|none>`.

## Skills inspector

`i` on an agent (or `tmux-agent-hub skills [%pane|dir]` from a shell) shows what
that agent actually sees and where it comes from:

- **memory** — the CLAUDE.md chain in load order (global → parent
  directories → cwd, including CLAUDE.local.md); AGENTS.md chain for
  Codex;
- **skills / commands / agents** — each labeled `global`,
  `project <dir>` or `plugin <name>` (only plugins enabled in
  `enabledPlugins` count);
- **mcp servers** — from `~/.claude.json` (global and per-project),
  `.mcp.json` files up the directory chain, or Codex's `config.toml`.

For Codex that means the skills it ships with, the ones you installed,
the ones each enabled plugin brings (newest version only), and any
`.codex/skills` in the project — each labeled with where it came from.

Move with `j`/`k`, press `Enter` to open the selected file (SKILL.md,
command, CLAUDE.md, MCP config…) in your editor inside a large popup.
The editor is the `editor` key in the config (default `nvim`, falls back
to `$EDITOR`).

## Activity timeline

`t` on an agent opens its tool-call history — a scrollable timeline of
everything the agent has done this session: timestamp, tool, and the
most representative argument (the command for Bash, the file for
Edit/Read, …), color-coded by tool class.

Press `Enter` on any call to open it: the full arguments it was given
(rendered as readable `key: value` blocks, not escaped JSON) and the
output it got back, marked when the tool returned an error. `j`/`k`
scroll, `^d`/`^u` page, `Esc` goes back to the timeline.

Everything is read straight from the session transcript — no extra
logging, and it works retroactively for the whole session.

## Agent search (fzf)

`prefix+f` opens a full-width fzf popup over all agents. The indexed line
contains everything you might remember: folder, session, agent/model,
status word, title, last prompt and a snippet of the last reply. fzf
syntax applies: `'waiting` (exact), `!codex`, `kube 'waiting`. The
preview pane shows the agent's card with the full tail of its last reply.

`Enter` jumps to the agent, `Ctrl-S` opens the advisor with it as the
source, `Esc` closes.

## Notifications

Configured per event in `[notify]`; each event maps to a list of effects:

```toml
[notify]
done = ["message", "bell", "blink"]
permission = ["message", "blink"]
stuck = ["message", "blink"]
debounce = 5                 # seconds between pings per agent
popup_position = "top-right" # or "center"
```

Effects:

- `message` — status-line toast on **every attached client**, so you see
  it whichever session you are in;
- `blink` — the status glyph pulses until the cause is gone: `?` until
  you answer the agent, `✓` until you visit its pane, `!` until the agent
  stops going in circles. Real terminal blink where supported plus a
  software pulse (color alternates every second) everywhere else;
- `bell` — rings the agent's pane: the window name lights up in the
  status bar and your terminal reacts per its bell settings;
- `desktop` — system notification (osascript on macOS, notify-send on
  Linux/BSD);
- `popup` — small auto-closing popup in the top-right corner. tmux popups
  grab the keyboard while open, so this one is off by default.

## Themes

A theme is not a separate format — it is a plain TOML config fragment
overlaid between the defaults and your file:

```toml
[theme]
name = "catppuccin-mocha"   # or "gruvbox-dark", or your own file in
                            # ~/.config/tmux-agent-hub/themes/<name>.toml
```

Your own values in `config.toml` always win over the theme. The `[theme]`
section also holds the selection style (`bar`/`reverse`), accent and dim
colors, and tree icons.

## Configuration

Everything lives in one file: `~/.config/tmux-agent-hub/config.toml`
(`$XDG_CONFIG_HOME` respected). The plugin works with no file at all;
`tmux-agent-hub config init` writes a fully commented template — that template
*is* the complete reference. The file is re-read on every render, so
edits apply instantly.

`~/.tmux.conf` keeps only the plugin load line and, optionally, a
`#{tmux_agent_hub_status}` placeholder inside your `status-right` to control
segment placement.

## CLI

```
tmux-agent-hub state list          who is doing what
tmux-agent-hub skills [%pane|dir]  what an agent sees (skills/commands/mcp/memory)
tmux-agent-hub find                fzf agent search (used by prefix+f)
tmux-agent-hub next                jump to the agent that needs you most
tmux-agent-hub advisor-send A B T  send A's last reply to B with template T
tmux-agent-hub advisor-assign A B  link B as A's live reviewer ("none" unlinks)
tmux-agent-hub send-text %pane [f] type a prompt into an agent (stdin by default)
tmux-agent-hub replay [path]       what the stuck-detectors would have said
tmux-agent-hub metrics %pane       one agent's run as JSON (work, findings, advisor)
tmux-agent-hub cleanup             drop state for panes that no longer exist
tmux-agent-hub config init         write the commented default config
tmux-agent-hub install-hooks       register hooks (Claude Code, Codex)
tmux-agent-hub uninstall-hooks     remove all hook entries
tmux-agent-hub version             build version (include it in bug reports)
```

`send-text` and `metrics` are the scripting pair behind unattended runs:
drive an agent from a script, then collect what it did.

## How it works

Agents call `tmux-agent-hub hook` on lifecycle events (`SessionStart`,
`UserPromptSubmit`, `PreToolUse`, `Notification`/`PermissionRequest`,
`Stop`, `SessionEnd`). The handler maps each event to a pane and persists
one small JSON file per agent under `~/.local/state/tmux-agent-hub/panes/`
(`$XDG_STATE_HOME` respected). The status bar and sidebar just read those
files; updates are push-based (`refresh-client -S` + fsnotify), with a 1s
render interval only while a blink effect is configured.

Mapping an event to a pane is the hard part — daemonized sessions
(Claude agent teams, `--fork-session`) run detached from the pane
environment, so resolution is layered:

1. `$TMUX_PANE`, validated (the pane must exist *and* match the event's
   directory — stale values can point at a live foreign pane);
2. session id already tracked in state;
3. the hook's parent process tty → pane;
4. a viewer/client process carrying the session id in its command line →
   its tty → pane;
5. working-directory match over agent-looking panes (process tree
   inspected, so a pane showing `zsh` mid-Bash-tool still counts), with
   screen-content arbitration when several agents share one directory:
   the pane that displays the session's latest text wins.

Pane-less teammates become virtual entries (`%parent~sessionid`) nested
under their parent. Statuses self-heal from three independent signals:
the user-interrupt marker in the transcript (Ctrl+C fires no Stop hook),
transcript silence (a "working" agent whose transcript is quiet for 15
minutes is not working), and hook events themselves. All tmux commands
are pinned to the user's server socket, so hooks fired from hidden agent
servers (`claude-swarm-*`) can never act on the wrong server.

Agent replies are always read from session transcripts (JSONL), never
scraped from the screen.

## Agent support

| | Claude Code | Codex CLI |
|---|---|---|
| statuses, prompts, current tool | ✅ | ✅ |
| model in sidebar | ✅ | ✅ (from event payload) |
| advisor as target | ✅ | ✅ |
| advisor as source (transcript) | ✅ | ✅ |
| live reviewer | ✅ both roles | ✅ both roles |
| activity timeline | ✅ | ✅ |
| reasoning-level analysis | ✅ | ➖ (rollout stores it encrypted) |
| interrupt detection | ✅ | ➖ (no abort marker; stale-detection covers it) |
| skills inspector | ✅ | ✅ (AGENTS.md, system/user/plugin skills, MCP) |

Custom agents can be integrated by calling
`tmux-agent-hub hook <agent-name>` from their lifecycle hooks with a
Claude-Code-shaped JSON payload on stdin.

## Debugging

Statuses look wrong? Enable the flight recorder:

```toml
[debug]
hooks_log = true    # ~/.local/state/tmux-agent-hub/hooks.log
advisor_log = true  # advisor.log — feeds, verdicts, deliveries (on by default)
log_max_kb = 1024   # rotation threshold for both
```

Every hook event logs its session, environment and resolution result
(`-> %pane` or `DROPPED`). Above the threshold the log rotates once to
`hooks.log.old`, so disk usage is capped at ~2× the threshold. Config is
re-read per event — enabling takes effect immediately.

## Limitations

- Sessions started before the hooks were installed are tracked
  best-effort (adopted): visible and jumpable, but statuses may be
  frozen until the session restarts.
- tmux popups always grab the keyboard while open — that is tmux, not
  the plugin; the popup notification effect is therefore opt-in.
- The blink effect sets `status-interval 1` (a ~16 ms render per second).
  Remove `"blink"` from the notify lists and restore your interval if
  you want absolute zero background cost.

## Contributing

Patches welcome. [CONTRIBUTING.md](CONTRIBUTING.md) has the build/test
loop and the house rules; [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
maps the packages, explains pane resolution and the live-reviewer
protocol, and shows how to add support for another agent CLI.

## License

MIT — see [LICENSE](LICENSE).
