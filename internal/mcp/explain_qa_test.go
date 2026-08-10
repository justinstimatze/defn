package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
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

// TestHandleExplainWithQuestion_ModuleDisambiguatesSameNamedType guards a
// real trajectory failure (prometheus-18712, 2026-08-10): handleExplainWithQuestion
// called GetDefinitionByName(name, "") directly, discarding args.Module/
// args.File/args.Receiver entirely -- unlike every other name-resolving op
// (outline, read, test, plain explain), which all go through
// resolveEditTarget. An ambiguous name with an explicit module: disambiguator
// still silently resolved to the wrong definition, and since explainCacheKey
// hashes the RESOLVED def's identity, the wrong resolution was invisible --
// changing module: between calls didn't even bust the cache.
func TestHandleExplainWithQuestion_ModuleDisambiguatesSameNamedType(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "bft"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "chess"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte(`package bft

type Engine struct{ Replica string }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	bftEngine, err := db.GetDefinitionByName("Engine", "testproj/bft")
	if err != nil || bftEngine == nil {
		t.Fatalf("GetDefinitionByName bft Engine: %v", err)
	}

	s := &server{backend: db, projectDir: projDir} // explainClient is nil -- resolution must still be correct

	items := []explainScopeItem{{name: "Engine", def: bftEngine}}
	cacheKey := explainCacheKey("what fields does Engine have", items)
	refs := []string{formatReceiver(bftEngine.Receiver) + bftEngine.Name}
	if err := db.SetExplainCache(cacheKey, "what fields does Engine have", strings.Join(refs, ","), "Engine has a Replica field.", "test-model", refs); err != nil {
		t.Fatalf("SetExplainCache: %v", err)
	}

	result, _, err := s.handleExplainWithQuestion(context.Background(), nil, codeParam{
		Name:     "Engine",
		Module:   "testproj/bft",
		Question: "what fields does Engine have",
	})
	if err != nil {
		t.Fatalf("handleExplainWithQuestion: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Engine has a Replica field.") {
		t.Errorf("module:\"testproj/bft\" should have resolved to bft's Engine (matching the pre-seeded cache entry), got:\n%s", text)
	}
}
