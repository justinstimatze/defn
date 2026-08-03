package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestHandleCode_PlanDSLAndPlanSExprRequirePlanField(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	for _, op := range []string{"plan-dsl", "plan-sexpr"} {
		result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: op})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if !result.IsError {
			t.Errorf("%s with no plan: expected an error result, got: %s", op, resultText(t, result))
		}
	}
}

func TestHandlePlanDSL_Basic(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handlePlanDSL(context.Background(), nil, codeParam{Plan: "@Greet.body\n@Farewell.outline\n"})
	if err != nil {
		t.Fatalf("plan-dsl: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "### body") || !strings.Contains(text, `return "Hello, " + name`) {
		t.Errorf("expected Greet's body, got: %s", text)
	}
	if !strings.Contains(text, "### outline") {
		t.Errorf("expected Farewell's outline, got: %s", text)
	}
}

func TestHandlePlanDSL_ExcludeTestCallers(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handlePlanDSL(context.Background(), nil, codeParam{Plan: "@Greet.callers[!test]\n"})
	if err != nil {
		t.Fatalf("plan-dsl: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Farewell") {
		t.Errorf("expected production caller Farewell, got: %s", text)
	}
	if strings.Contains(text, "TestGreet") {
		t.Errorf("[!test] should filter out TestGreet, got: %s", text)
	}
}

func TestHandlePlanDSL_ParseError(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handlePlanDSL(context.Background(), nil, codeParam{Plan: "not a valid dsl line"})
	if err != nil {
		t.Fatalf("plan-dsl: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected an error result for invalid DSL, got: %s", resultText(t, result))
	}
}

func TestHandlePlanDSL_UnknownTargetSkipped(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handlePlanDSL(context.Background(), nil, codeParam{Plan: "@Greet.body\n@NoSuchDef.body\n"})
	if err != nil {
		t.Fatalf("plan-dsl: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "not found, skipped: NoSuchDef") {
		t.Errorf("expected a skip note for NoSuchDef, got: %s", text)
	}
	if !strings.Contains(text, `return "Hello, " + name`) {
		t.Errorf("expected Greet's body despite the other step failing, got: %s", text)
	}
}

func TestHandlePlanSExpr_Basic(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handlePlanSExpr(context.Background(), nil, codeParam{Plan: "(read Greet)\n(outline Farewell)\n"})
	if err != nil {
		t.Fatalf("plan-sexpr: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "### body") || !strings.Contains(text, `return "Hello, " + name`) {
		t.Errorf("expected Greet's body, got: %s", text)
	}
	if !strings.Contains(text, "### outline") {
		t.Errorf("expected Farewell's outline, got: %s", text)
	}
}

func TestHandlePlanSExpr_ExcludeTestCallers(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handlePlanSExpr(context.Background(), nil, codeParam{Plan: "(impact Greet !test)\n"})
	if err != nil {
		t.Fatalf("plan-sexpr: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Farewell") {
		t.Errorf("expected production caller Farewell, got: %s", text)
	}
	if strings.Contains(text, "TestGreet") {
		t.Errorf("!test should filter out TestGreet, got: %s", text)
	}
}

func TestHandlePlanSExpr_ParseError(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handlePlanSExpr(context.Background(), nil, codeParam{Plan: "(bogus Greet)"})
	if err != nil {
		t.Fatalf("plan-sexpr: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected an error result for an unknown op, got: %s", resultText(t, result))
	}
}

func TestHandlePlanIntent_CacheHitReturnsWithoutCoprocessor(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db} // explainClient is nil -- cache hit must never touch it

	intent := "greet farewell"
	scored, _, err := s.gatherContextCandidates(intent)
	if err != nil {
		t.Fatalf("gatherContextCandidates: %v", err)
	}
	const planCandidateCap = 20
	if len(scored) > planCandidateCap {
		scored = scored[:planCandidateCap]
	}
	cacheKey := planCacheKey(intent, scored)
	trajectory := "(read Greet)\n(outline Farewell)\n"
	if err := db.SetExplainCache(cacheKey, intent, "candidates", trajectory, "test-model", nil); err != nil {
		t.Fatalf("SetExplainCache: %v", err)
	}

	result, _, err := s.handlePlanIntent(context.Background(), nil, codeParam{Intent: intent})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "### body") || !strings.Contains(text, `return "Hello, " + name`) {
		t.Errorf("expected Greet's body from the cached trajectory, got: %s", text)
	}
	if !strings.Contains(text, "### outline") {
		t.Errorf("expected Farewell's outline from the cached trajectory, got: %s", text)
	}
}

func TestHandlePlanIntent_NoCoprocessorClearError(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db} // explainClient is nil, no cache entry

	result, _, err := s.handlePlanIntent(context.Background(), nil, codeParam{Intent: "greet farewell"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected an error result when no co-processor is configured, got: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "plan-sexpr") {
		t.Errorf("expected the error to point at the plan-sexpr fallback, got: %s", text)
	}
}

func TestHandlePlanIntent_NoMatchingCandidates(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handlePlanIntent(context.Background(), nil, codeParam{Intent: "xyzzynonexistentqqq"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected an error result when nothing matches, got: %s", resultText(t, result))
	}
}

func TestHandlePlanIntent_RequiresIntent(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handlePlanIntent(context.Background(), nil, codeParam{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected an error result for missing intent, got: %s", resultText(t, result))
	}
}

func TestOneLine_CollapsesWhitespace(t *testing.T) {
	got := oneLine("first line\n  second   line\nthird")
	want := "first line second line third"
	if got != want {
		t.Errorf("oneLine: got %q, want %q", got, want)
	}
}

func TestPlanCacheKey_ChangesWithBodyHash(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()

	d, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("GetDefinitionByName: %v", err)
	}
	cands := []contextCandidate{{Def: *d}}
	key1 := planCacheKey("some intent", cands)

	d.Hash = "different-hash"
	cands2 := []contextCandidate{{Def: *d}}
	key2 := planCacheKey("some intent", cands2)

	if key1 == key2 {
		t.Errorf("expected planCacheKey to change when a candidate's body hash changes")
	}
}
