// Backend is the storage-agnostic surface that internal/mcp, internal/emit,
// internal/resolve, internal/ingest, and cmd/defn call to read and mutate
// the defn code graph. SQLite (*SQLiteDB) is the only implementation as
// of Phase 4; a future backend (in-memory, gopls-hybrid) would slot in
// here without touching callers.
package store

import "context"

type Backend interface {
	// Lifecycle
	Close() error
	Path() string
	Ping(ctx context.Context) error
	Ctx() context.Context
	// Begin returns a transaction-scoped Backend view (tx) alongside the
	// usual commit/rollback funcs. Callers doing a multi-write batch MUST
	// route every write through tx, not the original Backend -- writes
	// issued against the original still auto-commit immediately via the
	// pool, bypassing the transaction entirely (this was #214: Begin's
	// commit/rollback previously had nothing for callers to route writes
	// through, so every write auto-committed regardless of the batch's
	// outcome). tx is only valid until commit()/rollback() is called.
	Begin() (tx Backend, commit func() error, rollback func(), err error)
	CleanTempFiles()
	GC() error                        // no-op under SQLite (WAL checkpoint replaces this)
	ComputeRootHash() (string, error) // canonical dump hash under SQLite

	// Modules
	EnsureModule(path, name, doc string) (*Module, error)
	GetModuleByPath(path string) (*Module, error)
	ListModules() ([]Module, error)
	GetModuleDefinitions(moduleID int64) ([]Definition, error)

	// Definitions — reads
	GetDefinition(id int64) (*Definition, error)
	GetDefinitionByName(name, modulePath string) (*Definition, error)
	// CountDefinitionsByName returns how many definitions share name,
	// regardless of module/package -- used to detect when a bare-name
	// lookup's best-effort tiebreak (GetDefinitionByName's blast-radius
	// ORDER BY) silently picked one of several candidates.
	CountDefinitionsByName(name string) (int, error)
	GetDefinitionByNameAndReceiver(name, modulePath, receiver string) (*Definition, error)
	FilterDefinitions(name, kind, file string, limit int) ([]Definition, error)
	FindDefinitions(namePattern string) ([]Definition, error)
	FindDefinitionsByFile(fileSuffix string, sourceFile string, line int) ([]Definition, error)
	CountDefinitions() (int, error)
	SearchDefinitions(query string) ([]Definition, error)
	SearchBodiesLike(pattern string, limit int) ([]BodyMatch, error)
	SampleBodies(n int) ([]string, error)
	GetBodiesByDefIDs(ids []int64) (map[int64]string, error)
	GetUntested() ([]Definition, error)

	// Definitions — writes
	UpsertDefinition(d *Definition) (int64, error)
	UpsertDefinitionsBulk(defs []*Definition) ([]int64, error)
	DeleteDefinition(id int64) error
	RenameDefinition(id int64, newName, newBody, newSignature string, exported bool) error
	// UpdateDefinitionReceiver rewrites a method's receiver clause (plus
	// the body/signature an AST rename of the old receiver identifier
	// necessarily also touches) by ID. UpsertDefinition can't be reused
	// for this: receiver is part of the natural key (module_id, name,
	// kind, receiver, test, source_file) -- writing a Definition whose
	// Receiver field already changed just INSERTS a second row under the
	// new key instead of updating the existing one in place, silently
	// orphaning the original (confirmed live: a type rename that also
	// tried to repoint its methods' receivers via UpsertDefinition left
	// both the stale-receiver and new-receiver rows in the DB at once).
	UpdateDefinitionReceiver(id int64, newReceiver, newBody, newSignature string) error
	PruneStaleDefinitions(liveIDs map[int64]bool) (int, error)

	// References / call graph
	QueryRefs(fromName, toName, kind string, limit int) ([]Reference, error)
	SetReferences(fromDef int64, refs []Reference) error
	SetManyReferences(refsByDef map[int64][]Reference) error
	GetCallers(defID int64) ([]Definition, error)
	GetCallees(defID int64) ([]Definition, error)
	GetImpact(defID int64) (*Impact, error)
	RefCountsByTarget(targetIDs []int64) (map[int64]int, map[int64]int, error)
	Traverse(startID int64, direction string, refKinds []string, maxDepth int) ([]TraverseResult, error)

	// Imports (per-module)
	GetImports(moduleID int64) ([]Import, error)
	SetImports(moduleID int64, imports []Import) error

	// Literal fields (composite-literal extraction)
	// skipOrderBy/skipDefName are opt-OUT performance flags for bulk
	// callers that discard ordering and/or the joined definition name --
	// zero-value (false) preserves the original behavior for every
	// existing caller. skipOrderBy drops the `ORDER BY type_name,
	// field_name` (~103ms on a 90k-row bulk query); skipDefName skips the
	// LEFT JOIN definitions used only to populate DefName (~210ms).
	QueryLiteralFields(typeName, fieldName, fieldValue string, fieldNames []string, defIDs []int64, limit int, skipOrderBy, skipDefName bool) ([]LiteralField, error)
	SetLiteralFields(defID int64, fields []LiteralField) error
	SetManyLiteralFields(fieldsByDef map[int64][]LiteralField) error

	// Comments / pragmas
	GetCommentsByPragma(pragmaKey string) ([]Comment, error)
	GetCommentsForDef(defID int64) ([]Comment, error)
	SetFileComments(sourceFile string, comments []Comment) error

	// File sources (raw per-file, for lossless emit)
	SetFileSource(moduleID int64, sourceFile, raw string) error
	GetFileSource(moduleID int64, sourceFile string) (string, error)
	ListFileSources(moduleID int64) (map[string]string, error)
	// ListFileSourceNames is the metadata-only sibling of ListFileSources --
	// same scope, but filenames only, no raw file content. Use this when a
	// caller only needs to know WHICH files exist (e.g. checking for a
	// sibling _test.go), not their content.
	ListFileSourceNames(moduleID int64) ([]string, error)
	DistinctSourceFiles() ([]string, error)
	PruneStaleFileSources(live map[int64]map[string]bool) (int, error)
	DeleteFile(sourceFile string) error

	// Project files (go.mod / go.sum / embedded files)
	GetProjectFile(path string) (string, error)
	SetProjectFile(path, content string) error
	ListProjectFiles() ([]string, error)

	// Meta / arbitrary key-value
	GetMeta(key string) (string, error)
	SetMeta(key, value string) error

	// Upstream fingerprints (well-known-lib delta-from-prior)
	InsertUpstreamFingerprint(u UpstreamFingerprint) error
	InsertUpstreamFingerprints(rows []UpstreamFingerprint) error
	FindUpstreamMatch(modulePath, defName, kind, receiver, fingerprint string) (*UpstreamFingerprint, error)
	FindUpstreamVersions(modulePath, defName, kind, receiver string) ([]UpstreamFingerprint, error)
	CountUpstreamFingerprints() (int, error)

	// Ad-hoc SQL (op:query surface)
	Query(query string) ([]map[string]any, error)

	// Simulation (op:simulate speculative apply)
	Simulate(mutations []Mutation) (*SimulationResult, error)

	// Def summaries — precomputed MinHash signatures for approximate
	// similarity. Task #151. Set is idempotent; All returns everything
	// present as (def_id → minhash) map for the O(N) similarity scan
	// in handleSimilar.
	SetDefSummaryMinHash(defID int64, minhash []byte) error
	AllDefSummaryMinHashes() (map[int64][]byte, error)

	// #160 semantic summaries. GetDefSummary returns (nil, nil) when no
	// summary exists yet — callers must distinguish "no row" from "row
	// with empty OneLine" (latter shouldn't happen but is legal per the
	// schema NULL). SetDefSummary is idempotent — INSERT OR REPLACE keys
	// off def_id. Both are safe to call concurrently with UpsertDefinition
	// under SQLite's single-writer model.
	GetDefSummary(defID int64) (*DefSummary, error)
	SetDefSummary(defID int64, s *DefSummary) error
	// ListDefsMissingSummary returns def IDs with no one_line yet.
	// Sorted ascending for deterministic paging. #160 stage 3a backfill.
	ListDefsMissingSummary() ([]int64, error)

	// SearchDefSummaries finds def IDs whose one_line contains pattern
	// (LIKE %pattern%, case-insensitive). Returns def IDs ordered by
	// caller-count descending as a rough relevance signal — the caller
	// (context op #197) reranks with its own scoring. #197 bridges the
	// lexical-only ranker to the semantic summaries #160 already builds.
	SearchDefSummaries(pattern string) ([]int64, error)

	// #212 file-level narratives — one level up from #160's per-def
	// summaries. GetFileSummary returns (nil, nil) when no narrative
	// exists yet. SetFileSummary is idempotent — INSERT OR REPLACE
	// keys off source_file.
	GetFileSummary(sourceFile string) (*FileSummary, error)
	SetFileSummary(sourceFile string, moduleID int64, s *FileSummary) error

	// #192 explain-QA cache. GetExplainCache returns (nil, nil) when no
	// entry exists yet for cacheKey. SetExplainCache is idempotent —
	// INSERT OR REPLACE keys off cacheKey (content-addressed: question +
	// scoped defs' body hashes, computed by the caller).
	GetExplainCache(cacheKey string) (*ExplainCacheEntry, error)
	SetExplainCache(cacheKey, question, scope, answer, model string, refs []string) error
}
