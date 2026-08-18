package resolve

import (
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/store"
)

func hasCaller(defs []store.Definition, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

func namesOf(defs []store.Definition) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Name
	}
	return out
}

// TestResolve_GenericMethodReceiverCollisionResolvesCorrectly is the
// regression for receiverName/lookupFuncDefID's bracket-inclusive
// receiver keys: two generic types sharing a same-named method in one
// package (Stack[T].Len, Queue[T].Len) used to have their callers
// silently merged onto a single def, because neither generic receiver
// string ever matched Definition.Receiver's bare-name storage, so both
// lookups fell through to a receiver-agnostic "first def named Len in
// this package" match. Confirmed live before the fix: (*Stack).Len
// picked up BOTH useStack and useQueue as callers while (*Queue).Len
// got none.
func TestResolve_GenericMethodReceiverCollisionResolvesCorrectly(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"main.go": `package extifacebug2

type Stack[T any] struct{ items []T }

func (s *Stack[T]) Len() int { return len(s.items) }

type Queue[T any] struct{ items []T }

func (q *Queue[T]) Len() int { return len(q.items) }

func useStack(s *Stack[int]) int { return s.Len() }

func useQueue(q *Queue[string]) int { return q.Len() }
`,
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	stackLen, err := db.GetDefinitionByNameAndReceiver("Len", "", "*Stack")
	if err != nil {
		t.Fatalf("lookup (*Stack).Len: %v", err)
	}
	queueLen, err := db.GetDefinitionByNameAndReceiver("Len", "", "*Queue")
	if err != nil {
		t.Fatalf("lookup (*Queue).Len: %v", err)
	}

	stackCallers, err := db.GetCallers(stackLen.ID)
	if err != nil {
		t.Fatal(err)
	}
	queueCallers, err := db.GetCallers(queueLen.ID)
	if err != nil {
		t.Fatal(err)
	}

	if !hasCaller(stackCallers, "useStack") {
		t.Errorf("useStack is not recorded as a caller of (*Stack).Len: %v", namesOf(stackCallers))
	}
	if hasCaller(stackCallers, "useQueue") {
		t.Errorf("useQueue is WRONGLY recorded as a caller of (*Stack).Len (receiver collision): %v", namesOf(stackCallers))
	}
	if !hasCaller(queueCallers, "useQueue") {
		t.Errorf("useQueue is not recorded as a caller of (*Queue).Len: %v", namesOf(queueCallers))
	}
	if hasCaller(queueCallers, "useStack") {
		t.Errorf("useStack is WRONGLY recorded as a caller of (*Queue).Len (receiver collision): %v", namesOf(queueCallers))
	}
}
