package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/justinstimatze/defn/internal/emit"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *server) handleExplain(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.resolveEditTarget(args.Name, args.Receiver, args.Module, args.File)
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	impact, err := s.backend.GetImpact(d.ID)
	if err != nil {
		return errResult(err)
	}

	callees, _ := s.backend.GetCallees(d.ID) // best effort — nil is safe

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
	d, err := s.resolveWriteTarget(args.Name, args.Receiver, "", args.File)
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}
	if msg := unsupportedFieldOp(d.Kind, "move"); msg != "" {
		return errResult(fmt.Errorf("%s", msg))
	}

	// Find target module by fuzzy match.
	targetMod := s.findModule(args.ToModule)
	if targetMod == nil {
		return errResult(fmt.Errorf("target module %q not found", args.ToModule))
	}

	// emitModule places a def under its SourceFile's directory prefix
	// when one is present -- left unchanged, the moved def keeps
	// pointing at its OLD package's file even after ModuleID changes, so
	// emit re-splices it back into the OLD file (which still declares
	// the OLD package) and never actually writes it under the new
	// module's directory. Confirmed via a live probe: move reported
	// success while the source tree stayed byte-identical. Derive a new
	// project-relative path from an existing sibling def's directory in
	// the target module (falling back to a bare basename, which
	// emitModule's own pkgDir-from-module-path fallback still places
	// correctly) so the def's on-disk location actually follows its new
	// module.
	oldSourceFile := d.SourceFile
	newSourceFile := ""
	if oldSourceFile != "" {
		base := filepath.Base(oldSourceFile)
		if siblings, sErr := s.backend.GetModuleDefinitions(targetMod.ID); sErr == nil {
			for _, sib := range siblings {
				if sib.SourceFile != "" {
					newSourceFile = filepath.ToSlash(filepath.Join(filepath.Dir(sib.SourceFile), base))
					break
				}
			}
		}
		if newSourceFile == "" {
			newSourceFile = base
		}
	}

	// Identity key emit's merge safety net matches on -- same one
	// handleDelete whitelists through AllowedRemovals. Move needs BOTH
	// AllowedRemovals (drop the decl from its old file) and AllowedAdds
	// (land it in the new one); without these the merge net treats the
	// removal as "on-disk-only, leave it" and the add as unmatched
	// drift, so a move reports success while nothing actually relocates.
	identity := d.Name
	if d.Kind == "function" || d.Kind == "method" {
		identity = emit.FuncIdentity(d.Name, d.Receiver)
	}

	// #12-class gap: this used to write straight to s.backend with no
	// transaction at all -- every other write handler (handleDelete,
	// handleEdit, handleRename) already gates through Begin()/
	// commitOrRollbackOnBuild. Confirmed via a live probe: moving a
	// METHOD across packages (unconditionally illegal under Go's own
	// rules -- a method's receiver type must live in the method's own
	// package) produced a broken build AND a durably-committed DB row
	// claiming the method now belongs to the target module, silently
	// diverging DB state from a tree that no longer builds.
	tx, commit, rollback, txErr := s.backend.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback()

	// Delete from old module first, then create in new module.
	if err := tx.DeleteDefinition(d.ID); err != nil {
		return errResult(err)
	}
	d.ModuleID = targetMod.ID
	d.SourceFile = newSourceFile
	d.ID = 0 // force new insert
	newID, err := tx.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}
	d.ID = newID

	// Scope emit to just the two files this move touches -- besides
	// being the existing perf convention for singleton mutations,
	// scoping is required here for correctness: AllowedRemovals/
	// AllowedAdds match by bare identity name with no file or module
	// qualifier, so an unscoped full emit would let a same-named
	// decl in any OTHER package (a common Go shape -- New, String,
	// Close, Validate...) get silently spliced out as collateral
	// damage the instant its name happens to match.
	opts := emit.Opts{
		AllowedRemovals: []string{identity},
		AllowedAdds:     []string{identity},
	}
	var touched []string
	if oldSourceFile != "" {
		touched = append(touched, oldSourceFile)
	}
	if newSourceFile != "" && newSourceFile != oldSourceFile {
		touched = append(touched, newSourceFile)
	}
	if len(touched) > 0 {
		opts.GoimportsFiles = touched
		opts.TouchedFiles = touched
	}

	buildResult := s.commitOrRollbackOnBuild(tx, commit, rollback, opts)

	var sb strings.Builder
	if buildResult != "" {
		fmt.Fprintf(&sb, "move %s → %s rolled back — nothing was saved\n\n%s", args.Name, targetMod.Path, buildResult)
		return textResult(sb.String()), nil, nil
	}

	// #160: module changed → summary prompt now sees the new module
	// path, so intent may read differently. Enqueue against the fresh
	// row (new ID after delete+insert). Only reached once the move is
	// known-durable — matches handleDelete's success-only gating.
	s.enqueueSummary(d)
	s.autoResolve("") // full resolve — move changes module membership

	sb.WriteString(fmt.Sprintf("Moved %s to %s\n", args.Name, targetMod.Path))
	return textResult(sb.String()), nil, nil
}
