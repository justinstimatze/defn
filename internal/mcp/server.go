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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/justinstimatze/defn/internal/emit"
	"github.com/justinstimatze/defn/internal/goload"
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
	db              *store.DB
	projectDir      string
	lastResolved    atomic.Int64 // UnixNano timestamp of last resolve (to debounce watcher)
	ready           atomic.Bool  // true after startup ingest+resolve completes
	autoCommitCount atomic.Int64 // counts auto-commits; triggers GC every 50
}

// Run starts the MCP server over stdio. projDir is the project root where
// files should be emitted (for in-place sync with file-based tools).
func Run(ctx context.Context, database *store.DB, projDir string) error {
	_, mcpServer := newMCPServer(ctx, database, projDir)
	return mcpServer.Run(ctx, &sdkmcp.StdioTransport{})
}

// RunHTTP starts the MCP server over HTTP/SSE on addr (e.g. ":9420").
// Multiple clients can connect to the same server, sharing one defn process.
func RunHTTP(ctx context.Context, database *store.DB, projDir, addr string) error {
	_, mcpServer := newMCPServer(ctx, database, projDir)
	handler := sdkmcp.NewSSEHandler(func(*http.Request) *sdkmcp.Server {
		return mcpServer
	}, nil)
	fmt.Fprintf(os.Stderr, "defn: listening on %s\n", addr)
	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	return srv.ListenAndServe()
}

// RunShared starts an HTTP/SSE server on addr and simultaneously serves
// this client over stdio. Used for auto-sharing: first session starts the
// HTTP daemon; subsequent sessions proxy to it via RunProxy.
func RunShared(ctx context.Context, database *store.DB, projDir, addr string) error {
	_, mcpServer := newMCPServer(ctx, database, projDir)

	// Start HTTP/SSE in background.
	handler := sdkmcp.NewSSEHandler(func(*http.Request) *sdkmcp.Server {
		return mcpServer
	}, nil)
	fmt.Fprintf(os.Stderr, "defn: shared server on %s\n", addr)
	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "defn: http server error: %v\n", err)
		}
	}()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	// Serve this client over stdio (blocks until client disconnects).
	return mcpServer.Run(ctx, &sdkmcp.StdioTransport{})
}

// RunProxy bridges a stdio MCP client to an existing HTTP/SSE defn server.
// This is the lightweight path (~5 MB) for the second+ session.
func RunProxy(ctx context.Context, sseEndpoint string) error {
	// Connect stdio side.
	stdioConn, err := (&sdkmcp.StdioTransport{}).Connect(ctx)
	if err != nil {
		return fmt.Errorf("stdio connect: %w", err)
	}
	defer stdioConn.Close()

	// Connect to the SSE server.
	sseConn, err := (&sdkmcp.SSEClientTransport{Endpoint: sseEndpoint}).Connect(ctx)
	if err != nil {
		return fmt.Errorf("sse connect: %w", err)
	}
	defer sseConn.Close()

	// Bridge: stdio → SSE and SSE → stdio.
	errc := make(chan error, 2)
	go func() {
		for {
			msg, err := stdioConn.Read(ctx)
			if err != nil {
				errc <- err
				return
			}
			if err := sseConn.Write(ctx, msg); err != nil {
				errc <- err
				return
			}
		}
	}()
	go func() {
		for {
			msg, err := sseConn.Read(ctx)
			if err != nil {
				errc <- err
				return
			}
			if err := stdioConn.Write(ctx, msg); err != nil {
				errc <- err
				return
			}
		}
	}()
	return <-errc
}

// newMCPServer creates the internal server state and MCP server instance.
// Shared by both stdio and HTTP transports.
func newMCPServer(ctx context.Context, database *store.DB, projDir string) (*server, *sdkmcp.Server) {
	s := &server{db: database, projectDir: projDir}

	if projDir != "" {
		// Reconcile changes made while defn was not running (file moves,
		// deletions, renames). Runs async so the MCP server starts within
		// the client's connection timeout. Queries before completion serve
		// from whatever's in the DB; results include a staleness notice.
		go func() {
			if err := s.ingestAndResolve(); err != nil {
				// "connection is already closed" means the DB was torn
				// down mid-ingest (stdin EOF → db.Close()). Not a real
				// startup failure; stay quiet.
				if !strings.Contains(err.Error(), "connection is already closed") {
					fmt.Fprintf(os.Stderr, "defn: startup ingest/resolve failed: %v\n", err)
				}
			}
			s.ready.Store(true)
		}()
		go s.watchFiles(ctx)
	}

	mcpServer := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    "defn",
		Version: "0.13.0",
	}, nil)

	sdkmcp.AddTool(mcpServer, &sdkmcp.Tool{
		Name: "code",
		Description: `Go code database. One tool, many ops. Start with impact for blast radius — it returns callers, transitives, and test coverage in one call. Don't follow up with search/explain unless you need more.

Ops: impact (blast radius — START HERE; pass format:"json" for structured output), read, search, explain, similar, untested, edit (full body OR old_fragment+new_fragment), insert (after anchor), create, delete, rename, move, test, apply, diff, history, find, sync (pass file:"path" for fast single-file sync), query, overview, patch, simulate, validate-plan, pragmas (query comment pragmas), literals (query composite literal fields), traverse (recursive graph traversal), branch (list/create/delete — pass from to branch from a source, force to delete), checkout (switch branch), merge (merge branch into current), commit (snapshot current state), status (current branch + dirty state), conflicts (list unresolved merge conflicts), resolve (name+body OR pick:"ours"/"theirs"), merge-abort (cancel in-progress merge), diff-defs (definitions that differ between two refs — pass from:"X" and optionally to:"Y"; defaults to working tree)`,
	}, s.handleCode)

	return s, mcpServer
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
//	branch: (none to list; branch + optional from to create; branch + force=true to delete)
//	checkout: branch
//	merge: branch
//	commit: message
//	status: (no params)
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
	Format      string           `json:"format,omitempty"`
	Limit       int              `json:"limit,omitempty"`
	Direction   string           `json:"direction,omitempty"`
	RefKinds    []string         `json:"ref_kinds,omitempty"`
	Branch      string           `json:"branch,omitempty"`
	From        string           `json:"from,omitempty"`
	Message     string           `json:"message,omitempty"`
	Force       bool             `json:"force,omitempty"`
	Pick        string           `json:"pick,omitempty"`
	To          string           `json:"to,omitempty"`
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
	case "validate-plan":
		if len(args.Mutations) == 0 {
			return errResult(fmt.Errorf("validate-plan: mutations is required"))
		}
	case "traverse":
		if r, o, e := need(args.Name, "name"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.Direction, "direction"); r != nil {
			return r, o, e
		}
		if args.Direction != "callers" && args.Direction != "callees" {
			return errResult(fmt.Errorf("traverse: direction must be 'callers' or 'callees', got %q", args.Direction))
		}
	case "checkout", "merge":
		if r, o, e := need(args.Branch, "branch"); r != nil {
			return r, o, e
		}
	case "commit":
		if r, o, e := need(args.Message, "message"); r != nil {
			return r, o, e
		}
	case "branch":
		// Deleting requires branch; creating requires branch; listing needs nothing.
		if args.Force && strings.TrimSpace(args.Branch) == "" {
			return errResult(fmt.Errorf("branch: force requires branch"))
		}
	case "resolve":
		// Either (name + body) for a custom resolution, or pick=ours|theirs
		// for a one-shot shortcut. name alone with no body/pick is an error.
		if args.Pick != "" {
			if args.Pick != "ours" && args.Pick != "theirs" {
				return errResult(fmt.Errorf("resolve: pick must be 'ours' or 'theirs', got %q", args.Pick))
			}
		} else {
			if r, o, e := need(args.Name, "name"); r != nil {
				return r, o, e
			}
			if r, o, e := need(args.Body, "body (or pick:'ours'|'theirs')"); r != nil {
				return r, o, e
			}
		}
	case "diff-defs":
		if r, o, e := need(args.From, "from"); r != nil {
			return r, o, e
		}
	}

	// Tag results from read-only ops while startup ingest is still running.
	stale := !s.ready.Load() && s.projectDir != ""
	wrapStale := func(r *sdkmcp.CallToolResult, o any, e error) (*sdkmcp.CallToolResult, any, error) {
		if stale && r != nil && !r.IsError {
			if len(r.Content) > 0 {
				if tc, ok := r.Content[0].(*sdkmcp.TextContent); ok {
					tc.Text = "[startup ingest in progress — results may be stale]\n\n" + tc.Text
				}
			}
		}
		return r, o, e
	}

	switch args.Op {
	case "read":
		return wrapStale(s.handleGetDefinition(ctx, req, nameParam{Name: args.Name}))
	case "search":
		if args.Pattern == "" {
			args.Pattern = args.Name
		}
		return wrapStale(s.handleSearch(ctx, req, args))
	case "impact":
		return wrapStale(s.handleImpact(ctx, req, args))
	case "explain":
		return wrapStale(s.handleExplain(ctx, req, nameParam{Name: args.Name}))
	case "untested":
		return wrapStale(s.handleUntested(ctx, req, emptyParam{}))
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
		return wrapStale(s.handleSimilar(ctx, req, nameParam{Name: args.Name}))
	case "apply":
		return s.handleApply(ctx, req, applyParam{Operations: args.Operations, DryRun: args.DryRun})
	case "diff":
		return wrapStale(s.handleCodeDiff(ctx, req, emptyParam{}))
	case "history":
		return wrapStale(s.handleHistory(ctx, req, nameParam{Name: args.Name}))
	case "query":
		return wrapStale(s.handleQuery(ctx, req, sqlParam{SQL: args.SQL}))
	case "find":
		return wrapStale(s.handleFind(ctx, req, findParam{File: args.File, Line: args.Line}))
	case "overview":
		return wrapStale(s.handleOverview(ctx, req, args))
	case "patch":
		return s.handlePatch(ctx, req, args)
	case "sync":
		return s.handleSync(ctx, req, args)
	case "test-coverage":
		return wrapStale(s.handleTestCoverage(ctx, req, args))
	case "batch-impact":
		return wrapStale(s.handleBatchImpact(ctx, req, args))
	case "simulate":
		return s.handleSimulate(ctx, req, args)
	case "file-defs":
		return s.handleFileDefs(ctx, req, args)
	case "validate-plan":
		return wrapStale(s.handleValidatePlan(ctx, req, args))
	case "pragmas":
		return wrapStale(s.handlePragmas(ctx, req, args))
	case "literals":
		return wrapStale(s.handleLiterals(ctx, req, args))
	case "traverse":
		return wrapStale(s.handleTraverse(ctx, req, args))
	case "branch":
		return s.handleBranch(ctx, req, args)
	case "checkout":
		return s.handleCheckout(ctx, req, args)
	case "merge":
		return s.handleMerge(ctx, req, args)
	case "commit":
		return s.handleCommit(ctx, req, args)
	case "status":
		return wrapStale(s.handleStatus(ctx, req, args))
	case "conflicts":
		return wrapStale(s.handleConflicts(ctx, req, args))
	case "resolve":
		return s.handleResolve(ctx, req, args)
	case "merge-abort":
		return s.handleMergeAbort(ctx, req, args)
	case "diff-defs":
		return wrapStale(s.handleDiffDefs(ctx, req, args))
	default:
		return errResult(fmt.Errorf("unknown op %q — valid: read, search, impact, explain, similar, untested, edit, create, delete, rename, move, test, apply, diff, history, query, find, sync, test-coverage, batch-impact, simulate, validate-plan, pragmas, literals, traverse, branch, checkout, merge, commit, status, conflicts, resolve, merge-abort, diff-defs", args.Op))
	}
}

// --- Handlers ---

func (s *server) handleImpact(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}
	impact, err := s.db.GetImpact(d.ID)
	if err != nil {
		return errResult(err)
	}

	if args.Format == "json" {
		return s.impactJSON(impact)
	}

	// Formatted markdown response.
	var sb strings.Builder
	recv := formatReceiver(impact.Definition.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", recv, impact.Definition.Name, impact.Definition.Kind))
	sb.WriteString(fmt.Sprintf("Module: %s\n\n", impact.Module))

	// Compact format: caller names with file:line locations.
	var prodCallers, testCallers []store.Definition
	for _, c := range impact.DirectCallers {
		if c.Test {
			testCallers = append(testCallers, c)
		} else {
			prodCallers = append(prodCallers, c)
		}
	}
	sb.WriteString(fmt.Sprintf("Direct callers: %d (%d production, %d test)\n", len(impact.DirectCallers), len(prodCallers), len(testCallers)))
	for _, c := range prodCallers {
		name := formatReceiver(c.Receiver) + c.Name
		if c.SourceFile != "" && c.StartLine > 0 {
			sb.WriteString(fmt.Sprintf("  %s  (%s:%d)\n", name, c.SourceFile, c.StartLine))
		} else {
			sb.WriteString(fmt.Sprintf("  %s\n", name))
		}
	}
	sb.WriteString(fmt.Sprintf("Transitive callers: %d\n", impact.TransitiveCount))
	sb.WriteString(fmt.Sprintf("Tests covering this: %d\n", len(impact.Tests)))
	if impact.UncoveredBy > 0 {
		sb.WriteString(fmt.Sprintf("Uncovered direct callers: %d\n", impact.UncoveredBy))
	}

	return textResult(sb.String()), nil, nil
}

type impactDefRef struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Receiver   string `json:"receiver,omitempty"`
	SourceFile string `json:"source_file"`
	StartLine  int    `json:"start_line,omitempty"`
	Test       bool   `json:"test,omitempty"`
}

func (s *server) impactJSON(impact *store.Impact) (*sdkmcp.CallToolResult, any, error) {
	blastRadius := "low"
	if impact.TransitiveCount > 20 {
		blastRadius = "high"
	} else if impact.TransitiveCount > 5 {
		blastRadius = "medium"
	}

	toRef := func(d store.Definition) impactDefRef {
		return impactDefRef{
			Name:       d.Name,
			Kind:       d.Kind,
			Receiver:   d.Receiver,
			SourceFile: d.SourceFile,
			StartLine:  d.StartLine,
			Test:       d.Test,
		}
	}

	callers := make([]impactDefRef, 0, len(impact.DirectCallers))
	for _, c := range impact.DirectCallers {
		callers = append(callers, toRef(c))
	}
	ifaceDispatch := make([]impactDefRef, 0, len(impact.InterfaceDispatchCallers))
	for _, c := range impact.InterfaceDispatchCallers {
		ifaceDispatch = append(ifaceDispatch, toRef(c))
	}
	tests := make([]impactDefRef, 0, len(impact.Tests))
	for _, t := range impact.Tests {
		tests = append(tests, toRef(t))
	}

	result := map[string]any{
		"definition": impactDefRef{
			Name:       impact.Definition.Name,
			Kind:       impact.Definition.Kind,
			Receiver:   impact.Definition.Receiver,
			SourceFile: impact.Definition.SourceFile,
			StartLine:  impact.Definition.StartLine,
		},
		"module":                    impact.Module,
		"direct_callers":            callers,
		"interface_dispatch_callers": ifaceDispatch,
		"transitive_count":          impact.TransitiveCount,
		"tests":                     tests,
		"uncovered_by":              impact.UncoveredBy,
		"blast_radius":              blastRadius,
	}
	text, err := toJSON(result)
	if err != nil {
		return errResult(err)
	}
	return textResult(text), nil, nil
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

func (s *server) handleSearch(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
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

	limit := maxSearchResults
	if args.Limit > 0 {
		limit = args.Limit
	}

	type summary struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Receiver string `json:"receiver,omitempty"`
	}
	var results []summary
	for _, d := range defs {
		if len(results) >= limit {
			break
		}
		results = append(results, summary{
			Name: d.Name, Kind: d.Kind, Receiver: d.Receiver,
		})
	}
	truncated := ""
	if len(defs) > limit {
		truncated = fmt.Sprintf("\n(showing %d of %d results — pass limit:<n> to see more)", limit, len(defs))
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
	s.autoCommit()
	s.lastResolved.Store(time.Now().UnixNano())
}

// autoCommit commits the working set with an auto-generated message.
// This keeps Dolt's working set small so chunk accumulation doesn't
// cause storage bloat. No-op if nothing changed. Runs GC every 50
// auto-commits to compact the noms store.
func (s *server) autoCommit() {
	s.db.Commit("auto-sync")
	s.db.CleanTempFiles()
	if n := s.autoCommitCount.Add(1); n%50 == 0 {
		go s.db.GC() // background — GC can be slow on large databases
	}
}

// ingestAndResolve loads packages once and runs both ingest and resolve
// against the shared result, avoiding a redundant packages.Load (~1-2 GB).
func (s *server) ingestAndResolve() error {
	pkgs, err := goload.LoadAll(s.projectDir)
	if err != nil {
		return fmt.Errorf("load packages: %w", err)
	}
	if err := ingest.IngestPackages(s.db, pkgs, s.projectDir); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	if err := resolve.ResolvePackages(s.db, pkgs, s.projectDir); err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	s.autoCommit()
	s.lastResolved.Store(time.Now().UnixNano())
	return nil
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
			// Files changed externally — shared load for ingest+resolve.
			s.ingestAndResolve()
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

func (s *server) handleSync(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if s.projectDir == "" {
		return errResult(fmt.Errorf("no project directory configured"))
	}

	// Fast path: sync a single file without full packages.Load.
	if args.File != "" {
		filePath := args.File
		if !filepath.IsAbs(filePath) {
			filePath = filepath.Join(s.projectDir, filePath)
		}
		n, err := ingest.IngestFile(s.db, s.projectDir, filePath)
		if err != nil {
			return errResult(fmt.Errorf("ingest file: %w", err))
		}
		s.autoCommit()
		return textResult(fmt.Sprintf("Synced %s: updated %d definitions.", args.File, n)), nil, nil
	}

	// Full sync: re-ingest all packages and rebuild references.
	if err := s.ingestAndResolve(); err != nil {
		return errResult(err)
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

	defs, err := s.db.FindDefinitionsByFile(dir, file, 0)
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

	defs, err := s.db.FindDefinitionsByFile(dir, args.File, args.Line)
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
	defs, err := s.db.FindDefinitionsByFile(dir, file, 0)
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

func (s *server) handleTraverse(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found: %w", args.Name, err))
	}

	maxDepth := args.Depth
	if maxDepth <= 0 {
		maxDepth = 10
	}

	results, err := s.db.Traverse(d.ID, args.Direction, args.RefKinds, maxDepth)
	if err != nil {
		return errResult(fmt.Errorf("traverse: %w", err))
	}

	startName := d.Name
	if d.Receiver != "" {
		startName = "(" + d.Receiver + ")." + d.Name
	}

	if args.Format == "json" {
		type jsonResult struct {
			Name       string   `json:"name"`
			Kind       string   `json:"kind"`
			Receiver   string   `json:"receiver,omitempty"`
			SourceFile string   `json:"source_file"`
			Test       bool     `json:"test,omitempty"`
			Depth      int      `json:"depth"`
			Path       []string `json:"path"`
		}
		type jsonResponse struct {
			Start     string       `json:"start"`
			Direction string       `json:"direction"`
			MaxDepth  int          `json:"max_depth"`
			Results   []jsonResult `json:"results"`
			Total     int          `json:"total"`
		}
		resp := jsonResponse{
			Start:     startName,
			Direction: args.Direction,
			MaxDepth:  maxDepth,
			Results:   []jsonResult{},
			Total:     len(results),
		}
		for _, r := range results {
			resp.Results = append(resp.Results, jsonResult{
				Name:       r.Definition.Name,
				Kind:       r.Definition.Kind,
				Receiver:   r.Definition.Receiver,
				SourceFile: r.Definition.SourceFile,
				Test:       r.Definition.Test,
				Depth:      r.Depth,
				Path:       r.Path,
			})
		}
		data, _ := json.Marshal(resp)
		return textResult(string(data)), nil, nil
	}

	// Markdown output grouped by depth.
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Traverse: %s of %s (max %d hops, %d results)\n\n", args.Direction, startName, maxDepth, len(results))

	if len(results) == 0 {
		sb.WriteString("No results found.\n")
		return textResult(sb.String()), nil, nil
	}

	currentDepth := 0
	for _, r := range results {
		if r.Depth != currentDepth {
			currentDepth = r.Depth
			count := 0
			for _, r2 := range results {
				if r2.Depth == currentDepth {
					count++
				}
			}
			fmt.Fprintf(&sb, "\n### Depth %d (%d definitions)\n", currentDepth, count)
		}
		name := r.Definition.Name
		if r.Definition.Receiver != "" {
			name = "(" + r.Definition.Receiver + ")." + name
		}
		testMark := ""
		if r.Definition.Test {
			testMark = " [test]"
		}
		fmt.Fprintf(&sb, "- %s (%s)%s — %s\n", name, r.Definition.Kind, testMark, r.Definition.SourceFile)
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleLiterals(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	typeName := args.Pattern
	if typeName == "" {
		typeName = "%" // all types
	} else if !strings.Contains(typeName, "%") {
		typeName = "%" + typeName + "%" // convenience: partial match
	}
	fields, err := s.db.QueryLiteralFields(typeName, args.Name, args.Body, nil, 200)
	if err != nil {
		return errResult(fmt.Errorf("query literals: %w", err))
	}
	if len(fields) == 0 {
		return textResult("No literal fields found"), nil, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Literal fields (%d results)\n\n", len(fields))
	fmt.Fprintf(&sb, "| Definition | Type | Field | Value | Line |\n")
	fmt.Fprintf(&sb, "|---|---|---|---|---|\n")
	for _, f := range fields {
		defName := f.DefName
		if defName == "" {
			defName = fmt.Sprintf("#%d", f.DefID)
		}
		// Shorten type name: just the last component.
		shortType := f.TypeName
		if idx := strings.LastIndex(shortType, "."); idx >= 0 {
			shortType = shortType[idx+1:]
		}
		val := f.FieldValue
		if len(val) > 60 {
			val = val[:57] + "..."
		}
		fmt.Fprintf(&sb, "| %s | %s | %s | `%s` | %d |\n", defName, shortType, f.FieldName, val, f.Line)
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handlePragmas(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	pragmaKey := args.Pattern
	if pragmaKey == "" {
		pragmaKey = "%" // all pragmas
	}
	comments, err := s.db.GetCommentsByPragma(pragmaKey)
	if err != nil {
		return errResult(fmt.Errorf("query pragmas: %w", err))
	}
	if len(comments) == 0 {
		return textResult("No pragmas found matching " + pragmaKey), nil, nil
	}

	// Filter by file if specified.
	if args.File != "" {
		var filtered []store.Comment
		for _, c := range comments {
			if c.SourceFile == args.File || strings.HasSuffix(c.SourceFile, "/"+args.File) {
				filtered = append(filtered, c)
			}
		}
		comments = filtered
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Pragmas matching %q (%d results)\n\n", pragmaKey, len(comments))
	for _, c := range comments {
		defName := c.DefName
		if defName == "" {
			defName = "(file-level)"
		}
		fmt.Fprintf(&sb, "- `%s` %s — %s:%d → %s\n", c.PragmaKey, c.PragmaVal, c.SourceFile, c.Line, defName)
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleValidatePlan(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	// Build set of all names in the plan for O(1) lookup.
	planned := map[string]bool{}
	for _, m := range args.Mutations {
		key := m.Name
		if m.Receiver != "" {
			key = "(" + m.Receiver + ")." + m.Name
		}
		planned[key] = true
	}

	type callerGap struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Receiver   string `json:"receiver,omitempty"`
		SourceFile string `json:"source_file"`
	}
	type changeResult struct {
		Name             string      `json:"name"`
		ChangeType       string      `json:"change_type"`
		Error            string      `json:"error,omitempty"`
		DirectCallers    int         `json:"direct_callers"`
		TransitiveCount  int         `json:"transitive_count"`
		TestCount        int         `json:"test_count"`
		MissingTests     bool        `json:"missing_tests"`
		UncoveredCallers []callerGap `json:"uncovered_callers"`
		MissedInterfaces []string    `json:"missed_interfaces,omitempty"`
	}

	var results []changeResult
	totalGaps := 0

	for _, m := range args.Mutations {
		cr := changeResult{Name: m.Name, ChangeType: m.Type}

		d, err := s.db.GetDefinitionByName(m.Name, "")
		if err != nil {
			cr.Error = fmt.Sprintf("definition %q not found", m.Name)
			results = append(results, cr)
			continue
		}

		impact, err := s.db.GetImpact(d.ID)
		if err != nil {
			cr.Error = err.Error()
			results = append(results, cr)
			continue
		}

		cr.DirectCallers = len(impact.DirectCallers)
		cr.TransitiveCount = impact.TransitiveCount
		cr.TestCount = len(impact.Tests)
		cr.MissingTests = len(impact.Tests) == 0

		// Check which production callers are NOT in the plan.
		for _, c := range impact.DirectCallers {
			if c.Test {
				continue
			}
			key := c.Name
			if c.Receiver != "" {
				key = "(" + c.Receiver + ")." + c.Name
			}
			if !planned[key] {
				cr.UncoveredCallers = append(cr.UncoveredCallers, callerGap{
					Name: c.Name, Kind: c.Kind, Receiver: c.Receiver, SourceFile: c.SourceFile,
				})
			}
		}
		totalGaps += len(cr.UncoveredCallers)

		// Check interface dispatch callers not in plan.
		for _, ic := range impact.InterfaceDispatchCallers {
			key := ic.Name
			if ic.Receiver != "" {
				key = "(" + ic.Receiver + ")." + ic.Name
			}
			if !planned[key] {
				cr.MissedInterfaces = append(cr.MissedInterfaces, key)
			}
		}

		results = append(results, cr)
	}

	summary := "ok"
	if totalGaps > 0 {
		summary = fmt.Sprintf("%d uncovered production callers across %d changes", totalGaps, len(args.Mutations))
	}

	output := map[string]any{
		"changes":    results,
		"total_gaps": totalGaps,
		"summary":    summary,
	}
	text, err := toJSON(output)
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
