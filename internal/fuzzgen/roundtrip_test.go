package fuzzgen

import (
	"math/rand/v2"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/justinstimatze/defn/internal/emit"
	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/store"
)

// TestRoundTrip_Hazards runs every hazard individually, then all of them
// combined at a few fixed seeds, on every `go test ./...` -- the
// always-on floor. FuzzRoundTrip (fuzz_test.go) is the slower opt-in deep
// search over this same assertion.
func TestRoundTrip_Hazards(t *testing.T) {
	for _, h := range AllHazards {
		t.Run(h.Name, func(t *testing.T) {
			r := rand.New(rand.NewPCG(1, 1))
			m := Generate(r, GenOpts{Hazards: []Hazard{h}})
			assertRoundTrip(t, m)
		})
	}

	t.Run("all_hazards_combined", func(t *testing.T) {
		for _, seed := range []uint64{1, 2, 3} {
			r := rand.New(rand.NewPCG(seed, seed))
			m := Generate(r, GenOpts{Hazards: AllHazards})
			assertRoundTrip(t, m)
		}
	})
}

// assertRoundTrip is the phase-1 oracle: declaration-multiset equality
// (catches missing/duplicated decls -- both known historical bugs), then
// a real `go build` on the emitted tree (catches anything structurally
// subtler that still produced a buildable-but-wrong tree). The
// AST-normalized-diff layer from the design plan is deferred -- multiset
// equality plus a real build already reproduces both known bugs and is
// far cheaper to keep flake-free against goimports' own legitimate
// formatting churn.
func assertRoundTrip(t *testing.T, m *SyntheticModule) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH")
	}

	srcDir := t.TempDir()
	if err := m.WriteTo(srcDir); err != nil {
		t.Fatalf("write synthetic module: %v", err)
	}

	db := fuzzgenTestDB(t)
	if err := ingest.Ingest(db, srcDir); err != nil {
		t.Fatalf("ingest synthetic source (likely a generator bug, not a defn bug): %v", err)
	}
	before, err := declMultiset(db)
	if err != nil {
		t.Fatalf("declMultiset before: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("generator produced zero definitions")
	}

	outDir := t.TempDir()
	if err := emit.Emit(db, outDir); err != nil {
		t.Fatalf("emit: %v", err)
	}

	db2 := fuzzgenTestDB(t)
	if err := ingest.Ingest(db2, outDir); err != nil {
		t.Fatalf("re-ingest emitted output: %v", err)
	}
	after, err := declMultiset(db2)
	if err != nil {
		t.Fatalf("declMultiset after: %v", err)
	}

	if diff := diffMultisets(before, after); diff != "" {
		t.Fatalf("declaration multiset changed across ingest->emit round trip:\n%s", diff)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = outDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build on emitted tree failed:\n%s", out)
	}
}

func fuzzgenTestDB(t *testing.T) store.Backend {
	t.Helper()
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
