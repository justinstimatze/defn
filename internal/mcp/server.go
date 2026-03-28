// Package mcp implements the MCP server that exposes the defn database
// to Claude Code.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/justinstimatze/defn/internal/emit"
	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxSearchResults = 20 // keep responses compact to avoid context bloat

var (
	buildTimeout = envDuration("DEFN_BUILD_TIMEOUT", 30*time.Second)
	testTimeout  = envDuration("DEFN_TEST_TIMEOUT", 60*time.Second)
)

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

type server struct {
	db           *store.DB
	projectDir   string
	lastResolved atomic.Int64 // UnixNano timestamp of last resolve (to debounce watcher)
}

// Run starts the MCP server. projDir is the project root where files
// should be emitted (for in-place sync with file-based tools).
func Run(ctx context.Context, database *store.DB, projDir string) error {
	s := &server{db: database, projectDir: projDir}

	if projDir != "" {
		// Reconcile changes made while defn was not running (file moves,
		// deletions, renames). Runs async so the MCP server starts immediately
		// and serves from whatever's in the DB. PruneStaleDefinitions removes ghosts.
		go func() {
			if err := ingest.Ingest(s.db, projDir); err != nil {
				fmt.Fprintf(os.Stderr, "defn: startup ingest failed: %v\n", err)
				return
			}
			if err := resolve.Resolve(s.db, projDir); err != nil {
				fmt.Fprintf(os.Stderr, "defn: startup resolve failed: %v\n", err)
				return
			}
			s.lastResolved.Store(time.Now().UnixNano())
		}()
		go s.watchFiles(ctx)
	}

	server := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    "defn",
		Version: "0.2.0",
	}, nil)

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name: "code",
		Description: `Go code database. One tool, many ops. Start with impact for blast radius — it returns callers, transitives, and test coverage in one call. Don't follow up with search/explain unless you need more.

Ops: impact (blast radius — START HERE), read, search, explain, similar, untested, edit (full body OR old_fragment+new_fragment), insert (after anchor), create, delete, rename, move, test, apply, diff, history, find, sync, query, overview, patch`,
	}, s.handleCode)

	return server.Run(ctx, &sdkmcp.StdioTransport{})
}

// --- Params ---

// codeParam is the unified parameter for the single "code" tool.
// Required fields per op:
//
//	read, impact, explain, delete, test, history: name
//	search: pattern (or name as fallback)
//	edit: name + new_body (full replace) OR name + old_fragment + new_fragment (fragment)
//	insert: name + after + body
//	create: body (+ optional module)
//	rename: old_name + new_name
//	move: name + module
//	find: file (+ optional line)
//	query: sql
//	apply: operations
//	untested, diff, sync: (no params)
type codeParam struct {
	Op         string           `json:"op"`
	Name       string           `json:"name,omitempty"`
	Pattern    string           `json:"pattern,omitempty"`
	Body       string           `json:"body,omitempty"`
	NewBody    string           `json:"new_body,omitempty"`
	Module     string           `json:"module,omitempty"`
	OldName    string           `json:"old_name,omitempty"`
	NewName    string           `json:"new_name,omitempty"`
	SQL        string           `json:"sql,omitempty"`
	File       string           `json:"file,omitempty"`
	Line       int              `json:"line,omitempty"`
	Names      []string         `json:"names,omitempty"`
	Mutations  []store.Mutation `json:"mutations,omitempty"`
	Depth      int              `json:"depth,omitempty"`
	Receiver   string           `json:"receiver,omitempty"`
	OldFragment string           `json:"old_fragment,omitempty"`
	NewFragment string           `json:"new_fragment,omitempty"`
	After       string           `json:"after,omitempty"`
	ReplaceAll  bool             `json:"replace_all,omitempty"`
	Operations  []applyOp        `json:"operations,omitempty"`
	DryRun      bool             `json:"dry_run,omitempty"`
}

type applyOp struct {
	Op          string `json:"op"`
	Name        string `json:"name"`
	NewName     string `json:"new_name"`
	Body        string `json:"body"`
	NewBody     string `json:"new_body"`
	Module      string `json:"module"`
	OldFragment string `json:"old_fragment"`
	NewFragment string `json:"new_fragment"`
	After       string `json:"after"`
	ReplaceAll  bool   `json:"replace_all"`
}

// Legacy param types used by internal handlers.
type nameParam struct {
	Name string `json:"name"`
}
type patternParam struct {
	Pattern string `json:"pattern"`
}
type editParam struct {
	Name    string `json:"name"`
	NewBody string `json:"new_body"`
}
type createParam struct {
	Body   string `json:"body"`
	Module string `json:"module,omitempty"`
}
type applyParam struct {
	Operations []applyOp `json:"operations"`
	DryRun     bool      `json:"dry_run,omitempty"`
}
type renameParam struct {
	OldName string `json:"old_name"`
	NewName string `json:"new_name"`
}
type sqlParam struct {
	SQL string `json:"sql"`
}
type emptyParam struct{}
type findParam struct {
	File string `json:"file"`
	Line int    `json:"line"`
}
type moveParam struct {
	Name     string `json:"name"`
	ToModule string `json:"to_module"`
}

// --- Helpers ---

func textResult(text string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text}},
	}
}

func errResult(err error) (*sdkmcp.CallToolResult, any, error) {
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: err.Error()}},
		IsError: true,
	}, nil, nil
}

func toJSON(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func formatReceiver(recv string) string {
	if recv == "" {
		return ""
	}
	return "(" + recv + ")."
}

func (s *server) findModule(query string) *store.Module {
	mods, _ := s.db.ListModules() // best effort — nil is safe
	for _, m := range mods {
		if strings.EqualFold(m.Name, query) ||
			strings.Contains(strings.ToLower(m.Path), strings.ToLower(query)) {
			return &m
		}
	}
	return nil
}

func (s *server) modulePath(moduleID int64) string {
	mods, _ := s.db.ListModules() // best effort — nil is safe
	for _, m := range mods {
		if m.ID == moduleID {
			return m.Path
		}
	}
	return ""
}

// --- Dispatch ---

// handleCode is the single entry point for all operations.
// It dispatches based on the "op" field to the appropriate handler.
func (s *server) handleCode(ctx context.Context, req *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	// Validate required params per op (fail fast with clear error).
	need := func(field, label string) (*sdkmcp.CallToolResult, any, error) {
		if strings.TrimSpace(field) == "" {
			return errResult(fmt.Errorf("%s: %s is required", args.Op, label))
		}
		return nil, nil, nil
	}

	switch args.Op {
	case "read", "impact", "explain", "delete", "test", "history", "similar":
		if r, o, e := need(args.Name, "name"); r != nil {
			return r, o, e
		}
	case "edit":
		if r, o, e := need(args.Name, "name"); r != nil {
			return r, o, e
		}
		// Fragment mode: old_fragment + new_fragment (new_fragment can be empty for deletion).
		// Full mode: new_body.
		if args.OldFragment == "" {
			body := args.NewBody
			if body == "" {
				body = args.Body
			}
			if r, o, e := need(body, "new_body (or old_fragment + new_fragment for fragment edit)"); r != nil {
				return r, o, e
			}
		}
	case "insert":
		if r, o, e := need(args.Name, "name"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.After, "after"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.Body, "body"); r != nil {
			return r, o, e
		}
	case "create":
		if r, o, e := need(args.Body, "body"); r != nil {
			return r, o, e
		}
	case "rename":
		if r, o, e := need(args.OldName, "old_name"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.NewName, "new_name"); r != nil {
			return r, o, e
		}
	case "move":
		if r, o, e := need(args.Name, "name"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.Module, "module"); r != nil {
			return r, o, e
		}
	case "query":
		if r, o, e := need(args.SQL, "sql"); r != nil {
			return r, o, e
		}
	case "find":
		if r, o, e := need(args.File, "file"); r != nil {
			return r, o, e
		}
	}

	switch args.Op {
	case "read":
		return s.handleGetDefinition(ctx, req, nameParam{Name: args.Name})
	case "search":
		p := args.Pattern
		if p == "" {
			p = args.Name
		}
		return s.handleSearch(ctx, req, patternParam{Pattern: p})
	case "impact":
		return s.handleImpact(ctx, req, nameParam{Name: args.Name})
	case "explain":
		return s.handleExplain(ctx, req, nameParam{Name: args.Name})
	case "untested":
		return s.handleUntested(ctx, req, emptyParam{})
	case "edit":
		if args.OldFragment != "" {
			return s.handleFragmentEdit(ctx, req, args)
		}
		body := args.NewBody
		if body == "" {
			body = args.Body
		}
		return s.handleEdit(ctx, req, editParam{Name: args.Name, NewBody: body})
	case "insert":
		return s.handleInsert(ctx, req, args)
	case "create":
		return s.handleCreate(ctx, req, createParam{Body: args.Body, Module: args.Module})
	case "delete":
		return s.handleDelete(ctx, req, nameParam{Name: args.Name})
	case "rename":
		return s.handleRename(ctx, req, renameParam{OldName: args.OldName, NewName: args.NewName})
	case "move":
		return s.handleMove(ctx, req, moveParam{Name: args.Name, ToModule: args.Module})
	case "test":
		return s.handleTest(ctx, req, nameParam{Name: args.Name})
	case "similar":
		return s.handleSimilar(ctx, req, nameParam{Name: args.Name})
	case "apply":
		return s.handleApply(ctx, req, applyParam{Operations: args.Operations, DryRun: args.DryRun})
	case "diff":
		return s.handleCodeDiff(ctx, req, emptyParam{})
	case "history":
		return s.handleHistory(ctx, req, nameParam{Name: args.Name})
	case "query":
		return s.handleQuery(ctx, req, sqlParam{SQL: args.SQL})
	case "find":
		return s.handleFind(ctx, req, findParam{File: args.File, Line: args.Line})
	case "overview":
		return s.handleOverview(ctx, req, args)
	case "patch":
		return s.handlePatch(ctx, req, args)
	case "sync":
		return s.handleSync(ctx, req, emptyParam{})
	case "test-coverage":
		return s.handleTestCoverage(ctx, req, args)
	case "batch-impact":
		return s.handleBatchImpact(ctx, req, args)
	case "simulate":
		return s.handleSimulate(ctx, req, args)
	case "file-defs":
		return s.handleFileDefs(ctx, req, args)
	default:
		return errResult(fmt.Errorf("unknown op %q — valid: read, search, impact, explain, similar, untested, edit, create, delete, rename, move, test, apply, diff, history, query, find, sync, test-coverage, batch-impact", args.Op))
	}
}

// --- Handlers ---

func (s *server) handleImpact(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}
	impact, err := s.db.GetImpact(d.ID)
	if err != nil {
		return errResult(err)
	}

	// Formatted markdown response.
	var sb strings.Builder
	recv := formatReceiver(impact.Definition.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", recv, impact.Definition.Name, impact.Definition.Kind))
	sb.WriteString(fmt.Sprintf("Module: %s\n\n", impact.Module))

	// Compact format: counts + caller names only (no test names — agent can get those via code(op:"test")).
	var prodCallers, testCallers []string
	for _, c := range impact.DirectCallers {
		name := formatReceiver(c.Receiver) + c.Name
		if c.Test {
			testCallers = append(testCallers, name)
		} else {
			prodCallers = append(prodCallers, name)
		}
	}
	sb.WriteString(fmt.Sprintf("Direct callers: %d (%d production, %d test)\n", len(impact.DirectCallers), len(prodCallers), len(testCallers)))
	for _, name := range prodCallers {
		sb.WriteString(fmt.Sprintf("  %s\n", name))
	}
	sb.WriteString(fmt.Sprintf("Transitive callers: %d\n", impact.TransitiveCount))
	sb.WriteString(fmt.Sprintf("Tests covering this: %d\n", len(impact.Tests)))
	if impact.UncoveredBy > 0 {
		sb.WriteString(fmt.Sprintf("Uncovered direct callers: %d\n", impact.UncoveredBy))
	}

	return textResult(sb.String()), nil, nil
}

func (s *server) handleGetDefinition(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	// Look up module path for this definition.
	var modulePath string
	mods, _ := s.db.ListModules() // best effort — nil is safe
	for _, m := range mods {
		if m.ID == d.ModuleID {
			modulePath = m.Path
			break
		}
	}

	var sb strings.Builder
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", recv, d.Name, d.Kind))
	sb.WriteString(fmt.Sprintf("Module: %s\n\n", modulePath))
	if d.Doc != "" {
		sb.WriteString(d.Doc + "\n\n")
	}
	sb.WriteString("```go\n")
	sb.WriteString(d.Body)
	sb.WriteString("\n```\n")

	return textResult(sb.String()), nil, nil
}

func (s *server) handleSearch(_ context.Context, _ *sdkmcp.CallToolRequest, args patternParam) (*sdkmcp.CallToolResult, any, error) {
	var defs []store.Definition
	var err error

	if strings.Contains(args.Pattern, "%") {
		// SQL LIKE pattern (e.g., "%Auth%").
		defs, err = s.db.FindDefinitions(args.Pattern)
	} else {
		// Search names/signatures first (indexed, fast).
		defs, err = s.db.FindDefinitions("%" + args.Pattern + "%")
		if err != nil || len(defs) == 0 {
			// Fall back to body/doc search (LIKE scan, slower).
			defs, err = s.db.SearchDefinitions(args.Pattern)
		}
	}
	if err != nil {
		return errResult(err)
	}
	type summary struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Receiver string `json:"receiver,omitempty"`
	}
	var results []summary
	for _, d := range defs {
		if len(results) >= maxSearchResults {
			break
		}
		results = append(results, summary{
			Name: d.Name, Kind: d.Kind, Receiver: d.Receiver,
		})
	}
	truncated := ""
	if len(defs) > maxSearchResults {
		truncated = fmt.Sprintf("\n(showing %d of %d results)", maxSearchResults, len(defs))
	}
	text, err := toJSON(results)
	if err != nil {
		return errResult(err)
	}
	if truncated != "" {
		text += truncated
	}
	return textResult(text), nil, nil
}

func (s *server) handleUntested(_ context.Context, _ *sdkmcp.CallToolRequest, _ emptyParam) (*sdkmcp.CallToolResult, any, error) {
	defs, err := s.db.GetUntested()
	if err != nil {
		return errResult(err)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d exported definitions without test coverage:\n\n", len(defs)))
	for _, d := range defs {
		recv := formatReceiver(d.Receiver)
		sb.WriteString(fmt.Sprintf("- %s%s (%s)\n", recv, d.Name, d.Kind))
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleEdit(_ context.Context, _ *sdkmcp.CallToolRequest, args editParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	// Validate new body parses as Go.
	src := "package x\n" + args.NewBody
	if _, parseErr := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments); parseErr != nil {
		return errResult(fmt.Errorf("new_body has syntax error: %v", parseErr))
	}

	d.Body = args.NewBody
	d.Signature = extractSignature(args.NewBody)

	id, err := s.db.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}

	recv := formatReceiver(d.Receiver)

	buildResult := s.autoEmitAndBuild()

	s.autoResolve(s.modulePath(d.ModuleID))

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Updated %s%s (id=%d, hash=%s)\n", recv, d.Name, id, store.HashBody(args.NewBody)[:12]))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}

	// Impact nudge: show callers if this definition has any.
	if impact, err := s.db.GetImpact(id); err == nil && len(impact.DirectCallers) > 0 {
		prodCallers := 0
		for _, c := range impact.DirectCallers {
			if !c.Test {
				prodCallers++
			}
		}
		sb.WriteString(fmt.Sprintf("\nFYI: %d callers, %d tests affected. Run code(op:\"test\", name:\"%s\") to verify.\n",
			prodCallers, len(impact.Tests), d.Name))
	}
	return textResult(sb.String()), nil, nil
}

// autoResolve re-runs ingest+resolve in-process to keep the reference graph
// current after edits. If modulePath is non-empty, only resolves references
// for that module (incremental — much faster). Falls back to full resolve
// if modulePath is empty.
// autoResolve updates the reference graph after a definition change.
// When modulePath is set, only resolves that module (incremental).
// Skips re-ingest — the DB was already updated by UpsertDefinition and
// files were emitted by autoEmitAndBuild. Re-ingesting would just re-read
// from disk what we just wrote.
func (s *server) autoResolve(modulePath string) {
	if s.projectDir == "" {
		return
	}
	// Best effort — don't fail the edit if resolve fails.
	if modulePath != "" {
		resolve.ResolveModule(s.db, s.projectDir, modulePath)
	} else {
		resolve.Resolve(s.db, s.projectDir)
	}
	s.lastResolved.Store(time.Now().UnixNano())
}

// watchFiles polls for .go file changes and auto-reingests when detected.
// This keeps the defn database in sync when files are edited outside defn
// (e.g. via Edit/Write tools, vim, or other processes).
func (s *server) watchFiles(ctx context.Context) {
	var lastMod int64 // 0 means first poll — debounce window handles startup race
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}

		// Check directory modtimes instead of stat-ing every .go file.
		// When a file changes, its parent directory's modtime updates.
		var newest int64
		filepath.Walk(s.projectDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || !info.IsDir() {
				return nil
			}
			base := filepath.Base(path)
			if base == ".defn" || base == ".defn-server" || base == ".git" || base == "vendor" || base == "node_modules" {
				return filepath.SkipDir
			}
			if mod := info.ModTime().UnixNano(); mod > newest {
				newest = mod
			}
			return nil
		})

		if newest > lastMod && lastMod > 0 {
			// Skip if startup ingest or autoResolve ran recently.
			if time.Now().UnixNano()-s.lastResolved.Load() < int64(10*time.Second) {
				lastMod = newest
				continue
			}
			// Files changed externally — re-ingest and resolve.
			ingest.Ingest(s.db, s.projectDir)
			resolve.Resolve(s.db, s.projectDir)
			s.lastResolved.Store(time.Now().UnixNano())
		}
		lastMod = newest
	}
}

// autoEmitAndBuild emits to the project directory (so file-based tools
// see the changes) and runs go build to verify.
// Set DEFN_LEGACY=1 to disable auto-emit (for projects where you want
// to edit files directly and use defn as a read-only acceleration layer).
func (s *server) autoEmitAndBuild() string {
	if s.projectDir == "" || os.Getenv("DEFN_LEGACY") == "1" {
		return "Saved to database."
	}

	// Emit to the actual project directory — keeps files in sync.
	if err := emit.Emit(s.db, s.projectDir); err != nil {
		return fmt.Sprintf("emit error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "./...")
	cmd.Dir = s.projectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("BUILD FAILED:\n%s", string(out))
	}
	return "Build: OK"
}

// extractSignature pulls the signature from a Go definition body.
// Handles multi-line signatures like func Foo(\n  param string,\n) {
// and skips braces inside type expressions like map[string]interface{}.
func extractSignature(body string) string {
	// Parse the body to extract the signature from the AST.
	src := "package x\n" + strings.TrimSpace(body)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil || len(f.Decls) == 0 {
		// Unparseable — return first non-comment line.
		for line := range strings.SplitSeq(body, "\n") {
			t := strings.TrimSpace(line)
			if t != "" && !strings.HasPrefix(t, "//") {
				return t
			}
		}
		return body
	}

	switch d := f.Decls[0].(type) {
	case *ast.FuncDecl:
		var sig strings.Builder
		sig.WriteString("func ")
		if d.Recv != nil && len(d.Recv.List) > 0 {
			sig.WriteString("(")
			sig.WriteString(types.ExprString(d.Recv.List[0].Type))
			sig.WriteString(") ")
		}
		sig.WriteString(d.Name.Name)
		// types.ExprString on FuncType produces "func(...) ...", strip the "func" prefix.
		funcSig := types.ExprString(d.Type)
		sig.WriteString(strings.TrimPrefix(funcSig, "func"))
		return sig.String()
	case *ast.GenDecl:
		if len(d.Specs) > 0 {
			switch s := d.Specs[0].(type) {
			case *ast.TypeSpec:
				return fmt.Sprintf("type %s", s.Name.Name)
			case *ast.ValueSpec:
				return fmt.Sprintf("%s %s", d.Tok, s.Names[0].Name)
			}
		}
	}
	return body
}

func (s *server) handleFragmentEdit(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	// Reject empty old_fragment (strings.ReplaceAll inserts between every char).
	if args.OldFragment == "" {
		return errResult(fmt.Errorf("old_fragment cannot be empty"))
	}

	// Check old_fragment exists in body.
	count := strings.Count(d.Body, args.OldFragment)
	if count == 0 {
		return errResult(fmt.Errorf("old_fragment not found in %s body", args.Name))
	}
	if count > 1 && !args.ReplaceAll {
		return errResult(fmt.Errorf("old_fragment matches %d times in %s — use replace_all:true to replace all, or provide a more specific fragment", count, args.Name))
	}

	var newBody string
	if args.ReplaceAll {
		newBody = strings.ReplaceAll(d.Body, args.OldFragment, args.NewFragment)
	} else {
		newBody = strings.Replace(d.Body, args.OldFragment, args.NewFragment, 1)
	}

	// Validate syntax BEFORE dry-run response.
	src := "package x\n" + newBody
	if _, parseErr := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments); parseErr != nil {
		return errResult(fmt.Errorf("fragment edit produces invalid Go: %v", parseErr))
	}

	if args.DryRun {
		return textResult(fmt.Sprintf("Dry run — would edit %s:\n\n--- old ---\n%s\n\n+++ new ---\n%s", args.Name, args.OldFragment, args.NewFragment)), nil, nil
	}

	d.Body = newBody
	d.Signature = extractSignature(newBody)
	recv := formatReceiver(d.Receiver)

	id, err := s.db.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuild()
	s.autoResolve(s.modulePath(d.ModuleID))

	var sb strings.Builder
	replaced := "1 occurrence"
	if args.ReplaceAll {
		replaced = fmt.Sprintf("%d occurrences", count)
	}
	sb.WriteString(fmt.Sprintf("Edited %s%s — replaced %s\n", recv, d.Name, replaced))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	if impact, err := s.db.GetImpact(id); err == nil && len(impact.DirectCallers) > 0 {
		prodCallers := 0
		for _, c := range impact.DirectCallers {
			if !c.Test {
				prodCallers++
			}
		}
		sb.WriteString(fmt.Sprintf("\nFYI: %d callers, %d tests affected.\n", prodCallers, len(impact.Tests)))
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleInsert(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	idx := strings.Index(d.Body, args.After)
	if idx < 0 {
		return errResult(fmt.Errorf("anchor text not found in %s body", args.Name))
	}

	insertAt := idx + len(args.After)
	newBody := d.Body[:insertAt] + args.Body + d.Body[insertAt:]

	// Validate syntax BEFORE dry-run response.
	insertSrc := "package x\n" + newBody
	if _, parseErr := parser.ParseFile(token.NewFileSet(), "", insertSrc, parser.ParseComments); parseErr != nil {
		return errResult(fmt.Errorf("insert produces invalid Go: %v", parseErr))
	}

	if args.DryRun {
		return textResult(fmt.Sprintf("Dry run — would insert into %s after %q:\n\n%s", args.Name, args.After, args.Body)), nil, nil
	}

	d.Body = newBody
	d.Signature = extractSignature(newBody)
	recv := formatReceiver(d.Receiver)

	if _, err := s.db.UpsertDefinition(d); err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuild()
	s.autoResolve(s.modulePath(d.ModuleID))

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Inserted into %s%s\n", recv, d.Name))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleCreate(_ context.Context, _ *sdkmcp.CallToolRequest, args createParam) (*sdkmcp.CallToolResult, any, error) {
	// Infer name, kind, and test flag from the body.
	name, kind, receiver, isTest := s.inferFromBody(args.Body)
	if name == "" {
		return errResult(fmt.Errorf("couldn't infer definition name from body — make sure it starts with func/type/const/var"))
	}

	// Find module: use provided, or default to first.
	var mod *store.Module
	if args.Module != "" {
		mod = s.findModule(args.Module)
	}
	if mod == nil {
		mods, _ := s.db.ListModules() // best effort — nil is safe
		if len(mods) > 0 {
			mod = &mods[0]
		}
	}
	if mod == nil {
		return errResult(fmt.Errorf("no modules found — run defn init first"))
	}

	// Check if a definition with this name already exists in the target module.
	if existing, err := s.db.GetDefinitionByName(name, mod.Path); err == nil {
		recv := formatReceiver(existing.Receiver)
		return errResult(fmt.Errorf("definition %s%s already exists in %s (id=%d) — use code(op:\"edit\") to modify it", recv, name, mod.Path, existing.ID))
	}

	exported := len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
	d := &store.Definition{
		ModuleID:  mod.ID,
		Name:      name,
		Kind:      kind,
		Exported:  exported,
		Test:      isTest,
		Receiver:  receiver,
		Signature: extractSignature(args.Body),
		Body:      args.Body,
	}
	id, err := s.db.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuild()
	s.autoResolve(mod.Path)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Created %s (id=%d, kind=%s) in %s\n", name, id, kind, mod.Path))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	return textResult(sb.String()), nil, nil
}

// inferFromBody extracts definition name, kind, receiver, and test flag from Go source.
func (s *server) inferFromBody(body string) (name, kind, receiver string, isTest bool) {
	// Parse the body as a Go source file to extract definition metadata.
	src := "package x\n" + strings.TrimSpace(body)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil || len(f.Decls) == 0 {
		return // unparseable — caller will report error
	}

	switch d := f.Decls[0].(type) {
	case *ast.FuncDecl:
		name = d.Name.Name
		if d.Recv != nil && len(d.Recv.List) > 0 {
			kind = "method"
			receiver = types.ExprString(d.Recv.List[0].Type)
		} else {
			kind = "function"
		}
	case *ast.GenDecl:
		switch d.Tok {
		case token.TYPE:
			if len(d.Specs) > 0 {
				ts := d.Specs[0].(*ast.TypeSpec)
				name = ts.Name.Name
				if _, ok := ts.Type.(*ast.InterfaceType); ok {
					kind = "interface"
				} else {
					kind = "type"
				}
			}
		case token.CONST:
			if len(d.Specs) > 0 {
				vs := d.Specs[0].(*ast.ValueSpec)
				name = vs.Names[0].Name
				kind = "const"
			}
		case token.VAR:
			if len(d.Specs) > 0 {
				vs := d.Specs[0].(*ast.ValueSpec)
				name = vs.Names[0].Name
				kind = "var"
			}
		}
	}

	if name != "" {
		isTest = strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark")
	}
	return
}

func (s *server) handleApply(_ context.Context, _ *sdkmcp.CallToolRequest, args applyParam) (*sdkmcp.CallToolResult, any, error) {
	var sb strings.Builder
	var errors []string

	// Dry-run: validate all operations without executing.
	if args.DryRun {
		for _, op := range args.Operations {
			switch op.Op {
			case "create":
				name, kind, _, _ := s.inferFromBody(op.Body)
				if name == "" {
					errors = append(errors, "create: couldn't infer name from body")
				} else {
					sb.WriteString(fmt.Sprintf("+ would create %s (%s)\n", name, kind))
				}
			case "edit":
				if _, err := s.db.GetDefinitionByName(op.Name, ""); err != nil {
					errors = append(errors, fmt.Sprintf("edit %s: not found", op.Name))
				} else {
					sb.WriteString(fmt.Sprintf("~ would edit %s\n", op.Name))
				}
			case "delete":
				if _, err := s.db.GetDefinitionByName(op.Name, ""); err != nil {
					errors = append(errors, fmt.Sprintf("delete %s: not found", op.Name))
				} else {
					sb.WriteString(fmt.Sprintf("- would delete %s\n", op.Name))
				}
			case "rename":
				if op.Name == "" || op.NewName == "" {
					errors = append(errors, "rename: both name and new_name are required")
				} else if _, err := s.db.GetDefinitionByName(op.Name, ""); err != nil {
					errors = append(errors, fmt.Sprintf("rename %s: not found", op.Name))
				} else {
					sb.WriteString(fmt.Sprintf("→ would rename %s → %s\n", op.Name, op.NewName))
				}
			default:
				errors = append(errors, fmt.Sprintf("unknown op: %s", op.Op))
			}
		}
		if len(errors) > 0 {
			sb.WriteString("\nErrors:\n")
			for _, e := range errors {
				sb.WriteString("- " + e + "\n")
			}
		}
		sb.WriteString("\n(dry run — no changes made)")
		return textResult(sb.String()), nil, nil
	}

	// Wrap in transaction for atomicity.
	commit, rollback, txErr := s.db.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback() // rollback if we don't commit

	for _, op := range args.Operations {
		switch op.Op {
		case "create":
			name, kind, receiver, isTest := s.inferFromBody(op.Body)
			if name == "" {
				errors = append(errors, "create: couldn't infer name from body")
				continue
			}
			var mod *store.Module
			if op.Module != "" {
				mod = s.findModule(op.Module)
			}
			if mod == nil {
				mods, _ := s.db.ListModules() // best effort — nil is safe
				if len(mods) > 0 {
					mod = &mods[0]
				}
			}
			if mod == nil {
				errors = append(errors, "create: no modules found")
				continue
			}
			exported := len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
			d := &store.Definition{
				ModuleID: mod.ID, Name: name, Kind: kind, Exported: exported,
				Test: isTest, Receiver: receiver, Signature: extractSignature(op.Body), Body: op.Body,
			}
			id, err := s.db.UpsertDefinition(d)
			if err != nil {
				errors = append(errors, fmt.Sprintf("create %s: %v", name, err))
			} else {
				sb.WriteString(fmt.Sprintf("+ created %s (id=%d)\n", name, id))
			}

		case "edit":
			d, err := s.db.GetDefinitionByName(op.Name, "")
			if err != nil {
				errors = append(errors, fmt.Sprintf("edit %s: not found", op.Name))
				continue
			}
			if op.OldFragment != "" {
				// Fragment mode.
				count := strings.Count(d.Body, op.OldFragment)
				if count == 0 {
					errors = append(errors, fmt.Sprintf("edit %s: old_fragment not found", op.Name))
					continue
				}
				if count > 1 && !op.ReplaceAll {
					errors = append(errors, fmt.Sprintf("edit %s: old_fragment matches %d times, use replace_all:true", op.Name, count))
					continue
				}
				if op.ReplaceAll {
					d.Body = strings.ReplaceAll(d.Body, op.OldFragment, op.NewFragment)
				} else {
					d.Body = strings.Replace(d.Body, op.OldFragment, op.NewFragment, 1)
				}
			} else {
				body := op.NewBody
				if body == "" {
					body = op.Body
				}
				d.Body = body
			}
			// Validate syntax before saving.
			validSrc := "package x\n" + d.Body
			if _, parseErr := parser.ParseFile(token.NewFileSet(), "", validSrc, parser.ParseComments); parseErr != nil {
				errors = append(errors, fmt.Sprintf("edit %s: produces invalid Go: %v", op.Name, parseErr))
				continue
			}
			d.Signature = extractSignature(d.Body)
			if _, err := s.db.UpsertDefinition(d); err != nil {
				errors = append(errors, fmt.Sprintf("edit %s: %v", op.Name, err))
			} else {
				sb.WriteString(fmt.Sprintf("~ edited %s\n", op.Name))
			}

		case "delete":
			d, err := s.db.GetDefinitionByName(op.Name, "")
			if err != nil {
				errors = append(errors, fmt.Sprintf("delete %s: not found", op.Name))
				continue
			}
			if err := s.db.DeleteDefinition(d.ID); err != nil {
				errors = append(errors, fmt.Sprintf("delete %s: %v", op.Name, err))
			} else {
				sb.WriteString(fmt.Sprintf("- deleted %s\n", op.Name))
			}

		case "rename":
			if op.Name == "" || op.NewName == "" {
				errors = append(errors, "rename: both name and new_name are required")
				continue
			}
			d, err := s.db.GetDefinitionByName(op.Name, "")
			if err != nil {
				errors = append(errors, fmt.Sprintf("rename %s: not found", op.Name))
				continue
			}
			d.Body, _ = astRename(d.Body, op.Name, op.NewName)
			d.Name = op.NewName
			d.Signature = extractSignature(d.Body)
			d.Exported = len(op.NewName) > 0 && op.NewName[0] >= 'A' && op.NewName[0] <= 'Z'
			if _, err := s.db.UpsertDefinition(d); err != nil {
				errors = append(errors, fmt.Sprintf("rename %s: %v", op.Name, err))
				continue
			}
			// Update callers (same as handleRename).
			callers, _ := s.db.GetCallers(d.ID) // best effort
			callerCount := 0
			for _, caller := range callers {
				if strings.Contains(caller.Body, op.Name) {
					caller.Body, _ = astRename(caller.Body, op.Name, op.NewName)
					caller.Signature = extractSignature(caller.Body)
					if _, err := s.db.UpsertDefinition(&caller); err != nil {
						errors = append(errors, fmt.Sprintf("rename caller %s: %v", caller.Name, err))
					} else {
						callerCount++
					}
				}
			}
			sb.WriteString(fmt.Sprintf("→ renamed %s → %s (%d callers updated)\n", op.Name, op.NewName, callerCount))

		default:
			errors = append(errors, fmt.Sprintf("unknown op: %s", op.Op))
		}
	}

	if len(errors) > 0 {
		sb.WriteString(fmt.Sprintf("\n%d errors:\n", len(errors)))
		for _, e := range errors {
			sb.WriteString("  " + e + "\n")
		}
	}

	// Commit transaction if no errors.
	if len(errors) > 0 {
		// Rollback happens via defer. Don't commit partial state.
		sb.WriteString("\nErrors (transaction rolled back):\n")
		for _, e := range errors {
			sb.WriteString("- " + e + "\n")
		}
		return textResult(sb.String()), nil, nil
	}
	if err := commit(); err != nil {
		return errResult(fmt.Errorf("commit: %w", err))
	}

	// One emit + build for all changes.
	buildResult := s.autoEmitAndBuild()
	s.autoResolve("") // full resolve — batch may touch multiple modules
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}

	return textResult(sb.String()), nil, nil
}

func (s *server) handleDelete(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	// Show what we're about to delete.
	recv := formatReceiver(d.Receiver)

	if err := s.db.DeleteDefinition(d.ID); err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuild()
	s.autoResolve("") // full resolve — deletion may affect other modules' references

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Deleted %s%s (id=%d)\n", recv, d.Name, d.ID))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleRename(_ context.Context, _ *sdkmcp.CallToolRequest, args renameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.OldName, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.OldName))
	}

	// Update the definition name in its own body using AST rename.
	// Only renames identifiers — preserves comments and string literals.
	totalSkipped := 0
	d.Body, _ = astRename(d.Body, args.OldName, args.NewName)
	d.Name = args.NewName
	d.Signature = extractSignature(d.Body)
	d.Exported = len(args.NewName) > 0 && args.NewName[0] >= 'A' && args.NewName[0] <= 'Z'

	if _, err := s.db.UpsertDefinition(d); err != nil {
		return errResult(err)
	}

	// Update all callers' bodies that reference the old name.
	callers, err := s.db.GetCallers(d.ID)
	if err != nil {
		return errResult(fmt.Errorf("get callers for rename: %w", err))
	}
	updated := 0
	for _, caller := range callers {
		if strings.Contains(caller.Body, args.OldName) {
			var skipped int
			caller.Body, skipped = astRename(caller.Body, args.OldName, args.NewName)
			totalSkipped += skipped
			caller.Signature = extractSignature(caller.Body)
			if _, err := s.db.UpsertDefinition(&caller); err != nil {
				return errResult(fmt.Errorf("update caller %s: %w", caller.Name, err))
			}
			updated++
		}
	}

	buildResult := s.autoEmitAndBuild()
	s.autoResolve("") // full resolve — rename touches callers across modules

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Renamed %s → %s\n", args.OldName, args.NewName))
	sb.WriteString(fmt.Sprintf("Updated %d callers\n", updated))
	if totalSkipped > 0 {
		sb.WriteString(fmt.Sprintf("\nNote: %d local variable(s) named %q were preserved (not renamed).\n", totalSkipped, args.OldName))
	}
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleTest(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	impact, err := s.db.GetImpact(d.ID)
	if err != nil {
		return errResult(err)
	}

	if len(impact.Tests) == 0 {
		return textResult(fmt.Sprintf("No tests cover %s. Nothing to run.", args.Name)), nil, nil
	}

	if s.projectDir == "" {
		return errResult(fmt.Errorf("no project directory configured"))
	}

	// Ensure files are current.
	if err := emit.Emit(s.db, s.projectDir); err != nil {
		return errResult(fmt.Errorf("emit: %w", err))
	}

	// Build the -run regex from test names (escape metacharacters).
	var testNames []string
	for _, t := range impact.Tests {
		testNames = append(testNames, regexp.QuoteMeta(t.Name))
	}
	runPattern := "^(" + strings.Join(testNames, "|") + ")$"

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-run", runPattern, "-count=1", "-v", "./...")
	cmd.Dir = s.projectDir
	out, err := cmd.CombinedOutput()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Running %d of %d tests (affected by %s):\n\n",
		len(testNames), len(testNames), args.Name))
	sb.WriteString(string(out))

	if err != nil {
		sb.WriteString("\nSOME TESTS FAILED")
	} else {
		sb.WriteString("\nALL TESTS PASSED")
	}

	return textResult(sb.String()), nil, nil
}

func (s *server) handleQuery(_ context.Context, _ *sdkmcp.CallToolRequest, args sqlParam) (*sdkmcp.CallToolResult, any, error) {
	results, err := s.db.Query(args.SQL)
	if err != nil {
		return errResult(err)
	}
	text, err := toJSON(results)
	if err != nil {
		return errResult(err)
	}
	return textResult(text), nil, nil
}

func (s *server) handleSync(_ context.Context, _ *sdkmcp.CallToolRequest, _ emptyParam) (*sdkmcp.CallToolResult, any, error) {
	if s.projectDir == "" {
		return errResult(fmt.Errorf("no project directory configured"))
	}
	if err := ingest.Ingest(s.db, s.projectDir); err != nil {
		return errResult(fmt.Errorf("ingest: %w", err))
	}
	if err := resolve.Resolve(s.db, s.projectDir); err != nil {
		return errResult(fmt.Errorf("resolve: %w", err))
	}
	return textResult("Synced: re-ingested source and rebuilt reference graph."), nil, nil
}

func (s *server) handleSimilar(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}
	if d.Signature == "" {
		return errResult(fmt.Errorf("definition %q has no signature", args.Name))
	}

	// Find definitions with similar signatures by searching for shared type tokens.
	// Extract type names from the signature (e.g., "func Foo(ctx context.Context, id int) error"
	// → search for "context.Context" and "error").
	sig := d.Signature
	// Strip func keyword, receiver, and name to get just the params/returns.
	if idx := strings.Index(sig, "("); idx >= 0 {
		sig = sig[idx:]
	}

	// Find definitions with similar param/return signatures.
	sigDefs, _ := s.db.FindDefinitions("%" + sig + "%")

	// Deduplicate, exclude self.
	seen := map[string]bool{d.Name: true}
	type match struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Receiver  string `json:"receiver,omitempty"`
		Signature string `json:"signature"`
	}
	var matches []match
	for _, c := range sigDefs {
		key := c.Name + c.Receiver
		if seen[key] || c.Signature == "" {
			continue
		}
		seen[key] = true
		matches = append(matches, match{
			Name: c.Name, Kind: c.Kind, Receiver: c.Receiver, Signature: c.Signature,
		})
		if len(matches) >= 20 {
			break
		}
	}

	if len(matches) == 0 {
		return textResult(fmt.Sprintf("No definitions with similar signatures to %s", args.Name)), nil, nil
	}

	text, err := toJSON(matches)
	if err != nil {
		return errResult(err)
	}
	return textResult(fmt.Sprintf("Definitions with similar signatures to %s:\n\n%s", args.Name, text)), nil, nil
}

func (s *server) handleOverview(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	file := args.File
	if file == "" {
		file = args.Name
	}
	if strings.TrimSpace(file) == "" {
		return errResult(fmt.Errorf("overview: file or name is required"))
	}

	// Strip filename to get package directory.
	dir := file
	if idx := strings.LastIndex(dir, "/"); idx >= 0 {
		dir = dir[:idx]
	} else {
		dir = strings.TrimSuffix(dir, "_test.go")
		dir = strings.TrimSuffix(dir, ".go")
	}

	defs, err := s.db.FindDefinitionsByFile(dir, 0)
	if err != nil || len(defs) == 0 {
		return errResult(fmt.Errorf("no definitions found for %s", file))
	}

	// Get full definitions with bodies to check relationships.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## %s (%d definitions)\n\n", file, len(defs)))

	// Group by source file.
	byFile := map[string][]store.Definition{}
	for _, d := range defs {
		f := d.SourceFile
		if f == "" {
			f = "(unknown)"
		}
		byFile[f] = append(byFile[f], d)
	}

	for f, fileDefs := range byFile {
		if len(byFile) > 1 {
			sb.WriteString(fmt.Sprintf("### %s\n", f))
		}
		for _, d := range fileDefs {
			recv := formatReceiver(d.Receiver)
			sb.WriteString(fmt.Sprintf("- %s%s (%s)", recv, d.Name, d.Kind))

			// Show caller/callee counts.
			full, err := s.db.GetDefinition(d.ID)
			if err != nil {
				sb.WriteString("\n")
				continue
			}
			callers, _ := s.db.GetCallers(full.ID)
			callees, _ := s.db.GetCallees(full.ID)
			prodCallers := 0
			for _, c := range callers {
				if !c.Test {
					prodCallers++
				}
			}
			if prodCallers > 0 || len(callees) > 0 {
				sb.WriteString(fmt.Sprintf(" — %d callers, %d callees", prodCallers, len(callees)))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	return textResult(sb.String()), nil, nil
}

func (s *server) handlePatch(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.Name) == "" {
		return errResult(fmt.Errorf("patch: name is required"))
	}
	if args.OldName == "" || args.NewName == "" {
		return errResult(fmt.Errorf("patch: old_name and new_name are required (the old and new text)"))
	}

	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	if !strings.Contains(d.Body, args.OldName) {
		return errResult(fmt.Errorf("old text not found in %s body", args.Name))
	}

	d.Body = strings.Replace(d.Body, args.OldName, args.NewName, 1)
	d.Signature = extractSignature(d.Body)

	if _, err := s.db.UpsertDefinition(d); err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuild()
	s.autoResolve(s.modulePath(d.ModuleID))

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Patched %s: replaced %q → %q\n", args.Name, args.OldName, args.NewName))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	return textResult(sb.String()), nil, nil
}

// astRename renames identifiers in Go source using go/parser.
// Only renames *ast.Ident nodes — comments and string literals are preserved.
// Falls back to string replacement if the source can't be parsed.
func astRename(body, oldName, newName string) (string, int) {
	src := "package x\n" + body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return strings.ReplaceAll(body, oldName, newName), 0
	}

	// Collect locally-declared identifiers so we don't rename them.
	// A local var/param named "Render" shouldn't be renamed when we're
	// renaming the package-level "Render" definition.
	localDecls := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			// Params, receiver, results are local declarations.
			if d.Recv != nil {
				for _, field := range d.Recv.List {
					for _, name := range field.Names {
						localDecls[name] = true
					}
				}
			}
			if d.Type.Params != nil {
				for _, field := range d.Type.Params.List {
					for _, name := range field.Names {
						localDecls[name] = true
					}
				}
			}
			if d.Type.Results != nil {
				for _, field := range d.Type.Results.List {
					for _, name := range field.Names {
						localDecls[name] = true
					}
				}
			}
		case *ast.AssignStmt:
			if d.Tok == token.DEFINE { // :=
				for _, lhs := range d.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok {
						localDecls[ident] = true
					}
				}
			}
		case *ast.ValueSpec: // var/const inside function
			for _, name := range d.Names {
				localDecls[name] = true
			}
		case *ast.RangeStmt:
			if key, ok := d.Key.(*ast.Ident); ok && d.Tok == token.DEFINE {
				localDecls[key] = true
			}
			if val, ok := d.Value.(*ast.Ident); ok && d.Tok == token.DEFINE {
				localDecls[val] = true
			}
		}
		return true
	})

	// Rename only non-local identifiers matching oldName.
	skipped := 0
	ast.Inspect(f, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok || ident.Name != oldName {
			return true
		}
		if localDecls[ident] {
			skipped++
			return true
		}
		ident.Name = newName
		return true
	})
	var buf strings.Builder
	if err := format.Node(&buf, fset, f); err != nil {
		// format.Node failed — return original body unchanged rather than
		// silently falling back to string replacement (which would corrupt
		// comments and strings).
		return body, 0
	}
	result := buf.String()
	// Strip the "package x\n" prefix we added for parsing.
	if idx := strings.Index(result, "\n"); idx >= 0 {
		result = strings.TrimLeft(result[idx+1:], "\n")
	} else {
		// No newline — format.Node returned something unexpected. Return original.
		return body, 0
	}
	return result, skipped
}

func (s *server) handleFind(_ context.Context, _ *sdkmcp.CallToolRequest, args findParam) (*sdkmcp.CallToolResult, any, error) {
	if args.File == "" {
		return errResult(fmt.Errorf("file is required"))
	}

	// Strip filename to get the package directory path for module matching.
	// If there's no directory separator, the input is just a filename — strip
	// the .go extension and use the base name for fuzzy module matching.
	dir := args.File
	if idx := strings.LastIndex(dir, "/"); idx >= 0 {
		dir = dir[:idx]
	} else {
		dir = strings.TrimSuffix(dir, "_test.go")
		dir = strings.TrimSuffix(dir, ".go")
	}

	defs, err := s.db.FindDefinitionsByFile(dir, args.Line)
	if err != nil {
		return errResult(err)
	}
	if len(defs) == 0 {
		return errResult(fmt.Errorf("no definitions found at %s:%d", args.File, args.Line))
	}

	var sb strings.Builder
	if args.Line > 0 {
		sb.WriteString(fmt.Sprintf("Definition at %s:%d:\n\n", args.File, args.Line))
	} else {
		sb.WriteString(fmt.Sprintf("Definitions in %s:\n\n", args.File))
	}
	for _, d := range defs {
		recv := formatReceiver(d.Receiver)
		sb.WriteString(fmt.Sprintf("- %s%s (%s) lines %d-%d\n", recv, d.Name, d.Kind, d.StartLine, d.EndLine))
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleFileDefs(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	file := args.File
	if file == "" {
		file = args.Name
	}
	if strings.TrimSpace(file) == "" {
		return errResult(fmt.Errorf("file-defs: file is required"))
	}
	// Strip filename to get package directory.
	dir := file
	if idx := strings.LastIndex(dir, "/"); idx >= 0 {
		dir = dir[:idx]
	} else {
		dir = strings.TrimSuffix(dir, "_test.go")
		dir = strings.TrimSuffix(dir, ".go")
	}
	defs, err := s.db.FindDefinitionsByFile(dir, 0)
	if err != nil {
		return errResult(err)
	}
	type defSummary struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Receiver  string `json:"receiver,omitempty"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	var results []defSummary
	for _, d := range defs {
		results = append(results, defSummary{
			Name: d.Name, Kind: d.Kind, Receiver: d.Receiver,
			StartLine: d.StartLine, EndLine: d.EndLine,
		})
	}
	text, err := toJSON(results)
	if err != nil {
		return errResult(err)
	}
	return textResult(fmt.Sprintf("%d definitions in %s:\n\n%s", len(results), file, text)), nil, nil
}

func (s *server) handleSimulate(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if len(args.Mutations) == 0 {
		return errResult(fmt.Errorf("simulate: mutations is required"))
	}
	result, err := s.db.Simulate(args.Mutations)
	if err != nil {
		return errResult(fmt.Errorf("simulate: %w", err))
	}
	text, err := toJSON(result)
	if err != nil {
		return errResult(err)
	}
	return textResult(text), nil, nil
}

func (s *server) handleTestCoverage(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.Name) == "" {
		return errResult(fmt.Errorf("test-coverage: name is required"))
	}
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}
	impact, err := s.db.GetImpact(d.ID)
	if err != nil {
		return errResult(err)
	}

	type testInfo struct {
		Name string `json:"name"`
	}
	var tests []testInfo
	for _, t := range impact.Tests {
		tests = append(tests, testInfo{Name: t.Name})
	}

	result := map[string]any{
		"definition":         args.Name,
		"test_count":         len(tests),
		"transitive_callers": impact.TransitiveCount,
		"tests":              tests,
	}
	text, err := toJSON(result)
	if err != nil {
		return errResult(err)
	}
	return textResult(text), nil, nil
}

func (s *server) handleBatchImpact(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	names := args.Names
	if len(names) == 0 && args.Name != "" {
		names = []string{args.Name}
	}
	if len(names) == 0 {
		return errResult(fmt.Errorf("batch-impact: names is required"))
	}

	allCallers := map[string]bool{}
	allTests := map[string]bool{}
	var perDef []map[string]any

	for _, name := range names {
		d, err := s.db.GetDefinitionByName(name, "")
		if err != nil {
			perDef = append(perDef, map[string]any{"name": name, "error": "not found"})
			continue
		}
		impact, err := s.db.GetImpact(d.ID)
		if err != nil {
			perDef = append(perDef, map[string]any{"name": name, "error": err.Error()})
			continue
		}
		for _, c := range impact.DirectCallers {
			allCallers[formatReceiver(c.Receiver)+c.Name] = true
		}
		for _, t := range impact.Tests {
			allTests[t.Name] = true
		}
		perDef = append(perDef, map[string]any{
			"name":               formatReceiver(d.Receiver) + d.Name,
			"direct_callers":     len(impact.DirectCallers),
			"transitive_callers": impact.TransitiveCount,
			"tests":              len(impact.Tests),
		})
	}

	result := map[string]any{
		"definitions":      perDef,
		"combined_callers": len(allCallers),
		"combined_tests":   len(allTests),
	}
	text, err := toJSON(result)
	if err != nil {
		return errResult(err)
	}
	return textResult(text), nil, nil
}
