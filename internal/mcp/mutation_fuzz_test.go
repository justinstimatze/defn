package mcp

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/fuzzgen"
	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

// TestMutationSequence_Hazards runs a short, fixed-seed mutation
// sequence against each phase-1 hazard combination on every
// `go test ./...` -- the always-on floor. FuzzMutationSequence is the
// slower opt-in deep search over the same assertion.
func TestMutationSequence_Hazards(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3} {
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(seed, seed))
			synth := fuzzgen.Generate(r, fuzzgen.GenOpts{Hazards: fuzzgen.AllHazards})
			runMutationSequence(t, synth, seed, 6)
		})
	}
}

func assertBuildStillPasses(t *testing.T, projDir string) {
	t.Helper()
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed after a mutation defn reported as successful:\n%s", out)
	}
}

// assertNoDuplicateDecls is phase 2's standing invariant: after any
// sequence of mutations, no (file, kind, name, receiver) tuple should
// ever appear more than once -- this is the exact shape both the
// emitModule basename-collision bug (v0.26.23) and the ingest
// initCounter keying bug (v0.26.26) took, generalized to hold after
// arbitrary mutation sequences, not just static structural hazards.
func assertNoDuplicateDecls(t *testing.T, db store.Backend) {
	t.Helper()
	defs, err := db.FindDefinitions("%")
	if err != nil {
		t.Fatalf("find definitions: %v", err)
	}
	type key struct{ File, Kind, Name, Receiver string }
	seen := make(map[key]int, len(defs))
	for _, d := range defs {
		seen[key{d.SourceFile, d.Kind, d.Name, d.Receiver}]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Fatalf("duplicate declaration after mutation sequence: %s %s%s in %s appears %d times", k.Kind, k.Receiver, k.Name, k.File, n)
		}
	}
}

// insertNoOpStatement produces an identity-preserving body mutation:
// insert a harmless statement right after the opening brace, so the
// signature/name/receiver defn resolves this def by never changes (the
// #222 identity-preserving guard handleEdit itself enforces). Generic
// across every func/method body in the corpus -- no per-hazard-shape
// special-casing needed.
func insertNoOpStatement(body string) string {
	idx := strings.Index(body, "{")
	if idx < 0 {
		return body
	}
	return body[:idx+1] + "\n\t_ = 0" + body[idx+1:]
}

// pickMutation generates one real code(op:...) mutation targeting a
// known-live def (rename/edit/delete) or adding a new one (create).
// Returns the op, a label used to know how to update live[] afterward,
// and the live[] index it targets (-1 for create, which has none).
func pickMutation(r *rand.Rand, live []liveDef, seq int, db store.Backend) (codeParam, string, int) {
	choice := r.IntN(4)
	if len(live) == 0 {
		choice = 3 // create
	}
	switch choice {
	case 0: // rename
		idx := r.IntN(len(live))
		d := live[idx]
		newName := fmt.Sprintf("%sR%d", d.Name, seq)
		return codeParam{Op: "rename", OldName: d.Name, NewName: newName}, "rename", idx
	case 1: // delete
		idx := r.IntN(len(live))
		d := live[idx]
		return codeParam{Op: "delete", Name: d.Name, Receiver: d.Receiver, Force: true}, "delete", idx
	case 2: // edit
		idx := r.IntN(len(live))
		d := live[idx]
		body := "func " + d.Name + "() {\n\t_ = 0\n}\n"
		if def, err := db.GetDefinitionByName(d.Name, ""); err == nil {
			body = insertNoOpStatement(def.Body)
		}
		return codeParam{Op: "edit", Name: d.Name, Receiver: d.Receiver, NewBody: body}, "edit", idx
	default: // create
		name := fmt.Sprintf("FuzzGen%d", seq)
		file := fmt.Sprintf("extra/fuzz%d.go", seq)
		body := fmt.Sprintf("func %s() int { return %d }", name, seq)
		return codeParam{Op: "create", Body: body, File: file}, "create", -1
	}
}

// runMutationSequence is phase 2 of the differential fuzzer
// (docs/lessons-learned.md's standing plan): seed a real server+DB from
// a phase-1 SyntheticModule, then drive `steps` random rename/edit/
// delete/create ops through the SAME code(op:...) dispatch a real agent
// uses (handleCode, not the individual handlers directly -- this is
// deliberate: it's the dispatch layer itself that carried two of the
// bugs found building phase 1, so exercising it end-to-end is the point,
// not a shortcut around it). After every op defn reports as successful,
// independently re-verify the build (not trusting defn's own self-
// report) -- and at the end, assert no declaration was silently
// duplicated across the whole sequence.
func runMutationSequence(t *testing.T, synth *fuzzgen.SyntheticModule, seed uint64, steps int) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH")
	}

	dbDir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dbDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := t.TempDir()
	if err := synth.WriteTo(projDir); err != nil {
		t.Fatalf("write synthetic module: %v", err)
	}
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	live := seedLiveDefs(t, db)
	r := rand.New(rand.NewPCG(seed, seed))

	for i := 0; i < steps && len(live) > 0; i++ {
		op, label, idx := pickMutation(r, live, i, db)
		result, _, _ := s.handleCode(context.Background(), nil, op)
		text := resultText(t, result)
		// BUILD FAILED (without "rolled back") is force:true delete's
		// documented, intentional shape: force explicitly skips the
		// build-rollback gate, so a build failure is reported but the
		// mutation still commits. Treating this as a non-failure caused a
		// false positive: force-deleting the corpus's own main() reported
		// success by this check's original logic, and the independent
		// rebuild below correctly (but misleadingly) caught defn's own
		// documented behavior as if it were a bug.
		failed := result.IsError || strings.Contains(text, "rolled back") || strings.Contains(text, "BUILD FAILED")

		switch {
		case label == "rename" && !failed:
			live[idx].Name = op.NewName
		case label == "delete" && !failed:
			live = append(live[:idx], live[idx+1:]...)
		case label == "create" && !failed:
			live = append(live, liveDef{Name: fmt.Sprintf("FuzzGen%d", i), Kind: "function"})
		}

		if !failed {
			assertBuildStillPasses(t, projDir)
		}
	}

	assertNoDuplicateDecls(t, db)
}

func seedLiveDefs(t *testing.T, db store.Backend) []liveDef {
	t.Helper()
	defs, err := db.FindDefinitions("%")
	if err != nil {
		t.Fatalf("seed live defs: %v", err)
	}
	var live []liveDef
	for _, d := range defs {
		// "main" is excluded deliberately: a package main REQUIRES exactly
		// one func main(), so deleting/renaming it always breaks the build
		// by Go's own language rules regardless of defn's correctness --
		// not a signal worth generating. Found the hard way: a force:true
		// delete of main correctly skips defn's build-rollback gate (by
		// design -- force is an explicit "may break the build" escape
		// hatch) and reports "BUILD FAILED" without the "rolled back"
		// phrase, which the harness's failure detector didn't originally
		// recognize as an acknowledged failure.
		if d.Name == "main" {
			continue
		}
		if d.Kind == "function" || d.Kind == "method" {
			live = append(live, liveDef{Name: d.Name, Kind: d.Kind, Receiver: d.Receiver})
		}
	}
	return live
}

// liveDef tracks a function/method-kind def the mutation harness knows
// exists, so it can generate valid rename/edit/delete targets instead of
// guessing. Restricted to function/method kind -- renaming/editing/
// deleting a type or const under mutation risks legitimately breaking
// the build in ways unrelated to defn's own correctness (e.g. deleting a
// type out from under its own methods), which would make this a noisy,
// low-signal oracle. Phase 1's generator never has cross-def call
// dependencies among its hazard-generated funcs, so deleting any one of
// them is always safe to attempt.
type liveDef struct {
	Name, Kind, Receiver string
}

// FuzzMutationSequence is the slower, opt-in deep-search companion to
// TestMutationSequence_Hazards: `go test -run=FuzzMutationSequence
// -fuzz=FuzzMutationSequence -fuzztime=5m`. Corpus persists under
// testdata/fuzz/FuzzMutationSequence/ automatically on any crasher, same
// as FuzzRoundTrip.
func FuzzMutationSequence(f *testing.F) {
	f.Add(uint64(1), uint16(6))
	f.Add(uint64(2), uint16(10))
	f.Add(uint64(42), uint16(15))
	f.Fuzz(func(t *testing.T, seed uint64, stepsRaw uint16) {
		steps := int(stepsRaw%20) + 3 // bounded to 3-22 steps per sequence
		genR := rand.New(rand.NewPCG(seed, seed))
		synth := fuzzgen.Generate(genR, fuzzgen.GenOpts{Hazards: fuzzgen.AllHazards})
		runMutationSequence(t, synth, seed, steps)
	})
}
