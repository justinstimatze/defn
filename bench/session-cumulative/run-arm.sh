#!/usr/bin/env bash
# Run one arm of the session-cumulative bench.
# Usage: run-arm.sh <arm-name> <workdir> <extra-claude-args...>
#   arm-name: "files" or "defn"
#   workdir:  directory to run claude -p from (chi clone)
# Env:
#   TURNS_FILE (default: turns.txt)
#   OUT_DIR (default: ./out/<arm>)
#   DEFN_CAPTURE_USAGE (default: 1 for arms containing "defn"): bounce
#     the ambient defn serve with DEFN_USAGE_LOG_FILE pointed at
#     OUT_DIR/defn-usage.jsonl so per-op stats (#177) are captured
#     alongside the stream-json turn files. Set to 0 to skip.
#
# Emits one raw stream-json file per turn: OUT_DIR/turn-NN.json
# Also captures OUT_DIR/defn-usage.jsonl when DEFN_CAPTURE_USAGE=1.
set -euo pipefail

ARM=${1:?arm name}
WORKDIR=${2:?workdir}
shift 2
EXTRA_ARGS=("$@")

TURNS_FILE=${TURNS_FILE:-turns.txt}
OUT_DIR=${OUT_DIR:-./out/$ARM}

# 2026-08-07: turn-1-gaming persisted even after decoupling turn 1's topic
# from the task (see 2026-08-07-turn1-gaming-and-crg.md) -- every arm,
# files-mode included, front-loaded the whole 10-turn task into turn 1
# regardless of prompt content. This is a blunt countermeasure: tell the
# model directly not to do that. Applied on every invocation (not just
# turn 1) since --append-system-prompt's persistence across --resume is
# unverified -- cheaper to just always pass it than to check.
TURN_BUDGET_PROMPT='This is one turn in a longer, incremental multi-turn session. Only do what THIS message asks -- nothing more. Do not implement, test, or otherwise act on functionality that later messages might request, even if you can infer what a natural next step would be. Further instructions will follow in later messages; stop when this message'"'"'s request is done.'

mkdir -p "$OUT_DIR"

# #180 / #174 plumbing: capture defn serve's stderr JSONL for this arm.
# Two things are needed for the JSONL to actually get written:
# 1. The binary at /home/justin/go/bin/defn must know about
#    DEFN_USAGE_LOG_FILE — reinstall from source below.
# 2. The HTTP serve for this bench's DB must be started with our env
#    in scope — kill any stale serve for this DB so claude's first
#    MCP spawn actually starts a fresh one instead of proxying to a
#    long-lived one that predates our exports (#185 root cause).
DEFN_CAPTURE_USAGE=${DEFN_CAPTURE_USAGE:-$([[ "$ARM" == *defn* ]] && echo 1 || echo 0)}
if [ "$DEFN_CAPTURE_USAGE" = "1" ]; then
    USAGE_LOG="$(realpath "$OUT_DIR")/defn-usage.jsonl"
    : > "$USAGE_LOG"
    export DEFN_USAGE_LOG_FILE="$USAGE_LOG"
    echo "[$ARM] capturing defn usage to $USAGE_LOG"

    # #185: bench's mcp-defn.json points at /home/justin/go/bin/defn.
    # If that binary is stale, env-var plumbing / new server features
    # are silently absent — the 2026-07-23 chi-explore rerun had an
    # empty usage log for exactly this reason. Reinstall from the
    # source tree here so the bench always spawns a matching binary.
    # Repo root is inferred from this script's location so this works
    # from any cwd; skip with DEFN_SKIP_INSTALL=1 if the caller has
    # already installed.
    if [ "${DEFN_SKIP_INSTALL:-0}" != "1" ]; then
        REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
        echo "[$ARM] installing fresh defn from $REPO_ROOT"
        (cd "$REPO_ROOT" && go install ./cmd/defn) || {
            echo "[$ARM] WARNING: go install ./cmd/defn failed; bench will use stale binary" >&2
        }
    fi

    # #185 root cause: `defn serve` auto-shares HTTP endpoints across
    # processes with the same DB path. When claude MCP spawns a child
    # `defn serve`, the child either starts a fresh HTTP serve (which
    # inherits our exported env, including DEFN_USAGE_LOG_FILE) OR
    # becomes a lightweight stdio proxy to an existing HTTP serve. If
    # a stale HTTP serve is still running for this bench's DB from a
    # prior test, turn-1's env never reaches the handler process and
    # the usage log stays empty. Kill any such stale serve here so
    # claude's first spawn starts fresh with our env in scope.
    LOCKFILE="$WORKDIR/.defn/serve.pid"
    if [ -f "$LOCKFILE" ]; then
        STALE_PID=$(python3 -c "import json,sys; d=json.load(open('$LOCKFILE')); print(d.get('PID') or d.get('pid') or '')" 2>/dev/null || echo "")
        if [ -n "$STALE_PID" ] && kill -0 "$STALE_PID" 2>/dev/null; then
            echo "[$ARM] killing stale defn serve PID $STALE_PID for $WORKDIR/.defn"
            kill "$STALE_PID" 2>/dev/null || true
            for i in 1 2 3 4 5 6 7 8; do
                sleep 0.25
                kill -0 "$STALE_PID" 2>/dev/null || break
            done
        fi
    fi
fi

# Pre-generate a session ID (UUID v4)
SESSION_ID=$(uuidgen | tr 'A-Z' 'a-z')
echo "[$ARM] session-id: $SESSION_ID"
echo "$SESSION_ID" > "$OUT_DIR/session-id.txt"

TURN=0
while IFS= read -r prompt; do
    TURN=$((TURN + 1))
    OUT_FILE=$(printf "%s/turn-%02d.json" "$OUT_DIR" "$TURN")
    echo
    echo "[$ARM] turn $TURN: $(echo "$prompt" | head -c 100)..."

    if [ "$TURN" -eq 1 ]; then
        # First turn: seed the session
        (cd "$WORKDIR" && claude -p \
            --session-id "$SESSION_ID" \
            --output-format stream-json --verbose \
            --dangerously-skip-permissions \
            --strict-mcp-config \
            --append-system-prompt "$TURN_BUDGET_PROMPT" \
            "${EXTRA_ARGS[@]}" \
            -- "$prompt") > "$OUT_FILE" 2> "$OUT_FILE.err" || {
            echo "[$ARM] turn $TURN FAILED, stderr:"
            head -20 "$OUT_FILE.err" >&2
            exit 1
        }
    else
        # Subsequent turns: --resume
        (cd "$WORKDIR" && claude -p \
            --resume "$SESSION_ID" \
            --output-format stream-json --verbose \
            --dangerously-skip-permissions \
            --strict-mcp-config \
            --append-system-prompt "$TURN_BUDGET_PROMPT" \
            "${EXTRA_ARGS[@]}" \
            -- "$prompt") > "$OUT_FILE" 2> "$OUT_FILE.err" || {
            echo "[$ARM] turn $TURN FAILED, stderr:"
            head -20 "$OUT_FILE.err" >&2
            exit 1
        }
    fi
    LINES=$(wc -l < "$OUT_FILE")
    echo "[$ARM] turn $TURN done, $LINES stream-json lines"
done < "$TURNS_FILE"

echo
echo "[$ARM] all turns done"
