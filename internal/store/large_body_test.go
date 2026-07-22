package store

import (
	"strings"
	"testing"
)

// TestLargeBodyRoundTrip regression-tests winze's 2026-07-22 discovery
// that emit truncates function bodies > Dolt's inline threshold (~1.4KB).
// If the body written via UpsertDefinition reads back at a different
// length via any of the standard access paths, we've got a data-integrity
// bug — regardless of what path the truncation lives on (INSERT side,
// SCAN side, or a helper that raw-string-scans and slipped past #113).
func TestLargeBodyRoundTrip(t *testing.T) {
	// Sizes bracketing Dolt's known TextStorage threshold (~1.4KB) plus
	// deliberately-huge to catch any silent buffer cap. Every size must
	// round-trip byte-identically.
	sizes := []int{500, 1000, 1400, 1500, 2000, 5000, 10000, 50000}
	for _, n := range sizes {
		t.Run(sizeName(n), func(t *testing.T) {
			db := testDB(t)
			mod, err := db.EnsureModule("example.com/large", "large", "")
			if err != nil {
				t.Fatal(err)
			}
			body := makeGoBody(n)
			d := &Definition{
				ModuleID: mod.ID, Name: "F", Kind: "function", Exported: true,
				Body:       body,
				SourceFile: "f.go",
			}
			id, err := db.UpsertDefinition(d)
			if err != nil {
				t.Fatalf("upsert: %v", err)
			}

			// Path 1: single-row getter (GetDefinition → scanDefRow).
			got, err := db.GetDefinition(id)
			if err != nil {
				t.Fatalf("GetDefinition: %v", err)
			}
			assertBodyMatches(t, "GetDefinition", body, got.Body)

			// Path 2: name-based getter (GetDefinitionByName → scanDefRow).
			got, err = db.GetDefinitionByName("F", "")
			if err != nil {
				t.Fatalf("GetDefinitionByName: %v", err)
			}
			assertBodyMatches(t, "GetDefinitionByName", body, got.Body)

			// Path 3: bulk module load (GetModuleDefinitions → scanDefinitions
			// → scanDefRow) — this is emit's read path.
			defs, err := db.GetModuleDefinitions(mod.ID)
			if err != nil {
				t.Fatalf("GetModuleDefinitions: %v", err)
			}
			var found *Definition
			for i := range defs {
				if defs[i].Name == "F" {
					found = &defs[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("GetModuleDefinitions: F not returned")
			}
			assertBodyMatches(t, "GetModuleDefinitions", body, found.Body)

			// Path 4: bulk body map (GetBodiesByDefIDs → textCol scan).
			m, err := db.GetBodiesByDefIDs([]int64{id})
			if err != nil {
				t.Fatalf("GetBodiesByDefIDs: %v", err)
			}
			assertBodyMatches(t, "GetBodiesByDefIDs", body, m[id])
		})
	}
}

func assertBodyMatches(t *testing.T, path, want, got string) {
	t.Helper()
	if got == want {
		return
	}
	t.Errorf("%s: body length mismatch: got %d bytes, want %d bytes", path, len(got), len(want))
	// Where did it diverge?
	minLen := len(got)
	if len(want) < minLen {
		minLen = len(want)
	}
	for i := 0; i < minLen; i++ {
		if got[i] != want[i] {
			t.Errorf("%s: first byte difference at offset %d", path, i)
			return
		}
	}
	if len(got) < len(want) {
		t.Errorf("%s: TRUNCATED — got is a prefix of want, missing last %d bytes (tail: %q)",
			path, len(want)-len(got), want[len(got):min(len(want), len(got)+40)])
	}
}

// makeGoBody builds a parseable Go func whose body is roughly `size` bytes.
// Uses printable ASCII so any truncation is visible in error messages.
func makeGoBody(size int) string {
	var sb strings.Builder
	sb.WriteString("func F() {\n")
	// pad with a series of comments (each line ~50 chars) to reach size
	line := "\t// " + strings.Repeat("padding-content-", 2) + "\n"
	for sb.Len()+len(line)+2 < size {
		sb.WriteString(line)
	}
	sb.WriteString("}\n")
	return sb.String()
}

func sizeName(n int) string {
	if n < 1024 {
		return string(rune('0'+n/100)) + "00B"
	}
	kb := n / 1024
	rem := n % 1024
	if rem == 0 {
		return string(rune('0'+kb)) + "KB"
	}
	return string(rune('0'+kb)) + "K+"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
