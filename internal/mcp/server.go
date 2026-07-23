// Package mcp implements the MCP server that exposes the defn database
// to Claude Code.
package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

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

// #159: search inline-preview knobs. Set searchPreviewCount to 0 to
// disable previews entirely (fallback if a workload proves they inflate
// tokens). 3-hits × 5-lines was chosen from the Multi-SWE-bench Go
// corpus: 867 grep→view bigrams collapse into one call if the top hit's
// body head is inline, and the model rarely reads beyond the top-3
// results of a targeted search.
const searchPreviewCount = 3
const searchPreviewLines = 5

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
	backend         store.Backend
	projectDir      string
	lastResolved    atomic.Int64 // UnixNano timestamp of last resolve (to debounce watcher)
	ready           atomic.Bool  // true after startup ingest+resolve completes
	autoCommitCount atomic.Int64 // counts auto-commits; triggers GC every 10
	idf             *rank.LazyIDF
	respCache       *respCache  // #77/#152: per-session dedup of read-side responses
	reach           *reachCache // #154: in-memory reverse-refs cache for fast batch impact
	hint            *mutationHint // #158: apply-batching nudge on serial mutations to one file
}

// Run starts the MCP server over stdio. projDir is the project root where
// files should be emitted (for in-place sync with file-based tools).
func Run(ctx context.Context, database store.Backend, projDir string) error {
	_, mcpServer := newMCPServer(ctx, database, projDir)
	return mcpServer.Run(ctx, &sdkmcp.StdioTransport{})
}

// RunHTTP starts the MCP server over HTTP/SSE on addr (e.g. ":9420").
// Multiple clients can connect to the same server, sharing one defn process.
func RunHTTP(ctx context.Context, database store.Backend, projDir, addr string) error {
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
func RunShared(ctx context.Context, database store.Backend, projDir, addr string) error {
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
// MeasureRename runs handleRename synchronously against `database` and
// returns the elapsed wall clock + the raw text of the result. Exposed
// for perf measurement (see cmd/defn measure-rename) so a caller can
// time the same code path an MCP client would drive without spinning
// up a full serve. Skips the async startup ingest.
func MeasureRename(database store.Backend, projDir, oldName, newName string) (time.Duration, string, error) {
	s := &server{backend: database, projectDir: projDir}
	s.idf = newIDF(database)
	s.ready.Store(true) // caller-driven; skip the async ingest wait
	start := time.Now()
	result, _, err := s.handleRename(context.Background(), nil,
		renameParam{OldName: oldName, NewName: newName})
	elapsed := time.Since(start)
	if err != nil {
		return elapsed, "", err
	}
	if result == nil {
		return elapsed, "", nil
	}
	if result.IsError {
		return elapsed, resultTextRaw(result), fmt.Errorf("rename failed: %s", resultTextRaw(result))
	}
	return elapsed, resultTextRaw(result), nil
}

// MeasureEdit is the symmetric measurement path for handleEdit. Winze
// uses this to time the edit thesis on their reference-dense corpus —
// same shape as MeasureRename but exercises the file-scoped goimports
// + autoResolveFile lever (#109 pass 3) rather than rename's skip path.
func MeasureEdit(database store.Backend, projDir, name, newBody string) (time.Duration, string, error) {
	s := &server{backend: database, projectDir: projDir}
	s.idf = newIDF(database)
	s.ready.Store(true)
	start := time.Now()
	result, _, err := s.handleEdit(context.Background(), nil,
		editParam{Name: name, NewBody: newBody})
	elapsed := time.Since(start)
	if err != nil {
		return elapsed, "", err
	}
	if result == nil {
		return elapsed, "", nil
	}
	if result.IsError {
		return elapsed, resultTextRaw(result), fmt.Errorf("edit failed: %s", resultTextRaw(result))
	}
	return elapsed, resultTextRaw(result), nil
}

// Shared by both stdio and HTTP transports.
func newMCPServer(ctx context.Context, database store.Backend, projDir string) (*server, *sdkmcp.Server) {
	s := &server{backend: database, projectDir: projDir}
	s.idf = newIDF(database)
	s.respCache = newRespCache()
	s.reach = newReachCache()
	s.hint = newMutationHint()

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
		Description: `Go code database. One tool, many ops. Orient before you read: overview (project shape) → outline (def shape) → impact (when you know which def matters). Only read whole bodies when you're about to edit them; whole-file reads on files you won't touch are pure wire cost — use outline or search instead.

Ops: overview (project-wide shape when called with no args — one line per module with def counts + first exported names; pass file:"pkg-path" or file:"pkg-path/file.go" to drill in; the right first-touch when you don't know which def matters yet), outline (compact projection of a def — sig + doc + caller/callee summary, no body; use when body isn't needed), search, impact (blast radius of a known def — pass format:"json" for structured output; callers, transitives, test coverage in one call), read, read-and-verify (read a def AND run its covering tests in one call — use during bug triage so you see behavior alongside source and don't spiral into read-loops; pass name), read-file (all defs' bodies in one file — pass file:"path"; whole-file counterpart to read; prefer over N sequential read calls when scanning), slice (verbatim AST-role slice of a def — pass slice:"signature"|"doc"|"body"|"error-branch"|"return"|"loop" to get just that piece), insert-precondition (insert an if-block at function entry — byte-exact PUTGET; pass name+condition+ret), replace-slice (replace the Nth AST-role slice with verbatim bytes — byte-exact PUTGET; pass name+slice+index+new; refuses if replacement would discard interior comments — pass force:true to override), replace-hunk (replace a byte-exact occurrence of 'old' inside a def body with 'new' — byte-exact PUTGET, content-addressed inside the def; pass name+old+new, plus index=1..N if 'old' occurs more than once; empty 'new' deletes the hunk. Send zero anchor context when the hunk is def-unique — the name argument does the file-level disambiguation), wrap-in-defer (insert defer stmt before Nth top-level statement — byte-exact PUTGET; pass name+stmt_index+defer_body), rename-param (rename value param or receiver via ast.Object scoping — ≡_gofmt equivalence; pass name+old_param+new_param), add-import (add import path to file's module — goimports-canonical grouping (stdlib / third-party); pass import_path+file?+alias? — file inferred if DB has one non-test .go file), explain, similar, untested, edit (full body OR old_fragment+new_fragment), insert (after anchor), create (single def from body; with file: set, body may hold multiple top-level decls to author a whole file in one call — the whole-file equivalent of files-mode Write), delete (safe by default — refuses when other defs still reference this def; pass force:true to delete anyway. Refusal message lists the callers so you can rewrite them first), retarget-field-value (rewrite a composite-literal field's string value across every def whose body matches — pass name:"<StructType>" field:"<Field>" old:"<oldStr>" new:"<newStr>"; AST-safe, so unrelated occurrences of the string won't match), rename, move, test (run ONLY tests that cover a given def — pass name; scoped subset, not the full suite; prefer over bash 'go test ./...' when you only need coverage for a specific change. Also accepts test:"TestX" to run one test by name — use this to REPRODUCE a bug from the issue BEFORE writing any code; a passing test means your hypothesis about which def is broken is wrong), apply (batch multiple ops atomically in one turn — accepts create/edit/delete/rename PLUS all 6 projection ops insert-precondition/replace-slice/replace-hunk/wrap-in-defer/rename-param/add-import; rolls back on any error; one emit+build for the whole batch), diff, history, find, sync (rarely needed — every edit op auto-syncs the DB; only use after external file changes outside the code tool), query (raw SQL escape hatch — for schema analytics only; NEVER use to look up a def by name, grep bodies, or list files/defs-in-file — use search/outline/read-file/file-defs/impact instead, which are far cheaper on the wire), patch, simulate, validate-plan, pragmas (query comment pragmas), literals (query composite literal fields), traverse (recursive graph traversal), branch (list/create/delete — pass from to branch from a source, force to delete), checkout (switch branch), merge (merge branch into current), commit (snapshot current state), status (current branch + dirty state), conflicts (list unresolved merge conflicts), resolve (name+body OR pick:"ours"/"theirs"), merge-abort (cancel in-progress merge), diff-defs (definitions that differ between two refs — pass from:"X" and optionally to:"Y"; defaults to working tree), gc (compact Dolt noms store)`,
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
	Old         string           `json:"old,omitempty"` // replace-hunk
	ImportPath  string           `json:"import_path,omitempty"`
	Alias       string           `json:"alias,omitempty"`
	OldParam    string           `json:"old_param,omitempty"`
	NewParam    string           `json:"new_param,omitempty"`
	StmtIndex   int              `json:"stmt_index,omitempty"`
	DeferBody   string           `json:"defer_body,omitempty"`
	Full        bool             `json:"full,omitempty"`
	Include     []string         `json:"include,omitempty"` // expand op: which graph hops to fold in
	Test        string           `json:"test,omitempty"`    // L11: op:test named-test reproduction (`-run <regex>` verbatim)
	Field       string           `json:"field,omitempty"`   // retarget-field-value: composite-literal field name
	Query       string           `json:"query,omitempty"`   // #153: query-adaptive read — keep only body branches touching the query
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
	Index      int    `json:"index"`       // replace-slice / replace-hunk
	New        string `json:"new"`         // replace-slice / replace-hunk
	Old        string `json:"old"`         // replace-hunk
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
	// Force opts out of safety checks (currently: safe-delete's
	// caller-count refusal). Ignored by ops that don't have a safety
	// gate. Default false — safe delete refuses on any references.
	Force bool `json:"force,omitempty"`
	// Query, when non-empty, activates #153 query-adaptive read:
	// return only body statements whose source contains any token
	// from the query. Elided runs collapse to a single comment stub.
	// No-op if the body has <2 statements or all statements match.
	Query string `json:"query,omitempty"`
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
	raw, err := s.backend.GetFileSource(d.ModuleID, d.SourceFile)
	if err != nil {
		return 0
	}
	return len(raw)
}

// withUsage optionally appends a one-line savings footer to r's text
// content when the alt-Read savings are dramatic (≥50%) and non-trivial
// (alt ≥ 512 bytes). No-op on nil / error results.
//
// Historical note: this function previously ALSO set r.StructuredContent
// = u for bench harnesses. Claude's tool_result serialization treats
// structuredContent as a replacement for text content when both are set,
// so every read/read-file/outline/slice/file-defs/expand response
// silently reached the model as a JSON usage envelope — no body text
// at all. Detected 2026-07-20 via bench trajectories where the model
// literally complained "content stripped for the whole session." The
// StructuredContent write is removed; the footer stays because it's
// visible in-band and useful signal.
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

// notFoundSuggestCap bounds the "did you mean" list attached to
// definition-not-found errors. 5 is enough to catch the common
// case-swap / prefix / suffix miss without ballooning error size.
const notFoundSuggestCap = 5

// notFoundOrErr distinguishes a genuine "not found" from a real DB
// error (e.g. scan crash, connection failure). Winze dispatch 2026-07-22:
// swallowing every getter error as notFoundResult cost them an hour
// misdiagnosing the TextStorage regression — GetDefinitionByName was
// crashing on scan and the caller reported "not found", masking the
// real bug. Callers should use this in place of the bare notFoundResult
// call after a GetDefinitionByName-shape lookup.
func (s *server) notFoundOrErr(name string, err error) (*sdkmcp.CallToolResult, any, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return s.notFoundResult(name)
	}
	return errResult(fmt.Errorf("lookup %q: %w", name, err))
}

// notFoundResult builds the "definition %q not found" error and — when
// the DB has close-name candidates — appends a compact "Did you mean:"
// list so the model can retry with a real def name instead of a bare
// grep. Falls back to the plain error when no candidates match, so
// zero-length arg or truly-absent name don't get noisy suggestions.
func (s *server) notFoundResult(name string) (*sdkmcp.CallToolResult, any, error) {
	msg := fmt.Sprintf("definition %q not found", name)
	if name == "" || s.backend == nil {
		return errResult(fmt.Errorf("%s", msg))
	}
	// Case-insensitive prefix/suffix contains — the common cases are
	// "case wrong", "receiver missing", "prefix/suffix mismatch".
	// FindDefinitions ORDER BY name so we get a stable head-of-list.
	cands, err := s.backend.FindDefinitions("%" + name + "%")
	if err != nil || len(cands) == 0 {
		return errResult(fmt.Errorf("%s", msg))
	}
	var seen []string
	dedup := make(map[string]bool, len(cands))
	for _, c := range cands {
		key := formatReceiver(c.Receiver) + c.Name
		if dedup[key] {
			continue
		}
		dedup[key] = true
		seen = append(seen, key)
		if len(seen) >= notFoundSuggestCap {
			break
		}
	}
	suffix := ""
	if len(cands) > len(seen) {
		suffix = fmt.Sprintf(" (+%d more — refine with op:\"search\" pattern:%q)",
			len(cands)-len(seen), "%"+name+"%")
	}
	full := fmt.Sprintf("%s. Did you mean: %s%s",
		msg, strings.Join(seen, ", "), suffix)
	return errResult(fmt.Errorf("%s", full))
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
	mods, _ := s.backend.ListModules() // best effort — nil is safe
	for _, m := range mods {
		if strings.EqualFold(m.Name, query) ||
			strings.Contains(strings.ToLower(m.Path), strings.ToLower(query)) {
			return &m
		}
	}
	return nil
}

func (s *server) modulePath(moduleID int64) string {
	mods, _ := s.backend.ListModules() // best effort — nil is safe
	for _, m := range mods {
		if m.ID == moduleID {
			return m.Path
		}
	}
	return ""
}

// handleCode is the single entry point for all operations.
// It dispatches based on the "op" field to the appropriate handler.
func (s *server) handleCode(ctx context.Context, req *sdkmcp.CallToolRequest, args codeParam) (result *sdkmcp.CallToolResult, structured any, err error) {
	// #77/#152: post-dispatch dedup. Read ops that return byte-identical
	// content on repeat get replaced with a compact "already served" stub;
	// write ops invalidate the session cache so the next read is a clean
	// miss. See internal/mcp/dedup.go.
	defer func() {
		if err != nil || result == nil || result.IsError || req == nil {
			return
		}
		if op, argKey, ok := dedupOpKey(args); ok {
			result = s.respCache.dedup(req.Session, op, argKey, result)
			return
		}
		if isWriteOp(args.Op) {
			s.respCache.invalidate(req.Session)
			// #154: reachability cache is a graph snapshot; any
			// mutation invalidates. Next impact/batch-impact will
			// scan refs to rebuild. Nil-safe for Measure* paths
			// that construct servers without going through
			// newMCPServer.
			if s.reach != nil {
				s.reach.invalidate()
			}
		}
	}()

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
	case "replace-hunk":
		if r, o, e := need(args.Name, "name"); r != nil {
			return r, o, e
		}
		if r, o, e := need(args.Old, "old"); r != nil {
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
		return wrapStale(s.handleGetDefinition(ctx, req, nameParam{Name: args.Name, Full: args.Full, Query: args.Query}))
	case "read-and-verify":
		return wrapStale(s.handleReadAndVerify(ctx, req, args))
	case "retarget-field-value":
		return s.handleRetargetFieldValue(ctx, req, args)
	case "outline":
		return wrapStale(s.handleOutline(ctx, req, nameParam{Name: args.Name, Query: args.Query}))
	case "slice":
		return wrapStale(s.handleSlice(ctx, req, args))
	case "insert-precondition":
		return s.handleInsertPrecondition(ctx, req, args)
	case "replace-slice":
		return s.handleReplaceSlice(ctx, req, args)
	case "replace-hunk":
		return s.handleReplaceHunk(ctx, req, args)
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
		return s.handleDelete(ctx, req, nameParam{Name: args.Name, Force: args.Force})
	case "rename":
		return s.handleRename(ctx, req, renameParam{OldName: args.OldName, NewName: args.NewName})
	case "move":
		return s.handleMove(ctx, req, moveParam{Name: args.Name, ToModule: args.Module})
	case "test":
		if args.Test != "" {
			return s.handleTestByName(ctx, req, args.Test)
		}
		return s.handleTest(ctx, req, nameParam{Name: args.Name})
	case "similar":
		return wrapStale(s.handleSimilar(ctx, req, nameParam{Name: args.Name}))
	case "apply":
		return s.handleApply(ctx, req, applyParam{Operations: args.Operations, DryRun: args.DryRun})
	case "query":
		return wrapStale(s.handleQuery(ctx, req, sqlParam{SQL: args.SQL}))
	case "find":
		return wrapStale(s.handleFind(ctx, req, findParam{File: args.File, Line: args.Line}))
	case "overview":
		return wrapStale(s.handleOverview(ctx, req, args))
	case "methods":
		return wrapStale(s.handleMethods(ctx, req, nameParam{Name: args.Name, Query: args.Query}))
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
	case "emit":
		return s.handleEmit(ctx, req, args)
	case "gc":
		return s.handleGC(ctx, req, args)
	default:
		return errResult(fmt.Errorf("unknown op %q — valid: read, read-and-verify, outline, slice, insert-precondition, replace-slice, replace-hunk, wrap-in-defer, rename-param, add-import, search, impact, explain, similar, untested, edit, create, delete, retarget-field-value, rename, move, test, apply, diff, history, query, find, sync, test-coverage, batch-impact, simulate, validate-plan, pragmas, literals, traverse, branch, checkout, merge, commit, status, conflicts, resolve, merge-abort, diff-defs, emit, gc", args.Op))
	}
}

func (s *server) handleImpact(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}
	impact, err := s.backend.GetImpact(d.ID)
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
	// #157 query-context: filter callers to those whose name/
	// receiver/source_file matches any query token. Matching first,
	// non-matching hidden with a "N others" line.
	var queryHiddenProd, queryHiddenTest int
	if strings.TrimSpace(args.Query) != "" {
		tokens := extractQueryTokensLower(args.Query)
		if len(tokens) > 0 {
			prodCallers, queryHiddenProd = filterCallersByQuery(prodCallers, tokens)
			testCallers, queryHiddenTest = filterCallersByQuery(testCallers, tokens)
		}
	}
	// MDL surprise-first: if the safety-relevant signal is abnormal,
	// lead with it. A def with production callers but zero test
	// coverage is the highest-info bit for "is it safe to change?"
	// Buried at the bottom of the response prior, model often stops
	// reading before it — now the WARNING is line 3.
	if impact.UncoveredBy > 0 && len(prodCallers) > 0 {
		sb.WriteString(fmt.Sprintf("⚠ WARNING: %d/%d direct production callers have no test coverage — a change here may break code no test will catch.\n\n",
			impact.UncoveredBy, len(prodCallers)))
	} else if len(prodCallers) > 0 && len(impact.Tests) == 0 {
		sb.WriteString(fmt.Sprintf("⚠ WARNING: %d production callers, 0 tests covering this def. Ship-blocking risk on any semantic change.\n\n",
			len(prodCallers)))
	}
	sb.WriteString(fmt.Sprintf("Direct callers: %d (%d production, %d test)\n", len(impact.DirectCallers), len(prodCallers)+queryHiddenProd, len(testCallers)+queryHiddenTest))
	if queryHiddenProd+queryHiddenTest > 0 {
		sb.WriteString(fmt.Sprintf("  filtered by query=%q: %d callers hidden (%d production, %d test)\n",
			args.Query, queryHiddenProd+queryHiddenTest, queryHiddenProd, queryHiddenTest))
	}
	// #156: workspace-aware breakdown. On multi-module trees (winze:
	// 20 go.mod files under one repo) the model can't see the shape
	// of the blast without file-path inspection. Group callers by
	// module path so "10 callers in winze/, 3 in polecats/quartz"
	// is legible at a glance. Only emitted when callers span >1
	// module — no noise on single-module projects.
	if len(impact.DirectCallers) > 0 {
		if byMod := callerBreakdownByModule(s, impact.DirectCallers, impact.Module); len(byMod) > 1 {
			sb.WriteString("  by module: ")
			sb.WriteString(byMod)
			sb.WriteString("\n")
		}
	}
	for i, c := range prodCallers {
		if i >= impactCallerCap {
			sb.WriteString(fmt.Sprintf("  … (%d more production callers omitted; pass format:\"json\" for full list)\n", len(prodCallers)-impactCallerCap))
			break
		}
		name := formatReceiver(c.Receiver) + c.Name
		if c.SourceFile != "" && c.StartLine > 0 {
			sb.WriteString(fmt.Sprintf("  %s  (%s:%d)\n", name, c.SourceFile, c.StartLine))
		} else {
			sb.WriteString(fmt.Sprintf("  %s\n", name))
		}
	}
	sb.WriteString(fmt.Sprintf("Transitive callers: %d\n", impact.TransitiveCount))
	sb.WriteString(fmt.Sprintf("Tests covering this: %d\n", len(impact.Tests)))
	// L15: surface test names + a coherence hint. When none of the covering
	// test names lexically contain the def name (case-insensitive), the def
	// is likely indirectly tested — a bugfix here may not be verified by
	// its own coverage. Cheap "you may be looking at the wrong def" signal.
	if names := testNames(impact.Tests, impactTestNameCap); len(names) > 0 {
		sb.WriteString(fmt.Sprintf("  Names: %s\n", strings.Join(names, ", ")))
		if !anyTestNameMentions(impact.Tests, impact.Definition.Name) {
			sb.WriteString("  Note: no covering test name mentions this def by name — coverage is indirect. If you fix it, prefer running one of the above tests to verify (op:test test:\"<TestX>\").\n")
		}
	}
	if impact.UncoveredBy > 0 {
		sb.WriteString(fmt.Sprintf("Uncovered direct callers: %d\n", impact.UncoveredBy))
	}

	return textResult(sb.String()), nil, nil
}

const impactTestNameCap = 10

// extractQueryTokensLower splits a free-form query into ≥2-char
// case-folded tokens. Non-identifier chars are separators. Mirror of
// internal/projection's version but kept inline to avoid exporting.
// #157.
func extractQueryTokensLower(query string) []string {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	low := strings.ToLower(query)
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 2 {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	for _, r := range low {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// filterCallersByQuery partitions callers into (matching, hiddenCount)
// based on whether their name/receiver/source_file contains any
// query token (case-insensitive substring). Order preserved among
// matching entries. #157.
func filterCallersByQuery(callers []store.Definition, tokens []string) ([]store.Definition, int) {
	if len(tokens) == 0 {
		return callers, 0
	}
	var kept []store.Definition
	hidden := 0
	for _, c := range callers {
		hay := strings.ToLower(c.Name + " " + c.Receiver + " " + c.SourceFile)
		matched := false
		for _, t := range tokens {
			if strings.Contains(hay, t) {
				matched = true
				break
			}
		}
		if matched {
			kept = append(kept, c)
		} else {
			hidden++
		}
	}
	return kept, hidden
}

// callerBreakdownByModule groups callers by their module path and
// returns a compact "modA (12), modB (3), modC (1)" string, or ""
// if there's only one module represented (in which case the flat
// caller list already tells the story). #156.
//
// Returns a map of module→count as the string body; callers use
// len(...) via the returned display string's semicolon count
// approximation — actually just return the count as a second value.
func callerBreakdownByModule(s *server, callers []store.Definition, selfModule string) string {
	if len(callers) == 0 {
		return ""
	}
	mods, err := s.backend.ListModules()
	if err != nil {
		return ""
	}
	// module_id → path
	pathByID := make(map[int64]string, len(mods))
	for _, m := range mods {
		pathByID[m.ID] = m.Path
	}
	// count per module
	counts := make(map[string]int)
	for _, c := range callers {
		p := pathByID[c.ModuleID]
		if p == "" {
			p = "(unknown module)"
		}
		counts[p]++
	}
	// Distinct modules? If only one, and it's the target's own,
	// caller has no cross-module info — skip.
	if len(counts) < 2 {
		return ""
	}
	// Sort by count desc, then path asc for stability.
	type modCount struct {
		Path  string
		Count int
	}
	entries := make([]modCount, 0, len(counts))
	for p, c := range counts {
		entries = append(entries, modCount{p, c})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count != entries[j].Count {
			return entries[i].Count > entries[j].Count
		}
		return entries[i].Path < entries[j].Path
	})
	// Trim overly-long paths for display (keep last 2 segments if long).
	shorten := func(p string) string {
		if len(p) < 40 {
			return p
		}
		segs := strings.Split(p, "/")
		if len(segs) <= 2 {
			return p
		}
		return ".../" + strings.Join(segs[len(segs)-2:], "/")
	}
	var out []string
	const cap = 6
	for i, e := range entries {
		if i >= cap {
			remaining := 0
			for _, r := range entries[cap:] {
				remaining += r.Count
			}
			out = append(out, fmt.Sprintf("+%d more modules (%d callers)", len(entries)-cap, remaining))
			break
		}
		marker := ""
		if e.Path == selfModule {
			marker = "*" // caller is in the target's own module
		}
		out = append(out, fmt.Sprintf("%s%s (%d)", marker, shorten(e.Path), e.Count))
	}
	return strings.Join(out, ", ")
}

// testNames returns up to `cap` test names, in the order impact.Tests
// arrived (which is source-file order). Used by the markdown formatter.
func testNames(tests []store.Definition, cap int) []string {
	out := make([]string, 0, len(tests))
	for _, t := range tests {
		out = append(out, t.Name)
		if len(out) >= cap {
			break
		}
	}
	return out
}

// anyTestNameMentions reports whether any test in `tests` has the def
// name as a case-insensitive substring in its name. Cheap coherence
// check for the L15 hint — if the def is Foo and no test contains "foo",
// the def is indirectly tested and the model should verify via a named
// test rather than assume coverage means safety.
func anyTestNameMentions(tests []store.Definition, defName string) bool {
	if defName == "" || len(tests) == 0 {
		return true
	}
	needle := strings.ToLower(defName)
	for _, t := range tests {
		if strings.Contains(strings.ToLower(t.Name), needle) {
			return true
		}
	}
	return false
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

// handleReadAndVerify reads a def AND runs its covering tests in one call.
// L14: agents that read + read + read never see behavior; combining source
// with observed test outcome in one turn short-circuits the read-loop.
// Reuses handleGetDefinition + handleTest so ranking, upstream matching,
// and test truncation stay consistent with the individual ops.
func (s *server) handleReadAndVerify(ctx context.Context, req *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	readResult, _, err := s.handleGetDefinition(ctx, req, nameParam{Name: args.Name, Full: args.Full})
	if err != nil {
		return nil, nil, err
	}
	if readResult != nil && readResult.IsError {
		return readResult, nil, nil
	}
	testResult, _, err := s.handleTest(ctx, req, nameParam{Name: args.Name})
	if err != nil {
		return readResult, nil, nil // read succeeded; surface it even if test wiring failed
	}
	var sb strings.Builder
	sb.WriteString(resultTextRaw(readResult))
	sb.WriteString("\n---\n")
	sb.WriteString(resultTextRaw(testResult))
	return textResult(sb.String()), nil, nil
}

// resultTextRaw extracts the text content of a CallToolResult. Empty
// string when there is no TextContent. Cheap concatenation helper for
// ops that stitch other ops' outputs together (read-and-verify).
func resultTextRaw(r *sdkmcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	for _, c := range r.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func (s *server) handleGetDefinition(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}

	// Look up module path for this definition.
	var modulePath string
	mods, _ := s.backend.ListModules() // best effort — nil is safe
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
		if match, _ := s.backend.FindUpstreamMatch(modulePath, upstreamName, d.Kind, d.Receiver, hash); match != nil {
			return s.renderUpstreamMatch(d, match, modulePath)
		}
		// Miss: check whether any version of this def is known upstream.
		// If yes, it means the local copy has diverged — annotate the
		// body so the reader knows they're looking at patched code.
		if versions, _ := s.backend.FindUpstreamVersions(modulePath, upstreamName, d.Kind, d.Receiver); len(versions) > 0 {
			return s.renderDivergedFromUpstream(d, versions, modulePath)
		}
	}

	// #153: query-adaptive read. When args.Query is set, filter body
	// statements to those containing any query token. Elided statements
	// collapse to a single "…" comment; runs of elided stmts share one
	// stub. No-op when body has <2 stmts, all match, nothing matches,
	// or the hint header would exceed the byte savings.
	body := d.Body
	var queryHint string
	if strings.TrimSpace(args.Query) != "" {
		filtered, kept, elided := projection.FilterBodyByQuery(d.Body, args.Query)
		if elided > 0 && kept > 0 {
			candidateHint := fmt.Sprintf(
				"[query-adaptive read: query=%q, %d/%d statements kept, %d elided. Pass query=\"\" for the full body.]\n\n",
				args.Query, kept, kept+elided, elided,
			)
			// Only apply when the filter is a net win — the hint
			// header costs ~140 bytes; on tiny bodies it can dwarf
			// the elision savings.
			if len(filtered)+len(candidateHint) < len(d.Body) {
				body = filtered
				queryHint = candidateHint
			}
		}
	}

	var sb strings.Builder
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", recv, d.Name, d.Kind))
	sb.WriteString(fmt.Sprintf("Module: %s\n\n", modulePath))
	if queryHint != "" {
		sb.WriteString(queryHint)
	}
	if d.Doc != "" {
		sb.WriteString(d.Doc + "\n\n")
	}
	sb.WriteString("```go\n")
	sb.WriteString(body)
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
		defs, err = s.backend.FindDefinitions(args.Pattern)
	} else {
		// Search names/signatures first (indexed, fast).
		defs, err = s.backend.FindDefinitions("%" + args.Pattern + "%")
		if err != nil || len(defs) == 0 {
			// Fall back to body/doc search. Uses trigram FTS5 under
			// SQLite (task #137) which treats `_` as content, so no
			// underscore-guard needed anymore.
			defs, err = s.backend.SearchDefinitions(args.Pattern)
		}
	}
	if err != nil {
		return errResult(err)
	}

	limit := maxSearchResults
	if args.Limit > 0 {
		limit = args.Limit
	}

	// Stage 3: substring body-scan with def-scoped snippets. Reached
	// when name-LIKE + FTS body match both produced nothing. Trigram
	// FTS5 already substring-matches on bodies (task #137), so this
	// path is rarely hit — mostly useful when the caller supplied a
	// LIKE glob (%JobsURL%) that FTS wouldn't parse as a phrase.
	if len(defs) == 0 && args.Pattern != "" {
		scanPattern := strings.Trim(args.Pattern, "%")
		if scanPattern != "" && !strings.Contains(scanPattern, "%") {
			return s.bodyScanResult(scanPattern, limit)
		}
	}

	// Auto-rank when the candidate set exceeds `limit`. Alphabetical
	// truncation buries the useful defs behind whatever sorts first,
	// so trigger the caller-count/text-overlap ranker so the head of
	// the list is actually informative. Explicit rank:true still works.
	if args.Rank || len(defs) > limit {
		return s.rankedSearchResult(args.Pattern, defs, limit)
	}

	type summary struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Receiver string `json:"receiver,omitempty"`
		Preview  string `json:"preview,omitempty"`
	}
	var results []summary
	for _, d := range defs {
		if len(results) >= limit {
			break
		}
		// #159: inline body preview for the top-N hits collapses the
		// grep→view bigram (867 occurrences in the Multi-SWE-bench Go
		// corpus). Cap at 3 previews per response so it doesn't inflate
		// on name-browse queries; cap each preview at 5 lines. Model can
		// still call read for the full body.
		s := summary{Name: d.Name, Kind: d.Kind, Receiver: d.Receiver}
		if len(results) < searchPreviewCount {
			s.Preview = topLinesOfBody(d.Body, searchPreviewLines)
		}
		results = append(results, s)
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

// topLinesOfBody returns the first n lines of body with a "…" marker
// appended if the body was truncated. Empty body → empty string.
// Used by handleSearch (#159) to give each top hit a body preview so
// the model doesn't need a follow-up read on the winning result.
func topLinesOfBody(body string, n int) string {
	if body == "" || n <= 0 {
		return ""
	}
	lines := strings.SplitN(body, "\n", n+1)
	if len(lines) <= n {
		return body
	}
	return strings.Join(lines[:n], "\n") + "\n…"
}

// bodyScanResult formats stage-3 search results (substring-in-body hits)
// as compact JSON with def name + file:line + snippet, so the model can
// re-locate the match without a follow-up read. Empty result set returns
// a message that names the fallback tried, distinguishing "no def named
// X + no body containing X" from "search op failed silently."
func (s *server) bodyScanResult(pattern string, limit int) (*sdkmcp.CallToolResult, any, error) {
	hits, err := s.backend.SearchBodiesLike(pattern, limit)
	if err != nil {
		return errResult(fmt.Errorf("search body-scan: %w", err))
	}
	if len(hits) == 0 {
		msg := fmt.Sprintf(
			"[no matches for %q — tried name-LIKE, FTS on doc+body, and substring body-scan. If you're grepping for a comment or string literal, this substring wasn't found in any indexed body. Try `overview` for project shape or a broader pattern.]",
			pattern,
		)
		return textResult(msg), nil, nil
	}
	type match struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Receiver   string `json:"receiver,omitempty"`
		SourceFile string `json:"file"`
		Line       int    `json:"line"`
		Snippet    string `json:"snippet"`
	}
	var out []match
	for _, h := range hits {
		out = append(out, match{
			Name: h.Name, Kind: h.Kind, Receiver: h.Receiver,
			SourceFile: h.SourceFile, Line: h.Line, Snippet: h.Snippet,
		})
	}
	text, err := toJSON(out)
	if err != nil {
		return errResult(err)
	}
	text = fmt.Sprintf(
		"[body-scan for %q — %d hits. Each row is a definition whose body contains the substring. Use `read name:\"<Name>\"` for the full body.]\n%s",
		pattern, len(hits), text,
	)
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
	callers, tests, err := s.backend.RefCountsByTarget(ids)
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
	callers, tests, err := s.backend.RefCountsByTarget(ids)
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
		Preview  string  `json:"preview,omitempty"`
	}
	out := make([]rankedSummary, 0, limit)
	for i, r := range scored {
		if i >= limit {
			break
		}
		rs := rankedSummary{
			Name: r.Def.Name, Kind: r.Def.Kind, Receiver: r.Def.Receiver,
			Score: r.Score,
		}
		// #159: preview the top-N ranked hits — model can identify the
		// winner from body head without a follow-up read.
		if i < searchPreviewCount {
			rs.Preview = topLinesOfBody(r.Def.Body, searchPreviewLines)
		}
		out = append(out, rs)
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
	defs, err := s.backend.GetUntested()
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
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}

	// Validate new body parses as Go.
	src := "package x\n" + args.NewBody
	if _, parseErr := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments); parseErr != nil {
		return errResult(fmt.Errorf("new_body has syntax error: %v", parseErr))
	}

	// Capture the pre-edit signature so we can decide whether the build
	// gate is safely skippable (#148: body-only edit with a stable
	// signature keeps dispatch invariant — callers don't need re-typecheck).
	// Use extractSignature on both sides so the comparison is
	// AST-canonicalized (d.Signature from ingest has doc-comment prefix
	// lines; extractSignature strips them — comparing them directly
	// false-positives "sig changed" on every doc-adjacent edit).
	oldSignature := extractSignature(d.Body)
	d.Body = args.NewBody
	d.Signature = extractSignature(args.NewBody)

	id, err := s.backend.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}

	recv := formatReceiver(d.Receiver)

	sigStable := oldSignature == d.Signature
	var buildResult string
	if sigStable {
		buildResult = s.autoEmitOnly(d.SourceFile)
	} else {
		if os.Getenv("DEFN_MEASURE_TIMING") == "1" {
			fmt.Fprintf(os.Stderr, "  [edit] signature changed, build required:\n    old: %q\n    new: %q\n", oldSignature, d.Signature)
		}
		buildResult = s.autoEmitAndBuildForFile(d.SourceFile)
	}

	// #150: sig-stable body edit → refs graph is safe to defer.
	//   - Callers unaffected (refs are by def-ID, IDs stable, sig stable
	//     means dispatch stable too)
	//   - Interface satisfaction unaffected (sig-driven)
	//   - Only D's OUTGOING refs may have changed (D calls new funcs /
	//     stops calling old ones). Those refresh on the next full sync
	//     or explicit `code(op:"sync")`.
	// Skips ResolveFile's ~200ms packages.Load + all-file resolve.
	// Signature-changing edits still eagerly re-resolve (dispatch shifts).
	//
	// Set DEFN_STRICT_BUILD=1 to also force eager resolve (same escape
	// hatch as autoEmitOnly's build gate).
	if sigStable && os.Getenv("DEFN_STRICT_BUILD") != "1" {
		if os.Getenv("DEFN_MEASURE_TIMING") == "1" {
			fmt.Fprintf(os.Stderr, "  [edit] resolve deferred (sig-stable; run code(op:\"sync\") to refresh D's outgoing refs)\n")
		}
	} else {
		s.autoResolveFile(d.SourceFile, s.modulePath(d.ModuleID))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Updated %s%s (id=%d, hash=%s)\n", recv, d.Name, id, store.HashBody(args.NewBody)[:12]))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}

	// Impact nudge: show callers if this definition has any.
	if impact, err := s.backend.GetImpact(id); err == nil && len(impact.DirectCallers) > 0 {
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
		resolve.ResolveModule(s.backend, s.projectDir, modulePath)
	} else {
		resolve.Resolve(s.backend, s.projectDir)
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

// autoResolveFile is the file-scoped counterpart to autoResolve. Used
// after single-def edits when we know which source file changed — calls
// resolve.ResolveFile (loads ONE package, not the whole module) instead
// of resolve.ResolveModule (loads ./... for the whole project). #109.
//
// Caveat: cross-package refs FROM other packages TO the changed def's
// package aren't refreshed here — see resolve.ResolveFile doc. That's
// the same limitation cmdSync's fast path lives with; for op:edit /
// op:create / op:delete on a single def within its own package, callers
// in other packages don't need re-resolve because their outgoing edges
// are ID-based and IDs are stable across body-edit UpsertDefinition.
// Fall back to autoResolve(modulePath) if sourceFile is empty (e.g.,
// caller didn't have file info handy).
func (s *server) autoResolveFile(sourceFile, modulePath string) {
	if s.projectDir == "" {
		return
	}
	if sourceFile == "" {
		s.autoResolve(modulePath)
		return
	}
	absFile := filepath.Join(s.projectDir, sourceFile)
	_ = resolve.ResolveFile(s.backend, s.projectDir, absFile) // best-effort
	if err := s.autoCommit(); err != nil {
		fmt.Fprintf(os.Stderr, "defn: auto-commit failed (post-resolve): %v\n", err)
	}
	s.lastResolved.Store(time.Now().UnixNano())
	if s.idf != nil {
		s.idf.Invalidate()
	}
}

// autoCommit is a no-op checkpoint that keeps the storage compact.
// Under SQLite writes persist on tx commit — there's no working-set-to-
// branch step like Dolt had. The GC hook (WAL checkpoint) still fires
// every 10 calls; the time-based ticker (startGCTicker) covers serves
// that don't hit 10 within the tick window.
func (s *server) autoCommit() error {
	s.backend.CleanTempFiles()
	if n := s.autoCommitCount.Add(1); n%10 == 0 {
		go s.backend.GC() // background — GC can be slow on large databases
	}
	return nil
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
			if err := s.backend.GC(); err != nil {
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
	if err := ingest.IngestPackages(s.backend, pkgs, s.projectDir); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	if err := resolve.ResolvePackages(s.backend, pkgs, s.projectDir); err != nil {
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

// autoEmitAndBuildForFile is the file-scoped variant: pass the single
// source file the mutation touched, get goimports AND emit scoped to
// that file via emit.Opts.GoimportsFiles + Opts.TouchedFiles. On
// cli/cli warm rename this dropped goimports from 707ms → 11ms (#109
// pass 3); #117 adds the same scoping to emit itself so we don't
// rewrite every file in the tree for a single-def mutation (winze:
// 1.2s full-emit → per-touched-file cost). Empty file falls through
// to the full-project recursive form.
func (s *server) autoEmitAndBuildForFile(sourceFile string) string {
	if sourceFile == "" {
		return s.autoEmitAndBuild()
	}
	return s.autoEmitAndBuildWithOpts(emit.Opts{
		GoimportsFiles: []string{sourceFile},
		TouchedFiles:   []string{sourceFile},
	})
}

// autoEmitOnly emits without running `go build` — for projection ops
// that are AST-guaranteed sig-stable (insert-precondition, replace-slice,
// replace-hunk, wrap-in-defer, rename-param, add-import). Task #148:
// on winze, rename+build was 187ms with 148ms of that in go build;
// skipping the build takes the op to ~35ms and delivers the "faster
// than native because the index is maintained" thesis as a
// demonstrable fact rather than an aspiration.
//
// Safety: these ops preserve syntactic well-formedness by construction
// (they transform an already-valid AST). They CAN produce type errors
// (undefined identifier in a new precondition, wrong signature in a
// hunk replacement) — those surface on the next op that builds, or on
// an explicit code(op:"test") / native `go build`. The DB is
// authoritative; the emitted file is a projection.
//
// autoResolveFile still runs downstream via the callers so the ref
// graph stays consistent. Only the go-build gate is deferred.
//
// Set DEFN_STRICT_BUILD=1 to force the build (opt-out for users who
// want the old per-mutation gate — bench harnesses, CI, cautious flows).
func (s *server) autoEmitOnly(sourceFile string) string {
	opts := emit.Opts{}
	if sourceFile != "" {
		opts.GoimportsFiles = []string{sourceFile}
		opts.TouchedFiles = []string{sourceFile}
	}
	return s.autoEmitOnlyWithOpts(opts)
}

// autoEmitOnlyWithOpts is the multi-file variant used by handleRename,
// which touches the def's own file plus each caller's file.
func (s *server) autoEmitOnlyWithOpts(opts emit.Opts) string {
	if os.Getenv("DEFN_STRICT_BUILD") == "1" {
		return s.autoEmitAndBuildWithOpts(opts)
	}
	if s.projectDir == "" || os.Getenv("DEFN_LEGACY") == "1" {
		return "Saved to database."
	}
	timing := os.Getenv("DEFN_MEASURE_TIMING") == "1"

	t := time.Now()
	if err := emit.EmitWithOpts(s.backend, s.projectDir, opts); err != nil {
		return fmt.Sprintf("emit error: %v", err)
	}
	if timing {
		fmt.Fprintf(os.Stderr, "  [emit] emit.EmitWithOpts (build deferred): %s\n", time.Since(t).Round(time.Millisecond))
	}
	return "Build: deferred (safe mutation)"
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
	timing := os.Getenv("DEFN_MEASURE_TIMING") == "1"

	// Emit to the actual project directory — keeps files in sync.
	t := time.Now()
	if err := emit.EmitWithOpts(s.backend, s.projectDir, opts); err != nil {
		return fmt.Sprintf("emit error: %v", err)
	}
	if timing {
		fmt.Fprintf(os.Stderr, "  [emit] emit.EmitWithOpts: %s\n", time.Since(t).Round(time.Millisecond))
	}

	t = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	// #118 winze dispatch 2026-07-22: `go build ./...` on winze's corpus
	// drags in cmd/ cgo Dolt subtrees (seconds); the corpus itself gates
	// with `go build .` (25ms). When TouchedFiles is set, scope the build
	// to just the packages containing those files. Empty TouchedFiles
	// (full-tree emit) keeps the old ./... behavior for correctness on
	// broad changes.
	buildTargets := buildTargetsForFiles(opts.TouchedFiles)
	args := append([]string{"build"}, buildTargets...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = s.projectDir
	out, err := cmd.CombinedOutput()
	if timing {
		fmt.Fprintf(os.Stderr, "  [emit] go %s: %s\n", strings.Join(args, " "), time.Since(t).Round(time.Millisecond))
	}
	if err != nil {
		return fmt.Sprintf("BUILD FAILED:\n%s", string(out))
	}
	return "Build: OK"
}

// buildTargetsForFiles derives the minimal `go build` target list from
// the set of touched project-relative files. Empty input → ["./..."]
// (full-tree, safe default). Non-empty → sorted unique "./<dir>"
// entries (or "." for root-package files). Directory is the parent of
// each file, mapped to a package path Go understands.
func buildTargetsForFiles(files []string) []string {
	if len(files) == 0 {
		return []string{"./..."}
	}
	seen := map[string]bool{}
	var targets []string
	for _, f := range files {
		clean := filepath.Clean(f)
		if filepath.IsAbs(clean) || strings.Contains(clean, "..") {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(clean))
		var target string
		if dir == "" || dir == "." {
			target = "."
		} else {
			target = "./" + dir
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return []string{"./..."}
	}
	sort.Strings(targets)
	return targets
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
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
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

	id, err := s.backend.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuildForFile(d.SourceFile)
	s.autoResolveFile(d.SourceFile, s.modulePath(d.ModuleID))

	var sb strings.Builder
	replaced := "1 occurrence"
	if args.ReplaceAll {
		replaced = fmt.Sprintf("%d occurrences", count)
	}
	sb.WriteString(fmt.Sprintf("Edited %s%s — replaced %s\n", recv, d.Name, replaced))
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	if impact, err := s.backend.GetImpact(id); err == nil && len(impact.DirectCallers) > 0 {
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
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
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

	if _, err := s.backend.UpsertDefinition(d); err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuildForFile(d.SourceFile)
	s.autoResolveFile(d.SourceFile, s.modulePath(d.ModuleID))

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
		mods, _ := s.backend.ListModules() // best effort — nil is safe
		if len(mods) > 0 {
			mod = &mods[0]
		}
	}
	if mod == nil {
		return errResult(fmt.Errorf("no modules found — run defn init first"))
	}

	// Check if a definition with this name already exists in the target module.
	if existing, err := s.backend.GetDefinitionByName(name, mod.Path); err == nil {
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
	id, err := s.backend.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuildForFile(args.File)
	s.autoResolveFile(args.File, mod.Path)

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
		mods, _ := s.backend.ListModules()
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
		if existing, err := s.backend.GetDefinitionByName(d.Name, mod.Path); err == nil {
			recv := formatReceiver(existing.Receiver)
			return errResult(fmt.Errorf("definition %s%s already exists in %s (id=%d) — use code(op:\"edit\") to modify it", recv, d.Name, mod.Path, existing.ID))
		}
	}

	commit, rollback, txErr := s.backend.Begin()
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
		id, err := s.backend.UpsertDefinition(def)
		if err != nil {
			return errResult(fmt.Errorf("upsert %s: %v", d.Name, err))
		}
		ids = append(ids, id)
	}

	if err := commit(); err != nil {
		return errResult(fmt.Errorf("commit: %v", err))
	}

	buildResult := s.autoEmitAndBuildForFile(args.File)
	s.autoResolveFile(args.File, mod.Path)

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
	mods, _ := s.backend.ListModules() // best effort — nil is safe
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
				if _, err := s.backend.GetDefinitionByName(op.Name, ""); err != nil {
					errors = append(errors, fmt.Sprintf("edit %s: not found", op.Name))
				} else {
					sb.WriteString(fmt.Sprintf("~ would edit %s\n", op.Name))
				}
			case "delete":
				if _, err := s.backend.GetDefinitionByName(op.Name, ""); err != nil {
					errors = append(errors, fmt.Sprintf("delete %s: not found", op.Name))
				} else {
					sb.WriteString(fmt.Sprintf("- would delete %s\n", op.Name))
				}
			case "rename":
				if op.Name == "" || op.NewName == "" {
					errors = append(errors, "rename: both name and new_name are required")
				} else if _, err := s.backend.GetDefinitionByName(op.Name, ""); err != nil {
					errors = append(errors, fmt.Sprintf("rename %s: not found", op.Name))
				} else {
					sb.WriteString(fmt.Sprintf("→ would rename %s → %s\n", op.Name, op.NewName))
				}
			case "insert-precondition", "replace-slice", "replace-hunk", "wrap-in-defer", "rename-param":
				name := op.Name
				if name == "" {
					if inferred, err := s.inferSingleTargetName(); err != nil {
						errors = append(errors, fmt.Sprintf("%s: %v", op.Op, err))
						continue
					} else {
						name = inferred
					}
				}
				if _, err := s.backend.GetDefinitionByName(name, ""); err != nil {
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

	commit, rollback, txErr := s.backend.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback()

	// #114 batch scoping: collect the union of files touched, files whose
	// refs need re-derivation, and qualified names being removed. Mirrors
	// the singleton paths (#109 pass 1/3) so an N-op apply pays one
	// scoped emit + goimports + per-file autoResolveFile at the tail
	// instead of a full-project autoEmitAndBuild + autoResolve.
	type filePkg struct{ file, module string }
	touchedFiles := map[string]bool{}
	resolveSet := map[filePkg]bool{}
	var allowedRemovals []string
	addTouched := func(f string) {
		if f != "" {
			touchedFiles[f] = true
		}
	}
	addResolve := func(f string, moduleID int64) {
		if f == "" {
			return
		}
		mp := s.modulePath(moduleID)
		if mp == "" {
			return
		}
		resolveSet[filePkg{f, mp}] = true
	}

	// projEdit resolves the target name (with single-def inference), runs
	// the pure projection function, validates the new body, and upserts.
	// Body-changing → adds the def's source file to both touched + resolve.
	projEdit := func(op applyOp, compute func(body string) (string, error)) (string, string) {
		name := op.Name
		if name == "" {
			inferred, err := s.inferSingleTargetName()
			if err != nil {
				return "", fmt.Sprintf("%s: %v", op.Op, err)
			}
			name = inferred
		}
		d, err := s.backend.GetDefinitionByName(name, "")
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
		if _, err := s.backend.UpsertDefinition(d); err != nil {
			return "", fmt.Sprintf("%s %s: %v", op.Op, name, err)
		}
		addTouched(d.SourceFile)
		addResolve(d.SourceFile, d.ModuleID)
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
				mods, _ := s.backend.ListModules()
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
			id, err := s.backend.UpsertDefinition(d)
			if err != nil {
				errors = append(errors, fmt.Sprintf("create %s: %v", name, err))
			} else {
				addTouched(op.File)
				addResolve(op.File, mod.ID)
				sb.WriteString(fmt.Sprintf("+ created %s (id=%d)\n", name, id))
			}

		case "edit":
			d, err := s.backend.GetDefinitionByName(op.Name, "")
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
			if _, err := s.backend.UpsertDefinition(d); err != nil {
				errors = append(errors, fmt.Sprintf("edit %s: %v", op.Name, err))
			} else {
				addTouched(d.SourceFile)
				addResolve(d.SourceFile, d.ModuleID)
				sb.WriteString(fmt.Sprintf("~ edited %s\n", op.Name))
			}

		case "delete":
			d, err := s.backend.GetDefinitionByName(op.Name, "")
			if err != nil {
				errors = append(errors, fmt.Sprintf("delete %s: not found", op.Name))
				continue
			}
			if err := s.backend.DeleteDefinition(d.ID); err != nil {
				errors = append(errors, fmt.Sprintf("delete %s: %v", op.Name, err))
			} else {
				addTouched(d.SourceFile)
				// #109 rationale: DeleteDefinition already dropped every refs
				// row where from_def=D OR to_def=D — no resolve needed.
				qualified := d.Name
				if d.Receiver != "" {
					qualified = strings.TrimPrefix(d.Receiver, "*") + "." + d.Name
				}
				allowedRemovals = append(allowedRemovals, qualified)
				sb.WriteString(fmt.Sprintf("- deleted %s\n", op.Name))
			}

		case "rename":
			if op.Name == "" || op.NewName == "" {
				errors = append(errors, "rename: both name and new_name are required")
				continue
			}
			d, err := s.backend.GetDefinitionByName(op.Name, "")
			if err != nil {
				errors = append(errors, fmt.Sprintf("rename %s: not found", op.Name))
				continue
			}
			// Reserve the qualified pre-rename name so safeWriteGoFile lets
			// the disappearing decl actually vanish from the file (same as
			// handleRename's qualifiedOld).
			qualifiedOld := d.Name
			if d.Receiver != "" {
				qualifiedOld = strings.TrimPrefix(d.Receiver, "*") + "." + d.Name
			}
			allowedRemovals = append(allowedRemovals, qualifiedOld)
			addTouched(d.SourceFile)
			d.Body, _ = astRename(d.Body, op.Name, op.NewName)
			d.Name = op.NewName
			d.Signature = extractSignature(d.Body)
			d.Exported = len(op.NewName) > 0 && op.NewName[0] >= 'A' && op.NewName[0] <= 'Z'
			if _, err := s.backend.UpsertDefinition(d); err != nil {
				errors = append(errors, fmt.Sprintf("rename %s: %v", op.Name, err))
				continue
			}
			callers, _ := s.backend.GetCallers(d.ID)
			callerCount := 0
			for _, caller := range callers {
				if strings.Contains(caller.Body, op.Name) {
					caller.Body, _ = astRename(caller.Body, op.Name, op.NewName)
					caller.Signature = extractSignature(caller.Body)
					if _, err := s.backend.UpsertDefinition(&caller); err != nil {
						errors = append(errors, fmt.Sprintf("rename caller %s: %v", caller.Name, err))
					} else {
						addTouched(caller.SourceFile)
						callerCount++
					}
				}
			}
			// #109: rename is ID-preserving semantic transform — refs edges
			// unchanged. Skip adding to resolveSet.
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

		case "replace-hunk":
			line, errStr := projEdit(op, func(body string) (string, error) {
				return projection.ReplaceHunk(body, op.Old, op.New, op.Index)
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
				all, err := s.backend.DistinctSourceFiles()
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
			defs, err := s.backend.FindDefinitionsByFile(dir, file, 0)
			if err != nil || len(defs) == 0 {
				errors = append(errors, fmt.Sprintf("add-import: no defs in %q", file))
				continue
			}
			moduleID := defs[0].ModuleID
			existing, err := s.backend.GetImports(moduleID)
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
			if err := s.backend.SetImports(moduleID, updated); err != nil {
				errors = append(errors, fmt.Sprintf("add-import %q: %v", op.ImportPath, err))
			} else {
				// add-import changes imports header only; body/refs untouched.
				// Touch file so goimports re-formats the imports block.
				addTouched(file)
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

	// #114 batch scoping: if we tracked any touched files, run one scoped
	// emit + goimports + per-file resolve at the tail. Safety valve — if
	// tracking came up empty (every op had empty SourceFile, edge case),
	// fall back to full autoEmitAndBuild + autoResolve for correctness.
	var buildResult string
	if len(touchedFiles) > 0 || len(allowedRemovals) > 0 {
		goimportsFiles := make([]string, 0, len(touchedFiles))
		for f := range touchedFiles {
			goimportsFiles = append(goimportsFiles, f)
		}
		buildResult = s.autoEmitAndBuildWithOpts(emit.Opts{
			AllowedRemovals: allowedRemovals,
			GoimportsFiles:  goimportsFiles,
			TouchedFiles:    goimportsFiles,
		})
		for fp := range resolveSet {
			s.autoResolveFile(fp.file, fp.module)
		}
	} else {
		buildResult = s.autoEmitAndBuild()
		s.autoResolve("")
	}
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}

	return textResult(sb.String()), nil, nil
}

// handleRetargetFieldValue rewrites composite-literal field values across
// every def whose body contains a matching pattern. Winze use case: given
// `Claim{Subject: "s", Object: "OldTarget"}` var-decls scattered across
// many files, change every `Object: "OldTarget"` to `Object: "NewTarget"`
// atomically. Native equivalent is `sed -i 's/Object: "OldTarget"/Object:
// "NewTarget"/g'` + pray no unrelated occurrence collides — AST-safe here.
//
// MVP scope: matches Type{...Field: OLD...} where Field's value is a
// string literal. Non-string values (idents, other composites) skipped;
// return count reports affected defs so the model knows how much moved.
func (s *server) handleRetargetFieldValue(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if args.Name == "" || args.Field == "" {
		return errResult(fmt.Errorf("retarget-field-value: name (struct type) and field are required"))
	}
	if args.Old == "" && args.New == "" {
		return errResult(fmt.Errorf("retarget-field-value: at least one of old, new must be non-empty"))
	}
	typeName := args.Name
	field := args.Field

	// Iterate all modules → all defs. Load once, filter AST-side.
	mods, err := s.backend.ListModules()
	if err != nil {
		return errResult(fmt.Errorf("list modules: %w", err))
	}
	updated := 0
	var affectedNames []string
	// #109 pass 2: collect the (file, modulePath) tuples we touched so
	// we can scope the post-op resolve. Retarget only changes composite
	// literal string values → refs graph is unaffected; only literal_fields
	// need re-derivation for the touched defs. autoResolveFile per unique
	// (file, module) tuple gives us that without a full-project ResolveModule.
	type filePkg struct {
		file, module string
	}
	touched := make(map[filePkg]bool)
	for _, m := range mods {
		defs, err := s.backend.GetModuleDefinitions(m.ID)
		if err != nil {
			continue
		}
		for _, d := range defs {
			newBody, n, ok := retargetFieldInBody(d.Body, typeName, field, args.Old, args.New)
			if !ok || n == 0 {
				continue
			}
			d.Body = newBody
			d.Signature = extractSignature(newBody)
			if _, err := s.backend.UpsertDefinition(&d); err != nil {
				return errResult(fmt.Errorf("update %s: %w", d.Name, err))
			}
			updated++
			if d.SourceFile != "" {
				touched[filePkg{d.SourceFile, m.Path}] = true
			}
			if len(affectedNames) < 10 {
				affectedNames = append(affectedNames, formatReceiver(d.Receiver)+d.Name)
			}
		}
	}

	// #109 pass 3: pass the touched files through to goimports so it
	// only re-formats the changed files instead of walking the whole
	// project tree. Same set already collected for the scoped resolve.
	goimportsFiles := make([]string, 0, len(touched))
	for fp := range touched {
		goimportsFiles = append(goimportsFiles, fp.file)
	}
	buildResult := s.autoEmitAndBuildWithOpts(emit.Opts{
		GoimportsFiles: goimportsFiles,
		TouchedFiles:   goimportsFiles,
	})
	// Scoped resolve: iterate the unique touched files instead of the
	// whole project. Safety valve: if we couldn't collect any touched
	// files (e.g., every def had empty SourceFile — shouldn't happen),
	// fall back to full autoResolve for correctness.
	if len(touched) == 0 {
		s.autoResolve("")
	} else {
		for fp := range touched {
			s.autoResolveFile(fp.file, fp.module)
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Retargeted %s.%s: %q → %q in %d def(s).\n",
		typeName, field, args.Old, args.New, updated))
	if len(affectedNames) > 0 {
		suffix := ""
		if updated > len(affectedNames) {
			suffix = fmt.Sprintf(" (+%d more)", updated-len(affectedNames))
		}
		sb.WriteString("  Affected: " + strings.Join(affectedNames, ", ") + suffix + "\n")
	}
	if buildResult != "" {
		sb.WriteString("\n" + buildResult)
	}
	return textResult(sb.String()), nil, nil
}

// retargetFieldInBody parses `body` as Go source (wrapped in a var decl
// if it doesn't already parse as a top-level decl), walks composite
// literals, and rewrites any `typeName{...field: "old"...}` to substitute
// new for old on the field's string-literal value. Returns (newBody,
// rewriteCount, ok) — ok is false only on unparseable bodies.
//
// Match rules:
//   - Composite literal type is an *ast.Ident equal to typeName (bare or
//     &Type{}), or *ast.SelectorExpr whose Sel matches (pkg.Type{}).
//   - Key is *ast.Ident with Name == field.
//   - Value is *ast.BasicLit STRING whose UNQUOTED value matches old.
func retargetFieldInBody(body, typeName, field, old, new string) (string, int, bool) {
	fset := token.NewFileSet()
	// Try to parse as a full file first; fall back to wrapped expr.
	src := body
	wrapped := false
	if !strings.HasPrefix(strings.TrimLeftFunc(src, unicode.IsSpace), "package ") {
		// def bodies stored in the DB are single decls without package headers
		src = "package p\n" + body
		wrapped = true
	}
	file, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return body, 0, false
	}
	count := 0
	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if !compositeMatchesType(cl.Type, typeName) {
			return true
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			keyIdent, ok := kv.Key.(*ast.Ident)
			if !ok || keyIdent.Name != field {
				continue
			}
			lit, ok := kv.Value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			unquoted, err := strconv.Unquote(lit.Value)
			if err != nil || unquoted != old {
				continue
			}
			lit.Value = strconv.Quote(new)
			count++
		}
		return true
	})
	if count == 0 {
		return body, 0, true
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return body, 0, false
	}
	out := buf.String()
	if wrapped {
		out = strings.TrimPrefix(out, "package p\n")
		out = strings.TrimPrefix(out, "package p\n\n") // gofmt adds a blank line
	}
	return out, count, true
}

// compositeMatchesType reports whether a CompositeLit.Type expression
// names the target type — either bare Ident (Type{}) or SelectorExpr
// where the Sel matches (pkg.Type{}). Pointers are stripped upstream.
func compositeMatchesType(expr ast.Expr, typeName string) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == typeName
	case *ast.SelectorExpr:
		return t.Sel.Name == typeName
	case *ast.StarExpr:
		return compositeMatchesType(t.X, typeName)
	}
	return false
}

func (s *server) handleDelete(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}

	// #105 safe-delete: refuse when references remain unless caller
	// opts in via force:true. Prevents orphaning callers whose bodies
	// still name this def — a KB where deletes leave dangling
	// references is worse than one where you have to fix references
	// first. force:true preserves the pre-existing unsafe behavior.
	if !args.Force {
		callers, cerr := s.backend.GetCallers(d.ID)
		if cerr == nil && len(callers) > 0 {
			var names []string
			for i, c := range callers {
				if i >= 8 {
					names = append(names, fmt.Sprintf("… (%d more)", len(callers)-i))
					break
				}
				names = append(names, formatReceiver(c.Receiver)+c.Name)
			}
			return errResult(fmt.Errorf(
				"delete %q refused — %d caller(s) still reference this def: %s. "+
					"Rewrite or delete callers first, or pass force:true to delete anyway",
				args.Name, len(callers), strings.Join(names, ", ")))
		}
	}

	// Show what we're about to delete.
	recv := formatReceiver(d.Receiver)

	if err := s.backend.DeleteDefinition(d.ID); err != nil {
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
	deleteOpts := emit.Opts{AllowedRemovals: []string{qualified}}
	if d.SourceFile != "" {
		deleteOpts.GoimportsFiles = []string{d.SourceFile}
		deleteOpts.TouchedFiles = []string{d.SourceFile}
	}
	buildResult := s.autoEmitAndBuildWithOpts(deleteOpts)
	// #109 pass 2 (winze op-classification): skip autoResolve on delete.
	// DeleteDefinition already dropped every refs row where from_def=D
	// OR to_def=D (store.go:201), so both the def's own outgoing edges
	// and every caller's edge INTO D are gone. Caller bodies still name
	// D textually, but a full re-resolve would just walk those bodies,
	// fail to find D in the DB, and skip — no ref changes. Skipping
	// autoResolve removes the full-project ResolveModule walk on every
	// delete. Same autocommit + IDF-invalidate as the rename skip path.
	// force:true delete still applies (safe-delete's caller check gates
	// unforced deletes at zero-callers anyway).
	if err := s.autoCommit(); err != nil {
		fmt.Fprintf(os.Stderr, "defn: auto-commit failed (post-delete): %v\n", err)
	}
	if s.idf != nil {
		s.idf.Invalidate()
	}

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

	d, err := s.backend.GetDefinitionByName(args.OldName, "")
	if err != nil {
		return s.notFoundOrErr(args.OldName, err)
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
	if err := s.backend.RenameDefinition(originalID, args.NewName, newBody, newSig, exported); err != nil {
		return errResult(err)
	}

	// Update all callers' bodies that reference the old name. Also collect
	// each touched file so goimports can scope to just those (#109 pass 3):
	// rename touches the def's own file + every caller's file, typically a
	// small handful vs the whole project tree.
	callers, err := s.backend.GetCallers(originalID)
	if err != nil {
		return errResult(fmt.Errorf("get callers for rename: %w", err))
	}
	touchedFiles := map[string]bool{}
	if d.SourceFile != "" {
		touchedFiles[d.SourceFile] = true
	}
	updated := 0
	for _, caller := range callers {
		if strings.Contains(caller.Body, args.OldName) {
			var skipped int
			caller.Body, skipped = astRename(caller.Body, args.OldName, args.NewName)
			totalSkipped += skipped
			caller.Signature = extractSignature(caller.Body)
			if _, err := s.backend.UpsertDefinition(&caller); err != nil {
				return errResult(fmt.Errorf("update caller %s: %w", caller.Name, err))
			}
			if caller.SourceFile != "" {
				touchedFiles[caller.SourceFile] = true
			}
			updated++
		}
	}
	goimportsFiles := make([]string, 0, len(touchedFiles))
	for f := range touchedFiles {
		goimportsFiles = append(goimportsFiles, f)
	}

	// #148: rename is dispatch-safe by construction — refs are by def-ID,
	// no ID changes on rename, so the ref graph and interface satisfaction
	// are preserved regardless of build outcome. Skip the build gate;
	// this is the biggest single win of #148 (rename was 187ms wall on
	// winze with 148ms in go build; drops to ~40ms).
	buildResult := s.autoEmitOnlyWithOpts(emit.Opts{
		AllowedRemovals: []string{qualifiedOld},
		GoimportsFiles:  goimportsFiles,
		TouchedFiles:    goimportsFiles,
	})
	// #109: rename is a name-preserving semantic transform — every from_def
	// → to_def edge in the refs table is ID-based, and no def IDs change
	// on rename. Caller bodies were already rewritten via astRename so
	// their AST-shape matches, but the edge SET is identical. Interface
	// satisfaction is preserved because it's driven by types.Object
	// identities (also stable). Skipping autoResolve here removes the
	// full-module ResolveModule call that dominated a single-symbol
	// rename on winze (5,239 refs re-derived for one name change).
	// Still autocommit so the DB working set stays clean.
	if err := s.autoCommit(); err != nil {
		fmt.Fprintf(os.Stderr, "defn: auto-commit failed (post-rename): %v\n", err)
	}
	if s.idf != nil {
		s.idf.Invalidate()
	}

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

// handleTestByName runs `go test -run <pattern>` verbatim so the model
// can reproduce a bug from an issue that names the failing test directly.
// L11: agents that read the right code but never confirm the failure loop
// through the read graph without committing to a fix. Naming a test lets
// the model turn a hypothesis into an observation before writing.
func (s *server) handleTestByName(_ context.Context, _ *sdkmcp.CallToolRequest, pattern string) (*sdkmcp.CallToolResult, any, error) {
	if s.projectDir == "" {
		return errResult(fmt.Errorf("no project directory configured"))
	}
	if pattern == "" {
		return errResult(fmt.Errorf("test: pattern is empty"))
	}
	// Ensure files reflect any pending DB edits so the test sees them.
	if err := emit.Emit(s.backend, s.projectDir); err != nil {
		return errResult(fmt.Errorf("emit: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-run", pattern, "-count=1", "-v", "./...")
	cmd.Dir = s.projectDir
	out, err := cmd.CombinedOutput()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Running -run %q across ./...:\n\n", pattern))
	sb.WriteString(truncateTestOutput(string(out)))
	if err != nil {
		sb.WriteString("\nSOME TESTS FAILED")
	} else {
		sb.WriteString("\nALL TESTS PASSED")
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleTest(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}

	impact, err := s.backend.GetImpact(d.ID)
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
	if err := emit.Emit(s.backend, s.projectDir); err != nil {
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
	sb.WriteString(truncateTestOutput(string(out)))

	if err != nil {
		sb.WriteString("\nSOME TESTS FAILED")
	} else {
		sb.WriteString("\nALL TESTS PASSED")
	}

	return textResult(sb.String()), nil, nil
}

// testOutputCap is the byte threshold above which `test` op output is
// summarized rather than returned verbatim. Chosen to fit the interesting
// case (~1-2 failures + context) while cutting worst-case blowups (cli-3461
// paid ~30 KB for 10 test runs). Verbose subtest output on a large package
// can reach 100+ KB; we cap that hard.
const testOutputCap = 6000

// truncateTestOutput compresses `go test -v` output that exceeds the cap.
// Preserves head (first N lines — first failure's context), all `--- FAIL:`
// lines (which subtests broke), all package-level `FAIL`/`ok` lines (which
// packages ran), and tail (last N lines — summary). Emits a marker showing
// how many lines were dropped so the model can widen the search if needed.
func truncateTestOutput(out string) string {
	if len(out) <= testOutputCap {
		return out
	}
	lines := strings.Split(out, "\n")
	const headN, tailN = 40, 20
	if len(lines) <= headN+tailN {
		return out
	}
	head := lines[:headN]
	tail := lines[len(lines)-tailN:]

	// Collect failures and package-level results from the middle band.
	var failures, pkgResults []string
	seen := make(map[string]bool)
	for _, l := range lines[headN : len(lines)-tailN] {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "--- FAIL:"):
			if !seen[t] {
				failures = append(failures, l)
				seen[t] = true
			}
		case strings.HasPrefix(t, "FAIL\t"), strings.HasPrefix(t, "ok  \t"):
			pkgResults = append(pkgResults, l)
		}
	}

	var sb strings.Builder
	sb.WriteString(strings.Join(head, "\n"))
	sb.WriteString("\n")
	dropped := len(lines) - headN - tailN - len(failures) - len(pkgResults)
	if len(failures) > 0 {
		sb.WriteString(fmt.Sprintf("\n... [%d lines truncated; failed subtests below] ...\n", dropped))
		sb.WriteString(strings.Join(failures, "\n"))
		sb.WriteString("\n")
	} else {
		sb.WriteString(fmt.Sprintf("\n... [%d lines truncated; no failures in the middle] ...\n", dropped))
	}
	if len(pkgResults) > 0 {
		sb.WriteString("\n")
		sb.WriteString(strings.Join(pkgResults, "\n"))
		sb.WriteString("\n")
	}
	sb.WriteString("\n... [tail] ...\n")
	sb.WriteString(strings.Join(tail, "\n"))
	return sb.String()
}

// searchShapedSQLRedirects detects `query` op SQL that is really trying to
// do work the model should route through a first-class op. Returns a
// non-empty redirect message when the SQL matches a known anti-pattern
// (grepping bodies, direct name lookups, schema introspection), else "".
// The intercept exists because raw SQL for these shapes is a wire-cost
// disaster: the model burns turns re-discovering the schema and returns
// blob rows when a compact projection would do.
var (
	sqlBodyGrep    = regexp.MustCompile(`(?i)\bbody\s+LIKE\s+'`)
	sqlNameLookup  = regexp.MustCompile(`(?i)\b(?:d\.)?name\s*=\s*'`)
	sqlFileScoped  = regexp.MustCompile(`(?i)\b(?:d\.)?source_file\s*(?:LIKE\b|=|\bIN\b)`)
	sqlSchemaProbe = regexp.MustCompile(`(?i)^\s*(?:SHOW\s+(?:TABLES|DATABASES|COLUMNS)|DESCRIBE\s|DESC\s|EXPLAIN\s)`)
	sqlInfoSchema  = regexp.MustCompile(`(?i)\bINFORMATION_SCHEMA\b`)
)

func searchShapedSQLRedirect(sql string) string {
	switch {
	case sqlBodyGrep.MatchString(sql):
		return "raw SQL grep on definitions.bodies is a wire-cost anti-pattern — use `code(op:\"search\", pattern:\"<text>\")` instead; it returns compact name+file+line rows, not full bodies. If you truly need SQL analytics (e.g., counts, joins across tables), use `defn query` from the CLI."
	case sqlNameLookup.MatchString(sql):
		return "direct name lookup via SQL is a wire-cost anti-pattern — use `code(op:\"read\", name:\"<name>\")` for the body, `code(op:\"outline\", name:\"<name>\")` for the shape, or `code(op:\"impact\", name:\"<name>\")` for callers. All are cheaper on the wire than blob rows."
	case sqlFileScoped.MatchString(sql):
		return "file-scoped SQL against definitions is a wire-cost anti-pattern — use `code(op:\"file-defs\", file:\"<path>\")` to list all defs in a file, `code(op:\"read-file\", file:\"<path>\")` for all bodies in a file, `code(op:\"outline\", name:\"<name>\")` for a single def's shape, or `code(op:\"search\", pattern:\"<text>\")` for symbol/text search. These return compact rows tuned for LLM consumption; raw SQL dumps blobs."
	case sqlSchemaProbe.MatchString(sql) || sqlInfoSchema.MatchString(sql):
		return "schema introspection via SQL is unnecessary — the DB schema is documented at internal/store/schema.sql. Tables: definitions (name, kind, source_file, start_line, ...), bodies (def_id, body), refs, imports, modules, project_files. Use the graph ops (search/outline/read/impact) instead of raw SQL."
	}
	return ""
}

func (s *server) handleQuery(_ context.Context, _ *sdkmcp.CallToolRequest, args sqlParam) (*sdkmcp.CallToolResult, any, error) {
	if msg := searchShapedSQLRedirect(args.SQL); msg != "" {
		return errResult(fmt.Errorf("%s", msg))
	}
	results, err := s.backend.Query(args.SQL)
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
		n, err := ingest.IngestFile(s.backend, s.projectDir, filePath)
		if err != nil {
			return errResult(fmt.Errorf("ingest file: %w", err))
		}
		// Re-resolve refs for the affected package so structural changes
		// (added/removed embeds, signature changes, new defs) keep the
		// ref graph consistent. Without this, embed/implements/call refs
		// silently drift away from source over many sync calls.
		if err := resolve.ResolveFile(s.backend, s.projectDir, filePath); err != nil {
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
	locs, err := emit.EmitWithMap(s.backend, out)
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
	if err := s.backend.GC(); err != nil {
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
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}

	// #151 v2: MinHash-based body similarity, not sig-token overlap.
	// The prior sig-only implementation ("defs with the same param
	// types") missed the more useful question — "defs whose BODIES
	// do similar work." MinHash-32 approximates Jaccard of 5-char
	// body shingles; sub-linear-friendly (though we scan naively at
	// defn's scale — LSH later if needed).
	//
	// Falls back to the old sig-token search when the target def has
	// no body (kind=type/interface/const with no source text to hash)
	// or the summaries table is empty (upgrade race).
	summaries, err := s.backend.AllDefSummaryMinHashes()
	if err != nil || len(summaries) == 0 || len(d.Body) < 8 {
		return s.handleSimilarBySignature(d)
	}
	target, ok := summaries[d.ID]
	if !ok {
		// Not yet computed for this def; compute on the fly and
		// backfill for future queries.
		target = store.ComputeMinHash(d.Body)
		_ = s.backend.SetDefSummaryMinHash(d.ID, target)
	}

	type scored struct {
		id    int64
		score float64
	}
	scores := make([]scored, 0, len(summaries))
	for id, mh := range summaries {
		if id == d.ID {
			continue
		}
		if j := store.MinHashJaccard(target, mh); j > 0.15 {
			scores = append(scores, scored{id, j})
		}
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].score > scores[j].score })
	if len(scores) > 20 {
		scores = scores[:20]
	}
	if len(scores) == 0 {
		return textResult(fmt.Sprintf("No definitions structurally similar to %s (MinHash Jaccard > 0.15 on 5-char body shingles).", args.Name)), nil, nil
	}

	type match struct {
		Name       string  `json:"name"`
		Kind       string  `json:"kind"`
		Receiver   string  `json:"receiver,omitempty"`
		Signature  string  `json:"signature"`
		Similarity float64 `json:"similarity"`
	}
	matches := make([]match, 0, len(scores))
	for _, sc := range scores {
		def, err := s.backend.GetDefinition(sc.id)
		if err != nil || def == nil {
			continue
		}
		matches = append(matches, match{
			Name: def.Name, Kind: def.Kind, Receiver: def.Receiver,
			Signature: oneLineSignature(def.Signature), Similarity: sc.score,
		})
	}
	text, _ := toJSON(matches)
	return textResult(fmt.Sprintf("Definitions with similar bodies to %s (MinHash Jaccard, 5-char shingles):\n\n%s", args.Name, text)), nil, nil
}

// handleSimilarBySignature is the pre-#151 signature-token search,
// kept as a fallback for defs without body text (interface/type/const
// declarations) or when the summaries table is empty.
func (s *server) handleSimilarBySignature(d *store.Definition) (*sdkmcp.CallToolResult, any, error) {
	if d.Signature == "" {
		return errResult(fmt.Errorf("definition %q has no signature or body to compare on", d.Name))
	}
	sig := d.Signature
	if idx := strings.Index(sig, "("); idx >= 0 {
		sig = sig[idx:]
	}
	sigDefs, _ := s.backend.FindDefinitions("%" + sig + "%")
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
		return textResult(fmt.Sprintf("No definitions with similar signatures to %s", d.Name)), nil, nil
	}
	text, _ := toJSON(matches)
	return textResult(fmt.Sprintf("Definitions with similar signatures to %s (body-less fallback):\n\n%s", d.Name, text)), nil, nil
}

// projectOverview returns a compact module-level summary: package path,
// def count, first ~3 exported def names per module. Used by handleOverview
// when called with no file/name arg — orientation before the model
// commits to a subtree.
const projectOverviewModuleCap = 40
const projectOverviewDefsPerModule = 3

func (s *server) projectOverview() (*sdkmcp.CallToolResult, any, error) {
	mods, err := s.backend.ListModules()
	if err != nil {
		return errResult(fmt.Errorf("list modules: %w", err))
	}
	if len(mods) == 0 {
		return textResult("[project overview: no modules ingested — run defn ingest .]"), nil, nil
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Project overview (%d modules)\n\n", len(mods)))
	shown := 0
	for _, m := range mods {
		if shown >= projectOverviewModuleCap {
			sb.WriteString(fmt.Sprintf("… (%d more modules omitted — pass file:\"path/to/pkg\" for a subtree)\n", len(mods)-shown))
			break
		}
		defs, _ := s.backend.GetModuleDefinitions(m.ID)
		nExp := 0
		var exemplars []string
		for _, d := range defs {
			if !d.Exported || d.Test {
				continue
			}
			nExp++
			if len(exemplars) < projectOverviewDefsPerModule {
				exemplars = append(exemplars, formatReceiver(d.Receiver)+d.Name)
			}
		}
		sb.WriteString(fmt.Sprintf("- %s — %d defs (%d exported)", m.Path, len(defs), nExp))
		if len(exemplars) > 0 {
			sb.WriteString(fmt.Sprintf(" — %s", strings.Join(exemplars, ", ")))
		}
		sb.WriteString("\n")
		shown++
	}
	sb.WriteString("\nUse `op:\"overview\" file:\"<pkg-path>\"` to drill in, `op:\"search\" pattern:\"<term>\"` to jump to a def.\n")
	return textResult(sb.String()), nil, nil
}

func (s *server) handleOverview(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	file := args.File
	if file == "" {
		file = args.Name
	}
	if strings.TrimSpace(file) == "" {
		// L18: empty overview → project-wide module summary. The preamble
		// calls overview "the right first-touch" but the old impl errored
		// without a file; agents got a rejection instead of orientation.
		return s.projectOverview()
	}

	// Strip filename to get package directory.
	dir := file
	if idx := strings.LastIndex(dir, "/"); idx >= 0 {
		dir = dir[:idx]
	} else {
		dir = strings.TrimSuffix(dir, "_test.go")
		dir = strings.TrimSuffix(dir, ".go")
	}

	defs, err := s.backend.FindDefinitionsByFile(dir, file, 0)
	if err != nil || len(defs) == 0 {
		return errResult(fmt.Errorf("no definitions found for %s", file))
	}

	// #157 query-context: filter defs to those whose name/doc/
	// signature contains any query token. Empty result surfaces
	// as an error hint so the model can drop the query and retry.
	totalDefs := len(defs)
	var hiddenDefs int
	if q := strings.TrimSpace(args.Query); q != "" {
		if tokens := extractQueryTokensLower(q); len(tokens) > 0 {
			var kept []store.Definition
			for _, d := range defs {
				hay := strings.ToLower(d.Name + " " + d.Doc + " " + d.Signature)
				matched := false
				for _, t := range tokens {
					if strings.Contains(hay, t) {
						matched = true
						break
					}
				}
				if matched {
					kept = append(kept, d)
				} else {
					hiddenDefs++
				}
			}
			if len(kept) == 0 {
				return errResult(fmt.Errorf("overview: no defs in %s match query=%q (of %d total). Drop the query for the full listing.", file, args.Query, totalDefs))
			}
			defs = kept
		}
	}

	// Get full definitions with bodies to check relationships.
	var sb strings.Builder
	if hiddenDefs > 0 {
		sb.WriteString(fmt.Sprintf("## %s (%d of %d definitions, filtered by query=%q)\n\n", file, len(defs), totalDefs, args.Query))
	} else {
		sb.WriteString(fmt.Sprintf("## %s (%d definitions)\n\n", file, len(defs)))
	}

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
			full, err := s.backend.GetDefinition(d.ID)
			if err != nil {
				sb.WriteString("\n")
				continue
			}
			callers, _ := s.backend.GetCallers(full.ID)
			callees, _ := s.backend.GetCallees(full.ID)
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

	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}

	if !strings.Contains(d.Body, args.OldName) {
		return errResult(fmt.Errorf("old text not found in %s body", args.Name))
	}

	d.Body = strings.Replace(d.Body, args.OldName, args.NewName, 1)
	d.Signature = extractSignature(d.Body)

	if _, err := s.backend.UpsertDefinition(d); err != nil {
		return errResult(err)
	}

	buildResult := s.autoEmitAndBuildForFile(d.SourceFile)
	s.autoResolveFile(d.SourceFile, s.modulePath(d.ModuleID))

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

	defs, err := s.backend.FindDefinitionsByFile(dir, args.File, args.Line)
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
// `s.backend.FindDefinitionsByFile` — that's the single-source data layer;
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
	defs, err := s.backend.FindDefinitionsByFile(dir, file, 0)
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
	bodies, err := s.backend.GetBodiesByDefIDs(ids)
	if err != nil {
		return errResult(fmt.Errorf("read-file: fetch bodies: %w", err))
	}

	// Look up module path once (all defs in this file share it).
	var modulePath string
	mods, _ := s.backend.ListModules()
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
	if !args.Full && len(out) > readFileCapBytes {
		out = compactReadFile(file, modulePath, defs, len(out))
	}
	return withUsage(textResult(out), usageStats{
		Op:            "read-file",
		BytesReturned: len(out),
	}), nil, nil
}

// readFileCapBytes is the size ceiling above which read-file downgrades to
// a signatures-only projection. 8000 was picked from the head-to-head-go
// bench: files above this size are almost always exploratory browsing, not
// preparation to edit. Model can bypass with `full:true` or fetch specific
// bodies with `read name:"..."`.
const readFileCapBytes = 8000

func compactReadFile(file, modulePath string, defs []store.Definition, fullSize int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s (%d definitions", file, len(defs)))
	if modulePath != "" {
		sb.WriteString(", module ")
		sb.WriteString(modulePath)
	}
	sb.WriteString(") [signatures only — file exceeds cap]\n\n")
	for _, d := range defs {
		recv := formatReceiver(d.Receiver)
		sig := d.Signature
		if sig == "" {
			sig = "(sig unavailable)"
		}
		sb.WriteString(fmt.Sprintf("- %s%s (%s) L%d-%d — %s\n", recv, d.Name, d.Kind, d.StartLine, d.EndLine, sig))
	}
	sb.WriteString(fmt.Sprintf(
		"\n[read-file capped: full response would be %d bytes; showing signatures only. Fetch individual bodies with `code(op:\"read\", name:\"<Name>\")`, or pass `full:true` to bypass the cap.]\n",
		fullSize,
	))
	return sb.String()
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
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
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
	mods, _ := s.backend.ListModules()
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
		impact, err := s.backend.GetImpact(d.ID)
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
	defs, err := s.backend.FindDefinitionsByFile(dir, file, 0)
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
	result, err := s.backend.Simulate(args.Mutations)
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
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found: %w", args.Name, err))
	}

	maxDepth := args.Depth
	if maxDepth <= 0 {
		maxDepth = 10
	}

	results, err := s.backend.Traverse(d.ID, args.Direction, args.RefKinds, maxDepth)
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
	fields, err := s.backend.QueryLiteralFields(typeName, args.Name, args.Body, nil, 200)
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
	comments, err := s.backend.GetCommentsByPragma(pragmaKey)
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

		d, err := s.backend.GetDefinitionByName(m.Name, "")
		if err != nil {
			cr.Error = fmt.Sprintf("definition %q not found", m.Name)
			results = append(results, cr)
			continue
		}

		impact, err := s.backend.GetImpact(d.ID)
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
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}
	impact, err := s.backend.GetImpact(d.ID)
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

// transitiveTestsByIDs filters `ids` (typically the output of a
// reachability BFS) to those that are test defs. One bulk SELECT via
// the ad-hoc Query surface — no per-id round trips. Safe against SQL
// injection because IDs are int64 and formatted directly. #154.
func (s *server) transitiveTestsByIDs(ids []int64) ([]store.Definition, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", id)
	}
	sql := fmt.Sprintf(
		`SELECT id, name, kind, exported, test, COALESCE(receiver,'') as receiver
		 FROM definitions WHERE test = 1 AND id IN (%s)`, b.String())
	rows, err := s.backend.Query(sql)
	if err != nil {
		return nil, err
	}
	out := make([]store.Definition, 0, len(rows))
	for _, row := range rows {
		d := store.Definition{Test: true}
		if v, ok := row["id"].(int64); ok {
			d.ID = v
		}
		if v, ok := row["name"].(string); ok {
			d.Name = v
		}
		if v, ok := row["kind"].(string); ok {
			d.Kind = v
		}
		if v, ok := row["receiver"].(string); ok {
			d.Receiver = v
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *server) handleBatchImpact(ctx context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	names := args.Names
	if len(names) == 0 && args.Name != "" {
		names = []string{args.Name}
	}
	if len(names) == 0 {
		return errResult(fmt.Errorf("batch-impact: names is required"))
	}

	// #154 fast path: use the in-memory reverse-refs cache for
	// transitive counts. Prior code did N × GetImpact (each N × 46ms
	// on winze via recursive CTE); with the cache, one rebuild scan
	// + N in-memory BFSes. For 10 names on winze: ~460ms → ~15ms.
	// Direct callers + tests still come from backend queries — they
	// need name/receiver/test formatting the raw cache can't give.
	allCallers := map[string]bool{}
	allTests := map[string]bool{}
	var perDef []map[string]any

	for _, name := range names {
		d, err := s.backend.GetDefinitionByName(name, "")
		if err != nil {
			perDef = append(perDef, map[string]any{"name": name, "error": "not found"})
			continue
		}
		directCallers, err := s.backend.GetCallers(d.ID)
		if err != nil {
			perDef = append(perDef, map[string]any{"name": name, "error": err.Error()})
			continue
		}
		// Transitive via in-memory BFS if cache is warm; else
		// fall back to backend's GetImpact (CTE path).
		var transCount int
		var tests []store.Definition
		if s.reach != nil {
			if reach, ok := s.reach.reachableCallers(ctx, s.backend, d.ID); ok {
				transCount = len(reach)
				// Collect tests via the direct-callers list plus
				// backend lookup for transitive test-defs. For
				// batch-impact we only need the count + names —
				// direct+transitive test set via one query.
				for _, c := range directCallers {
					if c.Test {
						tests = append(tests, c)
					}
				}
				// Add transitive-only tests via one bulk lookup.
				if len(reach) > 0 {
					testDefs, _ := s.transitiveTestsByIDs(reach)
					tests = append(tests, testDefs...)
				}
			}
		}
		if transCount == 0 && len(tests) == 0 {
			// Cache miss or unpopulated — fall back to CTE.
			impact, err := s.backend.GetImpact(d.ID)
			if err != nil {
				perDef = append(perDef, map[string]any{"name": name, "error": err.Error()})
				continue
			}
			transCount = impact.TransitiveCount
			tests = impact.Tests
		}
		for _, c := range directCallers {
			allCallers[formatReceiver(c.Receiver)+c.Name] = true
		}
		for _, t := range tests {
			allTests[t.Name] = true
		}
		perDef = append(perDef, map[string]any{
			"name":               formatReceiver(d.Receiver) + d.Name,
			"direct_callers":     len(directCallers),
			"transitive_callers": transCount,
			"tests":              len(tests),
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

// outlineCalleeCap and outlineFlowCap bound the caller/flow lists in
// outline output. Bench trajectories showed some outlines pushing 7 kB
// entirely from unbounded callee lists; head-of-list is enough for the
// model to orient, and the total count is still reported.
const (
	outlineCalleeCap = 15
	outlineFlowCap   = 20
	// impactCallerCap bounds the markdown caller list in handleImpact.
	// Model rarely acts on more than the top 10-15; full list is still
	// available via format:"json" for the rare deep-analysis case.
	impactCallerCap = 15
)

// truncateList returns "a, b, c, … (N more)" when the list exceeds cap,
// else the full comma-joined string. Preserves the count in the summary
// so the model knows there's more if it needs to `read` for the full body.
func truncateList(names []string, cap int) string {
	if len(names) <= cap {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:cap], ", ") + fmt.Sprintf(", … (%d more)", len(names)-cap)
}

// truncateFlow is truncateList's " → " variant for the top-level flow
// summary (control-flow tokens joined by arrows, not commas).
func truncateFlow(flow []string, cap int) string {
	if len(flow) <= cap {
		return strings.Join(flow, " → ")
	}
	return strings.Join(flow[:cap], " → ") + fmt.Sprintf(" → … (%d more)", len(flow)-cap)
}

// handleMethods returns a compact projection of every method whose
// receiver is (or points to) the given type. Task #79: browsing a
// type's method set is one of the most common exploration patterns
// (agents ask "what can I do with this thing?") and the alternatives
// today are all bad: `read` on every method (N×full-body tokens),
// `overview` on the file (mixes methods with unrelated defs and
// includes bodies), or grep (misses interface dispatch, no signatures).
//
// Response shape: header line ("TypeName — N methods"), exported
// methods grouped first (alphabetical), then unexported, each on one
// line as `Method(args) return  // first-line doc`. Ends with a
// pointer at `code(op:"read")` for full body access.
//
// Also handles interfaces by parsing the interface body's inline
// method declarations — those live in the type body, not as separate
// method rows.
func (s *server) handleMethods(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return errResult(fmt.Errorf("methods: name is required (a type or interface name)"))
	}
	// Strip leading '*' — callers often paste "*Mux" from a receiver.
	name = strings.TrimPrefix(name, "*")

	// Interface path: methods live inline in the interface body, not
	// as separate method rows. If we find a type/interface def by
	// this name and its kind is 'interface', parse its body.
	if typeDef, err := s.backend.GetDefinitionByName(name, ""); err == nil && typeDef != nil && typeDef.Kind == "interface" {
		return s.methodsFromInterfaceBody(typeDef)
	}

	// Type path: scan all methods, keep those whose receiver matches.
	// Handles pointer receivers (*T), value receivers (T), and
	// generic receivers (T[X], *T[X]) — we compare against T after
	// stripping the pointer prefix and generic bracket suffix.
	allMethods, err := s.backend.FilterDefinitions("", "method", "", 0)
	if err != nil {
		return errResult(fmt.Errorf("methods: list: %w", err))
	}
	var mine []store.Definition
	for _, m := range allMethods {
		recv := strings.TrimPrefix(m.Receiver, "*")
		if idx := strings.Index(recv, "["); idx > 0 {
			recv = recv[:idx]
		}
		if recv == name {
			mine = append(mine, m)
		}
	}
	if len(mine) == 0 {
		return errResult(fmt.Errorf("methods: no methods found for type %q (check spelling, or try code(op:\"search\", pattern:%q))", name, name))
	}

	// #157 query-context: filter methods by name+doc substring.
	if strings.TrimSpace(args.Query) != "" {
		if tokens := extractQueryTokensLower(args.Query); len(tokens) > 0 {
			mine = filterMethodsByQuery(mine, tokens)
			if len(mine) == 0 {
				return errResult(fmt.Errorf("methods: no methods on %q match query=%q (try dropping the query for the full set)", name, args.Query))
			}
		}
	}

	return s.formatMethodList(name, "type", mine, args.Query)
}

// filterMethodsByQuery keeps only methods whose name or doc contains
// any query token. Case-insensitive substring match. #157.
func filterMethodsByQuery(methods []store.Definition, tokens []string) []store.Definition {
	var out []store.Definition
	for _, m := range methods {
		hay := strings.ToLower(m.Name + " " + m.Doc + " " + m.Signature)
		for _, t := range tokens {
			if strings.Contains(hay, t) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// methodsFromInterfaceBody handles the interface case: parse the
// interface's stored body, extract each method signature + preceding
// doc comment, format compactly.
func (s *server) methodsFromInterfaceBody(d *store.Definition) (*sdkmcp.CallToolResult, any, error) {
	src := "package x\n" + d.Body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil || len(f.Decls) == 0 {
		return errResult(fmt.Errorf("methods: interface %q body did not parse: %v", d.Name, err))
	}
	gen, ok := f.Decls[0].(*ast.GenDecl)
	if !ok || len(gen.Specs) == 0 {
		return errResult(fmt.Errorf("methods: interface %q: unexpected decl shape", d.Name))
	}
	ts, ok := gen.Specs[0].(*ast.TypeSpec)
	if !ok {
		return errResult(fmt.Errorf("methods: interface %q: type spec missing", d.Name))
	}
	iface, ok := ts.Type.(*ast.InterfaceType)
	if !ok {
		return errResult(fmt.Errorf("methods: %q is not an interface (kind=%s)", d.Name, d.Kind))
	}
	var out []store.Definition
	for _, field := range iface.Methods.List {
		if len(field.Names) == 0 {
			continue // embedded interface — skip, list as "embeds" in header if we wanted
		}
		for _, ident := range field.Names {
			sig := "func " + ident.Name + types.ExprString(field.Type)[len("func"):] // "func(x int) error" — trim leading "func"
			doc := ""
			if field.Doc != nil {
				doc = strings.TrimSpace(field.Doc.Text())
			}
			out = append(out, store.Definition{
				Name:      ident.Name,
				Kind:      "method",
				Exported:  len(ident.Name) > 0 && ident.Name[0] >= 'A' && ident.Name[0] <= 'Z',
				Signature: sig,
				Doc:       doc,
			})
		}
	}
	if len(out) == 0 {
		return errResult(fmt.Errorf("methods: interface %q has no method declarations", d.Name))
	}
	return s.formatMethodList(d.Name, "interface", out, "")
}

// formatMethodList renders a method set as compact text: exported
// group first, then unexported, one line each with signature + first
// line of doc.
func (s *server) formatMethodList(typeName, kind string, methods []store.Definition, query string) (*sdkmcp.CallToolResult, any, error) {
	sort.Slice(methods, func(i, j int) bool {
		if methods[i].Exported != methods[j].Exported {
			return methods[i].Exported // exported first
		}
		return methods[i].Name < methods[j].Name
	})
	var exp, unexp int
	for _, m := range methods {
		if m.Exported {
			exp++
		} else {
			unexp++
		}
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s (%s) — %d method", typeName, kind, len(methods)))
	if len(methods) != 1 {
		sb.WriteString("s")
	}
	if exp > 0 && unexp > 0 {
		sb.WriteString(fmt.Sprintf(" (%d exported, %d unexported)", exp, unexp))
	}
	if query != "" {
		sb.WriteString(fmt.Sprintf(" [query=%q]", query))
	}
	sb.WriteString("\n\n")

	var lastGroup string
	for _, m := range methods {
		group := "Unexported"
		if m.Exported {
			group = "Exported"
		}
		// Only emit group headers when both groups present.
		if exp > 0 && unexp > 0 && group != lastGroup {
			if lastGroup != "" {
				sb.WriteString("\n")
			}
			sb.WriteString(group + ":\n")
			lastGroup = group
		}
		sig := oneLineSignature(m.Signature)
		if sig == "" {
			sig = m.Name + "(…)"
		}
		sb.WriteString("  ")
		sb.WriteString(sig)
		if doc := firstDocLine(m.Doc); doc != "" {
			sb.WriteString("  // ")
			sb.WriteString(doc)
		}
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("\nFetch a full body: code(op:\"read\", name:\"%s.MethodName\")\n", typeName))

	out := sb.String()
	return textResult(out), nil, nil
}

// oneLineSignature collapses a multi-line signature (params split
// across lines, doc-prefixed) to a single line for the methods
// listing. Strips leading doc-comment prefixes and joins wrapped
// param lists back into one line.
func oneLineSignature(sig string) string {
	// Skip leading `// ...` doc lines; take the first non-doc line
	// and collapse continuation whitespace.
	lines := strings.Split(sig, "\n")
	var out []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "//") {
			continue
		}
		out = append(out, t)
	}
	joined := strings.Join(out, " ")
	// Collapse runs of whitespace.
	fields := strings.Fields(joined)
	return strings.Join(fields, " ")
}

func firstDocLine(doc string) string {
	for _, ln := range strings.Split(doc, "\n") {
		t := strings.TrimSpace(ln)
		t = strings.TrimPrefix(t, "//")
		t = strings.TrimSpace(t)
		if t != "" {
			// Cap length so a novella doc doesn't blow up the listing.
			if len(t) > 100 {
				t = t[:100] + "…"
			}
			return t
		}
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
	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}

	// Size-aware fallback: for tiny bodies, read is smaller than
	// outline. Route to the read handler transparently.
	if len(d.Body) < outlineBodyThreshold {
		return s.handleGetDefinition(nil, req, args)
	}

	var modulePath string
	mods, _ := s.backend.ListModules()
	for _, m := range mods {
		if m.ID == d.ModuleID {
			modulePath = m.Path
			break
		}
	}

	callers, _ := s.backend.GetCallers(d.ID)
	callees, _ := s.backend.GetCallees(d.ID)

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

	// #157 query-context: narrow callees to those matching any
	// query token. Overall count preserved via "(N total)".
	filteredCallees := callees
	var hiddenCallees int
	if q := strings.TrimSpace(args.Query); q != "" {
		if tokens := extractQueryTokensLower(q); len(tokens) > 0 {
			filteredCallees, hiddenCallees = filterCallersByQuery(callees, tokens)
		}
	}
	if len(filteredCallees) > 0 {
		names := make([]string, 0, len(filteredCallees))
		for _, c := range filteredCallees {
			names = append(names, formatReceiver(c.Receiver)+c.Name)
		}
		sort.Strings(names)
		hdr := fmt.Sprintf("Callees (%d)", len(filteredCallees))
		if hiddenCallees > 0 {
			hdr = fmt.Sprintf("Callees (%d of %d, filtered by query=%q)", len(filteredCallees), len(callees), args.Query)
		}
		sb.WriteString(fmt.Sprintf("%s: %s\n", hdr, truncateList(names, outlineCalleeCap)))
	} else if hiddenCallees > 0 {
		sb.WriteString(fmt.Sprintf("Callees: 0 matching query=%q (%d hidden)\n", args.Query, hiddenCallees))
	} else {
		sb.WriteString("Callees: 0\n")
	}

	if flow := topLevelFlow(d.Body); len(flow) > 0 {
		sb.WriteString(fmt.Sprintf("Flow (%d): %s\n", len(flow), truncateFlow(flow, outlineFlowCap)))
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

	d, err := s.backend.GetDefinitionByName(args.Name, "")
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
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
func (s *server) handleInsertPrecondition(_ context.Context, req *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
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
	d, err := s.backend.GetDefinitionByName(name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", name))
	}
	newBody, err := projection.InsertPrecondition(d.Body, args.Condition, args.Ret)
	if err != nil {
		return errResult(err)
	}
	snippet := fmt.Sprintf("if %s {\n\t%s\n}", args.Condition, args.Ret)
	return s.applyEditTerse(sessionOf(req), name, "inserted precondition at entry", snippet, newBody)
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
		all, err := s.backend.DistinctSourceFiles()
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
	defs, err := s.backend.FindDefinitionsByFile(dir, file, 0)
	if err != nil {
		return errResult(fmt.Errorf("add-import: locate file: %w", err))
	}
	if len(defs) == 0 {
		return errResult(fmt.Errorf("add-import: no definitions found in file %q — cannot resolve module", file))
	}
	moduleID := defs[0].ModuleID
	existing, err := s.backend.GetImports(moduleID)
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
	if err := s.backend.SetImports(moduleID, updated); err != nil {
		return errResult(fmt.Errorf("add-import: set imports: %w", err))
	}
	// #148: adding an import is dispatch-safe by construction — skip build.
	buildResult := s.autoEmitOnly(file)
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
func (s *server) handleRenameParam(_ context.Context, req *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
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
	d, err := s.backend.GetDefinitionByName(name, "")
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
	return s.applyEditTerse(sessionOf(req), name, action, snippet, newBody)
}

// handleWrapInDefer inserts a `defer <defer_body>` statement immediately
// before the Nth (1-based) top-level statement in the definition's body.
// Byte-exact PUTGET — see [[project_putget_edit_vocab_design]].
//
// If args.Name is empty, tries to infer the target: if the DB has exactly
// one non-test function, uses it; otherwise errors with the candidate list.
func (s *server) handleWrapInDefer(_ context.Context, req *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
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
	d, err := s.backend.GetDefinitionByName(name, "")
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
	return s.applyEditTerse(sessionOf(req), name, action, snippet, newBody)
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
func (s *server) handleReplaceSlice(_ context.Context, req *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
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
	d, err := s.backend.GetDefinitionByName(name, "")
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
	return s.applyEditTerse(sessionOf(req), name, action, args.New, newBody)
}

// handleReplaceHunk replaces a byte-exact occurrence of `old` inside
// the target definition's body with `new`. Content-addressed hunk edit
// — the write-side analog of files-mode str_replace but scoped to a
// single def (name provides the file-level disambiguation, so `old`
// need not carry padding context).
//
// If `old` occurs exactly once in the body, `index` may be 0. If it
// occurs more than once, the caller must pass a 1-based `index`. See
// internal/projection.ReplaceHunk for the pure function and PUTGET
// contract.
//
// If args.Name is empty, tries to infer the target: if the DB has
// exactly one non-test function, uses it; otherwise errors with the
// candidate list.
func (s *server) handleReplaceHunk(_ context.Context, req *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.Old) == "" {
		return errResult(fmt.Errorf("replace-hunk: old is required"))
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		inferred, err := s.inferSingleTargetName()
		if err != nil {
			return errResult(fmt.Errorf("replace-hunk: %w", err))
		}
		name = inferred
	}
	d, err := s.backend.GetDefinitionByName(name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", name))
	}
	newBody, err := projection.ReplaceHunk(d.Body, args.Old, args.New, args.Index)
	if err != nil {
		return errResult(err)
	}
	action := "replaced hunk"
	if args.Index > 0 {
		action = fmt.Sprintf("replaced hunk #%d", args.Index)
	}
	return s.applyEditTerse(sessionOf(req), name, action, args.New, newBody)
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
func (s *server) applyEditTerse(session *sdkmcp.ServerSession, name, action, snippet, newBody string) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.backend.GetDefinitionByName(name, "")
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", name))
	}
	src := "package x\n" + newBody
	if _, parseErr := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments); parseErr != nil {
		return errResult(fmt.Errorf("new_body has syntax error: %v", parseErr))
	}
	d.Body = newBody
	d.Signature = extractSignature(newBody)
	if _, err := s.backend.UpsertDefinition(d); err != nil {
		return errResult(err)
	}
	// #148: projection ops (all callers of applyEditTerse) are AST-
	// guaranteed sig-stable — skip the go-build gate to actually deliver
	// the "faster than native because the index is maintained" thesis.
	// Set DEFN_STRICT_BUILD=1 to force the old per-mutation build.
	buildResult := s.autoEmitOnly(d.SourceFile)
	s.autoResolveFile(d.SourceFile, s.modulePath(d.ModuleID))

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
	// #158: nudge apply-batching after N serial mutations to one file.
	// hint returns "" when session is nil (Measure* paths) or under threshold.
	if s.hint != nil {
		sb.WriteString(s.hint.note(session, d.SourceFile))
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
	defs, err := s.backend.FilterDefinitions("", "", "", 0)
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
