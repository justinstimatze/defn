#!/usr/bin/env bash
# Run one arm of the session-cumulative bench.
# Usage: run-arm.sh <arm-name> <workdir> <extra-claude-args...>
#   arm-name: "files" or "defn"
#   workdir:  directory to run claude -p from (chi clone)
# Env:
#   TURNS_FILE (default: turns.txt)
#   OUT_DIR (default: ./out/<arm>)
#
# Emits one raw stream-json file per turn: OUT_DIR/turn-NN.json
set -euo pipefail

ARM=${1:?arm name}
WORKDIR=${2:?workdir}
shift 2
EXTRA_ARGS=("$@")

TURNS_FILE=${TURNS_FILE:-turns.txt}
OUT_DIR=${OUT_DIR:-./out/$ARM}

mkdir -p "$OUT_DIR"

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
            "$prompt") > "$OUT_FILE" 2> "$OUT_FILE.err" || {
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
            "$prompt") > "$OUT_FILE" 2> "$OUT_FILE.err" || {
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
