package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defnReadShapedCircuitBreakerThreshold is how many individual
// read-shaped calls (read/outline/search/impact/overview/methods
// naming a specific target) a turn gets before being told to batch via
// context/expand/apply instead of continuing one call at a time. #209:
// chi-explore turn 1 made 44 such calls before ever calling context --
// 8 is generous enough for a genuinely multi-part question's core
// investigation (turn 1's ServeHTTP/routeHTTP chain took ~10 calls to
// map on its own) while tight enough to interrupt before a sprawl.
// Tunable via DEFN_CIRCUIT_BREAKER without a rebuild.
const defnReadShapedCircuitBreakerThreshold = 8

func circuitBreakerThreshold() int {
	if v := os.Getenv("DEFN_CIRCUIT_BREAKER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defnReadShapedCircuitBreakerThreshold
}

// checkTurnBoundary compares the on-disk turn-token (bumped once per
// user prompt by the same hook) against what this session last saw. A
// changed token means a new turn started, so the read-shaped circuit
// breaker counter resets -- each turn gets its own budget instead of
// accumulating across the whole session.
func (s *server) checkTurnBoundary(sc *sessionCache) {
	if s.projectDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(s.projectDir, ".defn", ".turn-token"))
	if err != nil {
		return
	}
	token := strings.TrimSpace(string(data))
	if token != "" && token != sc.turnToken {
		sc.turnToken = token
		sc.readShapedCount = 0
	}
}

// circuitBreakerCheck increments the per-turn read-shaped call counter
// and returns a non-empty refusal message once the threshold is
// exceeded, nudging the model toward context/expand/apply instead of
// continuing one call at a time. Calling a batching op resets the
// counter instead of incrementing it -- it's the intended escape
// valve, not more of the problem.
func (s *server) circuitBreakerCheck(sc *sessionCache, op string) string {
	if op == "context" || op == "expand" || op == "apply" {
		sc.readShapedCount = 0
		return ""
	}
	if !readShapedOps[op] {
		return ""
	}
	sc.readShapedCount++
	if sc.readShapedCount <= circuitBreakerThreshold() {
		return ""
	}
	return fmt.Sprintf(
		"[circuit breaker: %d individual lookups this turn without batching. "+
			"Use code(op:\"context\", question:\"...\") to bundle the rest of "+
			"this turn's exploration into one call, or code(op:\"expand\", "+
			"name:\"...\", include:[...]) for a known def's related info -- "+
			"not another singleton %s.]",
		sc.readShapedCount-1, op,
	)
}

// lastUserQuestion returns the most recent raw user prompt stashed by
// hooks/defn-capture-question.sh (#209) on UserPromptSubmit, or "" if
// unavailable -- no hook wired, fresh project, or nothing written yet.
func (s *server) lastUserQuestion() string {
	if s.projectDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.projectDir, ".defn", ".last-question"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readShapedOps fetch information about one named/searched target at a
// time -- the ones that sprawled into a 44-call binge in #209's
// chi-explore bench (turn 1: 21 read + 12 outline + 5 search + 3
// impact + 2 overview, several of them exact repeats). context/expand/
// apply batch multiple targets into one call and are deliberately
// excluded -- calling one resets the counter rather than counting
// against it.
var readShapedOps = map[string]bool{
	"read": true, "outline": true, "search": true,
	"impact": true, "overview": true, "methods": true,
}
