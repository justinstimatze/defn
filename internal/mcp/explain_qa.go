package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// handleExplainWithQuestion is the #186 co-processor path for op:"explain"
// when a `question` param is set. Assembles def bodies for the scope
// (either args.Names or args.Name), passes to Sonnet with the question,
// returns synthesized answer + provenance refs. Falls back to a clear
// error when ANTHROPIC_API_KEY is unset (explainClient is nil).
//
// Scope: if Names is set, load each; if only Name is set, load that one.
// If no scope defs given, error — a question without context is a
// non-starter (the model isn't a general Go assistant, it's answering
// FROM the provided source).
//
// #192: cache hit skips both the source-body assembly and the Sonnet
// call. cacheKey is content-addressed on the question plus each scoped
// def's (qualified name, body hash), so an edited body naturally misses
// rather than needing explicit invalidation.
func (s *server) handleExplainWithQuestion(ctx context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	scope := args.Names
	if len(scope) == 0 && strings.TrimSpace(args.Name) != "" {
		scope = []string{args.Name}
	}
	if len(scope) == 0 {
		return errResult(fmt.Errorf("explain: scope is required — pass name:\"X\" or names:[\"X\",\"Y\"] to ground the question"))
	}

	items := make([]explainScopeItem, 0, len(scope))
	for _, name := range scope {
		// #248-class bug (2026-08-10, prometheus-18712): this used to
		// call GetDefinitionByName(name, "") unconditionally, ignoring
		// args.Module/args.File/args.Receiver entirely -- unlike every
		// other name-resolving op (outline, read, test, plain explain),
		// which all go through resolveEditTarget. An ambiguous name with
		// an explicit module:/file: disambiguator still silently
		// resolved to the wrong definition, and since explainCacheKey
		// hashes the RESOLVED def's identity, changing the disambiguator
		// between calls didn't even bust the cache -- the same wrong
		// answer kept getting served back as a "cache hit".
		d, err := s.resolveEditTarget(name, args.Receiver, args.Module, args.File)
		if err != nil {
			items = append(items, explainScopeItem{name: name})
			continue
		}
		items = append(items, explainScopeItem{name: name, def: d})
	}
	var refs []string
	for _, it := range items {
		if it.def != nil {
			refs = append(refs, formatReceiver(it.def.Receiver)+it.def.Name)
		}
	}
	if len(refs) == 0 {
		return errResult(fmt.Errorf("explain: none of the requested defs were found: %v", scope))
	}

	// #192: cache hit skips both the source-body assembly and the Sonnet
	// call -- and works even with no ANTHROPIC_API_KEY configured, same
	// as #212's file/project narrative cache-hit path.
	cacheKey := explainCacheKey(args.Question, items)
	if cached, err := s.backend.GetExplainCache(cacheKey); err == nil && cached != nil {
		text := formatExplainAnswer(cached.Answer, cached.Refs)
		return withUsage(textResult(text), usageStats{
			Op:            "explain-qa-cached",
			BytesReturned: len(text),
		}), nil, nil
	}

	if s.explainClient == nil {
		return errResult(fmt.Errorf("explain: co-processor unavailable (set ANTHROPIC_API_KEY to enable)"))
	}

	var sourceBuf strings.Builder
	for _, it := range items {
		if it.def == nil {
			sourceBuf.WriteString(fmt.Sprintf("// (definition %q not found)\n\n", it.name))
			continue
		}
		d := it.def
		sourceBuf.WriteString(fmt.Sprintf("// %s%s (%s) — %s:%d\n",
			formatReceiver(d.Receiver), d.Name, d.Kind, d.SourceFile, d.StartLine))
		if d.Doc != "" {
			sourceBuf.WriteString(d.Doc + "\n")
		}
		sourceBuf.WriteString(d.Body)
		sourceBuf.WriteString("\n\n")
	}

	answer, err := s.explainClient.Explain(ctx, args.Question, sourceBuf.String())
	if err != nil {
		return errResult(fmt.Errorf("explain: %w", err))
	}

	// Best effort — a cache write failure shouldn't fail a request that
	// already got its answer.
	_ = s.backend.SetExplainCache(cacheKey, args.Question, strings.Join(refs, ","), answer, "explain-co-processor", refs)

	text := formatExplainAnswer(answer, refs)
	return withUsage(textResult(text), usageStats{
		Op:            "explain-qa",
		BytesReturned: len(text),
		BytesAltRead:  sourceBuf.Len(),
	}), nil, nil
}

// explainCacheKey hashes the question together with each scoped def's
// qualified name and body hash (in scope order), so the same question
// against the same code always maps to the same key, and any edited
// body in scope produces a fresh one.
func explainCacheKey(question string, items []explainScopeItem) string {
	h := sha256.New()
	h.Write([]byte(question))
	for _, it := range items {
		h.Write([]byte{0})
		if it.def == nil {
			h.Write([]byte("missing:" + it.name))
			continue
		}
		h.Write([]byte(formatReceiver(it.def.Receiver) + it.def.Name))
		h.Write([]byte{0})
		h.Write([]byte(it.def.Hash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// formatExplainAnswer renders the final #186/#192 explain-QA response
// text, shared by both the cache-hit and fresh-generation paths.
func formatExplainAnswer(answer string, refs []string) string {
	var out strings.Builder
	out.WriteString("## Explanation\n\n")
	out.WriteString(answer)
	out.WriteString("\n\n_Grounded in: " + strings.Join(refs, ", ") + "_\n")
	return out.String()
}

// explainScopeItem pairs a requested #186/#192 explain scope name with
// its resolved definition (nil if the name wasn't found).
type explainScopeItem struct {
	name string
	def  *store.Definition
}
