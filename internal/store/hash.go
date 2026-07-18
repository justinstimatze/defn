package store

import (
	"crypto/sha256"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

// HashBodyStructural computes an AST-shape hash of a Go definition body.
// Whitespace and comments do not affect the hash; statement structure,
// operators, identifiers, and literals do.
//
// Contract vs HashBody (twin, contracted-twin-ok):
//   - HashBody(SHA256 of raw text) is for cache-invalidation on ANY body edit.
//   - HashBodyStructural is for "same semantics as upstream" checks — used by
//     the delta-from-prior projection to decide when a library def matches
//     its upstream fingerprint and the body can be replaced by a provenance tag.
//
// Identifier renames DO change the hash (accepted limitation — rare in
// tagged upstream releases; a rename between library versions is a real
// divergence worth attaching the body for).
//
// Empty or unparseable bodies return the raw-text SHA256 as a fallback so
// callers always get a deterministic string.
func HashBodyStructural(body string) string {
	fset := token.NewFileSet()
	// Same "package p\n" prefix pattern the projection ops use. Parse WITHOUT
	// ParseComments so comments never enter the AST — the printer can't emit
	// what it doesn't have.
	src := "package p\n" + body
	f, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		// Fallback: raw hash. Also covers the case where body isn't a full
		// declaration (e.g. a bare statement) — the caller gets a stable
		// value but the "structural" invariance property doesn't apply.
		h := sha256.Sum256([]byte(body))
		return fmt.Sprintf("%x", h)
	}

	// Re-emit via go/printer with a canonical config. UseSpaces + Tabwidth 0
	// gives us a fixed rendering independent of the source's original spacing.
	var sb strings.Builder
	cfg := printer.Config{Mode: printer.UseSpaces, Tabwidth: 0, Indent: 0}
	if err := cfg.Fprint(&sb, fset, f); err != nil {
		h := sha256.Sum256([]byte(body))
		return fmt.Sprintf("%x", h)
	}
	// Drop the "package p" prefix we added so hashes are body-scoped, not
	// package-scoped. Everything before the first blank line is the package
	// clause + any file-level comments (already stripped).
	canonical := sb.String()
	if idx := strings.Index(canonical, "\n\n"); idx >= 0 {
		canonical = canonical[idx+2:]
	}

	// The printer preserves source-derived vertical whitespace even after we
	// strip comments — a comment's blank line stays as a blank line. Collapse
	// all whitespace runs to a single space so the hash reflects token-stream
	// structure, not preserved formatting quirks.
	canonical = collapseWhitespace(canonical)

	h := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%x", h)
}

// collapseWhitespace replaces every run of whitespace with a single space and
// trims leading/trailing whitespace. Used to make HashBodyStructural robust to
// vertical-spacing artifacts left by the printer after comment stripping.
func collapseWhitespace(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !inSpace && sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			inSpace = true
		} else {
			sb.WriteRune(r)
			inSpace = false
		}
	}
	return strings.TrimRight(sb.String(), " ")
}
