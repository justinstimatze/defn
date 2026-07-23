package mcp

import (
	"context"
	"fmt"
	"strings"

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
func (s *server) handleExplainWithQuestion(ctx context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if s.explainClient == nil {
		return errResult(fmt.Errorf("explain: co-processor unavailable (set ANTHROPIC_API_KEY to enable)"))
	}
	scope := args.Names
	if len(scope) == 0 && strings.TrimSpace(args.Name) != "" {
		scope = []string{args.Name}
	}
	if len(scope) == 0 {
		return errResult(fmt.Errorf("explain: scope is required — pass name:\"X\" or names:[\"X\",\"Y\"] to ground the question"))
	}
	var sourceBuf strings.Builder
	var refs []string
	for _, name := range scope {
		d, err := s.backend.GetDefinitionByName(name, "")
		if err != nil {
			sourceBuf.WriteString(fmt.Sprintf("// (definition %q not found)\n\n", name))
			continue
		}
		refs = append(refs, formatReceiver(d.Receiver)+d.Name)
		sourceBuf.WriteString(fmt.Sprintf("// %s%s (%s) — %s:%d\n",
			formatReceiver(d.Receiver), d.Name, d.Kind, d.SourceFile, d.StartLine))
		if d.Doc != "" {
			sourceBuf.WriteString(d.Doc + "\n")
		}
		sourceBuf.WriteString(d.Body)
		sourceBuf.WriteString("\n\n")
	}
	if len(refs) == 0 {
		return errResult(fmt.Errorf("explain: none of the requested defs were found: %v", scope))
	}
	answer, err := s.explainClient.Explain(ctx, args.Question, sourceBuf.String())
	if err != nil {
		return errResult(fmt.Errorf("explain: %w", err))
	}
	var out strings.Builder
	out.WriteString("## Explanation\n\n")
	out.WriteString(answer)
	out.WriteString("\n\n_Grounded in: " + strings.Join(refs, ", ") + "_\n")
	text := out.String()
	return withUsage(textResult(text), usageStats{
		Op:            "explain-qa",
		BytesReturned: len(text),
		BytesAltRead:  sourceBuf.Len(),
	}), nil, nil
}
