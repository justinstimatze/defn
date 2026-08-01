package mcp

import (
	"context"
	"os"
	"testing"

	"github.com/justinstimatze/defn/internal/store"
	"github.com/justinstimatze/defn/internal/summary"
)

// TestNarrativeCheck_LiveOverviewWithRealExplainClient is a manual,
// gated integration check for #212 -- confirms handleOverview actually
// generates a narrative via a REAL Sonnet call, bypassing the whole
// bench/Claude-Code layer. One real API call instead of a full bench
// run. Requires DEFN_NARRATIVE_CHECK_DB (path to an ingested
// .defn/defn.db) and ANTHROPIC_API_KEY; skipped otherwise so this
// never runs in normal `go test ./...`.
func TestNarrativeCheck_LiveOverviewWithRealExplainClient(t *testing.T) {
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
	result, _, err := s.handleOverview(context.Background(), nil, codeParam{File: scope})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	text := resultText(t, result)
	t.Logf("overview(%s) response:\n%s", scope, text)

	fs, err := db.GetFileSummary(scope)
	if err != nil {
		t.Fatalf("GetFileSummary: %v", err)
	}
	if fs == nil || fs.Narrative == "" {
		t.Error("expected a narrative to be generated and stored, got none")
	}
}

// TestNarrativeCheck_LiveProjectOverviewWithRealExplainClient mirrors
// TestNarrativeCheck_LiveOverviewWithRealExplainClient but for the
// bare (no file/name) overview call -- confirms the project-wide #212
// narrative extension actually generates and stores via a REAL Sonnet
// call. Requires DEFN_NARRATIVE_CHECK_DB and ANTHROPIC_API_KEY;
// skipped otherwise so this never runs in normal `go test ./...`.
func TestNarrativeCheck_LiveProjectOverviewWithRealExplainClient(t *testing.T) {
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

	result, _, err := s.handleOverview(context.Background(), nil, codeParam{})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	text := resultText(t, result)
	t.Logf("project overview response:\n%s", text)

	cached, err := db.GetMeta("project_narrative")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if cached == "" {
		t.Error("expected a project narrative to be generated and stored, got none")
	}
}
