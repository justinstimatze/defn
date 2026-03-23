// Package emit generates .go source files from the database.
// This is the inverse of ingest — it emits the database back into
// files that `go build` can compile.
package emit

import (
	"fmt"
	"go/format"
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

	// Split definitions into non-test and test.
	var nonTestDefs, testDefs []store.Definition
	for _, d := range defs {
		if d.Test {
			testDefs = append(testDefs, d)
		} else {
			nonTestDefs = append(nonTestDefs, d)
		}
	}

	var allLocs []DefLocation

	// Emit non-test definitions. Use lowercase filename to avoid
	// case-insensitive filesystem collisions (e.g. ginS.go vs gins.go on macOS).
	if len(nonTestDefs) > 0 {
		path := filepath.Join(pkgDir, strings.ToLower(mod.Name)+".go")
		locs, err := writeFile(path, mod.Name, mod.Path, mod.Doc, imports, nonTestDefs)
		if err != nil {
			return nil, err
		}
		allLocs = append(allLocs, locs...)
	}

	// Emit test definitions into a _test.go file.
	if len(testDefs) > 0 {
		path := filepath.Join(pkgDir, strings.ToLower(mod.Name)+"_test.go")
		locs, err := writeFile(path, mod.Name, mod.Path, "", imports, testDefs)
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
		for _, line := range strings.Split(pkgDoc, "\n") {
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
				for _, specLine := range strings.Split(defs[k].Body, "\n") {
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
	if err := os.WriteFile(path, formatted, 0644); err != nil {
		return nil, err
	}

	return buildLocIndex(path, modulePath, defs), nil
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
	for _, line := range strings.Split(d.Body, "\n") {
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
