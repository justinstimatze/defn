package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/justinstimatze/defn/internal/store"
)

// TestNewMCPServer_AsyncBackfillDisabledKeepsExplainClient covers the
// narrower #201 flag: DEFN_ASYNC_BACKFILL=0 should leave on-demand
// LLM ops (explainClient) intact -- it only opts out of the
// background per-def summary spend that otherwise fires on ingest,
// not the whole co-processor.
func TestNewMCPServer_AsyncBackfillDisabledKeepsExplainClient(t *testing.T) {
	t.Setenv("DEFN_ASYNC_BACKFILL", "0")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-fake-key-not-real")

	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer db.Close()

	s, _ := newMCPServer(context.Background(), db, "")
	defer s.summaryWorker.Stop()

	if s.explainClient == nil {
		t.Error("expected explainClient to remain set when only DEFN_ASYNC_BACKFILL=0 is set")
	}
}

// TestNewMCPServer_DefaultEnvUnsetMatchesPriorBehavior guards against
// #201 changing behavior for the overwhelming common case: no
// DEFN_LLM_OPS / DEFN_ASYNC_BACKFILL set at all. Without
// ANTHROPIC_API_KEY, explainClient must still be nil (the pre-#201
// no-API-key path), unaffected by the new flags being absent.
func TestNewMCPServer_DefaultEnvUnsetMatchesPriorBehavior(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer db.Close()

	s, _ := newMCPServer(context.Background(), db, "")
	defer s.summaryWorker.Stop()

	if s.explainClient != nil {
		t.Error("expected explainClient nil with no ANTHROPIC_API_KEY, regardless of #201 flags being unset")
	}
}

// TestNewMCPServer_LLMOpsDisabledNilsExplainClient is the #201
// regression: DEFN_LLM_OPS=0 must force s.explainClient to nil even
// when ANTHROPIC_API_KEY is set, since every co-processor call site
// (op:explain question, op:context synthesis, overview/file/project
// narratives) already treats a nil explainClient as "unavailable,
// degrade gracefully" -- the same path taken today when no API key is
// configured at all.
func TestNewMCPServer_LLMOpsDisabledNilsExplainClient(t *testing.T) {
	t.Setenv("DEFN_LLM_OPS", "0")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-fake-key-not-real")

	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer db.Close()

	s, _ := newMCPServer(context.Background(), db, "")
	defer s.summaryWorker.Stop()

	if s.explainClient != nil {
		t.Error("expected explainClient nil when DEFN_LLM_OPS=0, even with ANTHROPIC_API_KEY set")
	}
}
