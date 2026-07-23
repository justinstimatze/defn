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

mkdir -p "$OUT_DIR"

# #180 / #174 plumbing: capture defn serve's stderr JSONL for this arm.
# Default on for any arm whose name contains "defn"; files-only arms
# don't touch defn serve so nothing would be written.
DEFN_CAPTURE_USAGE=${DEFN_CAPTURE_USAGE:-$([[ "$ARM" == *defn* ]] && echo 1 || echo 0)}
if [ "$DEFN_CAPTURE_USAGE" = "1" ]; then
    USAGE_LOG="$(realpath "$OUT_DIR")/defn-usage.jsonl"
    : > "$USAGE_LOG"
    echo "[$ARM] capturing defn usage to $USAGE_LOG (bouncing serve)"
    DEFN_USAGE_LOG_FILE="$USAGE_LOG" defn restart >/dev/null 2>&1 || {
        echo "[$ARM] defn restart failed — usage log will be empty" >&2
    }
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
