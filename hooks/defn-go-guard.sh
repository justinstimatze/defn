#!/bin/bash
# Claude Code hook: block native Read/Bash-dump/Edit/Write on .go files.
# Place in .claude/settings.json hooks.PreToolUse, matcher "Read|Bash|Write|Edit|MultiEdit".
#
# CLAUDE.md already tells the model to use the `code` MCP tool for Go files
# instead of Read/Bash/Edit — this hook enforces it at the harness level
# instead of relying on the model to comply. Escape hatch: a sentinel file
# at ~/.claude-allow-go-edit lets a human (or a deliberate agent step)
# bypass the guard for the rare legitimate case (the sentinel is removed
# after use, by convention — this hook does not manage it).
#
# Inspired by TokenMiser's publicly-described "Bash read-guard" concept
# (tokenmiser.ai) — implemented independently here, no code shared.

SENTINEL="$HOME/.claude-allow-go-edit"
if [ -f "$SENTINEL" ]; then
    exit 0
fi

INPUT=$(cat)
TOOL=$(echo "$INPUT" | jq -r '.tool_name // empty')

deny() {
    echo "$1" >&2
    exit 2
}

case "$TOOL" in
Read)
    FILE=$(echo "$INPUT" | jq -r '.tool_input.file_path // empty')
    if [[ "$FILE" == *.go ]]; then
        deny "Use code(op:\"read\", name:\"...\") instead of Read for .go files — defn keeps the ref graph consistent and returns a smaller, related-defs-aware body. (Escape hatch: touch ~/.claude-allow-go-edit if you have a specific reason to bypass this.)"
    fi
    ;;
Write|Edit|MultiEdit)
    FILES=$(echo "$INPUT" | jq -r '
        [.tool_input.file_path?, (.tool_input.edits? // [] | .[] .file_path?)]
        | flatten | map(select(. != null)) | .[]')
    while IFS= read -r FILE; do
        [ -z "$FILE" ] && continue
        if [[ "$FILE" == *.go ]]; then
            deny "Use code(op:\"edit\"/\"create\"/\"apply\", ...) instead of $TOOL for .go files — this keeps defn's reference graph in sync. (Escape hatch: touch ~/.claude-allow-go-edit if you have a specific reason to bypass this.)"
        fi
    done <<<"$FILES"
    ;;
Bash)
    CMD=$(echo "$INPUT" | jq -r '.tool_input.command // empty')
    # Only block content-DUMP commands on .go files (cat/head/tail/less/more/bat).
    # Deliberately NOT blocking grep/go build/go test/git — those are either
    # already permitted workflows or not full-body reads.
    if echo "$CMD" | grep -Eq '(^|[;&|]|\s)(cat|head|tail|less|more|bat)\s[^;&|]*\.go(\s|$)'; then
        deny "Use code(op:\"read\", name:\"file:line\") instead of shelling out to dump .go file contents — defn returns the def body plus related-defs context in one call. (Escape hatch: touch ~/.claude-allow-go-edit if you have a specific reason to bypass this.)"
    fi
    ;;
esac

exit 0
