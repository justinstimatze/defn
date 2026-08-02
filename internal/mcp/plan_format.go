package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/justinstimatze/defn/internal/planformat"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// dropTestCallerLines implements the "[!test]" / "!test" filter:
// strips test-labeled entries from a rendered callers section. The
// section's header count (e.g. "callers (5 -- 3 production, 2 test)")
// is left as rendered rather than rewritten -- this is a prototype
// filter for #187's format decision, not the final #186 implementation.
func dropTestCallerLines(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(l, "_(test)_") {
			continue
		}
		out = append(out, l)
	}
	out = append(out, "_(test callers filtered by [!test])_")
	return strings.Join(out, "\n")
}

// handlePlanDSL is #188's mechanical-expansion prototype for #187
// (code(op:"plan")): the calling model emits a dense multi-step
// trajectory in the compact DSL format, defn parses it and walks each
// step via the same per-def renderer code(op:"expand") already uses
// (renderExpandSection) -- one round trip instead of one per step. See
// internal/planformat's doc comment and TestCorpusFormatComparison for
// the byte-cost measurement that fed #187's format decision.
func (s *server) handlePlanDSL(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	steps, err := planformat.ParseDSL(args.Plan)
	if err != nil {
		return errResult(fmt.Errorf("plan-dsl: %w", err))
	}
	return s.runPlanSteps(steps)
}

// handlePlanSExpr is #189's mechanical-expansion prototype -- see
// handlePlanDSL's doc comment for the shared design.
func (s *server) handlePlanSExpr(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	steps, err := planformat.ParseSExpr(args.Plan)
	if err != nil {
		return errResult(fmt.Errorf("plan-sexpr: %w", err))
	}
	return s.runPlanSteps(steps)
}

// runPlanSteps mechanically walks a parsed trajectory: resolve each
// step's target, render its requested field via the same section
// renderer expand uses, and concatenate. Unresolvable targets are
// skipped with a note rather than failing the whole plan, mirroring
// #210's expand-with-names behavior.
func (s *server) runPlanSteps(steps []planformat.Step) (*sdkmcp.CallToolResult, any, error) {
	if len(steps) == 0 {
		return errResult(fmt.Errorf("plan: no steps parsed"))
	}

	mods, _ := s.backend.ListModules()
	modulePathByID := make(map[int64]string, len(mods))
	for _, m := range mods {
		modulePathByID[m.ID] = m.Path
	}

	var sb strings.Builder
	var notFound []string
	resolved := 0
	for _, step := range steps {
		d, err := s.backend.GetDefinitionByName(step.Target, "")
		if err != nil {
			notFound = append(notFound, step.Target)
			continue
		}
		if resolved > 0 {
			sb.WriteString("\n---\n\n")
		}
		var section strings.Builder
		if err := s.renderExpandSection(&section, d, modulePathByID[d.ModuleID], map[string]bool{step.Field: true}); err != nil {
			return errResult(fmt.Errorf("plan: gather %s for %s: %w", step.Field, step.Target, err))
		}
		text := section.String()
		if step.Field == "callers" && step.ExcludeTest {
			text = dropTestCallerLines(text)
		}
		sb.WriteString(text)
		resolved++
	}

	if resolved == 0 {
		return errResult(fmt.Errorf("plan: none of the %d step target(s) resolved: %s", len(steps), strings.Join(notFound, ", ")))
	}
	if len(notFound) > 0 {
		sb.WriteString(fmt.Sprintf("\n_note: not found, skipped: %s_\n", strings.Join(notFound, ", ")))
	}

	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "plan",
		BytesReturned: len(out),
	}), nil, nil
}
