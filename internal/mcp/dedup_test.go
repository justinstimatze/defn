package mcp

import (
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeSess is a unique non-nil session key. We cast a struct pointer to
// *ServerSession — respCache never dereferences it, only uses it as a
// map key by pointer identity.
func fakeSess(t *testing.T) *sdkmcp.ServerSession {
	t.Helper()
	return &sdkmcp.ServerSession{}
}

func TestDedupOpKey(t *testing.T) {
	tests := []struct {
		name    string
		args    codeParam
		wantOp  string
		wantKey string
		wantOK  bool
	}{
		{"read", codeParam{Op: "read", Name: "Foo"}, "read", "Foo", true},
		{"read_full", codeParam{Op: "read", Name: "Foo", Full: true}, "read", "Foo|full", true},
		{"outline", codeParam{Op: "outline", Name: "Foo"}, "outline", "Foo", true},
		{"slice", codeParam{Op: "slice", Name: "Foo", Slice: "body"}, "slice", "Foo|body", true},
		{"slice_indexed", codeParam{Op: "slice", Name: "Foo", Slice: "return", Index: 2}, "slice", "Foo|return|2", true},
		{"read_file", codeParam{Op: "read-file", File: "pkg/x.go"}, "read-file", "pkg/x.go", true},
		{"file_defs", codeParam{Op: "file-defs", File: "pkg/x.go"}, "file-defs", "pkg/x.go", true},
		{"search_not_dedup", codeParam{Op: "search", Pattern: "foo"}, "", "", false},
		{"impact_not_dedup", codeParam{Op: "impact", Name: "Foo"}, "", "", false},
		{"edit_not_dedup", codeParam{Op: "edit", Name: "Foo"}, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, key, ok := dedupOpKey(tt.args)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v want %v", ok, tt.wantOK)
			}
			if op != tt.wantOp || key != tt.wantKey {
				t.Errorf("got (%q, %q) want (%q, %q)", op, key, tt.wantOp, tt.wantKey)
			}
		})
	}
}

func TestIsWriteOp(t *testing.T) {
	writes := []string{"edit", "create", "delete", "rename", "move", "apply",
		"insert-precondition", "replace-slice", "replace-hunk",
		"wrap-in-defer", "rename-param", "add-import"}
	reads := []string{"read", "outline", "slice", "read-file", "file-defs",
		"search", "impact", "overview", "explain"}
	for _, op := range writes {
		if !isWriteOp(op) {
			t.Errorf("expected %q to be a write op", op)
		}
	}
	for _, op := range reads {
		if isWriteOp(op) {
			t.Errorf("expected %q NOT to be a write op", op)
		}
	}
}

func TestRespCache_DedupHit(t *testing.T) {
	c := newRespCache()
	sess := fakeSess(t)
	full := textResult(strings.Repeat("abcdefghij", 100))
	// First call: passes through, caches.
	r1 := c.dedup(sess, "read", "Foo", full)
	if r1 != full {
		t.Fatalf("first call should pass through unchanged")
	}
	// Second call with same content: stub.
	r2 := c.dedup(sess, "read", "Foo", textResult(strings.Repeat("abcdefghij", 100)))
	tc2, ok := r2.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent on dedup hit")
	}
	if !strings.Contains(tc2.Text, "cached") || !strings.Contains(tc2.Text, "call #1") {
		t.Errorf("stub text missing cache marker or call#: %q", tc2.Text)
	}
	if len(tc2.Text) >= 1000 {
		t.Errorf("stub should be well under original payload; got %d bytes", len(tc2.Text))
	}
}

func TestRespCache_SmallPayloadNotDeduped(t *testing.T) {
	// A tiny response (below dedupMinBytes) would inflate wire cost if
	// replaced with the fixed-size stub — the size gate suppresses that.
	c := newRespCache()
	sess := fakeSess(t)
	c.dedup(sess, "read", "Foo", textResult("tiny"))
	r2 := c.dedup(sess, "read", "Foo", textResult("tiny"))
	tc, _ := r2.Content[0].(*sdkmcp.TextContent)
	if strings.Contains(tc.Text, "cached") {
		t.Errorf("payload under threshold should pass through; got %q", tc.Text)
	}
}

func TestRespCache_ContentChangedIsMiss(t *testing.T) {
	c := newRespCache()
	sess := fakeSess(t)
	c.dedup(sess, "read", "Foo", textResult("v1"))
	r2 := c.dedup(sess, "read", "Foo", textResult("v2 (different content)"))
	tc, _ := r2.Content[0].(*sdkmcp.TextContent)
	if strings.Contains(tc.Text, "cached") {
		t.Errorf("changed content should not cache-hit: %q", tc.Text)
	}
}

func TestRespCache_DifferentKeysDontCollide(t *testing.T) {
	c := newRespCache()
	sess := fakeSess(t)
	c.dedup(sess, "read", "Foo", textResult("body-foo"))
	r2 := c.dedup(sess, "read", "Bar", textResult("body-bar"))
	tc, _ := r2.Content[0].(*sdkmcp.TextContent)
	if strings.Contains(tc.Text, "cached") {
		t.Errorf("different name should not cache-hit: %q", tc.Text)
	}
}

func TestRespCache_InvalidateClears(t *testing.T) {
	c := newRespCache()
	sess := fakeSess(t)
	c.dedup(sess, "read", "Foo", textResult("body"))
	c.invalidate(sess)
	// After invalidate, same-content read is a miss again.
	r2 := c.dedup(sess, "read", "Foo", textResult("body"))
	tc, _ := r2.Content[0].(*sdkmcp.TextContent)
	if strings.Contains(tc.Text, "cached") {
		t.Errorf("post-invalidate read should be a miss: %q", tc.Text)
	}
}

func TestRespCache_SessionIsolation(t *testing.T) {
	c := newRespCache()
	s1 := fakeSess(t)
	s2 := fakeSess(t)
	c.dedup(s1, "read", "Foo", textResult("body"))
	// Different session, same key/content: still a miss.
	r := c.dedup(s2, "read", "Foo", textResult("body"))
	tc, _ := r.Content[0].(*sdkmcp.TextContent)
	if strings.Contains(tc.Text, "cached") {
		t.Errorf("cross-session should not hit: %q", tc.Text)
	}
}

func TestRespCache_NilSessionPassThrough(t *testing.T) {
	c := newRespCache()
	// Nil session (test/unit-call path) — never caches, always passes through.
	c.dedup(nil, "read", "Foo", textResult("body"))
	r2 := c.dedup(nil, "read", "Foo", textResult("body"))
	tc, _ := r2.Content[0].(*sdkmcp.TextContent)
	if strings.Contains(tc.Text, "cached") {
		t.Errorf("nil session should pass through: %q", tc.Text)
	}
}

func TestRespCache_ErrorResultNotCached(t *testing.T) {
	c := newRespCache()
	sess := fakeSess(t)
	errRes := &sdkmcp.CallToolResult{
		IsError: true,
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "boom"}},
	}
	c.dedup(sess, "read", "Foo", errRes)
	// Follow-up successful read should be pass-through (not treated as hit).
	r2 := c.dedup(sess, "read", "Foo", textResult("real body"))
	tc, _ := r2.Content[0].(*sdkmcp.TextContent)
	if strings.Contains(tc.Text, "cached") {
		t.Errorf("error result should not populate cache: %q", tc.Text)
	}
}
