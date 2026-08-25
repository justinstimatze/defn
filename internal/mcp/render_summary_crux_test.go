package mcp

import (
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/store"
)

// TestRenderSummaryOnly_IncludesCruxWhenPresent confirms mode:"summary"'s
// compact response surfaces the crux excerpt (the load-bearing lines) in
// addition to the one-line intent, not just a restatement of the
// signature.
func TestRenderSummaryOnly_IncludesCruxWhenPresent(t *testing.T) {
	d := &store.Definition{Name: "F", Kind: "function", Signature: "func F(x int) int"}
	sum := &store.DefSummary{OneLine: "returns early on a negative input", Crux: "\tif x < 0 {\n\t\treturn 0\n\t}", Model: "claude-haiku-4-5"}
	text := resultTextRaw(renderSummaryOnly(d, sum))
	if !strings.Contains(text, "_crux:_") || !strings.Contains(text, "if x < 0 {") {
		t.Errorf("expected the crux excerpt in the summary response, got: %s", text)
	}
}

// TestRenderSummaryOnly_OmitsCruxSectionWhenEmpty confirms a def with no
// single focal span (empty Crux) gets no dangling "_crux:_" header.
func TestRenderSummaryOnly_OmitsCruxSectionWhenEmpty(t *testing.T) {
	d := &store.Definition{Name: "Name", Kind: "function", Signature: "func Name() string"}
	sum := &store.DefSummary{OneLine: "returns a constant name", Crux: "", Model: "claude-haiku-4-5"}
	text := resultTextRaw(renderSummaryOnly(d, sum))
	if strings.Contains(text, "_crux:_") {
		t.Errorf("expected no crux section for an empty Crux, got: %s", text)
	}
}
