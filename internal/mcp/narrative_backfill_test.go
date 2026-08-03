package mcp

import (
	"context"
	"os"
	"testing"

	"github.com/justinstimatze/defn/internal/store"
	"github.com/justinstimatze/defn/internal/summary"
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

// TestNarrativeCheck_LiveBackfillWarmsDirectoryScope is a manual,
// gated integration check for #234 -- confirms backfillNarratives
// actually warms a directory/package-scope narrative (not just file
// and project scopes), via a REAL Sonnet call. Requires
// DEFN_NARRATIVE_CHECK_DB and ANTHROPIC_API_KEY; skipped otherwise so
// this never runs in normal `go test ./...`. No mockable Explain
// client exists in this codebase (see
// TestBackfillNarratives_AsyncBackfillDisabledNoOp's doc), so unlike
// the guard-only unit tests above, exercising the new directory
// enumeration requires a live co-processor.
func TestNarrativeCheck_LiveBackfillWarmsDirectoryScope(t *testing.T) {
	dbPath := os.Getenv("DEFN_NARRATIVE_CHECK_DB")
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if dbPath == "" || apiKey == "" {
		t.Skip("set DEFN_NARRATIVE_CHECK_DB and ANTHROPIC_API_KEY to run this manual check")
	}
	db, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	s := &server{
		backend:       db,
		explainClient: summary.NewExplain(summary.ExplainOptions{APIKey: apiKey}),
	}

	scope := os.Getenv("DEFN_NARRATIVE_CHECK_SCOPE")
	if scope == "" {
		scope = "internal/mcp"
	}

	// Clear any narrative fileNarrative may have already cached at this
	// scope from an earlier test in this run, so a stale cache hit can't
	// masquerade as backfillNarratives having done the work.
	db.SetFileSummary(scope, 0, &store.FileSummary{Narrative: "", BodyHash: "stale"})

	s.backfillNarratives(context.Background())

	fs, err := db.GetFileSummary(scope)
	if err != nil {
		t.Fatalf("GetFileSummary: %v", err)
	}
	if fs == nil || fs.Narrative == "" {
		t.Errorf("expected backfillNarratives to warm a directory-scope narrative for %s, got none", scope)
	}
}
