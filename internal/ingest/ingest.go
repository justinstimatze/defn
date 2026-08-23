// Package ingest loads Go source code from disk, parses it with go/ast,
// extracts definitions, and stores them in the defn database.
package ingest

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/justinstimatze/defn/internal/astutil"
	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/store"
	"golang.org/x/tools/go/packages"
)

// Ingest loads a Go module from modulePath and stores all definitions
// into the database. modulePath should be a directory containing go.mod.
func Ingest(db store.Backend, modulePath string) error {
	pkgs, err := goload.LoadAll(modulePath)
	if err != nil {
		return err
	}
	return IngestPackages(db, pkgs, modulePath)
}

// IngestPackages is like Ingest but accepts pre-loaded packages.
// Use with goload.LoadAll to share one packages.Load between ingest
// and resolve, saving ~1-2 GB of memory.
func IngestPackages(db store.Backend, pkgs []*packages.Package, modulePath string) error {
	clearSourceFileCache()

	// Check for load errors.
	var errs []string
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, e := range pkg.Errors {
			errs = append(errs, e.Error())
		}
	})
	if len(errs) > 0 {
		return fmt.Errorf("package errors:\n%s", strings.Join(errs, "\n"))
	}

	state := &ingestState{
		initCounter:     make(map[string]int),
		liveDefIDs:      make(map[int64]bool),
		liveFileSources: make(map[int64]map[string]bool),
	}

	// Store project-level files (go.mod, go.sum).
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join(modulePath, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := db.SetProjectFile(name, string(data)); err != nil {
			return fmt.Errorf("store %s: %w", name, err)
		}
	}

	// Each UpsertDefinition allocates row/body data that stays live in
	// the process heap until a GC (db.GC() below runs a WAL checkpoint
	// on the SQLite backend). Without intervention, peak retention
	// scales roughly linearly with (rows × avg body size) on a large
	// module.
	//
	// Strategy: rely on the end-of-IngestPackages GC below for the
	// common case (small/medium projects). Only trigger mid-loop GC
	// when the heap is truly large (>1 GB) -- these Dolt-era thresholds
	// (1 GB trigger, ~10-20s per checkpoint) haven't been re-measured
	// against the SQLite backend's WAL checkpoint cost, which is
	// typically far cheaper than Dolt's chunk-store GC was.
	const midLoopGCThresholdBytes = 1 << 30 // 1 GB
	filtered := goload.FilterPackages(pkgs)
	var m runtime.MemStats

	// #125 winze methodology: split the walk timer from the flush timer so
	// batched-upsert wins are unambiguous vs Go build-cache noise. Guarded
	// by DEFN_SYNC_TIMING to match resolve's convention.
	timing := os.Getenv("DEFN_SYNC_TIMING") == "1"
	var tWalk, tFlush time.Duration
	tPhaseStart := time.Now()

	for i, pkg := range filtered {
		// #223: ingestPackage now flushes this package's buffered defs
		// (#125 batched INSERT) internally, before extracting comments --
		// comments need to look up defs by FindDefinitionsByFile, which
		// can't see them until flushed. tFlush is no longer split out
		// separately; it's included in tWalk below.
		t0 := time.Now()
		if err := ingestPackage(db, pkg, modulePath, state); err != nil {
			return fmt.Errorf("ingest %s: %w", pkg.PkgPath, err)
		}
		tWalk += time.Since(t0)
		if i+1 >= len(filtered) {
			continue
		}
		runtime.ReadMemStats(&m)
		if m.HeapAlloc < midLoopGCThresholdBytes {
			continue
		}
		if err := db.GC(); err != nil {
			return fmt.Errorf("checkpoint gc: %w", err)
		}
	}
	if timing {
		fmt.Fprintf(os.Stderr, "    [inner] walk+enqueue: %s\n", tWalk.Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "    [inner] upsert defs+bodies (bulk): %s\n", tFlush.Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "    [inner] IngestPackages total: %s\n", time.Since(tPhaseStart).Round(time.Millisecond))
	}

	if err := db.GC(); err != nil {
		return fmt.Errorf("post-ingest gc: %w", err)
	}
	// Return mmap'd heap reservation to the OS. Go's runtime keeps freed
	// heap as mmap by default — fine for CLI processes that exit, but in
	// long-lived `defn serve` it means VmRSS only grows. Each ingest
	// cycle gets back to baseline RSS this way. Cost: ~ms.
	debug.FreeOSMemory()

	// Remove definitions that no longer exist in the source code.
	if pruned, err := db.PruneStaleDefinitions(state.liveDefIDs); err != nil {
		return fmt.Errorf("prune stale: %w", err)
	} else if pruned > 0 {
		fmt.Fprintf(os.Stderr, "pruned %d stale definitions\n", pruned)
	}

	// Remove file_sources rows for files no longer touched by ingest —
	// catches both genuine on-disk deletions and orphan basename rows
	// left by pre-0.22.3 relative-modulePath ingests, whose presence
	// otherwise breaks the incremental fast path's structural-change
	// guard. Scoped per-module: pruning runs for each module that was
	// (re)ingested in this pass; modules absent from this ingest (an
	// unusual situation) keep their rows.
	if pruned, err := db.PruneStaleFileSources(state.liveFileSources); err != nil {
		return fmt.Errorf("prune stale file_sources: %w", err)
	} else if pruned > 0 {
		fmt.Fprintf(os.Stderr, "pruned %d stale file_sources rows\n", pruned)
	}

	// Record last ingest timestamp for staleness detection.
	if err := db.SetMeta("last_ingest", strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		return fmt.Errorf("set last_ingest: %w", err)
	}

	return nil
}

func ingestPackage(db store.Backend, pkg *packages.Package, modulePath string, state *ingestState) error {
	// Strip _test suffix from external test package paths so test definitions
	// are stored in the same module as the code they test.
	pkgPath := pkg.PkgPath
	pkgName := pkg.Name
	if before, ok := strings.CutSuffix(pkgName, "_test"); ok {
		pkgName = before
		if before, ok := strings.CutSuffix(pkgPath, "_test"); ok {
			pkgPath = before
		}
	}
	// Extract package doc comment from the first file that has one.
	pkgDoc := ""
	for _, file := range pkg.Syntax {
		if file.Doc != nil {
			pkgDoc = strings.TrimSpace(file.Doc.Text())
			break
		}
	}
	if len(pkg.Syntax) == 0 {
		// go/packages.Load still returns a *packages.Package for
		// directories whose Go files are all excluded by build
		// constraints for the host's GOOS/GOARCH or custom tags (the
		// common //go:build tools idiom, or platform-specific files).
		// Nothing here was ever actually parsed -- skip creating a
		// module row entirely. Without this guard, EnsureModule below
		// created a phantom zero-def module row pointing at a real
		// directory defn never touched, and emitModule's zero-defs
		// cleanup mistook it for a module defn used to manage, deleting
		// real files on an unscoped emit (task #239).
		return nil
	}
	mod, err := db.EnsureModule(pkgPath, pkgName, pkgDoc)
	if err != nil {
		return err
	}

	// Collect imports from all files in this package.
	seen := make(map[string]string) // path → alias
	for _, file := range pkg.Syntax {
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			alias := ""
			if imp.Name != nil && imp.Name.Name != "." {
				alias = imp.Name.Name
			}
			// Keep the first alias seen (they should be consistent within a package).
			if _, ok := seen[path]; !ok {
				seen[path] = alias
			}
		}
	}
	var imports []store.Import
	for path, alias := range seen {
		imports = append(imports, store.Import{
			ModuleID:     mod.ID,
			ImportedPath: path,
			Alias:        alias,
		})
	}
	if err := db.SetImports(mod.ID, imports); err != nil {
		return fmt.Errorf("set imports: %w", err)
	}

	// Find and store //go:embed referenced files.
	if err := ingestEmbedFiles(db, pkg, modulePath); err != nil {
		return err
	}

	type fileForComments struct {
		file       *ast.File
		sourceFile string
	}
	var commentFiles []fileForComments

	for _, file := range pkg.Syntax {
		// Get source filename from the token.FileSet.
		isTest := false
		sourceFile := ""
		absFile := ""
		if file.Pos().IsValid() {
			absFile = pkg.Fset.Position(file.Pos()).Filename
			isTest = strings.HasSuffix(absFile, "_test.go")
			// Make relative to module root.
			if rel, err := filepath.Rel(modulePath, absFile); err == nil {
				sourceFile = rel
			} else {
				sourceFile = filepath.Base(absFile)
			}
		}
		// Phase C: capture the raw on-disk source as the authoritative
		// representation for this file. Emit uses it verbatim (or as the
		// merge base when defs have been edited). Read failures are
		// non-fatal — the definitions/bodies path still works for queries.
		if absFile != "" && sourceFile != "" {
			if raw, err := os.ReadFile(absFile); err == nil {
				if err := db.SetFileSource(mod.ID, sourceFile, string(raw)); err != nil {
					return fmt.Errorf("set file source for %s: %w", sourceFile, err)
				}
				if state.liveFileSources[mod.ID] == nil {
					state.liveFileSources[mod.ID] = make(map[string]bool)
				}
				state.liveFileSources[mod.ID][sourceFile] = true
			}
		}
		if err := ingestFile(db, pkg, mod, file, isTest, sourceFile, state); err != nil {
			return err
		}
		if sourceFile != "" && len(file.Comments) > 0 {
			commentFiles = append(commentFiles, fileForComments{file, sourceFile})
		}
	}

	// #223: flush this package's defs before extracting comments.
	// ingestComments looks up defs via FindDefinitionsByFile to link
	// pragma/doc comments to their DefID -- with the #125 batched-upsert
	// buffer, those defs don't exist in the database until flushed, so
	// doing this before the flush always saw an empty or stale set and
	// every comment's DefID came back nil regardless of position.
	if err := state.flushDefs(db); err != nil {
		return err
	}
	for _, cf := range commentFiles {
		if err := ingestComments(db, pkg.Fset, cf.file, cf.sourceFile); err != nil {
			return fmt.Errorf("ingest comments: %w", err)
		}
	}
	return nil
}

// ingestEmbedFiles finds //go:embed referenced files in a package
// and stores them as project files with their relative paths.
func ingestEmbedFiles(db store.Backend, pkg *packages.Package, modulePath string) error {
	// Use EmbedPatterns if available (requires NeedEmbedPatterns).
	if len(pkg.EmbedPatterns) == 0 {
		return nil
	}

	// pkg.GoFiles contains absolute paths to the package's source directory.
	var pkgDir string
	if len(pkg.GoFiles) > 0 {
		pkgDir = filepath.Dir(pkg.GoFiles[0])
	} else {
		return nil
	}

	absModulePath, _ := filepath.Abs(modulePath)

	for _, pattern := range pkg.EmbedPatterns {
		// EmbedPatterns may be absolute paths or glob patterns.
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(pkgDir, pattern)
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, absPath := range matches {
			content, err := os.ReadFile(absPath)
			if err != nil {
				continue
			}
			// Skip binary files (not valid UTF-8) — can't store in TEXT columns.
			if !utf8.Valid(content) {
				continue
			}
			relPath, err := filepath.Rel(absModulePath, absPath)
			if err != nil {
				continue
			}
			if err := db.SetProjectFile(relPath, string(content)); err != nil {
				continue // best effort for embeds
			}
		}
	}
	return nil
}

func ingestFile(db store.Backend, pkg *packages.Package, mod *store.Module, file *ast.File, isTest bool, sourceFile string, state *ingestState) error {
	fset := pkg.Fset

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if err := ingestFunc(db, fset, mod, file, d, isTest, sourceFile, state); err != nil {
				return err
			}
		case *ast.GenDecl:
			if err := ingestGenDecl(db, fset, mod, file, d, isTest, sourceFile, state); err != nil {
				return err
			}
		}
	}

	// #223: comment/pragma extraction moved to ingestPackage, run AFTER
	// state.flushDefs -- ingestComments looks up this file's defs via
	// FindDefinitionsByFile, which can't see them yet while they're still
	// sitting in state.pendingDefs (the #125 batched-upsert buffer).
	// Doing it here, mid-decl-loop, meant every pragma comment's DefID
	// came back nil regardless of position.

	return nil
}

// pragmaRe matches comment pragmas like //go:generate, //lint:ignore, //winze:contested.
var pragmaRe = regexp.MustCompile(`^//\s*([a-zA-Z_]\w*:[a-zA-Z_]\w*)\s*(.*)$`)

// defInterval represents a definition's line range for comment association.
type defInterval struct {
	startLine int // extended to include doc comment if present
	endLine   int
	defID     int64
}

// ingestComments extracts all comments from a file, associates them with
// definitions by line range, and stores them in the database.
func ingestComments(db store.Backend, fset *token.FileSet, file *ast.File, sourceFile string) error {
	// Build intervals from AST declarations, extended to include doc comments.
	// We use the AST directly (not a DB query) so we get doc comment positions.
	var intervals []defInterval
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			start := fset.Position(d.Pos()).Line
			if d.Doc != nil {
				if docLine := fset.Position(d.Doc.Pos()).Line; docLine < start {
					start = docLine
				}
			}
			end := fset.Position(d.End()).Line
			intervals = append(intervals, defInterval{startLine: start, endLine: end})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				var start, end int
				var doc *ast.CommentGroup
				switch s := spec.(type) {
				case *ast.TypeSpec:
					start = fset.Position(s.Pos()).Line
					end = fset.Position(s.End()).Line
					doc = s.Doc
					if doc == nil {
						doc = d.Doc
					}
				case *ast.ValueSpec:
					if len(s.Names) == 0 || s.Names[0].Name == "_" {
						continue
					}
					start = fset.Position(s.Pos()).Line
					end = fset.Position(s.End()).Line
					doc = s.Doc
					if doc == nil {
						doc = d.Doc
					}
				default:
					continue
				}
				if doc != nil {
					if docLine := fset.Position(doc.Pos()).Line; docLine < start {
						start = docLine
					}
				}
				intervals = append(intervals, defInterval{startLine: start, endLine: end})
			}
		}
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].startLine < intervals[j].startLine })

	// Query the DB for definitions in this file to get their IDs and line ranges.
	// We match AST intervals to DB definitions by overlapping line ranges.
	defs, err := db.FindDefinitionsByFile("", sourceFile, 0)
	if err != nil {
		return fmt.Errorf("find defs for comments: %w", err)
	}
	// Build a map from startLine to defID for matching. Struct fields
	// (#11) are excluded: a one-line struct decl (e.g. "type X struct{
	// V int }") gives its field the same start_line as the struct
	// itself, and letting a field def compete for that line silently
	// clobbers the type's entry depending on map iteration order --
	// misattributing the struct's leading pragma/doc comment to its
	// field instead. Fields aren't independent comment-attachable
	// declarations anyway.
	defByLine := make(map[int]int64)
	for _, d := range defs {
		if d.Kind == "field" {
			continue
		}
		defByLine[int(d.StartLine)] = d.ID
	}
	// Assign defIDs to intervals by matching: the DB's startLine should be
	// within the AST interval (since AST interval extends to doc comment).
	for i := range intervals {
		for dbStart, defID := range defByLine {
			if dbStart >= intervals[i].startLine && dbStart <= intervals[i].endLine {
				intervals[i].defID = defID
				break
			}
		}
	}

	// Associate each comment with a definition by line containment.
	findDef := func(line int) *int64 {
		for i := range intervals {
			if line >= intervals[i].startLine && line <= intervals[i].endLine && intervals[i].defID > 0 {
				return &intervals[i].defID
			}
		}
		return nil
	}

	var comments []store.Comment
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			line := fset.Position(c.Pos()).Line
			text := c.Text
			defID := findDef(line)

			kind := "line"
			if strings.HasPrefix(text, "/*") {
				kind = "block"
			}

			var pragmaKey, pragmaVal string
			if m := pragmaRe.FindStringSubmatch(text); m != nil {
				kind = "pragma"
				pragmaKey = m[1]
				pragmaVal = strings.TrimSpace(m[2])
			}

			comments = append(comments, store.Comment{
				DefID:      defID,
				SourceFile: sourceFile,
				Line:       line,
				Text:       text,
				Kind:       kind,
				PragmaKey:  pragmaKey,
				PragmaVal:  pragmaVal,
			})
		}
	}

	return db.SetFileComments(sourceFile, comments)
}

// ingestState holds mutable state for a single ingest run.
// Passed by pointer to avoid package-level mutable state.
type ingestState struct {
	// initCounter tracks init() occurrences keyed by "moduleID:sourceFile",
	// NOT by module alone (#241). Multiple init() funcs are valid Go, and
	// each needs a unique defn-internal name so they don't overwrite each
	// other -- but a module-wide counter accumulates across every file in
	// the module in whatever order that specific ingest run happened to
	// process them, so the SAME physical init() in the SAME file gets a
	// DIFFERENT synthetic name depending on which other files were
	// ingested alongside it in that run. A full-module ingest and a
	// single-file `sync` (IngestFile, which always starts a fresh
	// ingestState per call) would then assign different names to the
	// identical function -- and since name is part of UpsertDefinition's
	// natural key, that mismatch creates a NEW row instead of updating
	// the existing one, leaving the old name's row orphaned. Real
	// trajectory (cli-513): mixing file-level and module-level `sync`
	// calls during one session accumulated SIX separate, byte-identical
	// copies of one physical init() into the emitted file. Scoping the
	// counter per (module, file) makes the Nth init() in a given file
	// always get the same name regardless of ingest mode or file order.
	initCounter map[string]int
	liveDefIDs  map[int64]bool // tracks all definition IDs seen
	// liveFileSources tracks the (module_id, source_file) pairs written
	// during this ingest. Used to prune file_sources rows for files
	// that no longer exist on disk (e.g., orphan basename entries
	// from pre-0.22.3 relative-modulePath ingests).
	liveFileSources map[int64]map[string]bool
	// pendingDefs buffers definitions for batched UpsertDefinitionsBulk.
	// Flushed at each package boundary — winze profile 2026-07-22 showed
	// per-row upsert = 105s of a 112s warm ingest; batched shape matches
	// SetManyReferences (#108/#111) and closes the gap ~30x.
	pendingDefs []*store.Definition
}

// enqueueDef buffers a definition for batched upsert. The def is written
// on the next call to flushDefs (typically at end of ingestPackage).
// state.liveDefIDs is populated after flush from the returned IDs.
func (s *ingestState) enqueueDef(d *store.Definition) {
	s.pendingDefs = append(s.pendingDefs, d)
}

// flushDefs writes all buffered defs via db.UpsertDefinitionsBulk, records
// the returned IDs in liveDefIDs, and clears the buffer. Safe on empty
// buffer. Called at package boundaries so any single ingest failure only
// wastes one package's work (matches per-row failure semantics roughly).
func (s *ingestState) flushDefs(db store.Backend) error {
	if len(s.pendingDefs) == 0 {
		return nil
	}
	ids, err := db.UpsertDefinitionsBulk(s.pendingDefs)
	if err != nil {
		return err
	}
	for _, id := range ids {
		s.liveDefIDs[id] = true
	}
	s.pendingDefs = s.pendingDefs[:0]
	return nil
}

func ingestFunc(db store.Backend, fset *token.FileSet, mod *store.Module, file *ast.File, fn *ast.FuncDecl, isTest bool, sourceFile string, state *ingestState) error {
	start := fset.Position(fn.Pos())
	end := fset.Position(fn.End())

	var receiver string
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		receiver = receiverTypeName(fn.Recv.List[0].Type)
	}

	kind := "function"
	if receiver != "" {
		kind = "method"
	}

	body := renderNode(fset, fn)
	sig := renderSignature(fset, fn)
	doc := fn.Doc.Text()

	// Multiple PACKAGE-LEVEL init() functions are valid in Go. Give each
	// a unique name so they don't overwrite each other in the database.
	// Keyed by (module, sourceFile), not module alone -- see
	// ingestState's initCounter doc comment for why a module-wide
	// counter is unstable across ingest modes.
	//
	// #354: gated on receiver == "" -- a METHOD named init() (e.g.
	// func (rw *responseWriter) init()) is NOT the same Go quirk this
	// counter exists for. A method can never collide with a
	// package-level function of the same name (the receiver already
	// makes it a distinct identifier; Go itself would reject two
	// init() methods on the SAME receiver as a real redeclaration
	// error, same as any other method name), so it must keep its bare
	// "init" name -- disambiguated by Receiver, exactly like every
	// other method. Applying this counter to a method anyway
	// (confirmed live via caddy-6179, modules/caddyhttp/encode/
	// encode.go: a package-level init() at line 37 followed by
	// (*responseWriter).init() at line 438) renamed the METHOD to
	// "init_1" in the DB -- a name that doesn't match anything in the
	// actual .go source text (methods are matched by identifier, not a
	// synthetic DB-only alias). Every subsequent edit/create in that
	// file "could not be matched to an on-disk declaration" for
	// *responseWriter.init_1, and emit -- unable to find where "init_1"
	// belonged -- appended a byte-for-byte duplicate of the untouched
	// original method to the end of the file instead, corrupting it
	// with a genuine "method already declared" Go compile error the
	// agent never asked for and had never touched.
	name := fn.Name.Name
	if name == "init" && receiver == "" {
		key := fmt.Sprintf("%d:%s", mod.ID, sourceFile)
		n := state.initCounter[key]
		if n > 0 {
			name = fmt.Sprintf("init_%d", n)
		}
		state.initCounter[key]++
	}

	def := &store.Definition{
		ModuleID:   mod.ID,
		Name:       name,
		Kind:       kind,
		Exported:   fn.Name.IsExported(),
		Test:       isTest,
		Receiver:   receiver,
		Signature:  sig,
		Body:       body,
		Doc:        doc,
		StartLine:  start.Line,
		EndLine:    end.Line,
		SourceFile: sourceFile,
	}

	state.enqueueDef(def)
	return nil
}

// containsIota checks if a GenDecl contains iota in any of its value specs.
func containsIota(gd *ast.GenDecl) bool {
	if gd.Tok != token.CONST {
		return false
	}
	found := false
	ast.Inspect(gd, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && ident.Name == "iota" {
			found = true
			return false
		}
		return !found
	})
	return found
}

func ingestGenDecl(db store.Backend, fset *token.FileSet, mod *store.Module, file *ast.File, gd *ast.GenDecl, isTest bool, sourceFile string, state *ingestState) error {
	grouped := gd.Lparen.IsValid() // parenthesized group: const (...), var (...), type (...)

	// Iota const blocks must be stored as a single definition because
	// individual specs depend on their position in the block.
	if grouped && containsIota(gd) {
		body := renderNode(fset, gd)
		doc := gd.Doc.Text()
		// Use the first name as the definition name.
		firstName := "const_group"
		if vs, ok := gd.Specs[0].(*ast.ValueSpec); ok && len(vs.Names) > 0 {
			firstName = vs.Names[0].Name
		}
		start := fset.Position(gd.Pos())
		end := fset.Position(gd.End())
		def := &store.Definition{
			ModuleID:   mod.ID,
			Name:       firstName,
			Kind:       "const",
			Exported:   ast.IsExported(firstName),
			Test:       isTest,
			Signature:  fmt.Sprintf("const %s (iota group)", firstName),
			Body:       body,
			Doc:        doc,
			StartLine:  start.Line,
			EndLine:    end.Line,
			SourceFile: sourceFile,
		}
		state.enqueueDef(def)
		return nil
	}

	for _, spec := range gd.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			kind := "type"
			if _, ok := s.Type.(*ast.InterfaceType); ok {
				kind = "interface"
			}

			// For grouped type declarations, render just this spec.
			// For standalone, render the whole GenDecl (includes "type" keyword).
			var body string
			if grouped {
				body = renderNode(fset, s)
			} else {
				body = renderNode(fset, gd)
			}

			start := fset.Position(s.Pos())
			end := fset.Position(s.End())
			doc := gd.Doc.Text()
			if doc == "" {
				doc = s.Doc.Text()
			}

			def := &store.Definition{
				ModuleID:   mod.ID,
				Name:       s.Name.Name,
				Kind:       kind,
				Exported:   s.Name.IsExported(),
				Test:       isTest,
				Signature:  fmt.Sprintf("type %s", s.Name.Name),
				Body:       body,
				Doc:        doc,
				StartLine:  start.Line,
				EndLine:    end.Line,
				SourceFile: sourceFile,
			}
			state.enqueueDef(def)

			// #11: struct fields get their own "field" kind definitions,
			// Name=field, Receiver=declaring type -- the same shape methods
			// already use, so Type.Field resolves via the existing
			// receiver.method name-lookup path with no resolver changes.
			if st, ok := s.Type.(*ast.StructType); ok {
				ingestStructFields(&structFieldCtx{fset: fset, mod: mod, isTest: isTest, sourceFile: sourceFile, state: state}, st, s.Name.Name)
			}

		case *ast.ValueSpec:
			c := &valueSpecCtx{
				db: db, fset: fset, mod: mod, gd: gd,
				isTest: isTest, sourceFile: sourceFile, state: state,
			}
			if err := ingestValueSpec(c, s); err != nil {
				return err
			}
		}
	}
	return nil
}

// valueSpecCtx bundles the per-file parameters a ValueSpec ingest
// needs, so the helper doesn't have to pass nine positional args.
type valueSpecCtx struct {
	db         store.Backend
	fset       *token.FileSet
	mod        *store.Module
	gd         *ast.GenDecl
	isTest     bool
	sourceFile string
	state      *ingestState
}

func ingestValueSpec(c *valueSpecCtx, s *ast.ValueSpec) error {
	kind := "var"
	if c.gd.Tok == token.CONST {
		kind = "const"
	}
	// First non-blank name owns the spec. Multi-name specs (var x, y int)
	// are stored once under the first name — the body contains all names.
	firstName, exported := firstNonBlankName(s.Names)
	if firstName == "" {
		return nil
	}

	// Grouped specs render just the spec; standalone renders the whole
	// GenDecl so the `var`/`const` keyword is preserved in the body.
	body := renderNode(c.fset, s)
	if !c.gd.Lparen.IsValid() {
		body = renderNode(c.fset, c.gd)
	}

	doc := c.gd.Doc.Text()
	if doc == "" {
		doc = s.Doc.Text()
	}
	specStart := c.fset.Position(s.Pos())
	specEnd := c.fset.Position(s.End())
	def := &store.Definition{
		ModuleID:   c.mod.ID,
		Name:       firstName,
		Kind:       kind,
		Exported:   exported,
		Test:       c.isTest,
		Signature:  valueSpecSignature(kind, firstName, s),
		Body:       body,
		Doc:        doc,
		StartLine:  specStart.Line,
		EndLine:    specEnd.Line,
		SourceFile: c.sourceFile,
	}
	c.state.enqueueDef(def)
	return nil
}

func firstNonBlankName(names []*ast.Ident) (string, bool) {
	for _, n := range names {
		if n.Name != "_" {
			return n.Name, n.IsExported()
		}
	}
	return "", false
}

// ingestStructFields enqueues one "field" kind definition per named
// field of a struct type declaration (#11). Name is the field name,
// Receiver is the declaring struct's type name, mirroring how method
// definitions already use Receiver -- so a "Type.Field" lookup
// resolves via the same receiver.method name-parsing path
// GetDefinitionByName already has for methods, with no resolver
// changes needed. Anonymous/embedded fields are skipped; the issue
// calls those a bonus, not a requirement.
func ingestStructFields(c *structFieldCtx, st *ast.StructType, typeName string) {
	if st.Fields == nil {
		return
	}
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		start := c.fset.Position(field.Pos())
		end := c.fset.Position(field.End())
		doc := field.Doc.Text()
		if doc == "" {
			doc = field.Comment.Text()
		}
		sig := renderNode(c.fset, field.Type)
		body := renderNode(c.fset, field)
		for _, name := range field.Names {
			def := &store.Definition{
				ModuleID:   c.mod.ID,
				Name:       name.Name,
				Kind:       "field",
				Exported:   name.IsExported(),
				Test:       c.isTest,
				Receiver:   typeName,
				Signature:  sig,
				Body:       body,
				Doc:        doc,
				StartLine:  start.Line,
				EndLine:    end.Line,
				SourceFile: c.sourceFile,
			}
			c.state.enqueueDef(def)
		}
	}
}

// structFieldCtx bundles the per-type parameters ingestStructFields
// needs, matching valueSpecCtx's rationale: too many positional args.
type structFieldCtx struct {
	fset       *token.FileSet
	mod        *store.Module
	isTest     bool
	sourceFile string
	state      *ingestState
}

// receiverTypeName extracts a method's receiver type name for storage in
// Definition.Receiver. Delegates to astutil.BareReceiverName -- see its
// doc comment for why this used to be an independently-maintained copy
// (and why typeString, used for composite-literal types, is the wrong
// tool for a receiver identity key).
func receiverTypeName(e ast.Expr) string {
	return astutil.BareReceiverName(e)
}
