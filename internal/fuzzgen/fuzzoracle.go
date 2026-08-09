package fuzzgen

import (
	"fmt"
	"sort"

	"github.com/justinstimatze/defn/internal/store"
)

// declMultiset builds the (SourceFile, Kind, Name, Receiver) -> count
// multiset for every definition currently in db. Count matters, not just
// presence: the ingest initCounter bug (v0.26.26) produced a genuine
// duplicate row under a name never seen before, which a plain existence
// check wouldn't catch but an exact-multiset comparison does.
func declMultiset(db store.Backend) (map[declKey]int, error) {
	defs, err := db.FindDefinitions("%")
	if err != nil {
		return nil, fmt.Errorf("find definitions: %w", err)
	}
	out := make(map[declKey]int, len(defs))
	for _, d := range defs {
		out[declKey{SourceFile: d.SourceFile, Kind: d.Kind, Name: d.Name, Receiver: d.Receiver}]++
	}
	return out, nil
}

// diffMultisets returns a sorted, human-readable description of every key
// whose count differs between before and after, or "" if they're
// identical.
func diffMultisets(before, after map[declKey]int) string {
	seen := make(map[declKey]bool, len(before)+len(after))
	for k := range before {
		seen[k] = true
	}
	for k := range after {
		seen[k] = true
	}
	var diffs []string
	for k := range seen {
		b, a := before[k], after[k]
		if b != a {
			diffs = append(diffs, fmt.Sprintf("%s %s.%s in %s: before=%d after=%d", k.Kind, k.Receiver, k.Name, k.SourceFile, b, a))
		}
	}
	sort.Strings(diffs)
	out := ""
	for i, l := range diffs {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// declKey identifies a declaration for round-trip comparison. SourceFile
// is part of the key deliberately -- both known historical bugs
// manifested as a declaration surviving under the right (kind, name) but
// landing in the wrong file (or vanishing from its own), which a key
// without SourceFile would miss entirely.
type declKey struct {
	SourceFile string
	Kind       string
	Name       string
	Receiver   string
}
