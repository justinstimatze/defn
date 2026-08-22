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
//
// #312: also resets starterInjected on a new turn, so a multi-turn
// conversation gets a fresh #203 starter bundle per turn instead of
// only ever firing once, session-lifetime.
//
// #328: a token change alone isn't sufficient evidence of a genuine
// new turn -- see lastQuestion's doc comment on sessionCache for the
// real bench trajectory (prometheus-19184) that showed the hook
// re-firing with the IDENTICAL captured question mid-task. starterInjected
// only resets when the captured question actually changed; the
// circuit-breaker counters above still reset on any token change since
// a wider budget there is harmless either way.
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
		sc.pendingReadNames = nil
		sc.writeShapedCount = 0
		question := s.lastUserQuestion()
		if question == "" || question != sc.lastQuestion {
			sc.starterInjected = false
		}
		sc.lastQuestion = question
	}
}

func (s *server) circuitBreakerCheck(sc *sessionCache, op string, isBatch bool) string {
	if stripped("circuit-breaker") {
		return ""
	}
	if isBatch {
		sc.readShapedCount = 0
		sc.pendingReadNames = nil
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
			"names:[\"A\",\"B\",...], include:[...]) to batch several known defs "+
			"at once -- not another singleton %s.]",
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
// impact + 2 overview, several of them exact repeats). context/apply
// batch multiple targets into one call by design and are always
// excluded -- calling one resets the counter rather than counting
// against it. expand is included here too: a SINGLE-name expand call
// (the common case, per #210's chi-explore round-4 finding that the
// model kept calling expand one name at a time despite names:[...]
// being documented) provides no more consolidation than a lone read,
// so it counts like one; the call site only treats expand as a batch
// (resetting the counter) when 2+ names were actually requested.
//
// overview is deliberately NOT here (moved out per #212's validation
// bench, 2026-07-31): unlike read/outline, it was never a per-def
// singleton to begin with -- even a bare overview() summarizes the
// whole project, and overview(file:...) summarizes every def in that
// file in one call, optionally with a #212 narrative on top. Counting
// it as a singleton let the breaker block the one overview call that
// would have exercised #212 before it ever got a chance to fire.
var readShapedOps = map[string]bool{
	"read": true, "outline": true, "search": true,
	"impact": true, "methods": true, "expand": true,
}

// stripped reports whether feature is listed in DEFN_STRIP (a
// comma-separated env var), read fresh on every call. #180: lets a
// bench harness isolate exactly one feature's marginal cost/behavior
// (e.g. DEFN_STRIP=related-footer) instead of reverting code for a
// single-variable A/B, or conflating several response-enrichment
// features shipped this session when trying to explain a bench delta.
// Known names: starter-bundle (#203), related-footer (#202),
// circuit-breaker (#209), dedup (#77/#209). Deliberately NOT cached --
// os.Getenv is cheap and a memoized read would be wrong the moment a
// test (or a future runtime-reconfig path) changes the env mid-process.
func stripped(feature string) bool {
	for _, f := range strings.Split(os.Getenv("DEFN_STRIP"), ",") {
		if strings.TrimSpace(f) == feature {
			return true
		}
	}
	return false
}

// envDisabled reports whether the named env var is set to a falsy
// value (0, false, off, no -- case-insensitive). Unset or any other
// value means "not disabled" -- these are opt-OUT flags (#201), so
// absence is the enabled-by-default state, matching the existing
// no-ANTHROPIC_API_KEY degrade-gracefully path these flags reuse.
func envDisabled(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "0", "false", "off", "no":
		return true
	default:
		return false
	}
}

// checkCompactionEpoch compares the on-disk compaction-epoch counter
// (bumped once per PreCompact hook fire by hooks/defn-precompact.sh)
// against what this session last saw, advancing sc.compactionEpoch.
// Mirrors checkTurnBoundary's pattern for the same reason: the MCP
// server has no protocol-level signal that compaction happened, so a
// hook bumping a file is the only way to get one.
func (s *server) checkCompactionEpoch(sc *sessionCache) {
	if s.projectDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(s.projectDir, ".defn", ".compaction-epoch"))
	if err != nil {
		return
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return
	}
	if n > sc.compactionEpoch {
		sc.compactionEpoch = n
	}
}

// nameableReadOps is the subset of readShapedOps that resolve a single
// named definition -- read/outline/impact/methods by name:, or expand
// with exactly one entry in names:. A circuit-breaker block on one of
// these can redirect through expand with the names accumulated this
// turn instead of just refusing, since there's a concrete def to serve.
// search has no such resolution (pattern-based, no single target) and
// keeps the plain refusal.
var nameableReadOps = map[string]bool{
	"read": true, "outline": true, "impact": true, "methods": true, "expand": true,
}

// trackReadShapedName records name for a possible circuit-breaker
// auto-batch redirect. Called for every read-shaped call regardless of
// whether it ends up blocked, so a later block can reconstruct the
// whole turn's want-list. pendingReadNames is cleared wherever
// readShapedCount resets (batch call, new turn, or after a redirect
// consumes it); pendingBodyNames tracks the subset that were fetched
// via op:"read" specifically, so a redirect can restrict "body"
// inclusion to just those names (#279/#290) instead of applying it to
// every name in the batch.
func (s *server) trackReadShapedName(sc *sessionCache, op, name string) {
	if strings.TrimSpace(name) == "" || !nameableReadOps[op] {
		return
	}
	if op == "read" {
		already := false
		for _, n := range sc.pendingBodyNames {
			if n == name {
				already = true
				break
			}
		}
		if !already {
			sc.pendingBodyNames = append(sc.pendingBodyNames, name)
		}
	}
	for _, n := range sc.pendingReadNames {
		if n == name {
			return
		}
	}
	sc.pendingReadNames = append(sc.pendingReadNames, name)
}

// defnWriteShapedCircuitBreakerThreshold is how many individual
// apply-batchable write calls (see writeShapedOps) a turn gets before
// being nudged toward op:"apply" instead of continuing one at a time.
// Lower than the read threshold (8): unlike a read-shaped call, each of
// these independently auto-emits+builds, and a real v8 bench run found
// defn invoking the Go toolchain 82% more often than files-mode across
// a 15-task corpus (178 vs 98 calls) because of exactly this pattern.
// Tunable via DEFN_WRITE_CIRCUIT_BREAKER without a rebuild.
const defnWriteShapedCircuitBreakerThreshold = 3

// writeBatchNudge is a NON-blocking counterpart to circuitBreakerCheck.
// Unlike the read-shaped breaker, it never refuses a write -- the build
// gate's compile-and-rollback-on-failure check has to keep running on
// every single write regardless, so weakening THAT to save toolchain
// invocations would trade away the exact guarantee that makes defn's
// writes trustworthy. Instead, once a turn crosses the threshold on
// individual apply-batchable write calls without ever calling apply
// itself, it returns one informational note (appended to that call's
// own already-successful result) suggesting apply for any remaining
// edits this turn -- pure information, zero change to what actually got
// built. Fires once per crossing, not on every subsequent call, since
// unlike the read breaker it isn't blocking and doesn't need to keep
// redirecting.
func (s *server) writeBatchNudge(sc *sessionCache, op string) string {
	if stripped("write-batch-nudge") {
		return ""
	}
	if op == "apply" {
		sc.writeShapedCount = 0
		return ""
	}
	if !writeShapedOps[op] {
		return ""
	}
	sc.writeShapedCount++
	if sc.writeShapedCount != writeCircuitBreakerThreshold() {
		return ""
	}
	return fmt.Sprintf(
		"\n\n[%d individual write calls this turn, each paying its own build cost. "+
			"If you have more edits queued for this turn, code(op:\"apply\", "+
			"operations:[...]) batches them into one build instead of separate ones.]",
		sc.writeShapedCount,
	)
}

// writeShapedOps is the subset of write ops that op:"apply" can batch
// into a single build -- create, edit, delete, rename, plus the 6
// projection ops (insert-precondition, replace-slice, replace-hunk,
// wrap-in-defer, rename-param, add-import) and insert-header. Confirmed
// against handleApply's own switch, not assumed from documentation --
// move, insert, and retarget-field-value are NOT apply-batchable and
// are deliberately excluded here (nudging toward apply for those would
// send the model to a batch call that rejects them with "unknown op").
var writeShapedOps = map[string]bool{
	"edit": true, "create": true, "delete": true, "rename": true,
	"insert-precondition": true, "replace-slice": true, "replace-hunk": true,
	"wrap-in-defer": true, "rename-param": true, "add-import": true, "insert-header": true,
}

func writeCircuitBreakerThreshold() int {
	if v := os.Getenv("DEFN_WRITE_CIRCUIT_BREAKER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defnWriteShapedCircuitBreakerThreshold
}
