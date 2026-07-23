package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

// TestHandleGetDefinition_AutoDowngradesLargeBody locks in #184: a
// bare read on a def whose body exceeds readAutoOutlineThreshold
// returns the outline projection prefixed with an auto-downgrade
// note, not the full body. Escape hatches (full:true, mode:"body",
// query:non-empty) bypass the downgrade — verified in sibling tests.
func TestHandleGetDefinition_AutoDowngradesLargeBody(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0o755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0o644)
	// Fabricate a body > readAutoOutlineThreshold (1500 bytes).
	var body strings.Builder
	body.WriteString("package main\n\n// BigFunc has a body larger than the auto-outline threshold.\nfunc BigFunc(name string) string {\n\tresult := \"\"\n")
	for i := 0; i < 60; i++ {
		body.WriteString(fmt.Sprintf("\tresult += \"line %d: this is padding to push body past 1500 bytes\\n\"\n", i))
	}
	body.WriteString("\treturn result + name\n}\n")
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(body.String()), 0o644)
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal(err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal(err)
	}
	s := &server{backend: db}

	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "BigFunc"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Outline shown") {
		t.Errorf("expected outline-shown downgrade note, got: %s", text)
	}
	// #184.b: escape hatches (mode:"body", full:true) are documented in
	// the tool description, NOT advertised in the inline note — the
	// post-#184 bench showed the model reflexively retried when the
	// note enumerated them.
	if strings.Contains(text, `mode:"body"`) {
		t.Errorf("downgrade note must NOT advertise mode:\"body\" escape hatch (model retries); got: %s", text)
	}
	// Body content ("this is padding") must be absent — we returned outline.
	if strings.Contains(text, "this is padding") {
		t.Errorf("body leaked into auto-outline response; got: %s", text)
	}

	// Escape hatch #1: mode:"body" bypasses.
	result2, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "BigFunc", Mode: "body"})
	text2 := resultText(t, result2)
	if strings.Contains(text2, "Outline shown") {
		t.Errorf("mode:\"body\" should bypass auto-downgrade; got: %s", text2)
	}
	if !strings.Contains(text2, "this is padding") {
		t.Errorf("mode:\"body\" must return the full body; got: %s", text2)
	}

	// Escape hatch #2: full:true bypasses.
	result3, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "BigFunc", Full: true})
	text3 := resultText(t, result3)
	if strings.Contains(text3, "Outline shown") {
		t.Errorf("full:true should bypass auto-downgrade; got: %s", text3)
	}
	if !strings.Contains(text3, "this is padding") {
		t.Errorf("full:true must return the full body; got: %s", text3)
	}
}
