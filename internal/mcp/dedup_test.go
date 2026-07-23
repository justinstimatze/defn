package mcp

import (
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mkText builds a CallToolResult containing a single TextContent block
// with the given text. Kept small so the assertions read plainly.
func mkText(s string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: s}},
	}
}

// mkPayload pads a body past dedupMinBytes so the cache actually engages.
func mkPayload(prefix string) string {
	return prefix + strings.Repeat(" filler", 100)
}

func TestDedup_ReadHitReturnsStub(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	body := mkPayload("first read")

	r1 := c.dedup(sess, "read", "Foo", mkText(body))
	if rt := r1.Content[0].(*sdkmcp.TextContent).Text; rt != body {
		t.Errorf("first read should return original body; got %q", rt)
	}

	r2 := c.dedup(sess, "read", "Foo", mkText(body))
	got := r2.Content[0].(*sdkmcp.TextContent).Text
	if !strings.Contains(got, "cached") || !strings.Contains(got, "already served") {
		t.Errorf("second read should return dedup stub; got %q", got)
	}
}

func TestDedup_DifferentArgsMiss(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	body := mkPayload("body")

	c.dedup(sess, "read", "Foo", mkText(body))
	r := c.dedup(sess, "read", "Bar", mkText(body))
	if got := r.Content[0].(*sdkmcp.TextContent).Text; strings.Contains(got, "cached") {
		t.Errorf("different args should MISS; got stub %q", got)
	}
}

func TestDedup_ContentChangeMiss(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}

	c.dedup(sess, "read", "Foo", mkText(mkPayload("v1")))
	r := c.dedup(sess, "read", "Foo", mkText(mkPayload("v2")))
	if got := r.Content[0].(*sdkmcp.TextContent).Text; strings.Contains(got, "cached") {
		t.Errorf("changed content should MISS; got stub %q", got)
	}
}

func TestDedup_WriteInvalidates(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	body := mkPayload("body")

	c.dedup(sess, "read", "Foo", mkText(body))
	c.invalidate(sess)
	r := c.dedup(sess, "read", "Foo", mkText(body))
	if got := r.Content[0].(*sdkmcp.TextContent).Text; strings.Contains(got, "cached") {
		t.Errorf("after invalidate: read should MISS; got stub %q", got)
	}
}

func TestDedup_SessionIsolation(t *testing.T) {
	c := newRespCache()
	sess1 := &sdkmcp.ServerSession{}
	sess2 := &sdkmcp.ServerSession{}
	body := mkPayload("shared body")

	c.dedup(sess1, "read", "Foo", mkText(body))
	r := c.dedup(sess2, "read", "Foo", mkText(body))
	if got := r.Content[0].(*sdkmcp.TextContent).Text; strings.Contains(got, "cached") {
		t.Errorf("second session should not see first session's cache; got stub %q", got)
	}
}

func TestDedup_NilSessionPassThrough(t *testing.T) {
	c := newRespCache()
	body := mkPayload("body")
	r := c.dedup(nil, "read", "Foo", mkText(body))
	if rt := r.Content[0].(*sdkmcp.TextContent).Text; rt != body {
		t.Errorf("nil session should pass through unchanged; got %q", rt)
	}
	// Also on the second call — nil sessions never cache.
	r2 := c.dedup(nil, "read", "Foo", mkText(body))
	if rt := r2.Content[0].(*sdkmcp.TextContent).Text; strings.Contains(rt, "cached") {
		t.Errorf("nil session: second call should still pass through; got stub %q", rt)
	}
}

func TestDedup_SmallPayloadNotDeduped(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	small := "tiny"

	c.dedup(sess, "read", "Foo", mkText(small))
	r := c.dedup(sess, "read", "Foo", mkText(small))
	if got := r.Content[0].(*sdkmcp.TextContent).Text; strings.Contains(got, "cached") {
		t.Errorf("below dedupMinBytes should skip cache; got stub %q", got)
	}
}

func TestDedup_ErrorNotCached(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	body := mkPayload("body")

	errRes := mkText(body)
	errRes.IsError = true
	c.dedup(sess, "read", "Foo", errRes)

	r := c.dedup(sess, "read", "Foo", mkText(body))
	if got := r.Content[0].(*sdkmcp.TextContent).Text; strings.Contains(got, "cached") {
		t.Errorf("error result should not populate cache; got stub %q", got)
	}
}

func TestDedup_ExtendedOps(t *testing.T) {
	// #152 extensions must all dedup: impact, overview, expand, methods, explain
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	body := mkPayload("body")

	for _, op := range []string{"impact", "overview", "expand", "methods", "explain"} {
		key := "K:" + op
		c.dedup(sess, op, key, mkText(body))
		r := c.dedup(sess, op, key, mkText(body))
		if got := r.Content[0].(*sdkmcp.TextContent).Text; !strings.Contains(got, "cached") {
			t.Errorf("op=%s should dedup on repeat; got %q", op, got)
		}
	}
}

// dedupOpKey correctness — the switch determines which ops enter the cache.
func TestDedupOpKey_Mapping(t *testing.T) {
	cases := []struct {
		args   codeParam
		wantOp string
		wantOK bool
	}{
		{codeParam{Op: "read", Name: "Foo"}, "read", true},
		{codeParam{Op: "read", Name: "Foo", Full: true}, "read", true},
		{codeParam{Op: "outline", Name: "Foo"}, "outline", true},
		{codeParam{Op: "slice", Name: "Foo", Slice: "return"}, "slice", true},
		{codeParam{Op: "read-file", File: "main.go"}, "read-file", true},
		{codeParam{Op: "file-defs", File: "main.go"}, "file-defs", true},
		{codeParam{Op: "impact", Name: "Foo"}, "impact", true},
		{codeParam{Op: "overview", File: "cmd/"}, "overview", true},
		{codeParam{Op: "overview"}, "overview", true},
		{codeParam{Op: "expand", Name: "Foo", Include: []string{"body", "callers"}}, "expand", true},
		{codeParam{Op: "methods", Name: "Server"}, "methods", true},
		{codeParam{Op: "explain", Name: "Foo"}, "explain", true},
		// Not cached:
		{codeParam{Op: "search", Pattern: "auth"}, "", false},
		{codeParam{Op: "similar", Name: "Foo"}, "", false},
		{codeParam{Op: "edit", Name: "Foo"}, "", false},
	}
	for _, tc := range cases {
		gotOp, _, ok := dedupOpKey(tc.args)
		if ok != tc.wantOK {
			t.Errorf("op=%s: dedupOpKey ok = %v, want %v", tc.args.Op, ok, tc.wantOK)
		}
		if ok && gotOp != tc.wantOp {
			t.Errorf("op=%s: dedupOpKey op = %q, want %q", tc.args.Op, gotOp, tc.wantOp)
		}
	}
}

// isWriteOp correctness — write ops must invalidate the cache.
func TestIsWriteOp(t *testing.T) {
	writes := []string{"edit", "insert", "create", "delete", "rename", "move",
		"apply", "insert-precondition", "replace-slice", "replace-hunk",
		"wrap-in-defer", "rename-param", "add-import", "patch", "sync",
		"retarget-field-value"}
	reads := []string{"read", "outline", "slice", "read-file", "file-defs",
		"impact", "overview", "expand", "methods", "explain", "search",
		"similar", "test", "history"}
	for _, op := range writes {
		if !isWriteOp(op) {
			t.Errorf("op=%s should be classified as write", op)
		}
	}
	for _, op := range reads {
		if isWriteOp(op) {
			t.Errorf("op=%s should be classified as read", op)
		}
	}
}
