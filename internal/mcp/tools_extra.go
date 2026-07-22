package mcp

import (
	"context"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *server) handleExplain(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.dolt.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	impact, err := s.dolt.GetImpact(d.ID)
	if err != nil {
		return errResult(err)
	}

	callees, _ := s.dolt.GetCallees(d.ID) // best effort — nil is safe

	var sb strings.Builder
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("# %s%s (%s)\n", recv, d.Name, d.Kind))

	sb.WriteString(fmt.Sprintf("Module: %s\n\n", impact.Module))

	// Doc.
	if d.Doc != "" {
		sb.WriteString(d.Doc + "\n")
	}

	// Signature.
	sb.WriteString("```go\n")
	sig := extractSignature(d.Body)
	sb.WriteString(sig + "\n")
	sb.WriteString("```\n\n")

	// What it calls.
	if len(callees) > 0 {
		sb.WriteString(fmt.Sprintf("**Calls %d definitions:**\n", len(callees)))
		for _, c := range callees {
			r := formatReceiver(c.Receiver)
			sb.WriteString(fmt.Sprintf("- %s%s\n", r, c.Name))
		}
		sb.WriteString("\n")
	}

	// Who calls it.
	sb.WriteString(fmt.Sprintf("**Called by %d definitions** (%d transitively)\n", len(impact.DirectCallers), impact.TransitiveCount))
	limit := 15
	for i, c := range impact.DirectCallers {
		if i >= limit {
			sb.WriteString(fmt.Sprintf("- ... and %d more\n", len(impact.DirectCallers)-limit))
			break
		}
		tag := ""
		if c.Test {
			tag = " [test]"
		}
		r := formatReceiver(c.Receiver)
		sb.WriteString(fmt.Sprintf("- %s%s%s\n", r, c.Name, tag))
	}

	// Test coverage.
	sb.WriteString(fmt.Sprintf("\n**Test coverage: %d tests**\n", len(impact.Tests)))
	if impact.UncoveredBy > 0 {
		sb.WriteString(fmt.Sprintf("**%d direct callers have no test coverage**\n", impact.UncoveredBy))
	}

	return textResult(sb.String()), nil, nil
}

func (s *server) handleMove(_ context.Context, _ *sdkmcp.CallToolRequest, args moveParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.dolt.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	// Find target module by fuzzy match.
	targetMod := s.findModule(args.ToModule)
	if targetMod == nil {
		return errResult(fmt.Errorf("target module %q not found", args.ToModule))
	}

	// Delete from old module first, then create in new module.
	if err := s.dolt.DeleteDefinition(d.ID); err != nil {
		return errResult(err)
	}
	d.ModuleID = targetMod.ID
	d.ID = 0 // force new insert
	if _, err := s.dolt.UpsertDefinition(d); err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuild()
	s.autoResolve("") // full resolve — move changes module membership

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Moved %s to %s\n", args.Name, targetMod.Path))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	return textResult(sb.String()), nil, nil
}
