#!/bin/bash
# Claude Code hook: block native Read/Bash-dump/Edit/Write on .go files.
# Place in .claude/settings.json hooks.PreToolUse, matcher "Read|Bash|Write|Edit|MultiEdit".
#
# CLAUDE.md already tells the model to use the `code` MCP tool for Go files
# instead of Read/Bash/Edit — this hook enforces it at the harness level
# instead of relying on the model to comply.
#
# THREAT MODEL: this is an adoption-nudge for a cooperative-but-habitual
# model, NOT a security sandbox against an adversarial one. Regex-based
# classification of shell command text cannot be made complete — quote
# splitting (server.g''o), command substitution ($(echo .go)), base64+eval,
# or an interpreter/utility not on our list can all defeat it. We cover the
# common, unobfuscated cases that dominate real usage and stop there
# deliberately; do not treat this as a hard boundary.
#
# Escape hatch: a sentinel file at ~/.claude-allow-go-edit permits ONE
# bypass and is deleted by this script immediately on use (single-use,
# not "bypass until manually rm'd" — a prior version left an unattended
# sentinel as a standing, self-servable disable switch, which defeated the
# point of moving enforcement out of the model's hands). The exact
# touch-this-file mechanism is deliberately not repeated in the denial
# message shown to the model on every block, to avoid the hook teaching
# its own bypass as routine friction-relief; see CLAUDE.md/this comment
# for the human-facing instructions.
#
# Inspired by TokenMiser's publicly-described "Bash read-guard" concept
# (tokenmiser.ai marketing copy only) — implemented independently here,
# no code shared, no code borrowed.
#
# #207: DEFN_GUARD=0 disables this hook entirely for the current shell
# environment (e.g. `DEFN_GUARD=0 claude` for a whole session, or export
# it for a debugging shell) — a project-wide, non-single-use escape
# distinct from the ~/.claude-allow-go-edit sentinel below, which is
# scoped to exactly one blocked call. Use this when the guard itself is
# what you're working on/around, not as a routine bypass.
if [ "${DEFN_GUARD:-1}" = "0" ]; then
    exit 0
fi

SENTINEL="$HOME/.claude-allow-go-edit"

INPUT=$(cat)
TOOL=$(echo "$INPUT" | jq -r '.tool_name // empty')
VIOLATION=""

deny() {
    if [ -f "$SENTINEL" ]; then
        rm -f "$SENTINEL"
        echo "[defn-go-guard] sentinel consumed (single-use) — bypassing: $1" >&2
        exit 0
    fi
    echo "$1 (Human-authorized bypass exists; see CLAUDE.md.)" >&2
    exit 2
}

case "$TOOL" in
Read)
    FILE=$(echo "$INPUT" | jq -r '.tool_input.file_path // empty')
    if [[ "$FILE" == *.go ]]; then
        deny "Use code(op:\"read\", name:\"...\") instead of Read for .go files — defn keeps the ref graph consistent and returns a smaller, related-defs-aware body."
    fi
    ;;
Write|Edit|MultiEdit)
    FILES=$(echo "$INPUT" | jq -r '
        [.tool_input.file_path?, (.tool_input.edits? // [] | .[] .file_path?)]
        | flatten | map(select(. != null)) | .[]')
    while IFS= read -r FILE; do
        [ -z "$FILE" ] && continue
        if [[ "$FILE" == *.go ]]; then
            deny "Use code(op:\"edit\"/\"create\"/\"apply\", ...) instead of $TOOL for .go files — this keeps defn's reference graph in sync."
        fi
    done <<<"$FILES"
    ;;
Bash)
    CMD=$(echo "$INPUT" | jq -r '.tool_input.command // empty')
    # Content-DUMP utilities on .go files. Deliberately NOT blocking grep/go
    # build/go test/git — those are either already-permitted workflows or
    # not full-body reads. \.go\b (not "\.go(\s|$)") so a trailing quote,
    # paren, or bracket right after the extension — e.g. inside a
    # `python3 -c "...server.go').read()..."` one-liner — still counts as
    # a word boundary and doesn't slip past a naive whitespace-only anchor.
    if echo "$CMD" | grep -Eq '(^|[;&|]|\s)(cat|head|tail|less|more|bat|awk|sed|strings|xxd|od|hexdump|dd|nl|tac|python3?|perl|node|ruby)\s[^;&|]*\.go\b'; then
        deny "Use code(op:\"read\", name:\"file:line\") instead of shelling out to dump .go file contents — defn returns the def body plus related-defs context in one call."
    fi
    ;;
esac

exit 0
