# Contributing

Bug reports and patches are welcome. The plugin runs inside other
people's terminals next to long-running agent sessions, so the bar is
"never gets in the way" before "has more features".

## Getting started

```bash
make build   # go build -o bin/tmux-agent-hub ./cmd/tmux-agent-hub
make test    # go test ./...
make vet     # go vet ./...
```

Go ≥ 1.24. No other tooling is required; `fzf` is only needed to use the
search popup. A development checkout builds itself: `plugin.tmux`
downloads a release binary only when Go is missing, and a tree whose
sources are newer than `bin/tmux-agent-hub` is always built, never
downloaded — otherwise your changes would be silently replaced by the
last release.

To try a change against your own tmux, rebuild and re-source the plugin:

```bash
make build && tmux source ~/.tmux.conf
```

Hooks call the binary by absolute path, so a rebuild takes effect on the
agents' next event — no reinstall needed unless the path changed.

[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) maps the packages, explains
pane resolution and the live-reviewer protocol, and describes how to add
support for another agent CLI.

## House rules

**Tests must not touch the machine they run on.** This is the one rule
with a history:

- tmux commands go through `internal/tmuxctl`, which honours
  `TMUX_AGENT_HUB_TEST_SOCKET` — a test that reaches the user's server
  types into whatever they are running;
- set `XDG_STATE_HOME` and `XDG_CONFIG_HOME` to temp dirs. The pane store
  takes an explicit directory, but the event log resolves
  `XDG_STATE_HOME` itself, and unisolated tests have written fake reviews
  into a real `advisor.log`.

**Hooks must never break an agent.** Anything on the hook path
(`internal/hookd`, `internal/tmuxctl`, `internal/transcript`) fails
silently and returns: a broken config falls back to defaults, an
unreadable transcript is not an error, an unresolvable event is dropped.
An agent must never see a stack trace because the plugin had a bad day.

**Temporary probes stay out of the tree.** A scratch test file written
into the repo to answer one question is visible to anything watching the
work — including a reviewer agent, which will report it as debris. Put
throwaway scripts in a scratch directory outside the repository.

**Prefer the pure layer.** Row building, detectors, rendering and the
transcript readers are pure functions with tests. Put logic there, keep
the TUI and the tmux calls thin.

**Code, comments and UI strings are English.** Comments say *why*, not
what the next line does — the file's existing density is the target.

**One user-visible change, one README update.** The README is the manual;
`internal/config/template.go` is the config reference. A setting that
exists in neither does not exist.

## Releases

Tagging is the whole process: `git tag -a v0.1.0 -m v0.1.0 && git push
--tags` runs `.github/workflows/release.yml`, which runs the suite, then
cross-compiles `darwin/{arm64,amd64}` and `linux/{amd64,arm64}` (pure Go,
`CGO_ENABLED=0`, one runner) and publishes the tarballs plus `SHA256SUMS`
as a GitHub Release. The tag name is stamped into the binary — check with
`tmux-agent-hub version`.

`.github/workflows/ci.yml` is the other half: build, vet, test and a
gofmt check on every push and pull request. It publishes nothing.

## Reporting a bug

Statuses looking wrong is almost always pane resolution. Enable the
flight recorder and include the relevant lines:

```toml
[debug]
hooks_log = true    # ~/.local/state/tmux-agent-hub/hooks.log
```

Each line shows the event, its session, the environment it arrived with
and what it resolved to (`-> %pane` or `DROPPED`). Also useful: `tmux -V`,
the agent CLI and its version, and `tmux-agent-hub state list`.
