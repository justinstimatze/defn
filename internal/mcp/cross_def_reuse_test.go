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
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// chunkyFixtureBody is #176's test fixture: a function large enough to
// trip outlineBodyThreshold (so outline doesn't fall back to read for
// being too small) reused across this file's tests.
const chunkyFixtureBody = `package main

// Chunky processes items with a mix of control-flow shapes so the
// outline op's flow detection has something interesting to report.
// Body is padded past outlineBodyThreshold via repeated statements.
func Chunky(items []string) (int, error) {
	total := 0
	for _, item := range items {
		if item == "" {
			continue
		}
		total++
	}
	if total == 0 {
		return 0, nil
	}
	defer func() {
		total = 0
	}()
	go func() {
		process(items)
	}()
	select {
	case <-done:
	}
	return total, nil
}

func process(_ []string) {}

var done = make(chan struct{})

func main() {}
`

// TestHandleCode_OutlineNotSuppressedAfterPlainRead covers the
// asymmetry: a plain read() (no full:true) can be silently downgraded
// to summary or auto-outline mode (#174/#184), so it must NOT count as
// "the caller has the full body" -- only an explicit full:true does.
func TestHandleCode_OutlineNotSuppressedAfterPlainRead(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky"}); err != nil {
		t.Fatalf("read: %v", err)
	}

	outlineResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "outline", Name: "Chunky"})
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	outlineText := resultText(t, outlineResult)
	if strings.Contains(outlineText, "already read") {
		t.Errorf("plain read (no full:true) should not suppress a later outline, got:\n%s", outlineText)
	}
}

// TestHandleCode_OutlineNotSuppressedWithoutPriorFullRead is the
// control: outline() with no prior full read behaves exactly as
// before -- a real outline, not a stub.
func TestHandleCode_OutlineNotSuppressedWithoutPriorFullRead(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	outlineResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "outline", Name: "Chunky"})
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	outlineText := resultText(t, outlineResult)
	if strings.Contains(outlineText, "already read") {
		t.Errorf("expected a real outline with no prior full read, got suppression stub:\n%s", outlineText)
	}
	if !strings.Contains(outlineText, "Callees") {
		t.Errorf("expected a real outline body, got:\n%s", outlineText)
	}
}

// TestHandleCode_OutlineQueryBypassesSuppression covers the other
// carve-out: a query-filtered outline highlights different callees
// than a plain one, so it is not redundant even with the full body
// already in hand -- suppression must not fire when args.Query is set.
func TestHandleCode_OutlineQueryBypassesSuppression(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky", Full: true}); err != nil {
		t.Fatalf("read full:true: %v", err)
	}

	outlineResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "outline", Name: "Chunky", Query: "process"})
	if err != nil {
		t.Fatalf("outline with query: %v", err)
	}
	outlineText := resultText(t, outlineResult)
	if strings.Contains(outlineText, "already read") {
		t.Errorf("query-filtered outline should not be suppressed, got:\n%s", outlineText)
	}
}

// TestHandleCode_OutlineSuppressedAfterFullBodyRead is the #176
// regression: outline() on a def whose full body was already read
// (full:true) this session should return a compact stub instead of
// re-deriving the outline, since outline's info is a strict subset of
// what a full body read already carries.
func TestHandleCode_OutlineSuppressedAfterFullBodyRead(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	readResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky", Full: true})
	if err != nil {
		t.Fatalf("read full:true: %v", err)
	}
	readText := resultText(t, readResult)
	if !strings.Contains(readText, "total++") {
		t.Fatalf("expected full body in read response, got:\n%s", readText)
	}

	outlineResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "outline", Name: "Chunky"})
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	outlineText := resultText(t, outlineResult)
	if !strings.Contains(outlineText, "already read") {
		t.Errorf("expected suppression stub mentioning prior full read, got:\n%s", outlineText)
	}
	if strings.Contains(outlineText, "Callees") {
		t.Errorf("expected outline to be suppressed (no real outline body), got:\n%s", outlineText)
	}
}

// TestHandleCode_OutlineSuppressionClearsAfterWrite confirms the
// suppression state is invalidated when a write op actually touches the
// def in question -- otherwise a stale "already read" stub could hide a
// def that has since changed shape. (2026-08-04: invalidation is now
// scoped to the write's determined blast radius rather than the whole
// session -- see writeTargets/invalidateNames -- so this uses a write
// that names Chunky directly. See
// TestHandleCode_OutlineSuppressionSurvivesUnrelatedWrite for the flip
// side: a write that does NOT touch Chunky must NOT clear it.)
func TestHandleCode_OutlineSuppressionClearsAfterWrite(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky", Full: true}); err != nil {
		t.Fatalf("read full:true: %v", err)
	}

	editedBody := "func Chunky(items []string) (int, error) {\n\ttotal := 0\n\t// edited\n\tfor _, item := range items {\n\t\tif item == \"\" {\n\t\t\tcontinue\n\t\t}\n\t\ttotal++\n\t}\n\tif total == 0 {\n\t\treturn 0, nil\n\t}\n\tdefer func() {\n\t\ttotal = 0\n\t}()\n\tgo func() {\n\t\tprocess(items)\n\t}()\n\tselect {\n\tcase <-done:\n\t}\n\treturn total, nil\n}"
	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "edit", Name: "Chunky", NewBody: editedBody}); err != nil {
		t.Fatalf("edit Chunky: %v", err)
	}

	outlineResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "outline", Name: "Chunky"})
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	outlineText := resultText(t, outlineResult)
	if strings.Contains(outlineText, "already read") {
		t.Errorf("expected suppression state to be cleared after a write that touches Chunky, got:\n%s", outlineText)
	}
}

func setupCrossDefReuseServer(t *testing.T) (*server, *sdkmcp.CallToolRequest) {
	t.Helper()
	db, projDir := setupTestDB(t)
	t.Cleanup(func() { db.Close() })

	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(chunkyFixtureBody), 0644)
	os.Remove(filepath.Join(projDir, "main_test.go"))
	if _, err := ingest.IngestFile(db, projDir, filepath.Join(projDir, "main.go")); err != nil {
		t.Fatal("re-ingest:", err)
	}

	s := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
	s.ready.Store(true)
	req := &sdkmcp.CallToolRequest{Session: &sdkmcp.ServerSession{}}
	return s, req
}

// TestHandleCode_EditDoesNotInvalidateUnrelatedDefsCache is the
// end-to-end proof of the invalidate-scoping fix (2026-08-04): editing
// one def must not erase the dedup cache entry for a completely
// unrelated def read earlier in the same session.
func TestHandleCode_EditDoesNotInvalidateUnrelatedDefsCache(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)
	ctx := context.Background()

	if _, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "Chunky"}); err != nil {
		t.Fatalf("first read Chunky: %v", err)
	}

	if _, _, err := s.handleCode(ctx, req, codeParam{Op: "edit", Name: "process", NewBody: "func process(_ []string) {\n\t// touched\n}"}); err != nil {
		t.Fatalf("edit process: %v", err)
	}

	result, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "Chunky"})
	if err != nil {
		t.Fatalf("second read Chunky: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "cached") {
		t.Errorf("editing an unrelated def (process) should not invalidate Chunky's dedup cache; got full content instead of the cached stub:\n%s", text)
	}
}

// TestHandleCode_OutlineSuppressionSurvivesUnrelatedWrite is the flip
// side of TestHandleCode_OutlineSuppressionClearsAfterWrite: a write
// whose determined blast radius doesn't include Chunky (add-import on
// its file, which doesn't change Chunky's body or signature) must NOT
// clear Chunky's suppression state -- matching the scoped invalidation
// contract (writeTargets/invalidateNames, 2026-08-04).
func TestHandleCode_OutlineSuppressionSurvivesUnrelatedWrite(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky", Full: true}); err != nil {
		t.Fatalf("read full:true: %v", err)
	}

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "add-import", File: "main.go", ImportPath: "errors"}); err != nil {
		t.Fatalf("add-import: %v", err)
	}

	outlineResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "outline", Name: "Chunky"})
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	outlineText := resultText(t, outlineResult)
	if !strings.Contains(outlineText, "already read") {
		t.Errorf("expected suppression state to SURVIVE an unrelated write (add-import), got:\n%s", outlineText)
	}
}

// TestHandleCode_PlainReadSuppressedAfterFullBodyRead generalizes #176:
// a plain read() (no full:true) on a def whose full body was already
// read this session should return a compact stub instead of
// re-transmitting the same content, since a plain read's best case IS
// that same full body (2026-08-04).
func TestHandleCode_PlainReadSuppressedAfterFullBodyRead(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky", Full: true}); err != nil {
		t.Fatalf("read full:true: %v", err)
	}

	readResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky"})
	if err != nil {
		t.Fatalf("plain read: %v", err)
	}
	readText := resultText(t, readResult)
	if !strings.Contains(readText, "already read") {
		t.Errorf("expected suppression stub mentioning prior full read, got:\n%s", readText)
	}
	if strings.Contains(readText, "total++") {
		t.Errorf("expected plain read to be suppressed (no re-transmitted body), got:\n%s", readText)
	}
}

// TestHandleCode_SliceSuppressedAfterFullBodyRead generalizes #176: any
// slice() on a def whose full body was already read this session
// should return a compact stub, since a slice is always a strict
// subset of the full body (2026-08-04).
func TestHandleCode_SliceSuppressedAfterFullBodyRead(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky", Full: true}); err != nil {
		t.Fatalf("read full:true: %v", err)
	}

	sliceResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "slice", Name: "Chunky", Slice: "body"})
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	sliceText := resultText(t, sliceResult)
	if !strings.Contains(sliceText, "already read") {
		t.Errorf("expected suppression stub mentioning prior full read, got:\n%s", sliceText)
	}
	if strings.Contains(sliceText, "total++") {
		t.Errorf("expected slice to be suppressed (no re-transmitted body), got:\n%s", sliceText)
	}
}

// TestHandleCode_OutlineRedirectsToExpandWhenBodyServeIsStale is #227's
// fix, verified against the real failure it was built to close: once a
// prior full-body serve has survived more than staleEpochThreshold
// compactions, don't bet a bare "you already have this" stub on it --
// redirect to the richer expand bundle instead, which is guaranteed to
// contain the real content rather than a claim that might be wrong.
func TestHandleCode_OutlineRedirectsToExpandWhenBodyServeIsStale(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky", Full: true}); err != nil {
		t.Fatalf("read full:true: %v", err)
	}

	s.respCache.mu.Lock()
	s.respCache.getSession(req.Session).compactionEpoch = staleEpochThreshold + 1
	s.respCache.mu.Unlock()

	result, _, err := s.handleCode(context.Background(), req, codeParam{Op: "outline", Name: "Chunky"})
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "already read") {
		t.Errorf("expected a real expand bundle, not the suppression stub, got:\n%s", text)
	}
	if !strings.Contains(text, "total++") {
		t.Errorf("expected the expand redirect to include the actual body, got:\n%s", text)
	}
}

func TestHandleCode_ReadRedirectsToExpandWhenBodyServeIsStale(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky", Full: true}); err != nil {
		t.Fatalf("read full:true: %v", err)
	}

	s.respCache.mu.Lock()
	s.respCache.getSession(req.Session).compactionEpoch = staleEpochThreshold + 1
	s.respCache.mu.Unlock()

	result, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky"})
	if err != nil {
		t.Fatalf("plain read: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "already read") {
		t.Errorf("expected a real expand bundle, not the suppression stub, got:\n%s", text)
	}
	if !strings.Contains(text, "total++") {
		t.Errorf("expected the expand redirect to include the actual body, got:\n%s", text)
	}
}

func TestHandleCode_SliceRedirectsToExpandWhenBodyServeIsStale(t *testing.T) {
	s, req := setupCrossDefReuseServer(t)

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Chunky", Full: true}); err != nil {
		t.Fatalf("read full:true: %v", err)
	}

	s.respCache.mu.Lock()
	s.respCache.getSession(req.Session).compactionEpoch = staleEpochThreshold + 1
	s.respCache.mu.Unlock()

	result, _, err := s.handleCode(context.Background(), req, codeParam{Op: "slice", Name: "Chunky", Slice: "body"})
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "already read") {
		t.Errorf("expected a real expand bundle, not the suppression stub, got:\n%s", text)
	}
	if !strings.Contains(text, "total++") {
		t.Errorf("expected the expand redirect to include the actual body, got:\n%s", text)
	}
}

// TestHandleCode_RenameInvalidatesCallerBodyServedState is the
// regression for writeTargets' rename case under-scoping invalidation:
// handleRename rewrites every caller's body (astRename +
// UpsertDefinition) via tx.GetCallers, but writeTargets only ever
// returned {OldName, NewName} -- the caller's own name was never part
// of the scoped invalidate. Since the bodyServed short-circuit isn't
// hash-gated, a caller's full body served BEFORE a rename, then read
// again (without full:true) AFTER a rename that rewrote that caller's
// source, used to return the "already read... nothing new" stub even
// though the caller's actual source text just changed to use the new
// callee name.
func TestHandleCode_RenameInvalidatesCallerBodyServedState(t *testing.T) {
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
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

func Helper() int { return 1 }

func Caller() int { return Helper() }

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
	req := &sdkmcp.CallToolRequest{}

	// Establish bodyServed state for Caller via a full-body read.
	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Caller", Full: true}); err != nil {
		t.Fatalf("read Caller full:true: %v", err)
	}

	renameResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "rename", OldName: "Helper", NewName: "HelperX"})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	renameText := resultText(t, renameResult)
	if strings.Contains(renameText, "rolled back") {
		t.Fatalf("rename was refused: %s", renameText)
	}

	readResult, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Caller"})
	if err != nil {
		t.Fatalf("read Caller after rename: %v", err)
	}
	readText := resultText(t, readResult)
	if strings.Contains(readText, "already read") {
		t.Fatalf("stale bodyServed stub served after a rename that rewrote Caller's own body: %s", readText)
	}
	if !strings.Contains(readText, "HelperX") {
		t.Errorf("expected Caller's post-rename body (calling HelperX) to actually be returned, got: %s", readText)
	}
}

func TestHandleCode_ReadDowngradeTrackingInvalidatedByEdit(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0o755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0o644)
	bigBody := func(marker string) string {
		var body strings.Builder
		body.WriteString(fmt.Sprintf("package main\n\nfunc BigFunc(name string) string {\n\tresult := \"%s\"\n", marker))
		for i := 0; i < 60; i++ {
			body.WriteString(fmt.Sprintf("\tresult += \"line %d: padding to push body past 1500 bytes\\n\"\n", i))
		}
		body.WriteString("\treturn result + name\n}\n")
		return body.String()
	}
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(bigBody("v1")), 0o644)
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal(err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal(err)
	}
	s := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
	s.ready.Store(true)
	sess := &sdkmcp.ServerSession{}
	req := &sdkmcp.CallToolRequest{Session: sess}

	first, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "BigFunc"})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if !strings.Contains(resultText(t, first), "Outline shown") {
		t.Fatalf("expected first bare read to auto-downgrade to outline, got: %s", resultText(t, first))
	}

	// Edit to a DIFFERENT body that is STILL large (>1500 bytes) --
	// this isolates the #313 tracking-invalidation question from the
	// unrelated "body just got small" case: if invalidation did NOT
	// clear readDowngraded, this next read would wrongly skip straight
	// to full body (treating a fresh, never-yet-downgraded-since-edit
	// def as if it were a repeat), instead of re-evaluating and
	// downgrading again like any other first read of a large body.
	newBody, _, err := s.handleCode(context.Background(), req, codeParam{
		Op: "edit", Name: "BigFunc", NewBody: bigBody("v2"),
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if strings.Contains(resultText(t, newBody), "BUILD FAILED") {
		t.Fatalf("edit unexpectedly failed: %s", resultText(t, newBody))
	}

	afterEdit, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "BigFunc"})
	if err != nil {
		t.Fatalf("read after edit: %v", err)
	}
	afterEditText := resultText(t, afterEdit)
	if !strings.Contains(afterEditText, "Outline shown") {
		t.Errorf("expected the first read after an edit to re-downgrade (stale readDowngraded tracking must be invalidated), got: %s", afterEditText)
	}
	if strings.Contains(afterEditText, "padding to push body") {
		t.Errorf("body leaked into what should be a downgraded response: %s", afterEditText)
	}
}

// TestHandleCode_RepeatBareReadAfterOutlineDowngradeServesFullBody is
// the #313 regression: a bare read(name) on a large def correctly
// downgrades to outline (#184) the FIRST time, but a real prometheus
// bench cost-gap dig found the model then had to pay a full extra
// round-trip (read again with full:true) on almost every large def it
// actually intended to edit -- a repeat bare read of the SAME name is
// a strong enough intent signal that the second call should just
// serve the full body directly, without requiring the caller to know
// to add full:true.
func TestHandleCode_RepeatBareReadAfterOutlineDowngradeServesFullBody(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0o755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0o644)
	var body strings.Builder
	body.WriteString("package main\n\nfunc BigFunc(name string) string {\n\tresult := \"\"\n")
	for i := 0; i < 60; i++ {
		body.WriteString(fmt.Sprintf("\tresult += \"line %d: padding to push body past 1500 bytes\\n\"\n", i))
	}
	body.WriteString("\treturn result + name\n}\n")
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(body.String()), 0o644)
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal(err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal(err)
	}
	s := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
	s.ready.Store(true)
	sess := &sdkmcp.ServerSession{}
	req := &sdkmcp.CallToolRequest{Session: sess}

	first, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "BigFunc"})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	firstText := resultText(t, first)
	if !strings.Contains(firstText, "Outline shown") {
		t.Fatalf("expected first bare read to auto-downgrade to outline, got: %s", firstText)
	}
	if strings.Contains(firstText, "padding to push body") {
		t.Fatalf("body leaked into first (downgraded) response: %s", firstText)
	}

	second, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "BigFunc"})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	secondText := resultText(t, second)
	if strings.Contains(secondText, "Outline shown") {
		t.Errorf("expected second bare read of the SAME name to serve the full body, got another outline downgrade: %s", secondText)
	}
	if !strings.Contains(secondText, "padding to push body") {
		t.Errorf("expected second bare read to contain the full body, got: %s", secondText)
	}
}
