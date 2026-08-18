#!/usr/bin/env bash
# dev-reload.sh — rebuild defn and bounce the live serve process in one
# step, instead of two separate commands run by hand.
#
# Why this exists: defn's auto-sharing HTTP architecture means the MCP
# client can reconnect and silently re-attach to an already-running,
# stale serve process rather than spawning a fresh one from a just-
# rebuilt binary — `defn restart` handles the actual bounce correctly
# (kills the daemon, respawns, confirms the new version), but a plain
# `go build` without it leaves the old process running unaffected.
# Chaining them here removes the chance of doing the build and
# forgetting the restart, or restarting against a binary that never
# actually got rebuilt.
#
# Restarting severs any live MCP client connection to this project's
# serve (including the one this Claude Code session may be using) --
# that side of the reconnect still needs a human to run /mcp again;
# nothing run from a sandboxed shell can do that part.
#
# Usage: scripts/dev-reload.sh [module-path]  (default ./cmd/defn)

set -euo pipefail

MODULE="${1:-./cmd/defn}"
BIN="${GOBIN:-$HOME/go/bin}/defn"

echo "[dev-reload] building $MODULE -> $BIN" >&2
go build -o "$BIN" "$MODULE"

echo "[dev-reload] restarting serve" >&2
"$BIN" restart
