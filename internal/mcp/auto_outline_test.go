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

// TestHandleGetDefinition_LineRangeNarrowsBody mirrors the exact shape
// that motivated the line_range param: a real trajectory (prometheus-
// 18712 mining) reached for made-up "pick"/"slice"/"line" params trying
// to narrow a read of a large multi-hundred-line function down to a
// specific range, and those params were silently ignored (no such
// fields existed). This locks in that line_range, once it exists,
// actually narrows the returned body to the requested file-relative
// range, bypasses the #184 auto-outline-downgrade the same way full:
// true does, and correctly accounts for the def's leading doc comment
// when converting file-relative lines to a body-relative slice.
func TestHandleGetDefinition_LineRangeNarrowsBody(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0o755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0o644)
	// Line 1: package main
	// Line 2: (blank)
	// Line 3: // BigFunc doc comment (one line)
	// Line 4: func BigFunc(name string) string {
	// Line 5: result := ""
	// Lines 6-65: 60 padding lines, "line %d: ..." for i in 0..59
	// Line 66: return result + name
	// Line 67: }
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

	// File lines 10-12 correspond to padding indices 4, 5, 6 (line 6 == i=0).
	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "BigFunc", LineRange: "10-12"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := resultText(t, result)

	if strings.Contains(text, "Outline shown") {
		t.Errorf("line_range must bypass the #184 auto-outline-downgrade; got: %s", text)
	}
	if !strings.Contains(text, "line_range read") {
		t.Errorf("expected a line_range hint header; got: %s", text)
	}
	for _, want := range []string{"line 4:", "line 5:", "line 6:"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected requested range to contain %q; got: %s", want, text)
		}
	}
	for _, unwanted := range []string{"line 0:", "line 59:", "return result + name", "func BigFunc"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("line_range leaked content outside the requested range (%q); got: %s", unwanted, text)
		}
	}

	// Also accept the "start:end" separator form.
	result2, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "BigFunc", LineRange: "10:12"})
	if err != nil {
		t.Fatalf("read with ':' separator: %v", err)
	}
	text2 := resultText(t, result2)
	if !strings.Contains(text2, "line 4:") {
		t.Errorf("':' separator form should behave identically; got: %s", text2)
	}
}

// TestHandleGetDefinition_FullReadTruncatesBodyOverHardCap is the
// regression for #334: a real prometheus-18712 trajectory read a
// 1262-line/~51KB main() via full:true and the response overflowed
// Claude Code CLI's own oversized-tool-result handling, silently
// redirecting to a persisted-output file the model then had to
// blindly grep ~15 times to find the ~20 lines it actually needed.
// An explicit full:true/mode:"body" request for a body over
// readFullBodyHardCap must now be truncated on defn's own terms, with
// a note naming the real size and pointing at query-adaptive read.
func TestHandleGetDefinition_FullReadTruncatesBodyOverHardCap(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	mod, _ := db.EnsureModule("example.com/svc", "svc", "")
	var b strings.Builder
	b.WriteString("func Enormous() {\n")
	for i := 0; i < 800; i++ {
		fmt.Fprintf(&b, "\tfmt.Println(\"line %04d padding padding padding padding\")\n", i)
	}
	b.WriteString("}\n")
	body := b.String()
	if len(body) <= readFullBodyHardCap {
		t.Fatalf("fixture body (%d bytes) must exceed readFullBodyHardCap (%d) for this test to be meaningful", len(body), readFullBodyHardCap)
	}

	d := &store.Definition{
		ModuleID: mod.ID, Name: "Enormous", Kind: "function",
		Body: body, Signature: "func Enormous()",
	}
	d.Hash = store.HashBody(d.Body)
	if _, err := db.UpsertDefinition(d); err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Enormous", Full: true})
	text := resultText(t, result)

	if len(text) >= len(body) {
		t.Errorf("expected a truncated response smaller than the original %d-byte body, got %d bytes total", len(body), len(text))
	}
	if !strings.Contains(text, "body truncated") {
		t.Errorf("expected a truncation note, got: %.500s...", text)
	}
	if !strings.Contains(text, "query:") {
		t.Errorf("expected the truncation note to point at query-adaptive read, got: %.500s...", text)
	}
	if !strings.Contains(text, "line 0000") {
		t.Errorf("expected the START of the body to still be present (truncation keeps the head), got: %.300s...", text)
	}
	if strings.Contains(text, "line 0799") {
		t.Errorf("expected the END of the body to be truncated away, but found the last line")
	}
}

// TestHandleGetDefinition_QueryFilterOnHugeBodyAvoidsTruncation
// confirms the #334 hard-cap check runs AFTER query filtering: when a
// query narrows a huge body down to just the matching statements, the
// result should be small enough to skip the truncation note entirely
// -- query-adaptive read is the intended way out of the cap, not
// something it should fight with.
func TestHandleGetDefinition_QueryFilterOnHugeBodyAvoidsTruncation(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	mod, _ := db.EnsureModule("example.com/svc", "svc", "")
	var b strings.Builder
	b.WriteString("func Enormous() {\n")
	for i := 0; i < 800; i++ {
		fmt.Fprintf(&b, "\tfmt.Println(\"line %04d padding padding padding padding\")\n", i)
	}
	b.WriteString("\tfmt.Println(\"the needle statement\")\n")
	b.WriteString("}\n")
	body := b.String()
	if len(body) <= readFullBodyHardCap {
		t.Fatalf("fixture body (%d bytes) must exceed readFullBodyHardCap (%d)", len(body), readFullBodyHardCap)
	}

	d := &store.Definition{
		ModuleID: mod.ID, Name: "Enormous", Kind: "function",
		Body: body, Signature: "func Enormous()",
	}
	d.Hash = store.HashBody(d.Body)
	if _, err := db.UpsertDefinition(d); err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Enormous", Query: "needle"})
	text := resultText(t, result)

	if strings.Contains(text, "body truncated") {
		t.Errorf("query filtering should have shrunk the body below the hard cap, no truncation note expected, got: %.300s...", text)
	}
	if !strings.Contains(text, "the needle statement") {
		t.Errorf("expected the query-matched statement to survive filtering, got: %.300s...", text)
	}
}
