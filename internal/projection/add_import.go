package projection

import (
	"bytes"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"

	"golang.org/x/tools/go/ast/astutil"
)

// AddImport adds an import path (with optional alias) to a file source's
// import block, alphabetized within its group per astutil semantics.
//
// Quotient equivalence: ≡_import_order — the import block is regenerated
// via go/format, so any pre-existing alignment is normalized. Two output
// files are equivalent if their per-group import sets match.
//
// Idempotency: if the same (path, alias) is already imported, the input
// file source is returned unchanged (byte-exact no-op).
//
// v1 limitation: astutil groups by presence of "." in the path — stdlib
// vs. non-stdlib. Local imports (matching the current module prefix) are
// not distinguished from third-party in v1.
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
	return buf.String(), nil
}
