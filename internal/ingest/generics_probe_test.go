package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIngestFunc_GenericReceiverStoresBareTypeName is the ingest-level
// regression: ingestFunc used to store Definition.Receiver via typeString,
// which renders a generic receiver's FULL bracketed syntax ("*Stack[T]")
// for a single type param, and has no case at all for *ast.IndexListExpr
// (2+ type params, e.g. Pair[K, V]) -- falling through to the literal
// string "*<unknown>". Every downstream consumer of Receiver (emit's
// recvTypeName-based identity matching, resolve.go's method lookups keyed
// off concrete.Obj().Name(), GetDefinitionByNameAndReceiver) already
// assumes a bare, unbracketed type name, so both shapes silently broke
// every receiver-qualified operation on a generic method. Confirmed live
// before the fix: Swap's receiver stored as literal "*<unknown>".
func TestIngestFunc_GenericReceiverStoresBareTypeName(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

type Stack[T any] struct {
	items []T
}

func (s *Stack[T]) Push(v T) {
	s.items = append(s.items, v)
}

type Pair[K comparable, V any] struct {
	Key   K
	Value V
}

func (p *Pair[K, V]) Swap() (V, K) {
	return p.Value, p.Key
}

func main() {}
`), 0644)

	db := testDB(t)
	if err := Ingest(db, dir); err != nil {
		t.Fatal("ingest:", err)
	}

	push, err := db.GetDefinitionByName("Push", "")
	if err != nil {
		t.Fatal(err)
	}
	if push.Receiver != "*Stack" {
		t.Errorf("expected Push receiver to be the bare %q, got %q", "*Stack", push.Receiver)
	}

	swap, err := db.GetDefinitionByName("Swap", "")
	if err != nil {
		t.Fatal(err)
	}
	if swap.Receiver != "*Pair" {
		t.Errorf("expected Swap receiver to be the bare %q, got %q", "*Pair", swap.Receiver)
	}

	// Both must also be independently findable via the receiver-qualified
	// lookup every write handler actually uses -- the storage bug this
	// guards against would make GetDefinitionByNameAndReceiver("Swap", "",
	// "*Pair") return nothing at all, since the stored key was "*<unknown>".
	if _, err := db.GetDefinitionByNameAndReceiver("Push", "", "*Stack"); err != nil {
		t.Errorf("GetDefinitionByNameAndReceiver(Push, *Stack) failed: %v", err)
	}
	if _, err := db.GetDefinitionByNameAndReceiver("Swap", "", "*Pair"); err != nil {
		t.Errorf("GetDefinitionByNameAndReceiver(Swap, *Pair) failed: %v", err)
	}
}

