#!/usr/bin/env bash
# tmux-agent-hub entry point for TPM. Gets a binary — a release download or
# a local build — then wires the status bar, keys and hooks. Safe to re-run.

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$CURRENT_DIR/bin/tmux-agent-hub"
REPO="mrybas/tmux-agent-hub"

# Rebuild when anything that ends up inside the binary is newer than it:
# Go sources, embedded templates and themes, and the dependency set.
stale() {
  [ ! -x "$BIN" ] && return 0
  [ -n "$(find "$CURRENT_DIR/cmd" "$CURRENT_DIR/internal" \
      \( -name '*.go' -o -name '*.md' -o -name '*.toml' \) -newer "$BIN" 2>/dev/null | head -1)" ] && return 0
  [ "$CURRENT_DIR/go.mod" -nt "$BIN" ] || [ "$CURRENT_DIR/go.sum" -nt "$BIN" ]
}

# release_tag: the version this checkout is at, or the newest published one.
release_tag() {
  local tag
  tag=$(git -C "$CURRENT_DIR" describe --tags --abbrev=0 2>/dev/null) && [ -n "$tag" ] && { printf '%s' "$tag"; return; }
  # no tags locally (shallow clone): follow the /releases/latest redirect
  fetch "https://github.com/$REPO/releases/latest" --head 2>/dev/null |
    sed -n 's|.*/releases/tag/\([^[:space:]]*\).*|\1|p' | tr -d '\r' | tail -1
}

# fetch URL [--head] -> stdout, with whichever downloader exists.
fetch() {
  if command -v curl >/dev/null 2>&1; then
    if [ "${2:-}" = "--head" ]; then curl -sIL "$1"; else curl -fsSL "$1"; fi
  elif command -v wget >/dev/null 2>&1; then
    if [ "${2:-}" = "--head" ]; then wget -qS --spider "$1" 2>&1; else wget -qO- "$1"; fi
  else
    return 1
  fi
}

# download_release puts a published binary in place, checksum-verified.
download_release() {
  local os arch tag tmp asset sums
  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *) return 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) return 1 ;;
  esac
  tag=$(release_tag) || return 1
  [ -n "$tag" ] || return 1

  asset="tmux-agent-hub_${tag}_${os}_${arch}.tar.gz"
  tmp=$(mktemp -d) || return 1
  trap 'rm -rf "$tmp"' RETURN

  fetch "https://github.com/$REPO/releases/download/$tag/$asset" > "$tmp/$asset" || return 1
  [ -s "$tmp/$asset" ] || return 1

  # verify against the release's own checksum file before running anything
  if sums=$(fetch "https://github.com/$REPO/releases/download/$tag/SHA256SUMS"); then
    local want got
    want=$(printf '%s\n' "$sums" | awk -v a="$asset" '$2 == a || $2 == "*"a {print $1}')
    if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
    fi
    if [ -n "$want" ] && [ -n "${got:-}" ] && [ "$want" != "$got" ]; then
      tmux display-message "tmux-agent-hub: checksum mismatch on $asset, not installing"
      return 1
    fi
  fi

  tar -xzf "$tmp/$asset" -C "$tmp" || return 1
  mkdir -p "$CURRENT_DIR/bin"
  mv "$tmp/tmux-agent-hub_${tag}_${os}_${arch}/tmux-agent-hub" "$BIN" || return 1
  chmod +x "$BIN"
}

if stale; then
  # A checkout with local changes is a development one: build it. Everyone
  # else gets the published binary, because Go is not installed everywhere.
  if command -v go >/dev/null 2>&1; then
    (cd "$CURRENT_DIR" && make -s build) \
      || tmux display-message "tmux-agent-hub: go build failed, see 'make build'"
  elif download_release; then
    :
  elif [ ! -x "$BIN" ]; then
    tmux display-message "tmux-agent-hub: could not download a release binary — check the network, or install Go and reload"
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
