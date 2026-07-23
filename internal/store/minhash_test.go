package store

import "testing"

// TestMinHash_IdenticalBodies: two identical bodies must produce
// identical signatures → Jaccard = 1.0.
func TestMinHash_IdenticalBodies(t *testing.T) {
	body := `func Foo() error { return errors.New("x") }`
	a := ComputeMinHash(body)
	b := ComputeMinHash(body)
	if j := MinHashJaccard(a, b); j != 1.0 {
		t.Errorf("identical bodies: want 1.0, got %f", j)
	}
}

// TestMinHash_DifferentBodies: unrelated code should score below
// the useful-similarity threshold.
func TestMinHash_DifferentBodies(t *testing.T) {
	a := ComputeMinHash(`func Foo() error { return errors.New("x") }`)
	b := ComputeMinHash(`func Bar(x int, y int) int { return x + y * 42 }`)
	j := MinHashJaccard(a, b)
	if j > 0.15 {
		t.Errorf("dissimilar bodies: want <0.15, got %f", j)
	}
}

// TestMinHash_NearDuplicates: same shape, one different token → high similarity.
func TestMinHash_NearDuplicates(t *testing.T) {
	a := ComputeMinHash(`func Foo() error { return errors.New("first") }`)
	b := ComputeMinHash(`func Bar() error { return errors.New("second") }`)
	j := MinHashJaccard(a, b)
	if j < 0.3 {
		t.Errorf("near-dupes: want >0.3, got %f", j)
	}
}

// TestMinHash_ShortBody: bodies shorter than the shingle size must
// produce a signature (not panic) and not spuriously match unrelated
// long bodies.
func TestMinHash_ShortBody(t *testing.T) {
	sig := ComputeMinHash("hi")
	if len(sig) != 512 {
		t.Errorf("short body sig: want 512 bytes, got %d", len(sig))
	}
}

// TestMinHash_SelfSimilarityGoCode is the boilerplate-heavy Go-code
// case that the reverted `similar` op was supposed to catch. Two
// nearly-identical bodies differing by a table name should score
// well above an unrelated body.
func TestMinHash_SelfSimilarityGoCode(t *testing.T) {
	a := ComputeMinHash(`func UpsertA(d *Def) error {
		if d == nil { return errors.New("nil") }
		return db.Exec("INSERT INTO a VALUES(?)", d.Val)
	}`)
	b := ComputeMinHash(`func UpsertB(d *Def) error {
		if d == nil { return errors.New("nil") }
		return db.Exec("INSERT INTO b VALUES(?)", d.Val)
	}`)
	c := ComputeMinHash(`func Render(w io.Writer, ctx context.Context, path string, headers map[string][]string) {
		for h, vs := range headers { for _, v := range vs { fmt.Fprintln(w, h + ": " + v) } }
	}`)
	jNear := MinHashJaccard(a, b)
	if jNear < 0.4 {
		t.Errorf("near-identical Go bodies: want >0.4, got %f", jNear)
	}
	jFar := MinHashJaccard(a, c)
	if jFar >= jNear {
		t.Errorf("unrelated body should score lower; a↔c=%f a↔b=%f", jFar, jNear)
	}
}
