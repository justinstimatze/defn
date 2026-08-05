#!/bin/bash
# Claude Code hook: PreCompact -- fires before conversation compaction
# (manual /compact or automatic when context fills).
#
# The MCP server has no protocol-level signal that compaction happened --
# same gap defn-capture-question.sh already works around for turn
# boundaries via .turn-token. dedup/subsumption in internal/mcp/dedup.go
# need the equivalent signal for compaction specifically: a compacted
# summary can silently drop content a cache entry still vouches the
# caller "already has", so entries need to know how many compactions
# they've survived. Bumping an integer here, once per fire, is that
# signal -- checkCompactionEpoch (turn_state.go) reads it the same way
# checkTurnBoundary reads .turn-token.

[ -d .defn ] || exit 0

n=$(cat .defn/.compaction-epoch 2>/dev/null)
case "$n" in ''|*[!0-9]*) n=0 ;; esac
echo $((n + 1)) > .defn/.compaction-epoch
exit 0
