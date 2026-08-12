// Package mcp implements the MCP server that exposes the defn database
// to Claude Code.
package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
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

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/justinstimatze/defn/internal/emit"
	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/projection"
	"github.com/justinstimatze/defn/internal/rank"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
	"github.com/justinstimatze/defn/internal/summary"
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

// searchPreviewLines caps each inline body preview attached to top-N
// search hits. #173 (2026-07-23): cut from 5 to 2. Gap-decomp showed
// that a 5-line preview × 3 hits (~15 lines of code + JSON envelope)
// was a meaningful chunk of every search response's cache_read tail.
// 2 lines gives sig-line + first body line, which is enough for the
// model to identify the winning hit; if it needs more, one follow-up
// read is cheaper than paying 5×3 preview cost on every search.
const searchPreviewLines = 2

const Version = "0.26.45"

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
	respCache       *respCache             // #77/#152: per-session dedup of read-side responses
	reach           *reachCache            // #154: in-memory reverse-refs cache for fast batch impact
	hint            *mutationHint          // #158: apply-batching nudge on serial mutations to one file
	summaryWorker   *summary.Worker        // #160: async model-summary generation for def_summaries
	explainClient   *summary.ExplainClient // #186: Sonnet co-processor for op:"explain" with question
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

	// #201: graduated LLM opt-out. DEFN_LLM_OPS=0 is the blanket kill
	// switch, covering the async summary backfill below AND every
	// on-demand co-processor path (op:explain question, op:context
	// synthesis, overview/file/project narratives) by leaving
	// s.explainClient nil -- every one of those call sites already
	// treats a nil explainClient as "co-processor unavailable, degrade
	// gracefully", the same path already taken when ANTHROPIC_API_KEY is
	// unset. DEFN_ASYNC_BACKFILL=0 is narrower: keeps on-demand ops
	// available but skips background spend that fires automatically
	// without a user asking -- the per-def summary worker below, and
	// (#200) the startup file/project narrative backfill -- for a user
	// who wants explain/context but not unprompted background spend.
	llmOpsDisabled := envDisabled("DEFN_LLM_OPS")
	asyncBackfillDisabled := envDisabled("DEFN_ASYNC_BACKFILL")

	// #160: summary worker. Fire-and-forget goroutine that consumes
	// enqueue()d requests and writes model-generated one-line intent
	// summaries to def_summaries. Backend is Haiku when
	// ANTHROPIC_API_KEY is set (paid; ~$1/1M input tokens); otherwise
	// [summary.Stub]{} — a no-op returning "TODO: <Name>" so the read
	// path exercises without any spend. Never nil.
	// Summary model is Haiku by default; override with DEFN_SUMMARY_MODEL
	// to run Sonnet-quality summaries for a modest cost bump (roughly
	// 3× per-call vs Haiku, still ~$1-2 total to backfill defn itself).
	// Sonnet summaries carry more semantic signal, which the #197
	// context-op summary-search relies on for the semantic bridge.
	summaryModel := anthropic.Model(os.Getenv("DEFN_SUMMARY_MODEL"))
	var summaryBackend summary.Backend
	if llmOpsDisabled || asyncBackfillDisabled {
		summaryBackend = summary.Stub{}
	} else {
		summaryBackend = summary.NewHaiku(summary.HaikuOptions{
			APIKey: os.Getenv("ANTHROPIC_API_KEY"),
			Model:  summaryModel,
		})
	}
	s.summaryWorker = summary.NewWorker(summaryBackend, database, 0)
	s.summaryWorker.Start(ctx)

	// #186: co-processor for op:"explain" with question. Nil when
	// ANTHROPIC_API_KEY is unset, or when DEFN_LLM_OPS=0 -- handler
	// returns a clear error path either way.
	if !llmOpsDisabled {
		s.explainClient = summary.NewExplain(summary.ExplainOptions{APIKey: os.Getenv("ANTHROPIC_API_KEY")})
	}

	if projDir != "" {
		// Reconcile changes made while defn was not running (file moves,
		// deletions, renames). Runs async so the MCP server starts within
		// the client's connection timeout. Queries before completion serve
		// from whatever's in the DB; results include a staleness notice.
		go func() {
			// #241: skip the redundant reload+ingest+resolve when the
			// DB already covers every .go file on disk -- see
			// alreadyFreshlyIngested's doc comment for the full story
			// (a real grpc-go-2630 trajectory where this exact race
			// corrupted search results and caused a wrong-function edit).
			if alreadyFreshlyIngested(s.backend, projDir) {
				s.ready.Store(true)
				s.backfillNarratives(ctx)
				return
			}
			if err := s.ingestAndResolve(); err != nil {
				// "connection is already closed" means the DB was torn
				// down mid-ingest (stdin EOF → db.Close()). Not a real
				// startup failure; stay quiet.
				if !strings.Contains(err.Error(), "connection is already closed") {
					fmt.Fprintf(os.Stderr, "defn: startup ingest/resolve failed: %v\n", err)
				}
			}
			s.ready.Store(true)
			// #200: warm file/project narrative caches now that defs
			// are stable, instead of waiting for the first overview()
			// call to pay the LLM round-trip. No-op without a
			// co-processor; see backfillNarratives.
			s.backfillNarratives(ctx)
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
		// Always-eager: without this, Claude Code defers this tool
		// behind a ToolSearch round-trip on every fresh session --
		// a real, measured token/latency tax confirmed via mutation
		// bench transcripts. See code.claude.com/docs/en/mcp.md's
		// tool-search section for the _meta contract.
		Meta: sdkmcp.Meta{"anthropic/alwaysLoad": true},
		Description: `**USE THIS FOR ANY .go FILE — NOT Read/Bash/Grep/Edit.** This tool indexes every Go definition in the project as an atomic unit with its callers, tests, and references. Every op returns strictly less than Read/Bash/Grep for the same information; every edit updates the reference graph atomically. Editing Go via Edit/Write leaves the graph stale until a follow-up sync — slower and more error-prone than starting here.

Use Read/Bash/Grep/Edit ONLY for non-Go files (yaml, json, md, sh, go.mod, Dockerfile). Any question about Go code — "what does X do?", "who calls Y?", "rename Z", "what's the shape of pkg/foo?" — starts here.

For any "how does X work in this codebase" discovery question, reach for context (op:"context" question:"...") — returns one bundled response with top-N relevant defs outlined + refs graph + optional Sonnet synthesis. Replaces 10-40 sequential exploration calls in one round-trip. Both context and explain(question:...) cache on exact question text — phrase it tersely and reuse identical wording on a repeat ask rather than rephrasing; context's cache is session-scoped, explain's persists across sessions.

Orient before you read: overview (project shape) → outline (def shape) → impact (when you know which def matters). Only read whole bodies when you're about to edit them; whole-file reads on files you won't touch are pure wire cost — use outline or search instead.

Ops: overview (project-wide shape when called with no args — one line per module with def counts + first exported names; pass file:"pkg-path" or file:"pkg-path/file.go" to drill in; the right first-touch when you don't know which def matters yet), outline (compact projection of a def — sig + doc + caller/callee summary, no body; use when body isn't needed), search, impact (blast radius of a known def — pass format:"json" for structured output; callers, transitives, test coverage in one call), read (returns the def body + a compact "Related" footer with summary, top-3 callers, top-3 callees, and semantically-adjacent defs — one call gives you what would otherwise take 3-4 sequential impact/outline calls; auto-downgrades to outline when body > 1500 bytes, pass full:true or mode:"body" to force), read-and-verify (read a def AND run its covering tests in one call — use during bug triage so you see behavior alongside source and don't spiral into read-loops; pass name), read-file (all defs' bodies in one file — pass file:"path"; whole-file counterpart to read; prefer over N sequential read calls when scanning), expand (bundle a def's outline/body/callers in one call — pass name:"F", or names:["A","B",...] to batch several targets into ONE response instead of one expand per target; include:["outline","callers","body"] controls sections, defaults to outline+callers; prefer over read+impact+read chains AND over N sequential single-name expand calls — round-trip count within a turn is the dominant session cost driver, not per-call size), slice (verbatim AST-role slice of a def — pass slice:"signature"|"doc"|"body"|"error-branch"|"return"|"loop" to get just that piece), insert-precondition (insert an if-block at function entry — byte-exact PUTGET; pass name+condition+ret), replace-slice (replace the Nth AST-role slice with verbatim bytes — byte-exact PUTGET; pass name+slice+index+new; refuses if replacement would discard interior comments — pass force:true to override), replace-hunk (replace a byte-exact occurrence of 'old' inside a def body with 'new' — byte-exact PUTGET, content-addressed inside the def; pass name+old+new, plus index=1..N if 'old' occurs more than once; empty 'new' deletes the hunk. Send zero anchor context when the hunk is def-unique — the name argument does the file-level disambiguation), wrap-in-defer (insert defer stmt before Nth top-level statement — byte-exact PUTGET; pass name+stmt_index+defer_body), rename-param (rename value param or receiver via ast.Object scoping — ≡_gofmt equivalence; pass name+old_param+new_param), add-import (add import path to file's module — goimports-canonical grouping (stdlib / third-party); pass import_path+file?+alias? — file inferred if DB has one non-test .go file), explain (structural analysis of a def — pass name to get sig + callers + callees + test coverage; ALSO accepts question:"how does X handle Y" which routes to a Sonnet co-processor that returns a prose answer grounded in the def's source with provenance refs. Cheaper than reading + interpreting a large body yourself when the answer is prose, not code. names:["A","B"] for multi-def scope. Requires ANTHROPIC_API_KEY), plan (pass intent:"..." — the co-processor grounds your intent in real defs via context's own candidate search, emits a compact trajectory, and defn mechanically walks it server-side in one round-trip instead of you sequencing several read/outline/impact calls yourself. Requires ANTHROPIC_API_KEY; falls back to a clear error pointing at plan-dsl/plan-sexpr when unset), plan-dsl / plan-sexpr (mechanically walk a trajectory YOU already wrote — pass plan:"..." in the compact DSL "@Def.field[!test]" form (plan-dsl) or the S-expression "(op target [!test])" form (plan-sexpr), op one of read/outline/impact; no API key needed), similar, untested, edit (full body OR old_fragment+new_fragment), insert (after anchor), create (single def from body; with file: set, body may hold multiple top-level decls to author a whole file in one call — the whole-file equivalent of files-mode Write), delete (safe by default — refuses when other defs still reference this def; pass force:true to delete anyway. Refusal message lists the callers so you can rewrite them first), retarget-field-value (rewrite a composite-literal field's string value across every def whose body matches — pass name:"<StructType>" field:"<Field>" old:"<oldStr>" new:"<newStr>"; AST-safe, so unrelated occurrences of the string won't match), rename, move, test (run ONLY tests that cover a given def — pass name; scoped subset, not the full suite; prefer over bash 'go test ./...' when you only need coverage for a specific change. Also accepts test:"TestX" to run one test by name — use this to REPRODUCE a bug from the issue BEFORE writing any code; a passing test means your hypothesis about which def is broken is wrong), apply (batch multiple ops atomically in one turn — accepts create/edit/delete/rename PLUS all 6 projection ops insert-precondition/replace-slice/replace-hunk/wrap-in-defer/rename-param/add-import; rolls back on any error; one emit+build for the whole batch), diff, history, find, sync (rarely needed — every edit op auto-syncs the DB; only use after external file changes outside the code tool), query (raw SQL escape hatch — for schema analytics only; NEVER use to look up a def by name, grep bodies, or list files/defs-in-file — use search/outline/read-file/file-defs/impact instead, which are far cheaper on the wire), patch, simulate, validate-plan, pragmas (query comment pragmas), literals (query composite literal fields), traverse (recursive graph traversal), commit (snapshot current state), status (current branch + dirty state), diff-defs (definitions that differ between two refs — pass from:"X" and optionally to:"Y"; defaults to working tree)`,
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
	Include     []string         `json:"include,omitempty"`  // expand op: which graph hops to fold in
	Test        string           `json:"test,omitempty"`     // L11: op:test named-test reproduction (`-run <regex>` verbatim)
	Field       string           `json:"field,omitempty"`    // retarget-field-value: composite-literal field name
	Query       string           `json:"query,omitempty"`    // #153: query-adaptive read — keep only body branches touching the query
	Mode        string           `json:"mode,omitempty"`     // #160: "summary" returns model-generated one-line intent instead of body
	Question    string           `json:"question,omitempty"` // #186: natural-language question for op:"explain" co-processor
	Plan        string           `json:"plan,omitempty"`     // #187/#188/#189: trajectory plan text for op:"plan-dsl" / op:"plan-sexpr"
	Intent      string           `json:"intent,omitempty"`   // #186: natural-language exploration goal for op:"plan" (co-processor-generated trajectory)
}

// applyOp is one operation inside an apply batch. Only Op is
// required — every other field is conditional on Op's value. The
// omitempty tags matter: mcp-go generates the tool's JSON schema
// from these struct tags via reflection, and a field without
// omitempty ends up as "required" in the schema. Before #182 every
// field here was un-tagged, which meant a heterogeneous batch (e.g.,
// create + add-import in one call) was rejected at schema-validation
// time with "required: missing properties: [condition, ret, slice,
// import_path, ...]" — defeating apply's whole purpose. See the
// applyOp handler in handleApply for per-op field enforcement.
type applyOp struct {
	Op          string `json:"op"`
	Name        string `json:"name,omitempty"`
	Receiver    string `json:"receiver,omitempty"` // disambiguates same-named methods across types (#219), mirrors nameParam/editParam's Receiver
	NewName     string `json:"new_name,omitempty"`
	Body        string `json:"body,omitempty"`
	NewBody     string `json:"new_body,omitempty"`
	Module      string `json:"module,omitempty"`
	File        string `json:"file,omitempty"`
	OldFragment string `json:"old_fragment,omitempty"`
	NewFragment string `json:"new_fragment,omitempty"`
	After       string `json:"after,omitempty"`
	ReplaceAll  bool   `json:"replace_all,omitempty"`

	// Projection-op fields. Not all ops use every field; the op tag
	// picks which apply. See internal/projection for the pure functions.
	Condition  string `json:"condition,omitempty"`   // insert-precondition
	Ret        string `json:"ret,omitempty"`         // insert-precondition
	Slice      string `json:"slice,omitempty"`       // replace-slice
	Index      int    `json:"index,omitempty"`       // replace-slice / replace-hunk
	New        string `json:"new,omitempty"`         // replace-slice / replace-hunk
	Old        string `json:"old,omitempty"`         // replace-hunk
	Force      bool   `json:"force,omitempty"`       // replace-slice
	DeferBody  string `json:"defer_body,omitempty"`  // wrap-in-defer
	StmtIndex  int    `json:"stmt_index,omitempty"`  // wrap-in-defer
	OldParam   string `json:"old_param,omitempty"`   // rename-param
	NewParam   string `json:"new_param,omitempty"`   // rename-param
	ImportPath string `json:"import_path,omitempty"` // add-import
	Alias      string `json:"alias,omitempty"`       // add-import
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
	// DryRun previews the delete (mirrors apply's dry_run) without
	// touching the DB or disk. Only handleDelete reads this today; other
	// nameParam-shaped ops ignore it.
	DryRun bool `json:"dry_run,omitempty"`
	// Query, when non-empty, activates #153 query-adaptive read:
	// return only body statements whose source contains any token
	// from the query. Elided runs collapse to a single comment stub.
	// No-op if the body has <2 statements or all statements match.
	Query string `json:"query,omitempty"`
	// Mode selects the response shape. Default ("") returns the full
	// body. #160 introduces "summary": returns the model-generated
	// one-line intent summary if one exists, else falls back to the
	// full body with a header noting the summary is unavailable.
	Mode string `json:"mode,omitempty"`
	// Receiver disambiguates between multiple methods sharing Name
	// across different types (e.g. two "Equal" methods). Same #219 gap
	// editParam already closed for handleEdit, found again the hard
	// way: handleDelete accepted this field in its JSON schema but
	// never read it, always calling resolveEditTarget with receiver="",
	// which falls back to GetDefinitionByName's blast-radius tiebreak
	// -- it deletes whichever same-named def has the most references,
	// not the one actually requested. In a real trajectory this deleted
	// an unrelated, well-referenced pre-existing method instead of the
	// freshly-created, zero-reference one the agent asked for.
	Receiver string `json:"receiver,omitempty"`
	// Module/File disambiguate same-named defs across packages (#15),
	// same precedent as editParam's fields for the mutation path. File
	// wins when both are set, mirroring resolveEditTarget's precedence.
	Module string `json:"module,omitempty"`
	File   string `json:"file,omitempty"`
}

type editParam struct {
	Name    string `json:"name"`
	NewBody string `json:"new_body"`
	// Receiver disambiguates between multiple methods sharing Name
	// across different types (e.g. two "Reconsider" methods). #219:
	// previously accepted on the top-level codeParam but never
	// threaded through to handleEdit, so it was silently ignored and
	// the blast-radius tiebreak in GetDefinitionByName could resolve
	// to the wrong receiver's method.
	Receiver string `json:"receiver,omitempty"`
	// Module/File disambiguate between multiple non-method defs (e.g.
	// two "Engine" structs) sharing Name across different packages --
	// the same #219 gap Receiver closed for methods, reported again by
	// gemot dispatch for bare types: module:/file: were accepted by the
	// top-level codeParam schema but silently dropped before reaching
	// handleEdit, so a same-named type in an unrelated package could be
	// edited instead, with the tool reporting success. File wins when
	// both are set (most specific), mirroring createParam's precedent.
	Module string `json:"module,omitempty"`
	File   string `json:"file,omitempty"`
	// DryRun previews the edit (mirrors delete's dry_run) without
	// touching the DB or disk. #246: was accepted by the top-level
	// codeParam schema but silently dropped before reaching handleEdit
	// -- a caller asking for a preview got a real edit instead.
	DryRun bool `json:"dry_run,omitempty"`
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
	OldName  string `json:"old_name"`
	NewName  string `json:"new_name"`
	Receiver string `json:"receiver,omitempty"`
	Module   string `json:"module,omitempty"`
	File     string `json:"file,omitempty"`
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
	// Receiver/File disambiguate the SOURCE definition when multiple
	// defs share Name -- same #219-class gap as nameParam/editParam.
	// Before this, handleMove called GetDefinitionByName(args.Name, "")
	// directly (not resolveEditTarget), so it always took the
	// blast-radius tiebreak among same-named defs regardless of which
	// one was actually meant. For move specifically that's worse than
	// a misread: it deletes the winning def from its module and
	// recreates it in ToModule, silently relocating and (via the
	// delete+insert new-ID path) orphaning the real target.
	Receiver string `json:"receiver,omitempty"`
	File     string `json:"file,omitempty"`
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
//
// PrefixHash100 and BodySHA256 (added #177) enable cache-drift
// detection: if two shape-identical ops emit different prefixes,
// prompt-cache hits are being silently killed. Cross-call BodySHA256
// equality flags duplicate-content that dedup should have compressed.
type usageStats struct {
	Op            string `json:"op"`
	BytesReturned int    `json:"bytes_returned"`
	BytesAltRead  int    `json:"bytes_alt_read,omitempty"`
	SavingsPct    int    `json:"savings_pct,omitempty"`
	PrefixHash100 string `json:"prefix_hash_100,omitempty"`
	BodySHA256    string `json:"body_sha256,omitempty"`
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

// withUsage emits a per-op stats line to stderr and returns r
// unchanged. Previously appended a "_— returned XB vs ~YB —_" footer
// to the tool-result text; that put human-facing telemetry into the
// model's context and cached prefix for zero model-behavior value
// (#165). Bench harnesses now read stderr JSON lines instead of
// grepping the text response.
//
// #177 (2026-07-23): also computes prefix_hash_100 (first 100 bytes)
// and body_sha256 to enable cache-drift detection and cross-call
// dedup analysis. No response mutation — hashes go to stderr only.
//
// Historical note: an even earlier version of this function set
// r.StructuredContent = u. Claude's tool_result serialization treated
// structuredContent as a replacement for text content, silently
// stripping bodies (#96, detected 2026-07-20). That write is gone.
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
	if text := resultTextRaw(r); text != "" {
		full := sha256.Sum256([]byte(text))
		u.BodySHA256 = hex.EncodeToString(full[:8])
		n := len(text)
		if n > 100 {
			n = 100
		}
		prefix := sha256.Sum256([]byte(text[:n]))
		u.PrefixHash100 = hex.EncodeToString(prefix[:8])
	}
	emitUsageLog(u)
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
		if req == nil {
			return
		}
		// #245: write ops can partially mutate the DB even when they
		// return an error -- e.g. a module-spanning sync/apply that
		// fails partway through has already durably committed the
		// packages/ops processed before the failure (SQLite writes
		// commit as they happen; see autoCommit's doc comment -- there's
		// no staged/uncommitted working set to fall back on here). The
		// old code gated ALL invalidation on !result.IsError, so a
		// failed sync/apply left stale cache entries served with false
		// "nothing has changed since" confidence for exactly the defs
		// that DID change. Invalidate for write ops regardless of
		// success/failure; only the dedup/markBodyServed bookkeeping
		// below (which needs a genuine successful response) stays
		// gated on error.
		if isWriteOp(args.Op) {
			// Scope invalidation to what this write actually touched when
			// we can determine it, instead of wiping every other def's
			// cached reads in the session too. Falls back to the full
			// wipe when the blast radius isn't determinable (sync/resolve/
			// merge/checkout/commit/merge-abort, or an apply batch
			// containing an unrecognized op). Measured motivation
			// (2026-08-04): a read-locality analysis of 257 real sessions
			// on this repo found edits interleaved constantly with reads
			// of OTHER, unrelated defs -- the old blanket invalidate
			// erased the dedup benefit for all of them on every mutation.
			if names, files, ok := writeTargets(args); ok {
				s.respCache.invalidateNames(req.Session, names, files)
			} else {
				s.respCache.invalidate(req.Session)
			}
			// #154: reachability cache is a graph snapshot; any
			// mutation invalidates. Next impact/batch-impact will
			// scan refs to rebuild. Nil-safe for Measure* paths
			// that construct servers without going through
			// newMCPServer.
			if s.reach != nil {
				s.reach.invalidate()
			}
			if err == nil && result != nil && !result.IsError && args.Name != "" {
				// A read right after a mutation is almost always "show me
				// what I just did" -- mark it so the next read of this
				// name skips the summary-mode default. Only meaningful
				// on a genuine success.
				s.respCache.markMutated(req.Session, args.Name)
			}
		}
		if err != nil || result == nil || result.IsError {
			return
		}
		// #176: record before dedup potentially replaces result with a
		// stub -- marking is driven by args (what was asked for), not
		// by the final response bytes. Only an explicit full:true with
		// no query is an unambiguous full-body serve: a plain read()
		// can be silently downgraded to summary or auto-outline mode
		// (#174/#184), and a query-filtered read elides statements, so
		// neither is a reliable "the caller has everything" signal.
		if args.Op == "read" && args.Full && strings.TrimSpace(args.Query) == "" && args.Module == "" && args.File == "" {
			s.respCache.markBodyServed(req.Session, args.Name)
		}
		if op, argKey, ok := dedupOpKey(args); ok {
			result = s.respCache.dedup(req.Session, op, argKey, result)
		}
	}()

	// Validate required params per op (fail fast with clear error).
	need := func(field, label string) (*sdkmcp.CallToolResult, any, error) {
		if strings.TrimSpace(field) == "" {
			return errResult(fmt.Errorf("%s: %s is required", args.Op, label))
		}
		return nil, nil, nil
	}

	// Dolt-era git-semantics ops removed in the v0.27 SQLite migration --
	// "git-style branch/merge on definitions turned out to be a non-goal;
	// users prefer git worktrees + `defn sync`" (docs/lessons-learned.md).
	// Give a specific, honest answer instead of falling through to the
	// generic "unknown op" default below, which listed every one of these
	// names in its own "valid:" whitelist while rejecting them -- and, for
	// branch/checkout/merge/commit/resolve/diff-defs, first ran real param
	// validation (e.g. "branch: branch is required") that implied the op
	// would work once that param was supplied, when it never could.
	if removedDoltOps[args.Op] {
		return errResult(fmt.Errorf("%s: not supported -- git-style branch/merge/commit/diff ops on definitions were removed in the v0.27 SQLite migration (users prefer git worktrees + op:\"sync\"); use plain git for version control", args.Op))
	}

	if canonical, ok := opAliases[args.Op]; ok {
		args.Op = canonical
	}

	switch args.Op {
	case "read", "outline", "impact", "delete", "similar":
		if strings.TrimSpace(args.Name) == "" {
			if strings.TrimSpace(args.File) != "" {
				return errResult(fmt.Errorf("%s: name is required — pass name:\"<def>\" for one definition, or use op:\"overview\", file:%q to see every def in that file", args.Op, args.File))
			}
			return errResult(fmt.Errorf("%s: name is required", args.Op))
		}
	case "test":
		// #241: test:"TestX" (named-test reproduction -- e.g. replicate a
		// bug report before touching any code) doesn't need name; only
		// the def-scoped path (name:"F", run tests covering F) does. The
		// grouped case above required name unconditionally for "test"
		// too, which broke this tool's own documented named-test path.
		if strings.TrimSpace(args.Test) == "" && strings.TrimSpace(args.Name) == "" {
			if strings.TrimSpace(args.File) != "" {
				return errResult(fmt.Errorf("test: name is required — pass name:\"<def>\" for one definition, test:\"TestX\" to run a named test directly, or use op:\"overview\", file:%q to see every def in that file", args.File))
			}
			return errResult(fmt.Errorf("test: name is required (or test:\"TestX\" to run a named test directly)"))
		}
	case "explain":
		// #186: accept name OR names[]; the Q+A co-processor path
		// often passes a multi-def scope so a single "name" wouldn't
		// suffice.
		if strings.TrimSpace(args.Name) == "" && len(args.Names) == 0 {
			return errResult(fmt.Errorf("explain: name or names is required"))
		}
	case "context":
		// #195: question-driven bundle. No name — server picks defs
		// from the question tokens.
		if strings.TrimSpace(args.Question) == "" {
			return errResult(fmt.Errorf("context: question is required"))
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
		// old_fragment/new_fragment are edit's fragment-mode field names for
		// this exact same "before/after text" concept -- accept them here
		// too instead of erroring, since a model batching edit and
		// replace-hunk together naturally reaches for the name it just used
		// on the sibling op. Confirmed hitting two independent real
		// trajectories (one standalone, one inside apply).
		if args.Old == "" && args.OldFragment != "" {
			args.Old = args.OldFragment
		}
		if args.New == "" && args.NewFragment != "" {
			args.New = args.NewFragment
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
	case "emit":
		if r, o, e := need(args.Out, "out"); r != nil {
			return r, o, e
		}
	case "plan-dsl", "plan-sexpr":
		if r, o, e := need(args.Plan, "plan"); r != nil {
			return r, o, e
		}
	case "plan":
		if r, o, e := need(args.Intent, "intent"); r != nil {
			return r, o, e
		}
	}

	// Tag results from read-only ops while startup ingest is still running.
	stale := !s.ready.Load() && s.projectDir != ""
	wrapStale := func(r *sdkmcp.CallToolResult, o any, e error) (*sdkmcp.CallToolResult, any, error) {
		// Previously gated on !r.IsError, which meant an error result
		// during the startup race (e.g. overview(file:) hitting "no
		// definitions found" because ingest hasn't reached that file
		// yet) got no stale-index warning at all, while a NON-error "no
		// matches" from search did -- an inconsistency a real trajectory
		// (prometheus-18972) hit directly: overview(file:...) silently
		// said "no definitions found" with nothing to suggest the index
		// might just be incomplete, so the agent had no signal to try
		// op:"sync" instead of concluding the path was wrong. An error
		// during a stale window is exactly when this context matters
		// most -- it's the case most likely to be a false negative.
		if stale && r != nil {
			if len(r.Content) > 0 {
				if tc, ok := r.Content[0].(*sdkmcp.TextContent); ok {
					tc.Text = "[startup ingest in progress — results may be stale]\n\n" + tc.Text
				}
			}
		}
		return r, o, e
	}

	// #209: per-turn circuit breaker on read-shaped singleton calls
	// (read/outline/search/impact/overview/methods/single-name expand).
	// Resets on a genuine batch (context/apply, or expand with 2+
	// names), or when a new turn starts (detected via the turn-token
	// bumped by hooks/defn-capture-question.sh on UserPromptSubmit).
	if s.respCache != nil && req != nil {
		s.respCache.mu.Lock()
		sc := s.respCache.getSession(req.Session)
		s.checkTurnBoundary(sc)
		s.checkCompactionEpoch(sc)
		// #210: a single-name expand is not a batch -- only 2+ names (or
		// context/apply, which always consolidate) count as one. #212:
		// overview always consolidates (whole project, or every def in a
		// file) -- see readShapedOps for why it moved here instead of
		// counting as a singleton.
		isBatch := args.Op == "context" || args.Op == "apply" || args.Op == "overview" || (args.Op == "expand" && len(args.Names) >= 2)
		nameForTracking := args.Name
		if args.Op == "expand" && len(args.Names) == 1 {
			nameForTracking = args.Names[0]
		}
		s.trackReadShapedName(sc, args.Op, nameForTracking)
		breakerMsg := s.circuitBreakerCheck(sc, args.Op, isBatch)
		// A block on a nameable op does not just refuse -- it auto-batches
		// every name seen since the last reset into one expand call instead.
		// Measured motivation (2026-08-08 pilot digging): a bare refusal
		// assumes the model immediately restructures its whole remaining
		// strategy after one denial. It often doesn't -- one real
		// trajectory hit 11 CONSECUTIVE blocked calls (26% of that
		// trajectory's entire tool budget) before switching to batching,
		// each one a full round-trip returning zero information. This
		// makes the server robust to that instead of depending on the
		// model's compliance.
		var autoNames []string
		var autoWantsBody bool
		if breakerMsg != "" && nameableReadOps[args.Op] && len(sc.pendingReadNames) > 0 {
			autoNames = append([]string(nil), sc.pendingReadNames...)
			autoWantsBody = sc.pendingWantsBody
			sc.pendingReadNames = nil
			sc.pendingWantsBody = false
			sc.readShapedCount = 0
		}
		s.respCache.mu.Unlock()
		if len(autoNames) > 0 {
			include := []string{"outline", "callers"}
			if autoWantsBody {
				// #250: a blocked op:"read" mid-batch wants source, not just
				// outline+callers -- dropping the body silently downgraded the
				// response below what was actually asked for (a real
				// grpc-go-3351 trajectory burned 2 extra round-trips
				// re-requesting the body this should have returned).
				include = append(include, "body")
			}
			r, o, e := wrapStale(s.handleExpand(ctx, req, codeParam{Names: autoNames, Include: include}))
			note := fmt.Sprintf("[circuit breaker: auto-batched %d individual lookups this turn (%s) into one expand call instead of refusing -- call code(op:\"context\"/op:\"expand\", names:[...]) yourself next time to skip this extra round-trip.]\n\n", len(autoNames), strings.Join(autoNames, ", "))
			return prependNote(r, note), o, e
		}
		if breakerMsg != "" {
			return textResult(breakerMsg), nil, nil
		}
	}

	switch args.Op {
	case "read":
		// Cross-def context reuse, symmetric with outline's below: a
		// non-full read's best case IS the full body, so if that body was
		// already served this session via read(full:true), a later plain
		// read (no full:true, no query) can only return the same content
		// or a downgraded subset -- never more. Bypassed when full:true or
		// a query is set: those aren't redundant with a prior plain-args
		// full-body serve check the same way outline's bypass works.
		if !args.Full && req != nil && s.respCache != nil && strings.TrimSpace(args.Query) == "" && args.Module == "" && args.File == "" {
			if epochsAgo, ok := s.respCache.bodyServedEpochsAgo(req.Session, args.Name); ok {
				if epochsAgo <= staleEpochThreshold {
					stub := fmt.Sprintf(
						"[%s's full body was already read in this session (read with full:true) -- a plain read would return the same content or a downgraded subset, never more. Nothing new here. If the def may have changed since, call code(op:\"sync\") first.]\n",
						args.Name,
					)
					return textResult(stub), nil, nil
				}
				// #227: the earlier full-body serve survived enough
				// compactions that trusting the caller still has it is
				// risky. Give the richer expand bundle instead of either a
				// stub that might be wrong or a narrow read that just
				// re-derives a subset of what may have been lost.
				return wrapStale(s.handleExpand(ctx, req, codeParam{Name: args.Name, Include: []string{"outline", "callers", "body"}, Module: args.Module, File: args.File}))
			}
		}
		r, o, e := wrapStale(s.handleGetDefinition(ctx, req, nameParam{Name: args.Name, Full: args.Full, Query: args.Query, Mode: args.Mode, Receiver: args.Receiver, Module: args.Module, File: args.File}))
		if note := s.ambiguityNote(args.Name, args.Receiver, args.Module, args.File); note != "" {
			r = prependNote(r, note)
		}
		return r, o, e
	case "resummarize":
		return s.handleResummarize(ctx, req, args)
	case "read-and-verify":
		return wrapStale(s.handleReadAndVerify(ctx, req, args))
	case "retarget-field-value":
		return s.handleRetargetFieldValue(ctx, req, args)
	case "outline":
		// #176: cross-def context reuse. If this def's full body was
		// already served via read(full:true) this session, outline
		// would return strictly less information (signature/doc/
		// callers/callees, no body) -- skip re-deriving and
		// re-transmitting it. Bypassed when a query is set: a
		// query-filtered outline highlights different callees than a
		// plain one, so it is not redundant even with the body in hand.
		if req != nil && s.respCache != nil && strings.TrimSpace(args.Query) == "" && args.Module == "" && args.File == "" {
			if epochsAgo, ok := s.respCache.bodyServedEpochsAgo(req.Session, args.Name); ok {
				if epochsAgo <= staleEpochThreshold {
					stub := fmt.Sprintf(
						"[%s's full body was already read in this session (read with full:true) -- outline would return strictly less information (signature/doc/callers/callees, no body). Nothing new here. If the def may have changed since, call code(op:\"sync\") first.]\n",
						args.Name,
					)
					return textResult(stub), nil, nil
				}
				return wrapStale(s.handleExpand(ctx, req, codeParam{Name: args.Name, Include: []string{"outline", "callers", "body"}, Module: args.Module, File: args.File}))
			}
		}
		r, o, e := wrapStale(s.handleOutline(ctx, req, nameParam{Name: args.Name, Query: args.Query, Receiver: args.Receiver, Module: args.Module, File: args.File}))
		if note := s.ambiguityNote(args.Name, args.Receiver, args.Module, args.File); note != "" {
			r = prependNote(r, note)
		}
		return r, o, e
	case "slice":
		// Same cross-def context reuse as read/outline above: any slice
		// kind is a strict subset of the full body already served.
		if req != nil && s.respCache != nil && args.Module == "" && args.File == "" {
			if epochsAgo, ok := s.respCache.bodyServedEpochsAgo(req.Session, args.Name); ok {
				if epochsAgo <= staleEpochThreshold {
					stub := fmt.Sprintf(
						"[%s's full body was already read in this session (read with full:true) -- any slice is a strict subset of that body. Nothing new here. If the def may have changed since, call code(op:\"sync\") first.]\n",
						args.Name,
					)
					return textResult(stub), nil, nil
				}
				return wrapStale(s.handleExpand(ctx, req, codeParam{Name: args.Name, Include: []string{"outline", "callers", "body"}, Module: args.Module, File: args.File}))
			}
		}
		r, o, e := wrapStale(s.handleSlice(ctx, req, args))
		if note := s.ambiguityNote(args.Name, args.Receiver, args.Module, args.File); note != "" {
			r = prependNote(r, note)
		}
		return r, o, e
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
		// #248: query: is a real codeParam field, but only op:"read" wires
		// it up (query-adaptive body filtering). A caller reaching for
		// search(query:"X") instead of search(pattern:"X") got pattern=="",
		// which silently matched nearly everything and returned a
		// caller-count-ranked list dressed up with a plausible "score" --
		// indistinguishable from a real, relevant result. Accept query: as
		// a pattern alias here, same precedent as the name: fallback above.
		if args.Pattern == "" {
			args.Pattern = args.Query
		}
		r, o, e := wrapStale(s.handleSearch(ctx, req, args))
		return s.appendStarter(r, o, e, req, args.Pattern)
	case "impact":
		r, o, e := wrapStale(s.handleImpact(ctx, req, args))
		if note := s.ambiguityNote(args.Name, args.Receiver, args.Module, args.File); note != "" {
			r = prependNote(r, note)
		}
		return r, o, e
	case "explain":
		// #186: when a `question` is passed, route to the Sonnet
		// co-processor path (assembles bodies, calls Sonnet, returns
		// synthesized answer). Bare explain (no question) keeps the
		// legacy static-context shape.
		if strings.TrimSpace(args.Question) != "" {
			return wrapStale(s.handleExplainWithQuestion(ctx, req, args))
		}
		r, o, e := wrapStale(s.handleExplain(ctx, req, nameParam{Name: args.Name, Receiver: args.Receiver, Module: args.Module, File: args.File}))
		if note := s.ambiguityNote(args.Name, args.Receiver, args.Module, args.File); note != "" {
			r = prependNote(r, note)
		}
		return r, o, e
	case "context":
		// #195: server-side bundle to collapse turn-1 exploration.
		// Question drives the search; server picks top-N relevant
		// defs, outlines them, assembles ref graph, optionally adds
		// Sonnet synthesis. One tool call replaces 10-40 sequential
		// exploration calls in the anti-exploration turn shape.
		return wrapStale(s.handleContext(ctx, req, args))
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
		return s.handleEdit(ctx, req, editParam{Name: args.Name, NewBody: body, Receiver: args.Receiver, Module: args.Module, File: args.File, DryRun: args.DryRun})
	case "insert":
		return s.handleInsert(ctx, req, args)
	case "create":
		return s.handleCreate(ctx, req, createParam{Body: args.Body, Module: args.Module, File: args.File})
	case "delete":
		return s.handleDelete(ctx, req, nameParam{Name: args.Name, Force: args.Force, DryRun: args.DryRun, Receiver: args.Receiver, Module: args.Module, File: args.File})
	case "rename":
		return s.handleRename(ctx, req, renameParam{OldName: args.OldName, NewName: args.NewName, Receiver: args.Receiver, Module: args.Module, File: args.File})
	case "move":
		return s.handleMove(ctx, req, moveParam{Name: args.Name, ToModule: args.Module, Receiver: args.Receiver, File: args.File})
	case "test":
		if args.Test != "" {
			return s.handleTestByName(ctx, req, args.Test, args.Module, args.File)
		}
		r, o, e := s.handleTest(ctx, req, nameParam{Name: args.Name, Receiver: args.Receiver, Module: args.Module, File: args.File})
		if note := s.ambiguityNote(args.Name, args.Receiver, args.Module, args.File); note != "" {
			r = prependNote(r, note)
		}
		return r, o, e
	case "similar":
		r, o, e := wrapStale(s.handleSimilar(ctx, req, nameParam{Name: args.Name, Receiver: args.Receiver, Module: args.Module, File: args.File}))
		if note := s.ambiguityNote(args.Name, args.Receiver, args.Module, args.File); note != "" {
			r = prependNote(r, note)
		}
		return r, o, e
	case "apply":
		return s.handleApply(ctx, req, applyParam{Operations: args.Operations, DryRun: args.DryRun})
	case "query":
		return wrapStale(s.handleQuery(ctx, req, sqlParam{SQL: args.SQL}))
	case "find":
		return wrapStale(s.handleFind(ctx, req, findParam{File: args.File, Line: args.Line}))
	case "overview":
		r, o, e := wrapStale(s.handleOverview(ctx, req, args))
		q := args.File
		if q == "" {
			q = "project structure"
		}
		return s.appendStarter(r, o, e, req, q)
	case "methods":
		return wrapStale(s.handleMethods(ctx, req, nameParam{Name: args.Name, Query: args.Query, Module: args.Module, File: args.File}))
	case "patch":
		return s.handlePatch(ctx, req, args)
	case "sync":
		return s.handleSync(ctx, req, args)
	case "test-coverage":
		r, o, e := wrapStale(s.handleTestCoverage(ctx, req, args))
		if note := s.ambiguityNote(args.Name, args.Receiver, args.Module, args.File); note != "" {
			r = prependNote(r, note)
		}
		return r, o, e
	case "batch-impact":
		return wrapStale(s.handleBatchImpact(ctx, req, args))
	case "simulate":
		return s.handleSimulate(ctx, req, args)
	case "file-defs":
		return s.handleFileDefs(ctx, req, args)
	case "expand":
		return wrapStale(s.handleExpand(ctx, req, args))
	case "plan-dsl":
		return wrapStale(s.handlePlanDSL(ctx, req, args))
	case "plan-sexpr":
		return wrapStale(s.handlePlanSExpr(ctx, req, args))
	case "plan":
		return wrapStale(s.handlePlanIntent(ctx, req, args))
	case "read-file":
		return wrapStale(s.handleReadFile(ctx, req, args))
	case "validate-plan":
		return wrapStale(s.handleValidatePlan(ctx, req, args))
	case "pragmas":
		return wrapStale(s.handlePragmas(ctx, req, args))
	case "literals":
		return wrapStale(s.handleLiterals(ctx, req, args))
	case "traverse":
		r, o, e := wrapStale(s.handleTraverse(ctx, req, args))
		if note := s.ambiguityNote(args.Name, args.Receiver, args.Module, args.File); note != "" {
			r = prependNote(r, note)
		}
		return r, o, e
	case "emit":
		return s.handleEmit(ctx, req, args)
	case "gc":
		return s.handleGC(ctx, req, args)
	default:
		return errResult(fmt.Errorf("unknown op %q — valid: read, read-and-verify, outline, slice, insert-precondition, replace-slice, replace-hunk, wrap-in-defer, rename-param, add-import, search, impact, explain, context, similar, untested, edit, insert, create, delete, retarget-field-value, rename, move, test, apply, query, find, sync, test-coverage, batch-impact, simulate, file-defs, validate-plan, pragmas, literals, traverse, emit, gc, resummarize, plan-dsl, plan-sexpr, plan, overview, methods, patch, expand, read-file", args.Op))
	}
}

func (s *server) handleImpact(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.resolveEditTarget(args.Name, args.Receiver, args.Module, args.File)
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
	if len(prodCallers) > 0 {
		// #241: a caller-count fact was already visible here in a real
		// trajectory that still edited a signature-changing def alone --
		// the existing WARNING above is framed around test-coverage risk,
		// not "this caller's call site may need a corresponding change."
		// Same fact, different risk; make the coupled-change one explicit
		// too instead of relying on the reader to infer it.
		sb.WriteString("  tip: if you're changing this def's signature (params/returns), batch it with its production caller(s) via op:\"apply\" to avoid an edit-then-rollback round trip.\n")
	}
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

	callersTotal := len(impact.DirectCallers)
	callers := make([]impactDefRef, 0, min(callersTotal, impactJSONCap))
	for i, c := range impact.DirectCallers {
		if i >= impactJSONCap {
			break
		}
		callers = append(callers, toRef(c))
	}
	ifaceTotal := len(impact.InterfaceDispatchCallers)
	ifaceDispatch := make([]impactDefRef, 0, min(ifaceTotal, impactJSONCap))
	for i, c := range impact.InterfaceDispatchCallers {
		if i >= impactJSONCap {
			break
		}
		ifaceDispatch = append(ifaceDispatch, toRef(c))
	}
	testsTotal := len(impact.Tests)
	tests := make([]impactDefRef, 0, min(testsTotal, impactJSONCap))
	for i, t := range impact.Tests {
		if i >= impactJSONCap {
			break
		}
		tests = append(tests, toRef(t))
	}
	truncated := callersTotal > impactJSONCap || ifaceTotal > impactJSONCap || testsTotal > impactJSONCap

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
		"direct_callers_total":       callersTotal,
		"interface_dispatch_callers": ifaceDispatch,
		"interface_dispatch_total":   ifaceTotal,
		"transitive_count":           impact.TransitiveCount,
		"tests":                      tests,
		"tests_total":                testsTotal,
		"uncovered_by":               impact.UncoveredBy,
		"blast_radius":               blastRadius,
	}
	if truncated {
		result["truncated"] = fmt.Sprintf("each list capped at %d entries -- use op:\"query\" or narrow with op:\"impact\", query:\"<term>\" to see more of a specific list", impactJSONCap)
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

func (s *server) handleGetDefinition(_ context.Context, req *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	// #174: summary-mode is now the default for read (opt-out via
	// DEFN_SUMMARY_READ_DEFAULT=0), not opt-in. Safe regardless of
	// backfill coverage -- the mode=="summary" branch below falls
	// through to the full body whenever no summary exists yet or it's
	// stale, so an unbackfilled def behaves identically to before this
	// flip. !args.Full guards the escape hatch: an explicit full:true
	// must win over the implicit default -- without this, full:true
	// would be silently ignored on every def with a fresh summary,
	// since args.Mode == "" is now no longer a reliable signal that
	// the caller wants the body.
	justMutated := req != nil && s.respCache != nil && s.respCache.takeMutated(req.Session, args.Name)
	if args.Mode == "" && !args.Full && !justMutated && os.Getenv("DEFN_SUMMARY_READ_DEFAULT") != "0" {
		args.Mode = "summary"
	}
	d, err := s.resolveEditTarget(args.Name, args.Receiver, args.Module, args.File)
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}

	// #160 summary mode. Returns the compact model-generated intent
	// line if we have a fresh one, else falls back through to the full
	// body path below with a header noting summary unavailability. The
	// staleness check compares stored BodyHash against current body —
	// mismatch means the def was edited after the summary was written.
	if args.Mode == "summary" {
		if sum, sErr := s.backend.GetDefSummary(d.ID); sErr == nil && sum != nil {
			currentHash := store.HashBodyStructural(d.Body)
			// #248: a Stub-backend placeholder ("TODO: <Name>") is not a
			// real summary -- enqueueSummary's own doc comment promises the
			// read path treats it as a miss and falls back to full body,
			// but this check was missing, so summary-mode-by-default served
			// the literal stub text as if it were a genuine intent line.
			if sum.BodyHash == currentHash && sum.Model != summary.StubModelName {
				return renderSummaryOnly(d, sum), nil, nil
			}
			// Stale or stub — fall through to body but signal it.
		}
		// Falls through to full-body rendering; the reader can see
		// no summary appeared in the response header.
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

	// #184 auto-downgrade to outline on large bodies. Fires only when
	// no other compact projection is active (summary/upstream/query),
	// no explicit body request (full:true or mode:"body"), and the
	// body exceeds readAutoOutlineThreshold. Delegates to handleOutline
	// so the projection shape stays consistent; prepends a note that
	// tells the model how to get the body if it actually needs it.
	// Rationale: #174 receipt showed CLAUDE.md-level outline-first
	// nudges failed. Taking the choice away is the recommended lever.
	if !args.Full &&
		args.Mode != "body" &&
		strings.TrimSpace(args.Query) == "" &&
		len(d.Body) > readAutoOutlineThreshold {
		if outlineR, _, oErr := s.handleOutline(nil, nil, args); oErr == nil && outlineR != nil && !outlineR.IsError {
			text := resultTextRaw(outlineR)
			// Note intentionally does NOT enumerate the escape hatches.
			// Post-#184 chi bench showed the model reflexively retries
			// with full:true when the hint reads like a menu. Escape
			// hatches (full:true, mode:"body") still work; they're
			// documented in the tool description, not advertised inline.
			note := fmt.Sprintf(
				"_[Outline shown — body is %d bytes / %d lines. Full body only needed when editing.]_\n\n",
				len(d.Body), strings.Count(d.Body, "\n")+1,
			)
			out := note + text
			return withUsage(textResult(out), usageStats{
				Op:            "read",
				BytesReturned: len(out),
				BytesAltRead:  s.fileAltBytes(d),
			}), nil, nil
		}
		// Outline failed for some reason; fall through to full body.
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

	// #160 nudge: when the body is large and no compact projection is
	// active (summary/query/upstream-match), point at mode:"summary".
	if args.Mode != "summary" && strings.Count(body, "\n") > summaryHintLineThreshold {
		sb.WriteString(fmt.Sprintf(
			"\n_tip: body is %d lines; `mode:\"summary\"` returns intent+sig in a compact projection when a summary is available._\n",
			strings.Count(body, "\n")+1,
		))
	}

	// #202: fold the "who calls me / what am I next to" context into
	// the read response itself. Model gets bundled context whether or
	// not it asked, which collapses the read→impact→read-caller chain
	// into one round-trip. Skipped only for query-adaptive filtered
	// reads (query is set) since those are already narrower on purpose.
	if strings.TrimSpace(args.Query) == "" && !stripped("related-footer") {
		sb.WriteString(s.renderReadNeighborhood(d))
	}

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
		if err != nil {
			return errResult(err)
		}
		// #216: always also run the FTS body/doc search and merge, rather
		// than gating it on Stage 1 being completely empty. Stage 1's
		// signature LIKE can incidentally match a def whose ingested
		// signature has its doc comment baked in (a separate ingest bug
		// -- see #216), which used to silently suppress the far more
		// complete and correct FTS search entirely: one coincidental,
		// arbitrarily incomplete Stage 1 hit meant Stage 2 never ran.
		ftsDefs, ftsErr := s.backend.SearchDefinitions(args.Pattern)
		if ftsErr == nil {
			defs = mergeDefsByID(defs, ftsDefs)
		}
	}
	if err != nil {
		return errResult(err)
	}

	// #stale8: search doesn't apply Go's own "pkg.Symbol" qualified-name
	// convention that name-based ops resolve via resolveDottedQualifiedName
	// -- a caller reaching for search(pattern:"zrpc.WithUnaryClientInterceptor")
	// got a literal substring match against a string no def's name/body
	// actually contains verbatim (defs are named "WithUnaryClientInterceptor"
	// alone), even though the bare symbol finds it trivially. Retry with
	// just the part after the last "." when the qualified form comes up
	// empty and looks like an identifier, not a file path or LIKE glob.
	dottedNote := ""
	if len(defs) == 0 {
		if idx := strings.LastIndex(args.Pattern, "."); idx > 0 && !strings.ContainsAny(args.Pattern, "/%") {
			bare := args.Pattern[idx+1:]
			if bare != "" {
				bareDefs, bareErr := s.backend.FindDefinitions("%" + bare + "%")
				if bareErr == nil {
					if ftsDefs, ftsErr := s.backend.SearchDefinitions(bare); ftsErr == nil {
						bareDefs = mergeDefsByID(bareDefs, ftsDefs)
					}
					if len(bareDefs) > 0 {
						defs = bareDefs
						dottedNote = fmt.Sprintf("_note: no match for the qualified name %q -- retried with the bare symbol %q (search doesn't parse package qualifiers, unlike read/outline/edit). Pass file: to scope to the right package instead._\n\n", args.Pattern, bare)
					}
				}
			}
		}
	}

	// #250: include: is a real codeParam field, but only op:"expand" wires
	// it up (graph-hop selection). A caller reaching for
	// search(pattern:"X", include:["pkg"]) by analogy with expand's
	// scoping-flavored include got total silence -- no error, no note,
	// just an unfiltered repo-wide result set indistinguishable from a
	// genuinely scoped-and-empty query. Confirmed via a real
	// go-zero-1964 trajectory: search(pattern:"logx.Info", include:["rest"])
	// returned 12 unrelated repo-wide defs, twice, before the agent gave
	// up on search entirely. Same precedent as #241's file: fix and
	// expand's own "unsupported include kinds ignored" note -- surface it
	// instead of silently dropping it.
	includeNote := dottedNote
	if len(args.Include) > 0 {
		includeNote += fmt.Sprintf("_note: \"include\" has no effect on search (that's expand's graph-hop selector) -- ignored: %s. Use file:\"<hint>\" to scope search results by source path instead._\n\n", strings.Join(args.Include, ", "))
	}

	// #241: file: was accepted as a param but silently ignored -- every
	// search ran repo-wide regardless. Root-caused via a real
	// grpc-go-2630 trajectory: the agent called
	// search(pattern:"drop", file:"grpclb") expecting scoping, got
	// unfiltered repo-wide results ranked by IDF, and the top hits were
	// unrelated defs from elsewhere in the module -- contributing to a
	// wrong-function edit. Substring match on source_file (not
	// findModuleByFile's directory-suffix match used by read/outline/
	// edit) because callers pass bare package-ish hints like "grpclb",
	// not full paths with a directory component.
	if args.File != "" {
		filtered := defs[:0]
		for _, d := range defs {
			if strings.Contains(d.SourceFile, args.File) {
				filtered = append(filtered, d)
			}
		}
		defs = filtered
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
			r, o, e := s.bodyScanResult(scanPattern, limit, args.File)
			if includeNote != "" {
				r = prependNote(r, includeNote)
			}
			return r, o, e
		}
	}

	// Auto-rank when the candidate set exceeds `limit`. Alphabetical
	// truncation buries the useful defs behind whatever sorts first,
	// so trigger the caller-count/text-overlap ranker so the head of
	// the list is actually informative. Explicit rank:true still works.
	if args.Rank || len(defs) > limit {
		r, o, e := s.rankedSearchResult(args.Pattern, defs, limit)
		if includeNote != "" {
			r = prependNote(r, includeNote)
		}
		return r, o, e
	}

	type summary struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Receiver   string `json:"receiver,omitempty"`
		SourceFile string `json:"file,omitempty"`
		Preview    string `json:"preview,omitempty"`
	}
	results := make([]summary, 0, limit)
	for _, d := range defs {
		if len(results) >= limit {
			break
		}
		// #159: inline body preview for the top-N hits collapses the
		// grep→view bigram (867 occurrences in the Multi-SWE-bench Go
		// corpus). Cap at 3 previews per response so it doesn't inflate
		// on name-browse queries; cap each preview at 5 lines. Model can
		// still call read for the full body.
		s := summary{Name: d.Name, Kind: d.Kind, Receiver: d.Receiver, SourceFile: d.SourceFile}
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
	if includeNote != "" {
		text = includeNote + text
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
func (s *server) bodyScanResult(pattern string, limit int, file string) (*sdkmcp.CallToolResult, any, error) {
	// #241 covered stages 1-2's file: scoping but missed this stage-3
	// fallback -- SearchBodiesLike itself has no file parameter, so a
	// wider fetch is over-fetched here and filtered by SourceFile before
	// truncating to limit, rather than trusting the store layer to scope it.
	fetchLimit := limit
	if file != "" {
		fetchLimit = limit * 20
		if fetchLimit > 500 {
			fetchLimit = 500
		}
	}
	hits, err := s.backend.SearchBodiesLike(pattern, fetchLimit)
	if err != nil {
		return errResult(fmt.Errorf("search body-scan: %w", err))
	}
	if file != "" {
		filtered := hits[:0]
		for _, h := range hits {
			if strings.Contains(h.SourceFile, file) {
				filtered = append(filtered, h)
			}
		}
		hits = filtered
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	if len(hits) == 0 {
		scope := ""
		if file != "" {
			scope = fmt.Sprintf(" scoped to file:%q", file)
		}
		// A pattern like "A|B|C" reads as regex alternation but search has
		// no regex support anywhere in its path (LIKE + FTS + substring
		// scan, all literal) -- confirmed via a real go-zero-2283
		// trajectory where search(pattern:"SetCors|WithCors|...") came back
		// "no matches" even though WithCors existed, found trivially by a
		// follow-up single-term search. "|" is a reasonable multi-term
		// guess an agent will keep making without this hint.
		regexHint := ""
		if strings.Contains(pattern, "|") {
			regexHint = " Note: pattern is a plain substring/LIKE match, not regex — \"|\" is searched for as a literal character, not alternation. Call search once per term instead."
		}
		msg := fmt.Sprintf(
			"[no matches for %q%s — tried name-LIKE, FTS on doc+body, and substring body-scan.%s If you're grepping for a comment or string literal, this substring wasn't found in any indexed body. Try `overview` for project shape or a broader pattern.]",
			pattern, scope, regexHint,
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
		Name       string  `json:"name"`
		Kind       string  `json:"kind"`
		Receiver   string  `json:"receiver,omitempty"`
		SourceFile string  `json:"file,omitempty"`
		Score      float64 `json:"score"`
		Preview    string  `json:"preview,omitempty"`
	}
	out := make([]rankedSummary, 0, limit)
	for i, r := range scored {
		if i >= limit {
			break
		}
		rs := rankedSummary{
			Name: r.Def.Name, Kind: r.Def.Kind, Receiver: r.Def.Receiver,
			SourceFile: r.Def.SourceFile, Score: r.Score,
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
	// #219/gemot dispatch: receiver disambiguates same-named methods
	// across different types; module:/file: disambiguate same-named
	// non-method defs across different packages -- same gap, no
	// receiver to key off of. See resolveEditTarget.
	d, err := s.resolveWriteTarget(args.Name, args.Receiver, args.Module, args.File)
	if err != nil {
		if args.Receiver != "" {
			return s.notFoundOrErr(fmt.Sprintf("%s.%s", args.Receiver, args.Name), err)
		}
		return s.notFoundOrErr(args.Name, err)
	}

	// Validate new body parses as Go.
	src := "package x\n" + args.NewBody
	if _, parseErr := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments); parseErr != nil {
		return errResult(fmt.Errorf("new_body has syntax error: %v", parseErr))
	}

	// A multi-decl new_body silently stores every extra declaration as
	// literal text inside this ONE definition's Body -- it parses fine
	// (Go allows several func decls in one string) and passes the
	// identity check below (which only looks at the first decl), so
	// nothing catches it here. The real trajectory this guards against:
	// an edit's new_body concatenated 3 functions together, got stored
	// verbatim under the first one's name, and a later sync/re-ingest of
	// the emitted file split the extra two into duplicate definitions --
	// producing a "redeclared in this block" build failure neither the
	// edit nor the sync surfaced clearly.
	if n := countTopLevelDecls(args.NewBody); n > 1 {
		return errResult(fmt.Errorf("edit %s%s: new_body has %d top-level declarations — op:\"edit\" changes ONE definition's body; batch multiple changes with op:\"apply\", or use op:\"create\" with file: to add new declarations", formatReceiver(d.Receiver), d.Name, n))
	}

	// #222: edit must preserve identity. A new_body that declares a
	// different name/receiver than d leaves d.Name/d.Receiver stale
	// while the body says otherwise -- mergeDeclsIntoSource matches the
	// on-disk decl by the (now-stale) d.Name and splices in the
	// differently-named body, so the merged file ends up with no decl
	// under the old name. That trips safeWriteGoFile's separate
	// on-disk-decl-loss check and blocks the write for the WHOLE file,
	// silently dragging down any other op batched alongside this one.
	// Reject up front instead: op:"edit" changes content, op:"rename"
	// changes identity.
	if newName, _, newReceiver, _ := s.inferFromBody(args.NewBody); newName != "" && (newName != d.Name || newReceiver != d.Receiver) {
		return errResult(fmt.Errorf("edit %s%s: new_body declares %s%s, which changes its name/receiver — use code(op:\"rename\") to rename a definition; op:\"edit\" only changes body content", formatReceiver(d.Receiver), d.Name, formatReceiver(newReceiver), newName))
	}

	// #246: dry_run was accepted by the top-level codeParam schema and
	// silently dropped before reaching editParam -- the same
	// accepted-but-not-wired gap already fixed for delete's dry_run,
	// except here the effect is worse: the caller asked for a preview
	// and got a REAL edit instead, with no error to signal the mistake.
	// All validation above (parse, multi-decl, identity) has already
	// run, so a dry-run report here is a genuine preview, not a guess.
	if args.DryRun {
		return textResult(fmt.Sprintf("- would update %s%s (id=%d)\n\n(dry run — no changes made)", formatReceiver(d.Receiver), d.Name, d.ID)), nil, nil
	}

	// Capture the pre-edit signature so we can decide whether the build
	// gate is safely skippable (#148: body-only edit with a stable
	// signature keeps dispatch invariant — callers don't need re-typecheck).
	// Use extractSignature on both sides so the comparison is
	// AST-canonicalized (d.Signature from ingest has doc-comment prefix
	// lines; extractSignature strips them — comparing them directly
	// false-positives "sig changed" on every doc-adjacent edit).
	oldBody := d.Body
	oldSignature := extractSignature(d.Body)
	d.Body = args.NewBody
	d.Signature = extractSignature(args.NewBody)

	// #12: write and build/emit-gate through a transaction so a failure
	// leaves neither the DB nor the file changed. Previously this wrote
	// straight to s.backend, and a later failure (a build failure on
	// the sig-changed path, or an emit-level WARNING on the sig-stable
	// path) was informational only.
	tx, commit, rollback, txErr := s.backend.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback()

	id, err := tx.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}
	d.ID = id

	recv := formatReceiver(d.Receiver)

	sigStable := oldSignature == d.Signature
	var buildResult string
	if sigStable {
		opts := emit.Opts{}
		if d.SourceFile != "" {
			opts.GoimportsFiles = []string{d.SourceFile}
			opts.TouchedFiles = []string{d.SourceFile}
			// Quick win (2026-08-04 spike): a sig-stable body edit whose
			// package-selector references are unchanged can't have
			// changed what this file needs to import -- skip goimports'
			// subprocess spawn entirely rather than running it as a
			// guaranteed no-op. See Opts.SkipGoimports and
			// bodyImportFootprintUnchanged.
			opts.SkipGoimports = bodyImportFootprintUnchanged(oldBody, args.NewBody)
		}
		// #148's whole point is skipping go build here for perf --
		// commitOrRollbackOnEmit preserves that (emit-only, no build)
		// while still gating commit on the result coming back clean.
		buildResult = s.commitOrRollbackOnEmit(tx, commit, rollback, opts)
	} else {
		if os.Getenv("DEFN_MEASURE_TIMING") == "1" {
			fmt.Fprintf(os.Stderr, "  [edit] signature changed, build required:\n    old: %q\n    new: %q\n", oldSignature, d.Signature)
		}
		var opts emit.Opts
		if d.SourceFile != "" {
			opts = emit.Opts{GoimportsFiles: []string{d.SourceFile}, TouchedFiles: []string{d.SourceFile}}
		}
		buildResult = s.commitOrRollbackOnBuild(tx, commit, rollback, opts)
	}

	if buildResult == "" {
		// #160: fire-and-forget summary regeneration (see enqueueSummary).
		s.enqueueSummary(d)

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
		// hatch as commitOrRollbackOnEmit's build gate).
		if sigStable && os.Getenv("DEFN_STRICT_BUILD") != "1" {
			if os.Getenv("DEFN_MEASURE_TIMING") == "1" {
				fmt.Fprintf(os.Stderr, "  [edit] resolve deferred (sig-stable; run code(op:\"sync\") to refresh D's outgoing refs)\n")
			}
		} else {
			s.autoResolveFile(d.SourceFile, s.modulePath(d.ModuleID))
		}
	}

	var sb strings.Builder
	if buildResult != "" {
		// commitOrRollbackOnBuild/commitOrRollbackOnEmit's contract: any
		// non-empty result means the whole transaction was rolled back --
		// the id above was never durable. Saying "Updated X (id=N)" here,
		// even followed by a build-failure dump, reads as "it saved, but
		// something else is also broken" rather than "nothing was saved."
		// Same misleading-message bug handleCreate already got fixed for;
		// a real trajectory hit this exact shape here too (three
		// sequential edits, each showing "Updated X ... BUILD FAILED",
		// while chasing a signature change across call sites).
		sb.WriteString(fmt.Sprintf("edit %s%s rolled back — nothing was saved\n\n%s%s", recv, d.Name, buildResult, s.coupledChangeHint(d.ID)))
	} else {
		sb.WriteString(fmt.Sprintf("Updated %s%s (id=%d, hash=%s)\n", recv, d.Name, id, store.HashBody(args.NewBody)[:12]))
	}

	// Impact nudge: show callers if this definition has any. Only on
	// success -- a rolled-back edit's id may no longer exist.
	if buildResult == "" {
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
// and the shared connection's session state gets corrupted under the
// race (rename UPDATE silently discarded, ingest re-inserts stale defs).
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
	// (1 GiB) to keep the process's idle heap small, but enforcing that
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
		if isStaleProjectDirError(err) {
			return fmt.Errorf("ingest: %w\n\n%s no longer resolves inside a Go module -- "+
				"if this project was moved or renamed since 'defn serve' started, run 'defn restart' "+
				"to pick up the new location", err, s.projectDir)
		}
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
	return s.emitOnlyAgainst(s.backend, opts)
}

// emitOnlyAgainst is autoEmitOnlyWithOpts generalized to accept the
// store.Backend to emit against, mirroring emitAndBuildAgainst (#12).
func (s *server) emitOnlyAgainst(backend store.Backend, opts emit.Opts) string {
	if os.Getenv("DEFN_STRICT_BUILD") == "1" {
		return s.emitAndBuildAgainst(backend, opts)
	}
	if s.projectDir == "" || os.Getenv("DEFN_LEGACY") == "1" {
		return ""
	}
	timing := os.Getenv("DEFN_MEASURE_TIMING") == "1"

	t := time.Now()
	warnings, err := emit.EmitWithOpts(backend, s.projectDir, opts)
	if err != nil {
		return fmt.Sprintf("emit error: %v", err)
	}
	if timing {
		fmt.Fprintf(os.Stderr, "  [emit] emit.EmitWithOpts (build deferred): %s\n", time.Since(t).Round(time.Millisecond))
	}
	// #218: a warning here means the DB write succeeded but one or more
	// requested changes never made it to disk -- surface it prominently
	// rather than reporting silent success.
	if len(warnings) > 0 {
		return "WARNING: " + strings.Join(warnings, "\nWARNING: ")
	}
	return ""
}

// autoEmitAndBuildWithOpts is autoEmitAndBuild with caller-supplied
// emit.Opts. Used by handleDelete to whitelist the deleted decl through
// emit.safeWriteGoFile so the intentional removal isn't blocked by the
// data-loss safety net. Without this, the delete lands in the DB but
// never in the file — the watcher then re-ingests the "resurrected" def
// on the next tick. See project_defn_watch_delete_race memory.
//
// Return value is a status string appended to the tool response.
// Success paths return "" — silence is the signal. Failure paths return
// human-readable error strings the model must react to. #218: a
// WARNING-prefixed line means the DB write succeeded but the file write
// was refused or silently skipped one or more requested changes --
// treat this the same as a failure, not as success with a footnote.
func (s *server) autoEmitAndBuildWithOpts(opts emit.Opts) string {
	return s.emitAndBuildAgainst(s.backend, opts)
}

// emitAndBuildAgainst is autoEmitAndBuildWithOpts generalized to accept
// the store.Backend to emit against. #12: callers that want the build
// gate to actually protect their DB write pass a Begin()-scoped tx
// (which sees the batch's own uncommitted writes, the same way
// handleApply's existing tx already relies on for cross-op
// dependencies) instead of s.backend, and only commit it after this
// returns a clean (empty) result. autoEmitAndBuildWithOpts above is the
// legacy/unprotected shape, kept for callers that don't (yet) wrap
// their write in a transaction.
func (s *server) emitAndBuildAgainst(backend store.Backend, opts emit.Opts) string {
	if s.projectDir == "" || os.Getenv("DEFN_LEGACY") == "1" {
		return ""
	}
	timing := os.Getenv("DEFN_MEASURE_TIMING") == "1"

	// Emit to the actual project directory — keeps files in sync.
	t := time.Now()
	warnings, err := emit.EmitWithOpts(backend, s.projectDir, opts)
	if err != nil {
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
	// broad changes. runScopedBuild further scopes each touched file's
	// build to its NEAREST go.mod -- see its doc for why.
	out, buildErr := s.runScopedBuild(ctx, opts.TouchedFiles)
	if timing {
		fmt.Fprintf(os.Stderr, "  [emit] go build: %s\n", time.Since(t).Round(time.Millisecond))
	}
	var sb strings.Builder
	if len(warnings) > 0 {
		sb.WriteString("WARNING: " + strings.Join(warnings, "\nWARNING: "))
	}
	if buildErr != nil {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(fmt.Sprintf("BUILD FAILED:\n%s", out))
		sb.WriteString(s.suggestMissingImportFixes(out))
	}
	return sb.String()
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
	// #219/gemot dispatch: same disambiguation gap handleEdit had --
	// receiver/module/file were available on this handler's own
	// codeParam but never consulted, so GetDefinitionByName's blast-
	// radius tiebreak could silently target the wrong same-named def.
	d, err := s.resolveWriteTarget(args.Name, args.Receiver, args.Module, args.File)
	if err != nil {
		if args.Receiver != "" {
			return s.notFoundOrErr(fmt.Sprintf("%s.%s", args.Receiver, args.Name), err)
		}
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

	// Same multi-decl guard as handleEdit -- see its comment for the full
	// story (a real trajectory hit this via new_body; a fragment
	// replacement that inserts a whole extra declaration can hit it too).
	if n := countTopLevelDecls(newBody); n > 1 {
		return errResult(fmt.Errorf("edit %s%s: the fragment replacement produces %d top-level declarations — op:\"edit\" changes ONE definition's body; batch multiple changes with op:\"apply\"", formatReceiver(d.Receiver), d.Name, n))
	}

	if args.DryRun {
		return textResult(fmt.Sprintf("Dry run — would edit %s:\n\n--- old ---\n%s\n\n+++ new ---\n%s", args.Name, args.OldFragment, args.NewFragment)), nil, nil
	}

	d.Body = newBody
	d.Signature = extractSignature(newBody)
	recv := formatReceiver(d.Receiver)

	// #12: write and build-gate through a transaction so a build
	// failure leaves neither the DB nor the file changed. Previously
	// this wrote straight to s.backend and a later build failure was
	// informational only -- missed when #12 fixed handleEdit, since
	// this is a separate handler for the old_fragment/new_fragment
	// shape and never funnels through handleEdit.
	tx, commit, rollback, txErr := s.backend.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback()

	id, err := tx.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}
	d.ID = id

	var opts emit.Opts
	if d.SourceFile != "" {
		opts = emit.Opts{GoimportsFiles: []string{d.SourceFile}, TouchedFiles: []string{d.SourceFile}}
	}
	buildResult := s.commitOrRollbackOnBuild(tx, commit, rollback, opts)

	var sb strings.Builder
	if buildResult != "" {
		// Same misleading-message fix as handleEdit -- see its comment
		// for the full rationale. commitOrRollbackOnBuild's contract:
		// non-empty means the whole transaction was rolled back.
		sb.WriteString(fmt.Sprintf("edit %s%s rolled back — nothing was saved\n\n%s%s", recv, d.Name, buildResult, s.coupledChangeHint(d.ID)))
	} else {
		replaced := "1 occurrence"
		if args.ReplaceAll {
			replaced = fmt.Sprintf("%d occurrences", count)
		}
		sb.WriteString(fmt.Sprintf("Edited %s%s — replaced %s\n", recv, d.Name, replaced))
	}

	if buildResult == "" {
		s.autoResolveFile(d.SourceFile, s.modulePath(d.ModuleID))
		if impact, err := s.backend.GetImpact(id); err == nil && len(impact.DirectCallers) > 0 {
			prodCallers := 0
			for _, c := range impact.DirectCallers {
				if !c.Test {
					prodCallers++
				}
			}
			sb.WriteString(fmt.Sprintf("\nFYI: %d callers, %d tests affected.\n", prodCallers, len(impact.Tests)))
		}
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) handleInsert(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.resolveWriteTarget(args.Name, args.Receiver, args.Module, args.File)
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

	// Scaffold-file case: body is `package X` + imports (or comments)
	// with no user-defined decls yet. Route to the file_sources-only
	// path so callers can seed a new file before adding decls to it.
	// Requires file: — without it there's no target to write.
	if isImportsOnlyBody(args.Body) {
		if args.File == "" {
			return errResult(fmt.Errorf("body has no top-level declarations (imports only) — pass file: to scaffold a new file, or add a func/type/const/var body"))
		}
		return s.handleCreateScaffoldFile(args)
	}

	// Infer name, kind, and test flag from the body.
	name, kind, receiver, isTest := s.inferFromBody(args.Body)
	if name == "" {
		return errResult(fmt.Errorf("couldn't infer definition name from body — make sure it starts with func/type/const/var"))
	}

	// Find module: file: param wins (most specific), then module:, then first.
	var mod *store.Module
	fileResolvedDirectly := false
	if args.File != "" {
		mod = s.findModuleByFile(args.File)
		fileResolvedDirectly = mod != nil
	}
	if mod == nil && args.Module != "" {
		mod = s.findModule(args.Module)
		if mod == nil {
			return errResult(fmt.Errorf("module %q not found", args.Module))
		}
	}
	if mod == nil && args.File != "" {
		return errResult(fmt.Errorf("file %q does not map to any known module — run defn ingest first, or pass module: explicitly", args.File))
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

	// #12: write and build-gate through a transaction so a build
	// failure leaves neither the DB nor the file changed. Previously
	// this wrote straight to s.backend (durable immediately) and
	// treated a later build failure as informational only.
	//
	// #239: Begin() now runs before the "new directory" EnsureModule
	// call below (moved up from just before UpsertDefinition) and that
	// call now goes through tx, not s.backend. EnsureModule used to
	// commit the new module row directly to the backend, outside any
	// transaction — if the build below then failed and rolled back the
	// definition, the module row it belonged to was never rolled back
	// with it. That orphaned, forever-zero-def module row survived every
	// later emit; emitModule's zero-defs cleanup then guessed a filename
	// from it and deleted whatever real file already happened to live at
	// that path, e.g. a `create` attempt against grpc-go's
	// resolver/passthrough that failed to build left behind an empty
	// module row for resolver/passthrough, and the next unscoped emit
	// deleted the real, pre-existing passthrough.go.
	tx, commit, rollback, txErr := s.backend.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback()

	// #13: file: named a directory with no existing module -- this is
	// genuinely new territory, not the same package as whatever module:
	// fallback we just resolved above. Reusing that fallback module's
	// Name as the package clause is wrong whenever it differs from what
	// this new directory's package should actually be named (directory
	// = package boundary in Go): scaffolding into a brand-new
	// subdirectory of a "package main" module would emit package main
	// there too, with no func main() of its own, which can never build.
	// Ensure a directory-scoped module instead of borrowing the
	// fallback's identity.
	if !fileResolvedDirectly && args.File != "" {
		if dir := filepath.ToSlash(filepath.Dir(args.File)); dir != "" && dir != "." {
			newPath := dir
			// Prefer the real filesystem go.mod nearest this new
			// directory (correct even when it's inside a nested
			// module, e.g. etcd's server/, tests/, etcdctl/ each
			// have their own go.mod) over the DB-derived common-prefix
			// guess below, which is only right for single-module repos.
			absDir := filepath.Join(s.projectDir, dir)
			if modPrefix, modDir, mErr := ingest.ModuleForDir(absDir); mErr == nil {
				if relPkgDir, rErr := filepath.Rel(modDir, absDir); rErr == nil {
					if relPkgDir == "." {
						newPath = modPrefix
					} else {
						newPath = modPrefix + "/" + filepath.ToSlash(relPkgDir)
					}
				}
			} else {
				mods, _ := s.backend.ListModules()
				if root := emit.DetectModuleRoot(mods); root != "" {
					newPath = root + "/" + dir
				}
			}
			newMod, ensureErr := tx.EnsureModule(newPath, filepath.Base(dir), "")
			if ensureErr != nil {
				return errResult(fmt.Errorf("create module for new directory %q: %w", dir, ensureErr))
			}
			mod = newMod
		}
	}

	// Check if a definition with this name AND receiver already exists in
	// the target module. #220: a bare GetDefinitionByName ignores receiver
	// and falls back to a blast-radius tiebreak, so creating (*Agent).Foo
	// falsely collided with an unrelated (*LLM).Foo. Try both with and
	// without a leading "*" since inferFromBody's parsed receiver form may
	// not match how the DB stored an existing method's receiver.
	existing, existErr := s.backend.GetDefinitionByNameAndReceiver(name, mod.Path, receiver)
	if existErr != nil && receiver != "" {
		if alt := strings.TrimPrefix(receiver, "*"); alt != receiver {
			existing, existErr = s.backend.GetDefinitionByNameAndReceiver(name, mod.Path, alt)
		} else {
			existing, existErr = s.backend.GetDefinitionByNameAndReceiver(name, mod.Path, "*"+receiver)
		}
	}
	if existErr == nil {
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

	id, err := tx.UpsertDefinition(d)
	if err != nil {
		return errResult(err)
	}
	d.ID = id

	var opts emit.Opts
	if args.File != "" {
		opts = emit.Opts{
			GoimportsFiles: []string{args.File},
			TouchedFiles:   []string{args.File},
			AllowedAdds:    []string{emit.FuncIdentity(name, receiver)},
		}
	}
	buildResult := s.commitOrRollbackOnBuild(tx, commit, rollback, opts)
	if buildResult == "" {
		s.enqueueSummary(d)
		s.autoResolveFile(args.File, mod.Path)
	}

	var sb strings.Builder
	loc := mod.Path
	if args.File != "" {
		loc = args.File + " (" + mod.Path + ")"
	}
	if buildResult != "" {
		// commitOrRollbackOnBuild's contract: any non-empty result means
		// the whole transaction (including this UpsertDefinition) was
		// rolled back -- the id above was never durable. Saying
		// "Created X (id=N)" here, even followed by a build-failure
		// dump, reads as "it saved, but something else is also broken"
		// rather than "nothing was saved." That misled a real
		// trajectory: three sequential creates for three different
		// receivers all build-failed and rolled back except the last,
		// but each response said "Created ... (id=N)" -- the agent
		// concluded defn had a graph bug reusing the same id across
		// unrelated defs, and burned ~10 calls on a fix for a collision
		// that was never real.
		recv := formatReceiver(receiver)
		fmt.Fprintf(&sb, "create %s%s rolled back — nothing was saved\n\n%s", recv, name, buildResult)
	} else {
		sb.WriteString(fmt.Sprintf("Created %s (id=%d, kind=%s) in %s\n", name, id, kind, loc))
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

func (s *server) handleCreateMultiDecl(args createParam) (*sdkmcp.CallToolResult, any, error) {
	decls, err := sliceDecls(args.Body)
	if err != nil {
		return errResult(fmt.Errorf("multi-decl parse: %v", err))
	}

	mod := s.findModuleByFile(args.File)
	fileResolvedDirectly := mod != nil
	if mod == nil && args.Module != "" {
		mod = s.findModule(args.Module)
		if mod == nil {
			return errResult(fmt.Errorf("module %q not found", args.Module))
		}
	}

	tx, commit, rollback, txErr := s.backend.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback()

	// New-package case: file: points at a directory not yet ingested and
	// no module: was given to disambiguate. Mirror handleCreate's #13 fix
	// -- create a module genuinely scoped to the new directory. The prior
	// behavior fell back to whichever EXISTING module happened to have
	// the shortest registered path (e.g. this repo's own db/ module) and
	// silently attributed every def in the new file to it -- wrong module
	// association even with zero name collisions, and a false "already
	// exists" error against that unrelated package's local helpers
	// (testDB, declKey, ...) when a name did collide. Authoring a
	// brand-new file in a brand-new package is this op's primary use
	// case (the whole-file "new file with multiple functions" pattern),
	// so this path must work, not just error out.
	if !fileResolvedDirectly {
		dir := filepath.ToSlash(filepath.Dir(args.File))
		newPath := dir
		if dir != "" && dir != "." {
			mods, _ := s.backend.ListModules()
			if root := emit.DetectModuleRoot(mods); root != "" {
				newPath = root + "/" + dir
			}
		}
		newMod, ensureErr := tx.EnsureModule(newPath, filepath.Base(dir), "")
		if ensureErr != nil {
			return errResult(fmt.Errorf("create module for new directory %q: %w", dir, ensureErr))
		}
		mod = newMod
	}

	// Pre-check: no name+receiver collides with an existing def in the
	// target module. Same #220 receiver-disambiguation gap handleCreate
	// already closed for its single-decl path -- a bare GetDefinitionByName
	// ignores receiver and falls back to a blast-radius tiebreak, so
	// creating (*Baz).Bar alongside an unrelated (*Foo).Bar in the same
	// package would misreport a collision instead of correctly allowing
	// two distinct receivers.
	for _, d := range decls {
		existing, existErr := s.backend.GetDefinitionByNameAndReceiver(d.Name, mod.Path, d.Receiver)
		if existErr != nil && d.Receiver != "" {
			if alt := strings.TrimPrefix(d.Receiver, "*"); alt != d.Receiver {
				existing, existErr = s.backend.GetDefinitionByNameAndReceiver(d.Name, mod.Path, alt)
			} else {
				existing, existErr = s.backend.GetDefinitionByNameAndReceiver(d.Name, mod.Path, "*"+d.Receiver)
			}
		}
		if existErr == nil {
			recv := formatReceiver(existing.Receiver)
			return errResult(fmt.Errorf("definition %s%s already exists in %s (id=%d) — use code(op:\"edit\") to modify it", recv, d.Name, mod.Path, existing.ID))
		}
	}

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
		id, err := tx.UpsertDefinition(def)
		if err != nil {
			return errResult(fmt.Errorf("upsert %s: %v", d.Name, err))
		}
		def.ID = id
		s.enqueueSummary(def)
		ids = append(ids, id)
	}

	if err := commit(); err != nil {
		return errResult(fmt.Errorf("commit: %v", err))
	}

	addNames := make([]string, len(decls))
	for i, d := range decls {
		addNames[i] = emit.FuncIdentity(d.Name, d.Receiver)
	}
	buildResult := s.autoEmitAndBuildForCreate(args.File, addNames)
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
					if op.File == "" {
						errors = append(errors, fmt.Sprintf("create: body has %d top-level decls — set file: to author a whole file in one call, or split into %d create ops", n, n))
						continue
					}
					decls, declErr := sliceDecls(op.Body)
					if declErr != nil {
						errors = append(errors, fmt.Sprintf("create: multi-decl parse: %v", declErr))
						continue
					}
					for _, d := range decls {
						sb.WriteString(fmt.Sprintf("+ would create %s (%s) in %s\n", d.Name, d.Kind, op.File))
					}
					continue
				}
				name, kind, _, _ := s.inferFromBody(op.Body)
				if name == "" {
					errors = append(errors, "create: couldn't infer name from body")
				} else {
					sb.WriteString(fmt.Sprintf("+ would create %s (%s)\n", name, kind))
				}
			case "edit":
				if _, err := s.resolveApplyTarget(s.backend, op.Name, op.Receiver, op.Module, op.File); err != nil {
					errors = append(errors, fmt.Sprintf("edit %s: not found", op.Name))
				} else {
					sb.WriteString(fmt.Sprintf("~ would edit %s\n", op.Name))
				}
			case "delete":
				if _, err := s.resolveApplyTarget(s.backend, op.Name, op.Receiver, op.Module, op.File); err != nil {
					errors = append(errors, fmt.Sprintf("delete %s: not found", op.Name))
				} else {
					sb.WriteString(fmt.Sprintf("- would delete %s\n", op.Name))
				}
			case "rename":
				if op.Name == "" || op.NewName == "" {
					errors = append(errors, "rename: both name and new_name are required")
				} else if _, err := s.resolveApplyTarget(s.backend, op.Name, op.Receiver, op.Module, op.File); err != nil {
					errors = append(errors, fmt.Sprintf("rename %s: not found", op.Name))
				} else {
					sb.WriteString(fmt.Sprintf("→ would rename %s → %s\n", op.Name, op.NewName))
				}
			case "insert-precondition", "replace-slice", "replace-hunk", "wrap-in-defer", "rename-param":
				name := op.Name
				if name == "" {
					if inferred, err := s.inferSingleTargetName(s.backend); err != nil {
						errors = append(errors, fmt.Sprintf("%s: %v", op.Op, err))
						continue
					} else {
						name = inferred
					}
				}
				if _, err := s.resolveApplyTarget(s.backend, name, op.Receiver, op.Module, op.File); err != nil {
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

	// #214: tx is the transaction-scoped Backend view Begin() hands back.
	// Every read and write for this batch MUST go through tx, not
	// s.backend directly -- s.backend would auto-commit immediately via
	// the pool and also would not see this batch's own uncommitted writes,
	// breaking both rollback-on-failure and any op-to-op dependency within
	// the same batch (e.g. op2 editing a def op1 just created).
	tx, commit, rollback, txErr := s.backend.Begin()
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
	var allowedAdds []string
	// #241: IDs of defs edited in this batch, so a rolled-back build can
	// point at their callers via coupledChangeHint -- same rationale as
	// handleEdit's singleton path, just collected across the batch since
	// there's no single "the def just edited" here.
	var editedIDs []int64
	// #233: add-import's disk write can't go through mergeDeclsIntoSource
	// (it never touches import blocks) -- queued here, applied via
	// patchImportOnDisk after commit succeeds, mirroring how every other
	// op's disk write is deferred to the tail scoped emit.
	type pendingImport struct {
		moduleID                int64
		file, importPath, alias string
	}
	var pendingImports []pendingImport
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
			inferred, err := s.inferSingleTargetName(tx)
			if err != nil {
				return "", fmt.Sprintf("%s: %v", op.Op, err)
			}
			name = inferred
		}
		d, err := s.resolveApplyTarget(tx, name, op.Receiver, op.Module, op.File)
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
		// #222: same identity-preserving guard as the "edit" case above —
		// see its comment for the full mergeDeclsIntoSource/safeWriteGoFile
		// rationale.
		if newName, _, newReceiver, _ := s.inferFromBody(newBody); newName != "" && (newName != d.Name || newReceiver != d.Receiver) {
			return "", fmt.Sprintf("%s %s: new_body declares %s%s, which changes its name/receiver — use op:\"rename\" instead; this op only changes body content", op.Op, name, formatReceiver(newReceiver), newName)
		}
		d.Body = newBody
		d.Signature = extractSignature(newBody)
		if _, err := tx.UpsertDefinition(d); err != nil {
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
				if op.File == "" {
					errors = append(errors, fmt.Sprintf("create: body has %d top-level decls — set file: to author a whole file in one call, or split into %d create ops", n, n))
					continue
				}
				decls, declErr := sliceDecls(op.Body)
				if declErr != nil {
					errors = append(errors, fmt.Sprintf("create: multi-decl parse: %v", declErr))
					continue
				}
				mod := s.findModuleByFile(op.File)
				if mod == nil && op.Module != "" {
					mod = s.findModule(op.Module)
					if mod == nil {
						errors = append(errors, fmt.Sprintf("create: module %q not found", op.Module))
						continue
					}
				}
				if mod == nil {
					// New-package case, same #13-style fix as
					// handleCreate/handleCreateMultiDecl: create a module
					// scoped to the new directory instead of falling back
					// to whichever existing module has the shortest
					// registered path -- an arbitrary, unrelated package.
					// See handleCreateMultiDecl's identical fix for the
					// full story: this exact pattern silently
					// mis-attributed a brand-new package's defs to this
					// repo's own db/ module.
					dir := filepath.ToSlash(filepath.Dir(op.File))
					newPath := dir
					if dir != "" && dir != "." {
						mods, _ := s.backend.ListModules()
						if root := emit.DetectModuleRoot(mods); root != "" {
							newPath = root + "/" + dir
						}
					}
					newMod, ensureErr := tx.EnsureModule(newPath, filepath.Base(dir), "")
					if ensureErr != nil {
						errors = append(errors, fmt.Sprintf("create: create module for new directory %q: %v", dir, ensureErr))
						continue
					}
					mod = newMod
				}
				collided := false
				for _, d := range decls {
					existing, existErr := tx.GetDefinitionByNameAndReceiver(d.Name, mod.Path, d.Receiver)
					if existErr != nil && d.Receiver != "" {
						if alt := strings.TrimPrefix(d.Receiver, "*"); alt != d.Receiver {
							existing, existErr = tx.GetDefinitionByNameAndReceiver(d.Name, mod.Path, alt)
						} else {
							existing, existErr = tx.GetDefinitionByNameAndReceiver(d.Name, mod.Path, "*"+d.Receiver)
						}
					}
					if existErr == nil {
						recv := formatReceiver(existing.Receiver)
						errors = append(errors, fmt.Sprintf("create %s%s: already exists in %s (id=%d)", recv, d.Name, mod.Path, existing.ID))
						collided = true
						break
					}
				}
				if collided {
					continue
				}
				for _, d := range decls {
					exported := len(d.Name) > 0 && d.Name[0] >= 'A' && d.Name[0] <= 'Z'
					def := &store.Definition{
						ModuleID: mod.ID, Name: d.Name, Kind: d.Kind, Exported: exported,
						Test: d.IsTest, Receiver: d.Receiver, Signature: extractSignature(d.Body), Body: d.Body,
						SourceFile: op.File,
					}
					id, err := tx.UpsertDefinition(def)
					if err != nil {
						errors = append(errors, fmt.Sprintf("create %s: %v", d.Name, err))
						continue
					}
					def.ID = id
					s.enqueueSummary(def)
					addTouched(op.File)
					addResolve(op.File, mod.ID)
					allowedAdds = append(allowedAdds, emit.FuncIdentity(d.Name, d.Receiver))
					sb.WriteString(fmt.Sprintf("+ created %s (id=%d)\n", d.Name, id))
				}
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
			// Same same-module name+receiver collision check as
			// handleCreate and this same handler's multi-decl branch a
			// few lines above -- without it, a collision here (the
			// common case: most create ops in a batch are single
			// declarations) only failed at the Go-build stage with a
			// raw compiler error ("X redeclared in this block"), rolling
			// back the whole batch, instead of defn's own clear message.
			existing, existErr := tx.GetDefinitionByNameAndReceiver(name, mod.Path, receiver)
			if existErr != nil && receiver != "" {
				if alt := strings.TrimPrefix(receiver, "*"); alt != receiver {
					existing, existErr = tx.GetDefinitionByNameAndReceiver(name, mod.Path, alt)
				} else {
					existing, existErr = tx.GetDefinitionByNameAndReceiver(name, mod.Path, "*"+receiver)
				}
			}
			if existErr == nil {
				recv := formatReceiver(existing.Receiver)
				errors = append(errors, fmt.Sprintf("create %s%s: already exists in %s (id=%d)", recv, name, mod.Path, existing.ID))
				continue
			}
			exported := len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
			d := &store.Definition{
				ModuleID: mod.ID, Name: name, Kind: kind, Exported: exported,
				Test: isTest, Receiver: receiver, Signature: extractSignature(op.Body), Body: op.Body,
				SourceFile: op.File,
			}
			id, err := tx.UpsertDefinition(d)
			if err != nil {
				errors = append(errors, fmt.Sprintf("create %s: %v", name, err))
			} else {
				d.ID = id
				s.enqueueSummary(d)
				addTouched(op.File)
				addResolve(op.File, mod.ID)
				allowedAdds = append(allowedAdds, emit.FuncIdentity(name, receiver))
				sb.WriteString(fmt.Sprintf("+ created %s (id=%d)\n", name, id))
			}

		case "edit":
			d, err := s.resolveApplyTarget(tx, op.Name, op.Receiver, op.Module, op.File)
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
			// Same multi-decl guard as handleEdit's standalone path -- see its
			// comment for the full story.
			if n := countTopLevelDecls(d.Body); n > 1 {
				errors = append(errors, fmt.Sprintf("edit %s: new_body has %d top-level declarations — op:\"edit\" changes ONE definition's body; add separate edit/create ops to this same apply batch instead", op.Name, n))
				continue
			}
			// #222: edit must preserve identity -- see handleEdit's identical
			// check for the full rationale. A body that renames the decl
			// without going through op:"rename" leaves d.Name stale, which
			// makes mergeDeclsIntoSource splice a differently-named body
			// under the old key; the merged file then has no decl under
			// that old name, tripping safeWriteGoFile's on-disk-decl-loss
			// check and blocking the write for the WHOLE file -- including
			// any other op batched alongside this edit.
			if newName, _, newReceiver, _ := s.inferFromBody(d.Body); newName != "" && (newName != d.Name || newReceiver != d.Receiver) {
				errors = append(errors, fmt.Sprintf("edit %s: new_body declares %s%s, which changes its name/receiver — use op:\"rename\" instead; op:\"edit\" only changes body content", op.Name, formatReceiver(newReceiver), newName))
				continue
			}
			d.Signature = extractSignature(d.Body)
			if _, err := tx.UpsertDefinition(d); err != nil {
				errors = append(errors, fmt.Sprintf("edit %s: %v", op.Name, err))
			} else {
				addTouched(d.SourceFile)
				addResolve(d.SourceFile, d.ModuleID)
				editedIDs = append(editedIDs, d.ID)
				sb.WriteString(fmt.Sprintf("~ edited %s\n", op.Name))
			}

		case "delete":
			d, err := s.resolveApplyTarget(tx, op.Name, op.Receiver, op.Module, op.File)
			if err != nil {
				errors = append(errors, fmt.Sprintf("delete %s: not found", op.Name))
				continue
			}
			if err := tx.DeleteDefinition(d.ID); err != nil {
				errors = append(errors, fmt.Sprintf("delete %s: %v", op.Name, err))
			} else {
				addTouched(d.SourceFile)
				// #109 rationale: DeleteDefinition already dropped every refs
				// row where from_def=D OR to_def=D — no resolve needed.
				qualified := emit.FuncIdentity(d.Name, d.Receiver)
				allowedRemovals = append(allowedRemovals, qualified)
				sb.WriteString(fmt.Sprintf("- deleted %s\n", op.Name))
			}

		case "rename":
			if op.Name == "" || op.NewName == "" {
				errors = append(errors, "rename: both name and new_name are required")
				continue
			}
			d, err := s.resolveApplyTarget(tx, op.Name, op.Receiver, op.Module, op.File)
			if err != nil {
				errors = append(errors, fmt.Sprintf("rename %s: not found", op.Name))
				continue
			}
			// Reserve the qualified pre-rename name so safeWriteGoFile lets
			// the disappearing decl actually vanish from the file (same as
			// handleRename's qualifiedOld).
			qualifiedOld := emit.FuncIdentity(d.Name, d.Receiver)
			allowedRemovals = append(allowedRemovals, qualifiedOld)
			addTouched(d.SourceFile)
			newBody, _ := astRename(d.Body, op.Name, op.NewName)
			newSig := extractSignature(newBody)
			exported := len(op.NewName) > 0 && op.NewName[0] >= 'A' && op.NewName[0] <= 'Z'
			// RenameDefinition updates BY ID, preserving row identity --
			// mirrors handleRename's own fix (see its comment for the full
			// story). Mutating d.Name and calling UpsertDefinition here
			// instead looks up by (module,name,kind,receiver,test), which no
			// longer matches once the name changes: it INSERTS a new row
			// under the new name and leaves the old-named row orphaned in
			// the DB, so both the old and new names exist simultaneously.
			// mergeDeclsIntoSource then wants to write the orphaned old-name
			// row too (already spliced out via allowedRemovals, so it can't
			// match anything on disk), surfacing as a false "database and
			// disk have diverged" warning that trips #218's whole-batch
			// rollback on an otherwise perfectly valid rename+edit combo.
			if err := tx.RenameDefinition(d.ID, op.NewName, newBody, newSig, exported); err != nil {
				errors = append(errors, fmt.Sprintf("rename %s: %v", op.Name, err))
				continue
			}
			d.Name = op.NewName
			d.Body = newBody
			d.Signature = newSig
			d.Exported = exported
			s.enqueueSummary(d)
			// #163: rename creates a new-named def from the emit path's POV.
			allowedAdds = append(allowedAdds, emit.FuncIdentity(op.NewName, d.Receiver))
			callers, _ := tx.GetCallers(d.ID)
			callerCount := 0
			for _, caller := range callers {
				if strings.Contains(caller.Body, op.Name) {
					caller.Body, _ = astRename(caller.Body, op.Name, op.NewName)
					caller.Signature = extractSignature(caller.Body)
					if _, err := tx.UpsertDefinition(&caller); err != nil {
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
			// Same old_fragment/new_fragment alias as handleCode's
			// validation -- see its comment for the full rationale.
			oldText, newText := op.Old, op.New
			if oldText == "" && op.OldFragment != "" {
				oldText = op.OldFragment
			}
			if newText == "" && op.NewFragment != "" {
				newText = op.NewFragment
			}
			line, errStr := projEdit(op, func(body string) (string, error) {
				return projection.ReplaceHunk(body, oldText, newText, op.Index)
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
				all, err := tx.DistinctSourceFiles()
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
			// #233: dir must be "" for a root-level file (no "/"), not
			// the file name itself -- FindDefinitionsByFile matches dir
			// against module.path, and "main.go" never matches a real
			// module path. Mirrors handleAddImport's #221 fix.
			dir := ""
			if idx := strings.LastIndex(file, "/"); idx >= 0 {
				dir = file[:idx]
			}
			defs, err := tx.FindDefinitionsByFile(dir, file, 0)
			if err != nil || len(defs) == 0 {
				errors = append(errors, fmt.Sprintf("add-import: no defs in %q", file))
				continue
			}
			moduleID := defs[0].ModuleID
			existing, err := tx.GetImports(moduleID)
			if err != nil {
				errors = append(errors, fmt.Sprintf("add-import: read imports: %v", err))
				continue
			}
			alreadyInDB := false
			for _, imp := range existing {
				if imp.ImportedPath == op.ImportPath && imp.Alias == op.Alias {
					alreadyInDB = true
					break
				}
			}
			if !alreadyInDB {
				updated := append(existing, store.Import{ModuleID: moduleID, ImportedPath: op.ImportPath, Alias: op.Alias})
				if err := tx.SetImports(moduleID, updated); err != nil {
					errors = append(errors, fmt.Sprintf("add-import %q: %v", op.ImportPath, err))
					continue
				}
			}
			// #233: the DB's per-module imports table isn't what lands this
			// on disk -- mergeDeclsIntoSource never touches import blocks
			// (#221), so the real write must go through patchImportOnDisk,
			// applied directly to the file. That can't happen until the
			// transaction commits, so queue it and apply after commit.
			//
			// Deliberately NOT addTouched(file): patchImportOnDisk already
			// produces goimports-canonical grouping on its own (same as
			// the singleton handleAddImport, which also skips emit
			// entirely for this reason). Marking the file touched would
			// pull it into the tail's scoped goimports pass, which
			// legitimately strips imports with no usage yet in the file --
			// exactly what a bare add-import (not yet paired with code
			// that references it) always is. If another op in this same
			// batch also edits this file's body, that op's own addTouched
			// already covers it independently.
			pendingImports = append(pendingImports, pendingImport{moduleID: moduleID, file: file, importPath: op.ImportPath, alias: op.Alias})

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
	// #114 batch scoping: if we tracked any touched files, run one scoped
	// emit + goimports + per-file resolve at the tail. Safety valve — if
	// tracking came up empty AND there were no pending add-import disk
	// patches (edge case: every op had empty SourceFile), fall back to
	// full autoEmitAndBuild + autoResolve for correctness. #233: a batch
	// of ONLY add-import op(s) also leaves tracking empty by design (see
	// the add-import case above) — that must NOT hit either emit path,
	// scoped or unscoped: patchImportOnDisk already landed the change,
	// and running goimports over the file (scoped or project-wide) would
	// strip the import right back out if nothing uses it yet.
	//
	// #12: when the batch has no pending add-import disk patches, commit
	// is deferred until AFTER the build gate — commitOrRollbackOnBuild
	// only commits tx if emit+build comes back completely clean, so a
	// build failure now leaves NEITHER the DB nor the filesystem changed
	// (previously: commit ran unconditionally, then a build failure was
	// just a string in the response with no rollback). A batch that DOES
	// include add-import can't get this protection: patchImportOnDisk
	// writes directly to disk outside the emit/merge path and needs the
	// DB write durable first (pre-existing #233 asymmetry, not addressed
	// here — see #12's issue body for why).
	var buildResult string
	rolledBack := false
	switch {
	case len(pendingImports) == 0 && (len(touchedFiles) > 0 || len(allowedRemovals) > 0 || len(allowedAdds) > 0):
		goimportsFiles := make([]string, 0, len(touchedFiles))
		for f := range touchedFiles {
			goimportsFiles = append(goimportsFiles, f)
		}
		buildResult = s.commitOrRollbackOnBuild(tx, commit, rollback, emit.Opts{
			AllowedRemovals: allowedRemovals,
			AllowedAdds:     allowedAdds,
			GoimportsFiles:  goimportsFiles,
			TouchedFiles:    goimportsFiles,
		})
		if buildResult == "" {
			for fp := range resolveSet {
				s.autoResolveFile(fp.file, fp.module)
			}
		} else {
			// commitOrRollbackOnBuild's contract: any non-empty result
			// here means the WHOLE batch's transaction was rolled back,
			// including every "+ created"/"~ edited"/"- deleted" line
			// already written to sb during the per-op loop above. Same
			// misleading-message bug as handleCreate's single-op path
			// (see its fix), at batch scale: those lines read as
			// confirmed successes with no indication the whole thing
			// never landed.
			rolledBack = true
		}

	case len(pendingImports) == 0:
		// Nothing tracked at all (every op had empty SourceFile) — no
		// per-file snapshot is possible, so commit unconditionally and
		// fall back to the full-project emit+build, same as before #12.
		if err := commit(); err != nil {
			return errResult(fmt.Errorf("commit: %w", err))
		}
		buildResult = s.autoEmitAndBuild()
		s.autoResolve("")

	default:
		// add-import is present: commit unconditionally (its disk patch
		// needs the DB write durable first — #233), then apply the
		// pending imports, then still run the build for any OTHER
		// touched files in the same batch — unprotected, pre-existing
		// #233 asymmetry, unchanged by #12.
		if err := commit(); err != nil {
			return errResult(fmt.Errorf("commit: %w", err))
		}
		for _, pi := range pendingImports {
			changed, err := s.patchImportOnDisk(pi.moduleID, pi.file, pi.importPath, pi.alias)
			switch {
			case err != nil:
				sb.WriteString(fmt.Sprintf("WARNING: add-import %q on %s: %v\n", pi.importPath, pi.file, err))
			case changed:
				sb.WriteString(fmt.Sprintf("+ added import %q to %s\n", pi.importPath, pi.file))
			default:
				sb.WriteString(fmt.Sprintf("= import %q already present in %s\n", pi.importPath, pi.file))
			}
		}
		if len(touchedFiles) > 0 || len(allowedRemovals) > 0 || len(allowedAdds) > 0 {
			goimportsFiles := make([]string, 0, len(touchedFiles))
			for f := range touchedFiles {
				goimportsFiles = append(goimportsFiles, f)
			}
			buildResult = s.autoEmitAndBuildWithOpts(emit.Opts{
				AllowedRemovals: allowedRemovals,
				AllowedAdds:     allowedAdds,
				GoimportsFiles:  goimportsFiles,
				TouchedFiles:    goimportsFiles,
			})
			for fp := range resolveSet {
				s.autoResolveFile(fp.file, fp.module)
			}
		}
	}
	if buildResult != "" {
		if rolledBack {
			// Deliberately not including sb's per-op "+ created"/"~ edited"/
			// "- deleted" lines here: those record what was ATTEMPTED, but
			// this whole transaction was rolled back, so none of it landed.
			// Showing them alongside a "rolled back" banner still misled a
			// real trajectory that misread "+ created X (id=N)" as X
			// actually existing (see the sibling handleCreate fix for the
			// full story) -- the banner alone wasn't enough of a signal
			// against an explicit, itemized "+ created" line right below it.
			return textResult(fmt.Sprintf("apply rolled back — build failed, nothing was saved:\n\n%s%s", buildResult, s.coupledChangeHint(editedIDs...))), nil, nil
		}
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
	d, err := s.resolveWriteTarget(args.Name, args.Receiver, args.Module, args.File)
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

	if args.DryRun {
		return textResult(fmt.Sprintf("- would delete %s%s (id=%d)\n\n(dry run — no changes made)", recv, d.Name, d.ID)), nil, nil
	}

	// #12: delete + build-gate through a transaction so a build failure
	// leaves neither the DB nor the file changed. Previously
	// DeleteDefinition wrote straight to s.backend and a later build
	// failure was informational only.
	tx, commit, rollback, txErr := s.backend.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback()

	if err := tx.DeleteDefinition(d.ID); err != nil {
		return errResult(err)
	}

	// Whitelist the deleted decl through emit's safeWriteGoFile safety
	// net. topLevelDeclNames formats methods as "<Recv>.Name" (pointer
	// receivers unwrapped); match that. Without this, the file on disk
	// would be left unchanged and watchFiles would resurrect the def.
	qualified := emit.FuncIdentity(d.Name, d.Receiver)
	deleteOpts := emit.Opts{AllowedRemovals: []string{qualified}}
	if d.SourceFile != "" {
		deleteOpts.GoimportsFiles = []string{d.SourceFile}
		deleteOpts.TouchedFiles = []string{d.SourceFile}
	}
	// #12's build gate must not apply when force:true — that flag is an
	// explicit acknowledgment that this delete may leave dangling
	// references / a broken build (exactly what the safety check above
	// would otherwise refuse), so rolling it back on a build failure
	// the caller already opted into would defeat the point of force.
	// Commit unconditionally here, same as before #12.
	var buildResult string
	if args.Force {
		if err := commit(); err != nil {
			return errResult(fmt.Errorf("commit: %w", err))
		}
		buildResult = s.emitAndBuildAgainst(s.backend, deleteOpts)
	} else {
		buildResult = s.commitOrRollbackOnBuild(tx, commit, rollback, deleteOpts)
	}
	// #109 pass 2 (winze op-classification): skip autoResolve on delete.
	// DeleteDefinition already dropped every refs row where from_def=D
	// OR to_def=D (store.go:201), so both the def's own outgoing edges
	// and every caller's edge INTO D are gone. Caller bodies still name
	// D textually, but a full re-resolve would just walk those bodies,
	// fail to find D in the DB, and skip — no ref changes. Skipping
	// autoResolve removes the full-project ResolveModule walk on every
	// delete. force:true delete still applies (safe-delete's caller
	// check gates unforced deletes at zero-callers anyway).
	if buildResult == "" {
		if err := s.autoCommit(); err != nil {
			fmt.Fprintf(os.Stderr, "defn: auto-commit failed (post-delete): %v\n", err)
		}
		if s.idf != nil {
			s.idf.Invalidate()
		}
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
	// use of *sql.Conn, and under the race the shared connection's session
	// state gets corrupted. Waiting for ready serializes them.
	s.waitReady()

	// #248: was a raw bare-name lookup hardcoded to modulePath="" with no
	// receiver/module/file params on renameParam at all -- rename couldn't
	// be disambiguated even if the caller wanted to, the worst case of the
	// same silent-wrong-target bug fixed elsewhere via resolveWriteTarget.
	d, err := s.resolveWriteTarget(args.OldName, args.Receiver, args.Module, args.File)
	if err != nil {
		return s.notFoundOrErr(args.OldName, err)
	}

	// Compose the qualified old-name the safety net compares against (methods
	// use "<Recv>.Name", pointer receivers unwrapped). Reserve it BEFORE we
	// mutate d so the emit path knows this decl-name is *intentionally*
	// disappearing from the file — otherwise safeWriteGoFile refuses to drop
	// it and the merge appends the new name alongside the old one (bug fixed
	// for deletes in b274ccc; the same shape recurs for renames).
	qualifiedOld := emit.FuncIdentity(d.Name, d.Receiver)
	originalID := d.ID
	// astRename matches bare *ast.Ident nodes, but args.OldName may be the
	// receiver-qualified "(*T).method" form GetDefinitionByName also
	// accepts (and that method disambiguation naturally produces) -- that
	// never matches a real identifier, so capture the resolved bare name
	// once, before d.Name is mutated below, and use it for every AST-level
	// operation instead of the possibly-qualified args.OldName.
	oldBareName := d.Name

	// Update the definition name in its own body using AST rename.
	// Only renames identifiers — preserves comments and string literals.
	totalSkipped := 0
	newBody, _ := astRename(d.Body, oldBareName, args.NewName)
	newSig := extractSignature(newBody)
	exported := len(args.NewName) > 0 && args.NewName[0] >= 'A' && args.NewName[0] <= 'Z'

	// RenameDefinition updates BY ID so identity + refs edges are preserved.
	// Do NOT use UpsertDefinition here: it looks up by (module,name,kind,recv,test)
	// and would INSERT a new row for the new name, leaving the old row orphaned
	// in the DB and both defs in the emitted file.
	if err := s.backend.RenameDefinition(originalID, args.NewName, newBody, newSig, exported); err != nil {
		return errResult(err)
	}
	// #160: renamed def has new intent (name is a strong signal in the
	// summary prompt) — regenerate. Body/receiver/kind stay the same
	// otherwise; enqueue uses the post-rename shape.
	d.Name = args.NewName
	d.Body = newBody
	d.Signature = newSig
	d.Exported = exported
	s.enqueueSummary(d)

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
		if strings.Contains(caller.Body, oldBareName) {
			var skipped int
			caller.Body, skipped = astRename(caller.Body, oldBareName, args.NewName)
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
	// #163: rename = delete-old + create-new to the emit path. Declare
	// both so the merge can splice in-place (old name removed, new
	// name spliced) instead of leaving the new name behind as drift.
	buildResult := s.autoEmitOnlyWithOpts(emit.Opts{
		AllowedRemovals: []string{qualifiedOld},
		AllowedAdds:     []string{emit.FuncIdentity(args.NewName, d.Receiver)},
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

func (s *server) handleTestByName(_ context.Context, _ *sdkmcp.CallToolRequest, pattern, module, file string) (*sdkmcp.CallToolResult, any, error) {
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

	// store.Module is per go.mod, not per package -- a single-module repo
	// (the common case: go-zero, grpc-go, cli all have exactly one go.mod
	// at the root) has exactly one Module row covering every package, so
	// resolving module:/file: through findModule/findModuleByFile would
	// scope to the WHOLE repo regardless -- no better than the ./... this
	// is meant to replace. Match directly against source_file paths
	// instead (same approach as search's file: fix), which actually
	// distinguishes "core/logx" from every other package in the repo.
	hint := file
	if hint == "" {
		hint = module
	}
	ambiguityMsg := ""
	if hint == "" && testNamePattern.MatchString(pattern) {
		// No explicit scope, but pattern is very often the literal test
		// name being targeted (the documented, common case: reproduce an
		// issue's named failing test in one call). Real trajectory
		// (cli-2671): a pre-existing, unrelated compile error in a
		// sibling package (pkg/cmd/gist/create) made every whole-repo
		// `go test ./...` fail regardless of whether the agent's actual
		// edit -- in a completely different package -- was correct.
		// Scoping to the named test's own package sidesteps any sibling
		// package that isn't even imported by it. Best-effort tiebreak
		// (most production callers) when the name is ambiguous across
		// packages -- real trajectory (cli-5503): test:"TestNewCmdList"
		// silently ran against an unrelated same-named test in a sibling
		// package and reported PASS, with nothing verified in the package
		// actually edited. ambiguityNote discloses the tiebreak below
		// instead of staying silent about it, same precedent as #248's
		// read/outline fix.
		if d, err := s.backend.GetDefinitionByName(pattern, ""); err == nil && d != nil {
			hint = d.SourceFile
			ambiguityMsg = s.ambiguityNote(pattern, "", "", "")
		}
	}
	target := s.testScopeTarget(hint)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutFor(0, target))
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-run", pattern, "-count=1", "-v", target)
	cmd.Dir = s.projectDir
	out, err := cmd.CombinedOutput()

	outStr := string(out)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Running -run %q across %s:\n\n", pattern, target))
	sb.WriteString(truncateTestOutput(outStr))
	switch {
	case err != nil && testBuildFailed(outStr):
		sb.WriteString("\nBUILD FAILED -- the package did not compile; zero tests ran. This is NOT a test-failure signal, fix the compile error shown above first")
	case err != nil && testPanicked(outStr):
		sb.WriteString("\nTEST BINARY PANICKED -- not a normal assertion failure; likely caused by state unrelated to your edit (e.g. duplicate flag/command registration shared across tests in one binary). Investigate the panic trace above before assuming your edit is wrong")
	case ctx.Err() == context.DeadlineExceeded:
		sb.WriteString(fmt.Sprintf("\nTIMED OUT after %s -- this is NOT a pass; the run was killed before finishing. This may be a hang from your edit, or simply a large/slow test package -- set DEFN_TEST_TIMEOUT=<duration> (e.g. \"5m\") to allow more time before assuming a hang", testTimeout))
	case err != nil:
		sb.WriteString("\nSOME TESTS FAILED")
	case testMatchedNothing(outStr):
		sb.WriteString(fmt.Sprintf("\nNO TESTS MATCHED — pattern %q matched zero tests in %s; nothing was verified. Check the name/pattern and scope (module:/file:) before trusting this as a pass", pattern, target))
	default:
		sb.WriteString("\nALL TESTS PASSED")
	}
	return prependNote(textResult(sb.String()), ambiguityMsg), nil, nil
}

func (s *server) handleTest(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	d, err := s.resolveEditTarget(args.Name, args.Receiver, args.Module, args.File)
	if err != nil {
		return s.notFoundOrErr(args.Name, err)
	}

	impact, err := s.backend.GetImpact(d.ID)
	if err != nil {
		return errResult(err)
	}

	if len(impact.Tests) == 0 {
		if d.Test {
			return textResult(fmt.Sprintf("%s is itself a test function, not something other tests cover — run it directly with test:%q (the `test` param runs a test by name; `name` looks up coverage).", args.Name, args.Name)), nil, nil
		}
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

	// #248: this used to always run `./...` regardless of where the
	// target definition and its tests actually live -- the same
	// whole-repo flood #241 fixed for handleTestByName, just on the
	// sibling name-based entry point. Scope to the target definition's
	// own package the same way.
	target := s.testScopeTarget(d.SourceFile)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutFor(len(testNames), target))
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-run", runPattern, "-count=1", "-v", target)
	cmd.Dir = s.projectDir
	out, err := cmd.CombinedOutput()

	outStr := string(out)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Running %d of %d tests (affected by %s) across %s:\n\n",
		len(testNames), len(testNames), args.Name, target))
	sb.WriteString(truncateTestOutput(outStr))

	switch {
	case err != nil && testBuildFailed(outStr):
		sb.WriteString("\nBUILD FAILED -- the package did not compile; zero tests ran. This is NOT a test-failure signal, fix the compile error shown above first")
	case err != nil && testPanicked(outStr):
		sb.WriteString("\nTEST BINARY PANICKED -- not a normal assertion failure; likely caused by state unrelated to your edit (e.g. duplicate flag/command registration shared across tests in one binary). Investigate the panic trace above before assuming your edit is wrong")
	case ctx.Err() == context.DeadlineExceeded:
		sb.WriteString(fmt.Sprintf("\nTIMED OUT after %s -- this is NOT a pass; the run was killed before finishing. This may be a hang from your edit, or simply a large/slow test package -- set DEFN_TEST_TIMEOUT=<duration> (e.g. \"5m\") to allow more time before assuming a hang", testTimeout))
	case err != nil:
		sb.WriteString("\nSOME TESTS FAILED")
	case testMatchedNothing(outStr):
		sb.WriteString(fmt.Sprintf("\nNO TESTS MATCHED — the %d covering test(s) didn't run in %s (likely scoped to the wrong package, e.g. coverage via interface dispatch in a sibling package); nothing was verified", len(testNames), target))
	default:
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

var (
	sqlBodyGrep   = regexp.MustCompile(`(?i)\bbody\s+LIKE\s+'`)
	sqlFileScoped = regexp.MustCompile(`(?i)\b(?:d\.)?source_file\s*(?:LIKE\b|=|\bIN\b)`)
	sqlInfoSchema = regexp.MustCompile(`(?i)\bINFORMATION_SCHEMA\b`)
)

var sqlNameLookup = regexp.MustCompile("(?i)\\b(?:d\\.)?`?name`?\\s*(?:=\\s*'|LIKE\\s+'|IN\\s*\\()")

var (
	sqlSchemaProbe = regexp.MustCompile(`(?i)^\s*(?:SHOW\s+(?:TABLES|DATABASES|COLUMNS)|DESCRIBE\s|DESC\s|EXPLAIN\s)`)
)

func searchShapedSQLRedirect(sql string) string {
	switch {
	case sqlBodyGrep.MatchString(sql):
		return "raw SQL grep on definitions.bodies is a wire-cost anti-pattern — use `code(op:\"search\", pattern:\"<text>\")` instead; it returns compact name+file+line rows, not full bodies. If you truly need SQL analytics (e.g., counts, joins across tables), use `defn query` from the CLI."
	// file-scoped checked before name-lookup: a query combining
	// source_file + name filters (e.g. "list defs matching X in file Y")
	// is better served by file-defs than by a single-name read/outline.
	case sqlFileScoped.MatchString(sql):
		return "file-scoped SQL against definitions is a wire-cost anti-pattern — use `code(op:\"file-defs\", file:\"<path>\")` to list all defs in a file, `code(op:\"read-file\", file:\"<path>\")` for all bodies in a file, `code(op:\"outline\", name:\"<name>\")` for a single def's shape, or `code(op:\"search\", pattern:\"<text>\")` for symbol/text search. These return compact rows tuned for LLM consumption; raw SQL dumps blobs."
	case sqlNameLookup.MatchString(sql):
		return "direct name lookup via SQL is a wire-cost anti-pattern — use `code(op:\"read\", name:\"<name>\")` for the body, `code(op:\"outline\", name:\"<name>\")` for the shape, or `code(op:\"impact\", name:\"<name>\")` for callers. All are cheaper on the wire than blob rows."
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

	// Fast path: sync every file belonging to one module without a
	// full-repo packages.Load. Without this, module: was accepted by
	// the schema but silently ignored, falling through to a whole-repo
	// resync -- one unrelated, unbuildable package elsewhere in a large
	// repo then failed the ENTIRE sync (verified via a real grpc-go
	// trajectory: google.golang.org/grpc/balancer/rls/internal/keys
	// couldn't be resynced because an unrelated xds/experimental
	// package failed to load).
	if args.Module != "" {
		mod := s.findModule(args.Module)
		if mod == nil {
			return errResult(fmt.Errorf("sync: no module matching %q", args.Module))
		}
		sources, err := s.backend.ListFileSources(mod.ID)
		if err != nil {
			return errResult(fmt.Errorf("list module files: %w", err))
		}
		n := 0
		for sourceFile := range sources {
			filePath := sourceFile
			if !filepath.IsAbs(filePath) {
				filePath = filepath.Join(s.projectDir, filePath)
			}
			defs, err := ingest.IngestFile(s.backend, s.projectDir, filePath)
			if err != nil {
				return errResult(fmt.Errorf("ingest file %s: %w", sourceFile, err))
			}
			if err := resolve.ResolveFile(s.backend, s.projectDir, filePath); err != nil {
				return errResult(fmt.Errorf("resolve file %s: %w", sourceFile, err))
			}
			n += defs
		}
		if err := s.autoCommit(); err != nil {
			return errResult(fmt.Errorf("commit after sync: %w", err))
		}
		return textResult(fmt.Sprintf("Synced module %s: updated %d definitions across %d files.", mod.Path, n, len(sources))), nil, nil
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

// handleGC runs a WAL checkpoint (folds defn.db-wal back into
// defn.db) via the backend's GC method. Safe to invoke while the
// serve is running — PRAGMA wal_checkpoint(PASSIVE) doesn't block
// concurrent readers/writers.
func (s *server) handleGC(_ context.Context, _ *sdkmcp.CallToolRequest, _ codeParam) (*sdkmcp.CallToolResult, any, error) {
	dbDir := filepath.Join(s.projectDir, ".defn")
	before := dbDirSize(dbDir)
	start := time.Now()
	if err := s.backend.GC(); err != nil {
		return errResult(fmt.Errorf("gc: %w", err))
	}
	after := dbDirSize(dbDir)
	return textResult(fmt.Sprintf(
		"GC complete in %s. db size: %s → %s (saved %s).",
		time.Since(start).Truncate(time.Millisecond),
		humanSize(before), humanSize(after), humanSize(before-after),
	)), nil, nil
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
	d, err := s.resolveEditTarget(args.Name, args.Receiver, args.Module, args.File)
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
	// Every def -- bodied or not -- gets a real MinHash computed from
	// signature+body together, unconditionally (see
	// store.ComputeMinHashForDef): one formula for every kind, so
	// bodyless/short-body defs (interfaces, consts, vars) get real
	// comparable content instead of ComputeMinHash's all-max sentinel
	// for empty input. That happens synchronously at write time
	// (UpsertDefinition/UpsertDefinitionsBulk) and via a one-shot
	// backfill on DB open, so an empty/errored summaries table here
	// means something genuinely wrong (a fresh-open race or a real DB
	// error), not "this def has no body" -- there's no structurally
	// different fallback algorithm to reach for; report it honestly
	// instead of silently degrading to a worse one.
	summaries, err := s.backend.AllDefSummaryMinHashes()
	if err != nil || len(summaries) == 0 {
		return textResult(fmt.Sprintf("Similarity index isn't available for %s right now (empty or errored def_summaries) — try again in a moment.", args.Name)), nil, nil
	}
	target, ok := summaries[d.ID]
	if !ok {
		// Not yet computed for this def; compute on the fly and
		// backfill for future queries.
		target = store.ComputeMinHashForDef(d.Body, d.Signature)
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

// projectOverview returns a compact module-level summary: package path,
// def count, first ~3 exported def names per module. Used by handleOverview
// when called with no file/name arg — orientation before the model
// commits to a subtree.
const projectOverviewModuleCap = 40

const projectOverviewDefsPerModule = 3

func (s *server) projectOverview(ctx context.Context) (*sdkmcp.CallToolResult, any, error) {
	mods, err := s.backend.ListModules()
	if err != nil {
		return errResult(fmt.Errorf("list modules: %w", err))
	}
	if len(mods) == 0 {
		return textResult("[project overview: no modules ingested — call code(op:\"sync\") to ingest the project, or run `defn ingest .` from a shell.]"), nil, nil
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })

	var listing strings.Builder
	shown := 0
	totalDefs := 0
	for _, m := range mods {
		allDefs, _ := s.backend.GetModuleDefinitions(m.ID)
		// #11: struct fields are real definitions (Type.Field lookup)
		// but not top-level API surface -- counting/exemplifying them
		// here would crowd out real symbols and inflate the def count
		// this project-wide summary reports. Fully visible via
		// search/outline/read; just excluded from this orientation view.
		defs := make([]store.Definition, 0, len(allDefs))
		for _, d := range allDefs {
			if d.Kind != "field" {
				defs = append(defs, d)
			}
		}
		totalDefs += len(defs)
		if shown >= projectOverviewModuleCap {
			continue
		}
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
		listing.WriteString(fmt.Sprintf("- %s — %d defs (%d exported)", m.Path, len(defs), nExp))
		if len(exemplars) > 0 {
			listing.WriteString(fmt.Sprintf(" — %s", strings.Join(exemplars, ", ")))
		}
		listing.WriteString("\n")
		shown++
	}
	if len(mods) > shown {
		listing.WriteString(fmt.Sprintf("… (%d more modules omitted — pass file:\"path/to/pkg\" for a subtree)\n", len(mods)-shown))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Project overview (%d modules)\n\n", len(mods)))

	// #212 project-wide: same win mechanism as file/directory narratives
	// (#211's proven cache_creation-avoidance pattern), applied at the
	// top level -- the highest-leverage first-touch call a model makes.
	// Generation sources from the module listing itself (already a
	// compact, information-dense summary of the whole project's shape)
	// rather than concatenating doc+signature for every def
	// project-wide, which could be huge on a large codebase. Staleness
	// is keyed off module count + total def count -- cheap signals that
	// change whenever ingest adds/removes packages or defs, without
	// hashing every body in the project.
	if totalDefs >= fileNarrativeMinDefs && s.explainClient != nil {
		if narrative := s.projectNarrative(ctx, listing.String(), len(mods), totalDefs); narrative != "" {
			sb.WriteString(narrative + "\n\n")
		}
	}

	sb.WriteString(listing.String())
	sb.WriteString("\nUse `op:\"overview\" file:\"<pkg-path>\"` to drill in, `op:\"search\" pattern:\"<term>\"` to jump to a def.\n")
	return textResult(sb.String()), nil, nil
}

// projectNarrative returns a cached or freshly-generated #212
// project-wide architectural narrative, or "" if unavailable. Unlike
// fileNarrative (staleness hashed against concatenated def bodies),
// staleness here is keyed off module count + total def count --
// module/def churn is a cheap, sufficient signal for a project-level
// summary and avoids ever reading or hashing every body across a
// potentially huge codebase. Stored via the generic meta key-value
// store rather than file_summaries, since a project-wide narrative has
// no single owning module for that table's FOREIGN KEY.
func (s *server) projectNarrative(ctx context.Context, moduleListing string, moduleCount, totalDefs int) string {
	const metaKey = "project_narrative"
	sig := fmt.Sprintf("%d:%d", moduleCount, totalDefs)

	if cached, _ := s.backend.GetMeta(metaKey); cached != "" {
		if sigLine, narrative, ok := strings.Cut(cached, "\n"); ok && sigLine == sig {
			return narrative
		}
	}

	question := "Summarize this project's overall architecture in 2-3 sentences: what kind of system it is, its main components/packages, and how they fit together."
	narrative, err := s.explainClient.Explain(ctx, question, moduleListing)
	if err != nil || strings.TrimSpace(narrative) == "" {
		return ""
	}
	s.backend.SetMeta(metaKey, sig+"\n"+narrative)
	return narrative
}

func (s *server) handleOverview(ctx context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	file := args.File
	if file == "" {
		file = args.Name
	}
	if strings.TrimSpace(file) == "" {
		// L18: empty overview → project-wide module summary. The preamble
		// calls overview "the right first-touch" but the old impl errored
		// without a file; agents got a rejection instead of orientation.
		return s.projectOverview(ctx)
	}

	// #82/#212: for subpath files use the dirname; for root-level files
	// leave dir empty so the module-path LIKE-match is permissive and
	// the source_file exact match narrows to the right file. Mirrors
	// handleFileDefs/handleReadFile -- stripping .go to fabricate a
	// fake directory name (e.g. "main.go" -> "main") never matched
	// anything for root-level files, silently breaking
	// `overview file:"<root-level-file>.go"` entirely.
	// #212 validation (2026-07-31): overview(file:"pkg-path") -- the
	// tool's own documented usage for a bare package directory, no
	// trailing file -- has never worked at all. FindDefinitionsByFile's
	// sourceFile param does an EXACT match against d.source_file, which
	// is always a real file path like "internal/mcp/server.go"; passing
	// a bare directory like "internal/mcp" as sourceFile can never
	// match anything, so every such call silently returned "no
	// definitions found". Distinguish the two shapes: a .go path gets
	// the exact-file match (dir = its parent); a bare directory gets
	// dir = itself and NO sourceFile constraint, relying purely on the
	// module-path LIKE filter to gather every def under that prefix.
	var dir, sourceFile string
	if strings.HasSuffix(file, ".go") {
		sourceFile = file
		if idx := strings.LastIndex(file, "/"); idx >= 0 {
			dir = file[:idx]
		}
	} else {
		dir = file
	}

	defs, err := s.backend.FindDefinitionsByFile(dir, sourceFile, 0)
	if err != nil || len(defs) == 0 {
		return errResult(fmt.Errorf("no definitions found for %s -- check the path (try op:\"overview\" with no file: for the project map, or op:\"search\" to find the right path)", file))
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

	// #248: cap AFTER query filtering so a narrowing query still returns
	// its own matches in full when they fit.
	var cappedFrom int
	if len(defs) > overviewDefsCap {
		cappedFrom = len(defs)
		defs = defs[:overviewDefsCap]
	}

	// Get full definitions with bodies to check relationships.
	var sb strings.Builder
	switch {
	case hiddenDefs > 0:
		sb.WriteString(fmt.Sprintf("## %s (%d of %d definitions, filtered by query=%q)\n\n", file, len(defs), totalDefs, args.Query))
	case cappedFrom > 0:
		sb.WriteString(fmt.Sprintf("## %s (showing %d of %d definitions — pass a narrower file/query for the rest)\n\n", file, len(defs), cappedFrom))
	default:
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

	// #212: any overview scope (a single file OR a whole package
	// directory -- validation benches found the model calls overview
	// at the DIRECTORY level far more often than per-file, so gating on
	// a single matched file meant this almost never fired in practice)
	// with enough defs to be worth it gets ONE precomputed architectural
	// narrative prepended, covering every def in the requested scope --
	// not one narrative per individual file within it, which would be
	// as many Sonnet calls as files in the directory. Skipped when the
	// co-processor isn't configured (no ANTHROPIC_API_KEY) -- degrades
	// to the plain per-def listing below, unchanged from before this
	// landed.
	if len(defs) >= fileNarrativeMinDefs && s.explainClient != nil {
		if narrative := s.fileNarrative(ctx, file, defs); narrative != "" {
			sb.WriteString(narrative + "\n\n")
		}
	}

	// #180: iterate byFile in sorted order, not Go's randomized map
	// order -- an unsorted range here meant two identical overview
	// calls against an unchanged DB could emit "### file" sections in
	// a different order each time, breaking prompt-cache prefix
	// matching on the response (and just being confusing on its own).
	fileNames := make([]string, 0, len(byFile))
	for f := range byFile {
		fileNames = append(fileNames, f)
	}
	sort.Strings(fileNames)
	for _, f := range fileNames {
		fileDefs := byFile[f]
		if len(byFile) > 1 {
			sb.WriteString(fmt.Sprintf("### %s\n", f))
		}

		// #227ish: a generated file (Go's own `// Code generated ... DO NOT
		// EDIT.` convention) decomposed into the same per-def listing as
		// hand-written code clutters overview with dozens of autogenerated
		// getters/setters that are rarely what an overview call is actually
		// for. Collapse to one line; the individual defs are still fully
		// reachable via search/read/outline by name, this only affects the
		// orientation view.
		if raw, err := s.backend.GetFileSource(fileDefs[0].ModuleID, f); err == nil && isGeneratedSource(raw) {
			sb.WriteString(fmt.Sprintf("_(generated, %d definitions — DO NOT EDIT; use search/read to see individual defs)_\n\n", len(fileDefs)))
			continue
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

	d, err := s.resolveWriteTarget(args.Name, args.Receiver, args.Module, args.File)
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
//
// KNOWN LIMITATION, not a bug to chase here: this operates per-caller-body
// with no type information, unlike an LSP rename (which resolves via
// go/types Uses/Defs before touching anything). It renames every
// non-locally-shadowed *ast.Ident matching oldName in the body — including
// an unrelated selector `x.oldName` on some other receiver that merely
// shares the name. The caller SET passed in is correct (handleApply/
// handleRename get it from the type-checked refs graph), but a caller
// whose body coincidentally also references a different symbol of the
// same name will have that unrelated reference renamed too. Fixing this
// properly requires plumbing go/types occurrence positions through to
// rename time, not a quick patch — accepted, documented gap for now.
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
		return errResult(fmt.Errorf("no definitions found at %s:%d -- check the path (try op:\"overview\" with no file: for the project map, or op:\"search\" to find the right path)", args.File, args.Line))
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

func (s *server) handleExpand(_ context.Context, req *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	names := args.Names
	if len(names) == 0 {
		if strings.TrimSpace(args.Name) == "" {
			return errResult(fmt.Errorf("expand: name or names is required"))
		}
		names = []string{args.Name}
	}

	includes := args.Include
	if len(includes) == 0 {
		includes = []string{"outline", "callers"}
	}
	want := map[string]bool{}
	for _, k := range includes {
		want[strings.ToLower(strings.TrimSpace(k))] = true
	}

	mods, _ := s.backend.ListModules()
	modulePathByID := make(map[int64]string, len(mods))
	for _, m := range mods {
		modulePathByID[m.ID] = m.Path
	}

	var sb strings.Builder
	var notFound []string
	resolved := 0
	var firstErr error
	for i, name := range names {
		d, err := s.resolveEditTarget(name, "", args.Module, args.File)
		if err != nil {
			notFound = append(notFound, name)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if resolved > 0 {
			sb.WriteString("\n---\n\n")
		}
		// #248 gap: the circuit breaker (turn_state.go) silently redirects
		// blocked bare-name read/outline calls through this same expand
		// path, which resolves via the identical best-effort tiebreak as
		// handleCode's direct "read"/"outline" cases but -- unlike those --
		// never disclosed it. An ambiguous name auto-batched through expand
		// resolved silently, with no way to tell it wasn't the only match.
		if note := s.ambiguityNote(name, "", args.Module, args.File); note != "" {
			sb.WriteString(note)
		}
		// This name's own want-map: a per-name copy only when body needs to
		// be suppressed below, so other names in the same batch still get
		// whatever the caller asked for.
		sectionWant := want
		if want["body"] && req != nil && s.respCache != nil {
			// The direct read/outline/slice dispatch cases in handleCode all
			// check bodyServedEpochsAgo before re-serving a body, but this
			// batched path -- including the circuit breaker's own auto-batch
			// redirect through here -- never did, so a name whose full body
			// was already read verbatim this session got it re-dumped in
			// full again on the very next expand/auto-batch that swept it
			// in. Confirmed via a real grpc-go-3119 trajectory: a function
			// read explicitly via read(full:true) got its body re-served 10
			// calls later purely because it was caught in an unrelated
			// circuit-breaker batch.
			if epochsAgo, ok := s.respCache.bodyServedEpochsAgo(req.Session, name); ok && epochsAgo <= staleEpochThreshold {
				sectionWant = map[string]bool{}
				for k, v := range want {
					sectionWant[k] = v
				}
				sectionWant["body"] = false
				sb.WriteString(fmt.Sprintf("_(%s's full body was already read in this session via read(full:true) -- omitted here, nothing new. If it may have changed since, call code(op:\"sync\") first.)_\n", name))
			}
		}
		if err := s.renderExpandSection(&sb, d, modulePathByID[d.ModuleID], sectionWant); err != nil {
			return errResult(fmt.Errorf("expand: gather callers for %s: %w", name, err))
		}
		resolved++
		_ = i
	}

	if resolved == 0 {
		return s.notFoundOrErr(names[0], firstErr)
	}

	// Warn on any unsupported include kinds so the caller learns the vocabulary.
	var unknown []string
	for _, k := range includes {
		norm := strings.ToLower(strings.TrimSpace(k))
		switch norm {
		case "outline", "body", "callers":
			// supported
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sb.WriteString(fmt.Sprintf("\n_note: unsupported include kinds ignored: %s (supported: outline, body, callers)_\n",
			strings.Join(unknown, ", ")))
	}
	if len(notFound) > 0 {
		sb.WriteString(fmt.Sprintf("\n_note: not found, skipped: %s_\n", strings.Join(notFound, ", ")))
	}

	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "expand",
		BytesReturned: len(out),
	}), nil, nil
}

// renderExpandSection writes one def's expand sections (outline/body/
// callers per want) into sb. Split out of handleExpand so #210's
// multi-name batching can call it once per resolved def without
// duplicating the section-rendering logic.
func (s *server) renderExpandSection(sb *strings.Builder, d *store.Definition, modulePath string, want map[string]bool) error {
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", recv, d.Name, d.Kind))
	if modulePath != "" {
		sb.WriteString(fmt.Sprintf("Module: %s\n", modulePath))
	}
	sb.WriteString("\n")

	if want["outline"] {
		sb.WriteString("### outline\n")
		switch {
		case d.Signature != "":
			sb.WriteString("```go\n" + d.Signature + "\n```\n")
		case d.Doc != "":
			sb.WriteString(d.Doc + "\n")
		}
		bodyLines := strings.Count(d.Body, "\n") + 1
		sb.WriteString(fmt.Sprintf("Body: %d lines, %d bytes (add \"body\" to include for source)\n",
			bodyLines, len(d.Body)))
		callees, _ := s.backend.GetCallees(d.ID)
		if len(callees) > 0 {
			names := make([]string, 0, len(callees))
			for _, c := range callees {
				names = append(names, formatReceiver(c.Receiver)+c.Name)
			}
			sort.Strings(names)
			sb.WriteString(fmt.Sprintf("Callees (%d): %s\n", len(callees), truncateList(names, outlineCalleeCap)))
		}
		if flow := topLevelFlow(d.Body); len(flow) > 0 {
			sb.WriteString(fmt.Sprintf("Flow (%d): %s\n", len(flow), truncateFlow(flow, outlineFlowCap)))
		}
		sb.WriteString("\n")
	}

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
			return err
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
	return nil
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
	// #250: Limit was accepted but silently ignored -- output was always
	// capped at the fixed fileDefsCap with no way to ask for more.
	limit := fileDefsCap
	if args.Limit > 0 {
		limit = args.Limit
	}
	total := len(defs)
	if total > limit {
		defs = defs[:limit]
	}
	results := make([]defSummary, 0, len(defs))
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
	if total > limit {
		return textResult(fmt.Sprintf("%d of %d definitions in %s (showing first %d — pass limit: for more):\n\n%s", len(results), total, file, limit, text)), nil, nil
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
	d, err := s.resolveEditTarget(args.Name, args.Receiver, args.Module, args.File)
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
	rendered := 0
	for _, r := range results {
		if rendered >= traverseResultCap {
			fmt.Fprintf(&sb, "\n… (%d more results omitted; pass format:\"json\" for the full list)\n", len(results)-rendered)
			break
		}
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
		rendered++
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
	limit := 200
	if args.Limit > 0 {
		limit = args.Limit
	}

	// #250: File was accepted but silently ignored -- every literals query
	// ran repo-wide regardless, same silent-drop class as #241 (search's
	// file:). Scope via defIDs, the pre-filter QueryLiteralFields already
	// supports for exactly this purpose.
	var defIDs []int64
	if args.File != "" {
		allDefs, ferr := s.backend.FindDefinitions("%")
		if ferr != nil {
			return errResult(fmt.Errorf("query literals: %w", ferr))
		}
		for _, d := range allDefs {
			if strings.Contains(d.SourceFile, args.File) {
				defIDs = append(defIDs, d.ID)
			}
		}
		if len(defIDs) == 0 {
			return textResult("No literal fields found"), nil, nil
		}
	}

	fields, err := s.backend.QueryLiteralFields(typeName, args.Name, args.Body, nil, defIDs, limit, false, false)
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

	// #250: Limit was accepted but silently ignored -- output was always
	// capped at the fixed pragmasCap with no way to ask for more.
	limit := pragmasCap
	if args.Limit > 0 {
		limit = args.Limit
	}
	total := len(comments)
	if total > limit {
		comments = comments[:limit]
	}

	var sb strings.Builder
	if total > limit {
		fmt.Fprintf(&sb, "## Pragmas matching %q (showing %d of %d results — pass limit: or file: or a narrower pattern for the rest)\n\n", pragmaKey, len(comments), total)
	} else {
		fmt.Fprintf(&sb, "## Pragmas matching %q (%d results)\n\n", pragmaKey, len(comments))
	}
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

		// Mutation carries Receiver but this used to call plain
		// GetDefinitionByName, ignoring it -- same #248-class bug as
		// explain/batch-impact (2026-08-10 sweep): validating a plan
		// mutation for (*Foo).Bar could silently resolve to an unrelated
		// same-named method on a different receiver type instead of the
		// one actually being changed.
		var d *store.Definition
		var err error
		if m.Receiver != "" {
			d, err = s.backend.GetDefinitionByNameAndReceiver(m.Name, "", m.Receiver)
			if err != nil {
				if alt := strings.TrimPrefix(m.Receiver, "*"); alt != m.Receiver {
					d, err = s.backend.GetDefinitionByNameAndReceiver(m.Name, "", alt)
				} else {
					d, err = s.backend.GetDefinitionByNameAndReceiver(m.Name, "", "*"+m.Receiver)
				}
			}
		} else {
			d, err = s.backend.GetDefinitionByName(m.Name, "")
		}
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
	d, err := s.resolveEditTarget(args.Name, args.Receiver, args.Module, args.File)
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
	tests := make([]testInfo, 0, len(impact.Tests))
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
		// Same #248-class bug as explain (2026-08-10, prometheus batch
		// digging sweep): batch-impact receives the full codeParam
		// (Module/File/Receiver already in scope) but called
		// GetDefinitionByName(name, "") directly, discarding them --
		// an ambiguous name in a multi-name batch had no way to be
		// disambiguated even though the caller supplied the means to.
		d, err := s.resolveEditTarget(name, args.Receiver, args.Module, args.File)
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

const (
	impactCallerCap  = 15
	outlineCalleeCap = 15
	outlineFlowCap   = 20
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

func (s *server) handleMethods(_ context.Context, _ *sdkmcp.CallToolRequest, args nameParam) (*sdkmcp.CallToolResult, any, error) {
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return errResult(fmt.Errorf("methods: name is required (a type or interface name)"))
	}
	// Strip leading '*' -- callers often paste "*Mux" from a receiver.
	name = strings.TrimPrefix(name, "*")

	// #250 sweep: module:/file: were accepted codeParam fields but never
	// threaded into this op's nameParam at the handleCode dispatch site --
	// a caller trying to disambiguate two same-named types got silent
	// non-scoping. Resolve the same way resolveEditTarget does.
	var modulePath string
	if args.File != "" {
		if mod := s.findModuleByFile(args.File); mod != nil {
			modulePath = mod.Path
		}
	}
	if modulePath == "" && args.Module != "" {
		if mod := s.findModule(args.Module); mod != nil {
			modulePath = mod.Path
		}
	}

	// Interface path: methods live inline in the interface body, not
	// as separate method rows. If we find a type/interface def by
	// this name and its kind is 'interface', parse its body.
	if typeDef, err := s.backend.GetDefinitionByName(name, modulePath); err == nil && typeDef != nil && typeDef.Kind == "interface" {
		return s.methodsFromInterfaceBody(typeDef)
	}

	// Type path: scan all methods, keep those whose receiver matches.
	// Handles pointer receivers (*T), value receivers (T), and
	// generic receivers (T[X], *T[X]) -- we compare against T after
	// stripping the pointer prefix and generic bracket suffix.
	allMethods, err := s.backend.FilterDefinitions("", "method", "", 0)
	if err != nil {
		return errResult(fmt.Errorf("methods: list: %w", err))
	}
	var mine []store.Definition
	// distinctFiles tracks every source file contributing a match so an
	// unscoped lookup can warn when it silently merged methods from more
	// than one file -- two unrelated same-named types in different
	// packages would otherwise have their method sets combined into one
	// list with no indication they aren't the same type.
	distinctFiles := map[string]bool{}
	for _, m := range allMethods {
		recv := strings.TrimPrefix(m.Receiver, "*")
		if idx := strings.Index(recv, "["); idx > 0 {
			recv = recv[:idx]
		}
		if recv != name {
			continue
		}
		if args.File != "" && !strings.Contains(m.SourceFile, args.File) {
			continue
		}
		mine = append(mine, m)
		distinctFiles[m.SourceFile] = true
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

	r, o, e := s.formatMethodList(name, "type", mine, args.Query)
	if args.File == "" && len(distinctFiles) > 1 {
		files := make([]string, 0, len(distinctFiles))
		for f := range distinctFiles {
			files = append(files, f)
		}
		sort.Strings(files)
		note := fmt.Sprintf("[note: methods came from %d different files (%s) sharing receiver type %q -- these may be UNRELATED same-named types merged into one list. Pass file: to scope to a single one.]\n\n", len(distinctFiles), strings.Join(files, ", "), name)
		r = prependNote(r, note)
	}
	return r, o, e
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
	d, err := s.resolveEditTarget(args.Name, args.Receiver, args.Module, args.File)
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

	d, err := s.resolveEditTarget(args.Name, args.Receiver, args.Module, args.File)
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
		inferred, err := s.inferSingleTargetName(s.backend)
		if err != nil {
			return errResult(fmt.Errorf("insert-precondition: %w", err))
		}
		name = inferred
	}
	d, err := s.resolveWriteTarget(name, args.Receiver, args.Module, args.File)
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", name))
	}
	newBody, err := projection.InsertPrecondition(d.Body, args.Condition, args.Ret)
	if err != nil {
		return errResult(err)
	}
	snippet := fmt.Sprintf("if %s {\n\t%s\n}", args.Condition, args.Ret)
	return s.applyEditTerse(sessionOf(req), d, "inserted precondition at entry", snippet, newBody)
}

// handleAddImport adds a new import (with optional alias) to the given
// file directly, via patchImportOnDisk (shared with handleApply's
// batch "add-import" case).
//
// #221: presence is checked against the file itself (not the DB's
// per-module imports table, which is a union across every file in a
// package and unreliable for single-file presence), and the write
// actually lands via projection.AddImport.
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
		return errResult(fmt.Errorf("add-import: no definitions found in file %q -- cannot resolve module (check the path via op:\"overview\" or op:\"search\")", file))
	}
	moduleID := defs[0].ModuleID

	changed, perr := s.patchImportOnDisk(moduleID, file, args.ImportPath, args.Alias)
	if perr != nil {
		return errResult(fmt.Errorf("add-import: %w", perr))
	}
	if !changed {
		return textResult(fmt.Sprintf("%s: import %q already present (no-op)\n", file, args.ImportPath)), nil, nil
	}

	// Keep the DB's per-module imports table in sync too, so a later
	// full regen (or another file in the same package) sees this import
	// as part of the module's known set.
	existing, err := s.backend.GetImports(moduleID)
	if err != nil {
		return errResult(fmt.Errorf("add-import: read imports: %w", err))
	}
	alreadyInDB := false
	for _, imp := range existing {
		if imp.ImportedPath == args.ImportPath && imp.Alias == args.Alias {
			alreadyInDB = true
			break
		}
	}
	if !alreadyInDB {
		updated := append(existing, store.Import{
			ModuleID:     moduleID,
			ImportedPath: args.ImportPath,
			Alias:        args.Alias,
		})
		if err := s.backend.SetImports(moduleID, updated); err != nil {
			return errResult(fmt.Errorf("add-import: set imports: %w", err))
		}
	}

	snippet := fmt.Sprintf("import %q", args.ImportPath)
	if args.Alias != "" {
		snippet = fmt.Sprintf("import %s %q", args.Alias, args.ImportPath)
	}
	var sb strings.Builder
	sb.WriteString(file)
	sb.WriteString(": added import\n    ")
	sb.WriteString(snippet)
	sb.WriteString("\n")
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
		inferred, err := s.inferSingleTargetName(s.backend)
		if err != nil {
			return errResult(fmt.Errorf("rename-param: %w", err))
		}
		name = inferred
	}
	d, err := s.resolveWriteTarget(name, args.Receiver, args.Module, args.File)
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
	return s.applyEditTerse(sessionOf(req), d, action, snippet, newBody)
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
		inferred, err := s.inferSingleTargetName(s.backend)
		if err != nil {
			return errResult(fmt.Errorf("wrap-in-defer: %w", err))
		}
		name = inferred
	}
	d, err := s.resolveWriteTarget(name, args.Receiver, args.Module, args.File)
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
	return s.applyEditTerse(sessionOf(req), d, action, snippet, newBody)
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
		inferred, err := s.inferSingleTargetName(s.backend)
		if err != nil {
			return errResult(fmt.Errorf("replace-slice: %w", err))
		}
		name = inferred
	}
	d, err := s.resolveWriteTarget(name, args.Receiver, args.Module, args.File)
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
	return s.applyEditTerse(sessionOf(req), d, action, args.New, newBody)
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
		inferred, err := s.inferSingleTargetName(s.backend)
		if err != nil {
			return errResult(fmt.Errorf("replace-hunk: %w", err))
		}
		name = inferred
	}
	d, err := s.resolveWriteTarget(name, args.Receiver, args.Module, args.File)
	if err != nil {
		return errResult(fmt.Errorf("definition %q not found", name))
	}
	newBody, err := projection.ReplaceHunk(d.Body, args.Old, args.New, args.Index)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			if hint := suggestClosestFragmentHint(d.Body, args.Old); hint != "" {
				return errResult(fmt.Errorf("%w%s", err, hint))
			}
		}
		return errResult(err)
	}
	action := "replaced hunk"
	if args.Index > 0 {
		action = fmt.Sprintf("replaced hunk #%d", args.Index)
	}
	return s.applyEditTerse(sessionOf(req), d, action, args.New, newBody)
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
func (s *server) applyEditTerse(session *sdkmcp.ServerSession, d *store.Definition, action, snippet, newBody string) (*sdkmcp.CallToolResult, any, error) {
	// #edit-disambiguation dispatch (gemot): this used to re-resolve its
	// target by bare name (GetDefinitionByName(name, "")), discarding
	// the caller's already-disambiguated d and silently undoing whatever
	// receiver:/module:/file: scoping resolveEditTarget just applied --
	// the computed newBody came from the RIGHT def, but got written into
	// whatever this second, blind lookup's blast-radius tiebreak picked.
	// Taking d directly from the caller removes the redundant lookup
	// instead of just disambiguating it a second time.
	src := "package x\n" + newBody
	if _, parseErr := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments); parseErr != nil {
		return errResult(fmt.Errorf("new_body has syntax error: %v", parseErr))
	}
	// #222: same identity-preserving guard as handleEdit. A projection op
	// (replace-hunk, replace-slice, etc.) can technically produce a body
	// whose signature line declares a different name/receiver -- e.g. a
	// replace-hunk targeting the "func Foo(...)" line itself. That would
	// leave d.Name stale exactly like the edit-path bug: mergeDeclsIntoSource
	// splices the differently-named body under the old key, the merged file
	// ends up with no decl under the old name, and safeWriteGoFile's
	// on-disk-decl-loss check silently blocks the write.
	if newName, _, newReceiver, _ := s.inferFromBody(newBody); newName != "" && (newName != d.Name || newReceiver != d.Receiver) {
		return errResult(fmt.Errorf("%s%s: new_body declares %s%s, which changes its name/receiver — use code(op:\"rename\") to rename a definition; this op only changes body content", formatReceiver(d.Receiver), d.Name, formatReceiver(newReceiver), newName))
	}
	d.Body = newBody
	d.Signature = extractSignature(newBody)

	// #12: write and emit-gate through a transaction so a failure leaves
	// neither the DB nor the file changed -- this was missed when #12
	// fixed handleEdit/handleCreate/handleDelete/apply, since every
	// projection op (replace-hunk, replace-slice, insert-precondition,
	// wrap-in-defer, rename-param) funnels through this one function
	// instead of handleEdit.
	tx, commit, rollback, txErr := s.backend.Begin()
	if txErr != nil {
		return errResult(txErr)
	}
	defer rollback()

	if _, err := tx.UpsertDefinition(d); err != nil {
		return errResult(err)
	}

	// #148: projection ops (all callers of applyEditTerse) are AST-
	// guaranteed sig-stable — skip the go-build gate to actually deliver
	// the "faster than native because the index is maintained" thesis.
	// commitOrRollbackOnEmit preserves that (emit-only, no build) while
	// still gating commit on the result coming back clean. Set
	// DEFN_STRICT_BUILD=1 to force the old per-mutation build.
	var opts emit.Opts
	if d.SourceFile != "" {
		opts = emit.Opts{GoimportsFiles: []string{d.SourceFile}, TouchedFiles: []string{d.SourceFile}}
	}
	buildResult := s.commitOrRollbackOnEmit(tx, commit, rollback, opts)

	if buildResult == "" {
		// #160: fire-and-forget summary regeneration. Body changed → any
		// existing summary is stale. Worker computes async; if the queue
		// is full or backend unconfigured we drop silently (the summary
		// is best-effort, next mutation re-enqueues).
		s.enqueueSummary(d)
		s.autoResolveFile(d.SourceFile, s.modulePath(d.ModuleID))
	}

	recv := formatReceiver(d.Receiver)
	var sb strings.Builder
	sb.WriteString(recv)
	sb.WriteString(d.Name)
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
		// #244: previously kept only the first line of buildResult,
		// discarding the actual compiler diagnostics (file/line, the
		// undefined symbol, etc.) that handleEdit's failure path shows
		// in full -- forcing an agent to blind-guess or fall back to
		// the verbose edit op whenever a projection-op edit failed to
		// build. Write the full result, matching handleEdit.
		sb.WriteString("build: ")
		sb.WriteString(buildResult)
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
func (s *server) inferSingleTargetName(backend store.Backend) (string, error) {
	defs, err := backend.FilterDefinitions("", "", "", 0)
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

// renderSummaryOnly produces the #160 compact response for a def
// whose stored summary is still fresh. Includes signature and the
// model-generated one-line intent — no body. The reader can request
// the body separately by omitting mode:"summary" or by passing
// full:true if the def matched an upstream fingerprint.
func renderSummaryOnly(d *store.Definition, sum *store.DefSummary) *sdkmcp.CallToolResult {
	var sb strings.Builder
	recv := formatReceiver(d.Receiver)
	sb.WriteString(fmt.Sprintf("## %s%s (%s) — summary\n", recv, d.Name, d.Kind))
	if d.Signature != "" {
		sb.WriteString("```go\n")
		sb.WriteString(d.Signature)
		sb.WriteString("\n```\n\n")
	}
	if d.Doc != "" {
		sb.WriteString(d.Doc)
		sb.WriteString("\n\n")
	}
	sb.WriteString(fmt.Sprintf("_intent (%s):_ %s\n", sum.Model, sum.OneLine))
	sb.WriteString("\n_summary mode — omit `mode:\"summary\"` for the full body._\n")
	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "read",
		BytesReturned: len(out),
	})
}

// enqueueSummary submits d for background summary regeneration. Safe
// to call before the worker is initialized (nil-guarded), safe to
// call when no summary backend is configured (Stub silently succeeds,
// writes "TODO: <Name>" that the read path treats as a summary miss
// and falls back to full body — no user-visible degradation).
//
// Always fire-and-forget: the enqueue is non-blocking; a full queue
// drops silently so a slow model can't stall the write path.
func (s *server) enqueueSummary(d *store.Definition) {
	if s == nil || s.summaryWorker == nil || d == nil {
		return
	}
	modulePath := s.modulePath(d.ModuleID)
	s.summaryWorker.Enqueue(summary.Request{
		DefID:      d.ID,
		Name:       d.Name,
		Kind:       d.Kind,
		Receiver:   d.Receiver,
		ModulePath: modulePath,
		Body:       d.Body,
		BodyHash:   store.HashBodyStructural(d.Body),
	})
}

// summaryHintLineThreshold is the body-line count above which
// handleGetDefinition appends the #160 mode:"summary" tip. 40 lines
// ≈ the point where a 5-line "here's what it does" summary is
// meaningfully cheaper than sending the whole body.
const summaryHintLineThreshold = 40

// handleResummarize walks the DB for defs with no one_line summary
// and enqueues each on the summary worker. #160 stage 3a backfill.
//
// Fire-and-forget: enqueue is non-blocking; the worker's queue is
// bounded (defaultQueueDepth), so on very large corpora the tail of
// this batch may drop. Callers seeing "enqueued < missing" can
// re-run — the next call re-lists whatever's still missing, so
// backfill is idempotent and eventually complete.
//
// With the Stub backend (no ANTHROPIC_API_KEY) this enqueues but
// produces "TODO: <Name>" placeholders that handleGetDefinition
// treats as no summary — the read path still falls back to full
// body, so running this without a real backend is a no-op in effect
// (safe, but wastes DB rows). Response text calls that out.
func (s *server) handleResummarize(_ context.Context, _ *sdkmcp.CallToolRequest, _ codeParam) (*sdkmcp.CallToolResult, any, error) {
	if s.summaryWorker == nil {
		return errResult(fmt.Errorf("summary worker not initialized (projectDir empty?)"))
	}
	ids, err := s.backend.ListDefsMissingSummary()
	if err != nil {
		return errResult(fmt.Errorf("list missing summaries: %w", err))
	}
	var enqueued, dropped, lookupFailed int
	for _, id := range ids {
		d, err := s.backend.GetDefinition(id)
		if err != nil || d == nil {
			lookupFailed++
			continue
		}
		modulePath := s.modulePath(d.ModuleID)
		ok := s.summaryWorker.Enqueue(summary.Request{
			DefID:      d.ID,
			Name:       d.Name,
			Kind:       d.Kind,
			Receiver:   d.Receiver,
			ModulePath: modulePath,
			Body:       d.Body,
			BodyHash:   store.HashBodyStructural(d.Body),
		})
		if ok {
			enqueued++
		} else {
			dropped++
		}
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("resummarize: %d missing summaries\n", len(ids)))
	sb.WriteString(fmt.Sprintf("  enqueued: %d\n", enqueued))
	if dropped > 0 {
		sb.WriteString(fmt.Sprintf("  dropped:  %d  (worker queue full — re-run to catch the tail)\n", dropped))
	}
	if lookupFailed > 0 {
		sb.WriteString(fmt.Sprintf("  lookup-failed: %d  (defs vanished between list and read — usually harmless)\n", lookupFailed))
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		sb.WriteString("\n_note: ANTHROPIC_API_KEY is not set — the Stub backend is generating\n\"TODO: <Name>\" placeholders that the read path treats as no summary.\nSet ANTHROPIC_API_KEY (Haiku 4.5) for real summaries; see #160._\n")
	}
	return textResult(sb.String()), nil, nil
}

// autoEmitAndBuildForCreate is the create-op variant of
// autoEmitAndBuildForFile: same file-scoping, PLUS Opts.AllowedAdds
// tells mergeDeclsIntoSource that the named defs are intentional
// new additions (not drift), so it may splice them in place instead
// of falling through to full regeneration. Preserves floating
// comments in files that already had existing decls plus a
// code(op:"create") added a new one. #162.
func (s *server) autoEmitAndBuildForCreate(sourceFile string, addNames []string) string {
	if sourceFile == "" {
		return s.autoEmitAndBuild()
	}
	return s.autoEmitAndBuildWithOpts(emit.Opts{
		GoimportsFiles: []string{sourceFile},
		TouchedFiles:   []string{sourceFile},
		AllowedAdds:    addNames,
	})
}

// isImportsOnlyBody reports whether the body parses as a valid Go
// file with zero user-defined decls (funcs/types/consts/vars) and
// zero or more import blocks. Used by handleCreate to route
// "scaffold this new file with just package + imports" through a
// separate no-def path instead of erroring on "couldn't infer name".
// Comment-only and package-only bodies also count — the intent is
// the same: create the file, defs will land in future calls.
func isImportsOnlyBody(body string) bool {
	src := "package x\n" + stripLeadingPackageDecl(strings.TrimSpace(body))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return false
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			return false
		}
	}
	return true
}

// handleCreateScaffoldFile authors an imports-only (or package-only /
// comment-only) file when handleCreate detects a body with no
// user-defined decls. The body is stored verbatim in file_sources so
// emit reproduces it byte-for-byte; on the next full sync any decls
// added by future ops will replace it. Requires args.File to be set —
// with no file target there's nowhere to write.
//
// Package name is derived from the body's `package X` declaration if
// present, else from the directory containing args.File. Module is
// resolved the same way as handleCreate (file → module → fallback to
// shortest-path).
func (s *server) handleCreateScaffoldFile(args createParam) (*sdkmcp.CallToolResult, any, error) {
	// Ensure the body has a package clause; if the caller wrote just
	// imports without one, prepend `package X` derived from the target
	// dir. Anything below the package clause stays verbatim.
	body := strings.TrimSpace(args.Body)
	pkgName := ""
	if strings.HasPrefix(body, "package ") {
		nl := strings.IndexByte(body, '\n')
		if nl < 0 {
			nl = len(body)
		}
		pkgName = strings.TrimSpace(strings.TrimPrefix(body[:nl], "package "))
	}
	if pkgName == "" {
		pkgName = filepath.Base(filepath.Dir(args.File))
		body = "package " + pkgName + "\n\n" + body
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}

	// Resolve module by file, then by explicit --module, then shortest
	// path. Same as handleCreateMultiDecl's new-package fallback.
	mod := s.findModuleByFile(args.File)
	if mod == nil && args.Module != "" {
		mod = s.findModule(args.Module)
	}
	if mod == nil {
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

	if err := s.backend.SetFileSource(mod.ID, args.File, body); err != nil {
		return errResult(fmt.Errorf("write file source: %w", err))
	}

	// Emit + goimports the touched file. No defs changed, but the
	// file_sources row is the emit-side truth so a scoped emit lands
	// this content on disk. Skip build gate — no def graph changed.
	buildResult := s.autoEmitOnly(args.File)
	if buildResult == "" {
		buildResult = "ok"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Scaffolded %s (%s) — %d bytes, no defs yet\n", args.File, mod.Path, len(body)))
	sb.WriteString("emit: " + strings.ToLower(strings.SplitN(buildResult, "\n", 2)[0]) + "\n")
	sb.WriteString("_add defs with follow-up `code(op:\"create\", file:\"" + args.File + "\", body:\"...\")` calls._\n")
	return textResult(sb.String()), nil, nil
}

// emitUsageLog writes a single JSON line per op stats event.
// Default sink: os.Stderr. Bench harnesses can redirect via
// DEFN_USAGE_LOG_FILE=<path>; when set, the line is appended to that
// file instead (opened once per call, so log rotation & long-running
// serves are both fine). DEFN_USAGE_LOG=off disables emission entirely
// — silences quiet invocations without conditionalizing every callsite.
// See #177 for the prefix_hash_100 / body_sha256 fields these logs
// carry; #180 depends on capturing this stream during bench runs.
func emitUsageLog(u usageStats) {
	if os.Getenv("DEFN_USAGE_LOG") == "off" {
		return
	}
	b, err := json.Marshal(u)
	if err != nil {
		return
	}
	line := fmt.Sprintf("defn-usage %s\n", b)
	if path := os.Getenv("DEFN_USAGE_LOG_FILE"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
			return
		}
		// Fall through to stderr on open failure so the signal isn't lost.
	}
	fmt.Fprint(os.Stderr, line)
}

// readAutoOutlineThreshold is the body-size in bytes above which a
// bare `code(op:"read", name:X)` auto-downgrades to the outline
// projection. #174 receipt: CLAUDE.md-level "outline first" nudges
// had negligible adoption (2 outline calls vs 27 reads in a 10-turn
// bench). Taking the choice away from the model at the server is the
// mechanism the receipt recommends. Escape hatches: full:true and
// mode:"body" both bypass; query-adaptive reads also bypass so
// their filtered-body path is preserved.
//
// 1500 is deliberately larger than outlineBodyThreshold (300) —
// outline crosses over below 300, but 300-1500 is still comfortable
// to read directly and downgrading there produces model confusion
// without meaningful savings.
const readAutoOutlineThreshold = 1500

// fileNarrativeMinDefs is the smallest def count a file needs before
// #212 bothers generating an architectural narrative for it -- small
// files are already fully understood from the plain per-def listing,
// so a Sonnet call there would be pure cost with no orientation win.
const fileNarrativeMinDefs = 5

// fileNarrative returns a cached or freshly-generated #212
// architectural narrative for sourceFile, or "" if unavailable (no
// co-processor configured, or generation failed) -- callers degrade
// gracefully to the existing per-def listing with no narrative
// prepended. Staleness is checked via a structural hash of the file's
// concatenated def bodies (sorted by name for determinism); a mismatch
// means some def in the file changed since the narrative was generated.
//
// Generation deliberately sources from doc+signature only, not full
// bodies -- keeps the generation call itself cheap and avoids ever
// loading the whole file wholesale, the same win mechanism #211 found
// (avoiding cache_creation cost on giant files) applied one level up
// from per-def reads to file-level orientation.
func (s *server) fileNarrative(ctx context.Context, sourceFile string, defs []store.Definition) string {
	sorted := make([]store.Definition, len(defs))
	copy(sorted, defs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var bodyBuf strings.Builder
	for _, d := range sorted {
		bodyBuf.WriteString(d.Body)
		bodyBuf.WriteString("\n")
	}
	currentHash := store.HashBodyStructural(bodyBuf.String())

	if existing, _ := s.backend.GetFileSummary(sourceFile); existing != nil && existing.BodyHash == currentHash {
		return existing.Narrative
	}

	var sourceBuf strings.Builder
	for _, d := range sorted {
		recv := formatReceiver(d.Receiver)
		sourceBuf.WriteString(fmt.Sprintf("// %s%s (%s)\n", recv, d.Name, d.Kind))
		if d.Doc != "" {
			sourceBuf.WriteString(d.Doc + "\n")
		}
		if d.Signature != "" {
			sourceBuf.WriteString(d.Signature + "\n")
		}
		sourceBuf.WriteString("\n")
	}
	question := "Summarize this file's architectural role in 2-3 sentences: what it contains, the main types/functions, and how they relate to each other."
	narrative, err := s.explainClient.Explain(ctx, question, sourceBuf.String())
	if err != nil || strings.TrimSpace(narrative) == "" {
		return ""
	}
	if len(sorted) > 0 {
		s.backend.SetFileSummary(sourceFile, sorted[0].ModuleID, &store.FileSummary{
			Narrative: narrative,
			BodyHash:  currentHash,
			Model:     "explain-co-processor",
		})
	}
	return narrative
}

// mergeDefsByID unions a and b, preserving a's order first, then any
// defs from b not already present in a (by ID). #216: lets handleSearch
// combine Stage 1 (name/signature LIKE) and Stage 2 (FTS body/doc)
// results instead of treating Stage 1 as an all-or-nothing gate.
func mergeDefsByID(a, b []store.Definition) []store.Definition {
	seen := make(map[int64]bool, len(a))
	for _, d := range a {
		seen[d.ID] = true
	}
	out := a
	for _, d := range b {
		if !seen[d.ID] {
			out = append(out, d)
			seen[d.ID] = true
		}
	}
	return out
}

// backfillNarratives is #200: extends #160's async-precompute pattern
// to the file/project narratives #211/#212 shipped, and #234: also
// warms directory/package-scope narratives, reusing the identical
// handleOverview(file:"pkg/dir") path -- derived from each source
// file's parent directory (deduped) rather than guessed at, since
// that's the exact enumeration #212 validation found the model calls
// far more often than per-file scopes. Those are generated
// synchronously on first overview() call at each scope -- this walks
// every source file, its parent directory, and the project scope
// once, right after startup ingest/resolve completes, so the caches
// are warm before any user-facing overview() call pays the LLM
// round-trip. Reuses the real overview()/projectOverview() code paths
// (discarding their returned results) rather than duplicating
// fileNarrative's caching/threshold logic -- the side effect
// (populating file_summaries / the project_narrative meta row) is the
// point.
//
// No-op when explainClient is nil (no ANTHROPIC_API_KEY) or when
// DEFN_ASYNC_BACKFILL=0 (#201) -- that flag already means "skip
// background LLM spend that fires automatically"; this is a second
// instance of exactly that spend, not a new category needing its own
// flag. Sequential, one file/dir at a time, same conservative
// rationale summary.Worker's stage-1 loop documents: this is
// backfill, not a latency-sensitive path, so there is no pressure to
// parallelize API round-trips.
func (s *server) backfillNarratives(ctx context.Context) {
	if s.explainClient == nil || envDisabled("DEFN_ASYNC_BACKFILL") {
		return
	}

	// Project-level: same side effect a real bare overview() call has.
	s.projectOverview(ctx)

	rows, err := s.backend.Query("SELECT DISTINCT source_file FROM file_sources")
	if err != nil {
		return
	}
	dirs := map[string]bool{}
	for _, row := range rows {
		f, ok := row["source_file"].(string)
		if !ok || f == "" {
			continue
		}
		s.handleOverview(ctx, nil, codeParam{File: f})
		if idx := strings.LastIndex(f, "/"); idx >= 0 {
			dirs[f[:idx]] = true
		}
	}
	for dir := range dirs {
		s.handleOverview(ctx, nil, codeParam{File: dir})
	}
}

// patchImportOnDisk adds importPath (aliased as alias, if set) to
// file's actual current source via projection.AddImport -- the #221
// mechanism, extracted so both the singleton handleAddImport and
// handleApply's batch "add-import" case (which must defer all disk
// writes until after its transaction commits) share one
// implementation. Idempotent: changed=false and no write when the
// import is already present.
func (s *server) patchImportOnDisk(moduleID int64, file, importPath, alias string) (changed bool, err error) {
	var fileSrc []byte
	if s.projectDir != "" {
		diskPath := filepath.Join(s.projectDir, file)
		data, rerr := os.ReadFile(diskPath)
		if rerr != nil {
			raw, rawErr := s.backend.GetFileSource(moduleID, file)
			if rawErr != nil || raw == "" {
				return false, fmt.Errorf("read %s: %w", file, rerr)
			}
			data = []byte(raw)
		}
		fileSrc = data
	} else {
		raw, rawErr := s.backend.GetFileSource(moduleID, file)
		if rawErr != nil || raw == "" {
			return false, fmt.Errorf("read %s: no projectDir and no file_sources entry", file)
		}
		fileSrc = []byte(raw)
	}
	updatedSrc, aerr := projection.AddImport(string(fileSrc), importPath, alias)
	if aerr != nil {
		return false, aerr
	}
	if updatedSrc == string(fileSrc) {
		return false, nil
	}
	if s.projectDir != "" {
		if err := os.WriteFile(filepath.Join(s.projectDir, file), []byte(updatedSrc), 0644); err != nil {
			return false, fmt.Errorf("write %s: %w", file, err)
		}
	}
	_ = s.backend.SetFileSource(moduleID, file, updatedSrc)
	return true, nil
}

// suggestMissingImportFixes scans a `go build` failure for Go's
// "undefined: X" diagnostic and, when X is defined by exactly one
// definition elsewhere in this project, appends a ready-to-use
// add-import hint -- the code-action-style quick fix an LSP would
// offer inline, adapted to defn's op vocabulary since there's no
// editor UI to click through.
func (s *server) suggestMissingImportFixes(buildOutput string) string {
	var hints []string
	seen := map[string]bool{}
	for _, line := range strings.Split(buildOutput, "\n") {
		m := undefinedRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		// go build paths for the current directory's own package commonly
		// carry a "./" prefix that source_file never does -- strip it so
		// both the DB lookup and the hint's own file: param stay usable.
		file := strings.TrimPrefix(m[1], "./")
		name := m[4]
		key := file + ":" + name
		if seen[key] {
			continue
		}
		seen[key] = true

		def, err := s.backend.GetDefinitionByName(name, "")
		if err != nil || !def.Exported {
			continue
		}
		modPath := s.modulePath(def.ModuleID)
		if modPath == "" {
			continue
		}
		dir := ""
		if idx := strings.LastIndex(file, "/"); idx >= 0 {
			dir = file[:idx]
		}
		if failingDefs, _ := s.backend.FindDefinitionsByFile(dir, file, 0); len(failingDefs) > 0 {
			if s.modulePath(failingDefs[0].ModuleID) == modPath {
				continue
			}
		}
		hints = append(hints, fmt.Sprintf("HINT: %q is undefined in %s -- found in package %q. Try code(op:\"add-import\", file:%q, import_path:%q).", name, file, modPath, file, modPath))
	}
	if len(hints) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(hints, "\n")
}

// undefinedRe matches go build's "undefined: X" diagnostic line:
// path/to/file.go:line:col: undefined: X
var undefinedRe = regexp.MustCompile(`^(\S+\.go):(\d+):(\d+): undefined: (\w+)$`)

// generatedCodeMarker matches Go's own generated-code convention
// (https://go.dev/s/generatedcode): a line matching this pattern exactly
// marks a file as machine-generated. Recognized by goimports, golint,
// and most other Go tooling -- reusing it here rather than inventing a
// new convention.
var generatedCodeMarker = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// isGeneratedSource reports whether raw's first few lines carry Go's
// standard generated-code marker. Checked against only the first 10
// lines -- the convention requires it near the top of the file, and
// capping the scan keeps this cheap even on huge generated files.
func isGeneratedSource(raw string) bool {
	lines := strings.SplitN(raw, "\n", 11)
	for i, line := range lines {
		if i >= 10 {
			break
		}
		if generatedCodeMarker.MatchString(strings.TrimRight(line, "\r")) {
			return true
		}
	}
	return false
}

// fileDefsCap bounds code(op:"file-defs") -- previously uncapped, so a
// huge file (not least a generated one, see isGeneratedSource) could
// dump hundreds of defs in one response with no truncation.
const fileDefsCap = 50

// pragmasCap bounds code(op:"pragmas") -- previously uncapped, and the
// default pattern ("%") matches every pragma comment in the project.
const pragmasCap = 30

// traverseResultCap bounds the default markdown rendering of
// code(op:"traverse") -- unlike impact/search, traverse had no cap at
// all before this: a densely-connected def walked 10 hops deep (the
// default max_depth) could render an unbounded number of results.
// format:"json" remains the intentional full-data escape hatch,
// mirroring handleImpact's convention -- only the default rendering is
// capped.
const traverseResultCap = 50

// dbDirSize returns the total size in bytes of everything under dir,
// or 0 on error. Used for reporting GC before/after size; not
// load-bearing.
func dbDirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// isStaleProjectDirError reports whether err looks like packages.Load
// failing because its Dir no longer resolves inside a Go module --
// the shape produced when a project is moved or renamed on disk after
// 'defn serve' captured s.projectDir at startup and never revalidates
// it. Verified empirically (2026-08-05) against cmd/go's real error
// text for a directory outside any module.
func isStaleProjectDirError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "does not contain main module")
}

// commitOrRollbackOnBuild closes the #12 gap: it snapshots the files
// opts will touch, runs emit+build against tx (so emit sees the
// batch's own uncommitted writes), and only commits tx if the result
// comes back completely clean. A non-empty result -- build failure OR
// an emit-level WARNING (#218 already treats those as failure-
// equivalent, since a WARNING means the DB write succeeded but the
// file write was refused or skipped) -- restores the snapshotted
// files and lets rollback() undo the DB write, so neither side
// reflects the failed mutation.
//
// Snapshotting is scoped to opts.TouchedFiles (falling back to
// GoimportsFiles); a caller with neither set gets no file-level
// protection here -- that's the existing full-project-emit fallback
// path, already rare, and out of scope for this pass (see #12).
//
// Concurrency tradeoff, worth being explicit about: SQLiteDB.Begin()
// serializes ALL transactions on a single process-wide mutex (txMu).
// Previously the DB write committed (and released that lock)
// immediately, before emit+build ran outside it. This function holds
// tx open for the full emit+build duration instead, so a slow build
// now also holds the lock that long -- trading some cross-request
// concurrency for the correctness this closes. Builds are normally
// scoped to just the touched package(s), so this is expected to be
// small in practice, but it is a real cost, not a free rearrangement.
func (s *server) commitOrRollbackOnBuild(tx store.Backend, commit func() error, rollback func(), opts emit.Opts) string {
	return s.commitOrRollbackOn(tx, commit, rollback, opts, s.emitAndBuildAgainst)
}

// commitOrRollbackOnEmit is commitOrRollbackOnBuild's emit-only
// counterpart, for callers that deliberately skip the go build check
// for performance (handleEdit's #148 signature-stable path) but still
// want the same snapshot/rollback protection against an emit-level
// WARNING -- #218 already treats those as failure-equivalent, since a
// WARNING means a requested change wasn't actually written to disk.
func (s *server) commitOrRollbackOnEmit(tx store.Backend, commit func() error, rollback func(), opts emit.Opts) string {
	return s.commitOrRollbackOn(tx, commit, rollback, opts, s.emitOnlyAgainst)
}

// commitOrRollbackOn is the shared #12 mechanism behind both
// commitOrRollbackOnBuild and commitOrRollbackOnEmit: snapshot the
// files opts will touch, run the given emit variant against tx (so it
// sees the caller's own uncommitted writes), and commit tx only if
// the result comes back completely clean. A non-empty result restores
// the snapshotted files and lets rollback() undo the DB write, so
// neither side reflects the failed mutation.
//
// Snapshotting is scoped to opts.TouchedFiles (falling back to
// GoimportsFiles); a caller with neither set gets no file-level
// protection here -- that's the existing full-project-emit fallback
// path, already rare, and out of scope for this pass (see #12).
//
// Concurrency tradeoff, worth being explicit about: SQLiteDB.Begin()
// serializes ALL transactions on a single process-wide mutex (txMu).
// Previously the DB write committed (and released that lock)
// immediately, before emit (+ build, where applicable) ran outside
// it. This holds tx open for the full emit/build duration instead, so
// a slow build now also holds the lock that long -- trading some
// cross-request concurrency for the correctness this closes. Builds
// are normally scoped to just the touched package(s), so this is
// expected to be small in practice, but it is a real cost, not a free
// rearrangement.
func (s *server) commitOrRollbackOn(tx store.Backend, commit func() error, rollback func(), opts emit.Opts, run func(store.Backend, emit.Opts) string) string {
	files := opts.TouchedFiles
	if len(files) == 0 {
		files = opts.GoimportsFiles
	}
	var snaps []fileSnapshot
	if len(files) > 0 {
		snaps = snapshotFiles(s.projectDir, files)
	}

	result := run(tx, opts)
	if result != "" {
		restoreFiles(snaps)
		rollback()
		return result
	}
	if err := commit(); err != nil {
		restoreFiles(snaps)
		return fmt.Sprintf("commit error: %v (on-disk changes reverted)", err)
	}
	return ""
}

// fileSnapshot captures a file's pre-mutation on-disk bytes (existed
// is false when the file didn't exist yet, e.g. a brand-new file from
// op:"create") so a build failure can restore it (#12).
type fileSnapshot struct {
	path    string
	existed bool
	content []byte
}

// restoreFiles reverts each snapshot taken by snapshotFiles: rewrites
// the original content, or removes the file if it did not exist
// before the emit being undone (#12). Best-effort -- a restore
// failure is logged, not returned, since the caller is already on the
// build-failure path and has no better recovery than reporting it.
func restoreFiles(snaps []fileSnapshot) {
	for _, snap := range snaps {
		if snap.existed {
			if err := os.WriteFile(snap.path, snap.content, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "defn: failed to restore %s after rollback: %v\n", snap.path, err)
			}
			continue
		}
		if err := os.Remove(snap.path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "defn: failed to remove %s after rollback: %v\n", snap.path, err)
		}
	}
}

// snapshotFiles reads the current on-disk content of each project-
// relative file under projectDir, before an emit that might overwrite
// or create them, so a failed build can restore the pre-mutation
// state via restoreFiles (#12).
func snapshotFiles(projectDir string, files []string) []fileSnapshot {
	snaps := make([]fileSnapshot, 0, len(files))
	for _, f := range files {
		path := filepath.Join(projectDir, f)
		data, err := os.ReadFile(path)
		if err != nil {
			snaps = append(snaps, fileSnapshot{path: path, existed: false})
			continue
		}
		snaps = append(snaps, fileSnapshot{path: path, existed: true, content: data})
	}
	return snaps
}

// resolveEditTarget finds the definition an edit (full-body or
// fragment) should target, using name plus optional receiver/module/
// file qualifiers to disambiguate same-named defs. receiver
// disambiguates methods sharing Name across different types (#219);
// module/file disambiguate non-method defs (e.g. two "Engine" structs)
// sharing Name across different packages -- the same gap, reported
// again by gemot dispatch since receiver alone can't help when there's
// no receiver at all. File wins when both are set (most specific),
// mirroring findModuleByFile/findModule's precedence in handleCreate.
// Both GetDefinitionByName and GetDefinitionByNameAndReceiver already
// accept a modulePath to scope by -- this was purely a dispatch-layer
// wiring gap, not a store-layer one.
//
// #241: a bare lookup can fail when the caller used Go's own
// "pkg.Symbol"/"pkg/path.Symbol" qualified-name convention -- a
// completely natural thing to reach for, especially once a bare name
// has already come back ambiguous or ill-fitting. Real trajectory
// (go-zero-2283): read(name:"rest/internal/cors.Middleware") came
// back "not found" even though Middleware exists right there, forcing
// an extra outline+file: round trip to recover. resolveDottedQualifiedName
// retries by splitting on the last "." and matching the prefix against
// source_file directly, not through findModuleByFile/findModule:
// store.Module is too coarse for this (per go.mod, not per package),
// so a package-shaped hint can't resolve through it on a single-module
// repo.
func (s *server) resolveEditTarget(name, receiver, module, file string) (*store.Definition, error) {
	var modulePath string
	if file != "" {
		if mod := s.findModuleByFile(file); mod != nil {
			modulePath = mod.Path
		}
	}
	if modulePath == "" && module != "" {
		if mod := s.findModule(module); mod != nil {
			modulePath = mod.Path
		}
	}

	if receiver == "" {
		d, err := s.backend.GetDefinitionByName(name, modulePath)
		if err == nil && d != nil {
			return d, nil
		}
		if dotted, derr := s.resolveDottedQualifiedName(name); derr == nil && dotted != nil {
			return dotted, nil
		}
		return d, err
	}

	d, err := s.backend.GetDefinitionByNameAndReceiver(name, modulePath, receiver)
	if err != nil {
		if alt := strings.TrimPrefix(receiver, "*"); alt != receiver {
			d, err = s.backend.GetDefinitionByNameAndReceiver(name, modulePath, alt)
		} else {
			d, err = s.backend.GetDefinitionByNameAndReceiver(name, modulePath, "*"+receiver)
		}
	}
	return d, err
}

// prependNote prepends note to a successful text result's content,
// mirroring handleCode's wrapStale helper. No-op on an error result or
// unexpected content shape.
func prependNote(r *sdkmcp.CallToolResult, note string) *sdkmcp.CallToolResult {
	if r != nil && !r.IsError && len(r.Content) > 0 {
		if tc, ok := r.Content[0].(*sdkmcp.TextContent); ok {
			tc.Text = note + tc.Text
		}
	}
	return r
}

// resolveApplyTarget mirrors resolveEditTarget's disambiguation
// precedence (receiver for same-named methods; module/file for
// same-named non-methods, file winning when both are set), but takes
// its backend explicitly so it works for both apply's tx (mid-batch,
// must see this batch's own uncommitted writes) and s.backend
// (dry-run preview, no transaction open yet). Motivated by a real
// trajectory that burned ~10 apply retries trying to disambiguate a
// same-named method with no receiver field on applyOp to put it in --
// resolveEditTarget itself is hardwired to s.backend and can't see a
// batch's own uncommitted writes, so it can't be reused as-is here.
func (s *server) resolveApplyTarget(backend store.Backend, name, receiver, module, file string) (*store.Definition, error) {
	var modulePath string
	if file != "" {
		if mod := s.findModuleByFile(file); mod != nil {
			modulePath = mod.Path
		}
	}
	if modulePath == "" && module != "" {
		if mod := s.findModule(module); mod != nil {
			modulePath = mod.Path
		}
	}
	if receiver == "" {
		d, err := backend.GetDefinitionByName(name, modulePath)
		if err != nil {
			return d, err
		}
		// #248: same refusal as resolveWriteTarget -- apply's edit/delete/
		// rename/projection sub-ops all funnel through this one function,
		// so this is the single place to close the ambiguous-bare-name
		// write gap for the whole apply surface, not just standalone ops.
		if module == "" && file == "" {
			if n, cErr := backend.CountDefinitionsByName(name); cErr == nil && n > 1 {
				return nil, fmt.Errorf(
					"%q is ambiguous: %d definitions share this name across different packages -- refusing to guess which one to write. Pass receiver:, module:, or file: to disambiguate, or use search(pattern:%q) to see every candidate",
					name, n, name,
				)
			}
		}
		return d, nil
	}
	d, err := backend.GetDefinitionByNameAndReceiver(name, modulePath, receiver)
	if err != nil {
		if alt := strings.TrimPrefix(receiver, "*"); alt != receiver {
			d, err = backend.GetDefinitionByNameAndReceiver(name, modulePath, alt)
		} else {
			d, err = backend.GetDefinitionByNameAndReceiver(name, modulePath, "*"+receiver)
		}
	}
	return d, err
}

// alreadyFreshlyIngested reports whether the DB's last_ingest already
// covers every .go file under projectDir, so newMCPServer's startup
// goroutine can skip a redundant full packages.Load+ingest+resolve.
//
// Without this check, every MCP session start pays that reload even
// when a CLI `defn ingest .` (e.g. `defn init`, or a bench harness's
// setup step) ran moments earlier and produced an identical DB --
// and worse, the in-flight reingest tears down and rebuilds the defs
// table, so a read-shaped op racing s.ready during that window can
// return actively wrong results (not just stale ones), with only a
// soft "may be stale" text warning a model can easily miss.
// Root-caused via a real grpc-go-2630 head-to-head-go trajectory: the
// first `search` call landed mid-reingest, returned unrelated defs
// (Server/rpcStats/errDropped instead of regeneratePicker), and the
// agent confidently edited the wrong function -- scored F1=0.00 while
// files-mode got partial credit on the same task.
//
// Returns false (must reingest) whenever last_ingest is missing or
// unparseable -- a never-ingested DB must never be treated as fresh.
// Mirrors the walk in cmd/defn's countStaleFiles/walkGoFiles and
// db.DB.StaleFiles; kept as its own small copy since internal/mcp
// can't import cmd/defn (package main), and this check is narrower
// than the full nested-module-aware walk ingest itself needs -- it
// only needs to know whether ANY covered file changed.
func alreadyFreshlyIngested(db store.Backend, projectDir string) bool {
	lastIngestStr, err := db.GetMeta("last_ingest")
	if err != nil || lastIngestStr == "" {
		return false
	}
	lastIngest, err := strconv.ParseInt(lastIngestStr, 10, 64)
	if err != nil {
		return false
	}
	fresh := true
	_ = filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, werr error) error {
		if !fresh {
			return filepath.SkipAll
		}
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".defn" || name == ".git" || name == "vendor" ||
				name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			if path != projectDir {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Unix() > lastIngest {
			fresh = false
		}
		return nil
	})
	return fresh
}

// testNamePattern matches a bare Go identifier -- used to decide whether
// a test:"..." pattern is plausibly a literal test name (safe to resolve
// via GetDefinitionByName) versus a regex/alternation the caller built
// deliberately (e.g. "TestFoo|TestBar", "TestFoo$"), which must not be
// treated as a literal name lookup.
var testNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (s *server) coupledChangeHint(defIDs ...int64) string {
	seen := map[string]bool{}
	var names []string
	for _, id := range defIDs {
		impact, err := s.backend.GetImpact(id)
		if err != nil {
			continue
		}
		for _, c := range impact.DirectCallers {
			if c.Test || seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			names = append(names, c.Name)
			if len(names) >= 3 {
				break
			}
		}
		if len(names) >= 3 {
			break
		}
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("\nTip: this def has %s (%s) -- if this build failure is from a coupled signature change, batch this edit together with a fix to it via op:\"apply\".\n",
		pluralizeCallers(len(names)), strings.Join(names, ", "))
}

// pluralizeCallers renders "a direct caller"/"direct callers" to match
// coupledChangeHint's single sentence for either count without a
// separate branch at each call site.
func pluralizeCallers(n int) string {
	if n == 1 {
		return "a direct caller"
	}
	return "direct callers"
}

// resolveDottedQualifiedName handles Go's own "pkg.Symbol"/
// "pkg/path.Symbol" qualified-name convention as a fallback when a
// bare name lookup fails -- see resolveEditTarget's doc comment for
// the full rationale and the real trajectory that motivated it.
// Returns (nil, nil) when name has no dot or nothing matches, letting
// the caller fall through to its own not-found error.
func (s *server) resolveDottedQualifiedName(name string) (*store.Definition, error) {
	idx := strings.LastIndex(name, ".")
	if idx <= 0 {
		return nil, nil
	}
	hint, bare := name[:idx], name[idx+1:]
	files, err := s.backend.DistinctSourceFiles()
	if err != nil {
		return nil, nil
	}
	for _, f := range files {
		if !strings.Contains(f, hint) {
			continue
		}
		// FilterDefinitions is metadata-only (its query hardcodes an
		// empty body column) -- fetch the full definition by ID once it
		// has located the right one.
		matches, merr := s.backend.FilterDefinitions(bare, "", f, 1)
		if merr != nil || len(matches) == 0 {
			continue
		}
		if full, gerr := s.backend.GetDefinition(matches[0].ID); gerr == nil && full != nil {
			return full, nil
		}
		return &matches[0], nil
	}
	return nil, nil
}

// testScopeTarget resolves a source-file hint (substring match against
// DistinctSourceFiles) to a `go test` target, scoping the run to one
// package instead of the whole repo. A hint resolving to the module
// root package returns ".", not "./..." -- the naive dir!="." check
// used to silently skip scoping for root-package tests, falling through
// to the full-repo default it exists to avoid (#248). Best-effort: an
// empty or unresolvable hint returns "./...", never an error.
func (s *server) testScopeTarget(hint string) string {
	if hint == "" {
		return "./..."
	}
	// hint may be a full Go module/import path rather than a source-file
	// substring -- the same shape "module:" takes everywhere else in this
	// API (e.g. code(op:"impact", module:"github.com/x/y/z")), which a
	// caller reasonably expects to work here too. That never
	// substring-matches a repo-relative source_file path below, so it
	// silently fell through to "./..." every time -- confirmed via a real
	// prometheus-18534 trajectory where an explicit module: hint was
	// ignored 3 calls in a row, each falling back to a whole-repo `go
	// test ./...` that exhausted the box's disk compiling every unrelated
	// cloud-SDK dependency. Try an exact module-path match first, using
	// the same moduleRoot-stripping emitModule already does for on-disk
	// paths.
	if mods, err := s.backend.ListModules(); err == nil {
		for _, m := range mods {
			if m.Path != hint {
				continue
			}
			root := emit.DetectModuleRoot(mods)
			rel := strings.TrimPrefix(m.Path, root)
			rel = strings.TrimPrefix(rel, "/")
			if rel == "" {
				return "."
			}
			return "./" + rel + "/..."
		}
	}
	files, err := s.backend.DistinctSourceFiles()
	if err != nil {
		return "./..."
	}
	// DistinctSourceFiles has no ORDER BY -- picking the first substring
	// match was picking whichever match SQLite happened to return first,
	// with no preference for the package the hint actually names. A
	// short hint like "tsdb" substring-matches every file under it too
	// (tsdb/encoding/x.go, tsdb/wlog/y.go, ...), so it could resolve to
	// an arbitrary, wrong subdirectory instead of the tsdb package root
	// -- confirmed via a real prometheus-19114 trajectory where
	// module:"tsdb" scoped to "./tsdb/encoding/..." and never found the
	// target test living in tsdb/db_test.go, burning several dead test
	// calls before giving up and falling back to guessing more names.
	// Score every match and keep the best: an exact trailing path
	// component (dir == hint or dir ends in "/"+hint) beats a mere
	// substring match, and among equally-exact (or equally-inexact)
	// matches the shallowest directory wins.
	bestDir, bestDepth, bestExact, found := "", -1, false, false
	for _, f := range files {
		if !strings.Contains(f, hint) {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(f))
		if dir == "" {
			dir = "."
		}
		exact := dir == hint || strings.HasSuffix(dir, "/"+hint)
		depth := strings.Count(dir, "/")
		if !found || (exact && !bestExact) || (exact == bestExact && depth < bestDepth) {
			bestDir, bestDepth, bestExact, found = dir, depth, exact, true
		}
	}
	if !found {
		return "./..."
	}
	if bestDir == "." {
		return "."
	}
	return "./" + bestDir + "/..."
}

// overviewDefsCap bounds code(op:"overview") at directory/module scope --
// previously uncapped, so a large package's overview could exceed the MCP
// response token limit outright (a real trajectory hit a hard "268,223
// characters... exceeds maximum allowed tokens" failure on one call).
// Matches fileDefsCap's cap value for the sibling file-scoped op.
const overviewDefsCap = fileDefsCap

// resolveWriteTarget wraps resolveEditTarget for MUTATING ops. #248:
// a bare (no receiver/module/file) name lookup uses
// GetDefinitionByName's best-effort blast-radius tiebreak, which is
// fine for a read (worst case: stale info) but not for a write --
// live-reproduced this session when code(op:"edit", name:"Backend")
// with no module qualifier silently targeted the wrong one of two
// same-named "Backend" interfaces across different packages and
// overwrote it with the other's body, corrupting a file that was
// never meant to be touched. Refuse instead of guessing whenever the
// name is ambiguous and the caller gave no disambiguating qualifier.
func (s *server) resolveWriteTarget(name, receiver, module, file string) (*store.Definition, error) {
	d, err := s.resolveEditTarget(name, receiver, module, file)
	if err != nil {
		return d, err
	}
	if receiver == "" && module == "" && file == "" {
		if n, cErr := s.backend.CountDefinitionsByName(name); cErr == nil && n > 1 {
			return nil, fmt.Errorf(
				"%q is ambiguous: %d definitions share this name across different packages -- refusing to guess which one to write. Pass receiver:, module:, or file: to disambiguate, or use search(pattern:%q) to see every candidate",
				name, n, name,
			)
		}
	}
	return d, nil
}

// ambiguityNote warns when a bare-name lookup (no receiver/module/file
// qualifier) resolved via GetDefinitionByName's best-effort tiebreak
// while other same-named definitions exist -- #248: outline/read gave
// no indication another candidate existed, unlike search which lists
// every match. Empty qualifiers only: an explicit receiver/module/file
// is a deliberate disambiguation, not something to second-guess.
func (s *server) ambiguityNote(name, receiver, module, file string) string {
	if receiver != "" || module != "" || file != "" || strings.TrimSpace(name) == "" {
		return ""
	}
	n, err := s.backend.CountDefinitionsByName(name)
	if err != nil || n <= 1 {
		return ""
	}
	return fmt.Sprintf(
		"[note: %d definitions share the name %q; this resolved to one via a best-effort tiebreak (most production callers). Pass receiver:/module:/file: to target a specific one, or search(pattern:%q) to see every candidate.]\n\n",
		n, name, name,
	)
}

// testMatchedNothing reports whether a `go test -run <pattern>` invocation
// that exited 0 actually ran zero tests -- Go's own behavior when a -run
// pattern matches nothing: exit code 0, output just warns "no tests to
// run". Without this check, handleTest/handleTestByName reported "ALL
// TESTS PASSED" for a run that verified nothing at all -- confirmed via a
// real trajectory where a grpctest-suite method (addressed by go test as
// a subtest, Test/TestFoo, not TestFoo) was targeted by its bare method
// name, silently matched zero tests, and gave the agent false confidence
// nothing needed further investigation.
//
// A bare substring check on "no tests to run" over-fires whenever the
// scope target is a recursive "./pkg/..." with siblings: Go prints that
// warning per-package for every subpackage under pkg that doesn't have a
// matching test, even when the intended package's test ran and passed
// fine. Confirmed via a real prometheus-19114 trajectory: `go test -run
// TestQuerier ./tsdb/...` correctly ran and passed TestQuerier in tsdb
// itself, but tsdb/encoding (a sibling with no such test) also printed
// "no tests to run" -- the bare substring check flagged the whole run as
// "NO TESTS MATCHED", discarding a real pass and sending the agent back
// to guessing test names. Only report "matched nothing" when literally
// no test executed anywhere in the combined output.
func testMatchedNothing(out string) bool {
	if !strings.Contains(out, "no tests to run") {
		return false
	}
	return !strings.Contains(out, "=== RUN") &&
		!strings.Contains(out, "--- PASS:") &&
		!strings.Contains(out, "--- FAIL:")
}

// testBuildFailed reports whether a `go test` invocation failed because
// the target package didn't compile (or setup failed), not because any
// test actually ran and failed. Go's own output distinguishes this with
// a literal "[build failed]"/"[setup failed]" marker after FAIL, but
// handleTest/handleTestByName used to fold this into the same generic
// "SOME TESTS FAILED" as a genuine test failure -- confirmed via a real
// cli-3997 trajectory where a pre-existing vet error produced exactly
// this indistinguishable message, even though zero tests ran.
func testBuildFailed(out string) bool {
	return strings.Contains(out, "[build failed]") || strings.Contains(out, "[setup failed]")
}

// testPanicked reports whether a `go test` invocation's output shows the
// test binary crashed (e.g. a global-registration panic unrelated to the
// edited def), not that any test made a failing assertion. Confirmed via
// a real cli-405 trajectory: a duplicate-flag-registration panic in an
// unrelated shared Cobra command tree got the same generic "SOME TESTS
// FAILED" label as a genuine assertion failure, inviting a wrong-edit
// suspicion instead of pointing at the actual crash. Requires both
// markers (not just "panic: ") to avoid a false positive on a test that
// merely prints or asserts on the word "panic".
func testPanicked(out string) bool {
	return strings.Contains(out, "panic: ") && strings.Contains(out, "goroutine ")
}

// removedDoltOps names ops that existed under defn's pre-v0.27 Dolt
// backend (git-style branch/merge/commit/diff on definitions) and were
// deliberately dropped in the SQLite migration -- see
// docs/lessons-learned.md's "Key Design Decisions" entry. handleCode
// checks this before dispatch so these get one clear, specific answer
// instead of the generic "unknown op" fallthrough.
var removedDoltOps = map[string]bool{
	"branch":      true,
	"checkout":    true,
	"merge":       true,
	"commit":      true,
	"status":      true,
	"conflicts":   true,
	"resolve":     true,
	"merge-abort": true,
	"diff":        true,
	"diff-defs":   true,
	"history":     true,
}

// impactJSONCap bounds each list in impactJSON's output (direct_callers,
// interface_dispatch_callers, tests). format:"json" exists specifically
// as the markdown path's own escape hatch ("... N more production
// callers omitted; pass format:\"json\" for full list") -- but until
// this cap existed, the escape hatch had no cap of its own. A real
// trajectory (prometheus-18652, 2026-08-10) hit a def with 1,314
// covering tests: impactJSON dumped all of them uncapped, producing a
// 243,019-character/9,473-line response that exceeded the harness's own
// tool-result size limit and got redirected to a file the agent never
// successfully paged through -- the "full list" was less useful than
// the capped markdown view it was supposed to supplement. Higher than
// impactCallerCap (15) since JSON is the deliberately-fuller view, but
// still bounded.
const impactJSONCap = 200

// opAliases maps common near-miss op-name guesses to the one real op
// they unambiguously mean, so handleCode accepts them instead of
// round-tripping an "unknown op" error. Confirmed hitting real
// trajectories repeatedly (prometheus-17395/18765/18972/18841):
// "import"/"add_import"/"import_path" for "add-import" -- every other
// multi-word op name uses hyphens (add-import, replace-hunk,
// insert-precondition), but underscore is the natural
// programming-language-convention guess, and "import" alone is the
// obvious short name for "add an import". Same precedent as this
// file's other near-miss acceptances (old_fragment/new_fragment on
// replace-hunk, query: as a pattern alias on search) -- fix the
// mismatch instead of just explaining it in an error message.
var opAliases = map[string]string{
	"import":      "add-import",
	"add_import":  "add-import",
	"addimport":   "add-import",
	"import_path": "add-import",
}

// testTimeoutFor scales the test timeout for genuinely large runs
// instead of applying a flat one-size-fits-all default -- confirmed
// hitting real prometheus trajectories repeatedly (19114/17395/18534
// in a live rerun): "TIMED OUT after 1m0s" on runs covering 191-1166
// affected tests, or a whole-repo "./..." scope fallback, wasting a
// full retry with no path for the caller to recover. The timeout
// message's own suggested remedy ("set DEFN_TEST_TIMEOUT=<duration>")
// is structurally unactionable from inside a session -- it's a
// server-side env var the MCP caller has no way to set mid-call. Only
// scales the DEFAULT; an operator's explicit DEFN_TEST_TIMEOUT is
// honored exactly as set, never extended on top of their choice.
func testTimeoutFor(nTests int, target string) time.Duration {
	if os.Getenv("DEFN_TEST_TIMEOUT") != "" {
		return testTimeout
	}
	if nTests <= 50 && target != "./..." {
		return testTimeout
	}
	scaled := testTimeout * 3
	const maxTestTimeout = 5 * time.Minute
	if scaled > maxTestTimeout {
		return maxTestTimeout
	}
	return scaled
}

// suggestClosestFragmentHint tries a whitespace-insensitive match of
// old against body when the byte-exact replace-hunk match failed, and
// if found, returns a hint showing the ACTUAL bytes at that location so
// the caller can copy them verbatim instead of guessing narrower
// fragments blind. Confirmed hitting a real trajectory
// (prometheus-18712): replace-hunk's success response never shows the
// resulting body, so an agent making many sequential edits to the same
// large function has no way to see what changed -- it burned 17 of 34
// replace-hunk calls on bare "hunk not found in body" errors, each
// retry narrowing old to a smaller guess rather than converging,
// almost certainly because its remembered fragment's whitespace/
// indentation no longer matched the real bytes after prior edits.
// Joins old's words with \s+ so it matches regardless of exact
// whitespace differences, then returns the real substring straight
// from body -- no index-mapping needed.
func suggestClosestFragmentHint(body, old string) string {
	fields := strings.Fields(old)
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = regexp.QuoteMeta(f)
	}
	re, err := regexp.Compile(strings.Join(quoted, `\s+`))
	if err != nil {
		return ""
	}
	loc := re.FindStringIndex(body)
	if loc == nil {
		return ""
	}
	actual := body[loc[0]:loc[1]]
	if actual == old {
		return ""
	}
	return fmt.Sprintf("\n\nhint: old didn't match exactly, but a whitespace-only-different version was found in the current body -- copy this verbatim as old:\n%s", actual)
}

// runBuildIn runs `go build` against targets with dir as the working
// directory, returning combined output and the command's error (nil
// on a clean build).
func (s *server) runBuildIn(ctx context.Context, dir string, targets []string) (string, error) {
	args := append([]string{"build"}, targets...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runScopedBuild runs `go build` scoped to touchedFiles (repo-relative
// paths), grouping them by nearest go.mod so files inside a nested
// module (e.g. etcd's server/, tests/, etcdctl/, each with their own
// go.mod) build against THEIR OWN module root instead of
// s.projectDir's. Go's toolchain refuses to build a subtree that
// declares its own go.mod from an outer module's context -- "main
// module (X) does not contain package Y" -- which is exactly the
// build-verification failure real multi-module bench trajectories
// hit on every create/edit touching such a subtree, even once the
// definition's own module attribution (see ingest.ModuleForDir) was
// correct. Empty touchedFiles keeps the original full-tree ./...
// behavior, scoped to s.projectDir.
func (s *server) runScopedBuild(ctx context.Context, touchedFiles []string) (string, error) {
	if len(touchedFiles) == 0 {
		return s.runBuildIn(ctx, s.projectDir, []string{"./..."})
	}

	byModDir := map[string][]string{} // nearest module dir -> repo-relative files
	for _, f := range touchedFiles {
		absDir := filepath.Join(s.projectDir, filepath.Dir(f))
		modDir := s.projectDir
		if _, nearestDir, err := ingest.ModuleForDir(absDir); err == nil {
			modDir = nearestDir
		}
		byModDir[modDir] = append(byModDir[modDir], f)
	}

	dirs := make([]string, 0, len(byModDir))
	for d := range byModDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var outputs []string
	for _, modDir := range dirs {
		relFiles := make([]string, len(byModDir[modDir]))
		for i, f := range byModDir[modDir] {
			absFile := filepath.Join(s.projectDir, f)
			rel, err := filepath.Rel(modDir, absFile)
			if err != nil {
				rel = f
			}
			relFiles[i] = rel
		}
		targets := buildTargetsForFiles(relFiles)
		out, err := s.runBuildIn(ctx, modDir, targets)
		if err != nil {
			outputs = append(outputs, out)
			return strings.Join(outputs, "\n\n"), err
		}
	}
	return "", nil
}
