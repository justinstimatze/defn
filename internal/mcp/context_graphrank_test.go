package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

// TestGatherContextCandidates_GraphRerankPromotesHubOverIsolatedTie is the
// context-op integration counterpart to internal/rank's graphrank unit
// tests: four candidates tie on contextRank's lexical score (all mention
// "widget" in their doc comment, none in their name), so the plain
// alphabetical tiebreak would rank Alpha first and Zulu (the hub, which
// calls both helpers) last. graphRerankContext must promote Zulu above the
// isolated Alpha once its neighbors' seed mass flows in.
func TestGatherContextCandidates_GraphRerankPromotesHubOverIsolatedTie(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

// Alpha does widget work in isolation.
func Alpha() {}

// Zulu does widget work with helpers.
func Zulu() {
	Helper1()
	Helper2()
}

// Helper1 does widget helper one.
func Helper1() {}

// Helper2 does widget helper two.
func Helper2() {}

func main() { Zulu(); Alpha() }
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	cands, _, err := s.gatherContextCandidates("widget")
	if err != nil {
		t.Fatalf("gatherContextCandidates: %v", err)
	}

	pos := make(map[string]int, len(cands))
	for i, c := range cands {
		pos[c.Def.Name] = i
	}
	alphaPos, aok := pos["Alpha"]
	zuluPos, zok := pos["Zulu"]
	if !aok || !zok {
		t.Fatalf("expected both Alpha and Zulu among the candidates, got %v", pos)
	}
	if zuluPos >= alphaPos {
		t.Fatalf("expected the hub (Zulu, connected to both helpers) to rank above the isolated tie-mate (Alpha) after graph rerank; positions: Zulu=%d Alpha=%d, full order=%v", zuluPos, alphaPos, pos)
	}
}
