package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// handlePlanIntent is #186's Opus/Sonnet-driven half of code(op:"plan"):
// given a natural-language intent, ground it in real candidate defs
// (reusing #195/#197/#198's shared candidate search via
// gatherContextCandidates), ask the co-processor to emit an
// S-expression trajectory referencing only those candidates (#187's
// decision -- see internal/planformat), then mechanically expand it
// via runPlanSteps, the same walker code(op:"plan-sexpr") uses.
//
// Cached by (intent, candidate-set) hash -- see planCacheKey -- the
// same explain_cache table #192 already uses, so a repeated intent
// against unchanged code skips the co-processor call entirely. A
// cached response that no longer parses (e.g. the trajectory grammar
// changed since it was cached) falls through to a fresh call rather
// than wedging the op.
func (s *server) handlePlanIntent(ctx context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	intent := strings.TrimSpace(args.Intent)
	if intent == "" {
		return errResult(fmt.Errorf("plan: intent is required — pass intent:\"...\" describing what you want to explore"))
	}

	scored, _, err := s.gatherContextCandidates(intent)
	if err != nil {
		return errResult(fmt.Errorf("plan: %w", err))
	}
	const planCandidateCap = 20
	if len(scored) > planCandidateCap {
		scored = scored[:planCandidateCap]
	}

	candLines := make([]string, 0, len(scored))
	for _, c := range scored {
		name := formatReceiver(c.Def.Receiver) + c.Def.Name
		desc := c.Summary
		if desc == "" {
			desc = c.Def.Signature
		}
		if desc == "" {
			desc = c.Def.Doc
		}
		candLines = append(candLines, fmt.Sprintf("%s -- %s", name, oneLine(desc)))
	}
	candidatesText := strings.Join(candLines, "\n")

	cacheKey := planCacheKey(intent, scored)
	if cached, err := s.backend.GetExplainCache(cacheKey); err == nil && cached != nil {
		if steps, perr := planformat.ParseSExpr(cached.Answer); perr == nil {
			return s.runPlanSteps(steps)
		}
	}

	if s.explainClient == nil {
		return errResult(fmt.Errorf("plan: co-processor unavailable (set ANTHROPIC_API_KEY to enable) — use code(op:\"plan-sexpr\", plan:\"...\") to walk a hand-written trajectory instead"))
	}

	raw, err := s.explainClient.Plan(ctx, intent, candidatesText)
	if err != nil {
		return errResult(fmt.Errorf("plan: %w", err))
	}

	steps, err := planformat.ParseSExpr(raw)
	if err != nil {
		return errResult(fmt.Errorf("plan: co-processor response didn't parse as a trajectory: %w\n\nraw response:\n%s", err, raw))
	}

	// Best effort -- a cache write failure shouldn't fail a request that
	// already got its trajectory.
	_ = s.backend.SetExplainCache(cacheKey, intent, candidatesText, raw, "plan-co-processor", nil)

	return s.runPlanSteps(steps)
}

// oneLine collapses a doc comment or signature to a single line for
// the plan prompt's candidate list -- newlines in a multi-line doc
// would otherwise break the "one candidate per line" format the
// prompt promises the model.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

// planCacheKey hashes the intent together with the qualified name +
// body hash of each grounding candidate, in ranked order -- mirrors
// explainCacheKey's approach (#192) so an edited body or a reranked
// candidate set naturally produces a fresh key instead of needing
// explicit invalidation.
func planCacheKey(intent string, cands []contextCandidate) string {
	h := sha256.New()
	h.Write([]byte(intent))
	for _, c := range cands {
		h.Write([]byte{0})
		h.Write([]byte(formatReceiver(c.Def.Receiver) + c.Def.Name))
		h.Write([]byte{0})
		h.Write([]byte(c.Def.Hash))
	}
	return hex.EncodeToString(h.Sum(nil))
}
