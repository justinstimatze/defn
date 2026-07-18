// ingest-upstream ingests a checked-out Go module's exported definitions,
// computes their structural fingerprints, and writes them to the current
// project's upstream_fingerprints table. Used to seed the delta-from-prior
// corpus so read ops can return a compact provenance tag instead of the
// full body when a local dep matches upstream unchanged.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
)

// cmdIngestUpstream: `defn ingest-upstream <path> --module <M> --version <V>`
//
// Reads Go source files under <path> (a checked-out copy of the module at
// the given version tag), extracts each package-level declaration, computes
// HashBodyStructural on its canonicalized body, and inserts into the current
// project's upstream_fingerprints table.
//
// Only *exported* defs are recorded — a delta-from-prior tag is only useful
// when the agent might reasonably ask about the symbol, and non-exported
// helpers aren't public API. This also keeps the corpus size sane.
//
// Test files (_test.go) are skipped for the same reason.
func cmdIngestUpstream(args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: defn ingest-upstream <path> --module <module-path> --version <version>"))
	}
	srcPath := args[0]
	var modulePath, version string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--module":
			if i+1 >= len(args) {
				fatal(fmt.Errorf("--module requires an argument"))
			}
			modulePath = args[i+1]
			i++
		case "--version":
			if i+1 >= len(args) {
				fatal(fmt.Errorf("--version requires an argument"))
			}
			version = args[i+1]
			i++
		default:
			fatal(fmt.Errorf("unknown argument: %s", args[i]))
		}
	}
	if modulePath == "" || version == "" {
		fatal(fmt.Errorf("--module and --version are required"))
	}

	abs, err := filepath.Abs(srcPath)
	if err != nil {
		fatal(err)
	}
	srcPath = abs

	// Open the current project's DB (not the module's — we're writing
	// upstream fingerprints INTO the caller's project).
	dbPath := getDBPath()
	db, err := store.Open(dbPath)
	if err != nil {
		fatal(fmt.Errorf("open db: %w", err))
	}
	defer db.Close()

	rows, err := scanUpstreamModule(srcPath, modulePath, version)
	if err != nil {
		fatal(err)
	}
	if len(rows) == 0 {
		fmt.Fprintf(os.Stderr, "no exported definitions found under %s\n", srcPath)
		return
	}

	// Batch in chunks to keep the INSERT statement size reasonable.
	const batchSize = 200
	for i := 0; i < len(rows); i += batchSize {
		end := i + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		if err := db.InsertUpstreamFingerprints(rows[i:end]); err != nil {
			fatal(fmt.Errorf("insert batch [%d:%d]: %w", i, end, err))
		}
	}
	total, _ := db.CountUpstreamFingerprints()
	fmt.Fprintf(os.Stderr, "ingested %d defs from %s@%s (corpus total: %d rows)\n",
		len(rows), modulePath, version, total)
}

// scanUpstreamModule walks srcPath, parses every non-test Go file, and returns
// UpstreamFingerprint rows for exported top-level declarations.
func scanUpstreamModule(srcPath, modulePath, version string) ([]store.UpstreamFingerprint, error) {
	var out []store.UpstreamFingerprint
	err := filepath.Walk(srcPath, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			// Skip common non-source directories.
			name := info.Name()
			if name == "testdata" || name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rows, err := scanUpstreamFile(path, modulePath, version)
		if err != nil {
			// One bad file shouldn't abort the whole corpus. Log and continue.
			fmt.Fprintf(os.Stderr, "warn: skipping %s: %v\n", path, err)
			return nil
		}
		out = append(out, rows...)
		return nil
	})
	return out, err
}

// scanUpstreamFile parses one .go file and returns fingerprints for its
// exported top-level declarations (functions, methods, types, constants,
// variables).
func scanUpstreamFile(path, modulePath, version string) ([]store.UpstreamFingerprint, error) {
	fset := token.NewFileSet()
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	var out []store.UpstreamFingerprint
	for _, decl := range f.Decls {
		rows := extractUpstreamDecl(decl, fset, modulePath, version)
		out = append(out, rows...)
	}
	return out, nil
}

// extractUpstreamDecl produces one UpstreamFingerprint per exported top-level
// declaration in decl. A GenDecl can contain multiple specs (e.g. `type ( A struct{...}; B interface{...} )`)
// — each exported spec yields one row.
func extractUpstreamDecl(decl ast.Decl, fset *token.FileSet, modulePath, version string) []store.UpstreamFingerprint {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if !d.Name.IsExported() {
			return nil
		}
		name, kind, receiver := funcNameKindReceiver(d)
		body := printNode(fset, d)
		return []store.UpstreamFingerprint{{
			ModulePath:  modulePath,
			Version:     version,
			DefName:     name,
			Kind:        kind,
			Receiver:    receiver,
			Fingerprint: store.HashBodyStructural(body),
			Signature:   extractSignature(fset, d),
			Doc:         d.Doc.Text(),
		}}
	case *ast.GenDecl:
		var out []store.UpstreamFingerprint
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				if !s.Name.IsExported() {
					continue
				}
				kind := "type"
				if _, ok := s.Type.(*ast.InterfaceType); ok {
					kind = "interface"
				}
				body := printNode(fset, s)
				out = append(out, store.UpstreamFingerprint{
					ModulePath:  modulePath,
					Version:     version,
					DefName:     s.Name.Name,
					Kind:        kind,
					Fingerprint: store.HashBodyStructural(body),
					Signature:   fmt.Sprintf("type %s", s.Name.Name),
					Doc:         s.Doc.Text(),
				})
			case *ast.ValueSpec:
				kind := "var"
				if d.Tok == token.CONST {
					kind = "const"
				}
				for _, n := range s.Names {
					if !n.IsExported() {
						continue
					}
					body := printNode(fset, s)
					out = append(out, store.UpstreamFingerprint{
						ModulePath:  modulePath,
						Version:     version,
						DefName:     n.Name,
						Kind:        kind,
						Fingerprint: store.HashBodyStructural(body),
						Doc:         s.Doc.Text(),
					})
				}
			}
		}
		return out
	}
	return nil
}

// funcNameKindReceiver returns the fully-qualified name (e.g. "Mux.ServeHTTP"),
// the kind ("function" or "method"), and the receiver type ("*Mux", or "" for
// plain functions).
func funcNameKindReceiver(d *ast.FuncDecl) (name, kind, receiver string) {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name, "function", ""
	}
	recv := ""
	switch t := d.Recv.List[0].Type.(type) {
	case *ast.Ident:
		recv = t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			recv = "*" + id.Name
		}
	}
	recvBase := strings.TrimPrefix(recv, "*")
	return recvBase + "." + d.Name.Name, "method", recv
}

// printNode emits a canonical string form of node (whitespace-preserving),
// suitable for feeding to HashBodyStructural.
func printNode(fset *token.FileSet, node any) string {
	var sb strings.Builder
	cfg := printer.Config{Mode: printer.UseSpaces, Tabwidth: 0}
	_ = cfg.Fprint(&sb, fset, node)
	return sb.String()
}

// extractSignature returns just the signature line of a function/method,
// stripping the body AND any leading doc comments. Used for the compact
// provenance form — the doc is a separate field.
func extractSignature(fset *token.FileSet, d *ast.FuncDecl) string {
	// Temporarily null out the Doc so the printer doesn't emit it inline.
	doc := d.Doc
	d.Doc = nil
	full := printNode(fset, d)
	d.Doc = doc
	if idx := strings.Index(full, "{"); idx >= 0 {
		return strings.TrimSpace(full[:idx])
	}
	return strings.TrimSpace(full)
}
