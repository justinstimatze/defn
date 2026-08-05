package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
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
