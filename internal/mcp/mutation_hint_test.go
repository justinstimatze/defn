package mcp

import (
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMutationHint_ThresholdBehavior verifies the hint fires exactly
// once (at count == threshold) for a given (session, file) pair.
// Before threshold: empty. At threshold: hint text. After threshold:
// empty (avoids spamming on every subsequent mutation).
func TestMutationHint_ThresholdBehavior(t *testing.T) {
	h := newMutationHint()
	// Use non-nil sentinel — session identity is by pointer.
	fakeSession := sessionSentinel()

	// Counts 1 through threshold-1: no hint.
	for i := 1; i < mutationHintThreshold; i++ {
		got := h.note(fakeSession, "pkg/foo.go")
		if got != "" {
			t.Errorf("call %d: want empty hint, got %q", i, got)
		}
	}
	// Count == threshold: hint fires.
	got := h.note(fakeSession, "pkg/foo.go")
	if !strings.Contains(got, "apply(") {
		t.Errorf("threshold call: want hint containing 'apply(', got %q", got)
	}
	if !strings.Contains(got, "pkg/foo.go") {
		t.Errorf("threshold call: want hint mentioning file, got %q", got)
	}
	// Post-threshold: silent again.
	for i := 1; i <= 3; i++ {
		got := h.note(fakeSession, "pkg/foo.go")
		if got != "" {
			t.Errorf("post-threshold call %d: want empty, got %q", i, got)
		}
	}
}

// TestMutationHint_PerFileIsolation verifies two different files in the
// same session accumulate counts independently.
func TestMutationHint_PerFileIsolation(t *testing.T) {
	h := newMutationHint()
	sess := sessionSentinel()

	// Rack up threshold-1 on foo.go.
	for i := 1; i < mutationHintThreshold; i++ {
		h.note(sess, "foo.go")
	}
	// bar.go's counter should be zero.
	got := h.note(sess, "bar.go")
	if got != "" {
		t.Errorf("bar.go first hit should be silent, got %q", got)
	}
}

// TestMutationHint_NilSafe verifies nil session and empty file are
// both no-ops (Measure* paths).
func TestMutationHint_NilSafe(t *testing.T) {
	h := newMutationHint()
	for i := 0; i < 10; i++ {
		if got := h.note(nil, "any.go"); got != "" {
			t.Errorf("nil session should return empty, got %q", got)
		}
		if got := h.note(sessionSentinel(), ""); got != "" {
			t.Errorf("empty file should return empty, got %q", got)
		}
	}
}

// sessionSentinel returns a non-nil *sdkmcp.ServerSession pointer for
// tests. We only need identity, not method calls — mutationHint uses
// the pointer as a map key.
func sessionSentinel() *sdkmcp.ServerSession {
	return new(sdkmcp.ServerSession)
}
