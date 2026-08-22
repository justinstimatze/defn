package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCheckTurnBoundary_ResetsCounterOnNewToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".defn"), 0755); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, ".defn", ".turn-token")
	s := &server{projectDir: dir}
	sc := &sessionCache{entries: map[string]cacheEntry{}}

	if err := os.WriteFile(tokenPath, []byte("turn-1"), 0644); err != nil {
		t.Fatal(err)
	}
	s.checkTurnBoundary(sc)
	sc.readShapedCount = 5

	// Same token again -- same turn, counter must NOT reset.
	s.checkTurnBoundary(sc)
	if sc.readShapedCount != 5 {
		t.Fatalf("same turn-token should not reset counter, got %d", sc.readShapedCount)
	}

	if err := os.WriteFile(tokenPath, []byte("turn-2"), 0644); err != nil {
		t.Fatal(err)
	}
	s.checkTurnBoundary(sc)
	if sc.readShapedCount != 0 {
		t.Fatalf("new turn-token should reset counter, got %d", sc.readShapedCount)
	}
}

func TestCircuitBreaker_AllowsUpToThreshold(t *testing.T) {
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	for i := 1; i <= defnReadShapedCircuitBreakerThreshold; i++ {
		if msg := s.circuitBreakerCheck(sc, "read", false); msg != "" {
			t.Fatalf("call %d: expected no refusal within threshold, got %q", i, msg)
		}
	}
	msg := s.circuitBreakerCheck(sc, "read", false)
	if msg == "" {
		t.Fatal("expected refusal once threshold exceeded")
	}
	if !strings.Contains(msg, "circuit breaker") {
		t.Errorf("refusal message missing marker: %q", msg)
	}
}

func TestCircuitBreaker_BatchOpsReset(t *testing.T) {
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	for i := 0; i < defnReadShapedCircuitBreakerThreshold; i++ {
		s.circuitBreakerCheck(sc, "outline", false)
	}
	if msg := s.circuitBreakerCheck(sc, "context", true); msg != "" {
		t.Fatalf("batching op itself should never be refused, got %q", msg)
	}
	if sc.readShapedCount != 0 {
		t.Fatalf("context call should reset counter, got %d", sc.readShapedCount)
	}
	if msg := s.circuitBreakerCheck(sc, "read", false); msg != "" {
		t.Fatalf("expected fresh budget after batching reset, got refusal %q", msg)
	}
}

func TestCircuitBreaker_EnvOverride(t *testing.T) {
	t.Setenv("DEFN_CIRCUIT_BREAKER", "2")
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	s.circuitBreakerCheck(sc, "read", false)
	s.circuitBreakerCheck(sc, "read", false)
	if msg := s.circuitBreakerCheck(sc, "read", false); msg == "" {
		t.Fatal("expected refusal at lowered threshold=2")
	}
}

func TestCircuitBreaker_IgnoresNonReadShapedOps(t *testing.T) {
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	for i := 0; i < defnReadShapedCircuitBreakerThreshold+5; i++ {
		if msg := s.circuitBreakerCheck(sc, "edit", false); msg != "" {
			t.Fatalf("write ops must never be refused by the read-shaped breaker, got %q", msg)
		}
	}
	if sc.readShapedCount != 0 {
		t.Errorf("non-read-shaped ops should not increment the counter, got %d", sc.readShapedCount)
	}
}

func TestDedup_LoweredFloorCatchesSmallRepeat(t *testing.T) {
	// #209: a real auto-downgrade note (~300 bytes) repeated verbatim
	// used to slip past the old 512-byte floor entirely -- this is the
	// exact shape that let Router get read 3x in the chi-explore bench
	// (call #18 and #42 both returned the identical un-actionable
	// downgrade note before a 3rd attempt with full:true finally worked).
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	note := strings.Repeat("x", 300)

	c.dedup(sess, "read", "Router", mkText(note))
	r := c.dedup(sess, "read", "Router", mkText(note))
	got := r.Content[0].(*sdkmcp.TextContent).Text
	if !strings.Contains(got, "cached") {
		t.Errorf("300-byte exact repeat should now be caught (floor=200), got %q", got)
	}
	if !strings.Contains(got, "full:true") {
		t.Errorf("stub should hint at full:true for outline/size-only repeats, got %q", got)
	}
}

func TestLastUserQuestion_EmptyWhenMissing(t *testing.T) {
	s := &server{projectDir: t.TempDir()}
	if got := s.lastUserQuestion(); got != "" {
		t.Errorf("expected empty string when no file stashed, got %q", got)
	}
}

func TestLastUserQuestion_ReadsStashedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".defn"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".defn", ".last-question"), []byte("how does routing work?\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &server{projectDir: dir}
	if got := s.lastUserQuestion(); got != "how does routing work?" {
		t.Errorf("got %q, want trimmed stashed question", got)
	}
}

func TestCircuitBreaker_MultiNameExpandResets(t *testing.T) {
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	for i := 0; i < defnReadShapedCircuitBreakerThreshold; i++ {
		s.circuitBreakerCheck(sc, "read", false)
	}
	// A genuine multi-name expand (isBatch=true, computed by the caller
	// from len(args.Names) >= 2) resets the counter like context/apply.
	if msg := s.circuitBreakerCheck(sc, "expand", true); msg != "" {
		t.Fatalf("multi-name expand should never be refused, got %q", msg)
	}
	if sc.readShapedCount != 0 {
		t.Fatalf("multi-name expand should reset counter, got %d", sc.readShapedCount)
	}
}

func TestCircuitBreaker_SingleNameExpandCountsAsSingleton(t *testing.T) {
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	// #210: chi-explore round 4 showed the model calling expand one name
	// at a time despite names:[...] being documented -- a single-name
	// expand call must count against the breaker like any other
	// singleton, or it silently defeats the whole mechanism by resetting
	// the counter on every call.
	for i := 1; i <= defnReadShapedCircuitBreakerThreshold; i++ {
		if msg := s.circuitBreakerCheck(sc, "expand", false); msg != "" {
			t.Fatalf("call %d: expected no refusal within threshold, got %q", i, msg)
		}
	}
	if msg := s.circuitBreakerCheck(sc, "expand", false); msg == "" {
		t.Fatal("expected refusal once threshold exceeded by single-name expand calls")
	}
}

func TestCircuitBreakerCheck_StrippedDisablesEntirely(t *testing.T) {
	t.Setenv("DEFN_STRIP", "circuit-breaker")
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	for i := 0; i < defnReadShapedCircuitBreakerThreshold+10; i++ {
		if msg := s.circuitBreakerCheck(sc, "read", false); msg != "" {
			t.Fatalf("call %d: DEFN_STRIP=circuit-breaker should disable the breaker entirely, got %q", i, msg)
		}
	}
}

func TestStripped_EmptyEnvMeansNothingStripped(t *testing.T) {
	t.Setenv("DEFN_STRIP", "")
	if stripped("related-footer") {
		t.Error("empty DEFN_STRIP should strip nothing")
	}
}

func TestStripped_ReadsEnvFresh(t *testing.T) {
	t.Setenv("DEFN_STRIP", "foo,bar")
	if !stripped("foo") {
		t.Error("expected foo to be stripped")
	}
	if !stripped("bar") {
		t.Error("expected bar to be stripped")
	}
	if stripped("baz") {
		t.Error("baz was not in DEFN_STRIP, should not be stripped")
	}

	// Re-set to a different value in the SAME test -- stripped() must
	// re-read the env each call, not memoize the first result (a prior
	// version used sync.OnceValue and would have failed this).
	t.Setenv("DEFN_STRIP", "baz")
	if stripped("foo") {
		t.Error("foo should no longer be stripped after DEFN_STRIP changed")
	}
	if !stripped("baz") {
		t.Error("expected baz to be stripped after DEFN_STRIP changed")
	}
}

func TestCircuitBreaker_OverviewNeverCountsAsSingleton(t *testing.T) {
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	// #212 validation bench (2026-07-31): the one overview(file:...) call
	// that would have exercised the file-narrative feature got refused
	// by the breaker because overview was counted as a read-shaped
	// singleton. overview always consolidates (whole project, or every
	// def in a file) -- it must never trip the breaker, and calling it
	// resets the counter like context/apply.
	for i := 0; i < defnReadShapedCircuitBreakerThreshold+10; i++ {
		if msg := s.circuitBreakerCheck(sc, "overview", true); msg != "" {
			t.Fatalf("call %d: overview must never be refused, got %q", i, msg)
		}
	}
	if sc.readShapedCount != 0 {
		t.Fatalf("overview should reset the counter each call, got %d", sc.readShapedCount)
	}
}

func TestCheckCompactionEpoch_AdvancesOnFileChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".defn"), 0755); err != nil {
		t.Fatal(err)
	}
	epochPath := filepath.Join(dir, ".defn", ".compaction-epoch")
	s := &server{projectDir: dir}
	sc := &sessionCache{entries: map[string]cacheEntry{}}

	if err := os.WriteFile(epochPath, []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}
	s.checkCompactionEpoch(sc)
	if sc.compactionEpoch != 1 {
		t.Fatalf("expected compactionEpoch=1, got %d", sc.compactionEpoch)
	}

	// Same value again -- must not regress or error.
	s.checkCompactionEpoch(sc)
	if sc.compactionEpoch != 1 {
		t.Fatalf("re-reading same epoch value should not change it, got %d", sc.compactionEpoch)
	}

	if err := os.WriteFile(epochPath, []byte("3"), 0644); err != nil {
		t.Fatal(err)
	}
	s.checkCompactionEpoch(sc)
	if sc.compactionEpoch != 3 {
		t.Fatalf("expected compactionEpoch to advance to 3, got %d", sc.compactionEpoch)
	}
}

func TestCheckCompactionEpoch_MissingFileIsNoOp(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".defn"), 0755); err != nil {
		t.Fatal(err)
	}
	s := &server{projectDir: dir}
	sc := &sessionCache{entries: map[string]cacheEntry{}, compactionEpoch: 5}
	s.checkCompactionEpoch(sc)
	if sc.compactionEpoch != 5 {
		t.Fatalf("missing epoch file should be a no-op, got %d", sc.compactionEpoch)
	}
}

// TestHandleCode_CircuitBreakerAutoBatchesInsteadOfRefusing is #312:
// the breaker's auto-batch hijack (and its bare-refusal fallback) is
// gone -- both were reactive nudges the project's own prior measurement
// found had 0/19 follow-through (the model never adapted afterward, it
// just recovered cost after the fact), and one auto-batch was observed
// bundling in unrelated names' full outlines for a call that was
// already narrowly scoped. The breaker is instrumentation-only now
// (see mcpDebugf): tripping it must NOT change the response at all --
// a call past threshold still returns its own real, untouched content,
// same as if the breaker didn't exist.
func TestHandleCode_CircuitBreakerAutoBatchesInsteadOfRefusing(t *testing.T) {
	t.Setenv("DEFN_CIRCUIT_BREAKER", "2")
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
	s.ready.Store(true)
	req := &sdkmcp.CallToolRequest{Session: &sdkmcp.ServerSession{}}

	for _, name := range []string{"Greet", "Farewell"} {
		r, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: name})
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(resultText(t, r), "circuit breaker") {
			t.Fatalf("read %s should be within threshold, got: %s", name, resultText(t, r))
		}
	}

	// Third read is past threshold=2 -- must still return main's own real
	// content, not an auto-batch note and not a bare refusal.
	third, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "main"})
	if err != nil {
		t.Fatalf("third read: %v", err)
	}
	text := resultText(t, third)
	if strings.Contains(text, "auto-batched") || strings.Contains(text, "circuit breaker") {
		t.Fatalf("breaker is instrumentation-only now -- tripping it must not alter the response, got: %s", text)
	}
	if !strings.Contains(text, "func main()") {
		t.Fatalf("expected main's own real content past threshold, got: %s", text)
	}

	// Fourth read, still past threshold -- same guarantee holds every
	// subsequent call in the turn, not just the one that first tripped it.
	fourth, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Greet"})
	if err != nil {
		t.Fatalf("fourth read: %v", err)
	}
	fourthText := resultText(t, fourth)
	if strings.Contains(fourthText, "auto-batched") || strings.Contains(fourthText, "circuit breaker") {
		t.Fatalf("expected the response to stay unaltered on a later past-threshold call too, got: %s", fourthText)
	}
}

// TestWriteBatchNudge_FiresOnceAtThreshold guards the fix for a v8
// bench finding: defn invoked the Go toolchain 82% more often than
// files-mode (178 vs 98 calls across a 15-task corpus) because every
// individual apply-batchable write call auto-builds on its own. Unlike
// the read-shaped breaker, this must never refuse (the build gate has
// to run on every write regardless) -- it only appends a suggestion,
// exactly once at the threshold crossing, not on every call after.
func TestWriteBatchNudge_FiresOnceAtThreshold(t *testing.T) {
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	for i := 1; i < defnWriteShapedCircuitBreakerThreshold; i++ {
		if msg := s.writeBatchNudge(sc, "edit"); msg != "" {
			t.Fatalf("call %d: expected no nudge below threshold, got %q", i, msg)
		}
	}
	msg := s.writeBatchNudge(sc, "edit")
	if msg == "" {
		t.Fatal("expected a nudge exactly at the threshold crossing")
	}
	if !strings.Contains(msg, `op:"apply"`) {
		t.Errorf("nudge should point at op:\"apply\", got %q", msg)
	}
	// One call past the crossing: must NOT fire again (non-blocking, so
	// it shouldn't nag on every subsequent call the way a refusal would).
	if msg := s.writeBatchNudge(sc, "edit"); msg != "" {
		t.Errorf("expected no repeat nudge past the crossing, got %q", msg)
	}
}

// TestWriteBatchNudge_IgnoresNonBatchableWriteOps guards move/insert/
// retarget-field-value being excluded from writeShapedOps -- these
// mutate the DB but handleApply's own switch rejects them with
// "unknown op", so nudging toward apply for them would send the model
// to a batch call that can't actually accept what it's holding.
func TestWriteBatchNudge_IgnoresNonBatchableWriteOps(t *testing.T) {
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	for _, op := range []string{"move", "insert", "retarget-field-value", "read", "test"} {
		for i := 0; i < defnWriteShapedCircuitBreakerThreshold+5; i++ {
			if msg := s.writeBatchNudge(sc, op); msg != "" {
				t.Fatalf("op %q must never get the apply-batch nudge, got %q", op, msg)
			}
		}
	}
	if sc.writeShapedCount != 0 {
		t.Errorf("non-apply-batchable ops should not increment the counter, got %d", sc.writeShapedCount)
	}
}

func TestWriteBatchNudge_ApplyResetsCounter(t *testing.T) {
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	for i := 0; i < defnWriteShapedCircuitBreakerThreshold-1; i++ {
		s.writeBatchNudge(sc, "edit")
	}
	if msg := s.writeBatchNudge(sc, "apply"); msg != "" {
		t.Fatalf("apply itself should never get a nudge, got %q", msg)
	}
	if sc.writeShapedCount != 0 {
		t.Fatalf("apply should reset the counter, got %d", sc.writeShapedCount)
	}
	for i := 1; i < defnWriteShapedCircuitBreakerThreshold; i++ {
		if msg := s.writeBatchNudge(sc, "edit"); msg != "" {
			t.Fatalf("expected fresh budget after apply reset, got nudge %q", msg)
		}
	}
}

func TestWriteBatchNudge_EnvOverride(t *testing.T) {
	t.Setenv("DEFN_WRITE_CIRCUIT_BREAKER", "2")
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	s.writeBatchNudge(sc, "edit")
	if msg := s.writeBatchNudge(sc, "edit"); msg == "" {
		t.Fatal("expected a nudge at lowered threshold=2")
	}
}

func TestWriteBatchNudge_StrippedDisablesEntirely(t *testing.T) {
	t.Setenv("DEFN_STRIP", "write-batch-nudge")
	sc := &sessionCache{entries: map[string]cacheEntry{}}
	s := &server{}
	for i := 0; i < defnWriteShapedCircuitBreakerThreshold+10; i++ {
		if msg := s.writeBatchNudge(sc, "edit"); msg != "" {
			t.Fatalf("call %d: DEFN_STRIP=write-batch-nudge should disable the nudge entirely, got %q", i, msg)
		}
	}
}

// TestCheckTurnBoundary_ResetsStarterInjectedOnNewToken is #312: the
// #203 starter bundle used to be session-lifetime-once ("won't repeat"
// for the whole session) even though the intent was one per turn -- in
// a multi-turn conversation every turn after the first got no bundle at
// all. checkTurnBoundary now resets starterInjected alongside its other
// per-turn counters, so the SAME turn keeps its one-shot suppressed but
// a NEW turn gets a fresh shot at the bundle.
func TestCheckTurnBoundary_ResetsStarterInjectedOnNewToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".defn"), 0755); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, ".defn", ".turn-token")
	s := &server{projectDir: dir}
	sc := &sessionCache{entries: map[string]cacheEntry{}}

	if err := os.WriteFile(tokenPath, []byte("turn-1"), 0644); err != nil {
		t.Fatal(err)
	}
	s.checkTurnBoundary(sc)
	sc.starterInjected = true

	// Same token again -- same turn, the one-shot must stay consumed.
	s.checkTurnBoundary(sc)
	if !sc.starterInjected {
		t.Fatalf("same turn-token should not reset starterInjected")
	}

	if err := os.WriteFile(tokenPath, []byte("turn-2"), 0644); err != nil {
		t.Fatal(err)
	}
	s.checkTurnBoundary(sc)
	if sc.starterInjected {
		t.Fatalf("new turn-token should reset starterInjected so this turn can get its own bundle")
	}
}
