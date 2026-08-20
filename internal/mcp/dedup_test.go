package mcp

import (
	"context"
	"reflect"
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
		{codeParam{Op: "search", Pattern: "auth"}, "search", true},
		{codeParam{Op: "find", File: "main.go"}, "find", true},
		{codeParam{Op: "context", Question: "how does routing work"}, "context", true},
		// Not cached:
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

func TestDedup_StrippedDisablesCaching(t *testing.T) {
	t.Setenv("DEFN_STRIP", "dedup")
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	big := strings.Repeat("x", 1000)

	c.dedup(sess, "read", "Foo", mkText(big))
	r := c.dedup(sess, "read", "Foo", mkText(big))
	got := r.Content[0].(*sdkmcp.TextContent).Text
	if strings.Contains(got, "cached") {
		t.Errorf("DEFN_STRIP=dedup should disable caching entirely, got stub %q", got)
	}
	if got != big {
		t.Errorf("expected the original content unchanged, got %q", got)
	}
}

// TestBodyServed_InvalidateClears confirms invalidate() (already fired
// on every write op by handleCode) also clears the body-served state,
// same lifecycle as the existing dedup entries.
func TestBodyServed_InvalidateClears(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}

	c.markBodyServed(sess, "Foo")
	c.invalidate(sess)
	if c.hasBodyServed(sess, "Foo") {
		t.Error("expected hasBodyServed false after invalidate")
	}
}

// TestBodyServed_MarkAndCheck is the #176 unit-level regression for
// respCache's new cross-def reuse tracking: hasBodyServed must be
// false before any mark, true after markBodyServed, and scoped per
// name (marking Foo must not report Bar as served).
func TestBodyServed_MarkAndCheck(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}

	if c.hasBodyServed(sess, "Foo") {
		t.Error("expected hasBodyServed false before any mark")
	}
	c.markBodyServed(sess, "Foo")
	if !c.hasBodyServed(sess, "Foo") {
		t.Error("expected hasBodyServed true after markBodyServed")
	}
	if c.hasBodyServed(sess, "Bar") {
		t.Error("marking Foo should not affect Bar")
	}
}

// TestBodyServed_NilSessionSafe mirrors TestDedup_NilSessionPassThrough:
// a nil session must not panic and must simply report unserved.
func TestBodyServed_NilSessionSafe(t *testing.T) {
	c := newRespCache()
	c.markBodyServed(nil, "Foo") // must not panic
	if c.hasBodyServed(nil, "Foo") {
		t.Error("expected hasBodyServed false for nil session")
	}
}

// TestJustMutated_MarkAndTake pins markMutated/takeMutated: false
// before any mark, true (once) after markMutated, and consumed after
// the first takeMutated call.
func TestJustMutated_MarkAndTake(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}

	if c.takeMutated(sess, "Foo") {
		t.Error("expected takeMutated false before any mark")
	}
	c.markMutated(sess, "Foo")
	if !c.takeMutated(sess, "Foo") {
		t.Error("expected takeMutated true after markMutated")
	}
	if c.takeMutated(sess, "Foo") {
		t.Error("expected takeMutated to be consumed after the first call")
	}
}

func TestInvalidateNames_PreservesUnrelatedEntries(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	foo := mkPayload("foo-body")
	bar := mkPayload("bar-body")
	proj := mkPayload("project-overview")

	c.dedup(sess, "read", "Foo", mkText(foo))
	c.dedup(sess, "read", "Bar", mkText(bar))
	c.dedup(sess, "overview", "project", mkText(proj))

	c.invalidateNames(sess, []string{"Foo"}, nil)

	// Foo's entry is gone: a repeat read of Foo should NOT hit the stub.
	if r := c.dedup(sess, "read", "Foo", mkText(foo)); strings.Contains(r.Content[0].(*sdkmcp.TextContent).Text, "cached") {
		t.Errorf("Foo should have been invalidated; got a cache-hit stub")
	}

	// Bar's entry survives: a repeat read of Bar SHOULD hit the stub.
	if r := c.dedup(sess, "read", "Bar", mkText(bar)); !strings.Contains(r.Content[0].(*sdkmcp.TextContent).Text, "cached") {
		t.Errorf("Bar should NOT have been invalidated; got full content instead of a cache-hit stub")
	}

	// overview|project always gets cleared by any determinable-blast-radius write.
	if r := c.dedup(sess, "overview", "project", mkText(proj)); strings.Contains(r.Content[0].(*sdkmcp.TextContent).Text, "cached") {
		t.Errorf("overview|project should always be invalidated; got a cache-hit stub")
	}
}

func TestWriteTargets(t *testing.T) {
	cases := []struct {
		name      string
		args      codeParam
		wantNames []string
		wantFiles []string
		wantOK    bool
	}{
		{
			name:      "edit_scopes_to_name",
			args:      codeParam{Op: "edit", Name: "Foo"},
			wantNames: []string{"Foo"},
			wantOK:    true,
		},
		{
			// A rename's real blast radius includes every rewritten
			// caller's body (and, for a type rename, sibling method
			// receivers) -- none of that is knowable from OldName/NewName
			// alone, so it falls back to a full invalidate rather than
			// under-scoping and leaving a caller's stale bodyServed entry
			// in place.
			name:   "rename_is_not_determinable",
			args:   codeParam{Op: "rename", OldName: "Foo", NewName: "Bar"},
			wantOK: false,
		},
		{
			name:      "create_scopes_to_file",
			args:      codeParam{Op: "create", File: "pkg/x.go"},
			wantFiles: []string{"pkg/x.go"},
			wantOK:    true,
		},
		{
			name:      "add_import_scopes_to_file",
			args:      codeParam{Op: "add-import", File: "pkg/x.go"},
			wantFiles: []string{"pkg/x.go"},
			wantOK:    true,
		},
		{
			name:   "sync_is_not_determinable",
			args:   codeParam{Op: "sync"},
			wantOK: false,
		},
		{
			name: "apply_batch_mixes_names_and_files",
			args: codeParam{Op: "apply", Operations: []applyOp{
				{Op: "edit", Name: "Foo"},
				{Op: "create", File: "pkg/y.go"},
			}},
			wantNames: []string{"Foo"},
			wantFiles: []string{"pkg/y.go"},
			wantOK:    true,
		},
		{
			name: "apply_batch_with_unrecognized_op_is_not_determinable",
			args: codeParam{Op: "apply", Operations: []applyOp{
				{Op: "edit", Name: "Foo"},
				{Op: "sync"},
			}},
			wantOK: false,
		},
		{
			name: "apply_batch_with_rename_is_not_determinable",
			args: codeParam{Op: "apply", Operations: []applyOp{
				{Op: "edit", Name: "Foo"},
				{Op: "rename", Name: "Baz", NewName: "Qux"},
			}},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			names, files, ok := writeTargets(c.args)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(names, c.wantNames) && !(len(names) == 0 && len(c.wantNames) == 0) {
				t.Errorf("names = %v, want %v", names, c.wantNames)
			}
			if !reflect.DeepEqual(files, c.wantFiles) && !(len(files) == 0 && len(c.wantFiles) == 0) {
				t.Errorf("files = %v, want %v", files, c.wantFiles)
			}
		})
	}
}

func TestInvalidateNames_FindScopedToFile(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	a := mkPayload("find-a")
	b := mkPayload("find-b")

	c.dedup(sess, "find", "pkg/a.go|0", mkText(a))
	c.dedup(sess, "find", "pkg/b.go|0", mkText(b))

	c.invalidateNames(sess, nil, []string{"pkg/a.go"})

	if r := c.dedup(sess, "find", "pkg/a.go|0", mkText(a)); strings.Contains(r.Content[0].(*sdkmcp.TextContent).Text, "cached") {
		t.Errorf("find entries for the touched file should be cleared, got a cache-hit stub")
	}
	if r := c.dedup(sess, "find", "pkg/b.go|0", mkText(b)); !strings.Contains(r.Content[0].(*sdkmcp.TextContent).Text, "cached") {
		t.Errorf("find entries for an untouched file should survive, got full content instead of a cache-hit stub")
	}
}

func TestInvalidateNames_SearchAlwaysCleared(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	res := mkPayload("search-results")

	c.dedup(sess, "search", "auth|10|false", mkText(res))
	c.invalidateNames(sess, []string{"SomeUnrelatedDef"}, nil)

	if r := c.dedup(sess, "search", "auth|10|false", mkText(res)); strings.Contains(r.Content[0].(*sdkmcp.TextContent).Text, "cached") {
		t.Errorf("search entries should always be cleared by any determinable write, got a cache-hit stub")
	}
}

func TestBodyServedEpochsAgo(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}

	if _, ok := c.bodyServedEpochsAgo(sess, "Foo"); ok {
		t.Fatal("expected ok=false for a name never marked served")
	}

	c.markBodyServed(sess, "Foo")
	epochsAgo, ok := c.bodyServedEpochsAgo(sess, "Foo")
	if !ok || epochsAgo != 0 {
		t.Fatalf("expected epochsAgo=0 right after marking, got %d, ok=%v", epochsAgo, ok)
	}

	c.mu.Lock()
	c.getSession(sess).compactionEpoch = 2
	c.mu.Unlock()

	epochsAgo, ok = c.bodyServedEpochsAgo(sess, "Foo")
	if !ok || epochsAgo != 2 {
		t.Fatalf("expected epochsAgo=2 after advancing epoch, got %d, ok=%v", epochsAgo, ok)
	}
}

func TestDedup_StaleBeyondEpochThreshold(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	body := mkPayload("body")

	c.dedup(sess, "read", "Foo", mkText(body))
	c.mu.Lock()
	c.getSession(sess).compactionEpoch = staleEpochThreshold + 1
	c.mu.Unlock()

	r := c.dedup(sess, "read", "Foo", mkText(body))
	got := r.Content[0].(*sdkmcp.TextContent).Text
	if strings.Contains(got, "cached") {
		t.Errorf("expected real content past staleEpochThreshold, got suppression stub instead: %q", got)
	}
	if got != body {
		t.Errorf("expected the real body to pass through unchanged, got %q", got)
	}
}

func TestDedup_SurvivesWithinEpochThreshold(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	body := mkPayload("body")

	c.dedup(sess, "read", "Foo", mkText(body))
	c.mu.Lock()
	c.getSession(sess).compactionEpoch = staleEpochThreshold
	c.mu.Unlock()

	r := c.dedup(sess, "read", "Foo", mkText(body))
	if got := r.Content[0].(*sdkmcp.TextContent).Text; !strings.Contains(got, "cached") {
		t.Errorf("expected suppression within staleEpochThreshold, got full content: %q", got)
	}
}

// TestHandleCode_ContextRepeatHitsDedupStub verifies the fix: context
// was defn's most expensive uncovered op (a full context bundle,
// potentially including Sonnet synthesis) with zero dedup coverage
// before this. Confirmed against a real transcript first (2026-08-05):
// one exact question repeated 4 times in a single session with no
// caching at all.
func TestHandleCode_ContextRepeatHitsDedupStub(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	first, _, err := s.handleCode(context.Background(), req, codeParam{Op: "context", Question: "how does Chunky process items"})
	if err != nil {
		t.Fatalf("first context call: %v", err)
	}
	firstText := resultText(t, first)
	if strings.Contains(firstText, "cached") {
		t.Fatalf("first call should not be a dedup hit, got:\n%s", firstText)
	}

	second, _, err := s.handleCode(context.Background(), req, codeParam{Op: "context", Question: "how does Chunky process items"})
	if err != nil {
		t.Fatalf("second context call: %v", err)
	}
	secondText := resultText(t, second)
	if !strings.Contains(secondText, "cached") {
		t.Errorf("expected the identical repeat context call to hit the dedup stub, got full content:\n%s", secondText)
	}

	// A genuinely different question must NOT be suppressed.
	third, _, err := s.handleCode(context.Background(), req, codeParam{Op: "context", Question: "how does process work"})
	if err != nil {
		t.Fatalf("third context call: %v", err)
	}
	thirdText := resultText(t, third)
	if strings.Contains(thirdText, "cached: identical") {
		t.Errorf("a different question should not hit the dedup stub, got:\n%s", thirdText)
	}
}

// TestInvalidateNames_ContextAlwaysCleared is the regression for
// invalidateNames missing a "context|" branch alongside "search|" --
// context's dedup key is free-text question, modeled explicitly on
// search's own convention (per dedupOpKey's case comment), but only
// search actually got the blanket-clear treatment in invalidateNames.
func TestInvalidateNames_ContextAlwaysCleared(t *testing.T) {
	c := newRespCache()
	sess := &sdkmcp.ServerSession{}
	res := mkPayload("context-bundle")

	c.dedup(sess, "context", "how does auth work", mkText(res))
	c.invalidateNames(sess, []string{"SomeUnrelatedDef"}, nil)

	if r := c.dedup(sess, "context", "how does auth work", mkText(res)); strings.Contains(r.Content[0].(*sdkmcp.TextContent).Text, "cached") {
		t.Errorf("context entries should always be cleared by any determinable write, got a cache-hit stub")
	}
}

// TestDedupOpKey_ExplainKeyIncludesQuestion is the regression for
// dedupOpKey's "explain" case ignoring Question/Names: two different
// questions about the same def used to collide on one dedup key
// (args.Name alone), silently defeating dedup for the question-driven
// explain path -- interleaved explain(name:"F", question:"...") calls
// with different questions never got their own cache slot. Same
// convention "context" already follows for its own free-text key.
func TestDedupOpKey_ExplainKeyIncludesQuestion(t *testing.T) {
	_, key1, ok1 := dedupOpKey(codeParam{Op: "explain", Name: "Foo", Question: "how does it handle errors"})
	_, key2, ok2 := dedupOpKey(codeParam{Op: "explain", Name: "Foo", Question: "why is this exported"})
	if !ok1 || !ok2 {
		t.Fatalf("expected both explain calls to be cacheable, got ok1=%v ok2=%v", ok1, ok2)
	}
	if key1 == key2 {
		t.Errorf("two different questions about the same def collided on one dedup key: %q", key1)
	}

	_, bareKey, ok := dedupOpKey(codeParam{Op: "explain", Name: "Foo"})
	if !ok {
		t.Fatal("expected bare explain(name:) to still be cacheable")
	}
	if bareKey != "Foo" {
		t.Errorf("bare explain(name:) key changed shape: got %q, want %q", bareKey, "Foo")
	}
}

// TestMaybeAppendStarterBundle_EmptyQuestionDoesNotConsumeOneShot is
// the regression for maybeAppendStarterBundle burning its one-shot
// starterInjected flag before checking whether question was even
// non-empty: an empty question means there's nothing for handleContext
// to work with, so it shouldn't cost the session its only starter-
// bundle opportunity -- a later call with a real question should still
// get a shot at it.
func TestMaybeAppendStarterBundle_EmptyQuestionDoesNotConsumeOneShot(t *testing.T) {
	s := &server{respCache: newRespCache()}
	req := &sdkmcp.CallToolRequest{Session: &sdkmcp.ServerSession{}}

	if got := s.maybeAppendStarterBundle(req, "   "); got != "" {
		t.Fatalf("expected empty string for a blank question, got: %q", got)
	}

	sc := s.respCache.getSession(req.Session)
	if sc.starterInjected {
		t.Fatal("a blank-question call should not have consumed the one-shot starterInjected flag")
	}
}

// TestDedupOpKey_ReadKeyIncludesQuery is the regression for a real
// trajectory finding (prometheus-18712, v4 mining round): dedupOpKey's
// "read" case ignored args.Query, so a full-body read of a large
// function followed by a query-scoped read of the SAME def collided on
// one dedup key -- the query-scoped call got served the stale
// full-body "already served, nothing new" stub instead of the
// genuinely narrower, different output it asked for.
func TestDedupOpKey_ReadKeyIncludesQuery(t *testing.T) {
	_, key1, ok1 := dedupOpKey(codeParam{Op: "read", Name: "main", Full: true})
	_, key2, ok2 := dedupOpKey(codeParam{Op: "read", Name: "main", Full: true, Query: "memlimit"})
	if !ok1 || !ok2 {
		t.Fatalf("expected both read calls to be cacheable, got ok1=%v ok2=%v", ok1, ok2)
	}
	if key1 == key2 {
		t.Errorf("a plain full-body read and a query-scoped read of the same def collided on one dedup key: %q", key1)
	}
}

// TestDedupOpKey_ReadKeyIncludesLineRange is the same collision class as
// TestDedupOpKey_ReadKeyIncludesQuery, but for the line_range param added
// alongside it: a full-body read of a large function followed by a
// line-range-scoped read of the SAME def must not collide on one dedup
// key, or the ranged call would get served the stale full-body stub
// instead of the narrower range it actually asked for.
func TestDedupOpKey_ReadKeyIncludesLineRange(t *testing.T) {
	_, key1, ok1 := dedupOpKey(codeParam{Op: "read", Name: "main", Full: true})
	_, key2, ok2 := dedupOpKey(codeParam{Op: "read", Name: "main", Full: true, LineRange: "700-820"})
	_, key3, ok3 := dedupOpKey(codeParam{Op: "read", Name: "main", Full: true, LineRange: "900-950"})
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("expected all three read calls to be cacheable, got ok1=%v ok2=%v ok3=%v", ok1, ok2, ok3)
	}
	if key1 == key2 {
		t.Errorf("a plain full-body read and a line_range-scoped read of the same def collided on one dedup key: %q", key1)
	}
	if key2 == key3 {
		t.Errorf("two different line_range values on the same def collided on one dedup key: %q", key2)
	}
}
