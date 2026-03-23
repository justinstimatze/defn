#!/bin/bash
# Claude Code hook: auto-initialize defn on session start.
# Place in .claude/settings.json hooks.SessionStart
#
# If the project has a go.mod but no .defn/ database, runs defn init.
# If .defn/ exists but is stale (go.mod newer), re-ingests.

if [ ! -f "go.mod" ]; then
    exit 0  # Not a Go project.
fi

DEFN_BIN=$(which defn 2>/dev/null)
if [ -z "$DEFN_BIN" ]; then
    exit 0  # defn not installed.
fi

if [ ! -d ".defn" ]; then
    # First time: full init.
    DEFN_DB=.defn "$DEFN_BIN" init . >&2
elif [ "go.mod" -nt ".defn/.dolt/repo_state.json" ]; then
    # go.mod is newer than the database — re-ingest.
    DEFN_DB=.defn "$DEFN_BIN" ingest . >&2
fi
