package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *server) handleExplain(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	impact, err := s.db.GetImpact(d.ID)
	if err != nil {
		return errResult(err)
	}

	callees, _ := s.db.GetCallees(d.ID) // best effort — nil is safe

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
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	// Find target module by fuzzy match.
	targetMod := s.findModule(args.ToModule)
	if targetMod == nil {
		return errResult(fmt.Errorf("target module %q not found", args.ToModule))
	}

	// Delete from old module first, then create in new module.
	if err := s.db.DeleteDefinition(d.ID); err != nil {
		return errResult(err)
	}
	d.ModuleID = targetMod.ID
	d.ID = 0 // force new insert
	if _, err := s.db.UpsertDefinition(d); err != nil {
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

func (s *server) handleCodeDiff(_ context.Context, _ *sdkmcp.CallToolRequest, _ emptyParam) (*sdkmcp.CallToolResult, any, error) {
	status, err := s.db.Diff()
	if err != nil {
		return errResult(err)
	}

	if len(status) == 0 {
		return textResult("No uncommitted changes."), nil, nil
	}

	// Show Dolt status, then query for definition-level changes.
	var sb strings.Builder
	sb.WriteString("**Uncommitted changes:**\n\n")
	for _, s := range status {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", s["status"], s["table"]))
	}

	// Try to get definition-level diff from Dolt.
	defDiff, err := s.db.DiffDefinitions()
	if err == nil && len(defDiff) > 0 {
		sb.WriteString("\n**Definition changes:**\n")
		for _, d := range defDiff {
			diffType := fmt.Sprint(d["diff_type"])
			name := fmt.Sprint(d["to_name"])
			if name == "" || name == "<nil>" {
				name = fmt.Sprint(d["from_name"])
			}
			kind := fmt.Sprint(d["to_kind"])
			if kind == "" || kind == "<nil>" {
				kind = fmt.Sprint(d["from_kind"])
			}
			sb.WriteString(fmt.Sprintf("- %s: %s (%s)\n", diffType, name, kind))
		}
	}

	return textResult(sb.String()), nil, nil
}

func (s *server) handleHistory(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	// Query Dolt's diff history for a specific definition.
	// We look at commit_defs across commits to see how the body changed.
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	// Get commit log.
	log, err := s.db.Log(20)
	if err != nil {
		return errResult(err)
	}

	var sb strings.Builder
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("# History: %s%s\n\n", recv, d.Name))
	sb.WriteString(fmt.Sprintf("Current hash: %s\n\n", store.HashBody(d.Body)[:12]))

	// Show recent commits (Dolt doesn't easily let us filter by definition
	// without dolt_diff_definitions per commit pair — show all for now).
	sb.WriteString("**Recent commits:**\n")
	for _, entry := range log {
		hash := fmt.Sprint(entry["hash"])
		if len(hash) > 12 {
			hash = hash[:12]
		}
		sb.WriteString(fmt.Sprintf("- %s  %s  %s\n", hash, entry["date"], entry["message"]))
	}

	return textResult(sb.String()), nil, nil
}

func (s *server) handleBranch(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	// No branch arg → list.
	if strings.TrimSpace(args.Branch) == "" {
		current, _ := s.db.GetCurrentBranch()
		branches, err := s.db.ListBranches()
		if err != nil {
			return errResult(err)
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("**%d branches:**\n\n", len(branches)))
		for _, name := range branches {
			marker := "  "
			if name == current {
				marker = "* "
			}
			sb.WriteString(fmt.Sprintf("%s%s\n", marker, name))
		}
		return textResult(sb.String()), nil, nil
	}

	// Delete mode.
	if args.Force {
		if err := s.db.DeleteBranch(args.Branch, true); err != nil {
			return errResult(err)
		}
		return textResult(fmt.Sprintf("Deleted branch %s.", args.Branch)), nil, nil
	}

	// Create mode. With or without `from`.
	if args.From != "" {
		if err := s.db.BranchFrom(args.Branch, args.From); err != nil {
			return errResult(err)
		}
		return textResult(fmt.Sprintf("Created branch %s from %s.", args.Branch, args.From)), nil, nil
	}
	if err := s.db.Branch(args.Branch); err != nil {
		return errResult(err)
	}
	return textResult(fmt.Sprintf("Created branch %s.", args.Branch)), nil, nil
}

func (s *server) handleCheckout(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if err := s.db.Checkout(args.Branch); err != nil {
		return errResult(err)
	}
	return textResult(fmt.Sprintf("Switched to branch %s.", args.Branch)), nil, nil
}

func (s *server) handleMerge(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	current, _ := s.db.GetCurrentBranch()
	if err := s.db.Merge(args.Branch); err != nil {
		return errResult(err)
	}
	// With dolt_allow_commit_conflicts=1 the merge succeeds even when
	// conflicts exist — surface them instead of claiming success.
	conflicts, _ := s.db.Conflicts()
	if len(conflicts) > 0 {
		return textResult(fmt.Sprintf(
			"Merged %s into %s with %d conflict(s). Run code(op:\"conflicts\") to inspect, "+
				"code(op:\"resolve\", ...) to fix, code(op:\"commit\", message:...) to finalize, "+
				"or code(op:\"merge-abort\") to bail.",
			args.Branch, current, len(conflicts))), nil, nil
	}
	return textResult(fmt.Sprintf("Merged %s into %s.", args.Branch, current)), nil, nil
}

func (s *server) handleCommit(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if err := s.db.Commit(args.Message); err != nil {
		return errResult(err)
	}
	return textResult(fmt.Sprintf("Committed: %s", args.Message)), nil, nil
}

func (s *server) handleStatus(_ context.Context, _ *sdkmcp.CallToolRequest, _ codeParam) (*sdkmcp.CallToolResult, any, error) {
	current, err := s.db.GetCurrentBranch()
	if err != nil {
		return errResult(err)
	}
	status, err := s.db.Diff()
	if err != nil {
		return errResult(err)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**Branch:** %s\n", current))
	if len(status) == 0 {
		sb.WriteString("**State:** clean\n")
	} else {
		sb.WriteString(fmt.Sprintf("**State:** %d tables with uncommitted changes\n", len(status)))
		for _, row := range status {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", row["status"], row["table"]))
		}
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleConflicts(_ context.Context, _ *sdkmcp.CallToolRequest, _ codeParam) (*sdkmcp.CallToolResult, any, error) {
	conflicts, err := s.db.Conflicts()
	if err != nil {
		return errResult(err)
	}
	if len(conflicts) == 0 {
		return textResult("No unresolved conflicts."), nil, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**%d unresolved conflict(s):**\n\n", len(conflicts)))
	for _, c := range conflicts {
		recv := ""
		if c.Receiver != "" {
			recv = "(" + c.Receiver + ") "
		}
		sb.WriteString(fmt.Sprintf("### %s%s  (%s, def_id=%d)\n", recv, c.Name, c.Kind, c.DefID))
		sb.WriteString("**base:**\n```go\n" + c.Base + "\n```\n")
		sb.WriteString("**ours:**\n```go\n" + c.Ours + "\n```\n")
		sb.WriteString("**theirs:**\n```go\n" + c.Theirs + "\n```\n\n")
	}
	sb.WriteString("Resolve with `code(op:\"resolve\", name:\"<name>\", body:\"<new body>\")` ")
	sb.WriteString("or `code(op:\"resolve\", pick:\"ours\"|\"theirs\")` for bulk pick. ")
	sb.WriteString("Then `code(op:\"commit\", message:\"...\")` to finalize the merge.")
	return textResult(sb.String()), nil, nil
}

func (s *server) handleResolve(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	// Bulk-pick path.
	if args.Pick != "" {
		if err := s.db.ResolveAll(args.Pick); err != nil {
			return errResult(err)
		}
		return textResult(fmt.Sprintf("Resolved all conflicts (picked %s). Commit to finalize the merge.", args.Pick)), nil, nil
	}
	// Per-definition resolution: look up the def on the current branch.
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found: %w", args.Name, err))
	}
	if err := s.db.ResolveConflict(d.ID, args.Body); err != nil {
		return errResult(err)
	}
	return textResult(fmt.Sprintf("Resolved %s. Commit to finalize the merge.", args.Name)), nil, nil
}

func (s *server) handleMergeAbort(_ context.Context, _ *sdkmcp.CallToolRequest, _ codeParam) (*sdkmcp.CallToolResult, any, error) {
	if err := s.db.MergeAbort(); err != nil {
		return errResult(err)
	}
	return textResult("Merge aborted; working state restored."), nil, nil
}
