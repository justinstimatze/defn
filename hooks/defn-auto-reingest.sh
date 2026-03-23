#!/bin/bash
# Claude Code hook: re-ingest after file edits (for legacy mode).
# Place in .claude/settings.json hooks.PostToolUse for Edit/Write tools.
#
# Only runs if .defn/ exists and DEFN_LEGACY=1.

if [ "$DEFN_LEGACY" != "1" ] || [ ! -d ".defn" ]; then
    exit 0
fi

DEFN_BIN=$(which defn 2>/dev/null)
if [ -z "$DEFN_BIN" ]; then
    exit 0
fi

DEFN_DB=.defn "$DEFN_BIN" ingest . >&2 2>/dev/null
