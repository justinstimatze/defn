// Package emit generates .go source files from the database.
// This is the inverse of ingest — it emits the database back into
// files that `go build` can compile.
package emit

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
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

	for _, mod := range modules {
		locs, err := emitModule(db, &mod, outDir, moduleRoot)
		if err != nil {
			return nil, fmt.Errorf("emit %s: %w", mod.Path, err)
		}
		allLocs = append(allLocs, locs...)
	}

	// Run goimports to fix unused imports and formatting.
	goimports, err := exec.LookPath("goimports")
	if err != nil {
		return nil, fmt.Errorf("goimports not found — install with: go install golang.org/x/tools/cmd/goimports@latest")
	}
	if out, err := exec.Command(goimports, "-w", outDir).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("goimports: %s", out)
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

func emitModule(db *store.DB, mod *store.Module, outDir, moduleRoot string) ([]DefLocation, error) {
	defs, err := db.GetModuleDefinitions(mod.ID)
	if err != nil {
		return nil, err
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
		return nil, nil
	}

	// Get imports for this module.
	imports, err := db.GetImports(mod.ID)
	if err != nil {
		return nil, err
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
		return nil, err
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
	docWritten := false

	for file, fileDefs := range byFile {
		path := filepath.Join(pkgDir, file)
		// Only include package doc on the first non-test file.
		pkgDoc := ""
		if !docWritten && !strings.HasSuffix(file, "_test.go") {
			pkgDoc = mod.Doc
			docWritten = true
		}
		locs, err := writeFile(path, mod.Name, mod.Path, pkgDoc, imports, fileDefs)
		if err != nil {
			return nil, err
		}
		allLocs = append(allLocs, locs...)
	}

	return allLocs, nil
}

func writeFile(path, pkgName, modulePath, pkgDoc string, imports []store.Import, defs []store.Definition) ([]DefLocation, error) {
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
