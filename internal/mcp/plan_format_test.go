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
