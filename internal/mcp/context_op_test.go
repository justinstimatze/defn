package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestHandleContext_BundlesTopHits verifies the #195 vertical: given
// a question, the op finds relevant defs, outlines each with callers
// + callees + body-size, and returns one bundled response with
// provenance. Skips the Sonnet synthesis path (explainClient is nil
// in setup) — that's exercised by explain_qa_test when key is set.
func TestHandleContext_BundlesTopHits(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, explainClient: nil}

	result, _, err := s.handleContext(context.Background(), nil, codeParam{
		Op:       "context",
		Question: "greet farewell",
	})
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "## Context bundle for: greet farewell") {
		t.Errorf("expected bundle header, got: %s", text)
	}
	if !strings.Contains(text, "### Greet") {
		t.Errorf("expected Greet in the outlined defs, got: %s", text)
	}
	if !strings.Contains(text, "### Farewell") {
		t.Errorf("expected Farewell in the outlined defs, got: %s", text)
	}
	if !strings.Contains(text, "Body:") {
		t.Errorf("expected body-size line in outline, got: %s", text)
	}
	if !strings.Contains(text, "_Grounded in:") {
		t.Errorf("expected provenance footer, got: %s", text)
	}
	// Sonnet synthesis is skipped when explainClient is nil; the
	// "### Synthesis" section must not appear.
	if strings.Contains(text, "### Synthesis") {
		t.Errorf("synthesis section should be absent when explainClient is nil; got: %s", text)
	}
}

// TestHandleContext_RequiresQuestion locks the validation: the op is
// question-driven; without one there's nothing to search.
func TestHandleContext_RequiresQuestion(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}
	result, _, err := s.handleContext(context.Background(), nil, codeParam{Op: "context"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("expected IsError result when question is missing")
	}
}

// TestHandleContext_LimitFileModuleScopeResults guards the #250 fix:
// context accepted limit:/file:/module: params but silently ignored
// all three -- every call searched/returned the whole repo capped at a
// fixed top-5, with zero error or note. Same silent-drop class as
// #241 (search's file:).
func TestHandleContext_LimitFileModuleScopeResults(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, explainClient: nil}

	// limit:1 must cap the bundle to a single def instead of the default 5.
	result, _, err := s.handleContext(context.Background(), nil, codeParam{
		Question: "greet farewell",
		Limit:    1,
	})
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Top 1 of") {
		t.Errorf("expected limit:1 to cap the bundle to 1 hit, got: %s", text)
	}

	// file: scoping to a file with no matches must exclude everything,
	// proving file: actually filters instead of being a silent no-op.
	result, _, err = s.handleContext(context.Background(), nil, codeParam{
		Question: "greet farewell",
		File:     "nonexistent.go",
	})
	if err != nil {
		t.Fatalf("context with file: %v", err)
	}
	text = resultText(t, result)
	if strings.Contains(text, "### Greet") || strings.Contains(text, "### Farewell") {
		t.Errorf("file:\"nonexistent.go\" should have excluded every def, got: %s", text)
	}
}
