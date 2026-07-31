package mcp

import (
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
