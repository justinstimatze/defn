// Package emit generates .go source files from the database.
// This is the inverse of ingest — it emits the database back into
// files that `go build` can compile.
package emit

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
)

// DefLocation records where a definition was placed in an emitted file,
// so diagnostics can be mapped back to defn terms.
type DefLocation struct {
	DefID     int64
	DefName   string
	Kind      string
	Module    string
	File      string // emitted file path
	StartLine int    // 1-based line in emitted file where body begins
	EndLine   int    // 1-based line in emitted file where body ends
}

// Emit writes all definitions from the database as .go files into outDir.
// Each module becomes a directory, and definitions are grouped into files by kind.
func Emit(db *store.DB, outDir string) error {
	_, err := EmitWithMap(db, outDir)
	return err
}

// EmitWithMap is like Emit but also returns a source map: for each emitted
// line, which definition it belongs to. This powers defn lint.
func EmitWithMap(db *store.DB, outDir string) ([]DefLocation, error) {
	var allLocs []DefLocation

	// Write project-level files (go.mod, go.sum).
	projectFiles, err := db.ListProjectFiles()
	if err != nil {
		return nil, fmt.Errorf("list project files: %w", err)
	}
	for _, pf := range projectFiles {
		content, err := db.GetProjectFile(pf)
		if err != nil {
			return nil, fmt.Errorf("get project file %s: %w", pf, err)
		}
		// Sanitize path to prevent directory traversal.
		clean := filepath.Clean(pf)
		if filepath.IsAbs(clean) || strings.Contains(clean, "..") {
			return nil, fmt.Errorf("invalid project file path: %s", pf)
		}
		dst := filepath.Join(outDir, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, []byte(content), 0644); err != nil {
			return nil, fmt.Errorf("write %s: %w", pf, err)
		}
	}

	modules, err := db.ListModules()
	if err != nil {
		return nil, fmt.Errorf("list modules: %w", err)
	}

	// Determine the module root path from go.mod so we can compute
	// relative directories for each package.
	moduleRoot := detectModuleRoot(modules)

	var writtenFiles []writtenFile
	for _, mod := range modules {
		locs, written, err := emitModule(db, &mod, outDir, moduleRoot)
		if err != nil {
			return nil, fmt.Errorf("emit %s: %w", mod.Path, err)
		}
		allLocs = append(allLocs, locs...)
		writtenFiles = append(writtenFiles, written...)
	}

	// Run goimports to fix unused imports and formatting.
	goimports, err := exec.LookPath("goimports")
	if err != nil {
		return nil, fmt.Errorf("goimports not found — install with: go install golang.org/x/tools/cmd/goimports@latest")
	}
	if out, err := exec.Command(goimports, "-w", outDir).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("goimports: %s", out)
	}

	// Refresh file_sources with the post-goimports bytes so it stays in
	// sync with disk. Without this, the authoritative raw source drifts
	// every time emit rewrites a file (body edits, reorders, import
	// additions) until the next full re-ingest.
	//
	// Note on the safety-net case: if safeWriteGoFile declined to write
	// (because regenerating would drop an on-disk decl defn's schema
	// can't represent), disk still has its pre-emit content. We re-read
	// that and stamp it here, which is the correct invariant — the
	// authoritative raw source must always match what's on disk. The
	// next merge will use this refreshed base, so hand-edited decls
	// that tripped the safety net are now carried forward rather than
	// lost on the following emit.
	for _, wf := range writtenFiles {
		if wf.SourceFile == "" {
			continue
		}
		raw, err := os.ReadFile(wf.Path)
		if err != nil {
			continue
		}
		if err := db.SetFileSource(wf.ModuleID, wf.SourceFile, string(raw)); err != nil {
			return nil, fmt.Errorf("refresh file_sources for %s: %w", wf.SourceFile, err)
		}
	}

	// Rebuild location index after goimports (it may shift line numbers).
	allLocs = nil
	for _, mod := range modules {
		defs, err := db.GetModuleDefinitions(mod.ID)
		if err != nil || len(defs) == 0 {
			continue
		}
		relPath := mod.Path
		if moduleRoot != "" && strings.HasPrefix(mod.Path, moduleRoot) {
			relPath = strings.TrimPrefix(mod.Path, moduleRoot)
			relPath = strings.TrimPrefix(relPath, "/")
		}
		if relPath == "" {
			relPath = "."
		}
		pkgDir := filepath.Join(outDir, relPath)
		mainFile := filepath.Join(pkgDir, strings.ToLower(mod.Name)+".go")
		var nonTestDefs, testDefs []store.Definition
		for _, d := range defs {
			if d.Test {
				testDefs = append(testDefs, d)
			} else {
				nonTestDefs = append(nonTestDefs, d)
			}
		}
		if len(nonTestDefs) > 0 {
			allLocs = append(allLocs, buildLocIndex(mainFile, mod.Path, nonTestDefs)...)
		}
		if len(testDefs) > 0 {
			testFile := filepath.Join(pkgDir, strings.ToLower(mod.Name)+"_test.go")
			allLocs = append(allLocs, buildLocIndex(testFile, mod.Path, testDefs)...)
		}
	}

	return allLocs, nil
}

// detectModuleRoot finds the common module root from the stored module paths.
// For a Go project, this is the go.mod module path (e.g., "github.com/justinstimatze/defn").
// We detect it by finding the longest common prefix of all module paths that
// ends at a "/" boundary, then stripping one more component if the prefix
// itself is a stored module.
func detectModuleRoot(modules []store.Module) string {
	if len(modules) == 0 {
		return ""
	}
	// Find shortest path — it's likely the root or cmd package.
	// The module root is the prefix shared by all paths.
	prefix := modules[0].Path
	for _, m := range modules[1:] {
		for !strings.HasPrefix(m.Path, prefix) {
			idx := strings.LastIndex(prefix, "/")
			if idx < 0 {
				return ""
			}
			prefix = prefix[:idx]
		}
	}
	return prefix
}

// writtenFile records an emitted file so its post-goimports bytes can be
// written back to file_sources, keeping the authoritative raw source in
// sync with what's on disk.
type writtenFile struct {
	Path       string
	ModuleID   int64
	SourceFile string // project-relative; empty means don't refresh file_sources
}

func emitModule(db *store.DB, mod *store.Module, outDir, moduleRoot string) ([]DefLocation, []writtenFile, error) {
	defs, err := db.GetModuleDefinitions(mod.ID)
	if err != nil {
		return nil, nil, err
	}
	if len(defs) == 0 {
		// No definitions — clean up any previously emitted files for this module.
		relPath := mod.Path
		if moduleRoot != "" && strings.HasPrefix(mod.Path, moduleRoot) {
			relPath = strings.TrimPrefix(mod.Path, moduleRoot)
			relPath = strings.TrimPrefix(relPath, "/")
		}
		if relPath != "" && relPath != "." {
			pkgDir := filepath.Join(outDir, relPath)
			mainFile := filepath.Join(pkgDir, strings.ToLower(mod.Name)+".go")
			testFile := filepath.Join(pkgDir, strings.ToLower(mod.Name)+"_test.go")
			os.Remove(mainFile)
			os.Remove(testFile)
		}
		return nil, nil, nil
	}

	// Get imports for this module.
	imports, err := db.GetImports(mod.ID)
	if err != nil {
		return nil, nil, err
	}

	// Compute the relative directory path by stripping the module root.
	// e.g., "github.com/justinstimatze/defn/internal/store" → "internal/store"
	// For the root package itself (e.g., "github.com/justinstimatze/defn/cmd/defn") → "cmd/defn"
	relPath := mod.Path
	if moduleRoot != "" && strings.HasPrefix(mod.Path, moduleRoot) {
		relPath = strings.TrimPrefix(mod.Path, moduleRoot)
		relPath = strings.TrimPrefix(relPath, "/")
	}
	if relPath == "" {
		relPath = "."
	}
	pkgDir := filepath.Join(outDir, relPath)
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		return nil, nil, err
	}

	// Group definitions by source file. Use the basename because
	// source_file is stored as a project-relative path (e.g.
	// "internal/mcp/tools_extra.go"); joining that with pkgDir doubles
	// the directory prefix and writes land in a non-existent path.
	// If source_file is empty (old data), fall back to one file per package.
	byFile := map[string][]store.Definition{}
	for _, d := range defs {
		file := d.SourceFile
		if file == "" {
			if d.Test {
				file = strings.ToLower(mod.Name) + "_test.go"
			} else {
				file = strings.ToLower(mod.Name) + ".go"
			}
		} else {
			file = filepath.Base(file)
		}
		byFile[file] = append(byFile[file], d)
	}

	var allLocs []DefLocation
	var written []writtenFile
	docWritten := false

	// Phase C: pre-fetch the raw sources for this module. When present,
	// writeFile uses them as the authoritative merge base — that's the
	// byte-faithful copy, unaffected by whatever's on disk (which might
	// be stale or never have existed, e.g. fresh `defn emit /tmp/out`).
	rawMap, _ := db.ListFileSources(mod.ID)

	for file, fileDefs := range byFile {
		path := filepath.Join(pkgDir, file)
		// Only include package doc on the first non-test file.
		pkgDoc := ""
		if !docWritten && !strings.HasSuffix(file, "_test.go") {
			pkgDoc = mod.Doc
			docWritten = true
		}
		// Find the raw source for this file. source_file in the DB is
		// project-relative (e.g. "internal/mcp/tools_extra.go"), so look
		// up by the source_file of any def in this bucket.
		//
		// Invariant: all defs in a byFile bucket share the same
		// SourceFile. Buckets are keyed by basename, and within a
		// single module (one package directory) each basename maps to
		// exactly one project-relative path. So breaking at the first
		// def with a non-empty SourceFile yields the canonical one.
		var rawFromDB []byte
		var projectRelSource string
		for _, d := range fileDefs {
			if d.SourceFile != "" {
				projectRelSource = d.SourceFile
				if r, ok := rawMap[d.SourceFile]; ok {
					rawFromDB = []byte(r)
				}
				break
			}
		}
		locs, err := writeFile(path, mod.Name, mod.Path, pkgDoc, imports, fileDefs, rawFromDB)
		if err != nil {
			return nil, nil, err
		}
		allLocs = append(allLocs, locs...)
		written = append(written, writtenFile{
			Path:       path,
			ModuleID:   mod.ID,
			SourceFile: projectRelSource,
		})
	}

	return allLocs, written, nil
}

func writeFile(path, pkgName, modulePath, pkgDoc string, imports []store.Import, defs []store.Definition, rawFromDB []byte) ([]DefLocation, error) {
	// Phase C: file_sources.raw is the authoritative byte-faithful
	// representation. Prefer it over whatever is on disk, which might
	// be stale. Fall through to disk (Phase A) if the DB has nothing.
	existingSrc := rawFromDB
	if len(existingSrc) == 0 {
		if data, err := os.ReadFile(path); err == nil {
			existingSrc = data
		}
	}

	// AST-merge path: if we have a base file that parses, patch the
	// changed decl bodies into its AST and write the result. Preserves
	// everything defn's schema doesn't represent (package doc, build
	// constraints, per-file imports, init() names, floating comments).
	if len(existingSrc) > 0 {
		if merged, ok := mergeDeclsIntoSource(existingSrc, defs); ok {
			wrote, lost, err := safeWriteGoFile(path, merged)
			if err != nil {
				return nil, err
			}
			if wrote {
				return buildLocIndex(path, modulePath, defs), nil
			}
			fmt.Fprintf(os.Stderr,
				"defn: ast-merge safety net unexpectedly flagged %s (lost: %v) — falling back to regenerate\n",
				path, lost)
		}
	}

	// Build source by assembling each definition body into a parseable Go file.
	// This lets go/parser + go/format handle all formatting and line tracking,
	// eliminating manual line counting and grouped spec handling.
	var src strings.Builder

	// Package doc comment.
	if pkgDoc != "" {
		for line := range strings.SplitSeq(pkgDoc, "\n") {
			src.WriteString("// " + line + "\n")
		}
	}
	src.WriteString(fmt.Sprintf("package %s\n\n", pkgName))

	// Import block.
	if len(imports) > 0 {
		src.WriteString("import (\n")
		for _, imp := range imports {
			if imp.Alias != "" {
				src.WriteString(fmt.Sprintf("\t%s %q\n", imp.Alias, imp.ImportedPath))
			} else {
				src.WriteString(fmt.Sprintf("\t%q\n", imp.ImportedPath))
			}
		}
		src.WriteString(")\n\n")
	}

	// Definitions. Grouped specs get reassembled into blocks.
	i := 0
	for i < len(defs) {
		d := defs[i]
		if isGroupedSpec(d) {
			keyword := groupKeyword(d)
			j := i
			for j < len(defs) && isGroupedSpec(defs[j]) && groupKeyword(defs[j]) == keyword {
				j++
			}
			src.WriteString(fmt.Sprintf("%s (\n", keyword))
			for k := i; k < j; k++ {
				for specLine := range strings.SplitSeq(defs[k].Body, "\n") {
					src.WriteString("\t" + specLine + "\n")
				}
			}
			src.WriteString(")\n\n")
			i = j
		} else {
			src.WriteString(d.Body)
			src.WriteString("\n\n")
			i++
		}
	}

	// Format with go/format for canonical output. format.Source handles
	// parsing internally — no need to parse separately.
	formatted, err := format.Source([]byte(src.String()))
	if err != nil {
		// format.Source failed (invalid Go in body) — write raw source.
		// go build will catch syntax errors.
		formatted = []byte(src.String())
	}
	wrote, lost, err := safeWriteGoFile(path, formatted)
	if err != nil {
		return nil, err
	}
	if !wrote {
		// The DB's reconstruction would remove top-level declarations that
		// exist on disk (most often init(), ingested under a renamed name,
		// or top-level code the current schema can't represent). Keep the
		// file intact; downstream callers (lint, etc.) that need locations
		// will get an empty slice for this file — safer than corruption.
		fmt.Fprintf(os.Stderr,
			"defn: skipping emit of %s to preserve %d on-disk declaration(s) not in the database: %v\n",
			path, len(lost), lost)
		return nil, nil
	}
	return buildLocIndex(path, modulePath, defs), nil
}

// safeWriteGoFile writes content to path only if doing so will not remove
// any top-level named declaration that currently exists on disk. If a
// declaration would be lost, returns wrote=false with the list of lost
// names and no error; the caller is expected to log and move on.
//
// This is a defense against the database's representation being lossier
// than the on-disk source: a roundtrip edit/emit should never silently
// delete user code.
func safeWriteGoFile(path string, content []byte) (wrote bool, lost []string, err error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil, os.WriteFile(path, content, 0644)
		}
		return false, nil, err
	}

	oldDecls, oldParseErr := topLevelDeclNames(existing)
	if oldParseErr != nil {
		// On-disk file doesn't parse — safer to leave it alone than to
		// blindly replace broken code with something the caller may not
		// expect. A human can delete the file and re-emit explicitly.
		return false, nil, fmt.Errorf("cannot safety-check %s: existing file doesn't parse: %w", path, oldParseErr)
	}
	newDecls, newParseErr := topLevelDeclNames(content)
	if newParseErr != nil {
		return false, nil, fmt.Errorf("cannot safety-check %s: generated content doesn't parse: %w", path, newParseErr)
	}

	newSet := make(map[string]bool, len(newDecls))
	for _, n := range newDecls {
		newSet[n] = true
	}
	for _, n := range oldDecls {
		if !newSet[n] {
			lost = append(lost, n)
		}
	}
	if len(lost) > 0 {
		return false, lost, nil
	}
	return true, nil, os.WriteFile(path, content, 0644)
}

// topLevelDeclNames returns the qualified names of every top-level
// declaration in a Go source file: free functions as "Name", methods as
// "<Recv>.Name", and var/const/type specs as their spec name. Anonymous
// specs and blank identifiers are skipped.
func topLevelDeclNames(src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				if recv := recvTypeName(d.Recv.List[0].Type); recv != "" {
					name = recv + "." + name
				}
			}
			names = append(names, name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name != "_" {
							names = append(names, n.Name)
						}
					}
				case *ast.TypeSpec:
					names = append(names, s.Name.Name)
				}
			}
		}
	}
	return names, nil
}

// recvTypeName extracts the receiver type name for a method declaration,
// unwrapping pointer receivers and generic type params.
func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + recvTypeName(t.X)
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return ""
}

// buildLocIndex re-reads an emitted file and finds each definition's line.
func buildLocIndex(path, modulePath string, defs []store.Definition) []DefLocation {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	var locs []DefLocation
	for _, d := range defs {
		var searchFor string
		switch d.Kind {
		case "function":
			searchFor = "func " + d.Name + "("
		case "method":
			searchFor = ") " + d.Name + "("
		case "type", "interface":
			searchFor = "type " + d.Name + " "
		default:
			searchFor = d.Name
		}
		for i, line := range lines {
			if strings.Contains(strings.TrimSpace(line), searchFor) {
				locs = append(locs, DefLocation{
					DefID: d.ID, DefName: d.Name, Kind: d.Kind,
					Module: modulePath, File: path,
					StartLine: i + 1,
				})
				break
			}
		}
	}
	return locs
}

// isGroupedSpec returns true if the definition body is a spec extracted from
// a grouped declaration (doesn't contain the keyword at a line start).
func isGroupedSpec(d store.Definition) bool {
	if d.Kind != "const" && d.Kind != "var" && d.Kind != "type" && d.Kind != "interface" {
		return false
	}
	// Standalone declarations have the keyword at the start of a line:
	//   "type Foo struct { ... }" or "// Doc\ntype Foo struct { ... }"
	// Grouped specs are just the spec body: "Foo struct { ... }" or "X = 1"
	for line := range strings.SplitSeq(d.Body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "const ") ||
			strings.HasPrefix(trimmed, "var ") ||
			strings.HasPrefix(trimmed, "type ") {
			return false
		}
	}
	return true
}

func groupKeyword(d store.Definition) string {
	if d.Kind == "interface" {
		return "type"
	}
	return d.Kind
}

// mergeDeclsIntoSource patches declaration bodies in existing Go source
// by splicing DB bodies into the byte ranges occupied by their on-disk
// counterparts. Works at the byte level rather than editing the parsed
// AST, which preserves:
//
//   - Per-spec doc comments on grouped declarations (AST surgery with a
//     foreign fset drops the position association and format.Node then
//     renders the comment as an orphan floating between specs).
//   - Whitespace, blank-line grouping, and free-floating comments
//     outside the replaced ranges.
//
// The parsed AST is only used to find each decl's byte offsets; no
// tree mutation happens.
//
// Ok=true means a merged file was produced and is safe to write.
// Ok=false means the caller should fall back to regenerating — the
// source doesn't parse, the result after splicing doesn't parse, or
// nothing in defs matched an on-disk decl.
func mergeDeclsIntoSource(existing []byte, defs []store.Definition) ([]byte, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", existing, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, false
	}

	wantFuncs := make(map[string]string)
	wantTypes := make(map[string]string)
	wantConsts := make(map[string]string)
	wantVars := make(map[string]string)
	// wantGrouped holds bodies that represent a whole grouped GenDecl
	// (e.g. an iota const block that ingest stores as a single def under
	// the first name). These can't be spliced into one spec's range —
	// the whole parenthesized block has to be replaced.
	wantGrouped := make(map[string]string)
	for _, d := range defs {
		switch d.Kind {
		case "function", "method":
			wantFuncs[funcIdentity(d.Name, d.Receiver)] = d.Body
		case "type", "interface", "const", "var":
			if bodyIsGroupedGenDecl(d.Body) {
				wantGrouped[d.Name] = d.Body
				continue
			}
			switch d.Kind {
			case "type", "interface":
				wantTypes[d.Name] = d.Body
			case "const":
				wantConsts[d.Name] = d.Body
			case "var":
				wantVars[d.Name] = d.Body
			}
		}
	}
	if len(wantFuncs) == 0 && len(wantTypes) == 0 &&
		len(wantConsts) == 0 && len(wantVars) == 0 &&
		len(wantGrouped) == 0 {
		return nil, false
	}

	type replacement struct {
		start, end int
		body       string
	}
	var reps []replacement

	// declRange returns the byte range for a declaration or spec, using
	// the Doc position as the start when includeDoc is true. This
	// matches renderNode's behavior at ingest: FuncDecl/GenDecl bodies
	// include the leading doc comment (so the replacement range must
	// too); grouped-spec bodies don't, so we use s.Pos() directly.
	declRange := func(start, end token.Pos, doc *ast.CommentGroup, includeDoc bool) (int, int) {
		sp := fset.Position(start).Offset
		if includeDoc && doc != nil {
			if dp := fset.Position(doc.Pos()).Offset; dp >= 0 && dp < sp {
				sp = dp
			}
		}
		return sp, fset.Position(end).Offset
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			recv := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv = recvTypeName(d.Recv.List[0].Type)
			}
			body, ok := wantFuncs[funcIdentity(d.Name.Name, recv)]
			if !ok {
				continue
			}
			s, e := declRange(d.Pos(), d.End(), d.Doc, true)
			reps = append(reps, replacement{s, e, body})
		case *ast.GenDecl:
			// Whole-decl replacement: ingest bundles iota const blocks
			// (and any future whole-GenDecl case) under the first spec
			// name. Match on that before falling through to per-spec
			// splicing, which would otherwise try to cram the whole
			// parenthesized block into a single spec's byte range.
			if name := firstSpecName(d); name != "" {
				if body, ok := wantGrouped[name]; ok {
					sp, ep := declRange(d.Pos(), d.End(), d.Doc, true)
					reps = append(reps, replacement{sp, ep, body})
					continue
				}
			}
			grouped := d.Lparen.IsValid()
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if d.Tok != token.TYPE {
						continue
					}
					body, ok := wantTypes[s.Name.Name]
					if !ok {
						continue
					}
					if grouped {
						sp, ep := declRange(s.Pos(), s.End(), nil, false)
						reps = append(reps, replacement{sp, ep, body})
					} else {
						sp, ep := declRange(d.Pos(), d.End(), d.Doc, true)
						reps = append(reps, replacement{sp, ep, body})
					}
				case *ast.ValueSpec:
					// Multi-name specs (var a, b = 1, 2) share a single
					// DB def under the first name; partial patching
					// would leak the wrong value into siblings. Fall
					// through to regeneration.
					if len(s.Names) != 1 {
						continue
					}
					name := s.Names[0].Name
					var body string
					var ok bool
					switch d.Tok {
					case token.CONST:
						body, ok = wantConsts[name]
					case token.VAR:
						body, ok = wantVars[name]
					}
					if !ok {
						continue
					}
					if grouped {
						sp, ep := declRange(s.Pos(), s.End(), nil, false)
						reps = append(reps, replacement{sp, ep, body})
					} else {
						sp, ep := declRange(d.Pos(), d.End(), d.Doc, true)
						reps = append(reps, replacement{sp, ep, body})
					}
				}
			}
		}
	}

	if len(reps) == 0 {
		return nil, false
	}

	// Apply in reverse offset order so earlier splices don't invalidate
	// later offsets. Byte ranges for distinct decls never overlap (Go
	// syntax forbids it), so ordering by start offset is total.
	sort.Slice(reps, func(i, j int) bool { return reps[i].start > reps[j].start })
	result := append([]byte{}, existing...)
	for _, r := range reps {
		if r.start < 0 || r.end > len(result) || r.start > r.end {
			return nil, false
		}
		var buf bytes.Buffer
		buf.Grow(len(result) - (r.end - r.start) + len(r.body))
		buf.Write(result[:r.start])
		buf.WriteString(r.body)
		buf.Write(result[r.end:])
		result = buf.Bytes()
	}

	// Validate the spliced result parses. DB bodies are trusted, but a
	// corrupted body or an off-by-one offset should fail safe rather
	// than write invalid Go to disk.
	if _, err := parser.ParseFile(token.NewFileSet(), "", result,
		parser.ParseComments|parser.SkipObjectResolution); err != nil {
		return nil, false
	}
	return result, true
}

// bodyIsGroupedGenDecl reports whether a DB body is a parenthesized
// GenDecl (type (...), const (...), var (...)). Ingest renders iota
// const blocks this way: one def under the first name with the whole
// block as body. For those, the splice target must be the on-disk
// GenDecl's full range, not just the first spec's.
func bodyIsGroupedGenDecl(body string) bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", "package x\n\n"+body, parser.SkipObjectResolution)
	if err != nil || len(f.Decls) == 0 {
		return false
	}
	gd, ok := f.Decls[0].(*ast.GenDecl)
	if !ok {
		return false
	}
	return gd.Lparen.IsValid()
}

// firstSpecName returns the first declared name in a GenDecl, or "" if
// the decl is empty or imports-only. Used to match on-disk GenDecls
// against whole-decl DB bodies (which are keyed by first-spec name).
func firstSpecName(d *ast.GenDecl) string {
	if len(d.Specs) == 0 {
		return ""
	}
	switch s := d.Specs[0].(type) {
	case *ast.TypeSpec:
		return s.Name.Name
	case *ast.ValueSpec:
		if len(s.Names) > 0 {
			return s.Names[0].Name
		}
	}
	return ""
}

// funcIdentity produces the identity key used to match DB definitions
// to AST FuncDecls. Free functions and methods share the same space:
// "Foo" for a free function, "*Server.Foo" for a pointer-receiver method.
func funcIdentity(name, receiver string) string {
	if receiver == "" {
		return name
	}
	return receiver + "." + name
}

