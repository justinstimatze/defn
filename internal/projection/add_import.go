// Package projection implements defn's projection-level edit vocabulary:
// small, mechanically-verifiable source-code edit primitives whose `put`
// side (edit application) satisfies a byte-exact or quotient-lens PUTGET
// contract against the `get` side (projection read).
//
// Each operator is a pure function over a definition body string. The
// wiring into the MCP layer lives in internal/mcp/server.go; the pure
// functions live here so their PUTGET goldens can be tested without any
// DB or MCP dependencies.
//
// See project_putget_edit_vocab_design and project_projection_phase_c_next
// memory for the design contract and phase plan.
package projection

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// AddImport adds an import path (with optional alias) to a file source's
// import block, alphabetized within its group per astutil semantics.
//
// Import grouping: goimports-canonical stdlib-vs-thirdparty split. After
// astutil.AddImport runs, regroupImports rewrites the block to place all
// stdlib imports (no dot in first path segment) first, then a blank line,
// then all third-party imports. Homogeneous blocks (all stdlib or all
// third-party) are left as-is.
//
// Byte-exact goimports-canonical output for the block; the rest of the
// file is untouched. Two output files that agree on their import set
// under this grouping rule are byte-identical.
//
// Idempotency: if the same (path, alias) is already imported, the input
// file source is returned unchanged (byte-exact no-op).
//
// v1 limitation held over: local imports (matching the current module
// prefix) are not distinguished from third-party. Import Doc comments
// and trailing per-line comments are not preserved by the regroup step.
func AddImport(fileSrc, path, alias string) (string, error) {
	if fileSrc == "" {
		return "", fmt.Errorf("add-import: file source is empty")
	}
	if path == "" {
		return "", fmt.Errorf("add-import: path is required")
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", fileSrc, parser.ParseComments)
	if err != nil {
		return "", fmt.Errorf("add-import: parse: %w", err)
	}
	var added bool
	if alias != "" {
		added = astutil.AddNamedImport(fset, f, alias, path)
	} else {
		added = astutil.AddImport(fset, f, path)
	}
	if !added {
		return fileSrc, nil
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return "", fmt.Errorf("add-import: format: %w", err)
	}
	regrouped, err := regroupImports(buf.String())
	if err != nil {
		return "", fmt.Errorf("add-import: regroup: %w", err)
	}
	return regrouped, nil
}

// regroupImports rewrites the file's import block to put stdlib
// imports first, then a blank line, then third-party. Homogeneous
// blocks (all stdlib or all third-party) are left untouched.
//
// Classification: an import path is stdlib iff its first path segment
// contains no dot (matches goimports' heuristic — "fmt", "encoding/json"
// are stdlib; "github.com/pkg/errors", "golang.org/x/tools" are not).
//
// Import Doc comments and trailing per-line comments are not preserved
// by the regroup step (documented v1 limitation held over from add-import).
func regroupImports(src string) (string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return src, fmt.Errorf("parse for regroup: %w", err)
	}
	var importDecl *ast.GenDecl
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			importDecl = gd
			break
		}
	}
	if importDecl == nil || len(importDecl.Specs) < 2 {
		return src, nil
	}
	var std, third []string
	for _, sp := range importDecl.Specs {
		is, ok := sp.(*ast.ImportSpec)
		if !ok {
			return src, nil
		}
		pathStr := strings.Trim(is.Path.Value, "\"")
		firstSeg := strings.SplitN(pathStr, "/", 2)[0]
		line := "\t" + is.Path.Value
		if is.Name != nil {
			line = "\t" + is.Name.Name + " " + is.Path.Value
		}
		if strings.Contains(firstSeg, ".") {
			third = append(third, line)
		} else {
			std = append(std, line)
		}
	}
	if len(std) == 0 || len(third) == 0 {
		return src, nil
	}
	sort.Strings(std)
	sort.Strings(third)

	var block strings.Builder
	block.WriteString("import (\n")
	for _, l := range std {
		block.WriteString(l + "\n")
	}
	block.WriteString("\n")
	for _, l := range third {
		block.WriteString(l + "\n")
	}
	block.WriteString(")")

	startOff := fset.Position(importDecl.Pos()).Offset
	endOff := fset.Position(importDecl.End()).Offset
	return src[:startOff] + block.String() + src[endOff:], nil
}
