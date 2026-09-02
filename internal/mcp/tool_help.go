package mcp

import (
	"context"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolDescription picks the "code" tool's wire description. Default is
// legacyToolDescription (unchanged behavior) until the powered A/B (gap
// analysis 2026-09-02, docs/gap-analysis-2026-09-02.md work-order item
// 6) confirms leanToolDescription end to end -- DEFN_STRIP=verbose-tool-desc
// opts a session into the lean version early for that A/B, same
// mechanism as every other DEFN_STRIP feature flag (see stripped's doc
// comment). Read once at server startup (like the other env-gated
// choices in newMCPServer), not per-call -- this isn't a live runtime
// toggle.
//
// Why this exists: an in-process tools/list probe (2026-09-02) measured
// the "code" tool's real wire JSON at 13,915 bytes / 3,171 tokens, of
// which legacyToolDescription alone is 8,950 bytes / ~2.1k tokens --
// cache-read on every single API call for the life of a session. Against
// real per-task call counts from bench/prometheus-repo-opus/arm_defn
// (mean 50.2 calls/task) and bench/etcd-multifile-v2/arm_defn (27.3
// calls/task), that description tax alone accounts for ~87% and ~36%
// respectively of defn's measured per-task cost gap vs files-mode. The
// long-form per-op guidance below didn't disappear -- it moved to
// opHelp, served on demand via op:"help", topic:"<op>", and
// auto-discoverable from the tool description's own pointer to it.
func toolDescription() string {
	if stripped("verbose-tool-desc") {
		return leanToolDescription
	}
	return legacyToolDescription
}

// handleHelp returns the full usage text for one op (topic:"<op>"), or,
// with no topic, the list of every valid topic -- the long-form
// per-op guidance that used to live inline in the tool description
// (see toolDescription's doc comment for why it moved here).
func (s *server) handleHelp(_ context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	topic := strings.TrimSpace(args.Topic)
	if topic == "" {
		return textResult(fmt.Sprintf("Pass topic:\"<op>\" for full usage of one op. Valid topics:\n%s", strings.Join(opHelpOrder, ", "))), nil, nil
	}
	if canonical, ok := opAliases[topic]; ok {
		topic = canonical
	}
	text, ok := opHelp[topic]
	if !ok {
		return errResult(fmt.Errorf("help: unknown topic %q -- call op:\"help\" with no topic to list valid topics", topic))
	}
	return textResult(fmt.Sprintf("%s: %s", topic, text)), nil, nil
}

var leanToolDescription = `**USE THIS FOR ANY .go FILE — NOT Read/Bash/Grep/Edit.** Indexes every Go definition as an atomic unit with callers/tests/refs; every edit keeps the reference graph consistent (Edit/Write on a .go file leave it stale until a sync). Use Read/Bash/Grep/Edit ONLY for non-Go files (yaml, json, md, sh, go.mod, Dockerfile).

Orient before you read: overview (project shape) -> outline (def shape) -> impact (blast radius). For "how does X work" questions use context(question:"..."). Call help(topic:"<op>") for full usage of any op below -- required fields, gotchas, safety semantics live there, not here.

Read/discover: overview, outline, search, impact, read, read-file, read-and-verify, expand, slice, similar, untested, explain, context, methods, file-defs, test-coverage, batch-impact, traverse, literals, pragmas.
Write: edit, insert, insert-header, create, delete, rename, move, retarget-field-value, insert-precondition, replace-slice, replace-hunk, wrap-in-defer, rename-param, add-import, patch, apply.
Plan/verify: plan, plan-dsl, plan-sexpr, validate-plan, simulate.
Other: test, sync, query, find, emit, resummarize, version, help.`

var legacyToolDescription = `**USE THIS FOR ANY .go FILE — NOT Read/Bash/Grep/Edit.** This tool indexes every Go definition in the project as an atomic unit with its callers, tests, and references. Every op returns strictly less than Read/Bash/Grep for the same information; every edit updates the reference graph atomically. Editing Go via Edit/Write leaves the graph stale until a follow-up sync — slower and more error-prone than starting here.

Use Read/Bash/Grep/Edit ONLY for non-Go files (yaml, json, md, sh, go.mod, Dockerfile). Any question about Go code — "what does X do?", "who calls Y?", "rename Z", "what's the shape of pkg/foo?" — starts here.

For any "how does X work in this codebase" discovery question, reach for context (op:"context" question:"...") — returns one bundled response with top-N relevant defs outlined + refs graph + optional Sonnet synthesis. Replaces 10-40 sequential exploration calls in one round-trip. Both context and explain(question:...) cache on exact question text — phrase it tersely and reuse identical wording on a repeat ask rather than rephrasing; context's cache is session-scoped, explain's persists across sessions.

Orient before you read: overview (project shape) → outline (def shape) → impact (when you know which def matters). Only read whole bodies when you're about to edit them; whole-file reads on files you won't touch are pure wire cost — use outline or search instead.

Ops: overview (project-wide shape when called with no args — one line per module with def counts + first exported names; pass file:"pkg-path" or file:"pkg-path/file.go" to drill in; the right first-touch when you don't know which def matters yet), outline (compact projection of a def — sig + doc + caller/callee summary, no body; use when body isn't needed), search, impact (blast radius of a known def — pass format:"json" for structured output; callers, transitives, test coverage in one call), read (returns the def body + a compact "Related" footer with summary, top-3 callers, top-3 callees, and semantically-adjacent defs — one call gives you what would otherwise take 3-4 sequential impact/outline calls; auto-downgrades to outline when body > 1500 bytes, pass full:true or mode:"body" to force; for a large body, pass line_range:"700-820" (1-indexed, file-relative, "-" or ":" both accepted) to get back just that span instead — also bypasses the auto-downgrade, and composes with query: to filter further within the returned range), read-and-verify (read a def AND run its covering tests in one call — use during bug triage so you see behavior alongside source and don't spiral into read-loops; pass name), read-file (all defs' bodies in one file — pass file:"path"; whole-file counterpart to read; prefer over N sequential read calls when scanning; for a large file, pass line_range:"700-820" the same as read to keep only the definitions overlapping that file-relative range, each narrowed to its own overlapping span), expand (bundle a def's outline/body/callers in one call — pass name:"F", or names:["A","B",...] to batch several targets into ONE response instead of one expand per target; include:["outline","callers","body"] controls sections, defaults to outline+callers; prefer over read+impact+read chains AND over N sequential single-name expand calls — round-trip count within a turn is the dominant session cost driver, not per-call size), slice (verbatim AST-role slice of a def — pass slice:"signature"|"doc"|"body"|"error-branch"|"return"|"loop" to get just that piece), insert-precondition (insert an if-block at function entry — byte-exact PUTGET; pass name+condition+ret), replace-slice (replace the Nth AST-role slice with verbatim bytes — byte-exact PUTGET; pass name+slice+index+new; refuses if replacement would discard interior comments — pass force:true to override), replace-hunk (replace a byte-exact occurrence of 'old' inside a def body with 'new' — byte-exact PUTGET, content-addressed inside the def; pass name+old+new, plus index=1..N if 'old' occurs more than once, or replace_all:true to replace every occurrence with the same 'new' in one call — prefer replace_all over N indexed calls in one apply batch when every occurrence gets the identical replacement, since indices shift as earlier matches are consumed within the same batch; empty 'new' deletes the hunk(s). Send zero anchor context when the hunk is def-unique — the name argument does the file-level disambiguation), wrap-in-defer (insert defer stmt before Nth top-level statement — byte-exact PUTGET; pass name+stmt_index+defer_body), rename-param (rename value param or receiver via ast.Object scoping — ≡_gofmt equivalence; pass name+old_param+new_param), add-import (add import path to file's module — goimports-canonical grouping (stdlib / third-party); pass import_path+file?+alias? — file inferred if DB has one non-test .go file; safe to call unconditionally — it checks the file itself and no-ops if the import is already present, so don't burn a search call pre-checking first: search doesn't index import specs and will falsely report "no matches"), explain (structural analysis of a def — pass name to get sig + callers + callees + test coverage; ALSO accepts question:"how does X handle Y" which routes to a Sonnet co-processor that returns a prose answer grounded in the def's source with provenance refs. Cheaper than reading + interpreting a large body yourself when the answer is prose, not code. names:["A","B"] for multi-def scope. Requires ANTHROPIC_API_KEY), plan (pass intent:"..." — the co-processor grounds your intent in real defs via context's own candidate search, emits a compact trajectory, and defn mechanically walks it server-side in one round-trip instead of you sequencing several read/outline/impact calls yourself. Requires ANTHROPIC_API_KEY; falls back to a clear error pointing at plan-dsl/plan-sexpr when unset), plan-dsl / plan-sexpr (mechanically walk a trajectory YOU already wrote — pass plan:"..." in the compact DSL "@Def.field[!test]" form (plan-dsl) or the S-expression "(op target [!test])" form (plan-sexpr), op one of read/outline/impact; no API key needed), similar, untested, edit (full body OR old_fragment+new_fragment), insert (after anchor), create (single def from body; with file: set, body may hold multiple top-level decls to author a whole file in one call — the whole-file equivalent of files-mode Write), delete (safe by default — refuses when other defs still reference this def; pass force:true to delete anyway. Refusal message lists the callers so you can rewrite them first. Pass file:"path/to/x.go" with no name: to bulk-delete every definition in that file in one call — same safety check, scoped to callers outside the file; the file itself is NOT removed from disk by default, only its defs — pass remove_file:true alongside file: to also delete the file itself once its defs are purged), retarget-field-value (rewrite a composite-literal field's string value across every def whose body matches — pass name:"<StructType>" field:"<Field>" old:"<oldStr>" new:"<newStr>"; AST-safe, so unrelated occurrences of the string won't match), rename, move, test (run ONLY tests that cover a given def — pass name; scoped subset, not the full suite; prefer over bash 'go test ./...' when you only need coverage for a specific change. Also accepts test:"TestX" to run one test by name — use this to REPRODUCE a bug from the issue BEFORE writing any code; a passing test means your hypothesis about which def is broken is wrong. An identical test call repeated with no write in between this session returns the cached result instead of rerunning the real subprocess — pass force:true to force a genuine rerun anyway), apply (batch multiple ops atomically in one turn — accepts create/edit/delete/rename PLUS all 6 projection ops insert-precondition/replace-slice/replace-hunk/wrap-in-defer/rename-param/add-import; rolls back on any error; one emit+build for the whole batch), diff, history, find, sync (rarely needed — every edit op auto-syncs the DB; only use after external file changes outside the code tool), query (raw SQL escape hatch — for schema analytics only; NEVER use to look up a def by name, grep bodies, or list files/defs-in-file — use search/outline/read-file/file-defs/impact instead, which are far cheaper on the wire), patch, simulate, validate-plan, pragmas (query comment pragmas), literals (query composite literal fields), traverse (recursive graph traversal), commit (snapshot current state), status (current branch + dirty state), diff-defs (definitions that differ between two refs — pass from:"X" and optionally to:"Y"; defaults to working tree), version (no params — running build's version string + on-disk binary path/mtime; call this after a rebuild+reconnect to confirm you're actually talking to fresh code, not a stale already-running serve process)`

var opHelp = map[string]string{
	"add-import":           `add import path to file's module — goimports-canonical grouping (stdlib / third-party); pass import_path+file?+alias? — file inferred if DB has one non-test .go file; safe to call unconditionally — it checks the file itself and no-ops if the import is already present, so don't burn a search call pre-checking first: search doesn't index import specs and will falsely report "no matches"`,
	"apply":                `batch multiple ops atomically in one turn — accepts create/edit/delete/rename PLUS all 6 projection ops insert-precondition/replace-slice/replace-hunk/wrap-in-defer/rename-param/add-import; rolls back on any error; one emit+build for the whole batch`,
	"batch-impact":         `pass names:["A","B",...] — impact/blast-radius for several defs in ONE call instead of N sequential impact calls`,
	"context":              `pass question:"..." — returns one bundled response with top-N relevant defs outlined + refs graph + optional Sonnet synthesis. Replaces 10-40 sequential exploration calls in one round-trip. Caches on exact question text (session-scoped) — reuse identical wording on a repeat ask rather than rephrasing.`,
	"create":               `single def from body; with file: set, body may hold multiple top-level decls to author a whole file in one call — the whole-file equivalent of files-mode Write`,
	"delete":               `safe by default — refuses when other defs still reference this def; pass force:true to delete anyway. Refusal message lists the callers so you can rewrite them first. Pass file:"path/to/x.go" with no name: to bulk-delete every definition in that file in one call — same safety check, scoped to callers outside the file; the file itself is NOT removed from disk by default, only its defs — pass remove_file:true alongside file: to also delete the file itself once its defs are purged`,
	"edit":                 `pass name + new_body (full replace) OR name + old_fragment + new_fragment (fragment edit, empty new_fragment deletes the fragment)`,
	"expand":               `bundle a def's outline/body/callers in one call — pass name:"F", or names:["A","B",...] to batch several targets into ONE response instead of one expand per target; include:["outline","callers","body"] controls sections, defaults to outline+callers; prefer over read+impact+read chains AND over N sequential single-name expand calls — round-trip count within a turn is the dominant session cost driver, not per-call size`,
	"explain":              `structural analysis of a def — pass name to get sig + callers + callees + test coverage; ALSO accepts question:"how does X handle Y" which routes to a Sonnet co-processor that returns a prose answer grounded in the def's source with provenance refs. Cheaper than reading + interpreting a large body yourself when the answer is prose, not code. names:["A","B"] for multi-def scope. Requires ANTHROPIC_API_KEY`,
	"file-defs":            `pass file:"path" — list every definition in a file with kind + line range, no bodies; lighter than read-file when you just need the shape of a file, not its source`,
	"find":                 `pass file — locate a file's definitions/module context (lighter probe than read-file when you just need to confirm the file is indexed)`,
	"help":                 `pass topic:"<op>" for the full usage of that op (required fields, gotchas, safety semantics) — this text. No topic: lists every valid topic.`,
	"impact":               `blast radius of a known def — pass format:"json" for structured output; callers, transitives, test coverage in one call`,
	"insert":               `pass name (existing def to anchor after) + after + body — insert a new definition immediately after an existing one`,
	"insert-header":        `pass file + body — prepends body before any existing content in the file (e.g. a license header)`,
	"insert-precondition":  `insert an if-block at function entry — byte-exact PUTGET; pass name+condition+ret`,
	"literals":             `pass name:"<Type>" or file:"path" — query composite-literal field values across the codebase (AST-aware, so unrelated string occurrences don't match); see also op:"retarget-field-value" to rewrite them`,
	"methods":              `pass name:"<Type>" — list every method on a type or interface, including ones satisfying it structurally`,
	"move":                 `pass name + module — move a definition to a different package/module, updating every import site that referenced it`,
	"outline":              `compact projection of a def — sig + doc + caller/callee summary, no body; use when body isn't needed`,
	"overview":             `project-wide shape when called with no args — one line per module with def counts + first exported names; pass file:"pkg-path" or file:"pkg-path/file.go" to drill in; the right first-touch when you don't know which def matters yet`,
	"patch":                `low-level def-body replace primitive resolved via name/receiver/module/file (not old_name/new_name like rename) -- prefer edit/rename/replace-hunk for the common cases; patch backs apply's internal rename handling for the composite case`,
	"plan":                 `pass intent:"..." — the co-processor grounds your intent in real defs via context's own candidate search, emits a compact trajectory, and defn mechanically walks it server-side in one round-trip instead of you sequencing several read/outline/impact calls yourself. Requires ANTHROPIC_API_KEY; falls back to a clear error pointing at plan-dsl/plan-sexpr when unset`,
	"plan-dsl":             `mechanically walk a trajectory YOU already wrote — pass plan:"..." in the compact DSL "@Def.field[!test]" form (plan-dsl) or the S-expression "(op target [!test])" form (plan-sexpr), op one of read/outline/impact; no API key needed`,
	"plan-sexpr":           `mechanically walk a trajectory YOU already wrote — pass plan:"..." in the compact DSL "@Def.field[!test]" form (plan-dsl) or the S-expression "(op target [!test])" form (plan-sexpr), op one of read/outline/impact; no API key needed`,
	"pragmas":              `pass pattern to match a pragma key (e.g. a "//defn:key val" comment marker) — query comment pragmas project-wide; file/limit narrow the scope`,
	"query":                `raw SQL escape hatch — for schema analytics only; NEVER use to look up a def by name, grep bodies, or list files/defs-in-file — use search/outline/read-file/file-defs/impact instead, which are far cheaper on the wire`,
	"read":                 `returns the def body + a compact "Related" footer with summary, top-3 callers, top-3 callees, and semantically-adjacent defs — one call gives you what would otherwise take 3-4 sequential impact/outline calls; auto-downgrades to outline when body > 1500 bytes, pass full:true or mode:"body" to force; for a large body, pass line_range:"700-820" (1-indexed, file-relative, "-" or ":" both accepted) to get back just that span instead — also bypasses the auto-downgrade, and composes with query: to filter further within the returned range`,
	"read-and-verify":      `read a def AND run its covering tests in one call — use during bug triage so you see behavior alongside source and don't spiral into read-loops; pass name`,
	"read-file":            `all defs' bodies in one file — pass file:"path"; whole-file counterpart to read; prefer over N sequential read calls when scanning; for a large file, pass line_range:"700-820" the same as read to keep only the definitions overlapping that file-relative range, each narrowed to its own overlapping span`,
	"rename":               `pass old_name + new_name — rename a definition and update every reference across the repo in one call`,
	"rename-param":         `rename value param or receiver via ast.Object scoping — ≡_gofmt equivalence; pass name+old_param+new_param`,
	"replace-hunk":         `replace a byte-exact occurrence of 'old' inside a def body with 'new' — byte-exact PUTGET, content-addressed inside the def; pass name+old+new, plus index=1..N if 'old' occurs more than once, or replace_all:true to replace every occurrence with the same 'new' in one call — prefer replace_all over N indexed calls in one apply batch when every occurrence gets the identical replacement, since indices shift as earlier matches are consumed within the same batch; empty 'new' deletes the hunk(s). Send zero anchor context when the hunk is def-unique — the name argument does the file-level disambiguation`,
	"replace-slice":        `replace the Nth AST-role slice with verbatim bytes — byte-exact PUTGET; pass name+slice+index+new; refuses if replacement would discard interior comments — pass force:true to override`,
	"resummarize":          `no params — walks the DB for defs missing a one-line AI summary and enqueues each for backfill; requires ANTHROPIC_API_KEY to produce real summaries, otherwise a safe no-op`,
	"retarget-field-value": `rewrite a composite-literal field's string value across every def whose body matches — pass name:"<StructType>" field:"<Field>" old:"<oldStr>" new:"<newStr>"; AST-safe, so unrelated occurrences of the string won't match`,
	"similar":              `pass name — finds definitions semantically similar to this one (e.g. near-duplicate implementations worth deduping)`,
	"simulate":             `pass mutations:[{type,name,receiver}] — same shape as validate-plan; dry-runs a batch of hypothetical mutations against the live graph and returns a JSON impact preview without writing anything`,
	"slice":                `verbatim AST-role slice of a def — pass slice:"signature"|"doc"|"body"|"error-branch"|"return"|"loop" to get just that piece`,
	"sync":                 `rarely needed — every edit op auto-syncs the DB; only use after external file changes outside the code tool`,
	"test":                 `run ONLY tests that cover a given def — pass name; scoped subset, not the full suite; prefer over bash 'go test ./...' when you only need coverage for a specific change. Also accepts test:"TestX" to run one test by name — use this to REPRODUCE a bug from the issue BEFORE writing any code; a passing test means your hypothesis about which def is broken is wrong. An identical test call repeated with no write in between this session returns the cached result instead of rerunning the real subprocess — pass force:true to force a genuine rerun anyway`,
	"test-coverage":        `pass name — which tests currently cover a def, transitively (via impact's test graph); lighter-weight than impact when you only need test coverage, not the full caller graph`,
	"traverse":             `recursive graph traversal`,
	"untested":             `no params — lists every definition project-wide with zero test coverage`,
	"validate-plan":        `pass mutations:[{type,name,receiver}] — dry-run check whether a batch of hypothetical mutations would be valid (names resolve, no conflicts) before executing them for real`,
	"version":              `no params — running build's version string + on-disk binary path/mtime; call this after a rebuild+reconnect to confirm you're actually talking to fresh code, not a stale already-running serve process`,
	"wrap-in-defer":        `insert defer stmt before Nth top-level statement — byte-exact PUTGET; pass name+stmt_index+defer_body`,
}

var opHelpOrder = []string{
	"add-import",
	"apply",
	"batch-impact",
	"context",
	"create",
	"delete",
	"edit",
	"expand",
	"explain",
	"file-defs",
	"find",
	"help",
	"impact",
	"insert",
	"insert-header",
	"insert-precondition",
	"literals",
	"methods",
	"move",
	"outline",
	"overview",
	"patch",
	"plan",
	"plan-dsl",
	"plan-sexpr",
	"pragmas",
	"query",
	"read",
	"read-and-verify",
	"read-file",
	"rename",
	"rename-param",
	"replace-hunk",
	"replace-slice",
	"resummarize",
	"retarget-field-value",
	"similar",
	"simulate",
	"slice",
	"sync",
	"test",
	"test-coverage",
	"traverse",
	"untested",
	"validate-plan",
	"version",
	"wrap-in-defer",
}
