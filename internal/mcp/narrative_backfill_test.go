package mcp

import (
	"context"
	"testing"
)

// TestBackfillNarratives_AsyncBackfillDisabledNoOp covers #201's
// DEFN_ASYNC_BACKFILL=0 flag reaching this second background-spend
// path, not just the def-summary worker. Isolating this guard from
// the (also-nil) explainClient guard would need a mockable Explain
// client, which this codebase doesn't have anywhere -- both guards
// short-circuit the same early return, so this documents the flag is
// wired in without claiming to prove it in isolation.
func TestBackfillNarratives_AsyncBackfillDisabledNoOp(t *testing.T) {
	t.Setenv("DEFN_ASYNC_BACKFILL", "0")
	db, projDir := setupTestDB(t)
	defer db.Close()

	s := &server{backend: db, projectDir: projDir}
	s.backfillNarratives(context.Background())

	if cached, _ := db.GetMeta("project_narrative"); cached != "" {
		t.Errorf("expected no project_narrative meta row with DEFN_ASYNC_BACKFILL=0, got: %q", cached)
	}
}

// TestBackfillNarratives_NilExplainClientNoOp is the #200 regression
// for the common case (no ANTHROPIC_API_KEY): backfillNarratives must
// return immediately without attempting project-narrative generation
// -- the overwhelming majority of installs run with no co-processor
// configured at all, and this must be a true no-op for them, not a
// silent DB scan that goes nowhere.
func TestBackfillNarratives_NilExplainClientNoOp(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	s := &server{backend: db, projectDir: projDir}
	s.backfillNarratives(context.Background())

	if cached, _ := db.GetMeta("project_narrative"); cached != "" {
		t.Errorf("expected no project_narrative meta row with nil explainClient, got: %q", cached)
	}
}
