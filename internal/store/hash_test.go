package store

import "testing"

// Body pair generator: same semantics, different surface.
func TestHashBodyStructural_WhitespaceInvariant(t *testing.T) {
	a := `func Greet(name string) string {
	return "Hello, " + name
}`
	b := `func   Greet(name    string)    string {
    return    "Hello, "   +   name
}`
	ha := HashBodyStructural(a)
	hb := HashBodyStructural(b)
	if ha != hb {
		t.Errorf("whitespace changed hash:\n  a=%s\n  b=%s", ha, hb)
	}
}

func TestHashBodyStructural_CommentInvariant(t *testing.T) {
	a := `func Greet(name string) string {
	return "Hello, " + name
}`
	b := `// Greet returns a greeting.
func Greet(name string) string {
	// concatenate the greeting
	return "Hello, " + name /* trailing */
}`
	ha := HashBodyStructural(a)
	hb := HashBodyStructural(b)
	if ha != hb {
		t.Errorf("comments changed hash:\n  a=%s\n  b=%s", ha, hb)
	}
}

func TestHashBodyStructural_DifferentStatements(t *testing.T) {
	a := `func Greet(name string) string {
	return "Hello, " + name
}`
	b := `func Greet(name string) string {
	return "Hi, " + name
}`
	ha := HashBodyStructural(a)
	hb := HashBodyStructural(b)
	if ha == hb {
		t.Errorf("different literals should not share hash: both=%s", ha)
	}
}

func TestHashBodyStructural_DifferentOperators(t *testing.T) {
	a := `func f(x, y int) int { return x + y }`
	b := `func f(x, y int) int { return x - y }`
	ha := HashBodyStructural(a)
	hb := HashBodyStructural(b)
	if ha == hb {
		t.Errorf("different operators should not share hash: both=%s", ha)
	}
}

func TestHashBodyStructural_ExtraStatement(t *testing.T) {
	a := `func f(x int) int {
	return x
}`
	b := `func f(x int) int {
	x = x * 2
	return x
}`
	ha := HashBodyStructural(a)
	hb := HashBodyStructural(b)
	if ha == hb {
		t.Errorf("extra statement should not share hash: both=%s", ha)
	}
}

func TestHashBodyStructural_IdentifierRenamesChangeHash(t *testing.T) {
	// Documented limitation: identifier renames DO change hash.
	// This test pins the behavior so anyone who "fixes" it thinks twice.
	a := `func Greet(name string) string { return "Hello, " + name }`
	b := `func Greet(who string) string { return "Hello, " + who }`
	ha := HashBodyStructural(a)
	hb := HashBodyStructural(b)
	if ha == hb {
		t.Errorf("identifier rename should change hash under v1 policy; both=%s", ha)
	}
}

func TestHashBodyStructural_Deterministic(t *testing.T) {
	body := `func Greet(name string) string {
	return "Hello, " + name
}`
	h1 := HashBodyStructural(body)
	h2 := HashBodyStructural(body)
	if h1 != h2 {
		t.Errorf("non-deterministic: %s vs %s", h1, h2)
	}
	// Fixed-length hex string (SHA256 = 64 chars).
	if len(h1) != 64 {
		t.Errorf("expected 64-char hex, got %d chars: %s", len(h1), h1)
	}
}

func TestHashBodyStructural_UnparseableFallback(t *testing.T) {
	// Not a full Go declaration — parser will fail. We should still get
	// a deterministic string back (raw-text SHA256 fallback).
	body := "not valid go @#$"
	h := HashBodyStructural(body)
	if len(h) != 64 {
		t.Errorf("expected 64-char hex on fallback, got %d: %s", len(h), h)
	}
	// Same input should still be deterministic.
	if HashBodyStructural(body) != h {
		t.Error("fallback is not deterministic")
	}
}

func TestHashBodyStructural_DiffersFromHashBody(t *testing.T) {
	// The two hash functions serve different purposes and should not
	// collide for typical inputs. Not a strict requirement but a smell
	// test: if these ever agree on a normal body, someone likely broke
	// the structural invariance.
	body := `func Greet(name string) string {
	return "Hello, " + name
}`
	if HashBody(body) == HashBodyStructural(body) {
		t.Error("HashBody and HashBodyStructural collided on a typical body — did structural fall back to raw?")
	}
}
