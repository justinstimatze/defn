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

// TestUpsertDefinitionsBulk covers #125's batched insert path: insert
// path, hash-unchanged fast-skip, hash-changed fall-through, ID ordering
// aligned to input, mixed-module input.
func TestUpsertDefinitionsBulk(t *testing.T) {
	db := testDB(t)
	modA, _ := db.EnsureModule("example.com/a", "a", "")
	modB, _ := db.EnsureModule("example.com/b", "b", "")

	// Round 1: fresh insert of 6 defs across 2 modules.
	defs := []*Definition{
		{ModuleID: modA.ID, Name: "F1", Kind: "function", Body: "func F1() {}"},
		{ModuleID: modA.ID, Name: "F2", Kind: "function", Body: "func F2() {}"},
		{ModuleID: modA.ID, Name: "M", Kind: "method", Receiver: "*T", Body: "func (t *T) M() {}"},
		{ModuleID: modB.ID, Name: "F1", Kind: "function", Body: "func F1() { /*B*/ }"},
		{ModuleID: modB.ID, Name: "T", Kind: "type", Body: "type T struct{}"},
		{ModuleID: modA.ID, Name: "F3", Kind: "function", Body: "func F3() {}"},
	}
	ids, err := db.UpsertDefinitionsBulk(defs)
	if err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if len(ids) != len(defs) {
		t.Fatalf("expected %d ids, got %d", len(defs), len(ids))
	}
	for i, id := range ids {
		if id == 0 {
			t.Errorf("ids[%d] is zero for %s", i, defs[i].Name)
		}
	}
	// Every ID must be unique.
	seen := map[int64]bool{}
	for i, id := range ids {
		if seen[id] {
			t.Errorf("duplicate id %d at index %d", id, i)
		}
		seen[id] = true
	}
	// Verify bodies round-trip identically.
	for i, d := range defs {
		got, err := db.GetDefinition(ids[i])
		if err != nil {
			t.Fatalf("GetDefinition(%d): %v", ids[i], err)
		}
		if got.Body != d.Body {
			t.Errorf("body mismatch for %s: got %q want %q", d.Name, got.Body, d.Body)
		}
	}

	// Round 2: same input again — should hit fast-skip path (unchanged hash),
	// return same IDs, not create duplicates.
	ids2, err := db.UpsertDefinitionsBulk(defs)
	if err != nil {
		t.Fatalf("bulk re-insert: %v", err)
	}
	for i := range defs {
		if ids2[i] != ids[i] {
			t.Errorf("re-insert changed id for %s: %d → %d", defs[i].Name, ids[i], ids2[i])
		}
	}

	// Round 3: change one body — should fall through to per-row update.
	defs[1].Body = "func F2() { /* changed */ }"
	ids3, err := db.UpsertDefinitionsBulk(defs)
	if err != nil {
		t.Fatalf("bulk update: %v", err)
	}
	if ids3[1] != ids[1] {
		t.Errorf("update changed id for F2: %d → %d", ids[1], ids3[1])
	}
	got, _ := db.GetDefinition(ids[1])
	if !strings.Contains(got.Body, "/* changed */") {
		t.Errorf("update didn't persist: %q", got.Body)
	}

	// Round 4: mix new + existing. New rows get fresh IDs, existing keep theirs.
	mixed := append([]*Definition{
		{ModuleID: modA.ID, Name: "NEW", Kind: "function", Body: "func NEW() {}"},
	}, defs...)
	ids4, err := db.UpsertDefinitionsBulk(mixed)
	if err != nil {
		t.Fatalf("bulk mixed: %v", err)
	}
	if ids4[0] == 0 {
		t.Errorf("NEW got no id")
	}
	for i := 1; i < len(mixed); i++ {
		if ids4[i] != ids[i-1] {
			t.Errorf("mixed changed id for %s at pos %d: %d → %d",
				mixed[i].Name, i, ids[i-1], ids4[i])
		}
	}
}

// TestUpsertDefinitionsBulkLargeBodies is the batched-path equivalent of
// TestLargeBodyRoundTrip — proves the bulk INSERT for bodies preserves
// content across the Dolt TextStorage threshold, not just the per-row path.
func TestUpsertDefinitionsBulkLargeBodies(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/big", "big", "")

	defs := []*Definition{
		{ModuleID: mod.ID, Name: "Small", Kind: "function", Body: makeGoBody(500)},
		{ModuleID: mod.ID, Name: "AtThreshold", Kind: "function", Body: makeGoBody(1400)},
		{ModuleID: mod.ID, Name: "OverThreshold", Kind: "function", Body: makeGoBody(2000)},
		{ModuleID: mod.ID, Name: "Big", Kind: "function", Body: makeGoBody(10000)},
	}
	ids, err := db.UpsertDefinitionsBulk(defs)
	if err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	for i, d := range defs {
		got, err := db.GetDefinition(ids[i])
		if err != nil {
			t.Fatalf("GetDefinition(%d): %v", ids[i], err)
		}
		if got.Body != d.Body {
			t.Errorf("%s: body mismatch (got %d bytes want %d bytes)",
				d.Name, len(got.Body), len(d.Body))
		}
	}
}
