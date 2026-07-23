// Package store: MinHash-32 signatures over body shingles. Task #151.
//
// MinHash approximates Jaccard similarity of two sets in constant-size
// signatures. For defn: 5-char body shingles, 128 hash functions, 512
// bytes/def. Similarity between two defs ≈ fraction of matching min-
// hashes over all 128 positions.
//
// Not perfect for code (char shingles are noisy vs token shingles), but
// pure Go, no CGO, cheap to compute at ingest time. Post-migration
// unlock — [[project-post-dolt-projection-brainstorm]] item M. Later
// followup can swap to token shingles for higher fidelity.
package store

import (
	"encoding/binary"
	"hash/fnv"
	"math"
)

const (
	minHashK        = 5   // shingle size (chars)
	minHashN        = 128 // number of hash functions in the signature
	minHashSigBytes = minHashN * 4
)

// ComputeMinHash returns a 512-byte MinHash signature over the body's
// character 5-shingles. Uses FNV-1a-32 seeded with (2 × N distinct)
// prime-multiplied seeds to simulate N independent hash functions —
// standard "hash-with-different-seeds" MinHash pattern. Zero-length or
// too-short bodies return an all-max signature so they don't spuriously
// match everything.
func ComputeMinHash(body string) []byte {
	sig := make([]uint32, minHashN)
	for i := range sig {
		sig[i] = math.MaxUint32
	}
	if len(body) < minHashK {
		return encodeSig(sig)
	}
	// Precompute per-slot multipliers. Simple large primes; combined
	// with FNV inside the loop this gives adequately independent
	// hash families for approximate similarity.
	seeds := make([]uint32, minHashN)
	for i := range seeds {
		seeds[i] = uint32(i)*2654435761 + 1 // Knuth's multiplicative hash
	}
	// Slide a k-char window over the body.
	for start := 0; start+minHashK <= len(body); start++ {
		shingle := body[start : start+minHashK]
		h := fnv.New32a()
		h.Write([]byte(shingle))
		base := h.Sum32()
		for i := 0; i < minHashN; i++ {
			hi := base ^ seeds[i]
			// Standard MinHash mix so different seeds don't collapse.
			hi = hi*2654435761 + 0x9E3779B9
			if hi < sig[i] {
				sig[i] = hi
			}
		}
	}
	return encodeSig(sig)
}

func encodeSig(sig []uint32) []byte {
	out := make([]byte, minHashSigBytes)
	for i, v := range sig {
		binary.LittleEndian.PutUint32(out[i*4:(i+1)*4], v)
	}
	return out
}

// MinHashJaccard estimates the Jaccard similarity of two signatures by
// counting matching positions and dividing by minHashN. Returns 0 when
// either signature is malformed. Cheap: 128 uint32 comparisons.
func MinHashJaccard(a, b []byte) float64 {
	if len(a) != minHashSigBytes || len(b) != minHashSigBytes {
		return 0
	}
	matches := 0
	for i := 0; i < minHashN; i++ {
		off := i * 4
		if binary.LittleEndian.Uint32(a[off:off+4]) == binary.LittleEndian.Uint32(b[off:off+4]) {
			matches++
		}
	}
	return float64(matches) / float64(minHashN)
}
