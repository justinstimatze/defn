package summary

import (
	"context"
	"strings"
	"testing"
)

func TestBuildPlanPrompt_ContainsGoalAndCandidates(t *testing.T) {
	prompt := buildPlanPrompt("understand the request lifecycle", "Handler.ServeHTTP -- entry point\nHandler.proxyLoopIteration -- selects upstream")
	if !strings.Contains(prompt, "understand the request lifecycle") {
		t.Errorf("expected the goal in the prompt, got: %s", prompt)
	}
	if !strings.Contains(prompt, "Handler.ServeHTTP -- entry point") {
		t.Errorf("expected the candidate list in the prompt, got: %s", prompt)
	}
	if !strings.Contains(prompt, "read") || !strings.Contains(prompt, "outline") || !strings.Contains(prompt, "impact") {
		t.Errorf("expected the op vocabulary spelled out in the prompt, got: %s", prompt)
	}
}

func TestExplainClient_Plan_NilClientReturnsError(t *testing.T) {
	var e *ExplainClient
	_, err := e.Plan(context.Background(), "some intent", "some candidates")
	if err == nil {
		t.Fatal("expected an error from a nil *ExplainClient, got nil")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("expected the error to mention ANTHROPIC_API_KEY, got: %v", err)
	}
}
