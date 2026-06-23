// Package normalize provides symbol name canonicalization for cross-system comparison.
// Different systems produce different formats for the same symbol. This package
// normalizes them to a common form for ground-truth matching.
package normalize

import (
	"strings"
)

// Symbol canonicalizes a qualified symbol name for comparison.
//
// Input formats handled:
//   - knowing: "github.com/org/repo://path/to/file.py.ClassName.method"
//   - GitNexus: "pkg/FuncName" or "pkg.FuncName"
//   - Aider: "pkg/file.go:FuncName"
//   - SCIP: "github.com/org/repo/pkg.FuncName."
//   - grep: "file.go:42:func FuncName("
//   - Python-style: "flask.app.Flask.before_request"
//
// Output: the meaningful symbol identifier stripped of file paths and repo URLs.
func Symbol(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// Strip trailing dots (SCIP format)
	s = strings.TrimRight(s, ".")

	// Strip trailing parens (grep captures "func Foo(")
	s = strings.TrimRight(s, "(")

	// Handle grep format: "file.go:42:func FuncName"
	if parts := strings.SplitN(s, ":", 3); len(parts) == 3 && looksLikeFile(parts[0]) {
		content := strings.TrimSpace(parts[2])
		content = stripDeclKeyword(content)
		if idx := strings.IndexAny(content, "( {[<"); idx > 0 {
			content = content[:idx]
		}
		return content
	}

	// Handle "file.go:SymbolName" format (2-part, file:symbol)
	if parts := strings.SplitN(s, ":", 2); len(parts) == 2 && looksLikeFile(parts[0]) {
		if isNumeric(parts[1]) {
			return "" // file:line with no symbol info
		}
		return strings.TrimSpace(parts[1])
	}

	// Handle knowing format: "repoURL://filepath.Symbol.Name"
	// Strip everything before and including "://"
	if idx := strings.Index(s, "://"); idx >= 0 {
		s = s[idx+3:]
	}

	// Strip file path: everything up to and including the last file extension + dot
	// "bench/cross-system/corpus/repos/flask/src/flask/sansio/scaffold.py.Scaffold.before_request"
	// -> "Scaffold.before_request"
	s = stripFilePath(s)

	// Strip repository URL prefix (for formats without ://)
	s = stripRepoURL(s)

	// Keep only the last path component + symbol
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		s = s[idx+1:]
	}

	return s
}

// stripFilePath removes file path components, leaving only the symbol part.
// For "path/to/file.py.ClassName.method" -> "ClassName.method"
// For "path/to/file.go.FuncName" -> "FuncName"
// For "github.com/org/repo/internal/context.ContextEngine.ForTask" -> "ContextEngine.ForTask"
func stripFilePath(s string) string {
	// Case 1: explicit file extension followed by dot (Python, TS, etc.)
	exts := []string{".go.", ".py.", ".ts.", ".tsx.", ".js.", ".jsx.", ".rs.", ".java.", ".cs.", ".rb."}
	for _, ext := range exts {
		if idx := strings.LastIndex(s, ext); idx >= 0 {
			return s[idx+len(ext):]
		}
	}

	// Case 2: Go-style "package/path.Symbol" (no file extension in the qualified name).
	// After stripping repo URL, we have something like "internal/context.ContextEngine.ForTask".
	// The last slash separates the package path from the symbol.
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		after := s[idx+1:]
		// "context.ContextEngine.ForTask" -> split on first dot to get symbol
		if dotIdx := strings.Index(after, "."); dotIdx >= 0 {
			return after[dotIdx+1:]
		}
		return after
	}

	// Case 3: Python module-style "flask.app.Flask.before_request"
	// Heuristic: lowercase dot-components are modules, uppercase-starting are symbols.
	parts := strings.Split(s, ".")
	for i, p := range parts {
		if len(p) > 0 && p[0] >= 'A' && p[0] <= 'Z' {
			// First capitalized component: this is where the symbol starts.
			return strings.Join(parts[i:], ".")
		}
	}

	// Case 4: All-lowercase Python path like "django.template.defaultfilters.floatformat".
	// No uppercase marker, so the last component is the function name.
	// This matches knowing's normalization which strips via file extension
	// (e.g., "defaultfilters.py.floatformat" -> "floatformat").
	if len(parts) > 1 {
		return parts[len(parts)-1]
	}

	return s
}

// MatchesGroundTruth checks if a retrieved symbol matches a ground truth entry.
// Uses multiple matching strategies to bridge different qualification levels:
//   - Exact match after normalization
//   - Terminal name match (last component after all dots)
//   - Substring containment
//   - Suffix match
func MatchesGroundTruth(retrieved, groundTruth string) bool {
	r := Symbol(retrieved)
	g := Symbol(groundTruth)

	if r == "" || g == "" {
		return false
	}

	// Exact match
	if r == g {
		return true
	}

	// Case-insensitive exact match
	if strings.EqualFold(r, g) {
		return true
	}

	// Suffix match: bridges different qualification depths.
	// "AuthenticationMiddleware.Invoke" matches "Ocelot.Authentication.AuthenticationMiddleware.Invoke"
	// because they are the same symbol at different namespace depths. This is not
	// permissive: it handles the structural fact that knowing strips file paths
	// (producing "ClassName.Method") while ground truth may retain namespace prefixes
	// (producing "Namespace.ClassName.Method").
	if strings.HasSuffix(r, "."+g) || strings.HasSuffix(g, "."+r) {
		return true
	}
	if strings.EqualFold(r, g) {
		return true
	}
	// Also check case-insensitive suffix for Ruby (module::Class vs class)
	rLower := strings.ToLower(r)
	gLower := strings.ToLower(g)
	if strings.HasSuffix(rLower, "."+gLower) || strings.HasSuffix(gLower, "."+rLower) {
		return true
	}

	// Qualified terminal match: terminal name must match AND at least one
	// qualifier component must overlap. Library.filter matches
	// template.Library.filter (shared "Library"). Library.filter does NOT
	// match QuerySet.filter (no shared qualifier).
	rTerminal := terminalName(r)
	gTerminal := terminalName(g)
	if rTerminal != "" && gTerminal != "" && strings.EqualFold(rTerminal, gTerminal) {
		if qualifierOverlap(r, g) {
			return true
		}
		// If ground truth is just a bare name (no dots), terminal match is sufficient.
		if !strings.Contains(g, ".") || !strings.Contains(r, ".") {
			return true
		}
	}

	// Dot-bounded containment: one symbol is a complete dot-component of the other.
	// "QuerySet" matches "QuerySet.query" (complete component at dot boundary).
	// "Base" does NOT match "DatabaseBase" (mid-word, not dot-bounded).
	// This handles parent-child relationships (returning a method when ground
	// truth is the class, or vice versa) without false positives from
	// coincidental name containment.
	if dotBoundedContains(r, g) || dotBoundedContains(g, r) {
		return true
	}

	return false
}

// terminalName returns the last dot-separated component.
// "Flask.before_request" -> "before_request"
// "before_request" -> "before_request"
func terminalName(s string) string {
	if idx := strings.LastIndex(s, "."); idx >= 0 {
		return s[idx+1:]
	}
	return s
}

// qualifierOverlap checks if the symbols are likely referring to the same thing.
// Returns true if they share terminal name and this is a reasonable match.
// The main false-positive risk is generic method names (save, get, run) on
// different types. We accept this risk for the benchmark (favor recall over precision
// in matching; the benchmark itself measures retrieval precision separately).
func qualifierOverlap(a, b string) bool {
	// If either is unqualified (no dots), always match on terminal name alone.
	if !strings.Contains(a, ".") || !strings.Contains(b, ".") {
		return true
	}

	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	// Check if any NON-TERMINAL component overlaps (case-insensitive).
	// Exclude terminal from both sides to avoid matching on the method name itself.
	aSet := make(map[string]bool)
	for _, p := range aParts[:len(aParts)-1] {
		aSet[strings.ToLower(p)] = true
	}
	for _, p := range bParts[:len(bParts)-1] {
		if aSet[strings.ToLower(p)] {
			return true
		}
	}

	// Require qualifier overlap for all terminal names. No fallback.
	// QuerySet.filter does NOT match Library.filter (no shared qualifier).
	// Flask.before_request matches Scaffold.before_request only if "Flask" or
	// "Scaffold" appears in both sides.
	return false
}

func stripRepoURL(s string) string {
	// Remove "https://..." or "http://..." prefix
	if idx := strings.Index(s, "://"); idx >= 0 {
		s = s[idx+3:]
	}

	// Check if it starts with a domain (github.com, gitlab.com, etc.)
	parts := strings.Split(s, "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") && !containsUpper(parts[0]) {
		// domain/org/repo/rest -> rest
		return strings.Join(parts[3:], "/")
	}
	return s
}

func stripDeclKeyword(s string) string {
	for _, prefix := range []string{"func ", "type ", "var ", "const ", "def ", "class ", "interface ", "struct "} {
		if strings.HasPrefix(s, prefix) {
			return strings.TrimPrefix(s, prefix)
		}
	}
	return s
}

func looksLikeFile(s string) bool {
	exts := []string{".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs", ".java", ".cs", ".rb"}
	for _, ext := range exts {
		if strings.HasSuffix(s, ext) {
			return true
		}
	}
	return false
}

func containsUpper(s string) bool {
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// dotBoundedContains checks if inner appears as a complete dot-separated
// component (or sequence of components) within outer.
// "QuerySet" in "QuerySet.query" -> true (starts at dot boundary)
// "Library" in "Library.filter" -> true
// "Base" in "DatabaseBase" -> false (mid-word)
// "filter" in "EmptyFieldListFilter" -> false (mid-word)
// "Concern" in "ActiveSupport::Concern" -> true (:: boundary treated as dot)
func dotBoundedContains(outer, inner string) bool {
	// Normalize :: to . for Ruby module separators
	o := strings.ReplaceAll(outer, "::", ".")
	n := strings.ReplaceAll(inner, "::", ".")

	idx := strings.Index(o, n)
	if idx < 0 {
		return false
	}

	// Check left boundary: must be at start or preceded by a dot
	if idx > 0 && o[idx-1] != '.' {
		return false
	}

	// Check right boundary: must be at end or followed by a dot
	end := idx + len(n)
	if end < len(o) && o[end] != '.' {
		return false
	}

	return true
}
