// Package embed computes cheap, local, dependency-free semantic-ish
// vectors for short pieces of text (def names, signatures, doc
// comments, and natural-language questions) via the hashing trick
// (feature hashing). Unlike the Haiku/Sonnet co-processor paths in
// internal/summary, this never makes a network call, never costs
// money, and needs no API key or opt-out flag -- it is pure CPU,
// cheap enough to run inline on every call rather than precomputed
// and cached.
//
// #198: gives code(op:"context") a semantic-search signal that token
// matching (name-LIKE, FTS body, summary-LIKE) structurally can't
// provide -- a question can be semantically close to a def's name/doc
// even when they share zero literal words.
package embed

import (
	"hash/fnv"
	"math"
	"unicode"
)

// Dims is the fixed vector width. Small enough that embedding and
// comparing a few thousand short strings costs microseconds total;
// large enough that hash collisions don't wash out distinct tokens on
// typical Go-identifier vocabularies.
const Dims = 64

// Embed computes a deterministic vector for text: each token from
// tokenize is hashed into one of Dims buckets with a sign derived
// from a second bit of the same hash, contributions are summed, and
// the result is L2-normalized so Cosine reduces to a plain dot
// product. Empty or all-punctuation text returns a zero vector (all
// comparisons against it are 0 via Cosine's guard).
func Embed(text string) []float32 {
	vec := make([]float32, Dims)
	for _, tok := range tokenize(text) {
		h := fnv.New32a()
		h.Write([]byte(tok))
		hv := h.Sum32()
		bucket := hv % uint32(Dims)
		sign := float32(1)
		if (hv>>16)&1 == 1 {
			sign = -1
		}
		vec[bucket] += sign
	}
	normalize(vec)
	return vec
}

// Cosine returns the cosine similarity of two vectors in [-1, 1].
// Returns 0 for mismatched lengths or when either vector is all-zero
// (e.g. Embed of empty text) -- an undefined angle is not a match.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, sumA, sumB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		sumA += float64(a[i]) * float64(a[i])
		sumB += float64(b[i]) * float64(b[i])
	}
	if sumA == 0 || sumB == 0 {
		return 0
	}
	// sumA/sumB are each vector's squared norm -- dividing by the
	// product of their square roots is what actually makes this cosine
	// similarity rather than a raw dot product. Previously this
	// returned dot unchanged: harmless today because Embed's output is
	// already L2-normalized (its own doc comment says so, which is
	// what let this go unnoticed), but this function's doc comment
	// makes an unqualified "[-1, 1]" claim for ANY two vectors, not
	// just pre-normalized ones -- a future caller passing raw
	// (non-unit) vectors would have silently gotten a value outside
	// that range and not actually a cosine similarity.
	return dot / (math.Sqrt(sumA) * math.Sqrt(sumB))
}

func normalize(vec []float32) {
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	if sumSq == 0 {
		return
	}
	norm := float32(math.Sqrt(sumSq))
	for i := range vec {
		vec[i] /= norm
	}
}

// tokenize splits text into lowercase word tokens, treating any
// non-alphanumeric rune as a separator and additionally splitting at
// camelCase boundaries (lower->Upper, or an uppercase run followed by
// a lowercase run, e.g. "HTTPServer" -> "http", "server") so
// identifier-heavy Go source and natural-language questions hash to
// overlapping vocabulary -- "GetUserCredentials" and "get user
// credentials" produce the same three tokens. Tokens under 2 runes
// are dropped as noise, mirroring the floor internal/mcp's own
// query tokenizer uses.
func tokenize(text string) []string {
	runes := []rune(text)
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) >= 2 {
			word := make([]rune, len(cur))
			for i, r := range cur {
				word[i] = unicode.ToLower(r)
			}
			words = append(words, string(word))
		}
		cur = cur[:0]
	}
	for i, r := range runes {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && isCamelBoundary(runes, i):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

// isCamelBoundary reports whether position i in runes starts a new
// camelCase word: either the previous rune was lowercase ("get|User"),
// or the previous rune was uppercase but the next one is lowercase,
// marking the end of an acronym run ("HTTP|Server" splits after the
// 'P' since 'S' is followed by lowercase "erver").
func isCamelBoundary(runes []rune, i int) bool {
	prev := runes[i-1]
	if unicode.IsLower(prev) {
		return true
	}
	if unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
		return true
	}
	return false
}
