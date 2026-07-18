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
	"runtime/debug"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/justinstimatze/defn/internal/emit"
	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/projection"
	"github.com/justinstimatze/defn/internal/rank"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxSearchResults = 20

// Version is the running defn build's semver string. Kept as a package
// constant so the CLI can compare its own version against what a
// running serve reports via the /version HTTP endpoint, surfacing
// binary/serve skew in `defn status`.
const Version = "0.25.0"

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
	autoCommitCount atomic.Int64 // counts auto-commits; triggers GC every 10
	idf             *rank.LazyIDF
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
	fmt.Fprintf(os.Stderr, "defn: listening on %s\n", addr)
	srv := &http.Server{Addr: addr, Handler: mcpHTTPMux(mcpServer, projDir)}
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
	fmt.Fprintf(os.Stderr, "defn: shared server on %s\n", addr)
	srv := &http.Server{Addr: addr, Handler: mcpHTTPMux(mcpServer, projDir)}
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

// mcpHTTPMux returns the ServeMux used by RunHTTP and RunShared. MCP
// clients connect to /sse; CLI tools hit /version to check for binary
// skew (an older serve still running under an upgraded on-disk defn).
func mcpHTTPMux(mcpServer *sdkmcp.Server, projDir string) http.Handler {
	sse := sdkmcp.NewSSEHandler(func(*http.Request) *sdkmcp.Server {
		return mcpServer
	}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(Version))
	})
	// /identity returns the absolute project directory this serve is
	// pinned to. cmdServe's auto-sharing path uses this to detect FNV
	// hash collisions (two distinct projects mapping to the same port)
	// before silently proxying to the wrong DB.
	mux.HandleFunc("/identity", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(projDir))
	})
	// /sse is the SSE entry point; subpaths (/sse/<id>) are per-session
	// streams. Anything unmatched falls through to SSE for backward
	// compatibility with clients hitting "/".
	mux.Handle("/sse", sse)
	mux.Handle("/sse/", sse)
	mux.Handle("/", sse)
	return mux
}

// newMCPServer creates the internal server state and MCP server instance.
// Shared by both stdio and HTTP transports.
func newMCPServer(ctx context.Context, database *store.DB, projDir string) (*server, *sdkmcp.Server) {
	s := &server{db: database, projectDir: projDir}
	s.idf = newIDF(database)

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
		go s.startGCTicker(ctx)
	}

	mcpServer := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    "defn",
		Version: Version,
	}, nil)

	sdkmcp.AddTool(mcpServer, &sdkmcp.Tool{
		Name: "code",
		Description: `Go code database. One tool, many ops. Start with impact for blast radius — it returns callers, transitives, and test coverage in one call. Don't follow up with search/explain unless you need more.

Ops: impact (blast radius — START HERE; pass format:"json" for structured output), read, read-file (all defs' bodies in one file — pass file:"path"; whole-file counterpart to read; prefer over N sequential read calls when scanning), outline (compact projection — sig + doc + caller/callee summary, no body; use when body isn't needed), slice (verbatim AST-role slice of a def — pass slice:"signature"|"doc"|"body"|"error-branch"|"return"|"loop" to get just that piece), insert-precondition (insert an if-block at function entry — byte-exact PUTGET; pass name+condition+ret), replace-slice (replace the Nth AST-role slice with verbatim bytes — byte-exact PUTGET; pass name+slice+index+new; refuses if replacement would discard interior comments — pass force:true to override), wrap-in-defer (insert defer stmt before Nth top-level statement — byte-exact PUTGET; pass name+stmt_index+defer_body), rename-param (rename value param or receiver via ast.Object scoping — ≡_gofmt equivalence; pass name+old_param+new_param), add-import (add import path to file's module — goimports-canonical grouping (stdlib / third-party); pass import_path+file?+alias? — file inferred if DB has one non-test .go file), search, explain, similar, untested, edit (full body OR old_fragment+new_fragment), insert (after anchor), create (single def from body; with file: set, body may hold multiple top-level decls to author a whole file in one call — the whole-file equivalent of files-mode Write), delete, rename, move, test, apply (batch multiple ops atomically in one turn — accepts create/edit/delete/rename PLUS all 5 projection ops insert-precondition/replace-slice/wrap-in-defer/rename-param/add-import; rolls back on any error; one emit+build for the whole batch), diff, history, find, sync (pass file:"path" for fast single-file sync), query, overview, patch, simulate, validate-plan, pragmas (query comment pragmas), literals (query composite literal fields), traverse (recursive graph traversal), branch (list/create/delete — pass from to branch from a source, force to delete), checkout (switch branch), merge (merge branch into current), commit (snapshot current state), status (current branch + dirty state), conflicts (list unresolved merge conflicts), resolve (name+body OR pick:"ours"/"theirs"), merge-abort (cancel in-progress merge), diff-defs (definitions that differ between two refs — pass from:"X" and optionally to:"Y"; defaults to working tree), gc (compact Dolt noms store)`,
	}, s.handleCode)

	return s, mcpServer
}

// codeParam is the unified parameter for the single "code" tool.
// Required fields per op:
//
//	read, impact, explain, delete, test, history: name
//	search: pattern (or name as fallback)
//	edit: name + new_body (full replace) OR name + old_fragment + new_fragment (fragment)
//	insert: name + after + body
//	create: body (+ optional module or file). When file: is set, body may
//	         hold multiple top-level decls to author a whole file in one call.
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
//	emit: out (directory path — absolute or relative to the project root)
type codeParam struct {
	Op          string           `json:"op"`
	Name        string           `json:"name,omitempty"`
	Pattern     string           `json:"pattern,omitempty"`
	Body        string           `json:"body,omitempty"`
	NewBody     string           `json:"new_body,omitempty"`
	Module      string           `json:"module,omitempty"`
	OldName     string           `json:"old_name,omitempty"`
	NewName     string           `json:"new_name,omitempty"`
	SQL         string           `json:"sql,omitempty"`
	File        string           `json:"file,omitempty"`
	Line        int              `json:"line,omitempty"`
	Names       []string         `json:"names,omitempty"`
	Mutations   []store.Mutation `json:"mutations,omitempty"`
	Depth       int              `json:"depth,omitempty"`
	Receiver    string           `json:"receiver,omitempty"`
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
	Out         string           `json:"out,omitempty"`
	Rank        bool             `json:"rank,omitempty"`
	Slice       string           `json:"slice,omitempty"`
	Condition   string           `json:"condition,omitempty"`
	Ret         string           `json:"ret,omitempty"`
	Index       int              `json:"index,omitempty"`
	New         string           `json:"new,omitempty"`
	ImportPath  string           `json:"import_path,omitempty"`
	Alias       string           `json:"alias,omitempty"`
	OldParam    string           `json:"old_param,omitempty"`
	NewParam    string           `json:"new_param,omitempty"`
	StmtIndex   int              `json:"stmt_index,omitempty"`
	DeferBody   string           `json:"defer_body,omitempty"`
	Full        bool             `json:"full,omitempty"`
	Include     []string         `json:"include,omitempty"` // expand op: which graph hops to fold in
}

type applyOp struct {
	Op          string `json:"op"`
	Name        string `json:"name"`
	NewName     string `json:"new_name"`
	Body        string `json:"body"`
	NewBody     string `json:"new_body"`
	Module      string `json:"module"`
	File        string `json:"file"`
	OldFragment string `json:"old_fragment"`
	NewFragment string `json:"new_fragment"`
	After       string `json:"after"`
	ReplaceAll  bool   `json:"replace_all"`

	// Projection-op fields. Not all ops use every field; the op tag
	// picks which apply. See internal/projection for the pure functions.
	Condition  string `json:"condition"`   // insert-precondition
	Ret        string `json:"ret"`         // insert-precondition
	Slice      string `json:"slice"`       // replace-slice
	Index      int    `json:"index"`       // replace-slice
	New        string `json:"new"`         // replace-slice
	Force      bool   `json:"force"`       // replace-slice
	DeferBody  string `json:"defer_body"`  // wrap-in-defer
	StmtIndex  int    `json:"stmt_index"`  // wrap-in-defer
	OldParam   string `json:"old_param"`   // rename-param
	NewParam   string `json:"new_param"`   // rename-param
	ImportPath string `json:"import_path"` // add-import
	Alias      string `json:"alias"`       // add-import
}

// Legacy param types used by internal handlers.
type nameParam struct {
	Name string `json:"name"`
	// Full forces the read op to return the body even when the def
	// matches a known upstream fingerprint. Default (false) yields the
	// compact provenance form for library-symbol reads.
	Full bool `json:"full,omitempty"`
}

type editParam struct {
	Name    string `json:"name"`
	NewBody string `json:"new_body"`
}

type createParam struct {
	Body   string `json:"body"`
	Module string `json:"module,omitempty"`
	File   string `json:"file,omitempty"`
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

func textResult(text string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text}},
	}
}

// usageStats is the structured meta-signal we attach to read-side op
// responses so bench harnesses can measure per-op savings without
// re-parsing text. Also drives the compact footer line on dramatic
// wins. See task #59 / [[project_marketing_playbook]].
type usageStats struct {
	Op            string `json:"op"`
	BytesReturned int    `json:"bytes_returned"`
	BytesAltRead  int    `json:"bytes_alt_read,omitempty"`
	SavingsPct    int    `json:"savings_pct,omitempty"`
}

// fileAltBytes is a proxy for "what a Read on the source file would
// have returned in bytes." Uses the file_sources table (populated by
// ingest); returns 0 when unavailable so the caller can skip the
// comparison rather than log a bogus number.
func (s *server) fileAltBytes(d *store.Definition) int {
	if d == nil || d.SourceFile == "" {
		return 0
	}
	raw, err := s.db.GetFileSource(d.ModuleID, d.SourceFile)
	if err != nil {
		return 0
	}
	return len(raw)
}

// withUsage attaches u to r.StructuredContent and, when the alt-Read
// savings are both dramatic (≥50%) and non-trivial (alt ≥ 512 bytes),
// appends a one-line footer to r's text content. No-op on nil / error
// results.
func withUsage(r *sdkmcp.CallToolResult, u usageStats) *sdkmcp.CallToolResult {
	if r == nil || r.IsError {
		return r
	}
	if u.BytesAltRead > 0 {
		saved := u.BytesAltRead - u.BytesReturned
		if saved < 0 {
			saved = 0
		}
		u.SavingsPct = 100 * saved / u.BytesAltRead
	}
	r.StructuredContent = u
	if u.BytesAltRead >= 512 && u.SavingsPct >= 50 {
		footer := fmt.Sprintf("\n_— returned %dB vs ~%dB for full-file Read (-%d%%)_\n",
			u.BytesReturned, u.BytesAltRead, u.SavingsPct)
		for _, c := range r.Content {
			if tc, ok := c.(*sdkmcp.TextContent); ok {
				tc.Text += footer
			}
		}
	}
	return r
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
	case "insert-precondition":
		if r, o, e := need(args.Name, "name"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.Condition, "condition"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.Ret, "ret"); r != nil {
			return r, o, e
		}
	case "replace-slice":
		if r, o, e := need(args.Name, "name"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.Slice, "slice"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.New, "new"); r != nil {
			return r, o, e
		}
	case "wrap-in-defer":
		if r, o, e := need(args.Name, "name"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.DeferBody, "defer_body"); r != nil {
			return r, o, e
		}
	case "rename-param":
		if r, o, e := need(args.Name, "name"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.OldParam, "old_param"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.NewParam, "new_param"); r != nil {
			return r, o, e
		}
	case "add-import":
		if r, o, e := need(args.File, "file"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.ImportPath, "import_path"); r != nil {
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
	case "read-file":
		// Accept file: or name: (users may pass a path in either).
		if strings.TrimSpace(args.File) == "" && strings.TrimSpace(args.Name) == "" {
			return errResult(fmt.Errorf("read-file: file is required (pass file:\"path/to/x.go\")"))
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
	case "emit":
		if r, o, e := need(args.Out, "out"); r != nil {
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
		return wrapStale(s.handleGetDefinition(ctx, req, nameParam{Name: args.Name, Full: args.Full}))
	case "outline":
		return wrapStale(s.handleOutline(ctx, req, nameParam{Name: args.Name}))
	case "slice":
		return wrapStale(s.handleSlice(ctx, req, args))
	case "insert-precondition":
		return s.handleInsertPrecondition(ctx, req, args)
	case "replace-slice":
		return s.handleReplaceSlice(ctx, req, args)
	case "wrap-in-defer":
		return s.handleWrapInDefer(ctx, req, args)
	case "rename-param":
		return s.handleRenameParam(ctx, req, args)
	case "add-import":
		return s.handleAddImport(ctx, req, args)
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
		return s.handleCreate(ctx, req, createParam{Body: args.Body, Module: args.Module, File: args.File})
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
	case "expand":
		return wrapStale(s.handleExpand(ctx, req, args))
	case "read-file":
		return wrapStale(s.handleReadFile(ctx, req, args))
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
	case "emit":
		return s.handleEmit(ctx, req, args)
	case "gc":
		return s.handleGC(ctx, req, args)
	default:
		return errResult(fmt.Errorf("unknown op %q — valid: read, outline, slice, insert-precondition, replace-slice, wrap-in-defer, rename-param, add-import, search, impact, explain, similar, untested, edit, create, delete, rename, move, test, apply, diff, history, query, find, sync, test-coverage, batch-impact, simulate, validate-plan, pragmas, literals, traverse, branch, checkout, merge, commit, status, conflicts, resolve, merge-abort, diff-defs, emit, gc", args.Op))
	}
}

func (s *server) handleImpact(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}
	impact, err := s.db.GetImpact(d.ID)
	if err != nil {
		return errResult(err)
	}

	if args.Rank && len(impact.DirectCallers) > 1 {
		if err := s.rankDirectCallers(impact); err != nil {
			return errResult(fmt.Errorf("rank callers: %w", err))
		}
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
		"module":                     impact.Module,
		"direct_callers":             callers,
		"interface_dispatch_callers": ifaceDispatch,
		"transitive_count":           impact.TransitiveCount,
		"tests":                      tests,
		"uncovered_by":               impact.UncoveredBy,
		"blast_radius":               blastRadius,
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

	// Delta-from-prior: if this def belongs to a module we have upstream
	// fingerprints for AND the caller hasn't asked for the full body,
	// try the compact provenance form. See project_d_delta_from_prior.
	if !args.Full && modulePath != "" {
		upstreamName := upstreamDefName(d)
		hash := store.HashBodyStructural(d.Body)
		if match, _ := s.db.FindUpstreamMatch(modulePath, upstreamName, d.Kind, d.Receiver, hash); match != nil {
			return s.renderUpstreamMatch(d, match, modulePath)
		}
		// Miss: check whether any version of this def is known upstream.
		// If yes, it means the local copy has diverged — annotate the
		// body so the reader knows they're looking at patched code.
		if versions, _ := s.db.FindUpstreamVersions(modulePath, upstreamName, d.Kind, d.Receiver); len(versions) > 0 {
			return s.renderDivergedFromUpstream(d, versions, modulePath)
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

	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "read",
		BytesReturned: len(out),
		BytesAltRead:  s.fileAltBytes(d),
	}), nil, nil
}

// upstreamDefName returns the fully-qualified name used in the
// upstream_fingerprints table for a local definition. Plain functions
// use their unqualified name; methods use "ReceiverBase.Method" (with
// any leading "*" stripped from the receiver).
func upstreamDefName(d *store.Definition) string {
	if d.Receiver == "" {
		return d.Name
	}
	return strings.TrimPrefix(d.Receiver, "*") + "." + d.Name
}

// renderUpstreamMatch produces the compact provenance tag — one header
// line and the full:true escape hatch, nothing else. Doc and sig are
// intentionally omitted: measurement (bench/delta-prior/2026-07-17)
// showed that including them inflates the response past the size of
// the body they are meant to replace on typical library methods
// (chi/gin, 5-30 LOC bodies). The tag alone gives the model the
// pointer it needs — "this is Name @ version, unchanged from upstream"
// — and delegates body/doc/sig lookup to its prior (or to a follow-up
// full:true call when the prior is not enough).
func (s *server) renderUpstreamMatch(d *store.Definition, match *store.UpstreamFingerprint, modulePath string) (*sdkmcp.CallToolResult, any, error) {
	var sb strings.Builder
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (%s) — %s @ %s unchanged from upstream\n",
		recv, d.Name, d.Kind, modulePath, match.Version))
	sb.WriteString("(pass `full: true` for body + doc + sig)\n")

	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "read",
		BytesReturned: len(out),
		BytesAltRead:  s.fileAltBytes(d),
	}), nil, nil
}

// renderDivergedFromUpstream returns the body but annotates that the
// local copy differs from every known upstream version — a signal the
// reader should not fall back to their prior about the library code.
func (s *server) renderDivergedFromUpstream(d *store.Definition, versions []store.UpstreamFingerprint, modulePath string) (*sdkmcp.CallToolResult, any, error) {
	var sb strings.Builder
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", recv, d.Name, d.Kind))
	sb.WriteString(fmt.Sprintf("Module: %s\n\n", modulePath))
	vs := make([]string, 0, len(versions))
	for _, v := range versions {
		vs = append(vs, v.Version)
	}
	sb.WriteString(fmt.Sprintf("**Note:** local copy diverges from all known upstream versions (%s).\n\n", strings.Join(vs, ", ")))
	if d.Doc != "" {
		sb.WriteString(d.Doc + "\n\n")
	}
	sb.WriteString("```go\n")
	sb.WriteString(d.Body)
	sb.WriteString("\n```\n")

	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "read",
		BytesReturned: len(out),
		BytesAltRead:  s.fileAltBytes(d),
	}), nil, nil
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

	if args.Rank {
		return s.rankedSearchResult(args.Pattern, defs, limit)
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

// rankDirectCallers reorders impact.DirectCallers by descending rank score.
// The "query" is the impacted definition's name — callers with overlapping
// surface area (lexical match, body terms, receiver alignment) sort first,
// then graph weight (own caller count + test coverage) breaks ties. Mutates
// impact in place so both the JSON and markdown formatters pick up the new
// order without duplicating ranking logic on each path.
func (s *server) rankDirectCallers(impact *store.Impact) error {
	if s.idf == nil {
		// Server constructed without idf (test fixture or partial init);
		// skip ranking rather than panic on rank.Rank.
		return nil
	}
	ids := make([]int64, len(impact.DirectCallers))
	for i, c := range impact.DirectCallers {
		ids[i] = c.ID
	}
	callers, tests, err := s.db.RefCountsByTarget(ids)
	if err != nil {
		return err
	}
	cands := make([]rank.Candidate, len(impact.DirectCallers))
	for i, c := range impact.DirectCallers {
		cands[i] = rank.Candidate{
			Def:         c,
			CallerCount: callers[c.ID],
			TestCount:   tests[c.ID],
		}
	}
	scored := rank.Rank(impact.Definition.Name, cands, s.idf, rank.DefaultWeights)
	sorted := make([]store.Definition, len(scored))
	for i, r := range scored {
		sorted[i] = r.Def
	}
	impact.DirectCallers = sorted
	return nil
}

// rankedSearchResult scores the candidate set and returns the top `limit`
// by descending score. Caller/test counts are filled from a single batch
// refs query so the graph-signal features actually fire.
func (s *server) rankedSearchResult(query string, defs []store.Definition, limit int) (*sdkmcp.CallToolResult, any, error) {
	ids := make([]int64, len(defs))
	for i, d := range defs {
		ids[i] = d.ID
	}
	callers, tests, err := s.db.RefCountsByTarget(ids)
	if err != nil {
		return errResult(fmt.Errorf("ref counts: %w", err))
	}
	cands := make([]rank.Candidate, len(defs))
	for i, d := range defs {
		cands[i] = rank.Candidate{
			Def:         d,
			CallerCount: callers[d.ID],
			TestCount:   tests[d.ID],
		}
	}
	scored := rank.Rank(query, cands, s.idf, rank.DefaultWeights)

	type rankedSummary struct {
		Name     string  `json:"name"`
		Kind     string  `json:"kind"`
		Receiver string  `json:"receiver,omitempty"`
		Score    float64 `json:"score"`
	}
	out := make([]rankedSummary, 0, limit)
	for i, r := range scored {
		if i >= limit {
			break
		}
		out = append(out, rankedSummary{
			Name: r.Def.Name, Kind: r.Def.Kind, Receiver: r.Def.Receiver,
			Score: r.Score,
		})
	}
	text, err := toJSON(out)
	if err != nil {
		return errResult(err)
	}
	if len(scored) > limit {
		text += fmt.Sprintf("\n(showing top %d of %d ranked — pass limit:<n> to see more)", limit, len(scored))
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
// waitReady blocks until the startup ingestAndResolve goroutine has
// completed (or a hard timeout hits). Write handlers must call this so
// their SQL statements don't share the pinned *sql.Conn with the async
// ingest — Go's database/sql doesn't synchronize concurrent Conn use,
// and Dolt's session-level working set gets clobbered under the race
// (rename UPDATE silently discarded, ingest re-inserts stale defs).
//
// Timeout guards against a stuck LoadAll on a huge repo taking the
// serve down; 5 minutes is far above any legitimate startup and lets
// the handler proceed rather than hang the client indefinitely.
func (s *server) waitReady() {
	if s.ready.Load() {
		return
	}
	deadline := time.Now().Add(5 * time.Minute)
	for !s.ready.Load() {
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "defn: waitReady timeout — startup ingest still running after 5m; proceeding anyway")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

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
	// Best effort — log to stderr if commit fails so the operator notices
	// without breaking the edit they just made.
	if err := s.autoCommit(); err != nil {
		fmt.Fprintf(os.Stderr, "defn: auto-commit failed (post-resolve): %v\n", err)
	}
	s.lastResolved.Store(time.Now().UnixNano())
	if s.idf != nil {
		s.idf.Invalidate()
	}
}

// autoCommit commits the working set with an auto-generated message.
// This keeps Dolt's working set small so chunk accumulation doesn't
// cause storage bloat. No-op if nothing changed. Runs GC every 10
// auto-commits to compact the noms store. A separate time-based
// ticker (see startGCTicker) covers serves that don't hit 10 commits.
//
// Returns the commit error so callers can fail loudly when a write
// can't be persisted (e.g. "database is read only" after GC). Earlier
// versions swallowed this and left writes silently dropped.
func (s *server) autoCommit() error {
	err := s.db.Commit("auto-sync")
	s.db.CleanTempFiles()
	if n := s.autoCommitCount.Add(1); n%10 == 0 {
		go s.db.GC() // background — GC can be slow on large databases
	}
	return err
}

// startGCTicker fires a background GC every gcInterval. Counter-based
// GC alone (every N auto-commits) misses serves that idle or restart
// before reaching N — those let the journal grow unbounded. The ticker
// guarantees compaction over wall-clock time regardless of activity.
func (s *server) startGCTicker(ctx context.Context) {
	const gcInterval = 15 * time.Minute
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.db.GC(); err != nil {
				fmt.Fprintf(os.Stderr, "defn: periodic GC failed: %v\n", err)
			}
		}
	}
}

// ingestAndResolve loads packages once and runs both ingest and resolve
// against the shared result, avoiding a redundant packages.Load (~1-2 GB).
func (s *server) ingestAndResolve() error {
	// packages.Load + go/types peaks far above the steady-state heap —
	// ~2-3 GB type-checking a medium module. cmdServe pins a low GOMEMLIMIT
	// (1 GiB) to keep Dolt's caches small at idle, but enforcing that
	// ceiling during the load drives the GC into a back-to-back collection
	// spiral that pegs every core (and starves MCP requests into timeouts).
	// Lift the limit for the duration of the load, then restore it so idle
	// memory stays bounded.
	if prev := debug.SetMemoryLimit(-1); prev < 6<<30 {
		debug.SetMemoryLimit(6 << 30)
		defer debug.SetMemoryLimit(prev)
	}

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
	if err := s.autoCommit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.lastResolved.Store(time.Now().UnixNano())
	if s.idf != nil {
		s.idf.Invalidate()
	}
	return nil
}

// watchFiles polls for .go file changes and auto-reingests when detected.
// This keeps the defn database in sync when files are edited outside defn
// (e.g. via Edit/Write tools, vim, or other processes).
func (s *server) watchFiles(ctx context.Context) {
	// Poll responsively while edits are happening, but back off when idle so
	// a forgotten serve doesn't walk the whole tree every few seconds
	// forever. Snap back to minInterval the moment a change is seen.
	const (
		minInterval = 3 * time.Second
		maxInterval = 60 * time.Second
	)
	interval := minInterval
	var lastMod int64 // 0 means first poll — debounce window handles startup race
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		// Stat directories AND .go files. Dir mtime catches adds/renames/
		// deletes (the directory entry list changes); .go file mtime catches
		// in-place modifications (truncate+write, which doesn't bump parent
		// dir mtime on ext4/xfs). Dir-only would silently miss every in-place
		// edit from emit.Emit, code(op:"edit"), or editors using in-place save.
		var newest int64
		filepath.Walk(s.projectDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				base := filepath.Base(path)
				if base == ".defn" || base == ".defn-server" || base == ".git" || base == "vendor" || base == "node_modules" {
					return filepath.SkipDir
				}
			} else if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if mod := info.ModTime().UnixNano(); mod > newest {
				newest = mod
			}
			return nil
		})

		changed := newest > lastMod && lastMod > 0
		if changed {
			// Skip the re-ingest if startup ingest or autoResolve ran
			// recently — but DO NOT advance lastMod, so the next tick
			// after the 10s window retries this change instead of
			// silently dropping it.
			if time.Now().UnixNano()-s.lastResolved.Load() >= int64(10*time.Second) {
				s.ingestAndResolve()
				lastMod = newest
			}
			interval = minInterval
		} else {
			lastMod = newest
			if interval < maxInterval {
				if interval *= 2; interval > maxInterval {
					interval = maxInterval
				}
			}
		}
	}
}

// autoEmitAndBuild emits to the project directory (so file-based tools
// see the changes) and runs go build to verify.
// Set DEFN_LEGACY=1 to disable auto-emit (for projects where you want
// to edit files directly and use defn as a read-only acceleration layer).
func (s *server) autoEmitAndBuild() string {
	return s.autoEmitAndBuildWithOpts(emit.Opts{})
}

// autoEmitAndBuildWithOpts is autoEmitAndBuild with caller-supplied
// emit.Opts. Used by handleDelete to whitelist the deleted decl through
// emit.safeWriteGoFile so the intentional removal isn't blocked by the
// data-loss safety net. Without this, the delete lands in the DB but
// never in the file — the watcher then re-ingests the "resurrected" def
// on the next tick. See project_defn_watch_delete_race memory.
func (s *server) autoEmitAndBuildWithOpts(opts emit.Opts) string {
	if s.projectDir == "" || os.Getenv("DEFN_LEGACY") == "1" {
		return "Saved to database."
	}

	// Emit to the actual project directory — keeps files in sync.
	if err := emit.EmitWithOpts(s.db, s.projectDir, opts); err != nil {
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
	// Multi-decl bodies: allowed when file: is set. Each top-level decl is
	// upserted as its own Definition, all sharing the same SourceFile.
	// Single autoEmit+build at the end. Without file: the model has no way
	// to say where the defs land, so keep the rejection.
	if n := countTopLevelDecls(args.Body); n > 1 {
		if args.File == "" {
			return errResult(fmt.Errorf("body contains %d top-level declarations — op:create accepts one, OR set file: to author a whole file with multiple decls in one call", n))
		}
		return s.handleCreateMultiDecl(args)
	}

	// Infer name, kind, and test flag from the body.
	name, kind, receiver, isTest := s.inferFromBody(args.Body)
	if name == "" {
		return errResult(fmt.Errorf("couldn't infer definition name from body — make sure it starts with func/type/const/var"))
	}

	// Find module: file: param wins (most specific), then module:, then first.
	var mod *store.Module
	if args.File != "" {
		mod = s.findModuleByFile(args.File)
		if mod == nil {
			return errResult(fmt.Errorf("file %q does not map to any known module — run defn ingest first, or pass module: explicitly", args.File))
		}
	}
	if mod == nil && args.Module != "" {
		mod = s.findModule(args.Module)
		if mod == nil {
			return errResult(fmt.Errorf("module %q not found", args.Module))
		}
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
		ModuleID:   mod.ID,
		Name:       name,
		Kind:       kind,
		Exported:   exported,
		Test:       isTest,
		Receiver:   receiver,
		Signature:  extractSignature(args.Body),
		Body:       args.Body,
		SourceFile: args.File,
	}
	id, err := s.db.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuild()
	s.autoResolve(mod.Path)

	var sb strings.Builder
	loc := mod.Path
	if args.File != "" {
		loc = args.File + " (" + mod.Path + ")"
	}
	sb.WriteString(fmt.Sprintf("Created %s (id=%d, kind=%s) in %s\n", name, id, kind, loc))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	return textResult(sb.String()), nil, nil
}

// countTopLevelDecls returns the number of top-level declarations in a Go body
// fragment. Returns 0 if unparseable (caller surfaces a clearer error).
func countTopLevelDecls(body string) int {
	src := "package x\n" + stripLeadingPackageDecl(strings.TrimSpace(body))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return 0
	}
	return len(f.Decls)
}

// stripLeadingPackageDecl removes a leading `package X` declaration from a
// body fragment if present. The model naturally writes whole-file bodies
// beginning with `package foo` when asked to author a new file; without
// this the "package x\n" prefix we add for parsing produces two package
// decls and a parse error. The package name is redundant with the target
// file path anyway (defn derives package from module ingest).
func stripLeadingPackageDecl(body string) string {
	trimmed := strings.TrimLeft(body, " \t\n")
	if !strings.HasPrefix(trimmed, "package ") {
		return body
	}
	nl := strings.IndexByte(trimmed, '\n')
	if nl == -1 {
		return "" // whole body is just `package X`
	}
	return trimmed[nl+1:]
}

// slicedDecl is one top-level decl carved out of a multi-decl body.
type slicedDecl struct {
	Body     string
	Name     string
	Kind     string
	Receiver string
	IsTest   bool
}

// sliceDecls parses a multi-decl body and returns each top-level decl as
// its own slicedDecl (verbatim text including doc comments, name/kind
// metadata). Import blocks are silently skipped — goimports re-adds them
// at emit time from usage. Returns an error on unparseable input, no
// remaining decls after filtering, or a decl whose name cannot be inferred.
func sliceDecls(body string) ([]slicedDecl, error) {
	trimmed := stripLeadingPackageDecl(strings.TrimSpace(body))
	src := "package x\n" + trimmed
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse: %v", err)
	}
	if len(f.Decls) == 0 {
		return nil, fmt.Errorf("no top-level declarations found")
	}
	out := make([]slicedDecl, 0, len(f.Decls))
	for i, decl := range f.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			continue
		}
		startPos := decl.Pos()
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Doc != nil {
				startPos = d.Doc.Pos()
			}
		case *ast.GenDecl:
			if d.Doc != nil {
				startPos = d.Doc.Pos()
			}
		}
		startOff := fset.Position(startPos).Offset
		endOff := fset.Position(decl.End()).Offset
		if startOff < 0 || endOff > len(src) || startOff > endOff {
			return nil, fmt.Errorf("decl %d: bad offset range", i)
		}
		name, kind, receiver, isTest := inferOneDecl(decl)
		if name == "" {
			return nil, fmt.Errorf("decl %d: could not infer name (kind=%T)", i, decl)
		}
		out = append(out, slicedDecl{
			Body:     strings.TrimSpace(src[startOff:endOff]),
			Name:     name,
			Kind:     kind,
			Receiver: receiver,
			IsTest:   isTest,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no top-level declarations found (imports are ignored; goimports re-adds them at emit)")
	}
	return out, nil
}

// inferOneDecl is the per-decl extraction logic factored out of
// inferFromBody so both single- and multi-decl paths share the switch.
func inferOneDecl(decl ast.Decl) (name, kind, receiver string, isTest bool) {
	switch d := decl.(type) {
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
				if len(vs.Names) > 0 {
					name = vs.Names[0].Name
				}
				kind = "const"
			}
		case token.VAR:
			if len(d.Specs) > 0 {
				vs := d.Specs[0].(*ast.ValueSpec)
				if len(vs.Names) > 0 {
					name = vs.Names[0].Name
				}
				kind = "var"
			}
		}
	}
	if name != "" {
		isTest = strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark")
	}
	return
}

// handleCreateMultiDecl authors multiple defs into a single file in one call.
// Reached when handleCreate sees a multi-decl body and file: is set.
// All-or-nothing: a single upsert error rolls back all prior upserts.
func (s *server) handleCreateMultiDecl(args createParam) (*sdkmcp.CallToolResult, any, error) {
	decls, err := sliceDecls(args.Body)
	if err != nil {
		return errResult(fmt.Errorf("multi-decl parse: %v", err))
	}

	mod := s.findModuleByFile(args.File)
	if mod == nil && args.Module != "" {
		mod = s.findModule(args.Module)
	}
	if mod == nil {
		// New-package case: file: points at a directory not yet ingested.
		// Fall back to the shortest-path module (matches single-decl create's
		// fallback), which for a repo-rooted layout is the module root. Emit
		// will place the file at args.File; the new package appears on next
		// ingest.
		mods, _ := s.db.ListModules()
		for i := range mods {
			if mod == nil || len(mods[i].Path) < len(mod.Path) {
				mod = &mods[i]
			}
		}
	}
	if mod == nil {
		return errResult(fmt.Errorf("no modules found — run defn ingest first, or pass module: explicitly"))
	}

	// Pre-check: no name collides with an existing def in the target module.
	for _, d := range decls {
		if existing, err := s.db.GetDefinitionByName(d.Name, mod.Path); err == nil {
			recv := formatReceiver(existing.Receiver)
			return errResult(fmt.Errorf("definition %s%s already exists in %s (id=%d) — use code(op:\"edit\") to modify it", recv, d.Name, mod.Path, existing.ID))
		}
	}

	commit, rollback, txErr := s.db.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback()

	ids := make([]int64, 0, len(decls))
	for _, d := range decls {
		exported := len(d.Name) > 0 && d.Name[0] >= 'A' && d.Name[0] <= 'Z'
		def := &store.Definition{
			ModuleID:   mod.ID,
			Name:       d.Name,
			Kind:       d.Kind,
			Exported:   exported,
			Test:       d.IsTest,
			Receiver:   d.Receiver,
			Signature:  extractSignature(d.Body),
			Body:       d.Body,
			SourceFile: args.File,
		}
		id, err := s.db.UpsertDefinition(def)
		if err != nil {
			return errResult(fmt.Errorf("upsert %s: %v", d.Name, err))
		}
		ids = append(ids, id)
	}

	if err := commit(); err != nil {
		return errResult(fmt.Errorf("commit: %v", err))
	}

	buildResult := s.autoEmitAndBuild()
	s.autoResolve(mod.Path)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Created %d defs in %s (%s):\n", len(decls), args.File, mod.Path))
	for i, d := range decls {
		recv := formatReceiver(d.Receiver)
		sb.WriteString(fmt.Sprintf("  + %s%s (%s, id=%d)\n", recv, d.Name, d.Kind, ids[i]))
	}
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	return textResult(sb.String()), nil, nil
}

// findModuleByFile maps a source file path to its module by matching the
// file's directory against module Paths (which are import paths like
// "github.com/x/y/internal/code"). Accepts repo-relative or absolute paths;
// matches by suffix on the directory component.
func (s *server) findModuleByFile(file string) *store.Module {
	mods, _ := s.db.ListModules() // best effort — nil is safe
	if len(mods) == 0 {
		return nil
	}
	dir := filepath.ToSlash(filepath.Dir(file))
	dir = strings.TrimPrefix(dir, "./")
	if dir == "" || dir == "." {
		// File sits at repo root — pick the module whose Path has no
		// internal segment beyond the module root (shortest path wins).
		var best *store.Module
		for i, m := range mods {
			if best == nil || len(m.Path) < len(best.Path) {
				best = &mods[i]
			}
		}
		return best
	}
	// Prefer exact suffix match on the import path. Try longest dir component
	// first so "internal/code/foo" doesn't accidentally match "internal/code".
	var best *store.Module
	for i, m := range mods {
		mp := m.Path
		if mp == dir || strings.HasSuffix(mp, "/"+dir) {
			if best == nil || len(m.Path) > len(best.Path) {
				best = &mods[i]
			}
		}
	}
	return best
}

// inferFromBody extracts definition name, kind, receiver, and test flag from Go source.
func (s *server) inferFromBody(body string) (name, kind, receiver string, isTest bool) {
	// Parse the body as a Go source file to extract definition metadata.
	src := "package x\n" + stripLeadingPackageDecl(strings.TrimSpace(body))
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

	if args.DryRun {
		for _, op := range args.Operations {
			switch op.Op {
			case "create":
				if n := countTopLevelDecls(op.Body); n > 1 {
					errors = append(errors, fmt.Sprintf("create: body has %d top-level decls — split into %d create ops", n, n))
					continue
				}
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
			case "insert-precondition", "replace-slice", "wrap-in-defer", "rename-param":
				name := op.Name
				if name == "" {
					if inferred, err := s.inferSingleTargetName(); err != nil {
						errors = append(errors, fmt.Sprintf("%s: %v", op.Op, err))
						continue
					} else {
						name = inferred
					}
				}
				if _, err := s.db.GetDefinitionByName(name, ""); err != nil {
					errors = append(errors, fmt.Sprintf("%s %s: not found", op.Op, name))
				} else {
					sb.WriteString(fmt.Sprintf("~ would %s on %s\n", op.Op, name))
				}
			case "add-import":
				if op.ImportPath == "" {
					errors = append(errors, "add-import: import_path is required")
				} else {
					sb.WriteString(fmt.Sprintf("+ would add import %q\n", op.ImportPath))
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

	commit, rollback, txErr := s.db.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback()

	// projEdit resolves the target name (with single-def inference), runs
	// the pure projection function, validates the new body, and upserts.
	projEdit := func(op applyOp, compute func(body string) (string, error)) (string, string) {
		name := op.Name
		if name == "" {
			inferred, err := s.inferSingleTargetName()
			if err != nil {
				return "", fmt.Sprintf("%s: %v", op.Op, err)
			}
			name = inferred
		}
		d, err := s.db.GetDefinitionByName(name, "")
		if err != nil {
			return "", fmt.Sprintf("%s %s: not found", op.Op, name)
		}
		newBody, err := compute(d.Body)
		if err != nil {
			return "", fmt.Sprintf("%s %s: %v", op.Op, name, err)
		}
		validSrc := "package x\n" + newBody
		if _, parseErr := parser.ParseFile(token.NewFileSet(), "", validSrc, parser.ParseComments); parseErr != nil {
			return "", fmt.Sprintf("%s %s: produces invalid Go: %v", op.Op, name, parseErr)
		}
		d.Body = newBody
		d.Signature = extractSignature(newBody)
		if _, err := s.db.UpsertDefinition(d); err != nil {
			return "", fmt.Sprintf("%s %s: %v", op.Op, name, err)
		}
		return fmt.Sprintf("~ %s on %s\n", op.Op, name), ""
	}

	for _, op := range args.Operations {
		switch op.Op {
		case "create":
			if n := countTopLevelDecls(op.Body); n > 1 {
				errors = append(errors, fmt.Sprintf("create: body has %d top-level decls — split into %d create ops", n, n))
				continue
			}
			name, kind, receiver, isTest := s.inferFromBody(op.Body)
			if name == "" {
				errors = append(errors, "create: couldn't infer name from body")
				continue
			}
			var mod *store.Module
			if op.File != "" {
				mod = s.findModuleByFile(op.File)
				if mod == nil {
					errors = append(errors, fmt.Sprintf("create %s: file %q does not map to any known module", name, op.File))
					continue
				}
			}
			if mod == nil && op.Module != "" {
				mod = s.findModule(op.Module)
				if mod == nil {
					errors = append(errors, fmt.Sprintf("create %s: module %q not found", name, op.Module))
					continue
				}
			}
			if mod == nil {
				mods, _ := s.db.ListModules()
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
				SourceFile: op.File,
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
			callers, _ := s.db.GetCallers(d.ID)
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

		case "insert-precondition":
			line, errStr := projEdit(op, func(body string) (string, error) {
				return projection.InsertPrecondition(body, op.Condition, op.Ret)
			})
			if errStr != "" {
				errors = append(errors, errStr)
			} else {
				sb.WriteString(line)
			}

		case "replace-slice":
			idx := op.Index
			if idx == 0 {
				idx = 1
			}
			line, errStr := projEdit(op, func(body string) (string, error) {
				if op.Force {
					return projection.ReplaceSliceForce(body, op.Slice, idx, op.New)
				}
				return projection.ReplaceSlice(body, op.Slice, idx, op.New)
			})
			if errStr != "" {
				errors = append(errors, errStr)
			} else {
				sb.WriteString(line)
			}

		case "wrap-in-defer":
			line, errStr := projEdit(op, func(body string) (string, error) {
				return projection.WrapInDefer(body, op.StmtIndex, op.DeferBody)
			})
			if errStr != "" {
				errors = append(errors, errStr)
			} else {
				sb.WriteString(line)
			}

		case "rename-param":
			line, errStr := projEdit(op, func(body string) (string, error) {
				return projection.RenameParam(body, op.OldParam, op.NewParam)
			})
			if errStr != "" {
				errors = append(errors, errStr)
			} else {
				sb.WriteString(line)
			}

		case "add-import":
			if op.ImportPath == "" {
				errors = append(errors, "add-import: import_path is required")
				continue
			}
			file := op.File
			if file == "" {
				all, err := s.db.DistinctSourceFiles()
				if err != nil {
					errors = append(errors, fmt.Sprintf("add-import: %v", err))
					continue
				}
				var candidates []string
				for _, f := range all {
					if !strings.HasSuffix(f, "_test.go") {
						candidates = append(candidates, f)
					}
				}
				if len(candidates) == 1 {
					file = candidates[0]
				} else {
					errors = append(errors, fmt.Sprintf("add-import: file is required (found %d non-test .go files)", len(candidates)))
					continue
				}
			}
			dir := file
			if idx := strings.LastIndex(dir, "/"); idx >= 0 {
				dir = dir[:idx]
			}
			defs, err := s.db.FindDefinitionsByFile(dir, file, 0)
			if err != nil || len(defs) == 0 {
				errors = append(errors, fmt.Sprintf("add-import: no defs in %q", file))
				continue
			}
			moduleID := defs[0].ModuleID
			existing, err := s.db.GetImports(moduleID)
			if err != nil {
				errors = append(errors, fmt.Sprintf("add-import: read imports: %v", err))
				continue
			}
			alreadyPresent := false
			for _, imp := range existing {
				if imp.ImportedPath == op.ImportPath && imp.Alias == op.Alias {
					alreadyPresent = true
					break
				}
			}
			if alreadyPresent {
				sb.WriteString(fmt.Sprintf("= import %q already present\n", op.ImportPath))
				continue
			}
			updated := append(existing, store.Import{ModuleID: moduleID, ImportedPath: op.ImportPath, Alias: op.Alias})
			if err := s.db.SetImports(moduleID, updated); err != nil {
				errors = append(errors, fmt.Sprintf("add-import %q: %v", op.ImportPath, err))
			} else {
				sb.WriteString(fmt.Sprintf("+ added import %q\n", op.ImportPath))
			}

		default:
			errors = append(errors, fmt.Sprintf("unknown op: %s", op.Op))
		}
	}

	if len(errors) > 0 {
		sb.WriteString("\nErrors (transaction rolled back):\n")
		for _, e := range errors {
			sb.WriteString("- " + e + "\n")
		}
		return textResult(sb.String()), nil, nil
	}
	if err := commit(); err != nil {
		return errResult(fmt.Errorf("commit: %w", err))
	}

	buildResult := s.autoEmitAndBuild()
	s.autoResolve("")
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

	// Whitelist the deleted decl through emit's safeWriteGoFile safety
	// net. topLevelDeclNames formats methods as "<Recv>.Name" (pointer
	// receivers unwrapped); match that. Without this, the file on disk
	// would be left unchanged and watchFiles would resurrect the def.
	qualified := d.Name
	if d.Receiver != "" {
		qualified = strings.TrimPrefix(d.Receiver, "*") + "." + d.Name
	}
	buildResult := s.autoEmitAndBuildWithOpts(emit.Opts{AllowedRemovals: []string{qualified}})
	s.autoResolve("") // full resolve — deletion may affect other modules' references

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Deleted %s%s (id=%d)\n", recv, d.Name, d.ID))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleRename(_ context.Context, _ *sdkmcp.CallToolRequest, args renameParam) (*sdkmcp.CallToolResult, any, error) {
	// Wait for startup ingest/resolve to finish before running a rename.
	// newMCPServer launches ingestAndResolve() in a goroutine and marks
	// s.ready=true after. Both goroutines call execContext on the same
	// pinned sql.Conn; Go's database/sql does not synchronize concurrent
	// use of *sql.Conn, and under the race Dolt's session-level working
	// set is corrupted. Waiting for ready serializes them.
	s.waitReady()

	d, err := s.db.GetDefinitionByName(args.OldName, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.OldName))
	}

	// Compose the qualified old-name the safety net compares against (methods
	// use "<Recv>.Name", pointer receivers unwrapped). Reserve it BEFORE we
	// mutate d so the emit path knows this decl-name is *intentionally*
	// disappearing from the file — otherwise safeWriteGoFile refuses to drop
	// it and the merge appends the new name alongside the old one (bug fixed
	// for deletes in b274ccc; the same shape recurs for renames).
	qualifiedOld := d.Name
	if d.Receiver != "" {
		qualifiedOld = strings.TrimPrefix(d.Receiver, "*") + "." + d.Name
	}
	originalID := d.ID

	// Update the definition name in its own body using AST rename.
	// Only renames identifiers — preserves comments and string literals.
	totalSkipped := 0
	newBody, _ := astRename(d.Body, args.OldName, args.NewName)
	newSig := extractSignature(newBody)
	exported := len(args.NewName) > 0 && args.NewName[0] >= 'A' && args.NewName[0] <= 'Z'

	// RenameDefinition updates BY ID so identity + refs edges are preserved.
	// Do NOT use UpsertDefinition here: it looks up by (module,name,kind,recv,test)
	// and would INSERT a new row for the new name, leaving the old row orphaned
	// in the DB and both defs in the emitted file.
	if err := s.db.RenameDefinition(originalID, args.NewName, newBody, newSig, exported); err != nil {
		return errResult(err)
	}

	// Update all callers' bodies that reference the old name.
	callers, err := s.db.GetCallers(originalID)
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

	buildResult := s.autoEmitAndBuildWithOpts(emit.Opts{AllowedRemovals: []string{qualifiedOld}})
	s.autoResolve("")

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
		// Re-resolve refs for the affected package so structural changes
		// (added/removed embeds, signature changes, new defs) keep the
		// ref graph consistent. Without this, embed/implements/call refs
		// silently drift away from source over many sync calls.
		if err := resolve.ResolveFile(s.db, s.projectDir, filePath); err != nil {
			return errResult(fmt.Errorf("resolve file: %w", err))
		}
		// Surface commit failures (e.g. read-only after GC) instead of
		// leaving the ref table half-updated and reporting success.
		if err := s.autoCommit(); err != nil {
			return errResult(fmt.Errorf("commit after sync: %w", err))
		}
		return textResult(fmt.Sprintf("Synced %s: updated %d definitions.", args.File, n)), nil, nil
	}

	// Full sync: re-ingest all packages and rebuild references.
	if err := s.ingestAndResolve(); err != nil {
		return errResult(err)
	}
	return textResult("Synced: re-ingested source and rebuilt reference graph."), nil, nil
}

// handleEmit writes the current database state as .go files under
// args.Out. Relative paths resolve against the project root so agents
// can say `out:"."` or `out:"build/"` without needing absolute paths.
//
// This op exists so CLI-side workflows (lint, self-host checks, fresh
// checkouts) can run while defn serve is holding the embedded DB —
// the serve process has direct DB access and writes the emitted tree
// from its own goroutine.
func (s *server) handleEmit(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	out := args.Out
	if !filepath.IsAbs(out) {
		if s.projectDir == "" {
			return errResult(fmt.Errorf("relative out=%q but no project directory configured", out))
		}
		out = filepath.Join(s.projectDir, out)
	}
	if err := os.MkdirAll(out, 0755); err != nil {
		return errResult(fmt.Errorf("create out dir: %w", err))
	}
	locs, err := emit.EmitWithMap(s.db, out)
	if err != nil {
		return errResult(fmt.Errorf("emit: %w", err))
	}
	return textResult(fmt.Sprintf("Emitted %d definitions to %s.", len(locs), out)), nil, nil
}

// handleGC runs Dolt GC to compact the noms store. Safe to invoke
// while the serve is running — DOLT_GC kills the pinned conn but the
// pool reconnects on the next query (see store.Ping).
func (s *server) handleGC(_ context.Context, _ *sdkmcp.CallToolRequest, _ codeParam) (*sdkmcp.CallToolResult, any, error) {
	noms := filepath.Join(s.projectDir, ".defn", "defn", ".dolt", "noms")
	before := nomsSize(noms)
	start := time.Now()
	if err := s.db.GC(); err != nil {
		return errResult(fmt.Errorf("gc: %w", err))
	}
	after := nomsSize(noms)
	return textResult(fmt.Sprintf(
		"GC complete in %s. noms size: %s → %s (saved %s).",
		time.Since(start).Truncate(time.Millisecond),
		humanSize(before), humanSize(after), humanSize(before-after),
	)), nil, nil
}

// nomsSize returns the total size in bytes of the noms directory, or 0
// on error. Used for reporting GC savings; not load-bearing.
func nomsSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func humanSize(n int64) string {
	const unit = 1024
	if n < 0 {
		return "-" + humanSize(-n)
	}
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n2 := n / unit; n2 >= unit; n2 /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
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

// handleReadFile returns every definition's body in a single file, sorted
// by source order. It is the whole-file counterpart to `handleGetDefinition`
// (which reads one def) and a body-bearing twin of `handleFileDefs` (which
// returns just the metadata). See `.calque/registry.md`: both call
// `s.db.FindDefinitionsByFile` — that's the single-source data layer;
// this handler and file-defs project it differently.
func (s *server) handleReadFile(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	file := args.File
	if file == "" {
		file = args.Name
	}
	if strings.TrimSpace(file) == "" {
		return errResult(fmt.Errorf("read-file: file is required (pass file:\"path/to/x.go\")"))
	}
	// For subpath files use the dirname; for root-level files leave dir empty
	// so the module-path LIKE-match is permissive and the source_file exact
	// match narrows to the right file. NOTE: handleFileDefs strips a bare
	// filename's extension into a dir hint ("main.go" → "main"), which is
	// wrong for modules whose path doesn't contain that stem (e.g. module
	// "testproj" + file "main.go"). That's a latent bug in handleFileDefs;
	// this twin does the correct thing. TODO: fix handleFileDefs.
	dir := ""
	if idx := strings.LastIndex(file, "/"); idx >= 0 {
		dir = file[:idx]
	}
	defs, err := s.db.FindDefinitionsByFile(dir, file, 0)
	if err != nil {
		return errResult(err)
	}
	if len(defs) == 0 {
		return errResult(fmt.Errorf("read-file: no definitions found in %q (check path is relative to project root and file is ingested)", file))
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].StartLine < defs[j].StartLine })

	// Fetch bodies in one query — FindDefinitionsByFile returns metadata only.
	ids := make([]int64, len(defs))
	for i, d := range defs {
		ids[i] = d.ID
	}
	bodies, err := s.db.GetBodiesByDefIDs(ids)
	if err != nil {
		return errResult(fmt.Errorf("read-file: fetch bodies: %w", err))
	}

	// Look up module path once (all defs in this file share it).
	var modulePath string
	mods, _ := s.db.ListModules()
	for _, m := range mods {
		if m.ID == defs[0].ModuleID {
			modulePath = m.Path
			break
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s (%d definitions", file, len(defs)))
	if modulePath != "" {
		sb.WriteString(", module ")
		sb.WriteString(modulePath)
	}
	sb.WriteString(")\n\n")
	for _, d := range defs {
		recv := formatReceiver(d.Receiver)
		sb.WriteString(fmt.Sprintf("## %s%s (%s) L%d-%d\n", recv, d.Name, d.Kind, d.StartLine, d.EndLine))
		if d.Doc != "" {
			sb.WriteString(d.Doc)
			sb.WriteString("\n\n")
		}
		sb.WriteString("```go\n")
		sb.WriteString(bodies[d.ID])
		sb.WriteString("\n```\n\n")
	}
	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "read-file",
		BytesReturned: len(out),
	}), nil, nil
}

// handleExpand returns a definition plus caller-chosen graph neighborhoods
// in one tool_result. Attacks the N² cache-read cost problem: every kind
// under `include:` is a hop that would otherwise cost a separate turn.
//
// V1 supports two include kinds: "body" (the def source) and "callers"
// (direct callers with source locations). Default include when omitted is
// ["body","callers"] — the pair that kills the most common read→impact→read
// pattern. Additional kinds fold in only if the bench shows a signal.
//
// Design notes in scratchpad/expand-op-design.md.
func (s *server) handleExpand(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.Name) == "" {
		return errResult(fmt.Errorf("expand: name is required"))
	}
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	includes := args.Include
	if len(includes) == 0 {
		includes = []string{"body", "callers"}
	}
	want := map[string]bool{}
	for _, k := range includes {
		want[strings.ToLower(strings.TrimSpace(k))] = true
	}

	// Look up module path (all sections share it).
	var modulePath string
	mods, _ := s.db.ListModules()
	for _, m := range mods {
		if m.ID == d.ModuleID {
			modulePath = m.Path
			break
		}
	}

	var sb strings.Builder
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", recv, d.Name, d.Kind))
	if modulePath != "" {
		sb.WriteString(fmt.Sprintf("Module: %s\n", modulePath))
	}
	sb.WriteString("\n")

	if want["body"] {
		sb.WriteString("### body\n")
		if d.Doc != "" {
			sb.WriteString(d.Doc + "\n\n")
		}
		sb.WriteString("```go\n")
		sb.WriteString(d.Body)
		sb.WriteString("\n```\n\n")
	}

	if want["callers"] {
		impact, err := s.db.GetImpact(d.ID)
		if err != nil {
			return errResult(fmt.Errorf("expand: gather callers: %w", err))
		}
		var prodCallers, testCallers []store.Definition
		for _, c := range impact.DirectCallers {
			if c.Test {
				testCallers = append(testCallers, c)
			} else {
				prodCallers = append(prodCallers, c)
			}
		}
		sb.WriteString(fmt.Sprintf("### callers (%d — %d production, %d test)\n",
			len(impact.DirectCallers), len(prodCallers), len(testCallers)))
		for _, c := range prodCallers {
			name := formatReceiver(c.Receiver) + c.Name
			if c.SourceFile != "" && c.StartLine > 0 {
				sb.WriteString(fmt.Sprintf("- %s  (%s:%d)\n", name, c.SourceFile, c.StartLine))
			} else {
				sb.WriteString(fmt.Sprintf("- %s\n", name))
			}
		}
		for _, c := range testCallers {
			name := formatReceiver(c.Receiver) + c.Name
			if c.SourceFile != "" && c.StartLine > 0 {
				sb.WriteString(fmt.Sprintf("- %s _(test)_  (%s:%d)\n", name, c.SourceFile, c.StartLine))
			} else {
				sb.WriteString(fmt.Sprintf("- %s _(test)_\n", name))
			}
		}
		if len(impact.DirectCallers) == 0 {
			sb.WriteString("(none)\n")
		}
		sb.WriteString("\n")
	}

	// Warn on any unsupported include kinds so the caller learns the vocabulary.
	var unknown []string
	for _, k := range includes {
		norm := strings.ToLower(strings.TrimSpace(k))
		switch norm {
		case "body", "callers":
			// supported
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sb.WriteString(fmt.Sprintf("_note: unsupported include kinds ignored: %s (v1 supports: body, callers)_\n",
			strings.Join(unknown, ", ")))
	}

	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "expand",
		BytesReturned: len(out),
	}), nil, nil
}

func (s *server) handleFileDefs(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	file := args.File
	if file == "" {
		file = args.Name
	}
	if strings.TrimSpace(file) == "" {
		return errResult(fmt.Errorf("file-defs: file is required"))
	}
	// For subpath files use the dirname; for root-level files leave dir empty
	// so the module-path LIKE-match is permissive and the source_file exact
	// match narrows to the right file. Mirrors handleReadFile.
	dir := ""
	if idx := strings.LastIndex(file, "/"); idx >= 0 {
		dir = file[:idx]
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

// outlineBodyThreshold is the body-size below which outline returns
// the full read view instead: for tiny bodies, the outline's fixed
// overhead (header + refs summary + stats) exceeds the body's own
// tokens, so returning the body is strictly cheaper. Threshold measured
// on defn's own corpus — under ~300 chars, outline inflates the read.
const outlineBodyThreshold = 300

// topLevelFlow parses a function body and returns a compact sequence of
// top-level statement kinds ("if", "for", "return", ...) with 1-based
// line offsets from the body's first line. Non-parseable bodies (e.g.
// non-function kinds, corrupted storage) return nil, "" — callers omit
// the flow section entirely.
func topLevelFlow(body string) []string {
	if body == "" {
		return nil
	}
	src := "package p\n" + body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		return nil
	}
	if len(f.Decls) == 0 {
		return nil
	}
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok || fn.Body == nil {
		return nil
	}
	bodyStart := fset.Position(fn.Body.Lbrace).Line
	out := make([]string, 0, len(fn.Body.List))
	for _, stmt := range fn.Body.List {
		kind := stmtKind(stmt)
		if kind == "" {
			continue
		}
		line := fset.Position(stmt.Pos()).Line - bodyStart
		out = append(out, fmt.Sprintf("L%d:%s", line, kind))
	}
	return out
}

// stmtKind returns a short label for a top-level statement, or "" for
// kinds that don't carry useful flow information (empty stmts, labels).
func stmtKind(s ast.Stmt) string {
	switch x := s.(type) {
	case *ast.IfStmt:
		return "if"
	case *ast.ForStmt:
		return "for"
	case *ast.RangeStmt:
		return "range"
	case *ast.SwitchStmt:
		return "switch"
	case *ast.TypeSwitchStmt:
		return "typeswitch"
	case *ast.SelectStmt:
		return "select"
	case *ast.ReturnStmt:
		return "return"
	case *ast.DeferStmt:
		return "defer"
	case *ast.GoStmt:
		return "go"
	case *ast.SendStmt:
		return "send"
	case *ast.AssignStmt:
		return "assign"
	case *ast.IncDecStmt:
		return "incdec"
	case *ast.ExprStmt:
		return "call"
	case *ast.DeclStmt:
		return "decl"
	case *ast.BlockStmt:
		return "block"
	case *ast.BranchStmt:
		return strings.ToLower(x.Tok.String()) // break, continue, goto, fallthrough
	}
	return ""
}

// handleOutline returns a compact projection of a definition: header +
// signature (with doc prefix) + caller/callee summary + top-level flow
// outline + body byte/line counts. Deliberately excludes body content.
//
// Size-aware fallback: for bodies under outlineBodyThreshold, returns
// the read view instead — outline's fixed overhead is larger than a
// tiny body's own tokens, so the compression is negative.
//
// Aider-lineage compact-read baseline. Measured on defn's own 497
// funcs/methods: 33% of read output on average (67% compression), 13%
// on >2000-char bodies (87% compression). See
// [[project_putget_edit_vocab_design]] for the phase context.
func (s *server) handleOutline(_ context.Context, req *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	// Size-aware fallback: for tiny bodies, read is smaller than
	// outline. Route to the read handler transparently.
	if len(d.Body) < outlineBodyThreshold {
		return s.handleGetDefinition(nil, req, args)
	}

	var modulePath string
	mods, _ := s.db.ListModules()
	for _, m := range mods {
		if m.ID == d.ModuleID {
			modulePath = m.Path
			break
		}
	}

	callers, _ := s.db.GetCallers(d.ID)
	callees, _ := s.db.GetCallees(d.ID)

	var prodCallers, testCallers int
	for _, c := range callers {
		if c.Test {
			testCallers++
		} else {
			prodCallers++
		}
	}

	bodyLines := strings.Count(d.Body, "\n") + 1

	var sb strings.Builder
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", recv, d.Name, d.Kind))
	sb.WriteString(fmt.Sprintf("Module: %s\n", modulePath))
	if d.SourceFile != "" && d.StartLine > 0 {
		sb.WriteString(fmt.Sprintf("Location: %s:%d\n", d.SourceFile, d.StartLine))
	}
	sb.WriteString("\n")

	// d.Signature already carries doc as `// ...` prefix lines when doc
	// is present. Emit d.Signature only to avoid duplicating doc; fall
	// back to d.Doc only if the sig is empty (unusual).
	switch {
	case d.Signature != "":
		sb.WriteString("```go\n")
		sb.WriteString(d.Signature)
		sb.WriteString("\n```\n\n")
	case d.Doc != "":
		sb.WriteString(d.Doc + "\n\n")
	}

	sb.WriteString(fmt.Sprintf("Body: %d lines, %d bytes (fetch with op:\"read\")\n", bodyLines, len(d.Body)))
	sb.WriteString(fmt.Sprintf("Callers: %d (%d production, %d test)\n", len(callers), prodCallers, testCallers))
	if len(callees) > 0 {
		names := make([]string, 0, len(callees))
		for _, c := range callees {
			names = append(names, formatReceiver(c.Receiver)+c.Name)
		}
		sort.Strings(names)
		sb.WriteString(fmt.Sprintf("Callees (%d): %s\n", len(callees), strings.Join(names, ", ")))
	} else {
		sb.WriteString("Callees: 0\n")
	}

	if flow := topLevelFlow(d.Body); len(flow) > 0 {
		sb.WriteString(fmt.Sprintf("Flow (%d): %s\n", len(flow), strings.Join(flow, " → ")))
	}

	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "outline",
		BytesReturned: len(out),
		BytesAltRead:  s.fileAltBytes(d),
	}), nil, nil
}

// handleSlice returns verbatim source bytes for AST-role slices of a
// definition (signature, doc, body, error-branch, return, loop). Each
// slice is annotated with its line offset from the def's first line.
// Multiple matches (e.g. multiple `if err != nil` blocks) are returned
// as a numbered list.
//
// Phase B of the projection design: verbatim-slice queries are the
// foundation for the `replace-slice` edit primitive. Bytes returned
// here splice byte-exact back into the def via replace-slice. See
// [[project_putget_edit_vocab_design]].
func (s *server) handleSlice(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.Name) == "" {
		return errResult(fmt.Errorf("slice: name is required"))
	}
	if strings.TrimSpace(args.Slice) == "" {
		return errResult(fmt.Errorf("slice: kind is required — valid: %s", strings.Join(projection.SliceKindNames(), ", ")))
	}

	d, err := s.db.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", args.Name))
	}

	slices, err := projection.Slices(d.Body, args.Slice)
	if err != nil {
		return errResult(err)
	}

	var sb strings.Builder
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (slice: %s, %d match%s)\n",
		recv, d.Name, args.Slice, len(slices), pluralS(len(slices))))
	if d.SourceFile != "" && d.StartLine > 0 {
		sb.WriteString(fmt.Sprintf("Location: %s:%d\n", d.SourceFile, d.StartLine))
	}
	sb.WriteString("\n")

	if len(slices) == 0 {
		sb.WriteString(fmt.Sprintf("(no %s slices in this definition)\n", args.Slice))
		out := sb.String()
		return withUsage(textResult(out), usageStats{
			Op: "slice", BytesReturned: len(out), BytesAltRead: s.fileAltBytes(d),
		}), nil, nil
	}

	for i, sl := range slices {
		if len(slices) > 1 {
			sb.WriteString(fmt.Sprintf("### match %d/%d — L%d\n", i+1, len(slices), sl.Line))
		} else {
			sb.WriteString(fmt.Sprintf("### L%d\n", sl.Line))
		}
		sb.WriteString("```go\n")
		sb.WriteString(sl.Source)
		sb.WriteString("\n```\n\n")
	}

	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op: "slice", BytesReturned: len(out), BytesAltRead: s.fileAltBytes(d),
	}), nil, nil
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

// handleInsertPrecondition inserts an `if <condition> { <ret> }` block
// at the start of the definition's body, immediately after the opening
// brace. Byte-exact PUTGET against the input body — see
// [[project_putget_edit_vocab_design]] and internal/projection for the
// pure function and its fixture goldens.
//
// If args.Name is empty, tries to infer the target: if the DB has exactly
// one non-test function, uses it; otherwise errors with the candidate list.
func (s *server) handleInsertPrecondition(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.Condition) == "" {
		return errResult(fmt.Errorf("insert-precondition: condition is required"))
	}
	if strings.TrimSpace(args.Ret) == "" {
		return errResult(fmt.Errorf("insert-precondition: ret is required"))
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		inferred, err := s.inferSingleTargetName()
		if err != nil {
			return errResult(fmt.Errorf("insert-precondition: %w", err))
		}
		name = inferred
	}
	d, err := s.db.GetDefinitionByName(name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", name))
	}
	newBody, err := projection.InsertPrecondition(d.Body, args.Condition, args.Ret)
	if err != nil {
		return errResult(err)
	}
	snippet := fmt.Sprintf("if %s {\n\t%s\n}", args.Condition, args.Ret)
	return s.applyEditTerse(name, "inserted precondition at entry", snippet, newBody)
}

// handleAddImport adds a new import (with optional alias) to the module
// that owns the given file. The projection package hosts the pure
// AddImport function for testing over file source; defn's on-disk
// projection is regenerated via the normal emit path so goimports
// handles per-group placement.
//
// Idempotent: adding an already-present (path, alias) is a no-op.
//
// If args.File is empty, tries to infer the target: if the DB has exactly
// one non-test .go file, uses it; otherwise errors with the candidate
// list so the caller can retry with an explicit file.
func (s *server) handleAddImport(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.ImportPath) == "" {
		return errResult(fmt.Errorf("add-import: import_path is required"))
	}
	file := strings.TrimSpace(args.File)
	if file == "" {
		all, err := s.db.DistinctSourceFiles()
		if err != nil {
			return errResult(fmt.Errorf("add-import: list files: %w", err))
		}
		var candidates []string
		for _, f := range all {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			candidates = append(candidates, f)
		}
		switch {
		case len(candidates) == 1:
			file = candidates[0]
		case len(candidates) == 0:
			return errResult(fmt.Errorf("add-import: file is required (DB has no non-test .go files to infer from)"))
		default:
			return errResult(fmt.Errorf("add-import: file is required; pick one of: %s", strings.Join(candidates, ", ")))
		}
	}
	// FindDefinitionsByFile matches the first arg against module.path
	// (LIKE %fileSuffix%). We want the directory portion of the file
	// for that; for a root-level file with no "/", the module can be
	// anything, so pass "" (which LIKE '%%' — matches every module).
	// The exact source_file filter still pins us to the right file.
	dir := ""
	if idx := strings.LastIndex(file, "/"); idx >= 0 {
		dir = file[:idx]
	}
	defs, err := s.db.FindDefinitionsByFile(dir, file, 0)
	if err != nil {
		return errResult(fmt.Errorf("add-import: locate file: %w", err))
	}
	if len(defs) == 0 {
		return errResult(fmt.Errorf("add-import: no definitions found in file %q — cannot resolve module", file))
	}
	moduleID := defs[0].ModuleID
	existing, err := s.db.GetImports(moduleID)
	if err != nil {
		return errResult(fmt.Errorf("add-import: read imports: %w", err))
	}
	for _, imp := range existing {
		if imp.ImportedPath == args.ImportPath && imp.Alias == args.Alias {
			return textResult(fmt.Sprintf("%s: import %q already present (no-op)\n", file, args.ImportPath)), nil, nil
		}
	}
	updated := append(existing, store.Import{
		ModuleID:     moduleID,
		ImportedPath: args.ImportPath,
		Alias:        args.Alias,
	})
	if err := s.db.SetImports(moduleID, updated); err != nil {
		return errResult(fmt.Errorf("add-import: set imports: %w", err))
	}
	buildResult := s.autoEmitAndBuild()
	snippet := fmt.Sprintf("import %q", args.ImportPath)
	if args.Alias != "" {
		snippet = fmt.Sprintf("import %s %q", args.Alias, args.ImportPath)
	}
	var sb strings.Builder
	sb.WriteString(file)
	sb.WriteString(": added import\n    ")
	sb.WriteString(snippet)
	sb.WriteString("\n")
	if buildResult != "" {
		firstLine := buildResult
		if idx := strings.Index(buildResult, "\n"); idx > 0 {
			firstLine = buildResult[:idx]
		}
		sb.WriteString("build: ")
		sb.WriteString(strings.ToLower(firstLine))
		sb.WriteString("\n")
	}
	return textResult(sb.String()), nil, nil
}

// handleRenameParam renames a function parameter (or receiver) in the
// definition's body via ast.Object scoping. Output is gofmt-normalized,
// so the PUTGET contract is ≡_gofmt equivalence rather than byte-exact.
// See [[project_putget_edit_vocab_design]].
//
// If args.Name is empty, tries to infer the target: if the DB has exactly
// one non-test function, uses it; otherwise errors with the candidate list.
func (s *server) handleRenameParam(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.OldParam) == "" {
		return errResult(fmt.Errorf("rename-param: old_param is required"))
	}
	if strings.TrimSpace(args.NewParam) == "" {
		return errResult(fmt.Errorf("rename-param: new_param is required"))
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		inferred, err := s.inferSingleTargetName()
		if err != nil {
			return errResult(fmt.Errorf("rename-param: %w", err))
		}
		name = inferred
	}
	d, err := s.db.GetDefinitionByName(name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", name))
	}
	newBody, err := projection.RenameParam(d.Body, args.OldParam, args.NewParam)
	if err != nil {
		return errResult(err)
	}
	action := fmt.Sprintf("renamed param %q → %q", args.OldParam, args.NewParam)
	snippet := newBody
	if idx := strings.Index(newBody, "\n"); idx > 0 {
		snippet = newBody[:idx]
	}
	return s.applyEditTerse(name, action, snippet, newBody)
}

// handleWrapInDefer inserts a `defer <defer_body>` statement immediately
// before the Nth (1-based) top-level statement in the definition's body.
// Byte-exact PUTGET — see [[project_putget_edit_vocab_design]].
//
// If args.Name is empty, tries to infer the target: if the DB has exactly
// one non-test function, uses it; otherwise errors with the candidate list.
func (s *server) handleWrapInDefer(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.DeferBody) == "" {
		return errResult(fmt.Errorf("wrap-in-defer: defer_body is required"))
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		inferred, err := s.inferSingleTargetName()
		if err != nil {
			return errResult(fmt.Errorf("wrap-in-defer: %w", err))
		}
		name = inferred
	}
	d, err := s.db.GetDefinitionByName(name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", name))
	}
	newBody, err := projection.WrapInDefer(d.Body, args.StmtIndex, args.DeferBody)
	if err != nil {
		return errResult(err)
	}
	stmtIdx := args.StmtIndex
	if stmtIdx == 0 {
		stmtIdx = 1
	}
	action := fmt.Sprintf("inserted defer before stmt #%d", stmtIdx)
	snippet := fmt.Sprintf("defer %s", args.DeferBody)
	return s.applyEditTerse(name, action, snippet, newBody)
}

// handleReplaceSlice replaces the Nth (1-based) match of the given AST
// slice kind in the definition's body with `new` verbatim bytes. The
// rest of the body is preserved byte-exact. See
// [[project_putget_edit_vocab_design]] and internal/projection for the
// pure function and its fixture goldens.
//
// Interior comment defense: refuses if the replaced range contains a
// comment not present in `new`. Pass `force:true` to discard interior
// comments explicitly. See internal/projection.ReplaceSlice for the
// contract.
//
// If args.Name is empty, tries to infer the target: if the DB has exactly
// one non-test function, uses it; otherwise errors with the candidate list.
func (s *server) handleReplaceSlice(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.Slice) == "" {
		return errResult(fmt.Errorf("replace-slice: slice kind is required — valid: %s", strings.Join(projection.SliceKindNames(), ", ")))
	}
	if strings.TrimSpace(args.New) == "" {
		return errResult(fmt.Errorf("replace-slice: new is required"))
	}
	index := args.Index
	if index == 0 {
		index = 1
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		inferred, err := s.inferSingleTargetName()
		if err != nil {
			return errResult(fmt.Errorf("replace-slice: %w", err))
		}
		name = inferred
	}
	d, err := s.db.GetDefinitionByName(name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", name))
	}
	var newBody string
	if args.Force {
		newBody, err = projection.ReplaceSliceForce(d.Body, args.Slice, index, args.New)
	} else {
		newBody, err = projection.ReplaceSlice(d.Body, args.Slice, index, args.New)
	}
	if err != nil {
		return errResult(err)
	}
	action := fmt.Sprintf("replaced %s #%d", args.Slice, index)
	return s.applyEditTerse(name, action, args.New, newBody)
}

// applyEditTerse is the projection-op response path: takes a computed
// newBody + a compact human summary of what changed, does the same DB
// write + build that handleEdit does, and returns a much tighter
// response so the agent doesn't feel compelled to Read-verify.
//
// Format:
//
//	F: <action>
//	    <snippet-line-1>
//	    <snippet-line-2>
//	build: ok
//
// Snippet is truncated to ~200 chars / ~6 lines. Skips the caller-count
// FYI nudge that handleEdit prints (agents can ask for impact if they want).
func (s *server) applyEditTerse(name, action, snippet, newBody string) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.db.GetDefinitionByName(name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", name))
	}
	src := "package x\n" + newBody
	if _, parseErr := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments); parseErr != nil {
		return errResult(fmt.Errorf("new_body has syntax error: %v", parseErr))
	}
	d.Body = newBody
	d.Signature = extractSignature(newBody)
	if _, err := s.db.UpsertDefinition(d); err != nil {
		return errResult(err)
	}
	buildResult := s.autoEmitAndBuild()
	s.autoResolve(s.modulePath(d.ModuleID))

	recv := formatReceiver(d.Receiver)
	var sb strings.Builder
	sb.WriteString(recv)
	sb.WriteString(name)
	sb.WriteString(": ")
	sb.WriteString(action)
	sb.WriteString("\n")
	if snippet != "" {
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		for _, line := range strings.Split(strings.TrimRight(snippet, "\n"), "\n") {
			sb.WriteString("    ")
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	if buildResult != "" {
		firstLine := buildResult
		if idx := strings.Index(buildResult, "\n"); idx > 0 {
			firstLine = buildResult[:idx]
		}
		sb.WriteString("build: ")
		sb.WriteString(strings.ToLower(firstLine))
		sb.WriteString("\n")
	}
	return textResult(sb.String()), nil, nil
}

// inferSingleTargetName returns the name of the only non-test function
// or method in the corpus. Used by projection-op handlers to make `name`
// optional in the single-def corpus case (e.g. bench fixtures) — mirrors
// the file-inference pattern in handleAddImport. Errors when zero or
// more than one candidate exists, listing the candidates so the caller
// can retry with an explicit name.
func (s *server) inferSingleTargetName() (string, error) {
	defs, err := s.db.FilterDefinitions("", "", "", 0)
	if err != nil {
		return "", fmt.Errorf("infer name: list definitions: %w", err)
	}
	var candidates []string
	for _, d := range defs {
		if d.Test {
			continue
		}
		if d.Kind != "function" && d.Kind != "method" {
			continue
		}
		name := d.Name
		if d.Receiver != "" {
			name = strings.TrimPrefix(d.Receiver, "*") + "." + d.Name
		}
		candidates = append(candidates, name)
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", fmt.Errorf("name is required (DB has no non-test functions to infer from)")
	default:
		if len(candidates) > 8 {
			candidates = append(candidates[:8], "…")
		}
		return "", fmt.Errorf("name is required; %d candidates: %s", len(candidates), strings.Join(candidates, ", "))
	}
}
