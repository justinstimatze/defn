package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHandleInsert_BuildFailureRollsBackBothDBAndFile
func TestHandleInsert_BuildFailureRollsBackBothDBAndFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc Greet() string {\n\treturn \"hi\"\n}\n\nfunc main() {}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:    "insert",
		Name:  "Greet",
		After: "return \"hi\"",
		Body:  "\n\tundefinedFunctionCall()",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the insert to be refused/rolled back, got: %s", text)
	}

	after, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != before.Body {
		t.Errorf("DB was mutated despite a rolled-back insert:\nbefore: %s\nafter:  %s", before.Body, after.Body)
	}

	src, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "undefinedFunctionCall") {
		t.Errorf("main.go was left with the rolled-back insert on disk:\n%s", src)
	}
}

// TestHandlePatch_BuildFailureRollsBackBothDBAndFile
func TestHandlePatch_BuildFailureRollsBackBothDBAndFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc Greet() string {\n\treturn \"hi\"\n}\n\nfunc main() {}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:      "patch",
		Name:    "Greet",
		OldName: "\"hi\"",
		NewName: "undefinedIdent",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the patch to be refused/rolled back, got: %s", text)
	}

	after, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != before.Body {
		t.Errorf("DB was mutated despite a rolled-back patch:\nbefore: %s\nafter:  %s", before.Body, after.Body)
	}

	src, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "undefinedIdent") {
		t.Errorf("main.go was left with the rolled-back patch on disk:\n%s", src)
	}
}

// TestHandleRetargetFieldValue_DriftWarningRollsBackBothDBAndFile covers
// the #12-class gap for a failure mode this handler can actually
// produce on its own: retarget only ever writes a quoted string
// literal, so it can't break a build via undefined-identifier content
// the way insert/patch can -- but it CAN still trip emit's #218
// drift-detection safety net (the DB's def no longer matches its
// on-disk counterpart), which commitOrRollbackOnBuild treats as a
// failure exactly like a real build error.
func TestHandleRetargetFieldValue_DriftWarningRollsBackBothDBAndFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "claims.go"), []byte(`package main

type Claim struct {
	Subject string
	Object  string
}

var C1 = Claim{Subject: "s1", Object: "OldTarget"}

func main() {}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := db.GetDefinitionByName("C1", "")
	if err != nil {
		t.Fatal(err)
	}

	// Drift the on-disk file out from under the DB without telling
	// defn (renaming C1 -- the exact declaration retarget will try to
	// splice into): the DB still thinks the var is named C1, disk no
	// longer has a C1 decl at all.
	os.WriteFile(filepath.Join(projDir, "claims.go"), []byte(`package main

type Claim struct {
	Subject string
	Object  string
}

var C1Renamed = Claim{Subject: "s1", Object: "OldTarget"}

func main() {}
`), 0644)

	// Call the handler directly rather than through handleCode: handleCode
	// now runs ensureFresh (the auto-freshness gate, internal/mcp/
	// freshness.go) before every op, which would re-ingest claims.go and
	// heal exactly the drift this test manually induces above -- correctly
	// so; that's the gate doing its job. This test's actual target is
	// deeper: handleRetargetFieldValue's OWN #218 emit-time drift check,
	// the safety net for drift the gate can't see (e.g. a change landing
	// between the gate's probe and this call's own emit). Bypassing
	// handleCode isolates that from the newer, coarser gate.
	result, _, _ := s.handleRetargetFieldValue(context.Background(), nil, codeParam{
		Op:    "retarget-field-value",
		Name:  "Claim",
		Field: "Object",
		Old:   "OldTarget",
		New:   "NewTarget",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the drift to be caught and rolled back, got: %s", text)
	}

	after, err := db.GetDefinitionByName("C1", "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != before.Body {
		t.Errorf("DB was mutated despite a rolled-back retarget:\nbefore: %s\nafter:  %s", before.Body, after.Body)
	}
}

// TestHandleCode_RetargetFieldValueFailureStillInvalidatesCache restores
// the handleCode-path coverage TestHandleRetargetFieldValue_
// DriftWarningRollsBackBothDBAndFile lost when that test was switched to
// call handleRetargetFieldValue directly (to isolate it from the newer
// auto-freshness gate, which now legitimately heals the drift that test
// induces before the handler ever sees it -- see that test's own comment).
//
// This test covers the OTHER thing that was worth keeping: handleCode's
// write-op defer must invalidate the session's respCache even when
// retarget-field-value FAILS, not just on success (the #245 fix this
// package's own comments describe -- invalidation must not be gated on
// !result.IsError). Deliberately triggers the failure via a missing
// required param (field is required) rather than drift/build machinery:
// that failure path is unconditional and has zero interaction with
// ensureFresh, so it stays robust regardless of how the freshness gate
// evolves. Reads/retargets the SAME name ("Greet") -- writeTargets scopes
// retarget-field-value's invalidation to args.Name (the struct type name),
// so checking a name unrelated to the retarget call would test nothing;
// the value in args.Name doesn't need to be a real struct type here since
// the missing-field validation fails before any type lookup happens.
func TestHandleCode_RetargetFieldValueFailureStillInvalidatesCache(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
	s.ready.Store(true)
	req := &sdkmcp.CallToolRequest{Session: &sdkmcp.ServerSession{}}
	ctx := context.Background()

	first, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "Greet", Full: true})
	if err != nil {
		t.Fatalf("initial full read: %v", err)
	}
	if !strings.Contains(resultText(t, first), "Hello, ") {
		t.Fatalf("expected the original body, got: %s", resultText(t, first))
	}

	// Field is deliberately omitted -- handleRetargetFieldValue rejects
	// this immediately, before touching the DB/build machinery at all.
	failResult, _, _ := s.handleCode(ctx, req, codeParam{
		Op: "retarget-field-value", Name: "Greet", Old: "a", New: "b",
	})
	if failResult == nil || !failResult.IsError {
		t.Fatalf("expected retarget-field-value with no field to fail, got: %+v", failResult)
	}

	second, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "Greet"})
	if err != nil {
		t.Fatalf("re-read after failed retarget: %v", err)
	}
	text := resultText(t, second)
	if strings.Contains(text, "already read") || strings.Contains(text, "hasn't changed") {
		t.Fatalf("expected the FAILED retarget-field-value call to still invalidate respCache, got: %s", text)
	}
}

// TestHandleCode_RetargetFieldValueSuccessInvalidatesTouchedDefsCache is
// the regression for a real pre-existing bug found while restoring
// coverage for this op: writeTargets used to scope retarget-field-value's
// cache invalidation to args.Name -- the struct TYPE name ("Claim") -- not
// any def actually rewritten (e.g. "C1"). A session that read C1 via
// full:true, then ran a SUCCESSFUL retarget that changed C1's body, would
// keep serving C1's stale pre-retarget cached body afterward, because
// "Claim" (not "C1") was the only name ever invalidated. writeTargets now
// reports retarget-field-value's blast radius as undeterminable, falling
// back to a full session invalidate, same as rename already does for the
// same class of problem.
func TestHandleCode_RetargetFieldValueSuccessInvalidatesTouchedDefsCache(t *testing.T) {
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
	os.WriteFile(filepath.Join(projDir, "claims.go"), []byte(`package main

type Claim struct {
	Subject string
	Object  string
}

var C1 = Claim{Subject: "s1", Object: "OldTarget"}

func main() {}
`), 0644)
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}
	s := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
	s.ready.Store(true)
	req := &sdkmcp.CallToolRequest{Session: &sdkmcp.ServerSession{}}
	ctx := context.Background()

	first, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "C1", Full: true})
	if err != nil {
		t.Fatalf("initial full read: %v", err)
	}
	if !strings.Contains(resultText(t, first), "OldTarget") {
		t.Fatalf("expected the original body, got: %s", resultText(t, first))
	}

	retargetResult, _, _ := s.handleCode(ctx, req, codeParam{
		Op: "retarget-field-value", Name: "Claim", Field: "Object", Old: "OldTarget", New: "NewTarget",
	})
	if retargetResult == nil || retargetResult.IsError {
		t.Fatalf("expected retarget to succeed, got: %+v", retargetResult)
	}

	second, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "C1"})
	if err != nil {
		t.Fatalf("re-read after retarget: %v", err)
	}
	text := resultText(t, second)
	if strings.Contains(text, "already read") || strings.Contains(text, "hasn't changed") {
		t.Fatalf("expected the retarget to invalidate C1's cache, got: %s", text)
	}
	if !strings.Contains(text, "NewTarget") {
		t.Errorf("expected the re-read to show the retargeted body (\"NewTarget\"), got: %s", text)
	}
}
