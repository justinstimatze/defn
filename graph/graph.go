// Package graph provides an in-memory reference graph for defn databases.
// All queries are O(1) map lookups after initial load.
//
//	g, err := graph.Load("/path/to/project/.defn")
//	callerFiles := g.CallerFiles("create.go", moduleID)
//	calleeFiles := g.CalleeFiles("create.go", moduleID)
package graph

import (
	"fmt"
	"strings"
	"sync"
)

// Def represents a single definition in the graph.
type Def struct {
	ID         int64
	Name       string
	Kind       string
	Receiver   string
	Signature  string
	SourceFile string
	ModuleID   int64
	Test       bool
	Exported   bool
	Hash       string
	StartLine  int
	EndLine    int
}

// FullName returns Receiver.Name or just Name.
func (d *Def) FullName() string {
	if d.Receiver != "" {
		return "(" + d.Receiver + ")." + d.Name
	}
	return d.Name
}

// Ref represents a reference edge.
type Ref struct {
	FromDef int64
	ToDef   int64
	Kind    string
}

// Graph is an in-memory reference graph loaded from a defn database.
// All query methods are O(1) map lookups.
type Graph struct {
	defs      []*Def
	refs      []Ref
	byID      map[int64]*Def
	byName    map[string][]*Def
	byFile    map[fileKey][]*Def
	byModule  map[int64][]*Def
	callers       map[int64][]int64
	callees       map[int64][]int64
	constructorOf map[int64][]int64 // type def ID → constructor site IDs
	byHash    map[string][]*Def
	modByPath map[string][]int64 // path → IDs (multiple in merged graphs)
	modByID   map[int64]string
}

// Duplicates returns definitions that share the same body hash — identical
// implementations across packages or repos. Only includes hashes with 2+ matches.
func (g *Graph) Duplicates() map[string][]*Def {
	result := map[string][]*Def{}
	for hash, defs := range g.byHash {
		if hash != "" && len(defs) > 1 {
			result[hash] = defs
		}
	}
	return result
}

// ByHash returns all definitions with a given body hash.
func (g *Graph) ByHash(hash string) []*Def {
	return g.byHash[hash]
}

// DefCount returns the number of definitions.
func (g *Graph) DefCount() int { return len(g.defs) }

// RefCount returns the number of references.
func (g *Graph) RefCount() int { return len(g.refs) }

// ModuleCount returns the number of modules.
func (g *Graph) ModuleCount() int { return len(g.modByPath) }

// DefByID returns a definition by ID, or nil.
func (g *Graph) DefByID(id int64) *Def { return g.byID[id] }

// AllDefs returns all definitions.
func (g *Graph) AllDefs() []*Def { return g.defs }

// ModulePath returns the path for a module ID.
func (g *Graph) ModulePath(id int64) string { return g.modByID[id] }

// ModuleID returns the first module ID for a path (for single-repo graphs).
func (g *Graph) ModuleID(path string) int64 {
	if ids := g.modByPath[path]; len(ids) > 0 {
		return ids[0]
	}
	return 0
}

type fileKey struct {
	File     string
	ModuleID int64
}

// CallerDefs returns the direct callers of a definition as Def objects.
func (g *Graph) CallerDefs(defID int64) []*Def {
	var result []*Def
	for _, id := range g.callers[defID] {
		if d, ok := g.byID[id]; ok {
			result = append(result, d)
		}
	}
	return result
}

// CallerIDs returns the direct caller definition IDs.
func (g *Graph) CallerIDs(defID int64) []int64 {
	return g.callers[defID]
}

// CalleeDefs returns the direct callees of a definition as Def objects.
func (g *Graph) CalleeDefs(defID int64) []*Def {
	var result []*Def
	for _, id := range g.callees[defID] {
		if d, ok := g.byID[id]; ok {
			result = append(result, d)
		}
	}
	return result
}

// CalleeIDs returns the direct callee definition IDs.
func (g *Graph) CalleeIDs(defID int64) []int64 {
	return g.callees[defID]
}

// Constructors returns definitions that construct instances of the given type
// (struct literals, new() calls). Only available when the resolver captures
// "constructor" reference kinds.
func (g *Graph) Constructors(defID int64) []*Def {
	var result []*Def
	for _, id := range g.constructorOf[defID] {
		if d, ok := g.byID[id]; ok {
			result = append(result, d)
		}
	}
	return result
}

// DefsInFile returns all definitions in a source file. If moduleID is 0,
// searches across all modules.
func (g *Graph) DefsInFile(sourceFile string, moduleID int64) []*Def {
	if moduleID != 0 {
		return g.byFile[fileKey{sourceFile, moduleID}]
	}
	// Unscoped: search all modules for this file.
	var result []*Def
	for key, defs := range g.byFile {
		if key.File == sourceFile {
			result = append(result, defs...)
		}
	}
	return result
}

// CallerFiles returns a map of source_file → count of callers for all
// definitions in the given file and module. If moduleID is 0, searches
// across all modules.
func (g *Graph) CallerFiles(sourceFile string, moduleID int64) map[string]int {
	result := map[string]int{}
	for _, d := range g.DefsInFile(sourceFile, moduleID) {
		for _, callerID := range g.callers[d.ID] {
			if caller, ok := g.byID[callerID]; ok && caller.SourceFile != "" {
				result[caller.SourceFile]++
			}
		}
	}
	return result
}

// CalleeFiles returns a map of source_file → count of callees for all
// definitions in the given file and module.
func (g *Graph) CalleeFiles(sourceFile string, moduleID int64) map[string]int {
	result := map[string]int{}
	for _, d := range g.DefsInFile(sourceFile, moduleID) {
		for _, calleeID := range g.callees[d.ID] {
			if callee, ok := g.byID[calleeID]; ok && callee.SourceFile != "" {
				result[callee.SourceFile]++
			}
		}
	}
	return result
}

// FileRef represents a file and the specific definitions involved in a relationship.
type FileRef struct {
	SourceFile string
	Defs       []*Def
}

// FileDependencies is a structured view of a file's cross-file relationships.
type FileDependencies struct {
	File     string
	Calls    []FileRef // files this file's definitions call into
	CalledBy []FileRef // files whose definitions call into this file
}

// FileDeps returns structured cross-file dependencies for a source file,
// including which specific definitions are involved in each relationship.
func (g *Graph) FileDeps(sourceFile string, moduleID int64) *FileDependencies {
	defs := g.DefsInFile(sourceFile, moduleID)

	// Callees grouped by file.
	calleeByFile := map[string][]*Def{}
	for _, d := range defs {
		for _, id := range g.callees[d.ID] {
			if callee, ok := g.byID[id]; ok && callee.SourceFile != "" && callee.SourceFile != sourceFile {
				calleeByFile[callee.SourceFile] = append(calleeByFile[callee.SourceFile], callee)
			}
		}
	}

	// Callers grouped by file.
	callerByFile := map[string][]*Def{}
	for _, d := range defs {
		for _, id := range g.callers[d.ID] {
			if caller, ok := g.byID[id]; ok && caller.SourceFile != "" && caller.SourceFile != sourceFile {
				callerByFile[caller.SourceFile] = append(callerByFile[caller.SourceFile], caller)
			}
		}
	}

	// Deduplicate defs per file.
	dedup := func(m map[string][]*Def) []FileRef {
		var refs []FileRef
		for file, defs := range m {
			seen := map[int64]bool{}
			var unique []*Def
			for _, d := range defs {
				if !seen[d.ID] {
					seen[d.ID] = true
					unique = append(unique, d)
				}
			}
			refs = append(refs, FileRef{SourceFile: file, Defs: unique})
		}
		return refs
	}

	return &FileDependencies{
		File:     sourceFile,
		Calls:    dedup(calleeByFile),
		CalledBy: dedup(callerByFile),
	}
}

// SiblingFiles returns other files in the same module.
func (g *Graph) SiblingFiles(sourceFile string, moduleID int64) []string {
	seen := map[string]bool{sourceFile: true}
	var result []string
	for _, d := range g.byModule[moduleID] {
		if d.SourceFile != "" && !seen[d.SourceFile] {
			seen[d.SourceFile] = true
			result = append(result, d.SourceFile)
		}
	}
	return result
}

// ExportedNames returns exported definition names in a file. If moduleID is 0,
// searches across all modules.
func (g *Graph) ExportedNames(sourceFile string, moduleID int64) []string {
	var result []string
	for _, d := range g.DefsInFile(sourceFile, moduleID) {
		if d.Exported && !d.Test {
			result = append(result, d.FullName())
		}
	}
	return result
}

// GetDef finds a definition by name, with optional module path hint.
// Disambiguates by caller count (most callers wins).
func (g *Graph) GetDef(name, modulePath string) *Def {
	// Try receiver.method syntax.
	if strings.Contains(name, ".") && !strings.Contains(name, "/") {
		dotIdx := strings.LastIndex(name, ".")
		recv := strings.TrimPrefix(strings.Trim(name[:dotIdx], "()"), "*")
		methName := name[dotIdx+1:]
		for _, d := range g.byName[methName] {
			bareRecv := strings.TrimPrefix(d.Receiver, "*")
			if bareRecv == recv || strings.HasSuffix(bareRecv, recv) {
				if modulePath == "" || g.modByID[d.ModuleID] == modulePath {
					return d
				}
			}
		}
	}

	candidates := g.byName[name]
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	// Filter by module if provided.
	if modulePath != "" {
		modIDs := g.modByPath[modulePath]
		if len(modIDs) > 0 {
			for _, d := range candidates {
				for _, mid := range modIDs {
					if d.ModuleID == mid {
						return d
					}
				}
			}
		}
	}

	// Disambiguate by caller count.
	best := candidates[0]
	bestCount := len(g.callers[best.ID])
	for _, d := range candidates[1:] {
		if c := len(g.callers[d.ID]); c > bestCount {
			best = d
			bestCount = c
		}
	}
	return best
}

// ResolveModuleID finds the module ID for a file path relative to the project root.
// Returns (id, true) if found, (0, false) if not.
func (g *Graph) ResolveModuleID(projectRoot, relPath string) (int64, bool) {
	// Strip filename to get package directory.
	dir := relPath
	if idx := strings.LastIndex(dir, "/"); idx >= 0 {
		dir = dir[:idx]
	}
	for path, ids := range g.modByPath {
		if strings.HasSuffix(path, dir) && len(ids) > 0 {
			return ids[0], true
		}
	}
	return 0, false
}

// TransitiveCallers returns all transitive callers of a definition via BFS.
func (g *Graph) TransitiveCallers(defID int64) []*Def {
	visited := map[int64]bool{defID: true}
	queue := []int64{defID}
	var result []*Def

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, callerID := range g.callers[id] {
			if !visited[callerID] {
				visited[callerID] = true
				queue = append(queue, callerID)
				if d, ok := g.byID[callerID]; ok {
					result = append(result, d)
				}
			}
		}
	}
	return result
}

// Tests returns all test definitions that transitively call the given definition.
func (g *Graph) Tests(defID int64) []*Def {
	var tests []*Def
	for _, d := range g.TransitiveCallers(defID) {
		if d.Test && (strings.HasPrefix(d.Name, "Test") || strings.HasPrefix(d.Name, "Benchmark")) {
			tests = append(tests, d)
		}
	}
	return tests
}

// NewGraph creates a graph from raw definitions, references, and modules.
// Used by tests and custom loaders.
func NewGraph(defs []*Def, refs []Ref, modByPath map[string]int64, modByID map[int64]string) *Graph {
	// Convert single-value modByPath to multi-value for internal storage.
	multiPath := make(map[string][]int64, len(modByPath))
	for path, id := range modByPath {
		multiPath[path] = []int64{id}
	}
	// Defensive copies — callers may reuse slices/maps after construction.
	defsCopy := make([]*Def, len(defs))
	copy(defsCopy, defs)
	refsCopy := make([]Ref, len(refs))
	copy(refsCopy, refs)
	modByIDCopy := make(map[int64]string, len(modByID))
	for k, v := range modByID {
		modByIDCopy[k] = v
	}
	g := &Graph{
		defs:      defsCopy,
		refs:      refsCopy,
		modByPath: multiPath,
		modByID:   modByIDCopy,
	}
	g.build()
	return g
}

// build populates the index maps from raw Defs and Refs.
func (g *Graph) build() {
	g.byID = make(map[int64]*Def, len(g.defs))
	g.byName = make(map[string][]*Def)
	g.byFile = make(map[fileKey][]*Def)
	g.byModule = make(map[int64][]*Def)
	g.callers = make(map[int64][]int64)
	g.callees = make(map[int64][]int64)
	g.byHash = make(map[string][]*Def)

	for _, d := range g.defs {
		g.byID[d.ID] = d
		g.byName[d.Name] = append(g.byName[d.Name], d)
		if d.SourceFile != "" {
			key := fileKey{d.SourceFile, d.ModuleID}
			g.byFile[key] = append(g.byFile[key], d)
		}
		g.byModule[d.ModuleID] = append(g.byModule[d.ModuleID], d)
		if d.Hash != "" {
			g.byHash[d.Hash] = append(g.byHash[d.Hash], d)
		}
	}

	g.constructorOf = make(map[int64][]int64)
	for _, r := range g.refs {
		g.callers[r.ToDef] = append(g.callers[r.ToDef], r.FromDef)
		g.callees[r.FromDef] = append(g.callees[r.FromDef], r.ToDef)
		if r.Kind == "constructor" {
			g.constructorOf[r.ToDef] = append(g.constructorOf[r.ToDef], r.FromDef)
		}
	}
}

// --- Process-lifetime cache ---

type loadResult struct {
	wg  sync.WaitGroup
	g   *Graph
	err error
}

var (
	cacheMu  sync.Mutex
	cache    = map[string]*Graph{}
	inflight = map[string]*loadResult{}
)

// ClearCache clears all cached graphs. For testing.
func ClearCache() {
	cacheMu.Lock()
	cache = map[string]*Graph{}
	inflight = map[string]*loadResult{}
	cacheMu.Unlock()
}

// InvalidateCache removes a single cached graph, forcing reload on next access.
func InvalidateCache(path string) {
	cacheMu.Lock()
	delete(cache, path)
	cacheMu.Unlock()
}

// loadOnce ensures only one goroutine loads a given path at a time.
// Other goroutines wait for the first loader to finish.
func loadOnce(path string, loader func() (*Graph, error)) (*Graph, error) {
	cacheMu.Lock()
	if g, ok := cache[path]; ok {
		cacheMu.Unlock()
		return g, nil
	}
	if lr, ok := inflight[path]; ok {
		cacheMu.Unlock()
		lr.wg.Wait()
		return lr.g, lr.err
	}
	lr := &loadResult{}
	lr.wg.Add(1)
	inflight[path] = lr
	cacheMu.Unlock()

	lr.g, lr.err = loader()

	cacheMu.Lock()
	if lr.err == nil {
		cache[path] = lr.g
	}
	delete(inflight, path)
	cacheMu.Unlock()
	lr.wg.Done()

	return lr.g, lr.err
}

// String returns a summary of the graph.
func (g *Graph) String() string {
	if g == nil {
		return "Graph{nil}"
	}
	return fmt.Sprintf("Graph{defs=%d, refs=%d, modules=%d}",
		len(g.defs), len(g.refs), len(g.modByPath))
}
