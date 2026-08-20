package embed

import (
	"math"
	"testing"
)

func TestCosine_EmptyVectorsReturnZero(t *testing.T) {
	if got := Cosine(nil, nil); got != 0 {
		t.Errorf("expected 0 for empty vectors, got %v", got)
	}
}

func TestCosine_MismatchedLengthReturnsZero(t *testing.T) {
	if got := Cosine([]float32{1, 2}, []float32{1, 2, 3}); got != 0 {
		t.Errorf("expected 0 for mismatched lengths, got %v", got)
	}
}

func TestCosine_ZeroVectorReturnsZero(t *testing.T) {
	zero := make([]float32, Dims)
	v := Embed("something")
	if got := Cosine(zero, v); got != 0 {
		t.Errorf("expected 0 similarity against an all-zero vector, got %v", got)
	}
}

func TestEmbed_CamelCaseMatchesNaturalLanguage(t *testing.T) {
	code := Embed("func VerifyLoginToken(token string) bool")
	question := Embed("verify a login token")
	unrelated := Embed("compute checksum of a file on disk")

	simRelated := Cosine(code, question)
	simUnrelated := Cosine(code, unrelated)
	if simRelated <= simUnrelated {
		t.Errorf("expected VerifyLoginToken closer to its own natural-language description (%v) than to an unrelated question (%v)", simRelated, simUnrelated)
	}
}

func TestEmbed_Deterministic(t *testing.T) {
	a := Embed("get user credentials")
	b := Embed("get user credentials")
	if Cosine(a, b) < 0.9999 {
		t.Errorf("expected identical text to embed identically, cosine=%v", Cosine(a, b))
	}
}

func TestEmbed_EmptyTextReturnsZeroVector(t *testing.T) {
	v := Embed("")
	for i, x := range v {
		if x != 0 {
			t.Fatalf("expected all-zero vector for empty text, got nonzero at index %d: %v", i, v)
		}
	}
}

func TestEmbed_SelfSimilarityIsOne(t *testing.T) {
	v := Embed("verify a login token against the session store")
	sim := Cosine(v, v)
	if sim < 0.999 || sim > 1.001 {
		t.Errorf("expected self-cosine ~1.0, got %v", sim)
	}
}

func TestTokenize_CamelCaseAndSnakeCaseSplitting(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"GetUserCredentials", []string{"get", "user", "credentials"}},
		{"get_user_credentials", []string{"get", "user", "credentials"}},
		{"HTTPServer", []string{"http", "server"}},
		{"handleEdit", []string{"handle", "edit"}},
	}
	for _, tt := range tests {
		got := tokenize(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("tokenize(%q) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

// TestCosine_NormalizesNonUnitVectors is the regression for Cosine
// returning a raw dot product instead of actually dividing by the
// vectors' norms: previously harmless only because the sole caller
// (contextEmbeddingCandidates) always passes Embed's pre-normalized
// output, but Cosine's own doc comment makes an unqualified "[-1, 1]"
// claim for any two vectors. Two parallel but non-unit vectors must
// report similarity 1.0, not their raw (much larger) dot product.
func TestCosine_NormalizesNonUnitVectors(t *testing.T) {
	a := []float32{3, 4} // norm 5
	b := []float32{6, 8} // norm 10, same direction as a

	got := Cosine(a, b)
	if math.Abs(got-1.0) > 1e-9 {
		t.Errorf("Cosine(a, b) = %v, want 1.0 (parallel vectors) -- raw dot product would be 50", got)
	}

	orth := []float32{-4, 3} // norm 5, orthogonal to a
	got2 := Cosine(a, orth)
	if math.Abs(got2) > 1e-9 {
		t.Errorf("Cosine(a, orth) = %v, want 0.0 (orthogonal vectors)", got2)
	}
}
