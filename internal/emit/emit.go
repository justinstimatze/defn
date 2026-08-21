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
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/justinstimatze/defn/internal/astutil"
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

// Opts controls emit behavior beyond "write the DB to outDir."
type Opts struct {
	// AllowedRemovals whitelists top-level decl names that safeWriteGoFile
	// is permitted to drop from disk. Used by handleDelete so an
	// intentional code(op:"delete") isn't blocked by the safety net that
	// exists to prevent *accidental* loss. Names are compared against
	// topLevelDeclNames on the pre-emit file bytes.
	AllowedRemovals []string

	// AllowedAdds whitelists top-level decl names that the AST-merge path
	// is permitted to append to disk when they exist in the DB but not on
	// disk. Symmetric with AllowedRemovals — expresses caller intent for
	// a code(op:"create") so mergeDeclsIntoSource can splice the new def
	// in place, preserving floating comments and layout, instead of
	// falling through to full-file regeneration (which loses them, per
	// #162). Names without an on-disk counterpart AND without a matching
	// AllowedAdds entry are treated as drift signals and force the merge
	// to bail — same fail-safe as before this option existed.
	AllowedAdds []string

	// GoimportsFiles optionally restricts goimports's scope to a specific
	// set of files (module-relative paths under outDir). Empty (default)
	// runs `goimports -w outDir` recursively — the whole tree. Non-empty
	// runs `goimports -w <path1> <path2> ...` on only those files. #109
	// pass 3: on cli/cli's tree the recursive form was 707ms of a 797ms
	// warm-rename wall (89%); scoping to touched files collapses this to
	// per-file cost. Callers who know which files their mutation touched
	// (rename: def's file + all caller files; edit: def's file only)
	// should populate this. Non-existent paths are silently skipped by
	// goimports so it's safe to over-list.
	GoimportsFiles []string

	// TouchedFiles optionally restricts which files emit rewrites at all
	// (project-relative paths). Empty (default) emits every file for
	// every module — the full reconstruction. Non-empty limits both the
	// module-writes loop and the project-file writes to only these files:
	// rename → {def.SourceFile} ∪ {caller.SourceFile}, edit → {def.SourceFile},
	// add-import → {file}. On winze's 43-file corpus, a 3-file rename
	// wall was 1.2s full-emit; scoping to 3 files drops to per-file
	// cost. Winze dispatch 2026-07-22: this is THE remaining lever
	// once #109 moved the wall out of autoResolve. Non-existent paths
	// are silently skipped (mirrors GoimportsFiles semantics). When
	// non-empty, the loc-index rebuild + project-file writes are also
	// skipped — callers using TouchedFiles are the singleton-mutation
	// paths that don't consume the loc index and never touch go.mod.
	TouchedFiles []string

	// SkipGoimports skips the goimports subprocess entirely when the
	// caller has independently proven this edit's marginal contribution
	// to the file's import-need equation is zero-delta -- e.g. a
	// sig-stable body edit whose set of package-selector references is
	// unchanged (see bodyImportFootprintUnchanged in internal/mcp).
	// goimports is a subprocess spawn that runs unconditionally
	// otherwise; measured at ~25% of a warm sig-stable edit's wall even
	// when it had nothing to fix (2026-08-04 measure-edit spike).
	// Skipping it also skips goimports' gofmt-equivalent formatting pass
	// -- only set this when the incoming body is already well-formatted,
	// or accept minor cosmetic drift until the next non-skipped emit.
	SkipGoimports bool
}

// Emit writes all definitions from the database as .go files into outDir.
// Each module becomes a directory, and definitions are grouped into files by kind.
func Emit(db store.Backend, outDir string) error {
	_, _, err := emitWithOpts(db, outDir, Opts{})
	return err
}

// EmitWithMap is like Emit but also returns a source map: for each emitted
// line, which definition it belongs to. This powers defn lint.
func EmitWithMap(db store.Backend, outDir string) ([]DefLocation, error) {
	locs, _, err := emitWithOpts(db, outDir, Opts{})
	return locs, err
}

// EmitWithOpts is Emit with caller-supplied Opts (e.g. AllowedRemovals so
// a code(op:"delete") can actually land on disk without safeWriteGoFile
// blocking the write).
//
// The returned warnings (#218) are non-fatal: err is nil and the emit
// otherwise completed, but one or more requested changes could not be
// safely written to disk (a def's DB row didn't match any on-disk
// declaration, or writing would have dropped an on-disk declaration the
// database doesn't know about). Callers that surface emit results to a
// human or an agent MUST check this -- it is the difference between
// "the edit landed" and "the edit updated the graph but not the file."
func EmitWithOpts(db store.Backend, outDir string, opts Opts) ([]string, error) {
	_, warnings, err := emitWithOpts(db, outDir, opts)
	return warnings, err
}

// EmitWithMapAndOpts is EmitWithMap with caller-supplied Opts.
func EmitWithMapAndOpts(db store.Backend, outDir string, opts Opts) ([]DefLocation, error) {
	locs, _, err := emitWithOpts(db, outDir, opts)
	return locs, err
}

func emitWithOpts(db store.Backend, outDir string, opts Opts) ([]DefLocation, []string, error) {
	var allLocs []DefLocation
	var warnings []string
	timing := os.Getenv("DEFN_MEASURE_TIMING") == "1"
	timeIt := func(name string, t0 time.Time) {
		if timing {
			fmt.Fprintf(os.Stderr, "    [emit-inner] %s: %s\n", name, time.Since(t0).Round(time.Millisecond))
		}
	}

	// Build the project-relative scope set once. Empty = full emit,
	// non-empty = restrict module + project-file writes to just these
	// paths. Sanitize now so downstream loops can just check membership.
	scoped := len(opts.TouchedFiles) > 0
	touchedSet := make(map[string]bool, len(opts.TouchedFiles))
	for _, tf := range opts.TouchedFiles {
		clean := filepath.Clean(tf)
		if filepath.IsAbs(clean) || strings.Contains(clean, "..") {
			continue
		}
		touchedSet[filepath.ToSlash(clean)] = true
	}
	emitDebugf("scope: touchedFiles=%d goimportsFiles=%d skipGoimports=%v allowedRemovals=%d allowedAdds=%d",
		len(opts.TouchedFiles), len(opts.GoimportsFiles), opts.SkipGoimports, len(opts.AllowedRemovals), len(opts.AllowedAdds))
	if scoped {
		names := make([]string, 0, len(touchedSet))
		for f := range touchedSet {
			names = append(names, f)
		}
		sort.Strings(names)
		emitDebugf("touchedSet: %v", names)
	}

	// Write project-level files (go.mod, go.sum). Kept unconditional even
	// in scoped mode: a fresh tempdir needs go.mod to build, and the cost
	// (2 small files, a few ms) is negligible vs the emit for the .go
	// tree. #117 initially skipped these on scoped emit — that broke the
	// ceiling measurement path (fresh tempdir → no go.mod → build fails)
	// for a trivial optimization win.
	t := time.Now()
	projectFiles, err := db.ListProjectFiles()
	if err != nil {
		return nil, nil, fmt.Errorf("list project files: %w", err)
	}
	for _, pf := range projectFiles {
		content, err := db.GetProjectFile(pf)
		if err != nil {
			return nil, nil, fmt.Errorf("get project file %s: %w", pf, err)
		}
		// Sanitize path to prevent directory traversal.
		clean := filepath.Clean(pf)
		if filepath.IsAbs(clean) || strings.Contains(clean, "..") {
			return nil, nil, fmt.Errorf("invalid project file path: %s", pf)
		}
		dst := filepath.Join(outDir, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return nil, nil, err
		}
		// #217: unlike emitModule's per-.go-file write below, project
		// files have no merge logic at all -- this is a straight
		// overwrite. Warn (mirroring the .go disk-drift warning) when
		// disk no longer matches the DB's stored blob, since that means
		// a manual edit (or an out-of-band tool write) is about to be
		// silently clobbered. Full `defn ingest` (or code(op:"sync")
		// without file:) re-reads go:embed/project files from disk and
		// refreshes this row before that would happen.
		if onDisk, rerr := os.ReadFile(dst); rerr == nil && !bytes.Equal(onDisk, []byte(content)) {
			fmt.Fprintf(os.Stderr, "[emit] disk drift: %s changed on disk since defn last recorded it -- overwriting with the database's stored content; run a full `defn ingest` (or code(op:\"sync\") with no file:) first if this was a manual edit you want kept.\n", dst)
		}
		if err := os.WriteFile(dst, []byte(content), 0644); err != nil {
			return nil, nil, fmt.Errorf("write %s: %w", pf, err)
		}
	}
	timeIt("project-files", t)

	t = time.Now()
	modules, err := db.ListModules()
	if err != nil {
		return nil, nil, fmt.Errorf("list modules: %w", err)
	}

	// Determine the module root path from go.mod so we can compute
	// relative directories for each package.
	moduleRoot := DetectModuleRoot(modules)

	var writtenFiles []writtenFile
	for _, mod := range modules {
		locs, written, modWarnings, err := emitModule(db, &mod, outDir, moduleRoot, opts.AllowedRemovals, opts.AllowedAdds, touchedSet)
		if err != nil {
			return nil, nil, fmt.Errorf("emit %s: %w", mod.Path, err)
		}
		allLocs = append(allLocs, locs...)
		writtenFiles = append(writtenFiles, written...)
		warnings = append(warnings, modWarnings...)
	}
	timeIt("module-writes", t)

	// Run goimports to fix unused imports and formatting -- unless the
	// caller has proven this edit can't need it (Opts.SkipGoimports).
	// goimports is a subprocess spawn; measured at ~25% of a warm
	// sig-stable edit's wall even when it had nothing to fix (2026-08-04
	// measure-edit spike on receiverName).
	t = time.Now()
	if opts.SkipGoimports {
		timeIt("goimports-skipped", t)
	} else {
		goimports, err := exec.LookPath("goimports")
		if err != nil {
			return nil, nil, fmt.Errorf("goimports not found — install with: go install golang.org/x/tools/cmd/goimports@latest")
		}
		// #109 pass 3: prefer scoped goimports when the caller has named
		// touched files. Falls back to the full recursive walk when the
		// list is empty (existing behavior).
		args := []string{"-w"}
		if len(opts.GoimportsFiles) > 0 {
			for _, rel := range opts.GoimportsFiles {
				// Sanitize as we do for project files above — no absolute paths,
				// no traversal.
				clean := filepath.Clean(rel)
				if filepath.IsAbs(clean) || strings.Contains(clean, "..") {
					continue
				}
				// #117 followup: emit's output path for a source_file can diverge
				// from the source_file's project-relative path (single-module
				// projects where the module root == package root strip prefixes;
				// cli/cli's "command/root.go" writes to outDir/root.go, not
				// outDir/command/root.go). goimports does NOT tolerate missing
				// paths — it errors "stat X: no such file". Stat before adding
				// and try a Base-only fallback. If neither exists, silently
				// skip: goimports has nothing to do for a file that isn't there,
				// and correctness is preserved (the file was never written by
				// this emit).
				// #304 followup: GoimportsFiles can be over-broadened by
				// a caller's directory-prefix scoping (handleTestByName
				// matches "pkg/sub" as within scope "pkg", but Go
				// packages don't nest that way) and end up naming a
				// generated file nobody touched this pass. Apply the
				// same generated-file skip as the unscoped branch below,
				// unless the caller explicitly named it as touched.
				relSlash := filepath.ToSlash(clean)
				joined := filepath.Join(outDir, clean)
				if _, err := os.Stat(joined); err == nil {
					if !touchedSet[relSlash] && IsGeneratedFile(joined) {
						continue
					}
					args = append(args, joined)
					continue
				}
				// Fallback: emit may have written the basename at outDir root.
				baseJoined := filepath.Join(outDir, filepath.Base(clean))
				if _, err := os.Stat(baseJoined); err == nil {
					if !touchedSet[relSlash] && IsGeneratedFile(baseJoined) {
						continue
					}
					args = append(args, baseJoined)
				}
			}
		} else {
			// Don't blindly sweep the whole tree: a full/unscoped emit
			// (e.g. handleTestByName's scope-resolution-failed fallback)
			// would otherwise run goimports over every .go file under
			// outDir, including generated files (protobuf, goyacc,
			// stringer output) nothing in this pass actually touched.
			// goimports is happy to "fix" a generated file's own
			// non-canonical import grouping -- that shows up as a
			// spurious diff on a file nobody edited. Skip generated
			// files here unless the caller explicitly named them as
			// touched this pass.
			_ = filepath.WalkDir(outDir, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
					return nil
				}
				if rel, relErr := filepath.Rel(outDir, p); relErr == nil && touchedSet[filepath.ToSlash(rel)] {
					args = append(args, p)
					return nil
				}
				if IsGeneratedFile(p) {
					return nil
				}
				args = append(args, p)
				return nil
			})
		}
		emitDebugf("goimports args: %v", args)
		if out, err := exec.Command(goimports, args...).CombinedOutput(); err != nil {
			return nil, nil, fmt.Errorf("goimports: %s", out)
		}
		timeIt("goimports", t)
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
	t = time.Now()
	for _, wf := range writtenFiles {
		if wf.SourceFile == "" {
			continue
		}
		raw, err := os.ReadFile(wf.Path)
		if err != nil {
			continue
		}
		if err := db.SetFileSource(wf.ModuleID, wf.SourceFile, string(raw)); err != nil {
			return nil, nil, fmt.Errorf("refresh file_sources for %s: %w", wf.SourceFile, err)
		}
	}
	timeIt("refresh-file-sources", t)
	t = time.Now()

	// Rebuild location index after goimports (it may shift line numbers).
	// Skip in scoped mode: MCP mutation callers (rename/edit/delete/apply)
	// never consume the loc index — it's a `defn lint` construct — and
	// walking every module here would defeat the point of file-scoping.
	if scoped {
		timeIt("rebuild-loc-index-skipped", t)
		return nil, warnings, nil
	}
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
	timeIt("rebuild-loc-index", t)

	return allLocs, warnings, nil
}

// writtenFile records an emitted file so its post-goimports bytes can be
// written back to file_sources, keeping the authoritative raw source in
// sync with what's on disk.
type writtenFile struct {
	Path       string
	ModuleID   int64
	SourceFile string // project-relative; empty means don't refresh file_sources
}

func emitModule(db store.Backend, mod *store.Module, outDir, moduleRoot string, allowedRemovals, allowedAdds []string, touchedSet map[string]bool) ([]DefLocation, []writtenFile, []string, error) {
	scoped := len(touchedSet) > 0
	defs, err := db.GetModuleDefinitions(mod.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	// #11: struct fields are indexed as their own "field" kind
	// definitions for Type.Field lookup, but they aren't independent
	// top-level declarations -- a field only exists syntactically
	// inside its struct's braces, and the struct's own Body already
	// contains it as text. Passing a field def into the per-file decl
	// assembly below (writeFile/mergeDeclsIntoSource) would inject its
	// bare Body (e.g. "Port int") as a bogus floating top-level
	// statement and corrupt the emitted file. Exclude before anything
	// downstream sees them.
	fieldFree := defs[:0]
	for _, d := range defs {
		if d.Kind != "field" {
			fieldFree = append(fieldFree, d)
		}
	}
	defs = fieldFree
	if len(defs) == 0 {
		// No definitions -- nothing to write. This used to also delete
		// any previously-emitted file for the module, guessing its name
		// from mod.Name (later: from file_sources). Both guesses turned
		// out unsafe (task #239): a module can reach zero defs without
		// defn ever having correctly captured its content -- e.g. a
		// nested Go module (its own go.mod, like grpc-go's
		// security/advancedtls or test/tools) that the incremental
		// ingest fast path's filesystem walk discovers and ingests
		// under the WRONG (root) module path, only for the following
		// resolve pass to unreliably wipe what it just added. Both the
		// module row and file_sources end up populated regardless of
		// whether the defs stuck, so neither was ever reliable proof
		// defn owns the on-disk file. Never delete here; a module
		// stuck at zero defs is simply not emitted. If a user genuinely
		// wants a file removed after deleting its last definition,
		// that's an `rm`, not something defn should guess at.
		return nil, nil, nil, nil
	}

	// Get imports for this module.
	imports, err := db.GetImports(mod.ID)
	if err != nil {
		return nil, nil, nil, err
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
		return nil, nil, nil, err
	}

	// Group definitions by source file, keyed on the full cleaned
	// project-relative path -- NOT the bare basename. store.Module is
	// one row per Go package (see EnsureModule's callers -- #241's
	// "per go.mod, spanning every package" theory was wrong and was
	// corrected 2026-08-09, docs/lessons-learned.md), but pkgDir below
	// can still collapse to the same directory for two different
	// packages when the module-root-relative path computation does --
	// and keying on basename alone then collapses every same-named
	// file across those packages into one bucket (e.g. cli/cli's
	// pkg/cmd/gist/create/create.go and pkg/cmd/repo/create/create.go
	// both reduce to "create.go"). Only one of the colliding files
	// would then get written -- with the OTHER package's definitions
	// merged into it -- silently corrupting one file and dropping the
	// other. Confirmed via a real cli-2671 head-to-head-go trajectory:
	// editing repo/create's createRun overwrote gist/create's
	// createRun with repo's body and imports. The full path collapses
	// to a bare basename naturally whenever SourceFile itself has no
	// directory component (root-level files, or the empty-SourceFile
	// synthetic-name fallback below), so this doesn't change behavior
	// for genuinely single-directory packages.
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
			file = filepath.ToSlash(filepath.Clean(file))
		}
		byFile[file] = append(byFile[file], d)
	}

	// Deterministic file order. Map iteration order was the surface
	// cause of the package-doc duplication bug: whichever file iterated
	// first got mod.Doc auto-attached, even when another file in the
	// package already carried it via prefix preservation. Sorting also
	// makes emit output stable across runs.
	fileNames := make([]string, 0, len(byFile))
	for f := range byFile {
		fileNames = append(fileNames, f)
	}
	sort.Strings(fileNames)

	// Phase C: pre-fetch the raw sources for this module. When present,
	// writeFile uses them as the authoritative merge base — that's the
	// byte-faithful copy, unaffected by whatever's on disk (which might
	// be stale or never have existed, e.g. fresh `defn emit /tmp/out`).
	// Fetched here, before the scoped filter below (not after) -- the
	// #276 def-less pass at the end of this function needs to influence
	// whether the scoped filter's early-return actually fires. It used
	// to be fetched after that early return, so a module whose ONLY
	// relevant content for this emit was a def-less file_sources entry
	// (a freshly scaffolded file, no defs yet) bailed out before ever
	// reaching the code that would have written it.
	rawMap, _ := db.ListFileSources(mod.ID)

	// Scoped emit filter: keep only files whose canonical project-relative
	// path is in touchedSet. Pick the canonical path from the bucket's
	// first def with a non-empty SourceFile (same invariant as projectRelByFile
	// below — buckets share a SourceFile). Files without any SourceFile
	// (fresh defs) are always kept. If no file in this module matched,
	// return early -- unless a def-less file_sources entry is still
	// relevant to this emit (#276), in which case fall through so the
	// pass at the end of this function gets a chance to run.
	if scoped {
		kept := fileNames[:0]
		for _, file := range fileNames {
			projectRel := ""
			for _, d := range byFile[file] {
				if d.SourceFile != "" {
					projectRel = filepath.ToSlash(filepath.Clean(d.SourceFile))
					break
				}
			}
			keep := projectRel == "" || touchedSet[projectRel]
			if keep {
				reason := "in touchedSet"
				if projectRel == "" {
					reason = "no def in this file has a SourceFile (fresh file-less create fallback)"
				}
				emitDebugf("  KEEP %s/%s (sourceFile=%q): %s", mod.Path, file, projectRel, reason)
				kept = append(kept, file)
			} else {
				emitDebugf("  DROP %s/%s (sourceFile=%q): not in touchedSet", mod.Path, file, projectRel)
			}
		}
		fileNames = kept
		if len(fileNames) == 0 {
			relevantDeflessFile := false
			for sourceFile := range rawMap {
				if touchedSet[filepath.ToSlash(filepath.Clean(sourceFile))] {
					relevantDeflessFile = true
					break
				}
			}
			if !relevantDeflessFile {
				return nil, nil, nil, nil
			}
		}
	}

	// Per-file rawFromDB lookup, cached once for reuse.
	rawByFile := make(map[string][]byte, len(fileNames))
	projectRelByFile := make(map[string]string, len(fileNames))
	for _, file := range fileNames {
		// Invariant: all defs in a byFile bucket share the same
		// SourceFile. Buckets are keyed by basename, and within a
		// single module (one package directory) each basename maps to
		// exactly one project-relative path. So breaking at the first
		// def with a non-empty SourceFile yields the canonical one.
		for _, d := range byFile[file] {
			if d.SourceFile != "" {
				projectRelByFile[file] = d.SourceFile
				if r, ok := rawMap[d.SourceFile]; ok {
					rawByFile[file] = []byte(r)
				}
				break
			}
		}
	}

	// Detect whether mod.Doc is already carried by some file's
	// existing source. If yes, suppress auto-attach for every file —
	// writeFile's prefix preservation keeps the doc where it lives, so
	// attaching elsewhere would duplicate. If no, attach to the
	// alphabetically-first non-test file as a deterministic fallback
	// (so a fresh emit to an empty directory doesn't silently drop it).
	docAlreadyPresent := false
	if mod.Doc != "" {
		for _, file := range fileNames {
			src := rawByFile[file]
			if len(src) == 0 {
				// #120 root fix: read from the same path emit will write to.
				// Use source_file under outDir when it has a directory prefix;
				// pkgDir+basename otherwise (matches the write logic below).
				var readPath string
				projRel := projectRelByFile[file]
				if projRel != "" && filepath.Dir(filepath.Clean(projRel)) != "." {
					readPath = filepath.Join(outDir, projRel)
				} else {
					readPath = filepath.Join(pkgDir, file)
				}
				if data, err := os.ReadFile(readPath); err == nil {
					src = data
				}
			}
			if sourceHasPackageDoc(src, mod.Doc) {
				docAlreadyPresent = true
				break
			}
		}
	}
	docTarget := ""
	// In scoped emit, never attach mod.Doc to a touched file — the doc
	// belongs where it already lives (usually a file NOT in touchedSet),
	// and attaching it to an unrelated touched file would duplicate on
	// full re-emit. A singleton rename/edit never needs to (re)attach doc.
	if mod.Doc != "" && !docAlreadyPresent && !scoped {
		for _, file := range fileNames {
			if !strings.HasSuffix(file, "_test.go") {
				docTarget = file
				break
			}
		}
	}

	var allLocs []DefLocation
	var written []writtenFile
	var warnings []string
	for _, file := range fileNames {
		// #120 root fix: when source_file carries an intra-project
		// directory prefix (e.g. "command/root.go"), use it as the
		// authoritative output location under outDir. Previously we
		// joined the module-derived pkgDir with the basename — that
		// dropped the directory prefix on single-module projects where
		// module.Path == moduleRoot (relPath collapses to "."). Preserve
		// the old pkgDir+basename shortcut when source_file is a bare
		// filename (some tests + legacy ingests store source_file as
		// just "pkg.go" and rely on module.Path to fill in the directory).
		var path string
		projRel := projectRelByFile[file]
		if projRel != "" && filepath.Dir(filepath.Clean(projRel)) != "." {
			path = filepath.Join(outDir, projRel)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return nil, nil, nil, err
			}
		} else {
			path = filepath.Join(pkgDir, file)
		}
		pkgDoc := ""
		if file == docTarget {
			pkgDoc = mod.Doc
		}
		// #208: writeFile/safeWriteGoFile already merge against current
		// disk content safely (never drop on-disk decls) -- this doesn't
		// change that behavior, it just surfaces when it happens. If the
		// DB's last-known raw source for this file no longer matches
		// what's actually on disk, something touched it outside defn
		// (most likely the #205 sentinel bypass without a follow-up
		// code(op:"sync")). Silent before this; now at least visible in
		// the emit log instead of resolving invisibly.
		if raw := rawByFile[file]; len(raw) > 0 {
			if onDisk, rerr := os.ReadFile(path); rerr == nil && !bytes.Equal(onDisk, raw) {
				fmt.Fprintf(os.Stderr, "[emit] disk drift: %s changed on disk since defn last ingested it -- merging against current disk content; run code(op:\"sync\", file:%q) to bring the DB back in sync.\n", path, projectRelByFile[file])
			}
		}
		locs, warning, err := writeFile(path, mod.Name, mod.Path, pkgDoc, imports, byFile[file], rawByFile[file], allowedRemovals, allowedAdds)
		if err != nil {
			return nil, nil, nil, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
		emitDebugf("  WROTE %s (module=%s sourceFile=%q scoped=%v)", path, mod.Path, projectRelByFile[file], scoped)
		allLocs = append(allLocs, locs...)
		written = append(written, writtenFile{
			Path:       path,
			ModuleID:   mod.ID,
			SourceFile: projectRelByFile[file],
		})
	}

	// #276: byFile/fileNames above are built exclusively from
	// Definitions, so a file recorded in file_sources with ZERO
	// matching defs -- most notably handleCreateScaffoldFile's whole
	// reason to exist -- was invisible to every emit call in every
	// scenario: a module with other defs never puts a def-less file
	// into byFile at all, and a module with zero defs total hits the
	// len(defs)==0 bailout above before byFile is even built. Confirmed
	// live: a scaffolded file reported "Scaffolded ... N bytes" success
	// and was never actually written to disk. Handle these separately
	// from the def-driven path above -- there's nothing to AST-merge,
	// just the raw file_sources content to place on disk (safeWriteGoFile's
	// own data-loss check still applies if the target already has
	// on-disk decls this content doesn't account for). Additive only:
	// this can write a new file or refresh an existing one, never
	// delete -- consistent with the len(defs)==0 module bailout's own
	// "file_sources alone is not proof of ownership" reasoning, which
	// only ever applied to DELETION, not to placing content a caller
	// explicitly just asked to be written.
	handledPaths := make(map[string]bool, len(projectRelByFile))
	for _, pr := range projectRelByFile {
		if pr != "" {
			handledPaths[pr] = true
		}
	}
	for sourceFile, raw := range rawMap {
		clean := filepath.ToSlash(filepath.Clean(sourceFile))
		if handledPaths[clean] {
			continue
		}
		if scoped && !touchedSet[clean] {
			continue
		}
		path := filepath.Join(outDir, clean)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, nil, nil, err
		}
		wrote, lost, err := safeWriteGoFile(path, []byte(raw), allowedRemovals)
		if err != nil {
			return nil, nil, nil, err
		}
		if !wrote {
			warnings = append(warnings, fmt.Sprintf("%s: skipped writing a def-less file_sources entry -- would remove %d on-disk declaration(s) not in the database: %v", path, len(lost), lost))
			continue
		}
		emitDebugf("  WROTE %s (module=%s sourceFile=%q scoped=%v, def-less file_sources entry)", path, mod.Path, clean, scoped)
		written = append(written, writtenFile{
			Path:       path,
			ModuleID:   mod.ID,
			SourceFile: clean,
		})
	}

	return allLocs, written, warnings, nil
}

// writeFile merges defs into path (or regenerates it) and writes the
// result. Returns a non-empty warning (2nd value) when some requested
// change could not be safely written to disk -- #218: this used to be
// silent (stderr-only), so a def whose DB row no longer matched its
// on-disk counterpart (stale receiver/name, orphaned id) could report
// success up the stack while the file itself never changed.
func writeFile(path, pkgName, modulePath, pkgDoc string, imports []store.Import, defs []store.Definition, rawFromDB []byte, allowedRemovals, allowedAdds []string) ([]DefLocation, string, error) {
	// Pick the merge base. Prefer disk when it exists and parses: a
	// user's built-in Edit lands on disk before file_sources knows about
	// it (built-in tools bypass defn's sync), so disk is the post-Edit
	// truth. Fall back to file_sources (Phase C) when disk is missing
	// (fresh `defn emit /tmp/out`) or broken — file_sources is then
	// the authoritative byte-faithful copy that survives an empty
	// outDir. If neither parses, existingSrc stays empty and the
	// regenerate path rebuilds from defs alone.
	var existingSrc []byte
	if data, err := os.ReadFile(path); err == nil {
		fset := token.NewFileSet()
		if _, perr := parser.ParseFile(fset, "", data, parser.SkipObjectResolution); perr == nil {
			existingSrc = data
		} else {
			emitDebugf("writeFile %s: on-disk file doesn't parse (%v) -- falling back to file_sources", path, perr)
		}
	}
	if len(existingSrc) == 0 {
		existingSrc = rawFromDB
		emitDebugf("writeFile %s: using file_sources fallback (%d bytes) as merge base", path, len(rawFromDB))
	}

	// AST-merge path: if we have a base file that parses, patch the
	// changed decl bodies into its AST and write the result. Preserves
	// everything defn's schema doesn't represent (package doc, build
	// constraints, per-file imports, init() names, floating comments).
	if len(existingSrc) > 0 {
		if merged, ok, unmatched := mergeDeclsIntoSource(existingSrc, defs, allowedRemovals, allowedAdds); ok {
			emitDebugf("writeFile %s: AST-merge path (defs=%d, unmatched=%d)", path, len(defs), len(unmatched))
			wrote, lost, err := safeWriteGoFile(path, merged, allowedRemovals)
			if err != nil {
				return nil, "", err
			}
			if wrote {
				var warning string
				if len(unmatched) > 0 {
					warning = fmt.Sprintf("%s: %d requested change(s) could not be matched to an on-disk declaration and were NOT written: %v -- the database and disk have diverged for these (stale id, renamed receiver, or edited outside defn); run code(op:\"sync\", file:%q) to refresh, then retry",
						path, len(unmatched), unmatched, path)
					fmt.Fprintf(os.Stderr, "defn: %s\n", warning)
				}
				return buildLocIndex(path, modulePath, defs), warning, nil
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

	// Preserve the byte prefix before `package X` from existingSrc when
	// available: build constraints, file-level doc comments (even when
	// separated from `package X` by blank lines and thus NOT captured
	// as file.Doc by ingest), and any other leading content the schema
	// doesn't represent. Without this, the regenerate path silently
	// drops file-level comments — the AST-merge path is byte-faithful
	// here, but if we're falling through, we'd otherwise lose them.
	prefixWritten := false
	if len(existingSrc) > 0 {
		if idx := packageDeclStart(existingSrc); idx > 0 {
			src.Write(existingSrc[:idx])
			prefixWritten = true
		}
	}
	if !prefixWritten && pkgDoc != "" {
		for line := range strings.SplitSeq(pkgDoc, "\n") {
			src.WriteString("// " + line + "\n")
		}
	}
	src.WriteString(fmt.Sprintf("package %s\n\n", pkgName))

	// Import block. Imports are stored per-module (every file in the
	// package shares the union), but `_ "embed"` is meaningful only in
	// files with a //go:embed directive — emitting it elsewhere
	// injects spurious imports and goimports won't strip blank imports.
	// Special-case embed (the only stdlib blank import tied 1:1 to a
	// directive); other blank imports may genuinely be loaded for side
	// effects in any file and are passed through.
	hasEmbedDirective := false
	for _, d := range defs {
		if strings.Contains(d.Body, "//go:embed") {
			hasEmbedDirective = true
			break
		}
	}
	if !hasEmbedDirective && len(existingSrc) > 0 {
		if bytes.Contains(existingSrc, []byte("//go:embed")) {
			hasEmbedDirective = true
		}
	}
	filtered := make([]store.Import, 0, len(imports))
	for _, imp := range imports {
		if imp.Alias == "_" && imp.ImportedPath == "embed" && !hasEmbedDirective {
			continue
		}
		filtered = append(filtered, imp)
	}
	// #new: this is the regenerate path -- there's no on-disk file to
	// preserve a real per-file import block from, so restrict the
	// per-module union down to what this file's own bodies actually
	// reference (deduped on local name) rather than writing the whole
	// union into every file. See relevantImportsForFile.
	filtered = relevantImportsForFile(filtered, defs)
	if len(filtered) > 0 {
		src.WriteString("import (\n")
		for _, imp := range filtered {
			if imp.Alias != "" {
				src.WriteString(fmt.Sprintf("\t%s %q\n", imp.Alias, imp.ImportedPath))
			} else {
				src.WriteString(fmt.Sprintf("\t%q\n", imp.ImportedPath))
			}
		}
		src.WriteString(")\n\n")
	}

	// Preserve original on-disk declaration order. DB defs come back
	// alphabetical (source_file, kind, name) — that's fine for grouped
	// specs but reorders free-floating decls when there's no AST-merge
	// base to anchor them. Sort by existingSrc byte offset where the
	// def matches an on-disk decl; new defs without a match sort to
	// the end while keeping their current relative order.
	if len(existingSrc) > 0 {
		order := declOrderInSource(existingSrc)
		if len(order) > 0 {
			sort.SliceStable(defs, func(i, j int) bool {
				ki, kj := declKey(defs[i]), declKey(defs[j])
				iPos, iOk := order[ki]
				jPos, jOk := order[kj]
				if iOk && jOk {
					return iPos < jPos
				}
				if iOk {
					return true
				}
				if jOk {
					return false
				}
				return false
			})
		}
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
	emitDebugf("writeFile %s: regenerate path (defs=%d)", path, len(defs))
	formatted, err := format.Source([]byte(src.String()))
	if err != nil {
		// format.Source failed (invalid Go in body) — write raw source.
		// go build will catch syntax errors. This used to be completely
		// silent, even at DEFN_EMIT_DEBUG=1 -- the ONE point in the
		// regenerate path where a concatenation of otherwise-individually-
		// valid def bodies turned out not to parse as a whole file, with
		// no trace of why, deferring the only visible signal to whatever
		// safeWriteGoFile's own re-parse below reports (a real
		// prometheus-12024 trajectory hit exactly this: two edits each
		// reported clean success, then an unrelated later `test` call's
		// own emit failed with a bare "generated content doesn't parse",
		// no indication which def or which combination was responsible).
		emitDebugf("writeFile %s: format.Source failed on regenerated content (%d defs, %d bytes): %v -- writing raw source, relying on safeWriteGoFile's re-parse as the last check", path, len(defs), src.Len(), err)
		formatted = []byte(src.String())
	}
	wrote, lost, err := safeWriteGoFile(path, formatted, allowedRemovals)
	if err != nil {
		return nil, "", err
	}
	if !wrote {
		// The DB's reconstruction would remove top-level declarations that
		// exist on disk (most often init(), ingested under a renamed name,
		// or top-level code the current schema can't represent). Keep the
		// file intact; downstream callers (lint, etc.) that need locations
		// will get an empty slice for this file — safer than corruption.
		warning := fmt.Sprintf("%s: skipped writing — would remove %d on-disk declaration(s) not in the database: %v -- DB was updated but disk was NOT; run code(op:\"sync\", file:%q) to refresh, then retry",
			path, len(lost), lost, path)
		fmt.Fprintf(os.Stderr, "defn: %s\n", warning)
		return nil, warning, nil
	}
	return buildLocIndex(path, modulePath, defs), "", nil
}

// safeWriteGoFile writes content to path only if doing so will not remove
// any top-level named declaration that currently exists on disk. If a
// declaration would be lost, returns wrote=false with the list of lost
// names and no error; the caller is expected to log and move on.
//
// This is a defense against the database's representation being lossier
// than the on-disk source: a roundtrip edit/emit should never silently
// delete user code.
//
// allowedRemovals whitelists names the caller has explicitly acknowledged
// losing (e.g. via code(op:"delete")). Names in this list are filtered
// out of the "lost" set before the safety-net decision; a set containing
// only allowed names is treated as no loss and the write proceeds.
func safeWriteGoFile(path string, content []byte, allowedRemovals []string) (wrote bool, lost []string, err error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil, atomicWriteFile(path, content, 0644)
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
	allowed := make(map[string]bool, len(allowedRemovals))
	for _, n := range allowedRemovals {
		allowed[n] = true
	}
	for _, n := range oldDecls {
		if newSet[n] || allowed[n] {
			continue
		}
		lost = append(lost, n)
	}
	if len(lost) > 0 {
		return false, lost, nil
	}
	return true, nil, atomicWriteFile(path, content, 0644)
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
// unwrapping pointer receivers and generic type params. Delegates to
// astutil.BareReceiverName -- see its doc comment for why this used to
// be an independently-maintained copy.
func recvTypeName(e ast.Expr) string {
	return astutil.BareReceiverName(e)
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
//
// unmatched (only meaningful when ok=true) lists DB def identities that
// were requested (present in defs) but found no matching on-disk decl
// and were NOT in allowedAdds — #218: this is exactly the "an existing
// def silently fails to find its on-disk counterpart" case (stale
// receiver/name in the DB row, e.g. from a resolved-but-stale id), and
// previously these were dropped with no signal at all per the #163
// rationale below. The caller now surfaces this instead of trusting
// that a clean merge (ok=true) means every requested change actually
// landed.
func mergeDeclsIntoSource(existing []byte, defs []store.Definition, allowedRemovals, allowedAdds []string) ([]byte, bool, []string) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", existing, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		emitDebugf("mergeDeclsIntoSource: existing source doesn't parse: %v -- falling back to regenerate", err)
		return nil, false, nil
	}

	// remove holds names (pointer-unwrapped for methods, matching
	// topLevelDeclNames) whose on-disk decl should be spliced out
	// entirely. Used by handleDelete so an intentional deletion isn't
	// preserved by the merge's normal "on-disk-only decls stay" rule.
	remove := make(map[string]bool, len(allowedRemovals))
	for _, n := range allowedRemovals {
		remove[n] = true
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
			wantFuncs[FuncIdentity(d.Name, d.Receiver)] = d.Body
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
	totalWants := len(wantFuncs) + len(wantTypes) +
		len(wantConsts) + len(wantVars) + len(wantGrouped)
	if totalWants == 0 {
		emitDebugf("mergeDeclsIntoSource: defs (n=%d) contained nothing splice-able (no function/method/type/interface/const/var) -- falling back to regenerate", len(defs))
		return nil, false, nil
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
			// Removal takes precedence over replacement. topLevelDeclNames
			// keys methods as "Recv.Name" (pointer unwrapped by
			// recvTypeName), matching what handleDelete passes in.
			removeKey := d.Name.Name
			if recv != "" {
				removeKey = recv + "." + d.Name.Name
			}
			if remove[removeKey] {
				s, e := declRange(d.Pos(), d.End(), d.Doc, true)
				reps = append(reps, replacement{s, e, ""})
				continue
			}
			ident := FuncIdentity(d.Name.Name, recv)
			body, ok := wantFuncs[ident]
			if !ok {
				continue
			}
			s, e := declRange(d.Pos(), d.End(), d.Doc, true)
			reps = append(reps, replacement{s, e, body})
			delete(wantFuncs, ident)
		case *ast.GenDecl:
			// Whole-decl removal via allowedRemovals: only for a genuinely
			// single-spec (ungrouped) GenDecl, where the whole decl IS the
			// one removable unit. A grouped (parenthesized) block's removal
			// is handled per-spec below instead -- using this same
			// firstSpecName shortcut for a grouped block used to either
			// silently no-op (removing a NON-first member matched nothing
			// here, and the per-spec loop below never checked `remove` at
			// all) or, when the target WAS first, delete the entire block
			// including untouched sibling specs that were never authorized
			// for removal (caught by safeWriteGoFile's data-loss check, so
			// it failed safe, but made "delete A" unconditionally refused
			// whenever A shared a group with anything else).
			if !d.Lparen.IsValid() {
				if name := firstSpecName(d); name != "" && remove[name] {
					sp, ep := declRange(d.Pos(), d.End(), d.Doc, true)
					reps = append(reps, replacement{sp, ep, ""})
					continue
				}
			}
			// Whole-decl replacement: ingest bundles iota const blocks
			// (and any future whole-GenDecl case) under the first spec
			// name. Match on that before falling through to per-spec
			// splicing, which would otherwise try to cram the whole
			// parenthesized block into a single spec's byte range.
			if name := firstSpecName(d); name != "" {
				if body, ok := wantGrouped[name]; ok {
					sp, ep := declRange(d.Pos(), d.End(), d.Doc, true)
					reps = append(reps, replacement{sp, ep, body})
					delete(wantGrouped, name)
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
					// Per-spec removal: the whole-decl shortcut above only
					// fires for an ungrouped GenDecl, so a grouped block's
					// member -- first or not -- is removed here, scoped to
					// just its own spec range. Sibling specs are untouched.
					if remove[s.Name.Name] {
						if grouped {
							sp, ep := declRange(s.Pos(), s.End(), nil, false)
							reps = append(reps, replacement{sp, ep, ""})
						} else {
							sp, ep := declRange(d.Pos(), d.End(), d.Doc, true)
							reps = append(reps, replacement{sp, ep, ""})
						}
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
					delete(wantTypes, s.Name.Name)
				case *ast.ValueSpec:
					// Multi-name specs (var a, b = 1, 2; or var x, y T) share
					// a single DB def under the first non-blank name
					// (ingestValueSpec's own storage convention -- Body
					// already holds the WHOLE spec's rendered text, not just
					// one name's value). The splice below always replaces the
					// entire spec's byte range regardless of name count, so
					// there was never a real "partial patching leaks into
					// siblings" risk here -- bailing on len(s.Names)!=1 just
					// silently left the def unmatched forever (no "fall
					// through to regeneration" ever actually happened), which
					// is exactly the prometheus-16766 bug: a multi-name var
					// spec (agentOnlyFlags, serverOnlyFlags []string) blocked
					// every subsequent edit to ANY OTHER decl in the same
					// file, forever, even after a fresh sync.
					name := firstNonBlankValueSpecName(s.Names)
					if name == "" {
						continue
					}
					// Per-spec removal -- same rationale as the TypeSpec case
					// above.
					if remove[name] {
						if grouped {
							sp, ep := declRange(s.Pos(), s.End(), nil, false)
							reps = append(reps, replacement{sp, ep, ""})
						} else {
							sp, ep := declRange(d.Pos(), d.End(), d.Doc, true)
							reps = append(reps, replacement{sp, ep, ""})
						}
						continue
					}
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
					switch d.Tok {
					case token.CONST:
						delete(wantConsts, name)
					case token.VAR:
						delete(wantVars, name)
					}
				}
			}
		}
	}

	if len(reps) == 0 {
		emitDebugf("mergeDeclsIntoSource: none of %d wanted def(s) matched any on-disk decl -- falling back to regenerate", totalWants)
		return nil, false, nil
	}

	// Count only body replacements (not removals) when checking that
	// every DB def matched an on-disk decl; a removal doesn't contribute
	// to that coverage.
	nonRemovalReps := 0
	for _, r := range reps {
		if r.body != "" {
			nonRemovalReps++
		}
	}

	// #162 fix: when the DB has NEW defs with no on-disk counterpart,
	// AND the caller explicitly declared them via Opts.AllowedAdds,
	// append their bodies at end of file instead of falling through to
	// full regeneration. Regen rebuilds from defs alone and drops
	// floating (blank-line-separated) comments between top-level decls;
	// the append path leaves the interior byte layout untouched so
	// those comments survive.
	//
	// The AllowedAdds gate mirrors AllowedRemovals — only whitelisted
	// names count as intentional adds. Un-whitelisted unmatched wants
	// are drift signals (external file edit, stale DB) and still fall
	// through to regen + safeWriteGoFile's data-loss check.
	allowAdd := make(map[string]bool, len(allowedAdds))
	for _, n := range allowedAdds {
		allowAdd[n] = true
	}
	var appendBodies []string
	for name, body := range wantFuncs {
		if allowAdd[name] {
			appendBodies = append(appendBodies, body)
			delete(wantFuncs, name)
		}
	}
	for name, body := range wantTypes {
		if allowAdd[name] {
			appendBodies = append(appendBodies, body)
			delete(wantTypes, name)
		}
	}
	for name, body := range wantConsts {
		if allowAdd[name] {
			appendBodies = append(appendBodies, body)
			delete(wantConsts, name)
		}
	}
	for name, body := range wantVars {
		if allowAdd[name] {
			appendBodies = append(appendBodies, body)
			delete(wantVars, name)
		}
	}
	for name, body := range wantGrouped {
		if allowAdd[name] {
			appendBodies = append(appendBodies, body)
			delete(wantGrouped, name)
		}
	}
	// Deterministic order so successive emits are stable.
	sort.Strings(appendBodies)

	// #163 fix: unmatched wants not in AllowedAdds are drift the caller
	// didn't declare intent for. Skip them silently for write purposes —
	// the disk file stays consistent with what the caller ASKED for
	// (patched existing decls, appended allowed adds), and the orphan DB
	// entries neither land on disk nor cause the merge to fall through to
	// regen (which would drop floating comments the merge path
	// preserves). The real data-loss safety net is safeWriteGoFile, which
	// still runs after this returns and refuses any write that would
	// silently drop an on-disk decl the DB doesn't know about.
	//
	// #218: "skip silently" only ever meant silent to the FILE CONTENTS.
	// Collect what's left in the want maps here and hand it back to the
	// caller so it can be silent to disk but NOT silent to whoever asked
	// for the change — an edit whose target def never matched an on-disk
	// decl is exactly the "stale id" class of bug that made a def-update
	// vanish with no signal.
	var unmatched []string
	for name := range wantFuncs {
		unmatched = append(unmatched, name)
	}
	for name := range wantTypes {
		unmatched = append(unmatched, name)
	}
	for name := range wantConsts {
		unmatched = append(unmatched, name)
	}
	for name := range wantVars {
		unmatched = append(unmatched, name)
	}
	for name := range wantGrouped {
		unmatched = append(unmatched, name)
	}
	sort.Strings(unmatched)

	// Apply in reverse offset order so earlier splices don't invalidate
	// later offsets. Byte ranges for distinct decls never overlap (Go
	// syntax forbids it), so ordering by start offset is total --
	// UNLESS `defs` itself contains a duplicate entry for the same
	// on-disk decl (e.g. a stale/duplicate DB row surfacing at a
	// broader multi-def emit scope that a narrower single-def emit
	// would never hit): two reps entries would then share the same
	// original-coordinate start/end, and applying both sequentially
	// against a buffer that already shifted after the first splice
	// reads/writes the WRONG bytes the second time -- a real,
	// unconfirmed hypothesis for a prometheus-12024 mystery (see
	// docs/lessons-learned.md). Trace it explicitly rather than let it
	// silently corrupt: this is exactly the kind of bug the final parse
	// validation below is the last line of defense against, but by then
	// the specific cause is lost.
	for i := 1; i < len(reps); i++ {
		if reps[i].start == reps[i-1].start && reps[i].end == reps[i-1].end {
			emitDebugf("mergeDeclsIntoSource: DUPLICATE replacement at byte range [%d,%d) -- defs likely contains two rows for the same on-disk decl; applying both against a shifting buffer can corrupt content", reps[i].start, reps[i].end)
		}
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].start > reps[j].start })
	result := append([]byte{}, existing...)
	for _, r := range reps {
		if r.start < 0 || r.end > len(result) || r.start > r.end {
			emitDebugf("mergeDeclsIntoSource: invalid byte range [%d,%d) against a %d-byte buffer -- falling back to regenerate", r.start, r.end, len(result))
			return nil, false, nil
		}
		var buf bytes.Buffer
		buf.Grow(len(result) - (r.end - r.start) + len(r.body))
		buf.Write(result[:r.start])
		buf.WriteString(r.body)
		buf.Write(result[r.end:])
		result = buf.Bytes()
	}

	// Append new-def bodies at end of file. Each body already ends
	// with the trailing newline that renderNode emits; sandwich a
	// blank line between existing content and the appends so they
	// don't collide with the last on-disk decl's trailing comment.
	if len(appendBodies) > 0 {
		var tail bytes.Buffer
		if len(result) > 0 && result[len(result)-1] != '\n' {
			tail.WriteByte('\n')
		}
		for _, body := range appendBodies {
			tail.WriteByte('\n')
			tail.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				tail.WriteByte('\n')
			}
		}
		result = append(result, tail.Bytes()...)
	}

	// Validate the spliced result parses. DB bodies are trusted, but a
	// corrupted body or an off-by-one offset should fail safe rather
	// than write invalid Go to disk.
	if _, err := parser.ParseFile(token.NewFileSet(), "", result,
		parser.ParseComments|parser.SkipObjectResolution); err != nil {
		emitDebugf("mergeDeclsIntoSource: spliced result (%d reps, %d bytes) doesn't parse: %v -- falling back to regenerate", len(reps), len(result), err)
		return nil, false, nil
	}
	return result, true, unmatched
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

// sourceHasPackageDoc reports whether src carries the given package
// doc as the comment bound to its `package X` clause. Used by
// emitModule to decide whether mod.Doc is already present in some
// file in the package (preserved via merge or on-disk prefix) — if
// so, auto-attach is suppressed to avoid duplicating across files.
//
// Authoritative: parses src and compares file.Doc.Text() to the
// stored doc using the same primitive ingest used to populate
// mod.Doc, so format quirks (spacing on `//` blank lines, trailing
// whitespace) can't desync the check.
func sourceHasPackageDoc(src []byte, doc string) bool {
	if len(src) == 0 || doc == "" {
		return false
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ImportsOnly|parser.ParseComments)
	if err != nil || f.Doc == nil {
		return false
	}
	return strings.TrimSpace(f.Doc.Text()) == strings.TrimSpace(doc)
}

// packageDeclStart returns the byte offset of the `package X` keyword
// in src, or -1 if the source doesn't parse far enough. Callers use
// this to extract the byte prefix before the package clause —
// preserving build constraints, leading doc comments (whether bound
// to `package X` or separated by blank lines), and any other content
// the regenerate path would otherwise drop.
func packageDeclStart(src []byte) int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.PackageClauseOnly)
	if err != nil {
		return -1
	}
	return fset.Position(f.Package).Offset
}

// declOrderInSource returns a key → byte-offset map for every
// top-level declaration in src. Keys match declKey's output so the
// regenerate path can sort DB defs into the on-disk order. Returns
// an empty map when src doesn't parse — treat absence as "no order
// info available, leave as-is".
func declOrderInSource(src []byte) map[string]int {
	order := make(map[string]int)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return order
	}
	for _, decl := range f.Decls {
		pos := fset.Position(decl.Pos()).Offset
		switch d := decl.(type) {
		case *ast.FuncDecl:
			recv := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv = recvTypeName(d.Recv.List[0].Type)
			}
			order[FuncIdentity(d.Name.Name, recv)] = pos
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					order[s.Name.Name] = pos
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name != "_" {
							order[n.Name] = pos
						}
					}
				}
			}
		}
	}
	return order
}

// declKey returns the identifier for a Definition matching the keys
// declOrderInSource produces. Funcs/methods use funcIdentity (which
// encodes the receiver); everything else uses the bare name.
func declKey(d store.Definition) string {
	if d.Kind == "function" || d.Kind == "method" {
		return FuncIdentity(d.Name, d.Receiver)
	}
	return d.Name
}

// funcIdentity produces the identity key used to match DB definitions
// to AST FuncDecls. Free functions and methods share the same space:
// "Foo" for a free function, "*Server.Foo" for a pointer-receiver method.
func FuncIdentity(name, receiver string) string {
	if receiver == "" {
		return name
	}
	return receiver + "." + name
}

// detectModuleRoot finds the common module root from the stored module paths.
// For a Go project, this is the go.mod module path (e.g., "github.com/justinstimatze/defn").
// We detect it by finding the longest common prefix of all module paths that
// ends at a "/" boundary, then stripping one more component if the prefix
// itself is a stored module.
func DetectModuleRoot(modules []store.Module) string {
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

// relevantImportsForFile filters imports down to the subset a single
// file's own definition bodies actually reference, deduping on local
// name so two same-name-but-different-path imports (e.g. several AWS
// SDK "types" sub-packages sharing a package's per-module import
// union) never both land in one file's import block -- see writeFile's
// regenerate path, which has no on-disk file to preserve a real
// per-file import block from and would otherwise write the FULL
// per-module union into every file, including imports that file never
// uses or that collide with each other on local name ("redeclared",
// unrecoverable) rather than merely going unused ("undefined: X",
// actionable).
func relevantImportsForFile(imports []store.Import, defs []store.Definition) []store.Import {
	referenced := func(name string) bool {
		for _, d := range defs {
			if strings.Contains(d.Body, name+".") {
				return true
			}
		}
		return false
	}
	localName := func(imp store.Import) string {
		if imp.Alias != "" {
			return imp.Alias
		}
		return path.Base(imp.ImportedPath)
	}
	seen := map[string]bool{}
	var out []store.Import
	for _, imp := range imports {
		ln := localName(imp)
		if ln == "_" || ln == "." {
			out = append(out, imp)
			continue
		}
		if seen[ln] {
			continue
		}
		if referenced(ln) {
			seen[ln] = true
			out = append(out, imp)
		}
	}
	return out
}

// emitDebugf prints a trace line to stderr when DEFN_EMIT_DEBUG=1,
// prefixed for easy grepping. No-op (and effectively free -- one env
// lookup) otherwise. Companion to the existing DEFN_SYNC_TIMING /
// DEFN_MEASURE_TIMING dev-instrumentation env vars; re-checks the env
// var on every call rather than caching it at package init so tests
// can toggle it per-case via t.Setenv and a long-lived `defn serve`
// process doesn't need a restart to pick up the flag.
//
// Built to trace a real, unresolved mystery (2026-08-17): a scoped
// emit during a real etcd bench trajectory rewrote three unrelated
// generated .pb.gw.go files' import grouping even after every known
// unscoped-emit call site had been found and fixed. Static reading of
// emitWithOpts/emitModule's ~450 combined lines couldn't pin down
// which of several plausible branches was responsible. This gives
// live per-file keep/drop reasoning and the actual goimports
// invocation for the next time that (or a similar) mystery shows up --
// meant to stay in the tree as a permanent, reusable dev tool, not a
// one-off diagnostic ripped out after use.
//
// Usage: DEFN_EMIT_DEBUG=1 defn <ingest|serve|...> 2>&1 | grep emit-debug
func emitDebugf(format string, args ...any) {
	if os.Getenv("DEFN_EMIT_DEBUG") != "1" {
		return
	}
	fmt.Fprintf(os.Stderr, "[emit-debug] "+format+"\n", args...)
}

// firstNonBlankValueSpecName returns the first non-"_" name in a
// ValueSpec's Names list, or "" if all are blank. Mirrors
// ingestValueSpec's own "first non-blank name owns the spec"
// convention (internal/ingest/ingest.go) -- a multi-name spec is
// matched and replaced as a whole under this same key.
func firstNonBlankValueSpecName(names []*ast.Ident) string {
	for _, n := range names {
		if n.Name != "_" {
			return n.Name
		}
	}
	return ""
}

// atomicWriteFile writes content to path atomically via a temp file in
// the same directory followed by os.Rename, instead of the plain
// open+O_TRUNC+write+close os.WriteFile does. #311: two MCP tool calls
// issued in parallel by an agent in one turn (op:"test" scoped to the
// same package unconditionally re-emits its files before running `go
// test`) can both reach a write for the SAME path at nearly the same
// time. Confirmed live: a real prometheus-19017 trajectory hit exactly
// this -- the on-disk file ended up missing a function's closing brace,
// and the DB's stored body matched the truncated disk content after a
// later sync. os.Rename within a single filesystem is atomic on every
// platform Go supports (POSIX rename(2); Windows MoveFileEx with the
// replace flag) -- a concurrent reader/writer either sees the old file
// or the new one in full, never a partial mix of both, closing the
// exact interleaving window that produced the corruption. The temp file
// MUST live in the same directory as path -- a cross-directory (and so,
// often, cross-filesystem) rename is not guaranteed atomic and can fail
// outright with EXDEV.
func atomicWriteFile(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	succeeded := false
	defer func() {
		if !succeeded {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	succeeded = true
	return nil
}
