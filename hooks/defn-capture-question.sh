#!/bin/bash
# Claude Code hook: UserPromptSubmit -- fires once per user turn.
#
# #209: two things defn's MCP server needs and can't get from tool args
# alone.
#
# 1. The real question. Every defn op only sees its own args (a name, a
#    pattern, or nothing) -- never the natural-language prompt that
#    prompted the call. The #203 starter bundle used to fall back to a
#    hardcoded "project structure" placeholder when called with no args
#    (e.g. a bare `overview`), which returned content unrelated to what
#    was actually asked and got correctly ignored -- at full token cost.
#    Stashing the raw prompt here lets appendStarter/handleContext use
#    the real intent instead of guessing.
#
# 2. A turn boundary. The MCP server has no built-in signal for "a new
#    turn started" -- session state persists across the whole
#    conversation. The circuit breaker (internal/mcp/turn_state.go)
#    needs to reset its per-turn call counter each turn rather than
#    accumulating across the whole session; bumping a token here once
#    per prompt gives it that signal cheaply.
#
# Writes into .defn/ in the current working directory (same scope as
# the project's own database). No-op if .defn/ doesn't exist -- not a
# defn project, or defn not yet initialized.

[ -d .defn ] || exit 0

INPUT=$(cat)
PROMPT=$(echo "$INPUT" | jq -r '.prompt // .user_prompt // .message // empty')
[ -n "$PROMPT" ] && printf '%s' "$PROMPT" > .defn/.last-question

date +%s%N > .defn/.turn-token
exit 0
