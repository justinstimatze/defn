package ingest

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// TestMultiModuleIngest_IsolatesBrokenPackages runs a handful of fixed
// configurations of a synthetic multi-module tree (root module + nested
// modules, each with several packages, a random subset deliberately
// broken via a genuine compile error confined to one package's test
// file) on every `go test ./...` -- the always-on floor for the fix
// that made a single broken package's compile error stop poisoning the
// ENTIRE ingest. Confirmed live on a real go-zero bench trajectory: a
// pre-existing signature-mismatch error in a nested tools/goctl
// module's own test file lost every definition in the whole repo
// (core/, rest/, zrpc/, gateway/, all of it), while a plain `go build
// ./...` from the same root succeeded cleanly. FuzzMultiModuleIngest
// (multimodule_fuzz_test.go, same file) is the slower opt-in deep
// search over this same assertion -- this is fuzzgen's TestX/FuzzX
// pattern (see internal/fuzzgen/roundtrip_test.go), applied to repo
// TOPOLOGY (multi-module trees + partial package failure) rather than
// fuzzgen's own concern (single-module round-trip emit fidelity),
// which is a structurally different fuzzing dimension fuzzgen's own
// generator never exercised.
func TestMultiModuleIngest_IsolatesBrokenPackages(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(seed, seed))
			assertMultiModuleIngestIsolatesBroken(t, r, 3, 3, 40)
		})
	}
	t.Run("all_broken_hard_fails", func(t *testing.T) {
		r := rand.New(rand.NewPCG(99, 99))
		assertMultiModuleIngestIsolatesBroken(t, r, 2, 2, 100)
	})
	t.Run("none_broken_all_ingest", func(t *testing.T) {
		r := rand.New(rand.NewPCG(7, 7))
		assertMultiModuleIngestIsolatesBroken(t, r, 3, 2, 0)
	})
}

// FuzzMultiModuleIngest is the slower, opt-in deep-search companion to
// TestMultiModuleIngest_IsolatesBrokenPackages: `go test
// -run=FuzzMultiModuleIngest -fuzz=FuzzMultiModuleIngest -fuzztime=5m`.
// Any crasher Go's fuzzing engine finds is written to
// testdata/fuzz/FuzzMultiModuleIngest/<hash> automatically and becomes
// a permanent regression seed the next time `go test ./...` runs it as
// part of the corpus.
func FuzzMultiModuleIngest(f *testing.F) {
	f.Add(uint64(1), uint8(2), uint8(2), uint8(30))
	f.Add(uint64(2), uint8(4), uint8(3), uint8(60))
	f.Add(uint64(3), uint8(1), uint8(1), uint8(100))
	f.Fuzz(func(t *testing.T, seed uint64, numModules, pkgsPerModule, brokenPct uint8) {
		r := rand.New(rand.NewPCG(seed, seed))
		assertMultiModuleIngestIsolatesBroken(t, r,
			1+int(numModules%4), 1+int(pkgsPerModule%3), int(brokenPct)%101)
	})
}

// assertMultiModuleIngestIsolatesBroken builds a synthetic tree of
// numModules Go modules (module 0 is the root, at dir itself; modules
// 1..N-1 are nested under dir/modK, each with its own go.mod -- "./..."
// never crosses a nested go.mod boundary, matching real multi-module
// repos like go-zero, etcd, prometheus/documentation/examples), each
// declaring pkgsPerModule packages. Each package gets one healthy
// exported function; brokenPct is the percent chance (0-100) that a
// given package ALSO gets a sibling _test.go file with a genuine
// signature-mismatch type error confined to that one package -- the
// exact shape that used to poison the whole ingest. Asserts: Ingest
// succeeds and stores exactly one definition per healthy package (never
// more, never fewer -- a broken package's own healthy non-test code
// is collateral, matching the current all-or-nothing per-package
// design), and errors only when every single package is broken.
func assertMultiModuleIngestIsolatesBroken(t *testing.T, r *rand.Rand, numModules, pkgsPerModule, brokenPct int) {
	t.Helper()
	dir := t.TempDir()

	healthy := 0
	total := 0
	for m := 0; m < numModules; m++ {
		modDir := dir
		modPath := "example.com/root"
		if m > 0 {
			modPath = fmt.Sprintf("example.com/mod%d", m)
			modDir = filepath.Join(dir, fmt.Sprintf("mod%d", m))
			if err := os.MkdirAll(modDir, 0755); err != nil {
				t.Fatal(err)
			}
		}
		goMod := fmt.Sprintf("module %s\n\ngo 1.22\n", modPath)
		if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(goMod), 0644); err != nil {
			t.Fatal(err)
		}

		for p := 0; p < pkgsPerModule; p++ {
			pkgName := fmt.Sprintf("pkg%d_%d", m, p)
			pkgDir := filepath.Join(modDir, pkgName)
			if err := os.MkdirAll(pkgDir, 0755); err != nil {
				t.Fatal(err)
			}

			funcName := fmt.Sprintf("Func%d_%d", m, p)
			total++
			impl := fmt.Sprintf("package %s\n\nfunc %s(s string) string { return s }\n", pkgName, funcName)
			if err := os.WriteFile(filepath.Join(pkgDir, "impl.go"), []byte(impl), 0644); err != nil {
				t.Fatal(err)
			}

			if r.IntN(100) < brokenPct {
				// Genuine type error (too many arguments) confined to
				// this one package's own test file -- matching gen_test.go's
				// exact real-world shape, not a synthetic/contrived error.
				badTest := fmt.Sprintf(
					"package %s\n\nimport \"testing\"\n\nfunc TestFunc%d_%d(t *testing.T) {\n\t%s(\"a\", \"b\", \"c\")\n}\n",
					pkgName, m, p, funcName)
				if err := os.WriteFile(filepath.Join(pkgDir, "impl_test.go"), []byte(badTest), 0644); err != nil {
					t.Fatal(err)
				}
			} else {
				healthy++
			}
		}
	}

	db := testDB(t)
	err := Ingest(db, dir)
	if healthy == 0 {
		if err == nil {
			t.Fatalf("expected Ingest to fail when all %d package(s) across %d module(s) are broken", total, numModules)
		}
		return
	}
	if err != nil {
		t.Fatalf("Ingest failed despite %d/%d healthy package(s) across %d module(s): %v", healthy, total, numModules, err)
	}

	defs, err := db.FindDefinitions("Func%")
	if err != nil {
		t.Fatalf("FindDefinitions: %v", err)
	}
	if len(defs) != healthy {
		names := make([]string, len(defs))
		for i, d := range defs {
			names[i] = d.Name
		}
		t.Fatalf("expected exactly %d ingested def(s) (one per healthy package) out of %d total package(s), got %d: %v",
			healthy, total, len(defs), names)
	}
}
