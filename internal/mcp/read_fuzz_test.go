package mcp

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/fuzzgen"
	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

// FuzzReadOpsNeverMutate is the slower, opt-in deep-search companion to
// TestReadOpsNeverMutate_Hazards: `go test -run=FuzzReadOpsNeverMutate
// -fuzz=FuzzReadOpsNeverMutate -fuzztime=5m`. Corpus persists under
// testdata/fuzz/FuzzReadOpsNeverMutate/ automatically on any crasher,
// same as FuzzMutationSequence.
func FuzzReadOpsNeverMutate(f *testing.F) {
	f.Add(uint64(1), uint16(6))
	f.Add(uint64(2), uint16(10))
	f.Add(uint64(42), uint16(15))
	f.Fuzz(func(t *testing.T, seed uint64, stepsRaw uint16) {
		steps := int(stepsRaw%20) + 3 // bounded to 3-22 steps per sequence
		genR := rand.New(rand.NewPCG(seed, seed))
		synth := fuzzgen.Generate(genR, fuzzgen.GenOpts{Hazards: fuzzgen.AllHazards})
		runReadOpsSequence(t, synth, seed, steps)
	})
}

// TestReadOpsNeverMutate_Hazards runs a short, fixed-seed read-op
// sequence against each phase-1 hazard combination on every
// `go test ./...` -- the always-on floor, read-side companion to
// TestMutationSequence_Hazards. FuzzReadOpsNeverMutate is the slower
// opt-in deep search over the same assertion.
func TestReadOpsNeverMutate_Hazards(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3} {
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(seed, seed))
			synth := fuzzgen.Generate(r, fuzzgen.GenOpts{Hazards: fuzzgen.AllHazards})
			runReadOpsSequence(t, synth, seed, 12)
		})
	}
}

// pickReadOp generates one random read-shaped code(op:...) call -- the
// nameableReadOps set (read/outline/impact/methods/expand) plus
// search, targeting either a real def name (the realistic case) or a
// garbage string (the adversarial case).
func pickReadOp(r *rand.Rand, live []liveDef, seq int) codeParam {
	ops := []string{"read", "outline", "impact", "methods", "expand", "search"}
	op := ops[r.IntN(len(ops))]

	var name string
	if len(live) > 0 && r.IntN(2) == 0 {
		name = live[r.IntN(len(live))].Name
	} else {
		name = randGarbageName(r, seq)
	}

	switch op {
	case "search":
		return codeParam{Op: "search", Pattern: name}
	case "expand":
		return codeParam{Op: "expand", Names: []string{name}}
	default:
		return codeParam{Op: op, Name: name}
	}
}

// randGarbageName produces adversarial name/pattern input a confused or
// malicious caller might send: empty, whitespace-only, SQL/LIKE
// metacharacters, a literal SQL-injection attempt, path separators, a
// very long string, non-ASCII, control bytes, and Go's own
// qualified-name/dotted syntax with no real target behind it.
func randGarbageName(r *rand.Rand, seq int) string {
	garbage := []string{
		"", " ", "\t\n", "%", "%%", "'; DROP TABLE definitions; --",
		"../../../etc/passwd", "a/b/c.go", "pkg.Symbol", "*Receiver",
		strings.Repeat("x", 5000), "日本語識別子", "\x00\x01\x02",
		"[unclosed", "a|b|c", fmt.Sprintf("NoSuchDef%d", seq),
	}
	return garbage[r.IntN(len(garbage))]
}

// runReadOpsSequence is the read-side companion to runMutationSequence:
// seed a real server+DB from a phase-1 SyntheticModule, then drive
// `steps` random read-shaped ops (nameableReadOps -- read/outline/
// impact/methods/expand -- plus search, which the rest of the codebase
// already treats as read-only) through the SAME code(op:...) dispatch a
// real agent uses, with arguments drawn from a mix of real def names
// (the realistic case) and pure-garbage strings (the adversarial case:
// wildly wrong input a confused or adversarial caller might still
// send, including a literal SQL-injection attempt -- every store-layer
// query is supposed to be parameter-bound, and this is the harness
// that would actually catch it if one weren't). The standing
// invariant: a read op has no business mutating anything, on ANY
// input, valid or garbage -- checked by snapshotting every
// definition's identity+content fields before and after the whole
// sequence and asserting equality. Panics are caught for free by go
// test's own test/fuzz engine, the same way runMutationSequence gets
// panic detection for free.
func runReadOpsSequence(t *testing.T, synth *fuzzgen.SyntheticModule, seed uint64, steps int) {
	t.Helper()

	dbDir := t.TempDir()
	db, err := store.OpenBackend(dbDir + "/test.db")
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
	before := snapshotDefs(t, db)
	r := rand.New(rand.NewPCG(seed, seed))

	for i := 0; i < steps; i++ {
		op := pickReadOp(r, live, i)
		// Only a crash or a state mutation is a bug here -- the response
		// content itself (an error, a "not found", garbage-in-garbage-out)
		// is expected and deliberately not asserted on.
		_, _, _ = s.handleCode(context.Background(), nil, op)
	}

	after := snapshotDefs(t, db)
	if before != after {
		t.Fatalf("read-shaped op sequence mutated the DB -- a read op must never change state.\nbefore:\n%s\n\nafter:\n%s", before, after)
	}
}

// snapshotDefs renders every definition's identity+content fields as a
// stable, sorted string so two snapshots can be compared with a plain
// equality check -- byte-for-byte proof nothing changed under the read
// sequence.
func snapshotDefs(t *testing.T, db store.Backend) string {
	t.Helper()
	defs, err := db.FindDefinitions("%")
	if err != nil {
		t.Fatalf("snapshot defs: %v", err)
	}
	lines := make([]string, 0, len(defs))
	for _, d := range defs {
		full, err := db.GetDefinition(d.ID)
		if err != nil {
			t.Fatalf("snapshot def %d: %v", d.ID, err)
		}
		lines = append(lines, fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s|%s",
			full.ID, full.SourceFile, full.Kind, full.Name, full.Receiver,
			full.Signature, full.Doc, full.Body))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
