package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestReadNeighborhoodAppendedByDefault locks in #202: a bare
// code(op:"read") on a real def returns the body PLUS a "Related
// (#202)" footer listing callers/callees. Uses the setupTestDB
// fixture where Farewell calls Greet, so Greet has a caller.
func TestReadNeighborhoodAppendedByDefault(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := resultText(t, result)
	// Body still present (small def, below auto-downgrade threshold)
	if !strings.Contains(text, "Hello,") {
		t.Errorf("expected Greet body, got: %s", text)
	}
	// Neighborhood footer must appear
	if !strings.Contains(text, "Related (#202)") {
		t.Errorf("expected #202 Related footer, got: %s", text)
	}
	// Farewell calls Greet → should appear as a caller
	if !strings.Contains(text, "Farewell") {
		t.Errorf("expected Farewell as a caller in neighborhood, got: %s", text)
	}
}

// TestReadNeighborhoodSkippedForQuery verifies that query-adaptive
// filtered reads skip the neighborhood (query already narrows intent).
func TestReadNeighborhoodSkippedForQuery(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet", Query: "hello"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "Related (#202)") {
		t.Errorf("query-scoped read must NOT emit #202 footer; got: %s", text)
	}
}
