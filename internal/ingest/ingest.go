// Package ingest loads Go source code from disk, parses it with go/ast,
// extracts definitions, and stores them in the defn database.
package ingest

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/store"
	"golang.org/x/tools/go/packages"
)

// Ingest loads a Go module from modulePath and stores all definitions
// into the database. modulePath should be a directory containing go.mod.
func Ingest(db *store.DB, modulePath string) error {
	clearSourceFileCache()

	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedDeps |
			packages.NeedEmbedPatterns,
		Dir:   modulePath,
		Tests: true, // include test packages
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return fmt.Errorf("load packages: %w", err)
	}

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
	return nil
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
			if err := ingestValueSpec(db, fset, mod, gd, s, grouped, isTest, sourceFile, state); err != nil {
				return err
			}
		}
	}
	return nil
}

func ingestValueSpec(db *store.DB, fset *token.FileSet, mod *store.Module, gd *ast.GenDecl, s *ast.ValueSpec, grouped, isTest bool, sourceFile string, state *ingestState) error {
	kind := "var"
	if gd.Tok == token.CONST {
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
	body := renderNode(fset, s)
	if !grouped {
		body = renderNode(fset, gd)
	}

	doc := gd.Doc.Text()
	if doc == "" {
		doc = s.Doc.Text()
	}
	specStart := fset.Position(s.Pos())
	specEnd := fset.Position(s.End())
	def := &store.Definition{
		ModuleID:   mod.ID,
		Name:       firstName,
		Kind:       kind,
		Exported:   exported,
		Test:       isTest,
		Signature:  valueSpecSignature(kind, firstName, s),
		Body:       body,
		Doc:        doc,
		StartLine:  specStart.Line,
		EndLine:    specEnd.Line,
		SourceFile: sourceFile,
	}
	id, err := db.UpsertDefinition(def)
	if err != nil {
		return err
	}
	state.liveDefIDs[id] = true
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
