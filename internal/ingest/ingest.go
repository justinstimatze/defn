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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/store"
	"golang.org/x/tools/go/packages"
)

// Ingest loads a Go module from modulePath and stores all definitions
// into the database. modulePath should be a directory containing go.mod.
func Ingest(db *store.DB, modulePath string) error {
	pkgs, err := goload.LoadAll(modulePath)
	if err != nil {
		return err
	}
	return IngestPackages(db, pkgs, modulePath)
}

// IngestPackages is like Ingest but accepts pre-loaded packages.
// Use with goload.LoadAll to share one packages.Load between ingest
// and resolve, saving ~1-2 GB of memory.
func IngestPackages(db *store.DB, pkgs []*packages.Package, modulePath string) error {
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
		initCounter: make(map[int64]int),
		liveDefIDs:  make(map[int64]bool),
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

	for _, pkg := range goload.FilterPackages(pkgs) {
		if err := ingestPackage(db, pkg, modulePath, state); err != nil {
			return fmt.Errorf("ingest %s: %w", pkg.PkgPath, err)
		}
	}

	// Remove definitions that no longer exist in the source code.
	if pruned, err := db.PruneStaleDefinitions(state.liveDefIDs); err != nil {
		return fmt.Errorf("prune stale: %w", err)
	} else if pruned > 0 {
		fmt.Fprintf(os.Stderr, "pruned %d stale definitions\n", pruned)
	}

	// Record last ingest timestamp for staleness detection.
	if err := db.SetMeta("last_ingest", strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		return fmt.Errorf("set last_ingest: %w", err)
	}

	return nil
}

func ingestPackage(db *store.DB, pkg *packages.Package, modulePath string, state *ingestState) error {
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

	for _, file := range pkg.Syntax {
		// Get source filename from the token.FileSet.
		isTest := false
		sourceFile := ""
		if file.Pos().IsValid() {
			absFile := pkg.Fset.Position(file.Pos()).Filename
			isTest = strings.HasSuffix(absFile, "_test.go")
			// Make relative to module root.
			if rel, err := filepath.Rel(modulePath, absFile); err == nil {
				sourceFile = rel
			} else {
				sourceFile = filepath.Base(absFile)
			}
		}
		if err := ingestFile(db, pkg, mod, file, isTest, sourceFile, state); err != nil {
			return err
		}
	}
	return nil
}

// ingestEmbedFiles finds //go:embed referenced files in a package
// and stores them as project files with their relative paths.
func ingestEmbedFiles(db *store.DB, pkg *packages.Package, modulePath string) error {
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

func ingestFile(db *store.DB, pkg *packages.Package, mod *store.Module, file *ast.File, isTest bool, sourceFile string, state *ingestState) error {
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

	// Extract comments and pragmas.
	if sourceFile != "" && len(file.Comments) > 0 {
		if err := ingestComments(db, fset, file, sourceFile); err != nil {
			return fmt.Errorf("ingest comments: %w", err)
		}
	}

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
func ingestComments(db *store.DB, fset *token.FileSet, file *ast.File, sourceFile string) error {
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
	// Build a map from startLine to defID for matching.
	defByLine := make(map[int]int64)
	for _, d := range defs {
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
	initCounter map[int64]int  // tracks init functions per module
	liveDefIDs  map[int64]bool // tracks all definition IDs seen
}

func ingestFunc(db *store.DB, fset *token.FileSet, mod *store.Module, file *ast.File, fn *ast.FuncDecl, isTest bool, sourceFile string, state *ingestState) error {
	start := fset.Position(fn.Pos())
	end := fset.Position(fn.End())

	var receiver string
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		receiver = typeString(fn.Recv.List[0].Type)
	}

	kind := "function"
	if receiver != "" {
		kind = "method"
	}

	body := renderNode(fset, fn)
	sig := renderSignature(fset, fn)
	doc := fn.Doc.Text()

	// Multiple init() functions are valid in Go. Give each a unique name
	// so they don't overwrite each other in the database.
	name := fn.Name.Name
	if name == "init" {
		n := state.initCounter[mod.ID]
		if n > 0 {
			name = fmt.Sprintf("init_%d", n)
		}
		state.initCounter[mod.ID]++
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

	id, err := db.UpsertDefinition(def)
	if err != nil {
		return err
	}
	state.liveDefIDs[id] = true
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

func ingestGenDecl(db *store.DB, fset *token.FileSet, mod *store.Module, file *ast.File, gd *ast.GenDecl, isTest bool, sourceFile string, state *ingestState) error {
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
		id, err := db.UpsertDefinition(def)
		if err != nil {
			return err
		}
		state.liveDefIDs[id] = true
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
			id, err := db.UpsertDefinition(def)
			if err != nil {
				return err
			}
			state.liveDefIDs[id] = true

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
	db         *store.DB
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
	id, err := c.db.UpsertDefinition(def)
	if err != nil {
		return err
	}
	c.state.liveDefIDs[id] = true
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
