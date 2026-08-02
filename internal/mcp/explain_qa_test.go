package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/store"
)

// TestExplainCacheKey_ChangesWithBodyEdit is the #192 invalidation
// contract: the same question against the same scope name produces a
// different cache key once the def's body (and therefore its Hash)
// changes -- no explicit cache invalidation needed.
func TestExplainCacheKey_ChangesWithBodyEdit(t *testing.T) {
	d1 := &store.Definition{Name: "Foo", Hash: "aaa"}
	d2 := &store.Definition{Name: "Foo", Hash: "bbb"}
	items1 := []explainScopeItem{{name: "Foo", def: d1}}
	items2 := []explainScopeItem{{name: "Foo", def: d2}}

	k1 := explainCacheKey("what does Foo do", items1)
	k2 := explainCacheKey("what does Foo do", items2)
	if k1 == k2 {
		t.Error("expected different cache keys for different body hashes, got the same")
	}

	k1Again := explainCacheKey("what does Foo do", items1)
	if k1 != k1Again {
		t.Error("expected deterministic cache key for identical inputs")
	}
}

// TestHandleExplainWithQuestion_CacheHitReturnsWithoutExplainClient covers
// #192: a cached explain-QA answer must be served without touching
// explainClient at all -- the cache check must run before the nil-client
// guard, same pattern as #212's file/project narrative cache.
func TestHandleExplainWithQuestion_CacheHitReturnsWithoutExplainClient(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db} // explainClient is nil -- cache hit must never touch it

	d, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("GetDefinitionByName Greet: %v", err)
	}

	items := []explainScopeItem{{name: "Greet", def: d}}
	cacheKey := explainCacheKey("what does this do", items)
	refs := []string{formatReceiver(d.Receiver) + d.Name}
	if err := db.SetExplainCache(cacheKey, "what does this do", strings.Join(refs, ","), "Cached answer for testing.", "test-model", refs); err != nil {
		t.Fatalf("SetExplainCache: %v", err)
	}

	result, _, err := s.handleExplainWithQuestion(context.Background(), nil, codeParam{Name: "Greet", Question: "what does this do"})
	if err != nil {
		t.Fatalf("handleExplainWithQuestion: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Cached answer for testing.") {
		t.Errorf("expected cached answer returned without touching explainClient, got %q", text)
	}
}
