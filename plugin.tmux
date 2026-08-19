#!/usr/bin/env bash
# tmux-agent-hub entry point for TPM. Builds the Go binary on first run, wires the
# status-bar segment and the pane-focus hook. Safe to re-run (tmux reload).

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$CURRENT_DIR/bin/tmux-agent-hub"

# Rebuild when anything that ends up inside the binary is newer than it:
# Go sources, embedded templates and themes, and the dependency set.
stale() {
  [ ! -x "$BIN" ] && return 0
  [ -n "$(find "$CURRENT_DIR/cmd" "$CURRENT_DIR/internal" \
      \( -name '*.go' -o -name '*.md' -o -name '*.toml' \) -newer "$BIN" 2>/dev/null | head -1)" ] && return 0
  [ "$CURRENT_DIR/go.mod" -nt "$BIN" ] || [ "$CURRENT_DIR/go.sum" -nt "$BIN" ]
}
if stale; then
  if command -v go >/dev/null 2>&1; then
    (cd "$CURRENT_DIR" && make -s build) \
      || tmux display-message "tmux-agent-hub: go build failed, see 'make build'"
  elif [ ! -x "$BIN" ]; then
    # with a prebuilt binary in place there is nothing to complain about,
    # even when its sources look newer than it
    tmux display-message "tmux-agent-hub: no Go toolchain — install Go, or drop a release binary in bin/tmux-agent-hub"
  fi
fi
[ -x "$BIN" ] || exit 0

# --- status bar -------------------------------------------------------------
# Put #{tmux_agent_hub_status} anywhere in your status-right to control placement;
# without the placeholder the segment is prepended to the existing value.
SEGMENT="#(\"$BIN\" statusline) "
current="$(tmux show-option -gqv status-right)"
case "$current" in
  *tmux_agent_hub_status*)
    tmux set-option -g status-right "${current//"#{tmux_agent_hub_status}"/$SEGMENT}"
    ;;
  *"tmux-agent-hub\\\" statusline"* | *"$SEGMENT"*)
    : # already wired
    ;;
  *)
    tmux set-option -g status-right "$SEGMENT$current"
    ;;
esac

# --- key bindings from ~/.config/tmux-agent-hub/config.toml ------------------------
"$BIN" bind

# --- "sidebar everywhere": new windows get a sidebar while the mode is on ---
for hook in after-new-window after-new-session; do
  if ! tmux show-hooks -g "$hook" 2>/dev/null | grep -qF 'tmux-agent-hub\" sidebar-ensure'; then
    tmux set-hook -ga "$hook" "run-shell -b '\"$BIN\" sidebar-ensure #{window_id}'"
  fi
done

# --- focus hook: visiting a pane acknowledges its "done" state --------------
if ! tmux show-hooks -g pane-focus-in 2>/dev/null | grep -qF 'tmux-agent-hub\" focus'; then
  tmux set-hook -ga pane-focus-in "run-shell -b '\"$BIN\" focus #{hook_pane}'"
fi
