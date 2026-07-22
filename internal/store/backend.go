// Backend is the storage-agnostic surface that internal/mcp, internal/emit,
// internal/resolve, internal/ingest, and cmd/defn call to read and mutate
// the defn code graph. It DOES NOT include Dolt-specific version-control
// operations (branch, checkout, commit, merge, diff, log, remotes,
// conflicts) — those live only on the concrete *DB type and will be
// removed when the Dolt backend is retired.
//
// Any new backend (SQLite, in-memory, gopls-hybrid) must implement this
// interface. Existing callers should be migrated to depend on Backend
// rather than *DB so they compile against any backend.
package store

import "context"

// Backend is the migration-target surface. In Phase 0 it lives alongside
// *DB, which implements it via var _ Backend = (*DB)(nil) below. Category A
// methods (branch, checkout, commit, merge, diff, log, remotes) are
// deliberately excluded — they're implementation-specific to Dolt.
type Backend interface {
	// Lifecycle
	Close() error
	Path() string
	Ping(ctx context.Context) error
	Ctx() context.Context
	Begin() (commit func() error, rollback func(), err error)
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
	QueryLiteralFields(typeName, fieldName, fieldValue string, fieldNames []string, limit int) ([]LiteralField, error)
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
}

// Compile-time assertion: the concrete *DB must satisfy Backend. If a
// method is added to *DB and belongs in the interface, add it here; if a
// method is Dolt-specific version-control (branch, merge, commit, diff,
// log, remotes, conflicts), leave it OFF the interface.
var _ Backend = (*DB)(nil)
