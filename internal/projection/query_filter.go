package projection

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"unicode"
)

// FilterBodyByQuery returns a rewritten function body that keeps only
// top-level statements whose source contains any token from `query`.
// Elided statements are replaced by a single comment placeholder so the
// remaining shape is visually coherent. Task #153: query-adaptive read.
//
// Contract:
//   - Bodies with 0-1 top-level statements return unchanged (nothing to
//     elide).
//   - Query with < 2 usable tokens returns the body unchanged.
//   - Unparseable body or non-FuncDecl returns unchanged.
//   - If ALL statements match, returns unchanged (elision would be a
//     no-op wasting an "elided" comment).
//   - Case-insensitive substring match. Tokens shorter than 2 chars
//     dropped (avoid trivial-hit noise).
//
// Returns (rewritten, kept, elided). Callers can size-adapt: if
// elided==0, the projection is a no-op — fall back to full body.
func FilterBodyByQuery(body, query string) (rewritten string, kept, elided int) {
	tokens := extractQueryTokens(query)
	if len(tokens) == 0 {
		return body, 0, 0
	}
	src := "package x\n" + body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil || len(f.Decls) == 0 {
		return body, 0, 0
	}
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok || fn.Body == nil || len(fn.Body.List) < 2 {
		return body, 0, 0
	}

	lines := strings.Split(body, "\n")
	// AST positions are in the "package x\n"+body namespace: subtract
	// 1 for the package prefix line, then 1 more to convert to 0-index.
	stmtLine := func(pos token.Pos) int {
		return fset.Position(pos).Line - 2
	}

	type span struct {
		startLine, endLine int
		keep               bool
	}
	spans := make([]span, len(fn.Body.List))
	firstStart := stmtLine(fn.Body.List[0].Pos())
	if firstStart < 0 {
		firstStart = 0
	}
	for i, stmt := range fn.Body.List {
		s := stmtLine(stmt.Pos())
		e := stmtLine(stmt.End())
		if s < 0 {
			s = 0
		}
		if e >= len(lines) {
			e = len(lines) - 1
		}
		text := strings.Join(lines[s:e+1], "\n")
		spans[i] = span{s, e, matchesAny(text, tokens)}
		if spans[i].keep {
			kept++
		} else {
			elided++
		}
	}
	// No-op cases: either everything matches (nothing to elide) or
	// nothing matches (fall back to full body rather than return a
	// body containing only elision stubs). Return kept=elided=0 so
	// the caller uniformly detects "no filter applied."
	if elided == 0 || kept == 0 {
		return body, 0, 0
	}

	var out []string
	// Header: everything up to the first statement's start line
	// (signature + opening brace + any leading comments).
	out = append(out, lines[:firstStart]...)
	prevEnd := -1
	elisionEmitted := false
	for i, sp := range spans {
		if !sp.keep {
			// Emit ONE placeholder for a run of elided stmts.
			if !elisionEmitted || (i > 0 && spans[i-1].keep) {
				out = append(out, elisionPlaceholder(query))
				elisionEmitted = true
			}
			continue
		}
		// Emit gap between prev kept and this stmt only when the
		// previous was also kept (preserves inter-stmt blank lines).
		if prevEnd >= 0 && i > 0 && spans[i-1].keep {
			out = append(out, lines[prevEnd+1:sp.startLine]...)
		}
		out = append(out, lines[sp.startLine:sp.endLine+1]...)
		prevEnd = sp.endLine
		elisionEmitted = false
	}
	// Trailer: closing brace + anything after the last stmt.
	lastEnd := spans[len(spans)-1].endLine
	if lastEnd+1 <= len(lines)-1 {
		out = append(out, lines[lastEnd+1:]...)
	}
	return strings.Join(out, "\n"), kept, elided
}

func elisionPlaceholder(query string) string {
	return fmt.Sprintf("\t// … statements not matching query %q elided …", query)
}

// extractQueryTokens splits a free-form query into a slug of case-
// folded ≥2-char tokens. Non-identifier chars are separators.
func extractQueryTokens(query string) []string {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	folded := strings.ToLower(query)
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 2 {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	for _, r := range folded {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// matchesAny is a case-insensitive OR-match: returns true if any
// token appears in text (as a substring).
func matchesAny(text string, tokens []string) bool {
	low := strings.ToLower(text)
	for _, t := range tokens {
		if strings.Contains(low, t) {
			return true
		}
	}
	return false
}
