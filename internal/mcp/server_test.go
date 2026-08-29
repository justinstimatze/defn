package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/justinstimatze/defn/internal/emit"
	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/planformat"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestVersionEndpoint(t *testing.T) {
	// Route /version through the real mux to cover the method guard
	// and the Content-Type header contract that CLI status depends on.
	mcpServer := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "defn", Version: Version}, nil)
	srv := httptest.NewServer(mcpHTTPMux(mcpServer, "/tmp/test-project"))
	defer srv.Close()

	// GET returns the version as text/plain.
	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /version status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != Version {
		t.Errorf("body = %q, want %q", got, Version)
	}

	// POST is rejected with 405 + Allow header.
	resp, err = http.Post(srv.URL+"/version", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /version status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, should include GET", allow)
	}
}

func TestIdentityEndpoint(t *testing.T) {
	// /identity must echo the projDir verbatim — cmdServe relies on
	// exact-match comparison (after filepath.Abs) to detect FNV
	// hash collisions between distinct projects.
	mcpServer := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "defn", Version: Version}, nil)
	wantDir := "/some/abs/project/path"
	srv := httptest.NewServer(mcpHTTPMux(mcpServer, wantDir))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/identity")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /identity status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != wantDir {
		t.Errorf("body = %q, want %q", got, wantDir)
	}
}

func TestBuildTargetsForFiles(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  []string
	}{
		{"empty → full tree", nil, []string{"./..."}},
		{"root file → .", []string{"main.go"}, []string{"."}},
		{"single subdir", []string{"internal/mcp/server.go"}, []string{"./internal/mcp"}},
		{"dedup same dir", []string{"internal/mcp/a.go", "internal/mcp/b.go"}, []string{"./internal/mcp"}},
		{"multi dir sorted", []string{"internal/store/a.go", "cmd/defn/main.go"}, []string{"./cmd/defn", "./internal/store"}},
		{"root + subdir", []string{"root.go", "internal/mcp/x.go"}, []string{".", "./internal/mcp"}},
		{"bad paths skipped", []string{"/abs/path.go", "../up.go"}, []string{"./..."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildTargetsForFiles(c.files)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("got[%d]=%q, want[%d]=%q", i, got[i], i, c.want[i])
				}
			}
		})
	}
}

func TestExtractSignature(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"simple func", "func Foo(x int) error { return nil }", "func Foo(x int) error"},
		{"no params", "func Bar() { }", "func Bar()"},
		{"method", "func (c *Context) Render(code int) { }", "func (*Context) Render(code int)"},
		{"multi return", "func Baz() (int, error) { return 0, nil }", "func Baz() (int, error)"},
		{"const", "const MaxRetries = 5", "const MaxRetries"},
		{"var", "var ErrNotFound = errors.New(\"not found\")", "var ErrNotFound"},
		{"type", "type Config struct { Port int }", "type Config"},
		{"interface", "type Reader interface { Read(p []byte) (int, error) }", "type Reader"},
		{"doc comment", "// Foo does stuff.\nfunc Foo() {}", "func Foo()"},
		{"map param", "func Foo(m map[string]interface{}) error { return nil }", "func Foo(m map[string]interface{}) error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSignature(tt.body)
			if got != tt.want {
				t.Errorf("extractSignature(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestAstRename(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		oldName        string
		newName        string
		wantSkipped    int
		wantContain    string
		wantNotContain string
	}{
		{
			name:           "rename function call",
			body:           "func Foo() { Bar() }",
			oldName:        "Bar",
			newName:        "Baz",
			wantContain:    "Baz()",
			wantNotContain: "Bar()",
		},
		{
			name:        "preserve comment",
			body:        "func Foo() {\n\t// Bar is important\n\tBar()\n}",
			oldName:     "Bar",
			newName:     "Baz",
			wantContain: "// Bar is important", // comment preserved
		},
		{
			name:        "preserve string literal",
			body:        "func Foo() { fmt.Println(\"Bar\") }",
			oldName:     "Bar",
			newName:     "Baz",
			wantContain: "\"Bar\"", // string preserved
		},
		{
			name:        "skip local var declaration",
			body:        "func Foo() { Bar := 1; _ = Bar }",
			oldName:     "Bar",
			newName:     "Baz",
			wantSkipped: 1, // := declaration skipped, usage renamed
		},
		{
			name:        "skip param declaration",
			body:        "func Foo(Bar int) { _ = Bar }",
			oldName:     "Bar",
			newName:     "Baz",
			wantSkipped: 1, // param decl skipped, usage renamed
		},
		{
			// #304 regression: a bare package-level var's own top-level
			// ValueSpec used to get misclassified as a "local"
			// declaration (the same case that correctly skips a local
			// var := inside a function), so the var renamed everywhere
			// EXCEPT its own declaration.
			name:           "rename package-level var declaration itself",
			body:           "var Bar = regexp.MustCompile(`x`)",
			oldName:        "Bar",
			newName:        "Baz",
			wantContain:    "var Baz = regexp.MustCompile(`x`)",
			wantNotContain: "var Bar",
		},
		{
			name:           "rename package-level const declaration itself",
			body:           "const Bar = 42",
			oldName:        "Bar",
			newName:        "Baz",
			wantContain:    "const Baz = 42",
			wantNotContain: "const Bar",
		},
		{
			// KNOWN LIMITATION (not fixed here, see astRename's doc comment):
			// astRename has no type information, so it can't tell a genuine
			// call to the renamed def apart from an unrelated selector of
			// the same name on some other receiver. Both get renamed here.
			// This documents actual current behavior, not desired behavior.
			name:        "selector collision renames unrelated receiver too",
			body:        "func Foo() { Get(); cfg.Get() }",
			oldName:     "Get",
			newName:     "Fetch",
			wantContain: "cfg.Fetch()", // the actual bug: unrelated selector also renamed
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, skipped := astRename(tt.body, tt.oldName, tt.newName)
			if tt.wantSkipped > 0 && skipped != tt.wantSkipped {
				t.Errorf("skipped = %d, want %d", skipped, tt.wantSkipped)
			}
			if tt.wantContain != "" && !strings.Contains(result, tt.wantContain) {
				t.Errorf("result missing %q:\n%s", tt.wantContain, result)
			}
			if tt.wantNotContain != "" && strings.Contains(result, tt.wantNotContain) {
				t.Errorf("result should not contain %q:\n%s", tt.wantNotContain, result)
			}
		})
	}
}

func TestInferFromBody(t *testing.T) {
	s := &server{}
	tests := []struct {
		body     string
		wantName string
		wantKind string
		wantRecv string
		wantTest bool
	}{
		{"func Foo() {}", "Foo", "function", "", false},
		{"func (c *Context) Render() {}", "Render", "method", "*Context", false},
		{"func TestFoo(t *testing.T) {}", "TestFoo", "function", "", true},
		{"func BenchmarkBar(b *testing.B) {}", "BenchmarkBar", "function", "", true},
		{"type Config struct {}", "Config", "type", "", false},
		{"type Reader interface { Read() }", "Reader", "interface", "", false},
		{"const MaxRetries = 5", "MaxRetries", "const", "", false},
		{"var ErrNotFound = errors.New(\"x\")", "ErrNotFound", "var", "", false},
		{"// Doc comment\nfunc Foo() {}", "Foo", "function", "", false},
		// A whole-file body naturally includes package + imports before
		// the real declaration -- CLAUDE.md documents op:"create" for
		// exactly this "New def / whole file" shape. Confirmed live in 3
		// independent real bench trajectories (prometheus-12024, -17395,
		// -18534): each hit this exact shape and got "couldn't infer
		// definition name from body" despite the body clearly containing
		// one, because f.Decls[0] was the import GenDecl, not the func.
		{"package foo\n\nimport \"time\"\n\nfunc Foo() time.Duration { return 0 }", "Foo", "function", "", false},
		{"package foo\n\nimport (\n\t\"fmt\"\n\t\"time\"\n)\n\nfunc (c *Context) Render() { fmt.Println(time.Now()) }", "Render", "method", "*Context", false},
		{"package foo\n\nimport \"errors\"\n\ntype Config struct{}", "Config", "type", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.wantName, func(t *testing.T) {
			name, kind, recv, isTest := s.inferFromBody(tt.body)
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", kind, tt.wantKind)
			}
			if recv != tt.wantRecv {
				t.Errorf("receiver = %q, want %q", recv, tt.wantRecv)
			}
			if isTest != tt.wantTest {
				t.Errorf("isTest = %v, want %v", isTest, tt.wantTest)
			}
		})
	}
}

// TestCountTopLevelDecls_ImportBlockNotCountedAsADecl guards the other
// half of the same import-GenDecl bug inferFromBody's new test cases
// cover: without nonImportDecls, a single-decl whole-file body (package +
// imports + ONE real declaration) was miscounted as 2+ "top-level
// declarations", which would incorrectly force op:"create" callers down
// the multi-decl path (requiring file:, or erroring "split into N create
// ops") for what is really just one ordinary create.
func TestCountTopLevelDecls_ImportBlockNotCountedAsADecl(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"single_func_with_import", "package foo\n\nimport \"time\"\n\nfunc Foo() time.Duration { return 0 }", 1},
		{"single_func_with_import_block", "package foo\n\nimport (\n\t\"fmt\"\n\t\"time\"\n)\n\nfunc Foo() { fmt.Println(time.Now()) }", 1},
		{"two_funcs_with_import", "package foo\n\nimport \"fmt\"\n\nfunc Foo() { fmt.Println(\"a\") }\n\nfunc Bar() { fmt.Println(\"b\") }", 2},
		{"no_import", "func Foo() {}", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countTopLevelDecls(tt.body); got != tt.want {
				t.Errorf("countTopLevelDecls(%q) = %d, want %d", tt.body, got, tt.want)
			}
		})
	}
}

func TestFormatReceiver(t *testing.T) {
	tests := []struct{ recv, want string }{
		{"", ""},
		{"*Context", "(*Context)."},
		{"Context", "(Context)."},
	}
	for _, tt := range tests {
		got := formatReceiver(tt.recv)
		if got != tt.want {
			t.Errorf("formatReceiver(%q) = %q, want %q", tt.recv, got, tt.want)
		}
	}
}

func TestHandleCodeValidation(t *testing.T) {
	s := &server{backend: nil} // handlers will fail on DB access but validation runs first

	tests := []struct {
		name    string
		args    codeParam
		wantErr string
	}{
		{"read missing name", codeParam{Op: "read"}, "name is required"},
		{"edit missing name", codeParam{Op: "edit", NewBody: "func X() {}"}, "name is required"},
		{"edit missing body", codeParam{Op: "edit", Name: "X"}, "new_body"},
		// Fragment edit passes validation (OldFragment is set, name is set) — skip, needs real DB.
		{"rename missing old", codeParam{Op: "rename", NewName: "Y"}, "old_name is required"},
		{"rename missing new", codeParam{Op: "rename", OldName: "X"}, "new_name is required"},
		{"move missing module", codeParam{Op: "move", Name: "X"}, "module is required"},
		{"query missing sql", codeParam{Op: "query"}, "sql is required"},
		{"insert missing after", codeParam{Op: "insert", Name: "X", Body: "code"}, "after is required"},
		{"insert missing body", codeParam{Op: "insert", Name: "X", After: "anchor"}, "body is required"},
		{"unknown op", codeParam{Op: "nonexistent"}, "unknown op"},
		{"whitespace name", codeParam{Op: "read", Name: "  "}, "name is required"},
		{"read-file missing file", codeParam{Op: "read-file"}, "file is required"},
		{"replace-hunk missing name", codeParam{Op: "replace-hunk", Old: "x", New: "y"}, "name is required"},
		{"replace-hunk missing old", codeParam{Op: "replace-hunk", Name: "F", New: "y"}, "old is required"},
		{"replace-hunk missing new", codeParam{Op: "replace-hunk", Name: "F", Old: "x"}, "new is required"},
		// format:"json" is a real codeParam field, but only op:"impact" and
		// op:"traverse" honor it -- confirmed live in a real
		// prometheus-18712 trajectory, the model tried outline(...,
		// format:"json") expecting structured output and got the same
		// markdown text back, no error, no signal the param did nothing.
		{"format json rejected for outline", codeParam{Op: "outline", Name: "X", Format: "json"}, "format"},
		{"format json rejected for read", codeParam{Op: "read", Name: "X", Format: "json"}, "format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _, _ := s.handleCode(context.Background(), nil, tt.args)
			if tt.wantErr == "" {
				return // just checking it doesn't panic on validation
			}
			if result == nil {
				t.Fatal("expected error result, got nil")
			}
			text := resultText(t, result)
			if !strings.Contains(strings.ToLower(text), strings.ToLower(tt.wantErr)) {
				t.Errorf("error = %q, want to contain %q", text, tt.wantErr)
			}
		})
	}
}

func setupTestDB(t *testing.T) (store.Backend, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Create a minimal Go project for ingestion.
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

// Greet returns a greeting.
func Greet(name string) string {
	return "Hello, " + name
}

// Farewell says goodbye.
func Farewell(name string) string {
	return Greet(name) + " and goodbye"
}

func main() {
	Farewell("world")
}
`), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte(`package main

import "testing"

func TestGreet(t *testing.T) {
	if Greet("x") == "" {
		t.Fatal("empty")
	}
}

func TestFarewell(t *testing.T) {
	if Farewell("x") == "" {
		t.Fatal("empty")
	}
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	return db, projDir
}

// L14: op:read-and-verify returns the def body concatenated with its
// covering test-run output, so a single call surfaces source + behavior.
func TestHandleReadAndVerify_CombinesReadAndTest(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	result, _, _ := s.handleReadAndVerify(context.Background(), nil,
		codeParam{Name: "Greet"})
	text := resultText(t, result)

	// Read portion: body content
	if !strings.Contains(text, "Hello") {
		t.Errorf("expected Greet body containing 'Hello', got %q", text)
	}
	// Verify portion: test-run status
	if !strings.Contains(text, "TESTS PASSED") && !strings.Contains(text, "TESTS FAILED") {
		t.Errorf("expected test-run status, got %q", text)
	}
}

// L14: not-found def surfaces the not-found error (with suggestions from
// L10) rather than swallowing the read error and trying the test path.
func TestHandleReadAndVerify_NotFoundBubbles(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	result, _, _ := s.handleReadAndVerify(context.Background(), nil,
		codeParam{Name: "NoSuchDefXyz"})
	text := resultText(t, result)
	if !strings.Contains(text, "not found") {
		t.Errorf("expected not-found bubble, got %q", text)
	}
}

// L15: impact output lists covering test names and warns when none of
// them lexically mention the def — surfaces "indirectly tested" cases.
func TestHandleImpact_ListsTestNamesAndCoherenceHint(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	// TestGreet covers Greet — name mentions the def; no coherence warning.
	result, _, _ := s.handleImpact(context.Background(), nil, codeParam{Name: "Greet"})
	text := resultText(t, result)
	if !strings.Contains(text, "TestGreet") {
		t.Errorf("expected test name in impact output, got %q", text)
	}
	if strings.Contains(text, "coverage is indirect") {
		t.Errorf("Greet has TestGreet — should not warn about indirect coverage: %q", text)
	}
}

// L18: op:overview with no file/name returns a project-wide module summary.
func TestHandleOverview_EmptyReturnsProjectSummary(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleOverview(context.Background(), nil, codeParam{})
	text := resultText(t, result)
	if !strings.Contains(text, "Project overview") {
		t.Errorf("expected project overview header, got %q", text)
	}
	if !strings.Contains(text, "testproj") {
		t.Errorf("expected module 'testproj' listed, got %q", text)
	}
}

// L11: op:test test:"TestX" runs a named test directly (bypasses the
// def-name → coverage → -run path). Reproduces a bug from an issue's
// named failing test in one turn.
func TestHandleTestByName_RunsNamedTest(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	// Setup DB seeds TestGreet + TestFarewell as passing tests. Run just one.
	result, _, _ := s.handleTestByName(context.Background(), nil, "TestGreet", "", "")
	text := resultText(t, result)
	if !strings.Contains(text, "TestGreet") {
		t.Errorf("expected TestGreet in output, got %q", text)
	}
	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("expected passing status, got %q", text)
	}
	if strings.Contains(text, "TestFarewell") {
		t.Errorf("TestFarewell should not run under -run TestGreet, got %q", text)
	}
}

func TestHandleTestByName_EmptyPatternRejected(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	result, _, _ := s.handleTestByName(context.Background(), nil, "", "", "")
	text := resultText(t, result)
	if !strings.Contains(text, "empty") {
		t.Errorf("expected empty-pattern rejection, got %q", text)
	}
}

func TestHandleEmit(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	// Relative path resolves against projDir.
	outRel := filepath.Join("out-rel")
	result, _, _ := s.handleEmit(context.Background(), nil, codeParam{Out: outRel})
	text := resultText(t, result)
	if !strings.Contains(text, "Emitted") {
		t.Fatalf("expected success message, got: %s", text)
	}
	// Verify the emitted file exists and has Greet/Farewell.
	data, err := os.ReadFile(filepath.Join(projDir, outRel, "main.go"))
	if err != nil {
		t.Fatalf("read emitted file: %v", err)
	}
	if !strings.Contains(string(data), "func Greet(") {
		t.Errorf("emitted main.go missing Greet:\n%s", data)
	}
	if !strings.Contains(string(data), "func Farewell(") {
		t.Errorf("emitted main.go missing Farewell:\n%s", data)
	}

	// Absolute paths also work.
	outAbs := t.TempDir()
	result, _, _ = s.handleEmit(context.Background(), nil, codeParam{Out: outAbs})
	if !strings.Contains(resultText(t, result), "Emitted") {
		t.Fatalf("absolute emit failed: %s", resultText(t, result))
	}
	if _, err := os.Stat(filepath.Join(outAbs, "main.go")); err != nil {
		t.Fatalf("absolute emit didn't write main.go: %v", err)
	}
}

func TestHandleEmitRequiresOut(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "emit"})
	text := resultText(t, result)
	if !strings.Contains(text, "out is required") {
		t.Errorf("expected 'out is required' error, got: %s", text)
	}
}

func TestHandleImpact(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleImpact(context.Background(), nil, codeParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "Greet") {
		t.Error("expected Greet in impact output")
	}
	if !strings.Contains(text, "Direct callers") || !strings.Contains(text, "Farewell") {
		t.Error("expected Farewell as a caller of Greet")
	}
}

// TestHandleImpact_ModuleBreakdown seeds a target def called by two
// separate modules and asserts the "by module: ..." line appears
// with both module paths. #156 workspace-aware impact.
func TestHandleImpact_ModuleBreakdown(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	// Target module + its Def T.
	m1, _ := db.EnsureModule("example.com/lib", "lib", "")
	target := &store.Definition{
		ModuleID: m1.ID, Name: "T", Kind: "function", Exported: true,
		Body: "func T() {}", Signature: "func T()",
	}
	target.Hash = store.HashBody(target.Body)
	targetID, _ := db.UpsertDefinition(target)

	// Two caller modules with 2 and 1 callers respectively.
	m2, _ := db.EnsureModule("example.com/svc/handler", "handler", "")
	m3, _ := db.EnsureModule("example.com/svc/worker", "worker", "")
	callers := []*store.Definition{
		{ModuleID: m2.ID, Name: "H1", Kind: "function", Body: "func H1() { T() }"},
		{ModuleID: m2.ID, Name: "H2", Kind: "function", Body: "func H2() { T() }"},
		{ModuleID: m3.ID, Name: "W1", Kind: "function", Body: "func W1() { T() }"},
	}
	for _, c := range callers {
		c.Hash = store.HashBody(c.Body)
		id, _ := db.UpsertDefinition(c)
		_ = db.SetReferences(id, []store.Reference{{FromDef: id, ToDef: targetID, Kind: "call"}})
	}

	result, _, err := s.handleImpact(context.Background(), nil, codeParam{Name: "T"})
	if err != nil {
		t.Fatalf("handleImpact: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "by module:") {
		t.Errorf("expected 'by module:' breakdown line\n---\n%s", text)
	}
	// Handler module (2 callers) should sort before worker (1 caller).
	iH := strings.Index(text, "handler")
	iW := strings.Index(text, "worker")
	if iH < 0 || iW < 0 || iH > iW {
		t.Errorf("expected handler (2) before worker (1); got:\n%s", text)
	}
}

// TestHandleImpact_QueryFilter validates the #157 query-context on
// impact: callers whose name/receiver/source_file doesn't contain
// any query token are hidden with a "filtered by query" line.
func TestHandleImpact_QueryFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	m, _ := db.EnsureModule("example.com/svc", "svc", "")
	target := &store.Definition{ModuleID: m.ID, Name: "Handle", Kind: "function", Body: "func Handle() {}"}
	target.Hash = store.HashBody(target.Body)
	targetID, _ := db.UpsertDefinition(target)
	// 3 callers: two match "auth", one doesn't.
	callers := []*store.Definition{
		{ModuleID: m.ID, Name: "authenticate", Kind: "function", Body: "func authenticate() { Handle() }"},
		{ModuleID: m.ID, Name: "authorize", Kind: "function", Body: "func authorize() { Handle() }"},
		{ModuleID: m.ID, Name: "logRequest", Kind: "function", Body: "func logRequest() { Handle() }"},
	}
	for _, c := range callers {
		c.Hash = store.HashBody(c.Body)
		id, _ := db.UpsertDefinition(c)
		_ = db.SetReferences(id, []store.Reference{{FromDef: id, ToDef: targetID, Kind: "call"}})
	}

	result, _, err := s.handleImpact(context.Background(), nil, codeParam{Name: "Handle", Query: "auth"})
	if err != nil {
		t.Fatalf("handleImpact query: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "filtered by query=\"auth\": 1 callers hidden") {
		t.Errorf("expected 'filtered by query' hint w/ 1 hidden; got:\n%s", text)
	}
	if strings.Contains(text, "logRequest") {
		t.Errorf("logRequest should be filtered out; got:\n%s", text)
	}
	if !strings.Contains(text, "authenticate") || !strings.Contains(text, "authorize") {
		t.Errorf("expected authenticate + authorize to survive filter; got:\n%s", text)
	}
}

func TestHandleImpact_Rank(t *testing.T) {
	// rank=true must not panic, must not lose callers, and must keep
	// the formatted output coherent. Score ordering is exercised
	// directly in internal/rank — here we just verify the wire-up.
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}
	s.idf = newIDF(db)

	result, _, err := s.handleImpact(context.Background(), nil, codeParam{Name: "Greet", Rank: true})
	if err != nil {
		t.Fatalf("rank=true impact: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Greet") {
		t.Error("expected Greet in ranked impact output")
	}
	if !strings.Contains(text, "Farewell") {
		t.Error("expected Farewell still present after ranking")
	}
}

func TestHandleRead(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "Hello") {
		t.Error("expected function body containing 'Hello'")
	}

	// StructuredContent is intentionally not set (see withUsage comment):
	// Claude's tool_result serialization drops the text body when
	// structuredContent is populated. Usage metadata now lives in the
	// footer only.
}

// TestHandleRead_QueryAdaptive locks in the #153 wire-through: a
// read with a non-empty query filters top-level body statements to
// those touching the query, and annotates the response.
func TestHandleRead_QueryAdaptive(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	mod, _ := db.EnsureModule("example.com/svc", "svc", "")
	// Body needs to be substantial — the query-adaptive filter has a
	// net-savings gate: hint header (~140 bytes) must be cheaper than
	// the elided bytes, else the filter no-ops. This body is 700+
	// bytes with 5 distinct branches so filtering pays off.
	body := `func handleRequest(w http.ResponseWriter, r *http.Request) {
	if !authenticated(r) {
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"api\"")
		w.WriteHeader(401)
		w.Write([]byte("{\"error\":\"unauthorized\"}"))
		return
	}
	if r.Method == "POST" {
		body, err := io.ReadAll(r.Body)
		if err != nil { http.Error(w, err.Error(), 500); return }
		handlePost(w, r, body)
		return
	}
	if r.Method == "GET" {
		handleGet(w, r)
		w.Header().Set("Cache-Control", "no-store")
		return
	}
	if r.Method == "DELETE" {
		if err := checkAdmin(r); err != nil { http.Error(w, err.Error(), 403); return }
		handleDelete(w, r)
		return
	}
	w.Header().Set("Allow", "GET, POST, DELETE")
	w.WriteHeader(405)
	w.Write([]byte("method not allowed"))
}`
	d := &store.Definition{
		ModuleID: mod.ID, Name: "handleRequest", Kind: "function",
		Exported: false, Body: body, Signature: "func handleRequest(w http.ResponseWriter, r *http.Request)",
	}
	d.Hash = store.HashBody(d.Body)
	if _, err := db.UpsertDefinition(d); err != nil {
		t.Fatal(err)
	}

	// Full read: everything present.
	full, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "handleRequest"})
	fullTxt := resultText(t, full)
	for _, tok := range []string{"401", "POST", "GET", "405"} {
		if !strings.Contains(fullTxt, tok) {
			t.Errorf("full read missing %q", tok)
		}
	}

	// Query-adaptive read for "401": only the auth branch survives.
	q, _, _ := s.handleGetDefinition(context.Background(), nil,
		nameParam{Name: "handleRequest", Query: "401"})
	qTxt := resultText(t, q)
	if !strings.Contains(qTxt, "401") {
		t.Errorf("query-adaptive read should retain the 401 branch\n---\n%s", qTxt)
	}
	for _, dropped := range []string{"handlePost(", "handleGet(", "405"} {
		if strings.Contains(qTxt, dropped) {
			t.Errorf("query-adaptive read should NOT contain %q\n---\n%s", dropped, qTxt)
		}
	}
	if !strings.Contains(qTxt, "query-adaptive read") {
		t.Errorf("expected query-adaptive header hint\n---\n%s", qTxt)
	}
	if len(qTxt) >= len(fullTxt) {
		t.Errorf("query-adaptive should be smaller: got %d, full %d", len(qTxt), len(fullTxt))
	}

	// Query that matches nothing → no elision (all-match path returns
	// full body). Also: still no hint header.
	noMatch, _, _ := s.handleGetDefinition(context.Background(), nil,
		nameParam{Name: "handleRequest", Query: "zzznonexistent"})
	if strings.Contains(resultText(t, noMatch), "query-adaptive read") {
		t.Errorf("no-match query should skip hint header (all elided → falls back to full body)")
	}
}

// L10: not-found errors should attach a "Did you mean" list drawn from
// name-LIKE candidates so the model can retry without a round-trip.
func TestNotFoundResult_SuggestsClosest(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// "reet" is a substring of Greet; case-preserved LIKE should surface it.
	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "reet"})
	text := resultText(t, result)
	if !strings.Contains(text, "not found") {
		t.Fatalf("expected 'not found', got %q", text)
	}
	if !strings.Contains(text, "Did you mean") {
		t.Errorf("expected 'Did you mean' suggestion, got %q", text)
	}
	if !strings.Contains(text, "Greet") {
		t.Errorf("expected Greet in suggestions, got %q", text)
	}
}

// L10: when the name has no partial matches at all, degrade to the plain
// error — don't invent noise.
func TestNotFoundResult_NoSuggestionsWhenAbsent(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Xylophone123"})
	text := resultText(t, result)
	if !strings.Contains(text, "not found") {
		t.Fatalf("expected 'not found', got %q", text)
	}
	if strings.Contains(text, "Did you mean") {
		t.Errorf("expected no suggestion when no candidates match, got %q", text)
	}
}

// TestHandleRead_UpstreamMatch seeds an upstream fingerprint whose hash
// matches the local Greet body exactly, then verifies the read op
// returns the compact provenance form (no body, tagged with version).
func TestHandleRead_UpstreamMatch(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// Pull the local Greet so we can hash its exact body.
	d, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}
	hash := store.HashBodyStructural(d.Body)

	if err := db.InsertUpstreamFingerprint(store.UpstreamFingerprint{
		ModulePath:  "testproj",
		Version:     "v1.2.3",
		DefName:     "Greet",
		Kind:        "function",
		Receiver:    "",
		Fingerprint: hash,
		Signature:   "func Greet(name string) string",
		Doc:         "Greet returns a greeting.",
	}); err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "v1.2.3") {
		t.Errorf("expected upstream version tag, got: %s", text)
	}
	if !strings.Contains(text, "unchanged from upstream") {
		t.Errorf("expected provenance tag, got: %s", text)
	}
	if strings.Contains(text, "\"Hello, \"") {
		t.Errorf("expected body to be elided in compact form, got: %s", text)
	}
	if !strings.Contains(text, "full: true") {
		t.Errorf("expected hint about full:true, got: %s", text)
	}

	// full:true should bypass the compact form and return the body.
	fullResult, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet", Full: true})
	fullText := resultText(t, fullResult)
	if !strings.Contains(fullText, "\"Hello, \"") {
		t.Errorf("full:true should include body, got: %s", fullText)
	}
}

// TestHandleRead_UpstreamDivergence seeds an upstream row whose hash
// does NOT match the local body — the read op should return the full
// body with a divergence note (helpful when the user has patched a
// dep locally).
func TestHandleRead_UpstreamDivergence(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	if err := db.InsertUpstreamFingerprint(store.UpstreamFingerprint{
		ModulePath:  "testproj",
		Version:     "v1.2.3",
		DefName:     "Greet",
		Kind:        "function",
		Receiver:    "",
		Fingerprint: "different-hash-does-not-match",
		Signature:   "func Greet(name string) string",
		Doc:         "Greet returns a greeting.",
	}); err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "diverges from all known upstream versions") {
		t.Errorf("expected divergence note, got: %s", text)
	}
	if !strings.Contains(text, "v1.2.3") {
		t.Errorf("expected known version listed, got: %s", text)
	}
	if !strings.Contains(text, "\"Hello, \"") {
		t.Errorf("divergence path should include full body, got: %s", text)
	}
}

// TestHandleRead_UnknownModule verifies that a def whose module has
// no upstream_fingerprints rows falls through to the current body-in-fence
// behavior unchanged.
func TestHandleRead_UnknownModule(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "\"Hello, \"") {
		t.Errorf("expected full body in output for unknown module, got: %s", text)
	}
	if strings.Contains(text, "unchanged from upstream") {
		t.Errorf("no upstream rows exist — should not be tagged as unchanged, got: %s", text)
	}
	if strings.Contains(text, "diverges from") {
		t.Errorf("no upstream rows exist — should not be tagged as divergent, got: %s", text)
	}
}

func TestHandleReadFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleReadFile(context.Background(), nil, codeParam{File: "main.go"})
	if err != nil {
		t.Fatalf("read-file: %v", err)
	}
	text := resultText(t, result)

	// Both defs from main.go should appear, bodies included.
	if !strings.Contains(text, "Greet") {
		t.Error("expected Greet in output")
	}
	if !strings.Contains(text, "Farewell") {
		t.Error("expected Farewell in output")
	}
	if !strings.Contains(text, "Hello, ") {
		t.Error("expected Greet body ('Hello, ') in output")
	}
	if !strings.Contains(text, "goodbye") {
		t.Error("expected Farewell body ('goodbye') in output")
	}
	// Source-order: Greet (line 4) before Farewell (line 9).
	gi := strings.Index(text, "Greet")
	fi := strings.Index(text, "Farewell")
	if gi < 0 || fi < 0 || gi > fi {
		t.Errorf("expected Greet before Farewell in source order, got Greet@%d Farewell@%d", gi, fi)
	}

	// StructuredContent no longer set (see withUsage comment).
}

func TestCompactReadFile(t *testing.T) {
	defs := []store.Definition{
		{Name: "Foo", Kind: "function", Signature: "func Foo(x int) error", StartLine: 10, EndLine: 42},
		{Name: "Bar", Kind: "method", Receiver: "*T", Signature: "func (t *T) Bar() string", StartLine: 50, EndLine: 80},
		{Name: "Baz", Kind: "function", StartLine: 90, EndLine: 100}, // no sig
	}
	out := compactReadFile("pkg/x.go", "example.com/mod", defs, 12345)
	if !strings.Contains(out, "signatures only") {
		t.Errorf("expected 'signatures only' header marker; got %q", out[:200])
	}
	if !strings.Contains(out, "Foo (function) L10-42 — func Foo(x int) error") {
		t.Errorf("expected Foo sig line; got %q", out)
	}
	if !strings.Contains(out, "(*T).Bar (method) L50-80 — func (t *T) Bar() string") {
		t.Errorf("expected Bar sig line (with receiver); got %q", out)
	}
	if !strings.Contains(out, "Baz (function) L90-100 — (sig unavailable)") {
		t.Errorf("expected Baz sig unavailable line; got %q", out)
	}
	if !strings.Contains(out, "would be 12345 bytes") {
		t.Errorf("expected full-size mention; got %q", out)
	}
	if strings.Contains(out, "```go") {
		t.Errorf("compact form should not include code fences; got %q", out)
	}
}

func TestHandleReadFile_MissingFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleReadFile(context.Background(), nil, codeParam{File: "does-not-exist.go"})
	text := resultText(t, result)
	if !strings.Contains(text, "no definitions found") {
		t.Errorf("expected 'no definitions found' error, got: %s", text)
	}
}

// TestHandleExpand_BodyAndCallers exercises expand's happy path — one call
// returns body + callers in one tool_result. Attacks the N² cache-read
// problem by killing the read → impact → read multi-turn pattern.
func TestHandleExpand_BodyAndCallers(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{
		Name:    "Greet",
		Include: []string{"body", "callers"},
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	text := resultText(t, result)

	if !strings.Contains(text, "### body") {
		t.Errorf("expected body section header, got: %s", text)
	}
	if !strings.Contains(text, "Hello, ") {
		t.Errorf("expected Greet body ('Hello, '), got: %s", text)
	}
	if !strings.Contains(text, "### callers") {
		t.Errorf("expected callers section header, got: %s", text)
	}
	if !strings.Contains(text, "Farewell") {
		t.Errorf("expected Farewell as a caller of Greet, got: %s", text)
	}
	// Test callers should be marked _(test)_.
	if !strings.Contains(text, "TestGreet") {
		t.Errorf("expected TestGreet as a (test) caller of Greet, got: %s", text)
	}

	// StructuredContent no longer set (see withUsage comment).
}

// TestHandleExpand_DefaultInclude verifies empty include:[] defaults to
// [outline, callers] — the exploratory pair that answers most "what does
// X do / who calls it" questions without paying for the body. Body must
// be explicitly requested. Regression lock for #172.
func TestHandleExpand_DefaultInclude(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{Name: "Greet"})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "### outline") {
		t.Errorf("default include should include outline, got: %s", text)
	}
	if !strings.Contains(text, "### callers") {
		t.Errorf("default include should include callers, got: %s", text)
	}
	if strings.Contains(text, "### body") {
		t.Errorf("default include must NOT include body — #172 outline-first default; got: %s", text)
	}
}

// TestHandleExpand_OutlineInclude verifies include:["outline"] returns
// the compact projection (sig + doc + body-size + callees + flow) with
// NO body block. This is the include kind that carries the -80% cost
// story: 5-10× smaller than body for exploratory questions. #172.
func TestHandleExpand_OutlineInclude(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{
		Name:    "Greet",
		Include: []string{"outline"},
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "### outline") {
		t.Errorf("expected outline section header, got: %s", text)
	}
	if !strings.Contains(text, "Body:") {
		t.Errorf("outline should report body byte/line count, got: %s", text)
	}
	if strings.Contains(text, "### body") {
		t.Errorf("outline-only include must not emit body section; got: %s", text)
	}
	// The literal "Hello, " string is inside Greet's body — if it appears
	// under outline-only, we're leaking the body via outline.
	if strings.Contains(text, "Hello, ") {
		t.Errorf("outline-only leaked body content; got: %s", text)
	}
}

// TestHandleExpand_UnknownIncludeKind ensures unsupported include kinds
// are ignored with a note (learn-the-vocabulary affordance) rather than
// erroring the whole request.
func TestHandleExpand_UnknownIncludeKind(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{
		Name:    "Greet",
		Include: []string{"body", "callers", "types-used"},
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "### body") {
		t.Error("expected body section")
	}
	if !strings.Contains(text, "types-used") {
		t.Error("expected note about the unsupported kind")
	}
}

// TestHandleFileDefs_RootLevelFile is the regression for the bug that
// let handleFileDefs miss defs when the file is at the module root and
// the module path did not contain the file stem (e.g. module "testproj"
// + file "main.go" — the old code stripped .go and searched for a "main"
// dir substring, which never matches "testproj"). Fixed by mirroring
// handleReadFile's dir="" pattern for bare filenames.
func TestHandleFileDefs_RootLevelFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleFileDefs(context.Background(), nil, codeParam{File: "main.go"})
	if err != nil {
		t.Fatalf("file-defs: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Greet") {
		t.Errorf("expected Greet in file-defs output, got: %s", text)
	}
	if !strings.Contains(text, "Farewell") {
		t.Errorf("expected Farewell in file-defs output, got: %s", text)
	}
}

// TestHandleAddImportRootFile regresses a bug where handleAddImport
// treated a root-level file (no "/") as its own directory when
// looking up its module, so FindDefinitionsByFile searched
// m.path LIKE '%main.go%' — which never matched the module path
// (e.g. "testproj"). The lookup returned zero defs and add-import
// errored with "no definitions found in file X — cannot resolve
// module", making callers thrash for many retries in benches.
func TestHandleAddImportRootFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleAddImport(context.Background(), nil, codeParam{
		File:       "main.go",
		ImportPath: "hash/fnv",
	})
	text := resultText(t, result)
	if strings.Contains(text, "no definitions found") {
		t.Fatalf("add-import failed on root-level file: %s", text)
	}
	if !strings.Contains(text, "added import") && !strings.Contains(text, "already present") {
		t.Errorf("expected success indicator in output, got: %s", text)
	}
}

func TestHandleEdit(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	newBody := `func Greet(name string) string {
	return "Hi, " + name
}`
	result, _, _ := s.handleEdit(context.Background(), nil, editParam{Name: "Greet", NewBody: newBody})
	text := resultText(t, result)

	if !strings.Contains(text, "Updated") {
		t.Error("expected 'Updated' in edit response")
	}

	// Verify the change persisted.
	d, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Body, "Hi, ") {
		t.Error("body not updated")
	}
}

func TestHandleEditSyntaxValidation(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleEdit(context.Background(), nil, editParam{
		Name:    "Greet",
		NewBody: "func Greet() { syntax error here",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "syntax error") {
		t.Errorf("expected syntax error, got: %s", text)
	}
}

func TestHandleFragmentEdit(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleFragmentEdit(context.Background(), nil, codeParam{
		Name:        "Greet",
		OldFragment: "Hello",
		NewFragment: "Hey",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Edited") {
		t.Errorf("expected 'Edited', got: %s", text)
	}

	d, _ := db.GetDefinitionByName("Greet", "")
	if !strings.Contains(d.Body, "Hey") {
		t.Error("fragment not replaced")
	}
}

func TestHandleFragmentEditNotFound(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleFragmentEdit(context.Background(), nil, codeParam{
		Name:        "Greet",
		OldFragment: "nonexistent text",
		NewFragment: "x",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "not found") {
		t.Errorf("expected 'not found', got: %s", text)
	}
}

func TestHandleFragmentEditEmpty(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleFragmentEdit(context.Background(), nil, codeParam{
		Name:        "Greet",
		OldFragment: "",
		NewFragment: "x",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "cannot be empty") {
		t.Errorf("expected 'cannot be empty', got: %s", text)
	}
}

func TestHandleInsert(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleInsert(context.Background(), nil, codeParam{
		Name:  "Greet",
		After: "Hello",
		Body:  " there",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Inserted") {
		t.Errorf("expected 'Inserted', got: %s", text)
	}

	d, _ := db.GetDefinitionByName("Greet", "")
	if !strings.Contains(d.Body, "Hello there") {
		t.Error("insert not applied")
	}
}

func TestHandleSearch(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleSearch(context.Background(), nil, codeParam{Pattern: "%Greet%"})
	text := resultText(t, result)

	if !strings.Contains(text, "Greet") {
		t.Error("expected Greet in search results")
	}
}

func TestSearchBodiesLike(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()

	// "Hello" appears in Greet's body (`return "Hello, " + name`) — single
	// hit, snippet should include the substring.
	hits, err := db.SearchBodiesLike("Hello", 100)
	if err != nil {
		t.Fatalf("SearchBodiesLike: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit for 'Hello', got %d", len(hits))
	}
	h := hits[0]
	if h.Name != "Greet" {
		t.Errorf("hit.Name = %q, want Greet", h.Name)
	}
	if !strings.Contains(h.Snippet, "Hello") {
		t.Errorf("snippet missing needle: %q", h.Snippet)
	}
	if h.Line <= 0 {
		t.Errorf("Line should be >0 (absolute in source): %d", h.Line)
	}

	// Case-insensitive: "hello" (lowercase) should also find Greet.
	hits, err = db.SearchBodiesLike("hello", 100)
	if err != nil || len(hits) != 1 {
		t.Fatalf("case-insensitive miss: %d hits, err=%v", len(hits), err)
	}

	// "goodbye" appears in Farewell's body.
	hits, _ = db.SearchBodiesLike("goodbye", 100)
	found := false
	for _, h := range hits {
		if h.Name == "Farewell" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Farewell hit for 'goodbye', got %+v", hits)
	}

	// No matches: empty slice, no error.
	hits, err = db.SearchBodiesLike("this-string-is-nowhere-in-the-fixture", 100)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected 0 hits, got %d", len(hits))
	}
}

func TestSearchBodiesLike_EscapesLikeMetachars(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()

	// User pattern with a literal % should not become a wildcard: without
	// escaping this would match everything; with escaping it should match
	// nothing since no body contains a literal %.
	hits, err := db.SearchBodiesLike("%", 100)
	if err != nil {
		t.Fatalf("SearchBodiesLike: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("literal %% should not wildcard-match everything; got %d hits", len(hits))
	}
}

func TestBodyScanResult_Empty(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.bodyScanResult("no-such-string-anywhere", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "no matches") || !strings.Contains(text, "body-scan") {
		t.Errorf("expected 'no matches' + 'body-scan' hint; got %q", text)
	}
}

func TestBodyScanResult_Hits(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.bodyScanResult("Hello", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	// Header line: "[body-scan for \"Hello\" — N hits."
	if !strings.Contains(text, "body-scan for \"Hello\"") {
		t.Errorf("expected header naming pattern; got %q", text[:200])
	}
	// JSON body includes name+file+snippet.
	if !strings.Contains(text, `"name":"Greet"`) {
		t.Errorf("expected Greet in JSON body; got %q", text)
	}
	if !strings.Contains(text, `"snippet"`) {
		t.Errorf("expected snippet field; got %q", text)
	}
}

func TestHandleSearch_Stage3FallsBackToBodyScan(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// "Hello" is not a def name and unlikely to be an FTS match (short-ish
	// and inside a string literal). Stage-3 body-scan should catch it.
	result, _, _ := s.handleSearch(context.Background(), nil, codeParam{Pattern: "Hello"})
	text := resultText(t, result)
	if !strings.Contains(text, "Greet") {
		t.Errorf("stage-3 body-scan should find Greet via 'Hello' in body; got %q", text)
	}
}

// Bundle B1: `%Hello%` (agent LIKE-glob form) with 0 name hits must strip
// wildcards and body-scan. Regression bug: stage-3 was skipping ANY pattern
// containing `%`, so `%JobsURL%` and similar returned nothing when the def
// body would have matched.
func TestHandleSearch_Stage3StripsWildcards(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleSearch(context.Background(), nil, codeParam{Pattern: "%Hello%"})
	text := resultText(t, result)
	if !strings.Contains(text, "Greet") {
		t.Errorf("wildcard-strip fallback should find Greet via '%%Hello%%' in body; got %q", text)
	}
	// The scan query should be the stripped form, not the wildcard form.
	if strings.Contains(text, "%Hello%") {
		t.Errorf("expected scan header to show stripped pattern, saw raw wildcards: %q", text)
	}
}

// Bundle B1: `_` name-wildcard is not something we substitute — refuse to
// body-scan under it (would silently mean the wrong thing). Test locks in
// that guard so a future refactor doesn't paper over the semantic mismatch.
func TestHandleSearch_Stage3SkipsUnderscore(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// _ is SQL LIKE's single-char wildcard. `Hell_` is a name-LIKE query
	// that finds nothing here — we should NOT then body-scan "Hell_".
	result, _, _ := s.handleSearch(context.Background(), nil, codeParam{Pattern: "Hell_"})
	text := resultText(t, result)
	if strings.Contains(text, "Greet") {
		t.Errorf("underscore-pattern should not trigger body-scan; got %q", text)
	}
}

func TestHandleCreate(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func NewHelper() string { return \"help\" }",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Created") {
		t.Errorf("expected 'Created', got: %s", text)
	}
}

// Bug C: op:create with multi-decl body and NO file: must reject — the
// caller has no way to say where the defs land.
func TestHandleCreateRejectsMultiDeclWithoutFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	body := `const Limit = 10

func Helper() int { return Limit }

func Other() int { return 0 }`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{Body: body})
	text := resultText(t, result)

	if !strings.Contains(text, "top-level declarations") {
		t.Errorf("expected multi-decl rejection error, got: %s", text)
	}
	if _, err := db.GetDefinitionByName("Helper", ""); err == nil {
		t.Error("Helper should not have been created when body was rejected")
	}
	if _, err := db.GetDefinitionByName("Other", ""); err == nil {
		t.Error("Other should not have been created when body was rejected")
	}
}

// Multi-def file authoring: with file: set, a multi-decl body should
// upsert each decl as its own Definition sharing the same SourceFile,
// running a single autoEmit+build. This is the write-granularity fix
// motivated by 2026-07-11 turns.txt trajectory analysis.
func TestHandleCreateMultiDeclWithFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	body := `// Limit is the max requests per second.
const Limit = 10

// Bucket tracks tokens.
type Bucket struct {
	tokens int
}

// NewBucket seeds a Bucket.
func NewBucket(n int) *Bucket {
	return &Bucket{tokens: n}
}

// Take drains a token.
func (b *Bucket) Take() bool {
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "main.go",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Created 4 defs") {
		t.Fatalf("expected 'Created 4 defs', got: %s", text)
	}
	for _, name := range []string{"Limit", "Bucket", "NewBucket", "Take"} {
		if !strings.Contains(text, name) {
			t.Errorf("expected %s in summary, got: %s", name, text)
		}
	}

	for _, want := range []struct{ Name, Kind string }{
		{"Limit", "const"},
		{"Bucket", "type"},
		{"NewBucket", "function"},
		{"Take", "method"},
	} {
		d, err := db.GetDefinitionByName(want.Name, "")
		if err != nil {
			t.Errorf("%s not found: %v", want.Name, err)
			continue
		}
		if d.Kind != want.Kind {
			t.Errorf("%s.Kind = %q, want %q", want.Name, d.Kind, want.Kind)
		}
		if d.SourceFile != "main.go" {
			t.Errorf("%s.SourceFile = %q, want main.go", want.Name, d.SourceFile)
		}
		if want.Name == "Take" && d.Receiver != "*Bucket" {
			t.Errorf("Take.Receiver = %q, want *Bucket", d.Receiver)
		}
		if !strings.Contains(d.Body, want.Name) {
			t.Errorf("%s body missing name: %q", want.Name, d.Body)
		}
	}
}

// If any name in a multi-decl body collides with an existing def, the
// whole batch must be rejected — no partial creates.
func TestHandleCreateMultiDeclRejectsNameCollision(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// Seed one def whose name will collide.
	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "func Existing() int { return 1 }",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	body := `func Fresh() int { return 2 }

func Existing() int { return 3 }`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "main.go",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "already exists") {
		t.Errorf("expected 'already exists' error, got: %s", text)
	}
	if _, err := db.GetDefinitionByName("Fresh", ""); err == nil {
		t.Error("Fresh must not have been created (collision aborts batch)")
	}
}

// The model naturally writes whole-file bodies beginning with `package
// foo` when asked to author a new file. Multi-decl create must strip
// the leading package decl instead of choking on the resulting duplicate
// package clause. Regression from the 2026-07-17 probe.
func TestHandleCreateMultiDeclStripsLeadingPackage(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	body := `package multitest

// Alpha runs.
func Alpha() int { return 1 }

// Beta runs.
func Beta() int { return 2 }

// Gamma runs.
func Gamma() int { return 3 }`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "main.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created 3 defs") {
		t.Fatalf("expected 'Created 3 defs' (with leading package stripped), got: %s", text)
	}
	for _, name := range []string{"Alpha", "Beta", "Gamma"} {
		if _, err := db.GetDefinitionByName(name, ""); err != nil {
			t.Errorf("%s not created: %v", name, err)
		}
	}
}

// Regression: model naturally writes bodies that start with `package X`
// followed by `import (...)` — that import block was tripping sliceDecls
// with "could not infer name (kind=*ast.GenDecl)". Fix skips import
// GenDecls silently; goimports re-adds them at emit from usage.
func TestHandleCreateMultiDeclSkipsImportBlock(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	body := `package middleware

import (
	"net/http"
	"sync"
	"time"
)

// RateLimiter guards.
type RateLimiter struct {
	mu       sync.Mutex
	requests map[string]time.Time
}

// Allow reports whether the request may proceed.
func (r *RateLimiter) Allow(req *http.Request) bool {
	return true
}`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "middleware/ratelimit.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created 2 defs") {
		t.Fatalf("expected 'Created 2 defs' (import block skipped), got: %s", text)
	}
	for _, name := range []string{"RateLimiter", "Allow"} {
		if _, err := db.GetDefinitionByName(name, ""); err != nil {
			t.Errorf("%s not created: %v", name, err)
		}
	}
}

// TestHandleCreateScaffoldsImportsOnlyBody: imports-only body with
// file: set is a valid scaffold — writes the file to file_sources
// so subsequent create ops can append decls into it. Prior behavior
// (error) blocked a common real workflow (author a new package by
// seeding the file first, then filling defs).
func TestHandleCreateMultiDeclImportsOnlyFails(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	body := `package middleware

import (
	"net/http"
	"sync"
)`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "middleware/ratelimit.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Scaffolded") {
		t.Fatalf("expected scaffold success, got: %s", text)
	}
	if !strings.Contains(text, "middleware/ratelimit.go") {
		t.Fatalf("scaffold response should mention target file, got: %s", text)
	}
}

// Bug C: op:create with file: param must route the new def into that file
// (SourceFile populated on the stored Definition).
func TestHandleCreateHonorsFileParam(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func PlacedHere() int { return 1 }",
		File: "main.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created") {
		t.Fatalf("expected 'Created', got: %s", text)
	}
	if !strings.Contains(text, "main.go") {
		t.Errorf("expected file path in output, got: %s", text)
	}

	d, err := db.GetDefinitionByName("PlacedHere", "")
	if err != nil {
		t.Fatalf("def not found: %v", err)
	}
	if d.SourceFile != "main.go" {
		t.Errorf("SourceFile = %q, want %q", d.SourceFile, "main.go")
	}
}

func TestHandleRename(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}
	s.ready.Store(true) // setupTestDB ingest+resolve is synchronous; skip the wait

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName: "Greet",
		NewName: "SayHi",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Renamed") {
		t.Errorf("expected 'Renamed', got: %s", text)
	}

	// Verify caller was updated too.
	d, _ := db.GetDefinitionByName("Farewell", "")
	if !strings.Contains(d.Body, "SayHi") {
		t.Error("caller not updated after rename")
	}
}

// #105 winze ask #4.3: retarget-field-value rewrites a composite-literal
// field's string value across every def whose body matches. Flagship
// winze op: "change Object: A to Object: B across every claim" without
// sed-across-files + praying no unrelated text collides.
func TestHandleRetargetFieldValue_RewritesMatchingComposites(t *testing.T) {
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
var C2 = Claim{Subject: "s2", Object: "OldTarget"}
var C3 = Claim{Subject: "s3", Object: "Different"}

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

	result, _, _ := s.handleRetargetFieldValue(context.Background(), nil, codeParam{
		Name:  "Claim",
		Field: "Object",
		Old:   "OldTarget",
		New:   "NewTarget",
	})
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("unexpected error: %s", text)
	}
	if !strings.Contains(text, "2 def(s)") {
		t.Errorf("expected '2 def(s)' updated, got %q", text)
	}

	// C1 + C2 rewritten, C3 untouched.
	for _, want := range []struct {
		name, body string
	}{
		{"C1", "NewTarget"},
		{"C2", "NewTarget"},
		{"C3", "Different"},
	} {
		d, err := db.GetDefinitionByName(want.name, "")
		if err != nil {
			t.Fatalf("%s not found: %v", want.name, err)
		}
		if !strings.Contains(d.Body, want.body) {
			t.Errorf("%s body should contain %q, got %s", want.name, want.body, d.Body)
		}
	}
	// C1 must NOT still contain OldTarget.
	d, _ := db.GetDefinitionByName("C1", "")
	if strings.Contains(d.Body, "OldTarget") {
		t.Errorf("C1 still has OldTarget: %s", d.Body)
	}
}

func TestHandleRetargetFieldValue_RejectsMissingParams(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}
	s.ready.Store(true)

	// Missing field
	r, _, _ := s.handleRetargetFieldValue(context.Background(), nil, codeParam{Name: "Foo", Old: "x", New: "y"})
	if !r.IsError {
		t.Error("expected error when field missing")
	}
	// Missing name
	r, _, _ = s.handleRetargetFieldValue(context.Background(), nil, codeParam{Field: "F", Old: "x", New: "y"})
	if !r.IsError {
		t.Error("expected error when name missing")
	}
}

// #105 winze ask #4: safe-delete refuses when references remain, unless
// caller opts in via force:true. A KB where deletes leave dangling
// references is worse than one where you must fix references first.
func TestHandleDelete_SafeRefusesWhenReferenced(t *testing.T) {
	db, projDir := setupTestDBWithVars(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// OriginalClaim is referenced by Reference1 + Reference2. Delete must refuse.
	result, _, _ := s.handleDelete(context.Background(), nil, nameParam{Name: "OriginalClaim"})
	text := resultText(t, result)
	if !result.IsError {
		t.Fatalf("expected error result, got success: %s", text)
	}
	if !strings.Contains(text, "refused") {
		t.Errorf("expected 'refused' in error, got %q", text)
	}
	if !strings.Contains(text, "Reference1") || !strings.Contains(text, "Reference2") {
		t.Errorf("expected caller names in error, got %q", text)
	}

	// Def must still exist — refusal is atomic.
	if _, err := db.GetDefinitionByName("OriginalClaim", ""); err != nil {
		t.Errorf("OriginalClaim should still exist after refused delete: %v", err)
	}
}

func TestHandleDelete_ForceBypassesSafetyCheck(t *testing.T) {
	db, projDir := setupTestDBWithVars(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// force:true — deletes even with live references. Legacy escape hatch.
	result, _, _ := s.handleDelete(context.Background(), nil, nameParam{Name: "OriginalClaim", Force: true})
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected success with force:true, got error: %s", text)
	}
	if !strings.Contains(text, "Deleted") {
		t.Errorf("expected 'Deleted', got %q", text)
	}

	if _, err := db.GetDefinitionByName("OriginalClaim", ""); err == nil {
		t.Error("OriginalClaim should be gone after force delete")
	}
}

func TestHandleDelete_SucceedsWhenNoReferences(t *testing.T) {
	db, projDir := setupTestDBWithVars(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Reference2 has no callers — safe delete without force.
	result, _, _ := s.handleDelete(context.Background(), nil, nameParam{Name: "Reference2"})
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("Reference2 has no callers — should delete without force: %s", text)
	}
}

func TestHandleRename_PackageLevelVar(t *testing.T) {
	db, projDir := setupTestDBWithVars(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName: "OriginalClaim",
		NewName: "RefinedClaim",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Renamed") {
		t.Fatalf("expected 'Renamed', got: %s", text)
	}

	// The var itself renamed -- both its Name column AND its own body
	// text. #304-class regression: astRename's local-decl detector used
	// to wrongly classify a bare package-level var's own top-level
	// ValueSpec as a "local" declaration and skip renaming it, so the
	// Name column changed but the literal "var OriginalClaim = ..."
	// text never did.
	d, err := db.GetDefinitionByName("RefinedClaim", "")
	if err != nil {
		t.Fatalf("RefinedClaim not found after rename: %v", err)
	}
	if d.Kind != "var" {
		t.Errorf("expected kind var, got %s", d.Kind)
	}
	if strings.Contains(d.Body, "OriginalClaim") {
		t.Errorf("renamed var's own body still contains the old name: %s", d.Body)
	}
	if !strings.Contains(d.Body, "RefinedClaim") {
		t.Errorf("renamed var's own body should declare RefinedClaim: %s", d.Body)
	}

	// Every referencing var should now name RefinedClaim, not OriginalClaim.
	for _, name := range []string{"Reference1", "Reference2"} {
		d, err := db.GetDefinitionByName(name, "")
		if err != nil {
			t.Fatalf("%s not found: %v", name, err)
		}
		if strings.Contains(d.Body, "OriginalClaim") {
			t.Errorf("%s still references OriginalClaim: %s", name, d.Body)
		}
		if !strings.Contains(d.Body, "RefinedClaim") {
			t.Errorf("%s should reference RefinedClaim: %s", name, d.Body)
		}
	}
}

// setupTestDBWithVars is a winze-shape fixture: package-level vars that
// cross-reference each other, no functions to speak of. Used by #105
// mutation-op tests.
func setupTestDBWithVars(t *testing.T) (store.Backend, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "claims.go"), []byte(`package main

type Claim struct {
	Subject string
	Object  string
}

var OriginalClaim = Claim{Subject: "s", Object: "o"}

var Reference1 = Claim{Subject: "one", Object: OriginalClaim.Object}

var Reference2 = Claim{Subject: "two", Object: OriginalClaim.Subject}
`), 0644)
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}
	return db, projDir
}

func resultText(t *testing.T, result *sdkmcp.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) == 0 {
		t.Fatal("nil or empty result")
	}
	for _, c := range result.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatal("no text content in result")
	return ""
}

func TestTopLevelFlow(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "empty body",
			body: "",
			want: nil,
		},
		{
			name: "not parseable",
			body: "not go source",
			want: nil,
		},
		{
			name: "simple return",
			body: "func F() int { return 42 }",
			want: []string{"L0:return"},
		},
		{
			name: "err check pattern",
			body: `func F() error {
	x, err := doThing()
	if err != nil {
		return err
	}
	return nil
}`,
			want: []string{"L1:assign", "L2:if", "L5:return"},
		},
		{
			name: "loop + defer + go",
			body: `func F() {
	defer cleanup()
	go bg()
	for i := 0; i < 10; i++ {
		process(i)
	}
	select {
	case <-ch:
	}
}`,
			want: []string{"L1:defer", "L2:go", "L3:for", "L6:select"},
		},
		{
			// #299: guards against a large dispatch switch collapsing into
			// one opaque token -- see switchCaseLabels' doc comment for the
			// prometheus-18534 motivation.
			name: "switch surfaces case labels",
			body: `func F(x int) {
	switch x {
	case 1:
		a()
	case 2, 3:
		b()
	default:
		c()
	}
}`,
			want: []string{"L1:switch", "L2:case 1", "L4:case 2, 3", "L6:case default"},
		},
		{
			name: "type switch surfaces case labels",
			body: `func F(x interface{}) {
	switch v := x.(type) {
	case *Foo:
		useFoo(v)
	case *Bar:
		useBar(v)
	}
}`,
			want: []string{"L1:typeswitch", "L2:case *Foo", "L4:case *Bar"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := topLevelFlow(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("length: got %d %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHandleOutline_SmallBodyFallsBackToRead(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// Greet's body is well under outlineBodyThreshold (300 bytes) so
	// the size-aware fallback should return the read view — which
	// includes the full body inside a fenced code block.
	result, _, _ := s.handleOutline(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "Hello, ") {
		t.Errorf("expected small body to fall back to read view (should include body content); got:\n%s", text)
	}
	if strings.Contains(text, "Body:") && strings.Contains(text, "fetch with") {
		t.Errorf("expected fallback to read, but output looks like outline (has 'Body: ... fetch with'):\n%s", text)
	}
}

func TestHandleOutline_LargeBodyReturnsOutline(t *testing.T) {
	// Build a project with one large function that trips the outline
	// threshold, to exercise the outline branch (not the small-body
	// fallback). setupTestDB's Greet/Farewell are both too small.
	db, projDir := setupTestDB(t)
	defer db.Close()

	// Overwrite main.go with a chunkier function that will comfortably
	// exceed outlineBodyThreshold (300 bytes) and has a mix of stmts
	// for the flow section to detect.
	big := `package main

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
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(big), 0644)
	os.Remove(filepath.Join(projDir, "main_test.go"))

	// Re-ingest.
	if _, err := ingest.IngestFile(db, projDir, filepath.Join(projDir, "main.go")); err != nil {
		t.Fatal("re-ingest:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, _ := s.handleOutline(context.Background(), nil, nameParam{Name: "Chunky"})
	text := resultText(t, result)

	// Outline output must NOT contain the body statements — that would
	// mean we fell through to read despite the body being large enough.
	if strings.Contains(text, "continue") || strings.Contains(text, "total++") {
		t.Errorf("expected outline (no body content); got read-shaped output:\n%s", text)
	}
	// Must contain the outline-specific lines.
	for _, want := range []string{"Body:", "fetch with", "Callers:", "Callees"} {
		if !strings.Contains(text, want) {
			t.Errorf("outline missing %q in:\n%s", want, text)
		}
	}
	// Flow section must be present and list at least one recognized
	// statement kind from the fixture.
	if !strings.Contains(text, "Flow (") {
		t.Errorf("outline missing Flow section:\n%s", text)
	}
	for _, kind := range []string{"range", "if", "defer", "go", "select", "return", "assign"} {
		if !strings.Contains(text, kind) {
			t.Errorf("flow section missing %q kind:\n%s", kind, text)
		}
	}
}

// TestHandleOutline_QueryFilter validates the #157 query-context on
// outline: callees list narrowed to those matching query token; count
// header shows "N of M filtered by query" when hits < total.
func TestHandleOutline_QueryFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	mod, _ := db.EnsureModule("example.com/svc", "svc", "")
	target := &store.Definition{
		ModuleID: mod.ID, Name: "Handler", Kind: "function",
		Body: strings.Repeat("// pad the body past outlineBodyThreshold\n", 20) +
			`func Handler() { authenticate(); logRequest(); parseJSON() }`,
		Signature: "func Handler()",
	}
	target.Hash = store.HashBody(target.Body)
	targetID, _ := db.UpsertDefinition(target)

	// Three callees; only one matches "auth".
	callees := []*store.Definition{
		{ModuleID: mod.ID, Name: "authenticate", Kind: "function", Body: "func authenticate() {}"},
		{ModuleID: mod.ID, Name: "logRequest", Kind: "function", Body: "func logRequest() {}"},
		{ModuleID: mod.ID, Name: "parseJSON", Kind: "function", Body: "func parseJSON() {}"},
	}
	for _, c := range callees {
		c.Hash = store.HashBody(c.Body)
		cid, _ := db.UpsertDefinition(c)
		_ = db.SetReferences(targetID, []store.Reference{{FromDef: targetID, ToDef: cid, Kind: "call"}})
	}
	// SetReferences replaces prior calls, so wire ALL three in one pass.
	var allRefs []store.Reference
	for _, c := range callees {
		cid, _ := db.UpsertDefinition(c)
		allRefs = append(allRefs, store.Reference{FromDef: targetID, ToDef: cid, Kind: "call"})
	}
	_ = db.SetReferences(targetID, allRefs)

	result, _, err := s.handleOutline(context.Background(), nil,
		nameParam{Name: "Handler", Query: "auth"})
	if err != nil {
		t.Fatalf("handleOutline query: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "filtered by query=\"auth\"") {
		t.Errorf("expected 'filtered by query' header in Callees line\n---\n%s", text)
	}
	if !strings.Contains(text, "authenticate") {
		t.Errorf("expected authenticate to survive filter\n---\n%s", text)
	}
	if strings.Contains(text, "logRequest") || strings.Contains(text, "parseJSON") {
		t.Errorf("non-matching callees should be hidden\n---\n%s", text)
	}
}

// Overview's query filter follows the same pattern proven by
// TestHandleOutline_QueryFilter and TestHandleImpact_QueryFilter.
// A dedicated overview-integration test is blocked by the known
// dir-derivation bug for root-level files in handleOverview
// (setupTestDB's main.go lives at project root; handleOverview
// derives dir="main" from "main.go" and can't find defs whose
// module.path is "testproj"). When that file-lookup wart is
// fixed under a separate task, add a proper integration test.

func TestHandleSlice_MissingArgs(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// Missing name.
	result, _, _ := s.handleSlice(context.Background(), nil, codeParam{Slice: "return"})
	if !strings.Contains(resultText(t, result), "name is required") {
		t.Errorf("expected 'name is required' error")
	}

	// Missing slice kind.
	result, _, _ = s.handleSlice(context.Background(), nil, codeParam{Name: "Greet"})
	if !strings.Contains(resultText(t, result), "kind is required") {
		t.Errorf("expected 'kind is required' error")
	}
}

func TestHandleSlice_ReturnStmt(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// Greet has one return statement.
	result, _, _ := s.handleSlice(context.Background(), nil, codeParam{Name: "Greet", Slice: "return"})
	text := resultText(t, result)

	if !strings.Contains(text, "return") {
		t.Errorf("expected return keyword in slice output:\n%s", text)
	}
	if !strings.Contains(text, `"Hello, "`) {
		t.Errorf("expected return expression content:\n%s", text)
	}
	if !strings.Contains(text, "slice: return, 1 match") {
		t.Errorf("expected match count header:\n%s", text)
	}
}

// TestHandleRename_EmitsOnlyNewName regression test for the chain-bench
// failure surfaced on 2026-07-08: rename left the OLD def name in the
// emitted file alongside the new one because the emit path treats the
// old on-disk decl as untracked. Same shape as the delete-race fixed in
// b274ccc; the rename fix (this file) passes the old qualified name
// through emit.Opts.AllowedRemovals.
func TestHandleRename_EmitsOnlyNewName(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true) // setupTestDB ingest+resolve is synchronous; skip the wait

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName: "Greet",
		NewName: "SayHi",
	})
	if result == nil {
		t.Fatal("nil result")
	}

	final, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(final)
	if strings.Contains(src, "func Greet(") {
		t.Errorf("emitted main.go still contains old def:\n%s", src)
	}
	if !strings.Contains(src, "func SayHi(") {
		t.Errorf("emitted main.go missing new def:\n%s", src)
	}
	if !strings.Contains(src, "SayHi(name)") {
		t.Errorf("emitted main.go missing updated caller (should call SayHi):\n%s", src)
	}
	if strings.Contains(src, "Greet(name)") {
		t.Errorf("emitted main.go still has old caller reference:\n%s", src)
	}
}

// TestHandleRename_SurvivesReopen synthetic repro of the chain-bench
// failure. Mirrors what the bench does step-for-step, but with NO MCP
// goroutines (no watchFiles, no ingestAndResolve at startup) and NO
// claude -p — just the store/emit/handler layer in a plain Go test.
//
// The bench fails: rename returns success, but a later `defn query`
// (fresh connection) shows the OLD name still on the original id AND
// a stray new id for the new name. This test isolates whether the bug
// requires MCP-level concurrency or reproduces in pure code paths.
//
// Bisect logic: if this test FAILS, the bug is in store/emit —
// something about Dolt session persistence, working-set commit, or
// journal flush. If this test PASSES, the bug requires goroutine
// concurrency (watchFiles polling, background ingestAndResolve, etc.).
func TestHandleRename_SurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")

	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, projDir, "go.mod", "module fixture\n\ngo 1.26\n")
	writeFixtureFile(t, projDir, "core.go", `package fixture

// ProcessData is the core operation.
func ProcessData(x int) int {
	return x * 2
}
`)
	writeFixtureFile(t, projDir, "caller_a.go", `package fixture

func RunA(x int) int {
	return ProcessData(x) + 1
}
`)
	writeFixtureFile(t, projDir, "caller_b.go", `package fixture

func RunB(x int) int {
	total := 0
	for i := 0; i < x; i++ {
		total += ProcessData(i)
	}
	return total
}
`)

	// --- FIRST SESSION: ingest, rename, close ---
	db1, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := ingest.Ingest(db1, projDir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := resolve.Resolve(db1, projDir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	s := &server{backend: db1, projectDir: projDir}
	s.ready.Store(true)
	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName: "ProcessData",
		NewName: "Handle",
	})
	if result == nil {
		t.Fatal("nil result from handleRename")
	}
	if txt := resultText(t, result); !strings.Contains(txt, "Renamed") {
		t.Fatalf("expected Renamed, got: %s", txt)
	}
	if err := db1.Close(); err != nil {
		t.Logf("db1.Close error (non-fatal): %v", err)
	}

	// --- SECOND SESSION: reopen, query — does the rename survive? ---
	db2, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer db2.Close()

	defs, err := db2.FilterDefinitions("", "function", "", 0)
	if err != nil {
		t.Fatalf("filter defs: %v", err)
	}
	names := make(map[string]int)
	for _, d := range defs {
		if d.Test {
			continue
		}
		names[d.Name]++
	}
	if names["ProcessData"] > 0 {
		t.Errorf("post-reopen DB still has ProcessData (should have been renamed to Handle): %v", names)
	}
	if names["Handle"] != 1 {
		t.Errorf("post-reopen DB should have exactly one Handle def, got: %v", names)
	}

	// Also verify on-disk core.go doesn't have both.
	if data, err := os.ReadFile(filepath.Join(projDir, "core.go")); err == nil {
		src := string(data)
		if strings.Contains(src, "func ProcessData(") {
			t.Errorf("core.go still contains func ProcessData:\n%s", src)
		}
		if !strings.Contains(src, "func Handle(") {
			t.Errorf("core.go missing func Handle:\n%s", src)
		}
	}
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestHandleRename_SurvivesReopen_WithBackgroundIngest layers the MCP
// startup pattern onto the synthetic repro: fires
// `go s.ingestAndResolve()` right before handleRename, mirroring what
// newMCPServer does. If the bench-level failure reproduces here, the
// bug is the goroutine race (ingest writing to Dolt's session state
// while handleRename is doing its work); if this ALSO passes, the bug
// requires either watchFiles polling or the specific shutdown path
// defn serve → RunShared → SIGTERM triggers.
func TestHandleRename_SurvivesReopen_WithBackgroundIngest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, projDir, "go.mod", "module fixture\n\ngo 1.26\n")
	writeFixtureFile(t, projDir, "core.go", `package fixture

// ProcessData is the core operation.
func ProcessData(x int) int {
	return x * 2
}
`)
	writeFixtureFile(t, projDir, "caller_a.go", `package fixture

func RunA(x int) int {
	return ProcessData(x) + 1
}
`)
	writeFixtureFile(t, projDir, "caller_b.go", `package fixture

func RunB(x int) int {
	total := 0
	for i := 0; i < x; i++ {
		total += ProcessData(i)
	}
	return total
}
`)

	db1, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	// Seed the DB the way defn ingest CLI does (before serve starts).
	if err := ingest.Ingest(db1, projDir); err != nil {
		t.Fatalf("initial ingest: %v", err)
	}
	if err := resolve.Resolve(db1, projDir); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}

	// Now spin up a server the way newMCPServer does: fire the async
	// startup ingest+resolve, then serve requests.
	s := &server{backend: db1, projectDir: projDir}
	go func() {
		if err := s.ingestAndResolve(); err != nil {
			t.Logf("startup ingestAndResolve: %v", err)
		}
		s.ready.Store(true)
	}()

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName: "ProcessData",
		NewName: "Handle",
	})
	if result == nil {
		t.Fatal("nil result from handleRename")
	}

	// Give any lingering goroutine time to potentially clobber state.
	time.Sleep(200 * time.Millisecond)

	if err := db1.Close(); err != nil {
		t.Logf("db1.Close error (non-fatal): %v", err)
	}

	db2, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer db2.Close()

	defs, err := db2.FilterDefinitions("", "function", "", 0)
	if err != nil {
		t.Fatalf("filter defs: %v", err)
	}
	names := make(map[string]int)
	for _, d := range defs {
		if d.Test {
			continue
		}
		names[d.Name]++
	}
	if names["ProcessData"] > 0 {
		t.Errorf("post-reopen DB still has ProcessData (should have been renamed to Handle): %v", names)
	}
	if names["Handle"] != 1 {
		t.Errorf("post-reopen DB should have exactly one Handle def, got: %v", names)
	}
}

func TestTruncateTestOutput(t *testing.T) {
	// Small output has RUN-announcement noise stripped but keeps
	// --- PASS/summary lines (which still name which test ran).
	small := "=== RUN   TestFoo\n--- PASS: TestFoo (0.00s)\nPASS\nok  \tpkg\t0.001s\n"
	wantSmall := "--- PASS: TestFoo (0.00s)\nPASS\nok  \tpkg\t0.001s\n"
	if got := truncateTestOutput(small); got != wantSmall {
		t.Errorf("small output should have RUN-line noise stripped; got %q, want %q", got, wantSmall)
	}

	// Large output gets summarized: head + failures preserved, middle dropped.
	var large strings.Builder
	for i := 0; i < 200; i++ {
		large.WriteString(fmt.Sprintf("=== RUN   TestFoo_%d\n", i))
		large.WriteString("    foo_test.go:10: some noisy output that would inflate wire cost\n")
		if i == 100 {
			large.WriteString("--- FAIL: TestFoo_100 (0.01s)\n")
		} else {
			large.WriteString(fmt.Sprintf("--- PASS: TestFoo_%d (0.00s)\n", i))
		}
	}
	large.WriteString("FAIL\tpkg\t0.500s\n")
	large.WriteString("FAIL\n")
	got := truncateTestOutput(large.String())
	if len(got) >= len(large.String()) {
		t.Errorf("expected truncation, got same-or-larger length %d vs %d", len(got), len(large.String()))
	}
	if !strings.Contains(got, "--- FAIL: TestFoo_100") {
		t.Errorf("truncated output must preserve failed-test names, got:\n%s", got)
	}
	if !strings.Contains(got, "FAIL\tpkg\t0.500s") {
		t.Errorf("truncated output must preserve package-level result, got:\n%s", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncated output must include truncation marker, got:\n%s", got)
	}

	// All-pass large output gets collapsed by collapseAllPassLines (#311)
	// before the cap check even runs, so this now exercises the collapse
	// path rather than the windowed-summary path -- "no failures" comes
	// from collapseAllPassLines's own message, not the cap-exceeded
	// branch's. See TestCollapseAllPassLines for the collapse's own
	// dedicated coverage.
	var allPass strings.Builder
	for i := 0; i < 400; i++ {
		allPass.WriteString(fmt.Sprintf("=== RUN   TestFoo_%d\n--- PASS: TestFoo_%d (0.00s)\n", i, i))
	}
	allPass.WriteString("PASS\nok  \tpkg\t0.500s\n")
	got = truncateTestOutput(allPass.String())
	if !strings.Contains(got, "no failures") {
		t.Errorf("all-pass truncation should say 'no failures', got:\n%s", got)
	}
	if !strings.Contains(got, "ok  \tpkg\t0.500s") {
		t.Errorf("all-pass truncation must preserve package result, got:\n%s", got)
	}
}

func TestSearchShapedSQLRedirect(t *testing.T) {
	tests := []struct {
		name      string
		sql       string
		wantHint  string
		wantEmpty bool
	}{
		// Body grep (the cli-3461 anti-pattern)
		{"body_like", "SELECT d.name FROM definitions d JOIN bodies b ON b.def_id = d.id WHERE b.body LIKE '%api/v3%'", "op:\"search\"", false},
		{"body_like_lower", "select * from bodies where body like '%foo%'", "op:\"search\"", false},

		// Direct name lookup
		{"name_eq", "SELECT * FROM definitions WHERE name = 'GetJobs'", "op:\"read\"", false},
		{"d_name_eq", "SELECT d.name, b.body FROM definitions d JOIN bodies b ON b.def_id=d.id WHERE d.name = 'GetJobs'", "op:\"read\"", false},
		// 2026-08-05: name IN (...) and name LIKE were confirmed (via a
		// real-transcript corpus scan) to slip past the old =-only regex —
		// 16/51 post-guard real query calls used exactly this shape.
		{"name_in", "SELECT name, source_file FROM definitions WHERE name IN ('OpenBackend','OpenSQLite')", "op:\"read\"", false},
		{"d_name_in", "SELECT d.name, d.receiver, m.path FROM definitions d JOIN modules m ON d.module_id=m.id WHERE d.name IN ('ComputeMinHash','MinHashJaccard')", "op:\"read\"", false},
		{"name_like", "SELECT id, name, source_file FROM definitions WHERE name LIKE 'TestHandleAddImport%'", "op:\"read\"", false},
		{"backtick_name_like", "SELECT `name`, `kind`, `receiver`, `source_file` FROM definitions WHERE `name` LIKE '%mport%' LIMIT 20", "op:\"read\"", false},

		// Schema introspection
		{"show_tables", "SHOW TABLES", "schema is documented", false},
		{"describe", "DESCRIBE bodies", "schema is documented", false},
		{"info_schema", "SELECT * FROM INFORMATION_SCHEMA.COLUMNS", "schema is documented", false},

		// File-scoped SQL (the go-zero-1907 anti-pattern)
		{"file_scoped_like", "select d.name, d.kind from definitions d where d.source_file like 'zrpc/%' and d.name like '%Interceptor%'", "op:\"file-defs\"", false},
		{"file_scoped_eq", "select d.name, d.start_line from definitions d where d.source_file='zrpc/client.go' order by d.start_line", "op:\"file-defs\"", false},
		{"file_scoped_in", "select d.name from definitions d where d.source_file in ('a.go','b.go') and d.start_line > 82", "op:\"file-defs\"", false},

		// Legitimate analytics — should pass through
		{"count_by_kind", "SELECT `kind`, COUNT(*) FROM definitions GROUP BY `kind`", "", true},
		{"orphan_refs", "SELECT * FROM refs WHERE target_id NOT IN (SELECT id FROM definitions)", "", true},
		{"file_scan", "SELECT DISTINCT source_file FROM definitions", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := searchShapedSQLRedirect(tt.sql)
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("expected pass-through, got redirect: %q", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("expected redirect containing %q, got empty (SQL not intercepted)", tt.wantHint)
			}
			if !strings.Contains(got, tt.wantHint) {
				t.Errorf("redirect missing hint %q, got: %q", tt.wantHint, got)
			}
		})
	}
}

// TestHandleMethods locks in task #79's projection contract: for a
// struct/named type, list every method's compact signature (grouped
// exported first, alphabetical within), one line each. For an
// interface, parse the body's inline method decls.
func TestHandleMethods(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	mod, err := db.EnsureModule("example.com/svc", "svc", "")
	if err != nil {
		t.Fatal(err)
	}

	// Seed a type + methods on both value and pointer receivers,
	// exported and unexported, so we exercise the grouping + sort.
	typeDef := &store.Definition{
		ModuleID: mod.ID, Name: "Server", Kind: "type", Exported: true,
		Body: "type Server struct { addr string }",
	}
	typeDef.Hash = store.HashBody(typeDef.Body)
	if _, err := db.UpsertDefinition(typeDef); err != nil {
		t.Fatal(err)
	}
	methods := []*store.Definition{
		{ModuleID: mod.ID, Name: "Start", Kind: "method", Exported: true,
			Receiver: "*Server", Body: "func (s *Server) Start(addr string) error { return nil }",
			Signature: "func (s *Server) Start(addr string) error", Doc: "// Start binds the listener and blocks."},
		{ModuleID: mod.ID, Name: "Stop", Kind: "method", Exported: true,
			Receiver: "*Server", Body: "func (s *Server) Stop() { }",
			Signature: "func (s *Server) Stop()", Doc: ""},
		{ModuleID: mod.ID, Name: "Addr", Kind: "method", Exported: true,
			Receiver: "Server", Body: "func (s Server) Addr() string { return s.addr }",
			Signature: "func (s Server) Addr() string", Doc: ""},
		{ModuleID: mod.ID, Name: "reset", Kind: "method", Exported: false,
			Receiver: "*Server", Body: "func (s *Server) reset() { s.addr = \"\" }",
			Signature: "func (s *Server) reset()", Doc: "// reset zeros internal state."},
	}
	for _, m := range methods {
		m.Hash = store.HashBody(m.Body)
		if _, err := db.UpsertDefinition(m); err != nil {
			t.Fatalf("upsert %s: %v", m.Name, err)
		}
	}

	// Basic call.
	result, _, err := s.handleMethods(context.Background(), nil, nameParam{Name: "Server"})
	if err != nil {
		t.Fatalf("handleMethods err: %v", err)
	}
	text := resultText(t, result)
	for _, want := range []string{"Server", "4 methods", "3 exported", "1 unexported", "Start", "Stop", "Addr", "reset", "binds the listener", "zeros internal state"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q\n---\n%s", want, text)
		}
	}
	// Exported group must come before unexported (Start appears before reset).
	if strings.Index(text, "Start") > strings.Index(text, "reset") {
		t.Errorf("expected exported methods before unexported\n---\n%s", text)
	}

	// Pointer-receiver call — same result.
	result2, _, _ := s.handleMethods(context.Background(), nil, nameParam{Name: "*Server"})
	if resultText(t, result2) != text {
		t.Errorf("'*Server' should produce same output as 'Server'")
	}

	// Missing type → helpful error.
	result3, _, _ := s.handleMethods(context.Background(), nil, nameParam{Name: "NoSuchType"})
	if !strings.Contains(resultText(t, result3), "no methods found") {
		t.Errorf("expected 'no methods found' error, got: %q", resultText(t, result3))
	}

	// Empty name → error.
	result4, _, _ := s.handleMethods(context.Background(), nil, nameParam{Name: ""})
	if !strings.Contains(resultText(t, result4), "name is required") {
		t.Errorf("expected 'name is required' error, got: %q", resultText(t, result4))
	}
}

// TestHandleMethods_QueryFilter validates #157: query filters method
// list to those whose name/doc/signature contains any query token.
func TestHandleMethods_QueryFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	mod, _ := db.EnsureModule("example.com/svc", "svc", "")
	typeDef := &store.Definition{
		ModuleID: mod.ID, Name: "Server", Kind: "type", Exported: true,
		Body: "type Server struct{}",
	}
	typeDef.Hash = store.HashBody(typeDef.Body)
	_, _ = db.UpsertDefinition(typeDef)
	methods := []*store.Definition{
		{ModuleID: mod.ID, Name: "Start", Kind: "method", Exported: true, Receiver: "*Server",
			Body: "func (s *Server) Start() {}", Signature: "func (s *Server) Start()",
			Doc: "// Start binds the listener and blocks."},
		{ModuleID: mod.ID, Name: "Stop", Kind: "method", Exported: true, Receiver: "*Server",
			Body: "func (s *Server) Stop() {}", Signature: "func (s *Server) Stop()",
			Doc: "// Stop shuts down gracefully."},
		{ModuleID: mod.ID, Name: "Ping", Kind: "method", Exported: true, Receiver: "*Server",
			Body: "func (s *Server) Ping() error { return nil }", Signature: "func (s *Server) Ping() error",
			Doc: "// Ping returns nil if healthy."},
	}
	for _, m := range methods {
		m.Hash = store.HashBody(m.Body)
		_, _ = db.UpsertDefinition(m)
	}

	// Query "listener" — only Start's doc matches.
	result, _, err := s.handleMethods(context.Background(), nil,
		nameParam{Name: "Server", Query: "listener"})
	if err != nil {
		t.Fatalf("handleMethods query: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "[query=\"listener\"]") {
		t.Errorf("expected [query=...] header on filtered listing\n---\n%s", text)
	}
	if !strings.Contains(text, "Start") {
		t.Errorf("expected Start (matches 'listener' in doc)\n---\n%s", text)
	}
	if strings.Contains(text, "Ping") {
		t.Errorf("Ping should NOT appear (no 'listener' match)\n---\n%s", text)
	}
}

// TestHandleMethods_Interface exercises the interface body-parsing
// path (methods live inline, not as separate rows).
func TestHandleMethods_Interface(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	mod, _ := db.EnsureModule("example.com/svc", "svc", "")
	ifaceBody := `type Handler interface {
	// Handle processes one request.
	Handle(req Request) error
	// Close releases resources.
	Close() error
}`
	iface := &store.Definition{
		ModuleID: mod.ID, Name: "Handler", Kind: "interface", Exported: true,
		Body: ifaceBody,
	}
	iface.Hash = store.HashBody(iface.Body)
	if _, err := db.UpsertDefinition(iface); err != nil {
		t.Fatal(err)
	}

	result, _, err := s.handleMethods(context.Background(), nil, nameParam{Name: "Handler"})
	if err != nil {
		t.Fatalf("interface handleMethods: %v", err)
	}
	text := resultText(t, result)
	for _, want := range []string{"Handler", "interface", "2 methods", "Handle", "Close", "processes one request", "releases resources"} {
		if !strings.Contains(text, want) {
			t.Errorf("interface output missing %q\n---\n%s", want, text)
		}
	}
}

func TestHandleExpand_AllNamesNotFound(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{Names: []string{"NoSuchDef1", "NoSuchDef2"}})
	if err == nil && (result == nil || !result.IsError) {
		t.Fatalf("expected an error/not-found result when every name fails to resolve, got result=%v err=%v", result, err)
	}
}

func TestHandleExpand_MultipleNames(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{Names: []string{"Greet", "Farewell"}})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "## Greet") {
		t.Errorf("expected Greet section, got: %s", text)
	}
	if !strings.Contains(text, "## Farewell") {
		t.Errorf("expected Farewell section, got: %s", text)
	}
	if !strings.Contains(text, "---") {
		t.Errorf("expected --- separator between multiple names, got: %s", text)
	}
	if strings.Index(text, "## Greet") > strings.Index(text, "## Farewell") {
		t.Errorf("expected names in request order (Greet before Farewell), got: %s", text)
	}
}

func TestHandleExpand_MultipleNamesSkipsNotFound(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{Names: []string{"Greet", "NoSuchDef"}})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "## Greet") {
		t.Errorf("expected Greet section despite the other name failing, got: %s", text)
	}
	if !strings.Contains(text, "not found, skipped: NoSuchDef") {
		t.Errorf("expected a not-found note for the unresolvable name, got: %s", text)
	}
}

func TestHandleGetDefinition_StrippedRelatedFooterOmitsIt(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// Unstripped: Greet's related footer should surface Farewell, its
	// only caller -- Farewell is never mentioned in Greet's own body.
	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Farewell") {
		t.Fatalf("expected related footer to mention caller Farewell, got: %s", text)
	}

	// Stripped: same read, DEFN_STRIP=related-footer -- Farewell should
	// no longer appear anywhere in the response.
	t.Setenv("DEFN_STRIP", "related-footer")
	result2, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text2 := resultText(t, result2)
	if strings.Contains(text2, "Farewell") {
		t.Errorf("DEFN_STRIP=related-footer should omit the related footer, but Farewell still appears: %s", text2)
	}
}

func TestFileNarrative_CacheHitReturnsWithoutExplainClient(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db} // explainClient is nil -- cache hit must never touch it

	defs, err := db.FindDefinitionsByFile("", "main.go", 0)
	if err != nil || len(defs) == 0 {
		t.Fatalf("expected defs in main.go, got %v (err=%v)", defs, err)
	}

	// Compute the same structural hash fileNarrative would (sorted by
	// name, concatenated bodies), pre-populate a matching row, and
	// confirm fileNarrative returns the cached narrative without ever
	// dereferencing the nil explainClient (which would panic if reached).
	sorted := make([]store.Definition, len(defs))
	copy(sorted, defs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var bodyBuf strings.Builder
	for _, d := range sorted {
		bodyBuf.WriteString(d.Body)
		bodyBuf.WriteString("\n")
	}
	hash := store.HashBodyStructural(bodyBuf.String())

	if err := db.SetFileSummary("main.go", sorted[0].ModuleID, &store.FileSummary{
		Narrative: "Cached narrative for testing.",
		BodyHash:  hash,
		Model:     "test",
	}); err != nil {
		t.Fatalf("SetFileSummary: %v", err)
	}

	got := s.fileNarrative(context.Background(), "main.go", defs)
	if got != "Cached narrative for testing." {
		t.Errorf("expected cached narrative returned without touching explainClient, got %q", got)
	}
}

func TestHandleOverview_NilExplainClientNoNarrative(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db} // explainClient nil -- the common no-API-key case

	result, _, err := s.handleOverview(context.Background(), nil, codeParam{File: "main.go"})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Greet") {
		t.Fatalf("expected the normal per-def listing to still work, got: %s", text)
	}
	// No narrative should have been generated or stored -- explainClient
	// is nil, so handleOverview must skip fileNarrative entirely rather
	// than call it and crash on the nil receiver.
	if fs, _ := db.GetFileSummary("main.go"); fs != nil {
		t.Errorf("expected no file summary written when explainClient is nil, got %+v", fs)
	}
}

func TestProjectNarrative_CacheHitReturnsWithoutExplainClient(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db} // explainClient is nil -- cache hit must never touch it

	if err := db.SetMeta("project_narrative", "1:3\nCached project narrative for testing."); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	got := s.projectNarrative(context.Background(), "- testproj — 3 defs", 1, 3)
	if got != "Cached project narrative for testing." {
		t.Errorf("expected cached narrative returned without touching explainClient, got %q", got)
	}
}

// TestHandleApply_CreateMethodOnExistingFileLandsOnDisk is the
// code(op:"apply") counterpart to TestHandleCreate_MethodOnExistingFileLandsOnDisk
// -- same #213 bug, reached via handleApply's "create" case instead of
// the handleCreate singleton path.
func TestHandleApply_CreateMethodOnExistingFileLandsOnDisk(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			// #12: the build gate is now real -- greeter must actually be
			// declared, or the build legitimately fails and the method
			// (correctly) never lands. Kept as its own op rather than
			// folded into a shared fixture change, since dozens of other
			// tests depend on setupTestDB's exact def/exported counts.
			{Op: "create", Body: "type greeter struct{}", File: "main.go"},
			{Op: "create", Body: "func (g *greeter) Whisper() string { return \"hi\" }", File: "main.go"},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "created Whisper") {
		t.Fatalf("expected creation confirmation, got: %s", text)
	}

	final, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(final)
	if !strings.Contains(src, "func (g *greeter) Whisper()") {
		t.Errorf("emitted main.go missing new method -- silently dropped (the #213 bug):\n%s", src)
	}
}

// TestHandleCreate_MethodOnExistingFileLandsOnDisk is a regression test
// for #213: creating a new METHOD (has a receiver) into a file that
// already has content silently dropped the method from disk, even
// though the DB believed it existed. Root cause: mergeDeclsIntoSource
// keys new-function bodies by emit.FuncIdentity(name, receiver) --
// receiver-qualified for methods (e.g. "*Server.Foo") -- but
// handleCreate passed only the bare inferred name in AllowedAdds, so
// the qualified lookup never matched and the method's body was
// silently skipped instead of being appended to the merged file.
// Fixed by qualifying the AllowedAdds entry with emit.FuncIdentity.
func TestHandleCreate_MethodOnExistingFileLandsOnDisk(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// #12: the build gate is now real -- greeter must actually be
	// declared first, or the build legitimately fails and the method
	// (correctly) never lands. Two calls instead of one apply batch,
	// since handleCreate only accepts a single decl per call.
	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "type greeter struct{}",
		File: "main.go",
	}); err != nil {
		t.Fatalf("create greeter type: %v", err)
	}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func (g *greeter) Shout() string { return \"HI\" }",
		File: "main.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created") {
		t.Fatalf("expected 'Created', got: %s", text)
	}

	d, err := db.GetDefinitionByName("Shout", "")
	if err != nil {
		t.Fatalf("def not found in DB: %v", err)
	}
	if d.Receiver == "" {
		t.Fatalf("expected a receiver on the stored def, got none")
	}

	final, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(final)
	if !strings.Contains(src, "func (g *greeter) Shout()") {
		t.Errorf("emitted main.go missing new method -- silently dropped (the #213 bug):\n%s", src)
	}
}

// TestHandleDelete_PointerReceiverMethodActuallyLeavesFile is a
// probe for a suspected sibling of #213 found while investigating it:
// mergeDeclsIntoSource's removeKey (internal/emit/emit.go) is built via
// recvTypeName, which KEEPS the pointer star (e.g. "*greeter.Shout"),
// but handleDelete/handleRename/handleApply build their
// emit.Opts.AllowedRemovals entries via
// strings.TrimPrefix(d.Receiver, "*") -- star STRIPPED (e.g.
// "greeter.Shout"). If those never match, the splice-based removal
// never fires for POINTER-receiver methods (value-receiver methods
// have no star to strip, so they're unaffected), and
// safeWriteGoFile's own lost-decl check also can't catch it, because
// topLevelDeclNames uses the same star-kept recvTypeName on both the
// before and after content -- the stale decl is simply never spliced
// out, so it's present in both and nothing looks "lost". This test
// creates a pointer-receiver method into an existing file, deletes it,
// and checks whether the method body is actually gone from disk.
func TestHandleDelete_PointerReceiverMethodActuallyLeavesFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// #12: the build gate is now real -- greeter must actually be
	// declared, or the setup create legitimately fails.
	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "type greeter struct{}",
		File: "main.go",
	}); err != nil {
		t.Fatalf("setup: create greeter type: %v", err)
	}

	createResult, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func (g *greeter) Shout() string { return \"HI\" }",
		File: "main.go",
	})
	if !strings.Contains(resultText(t, createResult), "Created") {
		t.Fatalf("setup: create failed: %s", resultText(t, createResult))
	}
	afterCreate, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil || !strings.Contains(string(afterCreate), "func (g *greeter) Shout()") {
		t.Fatalf("setup: method not on disk after create: err=%v src=%s", err, afterCreate)
	}

	deleteResult, _, _ := s.handleDelete(context.Background(), nil, nameParam{Name: "Shout"})
	text := resultText(t, deleteResult)
	if !strings.Contains(text, "Deleted") {
		t.Fatalf("expected 'Deleted', got: %s", text)
	}

	final, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(final)
	if strings.Contains(src, "func (g *greeter) Shout()") {
		t.Errorf("deleted pointer-receiver method still present on disk after delete:\n%s", src)
	}
}

// TestHandleRename_PointerReceiverMethodRemovesOldDecl is the rename
// counterpart to TestHandleDelete_PointerReceiverMethodActuallyLeavesFile
// -- same sibling-of-#213 bug (star-stripped allowedRemovals vs
// star-kept funcIdentity/topLevelDeclNames), reached via handleRename
// instead of handleDelete. Before the fix, renaming a pointer-receiver
// method left the OLD method body on disk alongside the new one.
func TestHandleRename_PointerReceiverMethodRemovesOldDecl(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// #12: the build gate is now real -- greeter must actually be
	// declared, or the setup create legitimately fails.
	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "type greeter struct{}",
		File: "main.go",
	}); err != nil {
		t.Fatalf("setup: create greeter type: %v", err)
	}

	createResult, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func (g *greeter) Shout() string { return \"HI\" }",
		File: "main.go",
	})
	if !strings.Contains(resultText(t, createResult), "Created") {
		t.Fatalf("setup: create failed: %s", resultText(t, createResult))
	}

	renameResult, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName: "Shout",
		NewName: "Yell",
	})
	if renameResult == nil {
		t.Fatal("nil rename result")
	}

	final, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(final)
	if strings.Contains(src, "func (g *greeter) Shout()") {
		t.Errorf("renamed pointer-receiver method still has old decl on disk:\n%s", src)
	}
	if !strings.Contains(src, "func (g *greeter) Yell()") {
		t.Errorf("renamed pointer-receiver method missing new decl on disk:\n%s", src)
	}
}

// TestHandleApply_PartialFailureRollsBackEarlierDBWrite probes #214:
// does handleApply's Begin()/commit()/rollback() wrapper actually
// protect an earlier op's DB write when a LATER op in the same batch
// fails? op1 edits "Greet" (should succeed in isolation); op2 targets
// a name that doesn't exist (guaranteed clean failure, no hunk-format
// ambiguity). If the batch is truly atomic, op1's write must NOT be
// visible in the DB after the batch reports an error.
func TestHandleApply_PartialFailureRollsBackEarlierDBWrite(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("setup: Greet not found: %v", err)
	}
	originalBody := before.Body

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "edit", Name: "Greet", NewBody: "func Greet(name string) string { return \"HELLO\" }"},
			{Op: "edit", Name: "DoesNotExist", NewBody: "func DoesNotExist() {}"},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "not found") {
		t.Fatalf("expected the batch to report the missing def, got: %s", text)
	}

	after, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("Greet vanished after failed batch: %v", err)
	}
	if after.Body != originalBody {
		t.Errorf("#214: Greet's DB body changed to %q despite the batch failing overall (was %q) -- handleApply's rollback did not protect this earlier op's write", after.Body, originalBody)
	}
}

// TestHandleOverview_MultiFileSectionsSortedDeterministically is a
// #180 regression test: handleOverview's byFile grouping used to be
// ranged directly (Go map iteration order, randomized per call), so a
// directory overview spanning multiple files could emit "### file"
// sections in a different order across identical, back-to-back calls
// -- breaking prompt-cache prefix stability on the response. Calls
// handleOverview many times against an unchanged DB and asserts every
// response has "### main.go" before "### main_test.go" (sorted order).
func TestHandleOverview_MultiFileSectionsSortedDeterministically(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleOverview(context.Background(), nil, codeParam{File: "testproj"})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "### main.go") || !strings.Contains(text, "### main_test.go") {
		t.Fatalf("expected main.go + main_test.go grouped with ### headers, got:\n%s", text)
	}
	for i := 0; i < 20; i++ {
		result, _, err := s.handleOverview(context.Background(), nil, codeParam{File: "testproj"})
		if err != nil {
			t.Fatalf("overview: %v", err)
		}
		text := resultText(t, result)
		mainIdx := strings.Index(text, "### main.go")
		testIdx := strings.Index(text, "### main_test.go")
		if mainIdx == -1 || testIdx == -1 || mainIdx > testIdx {
			t.Fatalf("iteration %d: expected \"### main.go\" before \"### main_test.go\" (sorted), got:\n%s", i, text)
		}
	}
}

// TestHandleSearch_MergesNameMatchAndFTSMatch is a #216 regression
// test: a Stage 1 (name/signature LIKE) hit must not suppress Stage 2
// (FTS body/doc) results. Creates two defs for a needle that appears
// in one def's NAME and in a completely different def's BODY only,
// and asserts search returns both -- before the fix, finding the
// name-match alone (Stage 1 non-empty) skipped the FTS stage entirely,
// silently dropping the body-only match.
func TestHandleSearch_MergesNameMatchAndFTSMatch(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	mods, err := db.ListModules()
	if err != nil || len(mods) == 0 {
		t.Fatalf("setup: no modules: %v", err)
	}
	moduleID := mods[0].ID

	const needle = "XyzzyPlughNeedle"

	if _, err := db.UpsertDefinition(&store.Definition{
		ModuleID:   moduleID,
		Name:       needle + "Func",
		Kind:       "function",
		Exported:   true,
		Signature:  "func " + needle + "Func()",
		Body:       "func " + needle + "Func() {}",
		SourceFile: "extra1.go",
	}); err != nil {
		t.Fatalf("upsert name-match def: %v", err)
	}
	if _, err := db.UpsertDefinition(&store.Definition{
		ModuleID:   moduleID,
		Name:       "UnrelatedBodyOnly",
		Kind:       "function",
		Exported:   true,
		Signature:  "func UnrelatedBodyOnly()",
		Body:       "func UnrelatedBodyOnly() {\n\t_ = \"" + needle + "\"\n}",
		SourceFile: "extra2.go",
	}); err != nil {
		t.Fatalf("upsert body-only def: %v", err)
	}

	result, _, _ := s.handleSearch(context.Background(), nil, codeParam{Pattern: needle})
	text := resultText(t, result)
	if !strings.Contains(text, needle+"Func") {
		t.Errorf("expected the name-match def in results, got:\n%s", text)
	}
	if !strings.Contains(text, "UnrelatedBodyOnly") {
		t.Errorf("expected the FTS body-only match in results (this is the #216 bug), got:\n%s", text)
	}
}

// TestHandleEdit_ReceiverDisambiguatesSameNamedMethod is the #219
// regression: gemot-2847127 reported op:edit name:"Reconsider"
// receiver:"LLM" updating (*Agent).Reconsider instead of
// (*LLM).Reconsider -- codeParam.Receiver was accepted by the schema
// but never threaded through to handleEdit, which fell back to
// GetDefinitionByName's blast-radius tiebreak (most callers wins,
// receiver-blind). With Receiver wired through, the edit must land on
// the named receiver's method only.
func TestHandleEdit_ReceiverDisambiguatesSameNamedMethod(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

type LLM struct{}
type Agent struct{}

func (l *LLM) Reconsider() string { return "llm-original" }

func (a *Agent) Reconsider() string { return "agent-original" }

func main() {}
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleEdit(context.Background(), nil, editParam{
		Name:     "Reconsider",
		Receiver: "LLM",
		NewBody:  "func (l *LLM) Reconsider() string { return \"llm-updated\" }",
	})
	if err != nil {
		t.Fatalf("handleEdit: %v", err)
	}
	_ = resultText(t, result)

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go: %v", ferr)
	}
	src := string(final)
	if !strings.Contains(src, "llm-updated") {
		t.Errorf("expected (*LLM).Reconsider to be updated, got:\n%s", src)
	}
	if !strings.Contains(src, "agent-original") {
		t.Errorf("expected (*Agent).Reconsider to remain untouched, got:\n%s", src)
	}
}

// TestHandleAddImport_LandsOnDisk is the #221 regression: previously
// add-import only updated the DB's per-module imports table and
// reported "added import" success, but mergeDeclsIntoSource's
// AST-merge path never touches import blocks at all -- unless
// something else forced a full regen, the import never actually
// reached disk despite the success message. The fix patches the
// file's real source directly via projection.AddImport.
func TestHandleAddImport_LandsOnDisk(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleAddImport(context.Background(), nil, codeParam{
		File:       "main.go",
		ImportPath: "hash/fnv",
	})
	if err != nil {
		t.Fatalf("handleAddImport: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "added import") {
		t.Errorf("expected 'added import' in response, got: %s", text)
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go: %v", ferr)
	}
	if !strings.Contains(string(final), "\"hash/fnv\"") {
		t.Errorf("expected hash/fnv import to land on disk, got:\n%s", final)
	}
}

// TestHandleAddImport_NoFalsePositiveAlreadyPresent is the other half
// of #221: gemot-2847127 reported add-import falsely reporting
// "already present (no-op)" for an import that was NOT actually in the
// file, because presence was checked against the DB's per-module
// imports table (a union shared by every file in the package, never
// refreshed by an incremental code(op:"sync")) rather than the file
// itself. Seed the DB's imports table with an import for THIS module
// that the target file does not actually have, and confirm add-import
// still adds it instead of reporting a false no-op.
func TestHandleAddImport_NoFalsePositiveAlreadyPresent(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	d, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("GetDefinitionByName Greet: %v", err)
	}
	// Simulate a stale per-module imports row: the DB thinks "math" is
	// already imported somewhere in this module, but main.go on disk
	// does not actually have it.
	if err := db.SetImports(d.ModuleID, []store.Import{{ModuleID: d.ModuleID, ImportedPath: "math"}}); err != nil {
		t.Fatalf("seed stale imports: %v", err)
	}

	result, _, err := s.handleAddImport(context.Background(), nil, codeParam{
		File:       "main.go",
		ImportPath: "math",
	})
	if err != nil {
		t.Fatalf("handleAddImport: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "already present") {
		t.Errorf("false positive: reported already present despite math not being in main.go, got: %s", text)
	}
	if !strings.Contains(text, "added import") {
		t.Errorf("expected 'added import', got: %s", text)
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go: %v", ferr)
	}
	if !strings.Contains(string(final), "\"math\"") {
		t.Errorf("expected math import to land on disk, got:\n%s", final)
	}
}

// TestHandleCreate_ReceiverBlindCollisionAllowsDistinctReceivers is the
// #220 regression: handleCreate's pre-existence check called
// GetDefinitionByName(name, modulePath) without the receiver, so it
// could match an unrelated same-named method via the blast-radius
// tiebreak and falsely reject a legitimate create. Go allows the same
// method name on different receiver types -- creating (*Agent).Reconsider
// must succeed even though (*LLM).Reconsider already exists.
func TestHandleCreate_ReceiverBlindCollisionAllowsDistinctReceivers(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

type LLM struct{}
type Agent struct{}

func (l *LLM) Reconsider() string { return "llm-original" }

func main() {}
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCreate(context.Background(), nil, createParam{
		File: "main.go",
		Body: "func (a *Agent) Reconsider() string { return \"agent-original\" }",
	})
	if err != nil {
		t.Fatalf("handleCreate: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "already exists") {
		t.Fatalf("false collision: (*Agent).Reconsider should not collide with (*LLM).Reconsider, got: %s", text)
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go: %v", ferr)
	}
	src := string(final)
	if !strings.Contains(src, "agent-original") {
		t.Errorf("expected (*Agent).Reconsider to be created, got:\n%s", src)
	}
	if !strings.Contains(src, "llm-original") {
		t.Errorf("expected (*LLM).Reconsider to remain untouched, got:\n%s", src)
	}
}

// TestHandleApply_EditRejectsIdentityChangeInBody is the #222
// regression: an "edit" op whose new_body declares a different
// function name than the def being edited (a rename attempted via
// edit instead of op:"rename") left the DB's Name field stale while
// the body said otherwise. mergeDeclsIntoSource matched the on-disk
// decl by the stale Name and spliced in the differently-named body;
// the resulting file then had no decl under the old name, which
// tripped safeWriteGoFile's on-disk-decl-loss check and silently
// blocked the write for the WHOLE file -- including an unrelated
// "create" op batched in the same apply call. The fix rejects an
// identity-changing edit up front, before any DB write, so the whole
// batch cleanly rolls back with an actionable error instead of landing
// a desynced DB/disk state.
func TestHandleApply_EditRejectsIdentityChangeInBody(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte("package testproj\n\nimport \"testing\"\n\nfunc TestOldName(t *testing.T) {\n\tt.Log(\"old\")\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package testproj\n\nfunc main() {}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{
				Op:      "edit",
				Name:    "TestOldName",
				NewBody: "func TestNewName(t *testing.T) {\n\tt.Log(\"new\")\n}",
			},
			{
				Op:   "create",
				File: "main_test.go",
				Body: "func TestBrandNew(t *testing.T) {\n\tt.Log(\"brand new\")\n}",
			},
		},
	})
	if err != nil {
		t.Fatalf("handleApply: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "use op:\"rename\"") {
		t.Fatalf("expected identity-change rejection pointing at op:\"rename\", got: %s", text)
	}
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the whole batch to roll back, got: %s", text)
	}

	// Neither the identity-changing edit NOR the unrelated batched
	// create should have landed -- a clean all-or-nothing rejection,
	// not a partial DB/disk desync.
	final, ferr := os.ReadFile(filepath.Join(projDir, "main_test.go"))
	if ferr != nil {
		t.Fatalf("read main_test.go: %v", ferr)
	}
	src := string(final)
	if !strings.Contains(src, "TestOldName") {
		t.Errorf("expected TestOldName to remain untouched on disk, got:\n%s", src)
	}
	if strings.Contains(src, "TestNewName") || strings.Contains(src, "TestBrandNew") {
		t.Errorf("expected neither TestNewName nor TestBrandNew to land (whole batch rejected), got:\n%s", src)
	}
	if _, err := db.GetDefinitionByName("TestBrandNew", ""); err == nil {
		t.Errorf("expected TestBrandNew to NOT exist in the DB (transaction rolled back)")
	}
}

// TestHandleEdit_RejectsIdentityChangeInBody is the singleton-path half
// of the #222 fix: op:"edit" must reject a new_body that renames the
// def (same root cause as TestHandleApply_EditRejectsIdentityChangeInBody),
// pointing the caller at op:"rename" instead of silently landing a
// stale Name in the DB.
func TestHandleEdit_RejectsIdentityChangeInBody(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte("package testproj\n\nimport \"testing\"\n\nfunc TestOldName(t *testing.T) {\n\tt.Log(\"old\")\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package testproj\n\nfunc main() {}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleEdit(context.Background(), nil, editParam{
		Name:    "TestOldName",
		NewBody: "func TestNewName(t *testing.T) {\n\tt.Log(\"new\")\n}",
	})
	if err != nil {
		t.Fatalf("handleEdit: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "use code(op:\"rename\")") {
		t.Fatalf("expected identity-change rejection pointing at op:\"rename\", got: %s", text)
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main_test.go"))
	if ferr != nil {
		t.Fatalf("read main_test.go: %v", ferr)
	}
	src := string(final)
	if !strings.Contains(src, "TestOldName") || strings.Contains(src, "TestNewName") {
		t.Errorf("expected TestOldName to remain untouched, got:\n%s", src)
	}
}

// TestHandleApply_ReplaceHunkBatchRollsBackOnLaterFailure probes #229:
// the exact repro shape reported from real usage -- multiple
// replace-hunk ops followed by a later op that fails (a multi-decl
// create body, which handleApply rejects before ever touching the
// DB). If the batch is truly atomic, NEITHER replace-hunk's write may
// be visible after the reported rollback, regardless of position in
// the batch (the field report specifically saw an interior op survive
// while its neighbors correctly rolled back).
func TestHandleApply_ReplaceHunkBatchRollsBackOnLaterFailure(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	greetBefore, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("setup: Greet not found: %v", err)
	}
	farewellBefore, err := db.GetDefinitionByName("Farewell", "")
	if err != nil {
		t.Fatalf("setup: Farewell not found: %v", err)
	}
	greetOriginalBody := greetBefore.Body
	farewellOriginalBody := farewellBefore.Body

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "replace-hunk", Name: "Greet", Old: `return "Hello, " + name`, New: `return "HELLO, " + name`},
			{Op: "replace-hunk", Name: "Farewell", Old: `Greet(name) + " and goodbye"`, New: `Greet(name) + " and farewell"`},
			{Op: "create", Body: "func A() {}\nfunc B() {}"},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the batch to report rollback, got: %s", text)
	}

	greetAfter, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("Greet vanished after failed batch: %v", err)
	}
	if greetAfter.Body != greetOriginalBody {
		t.Errorf("#229: Greet's DB body changed to %q despite the batch failing overall (was %q)", greetAfter.Body, greetOriginalBody)
	}

	farewellAfter, err := db.GetDefinitionByName("Farewell", "")
	if err != nil {
		t.Fatalf("Farewell vanished after failed batch: %v", err)
	}
	if farewellAfter.Body != farewellOriginalBody {
		t.Errorf("#229: Farewell's DB body changed to %q despite the batch failing overall (was %q)", farewellAfter.Body, farewellOriginalBody)
	}
}

// TestHandleApply_SameDefMultiHunkChainRollsBackOnLaterFailure probes
// #229's likely actual shape more closely than the independent-def
// variant above: the field report described 4 replace-hunk ops
// inserting distinct case clauses into the SAME switch (i.e. the SAME
// def's body, edited incrementally, each hunk depending on the
// previous op's write within the batch's tx) followed by a failing
// create. If tx-scoped read-your-own-writes visibility isn't fully
// undone by rollback for some reason, an interior hunk in this chain
// is the shape most likely to expose it.
func TestHandleApply_SameDefMultiHunkChainRollsBackOnLaterFailure(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("setup: Greet not found: %v", err)
	}
	originalBody := before.Body

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "replace-hunk", Name: "Greet", Old: `return "Hello, " + name`, New: "step1 := \"Hello, \" + name\n\treturn step1"},
			{Op: "replace-hunk", Name: "Greet", Old: `step1 := "Hello, " + name`, New: `step2 := "Hello, " + name`},
			{Op: "replace-hunk", Name: "Greet", Old: `step2 := "Hello, " + name`, New: `step3 := "Hello, " + name`},
			{Op: "replace-hunk", Name: "Greet", Old: `step3 := "Hello, " + name`, New: `step4 := "Hello, " + name`},
			{Op: "create", Body: "func A() {}\nfunc B() {}"},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the batch to report rollback, got: %s", text)
	}

	after, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("Greet vanished after failed batch: %v", err)
	}
	if after.Body != originalBody {
		t.Errorf("#229: Greet's DB body changed to %q despite the batch failing overall (was %q)", after.Body, originalBody)
	}
}

// TestHandleCreateFallsBackToModuleWhenFileUnresolved fixes #225:
// handleCreate used to return "file %q does not map to any known
// module" immediately whenever file: didn't resolve to a module,
// even when the caller explicitly passed module: as a fallback —
// making it impossible to scaffold the first file in a brand-new
// package directory via the normal MCP path. file: naming a
// not-yet-existing directory (no prior .go files there, so
// findModuleByFile has nothing to match) combined with an explicit
// module: naming an already-known module must now succeed, with the
// new def's SourceFile set to the requested (new) path.
func TestHandleCreateFallsBackToModuleWhenFileUnresolved(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	// #13: testproj's only existing module is "package main" -- before
	// #13's fix this would have reused that Name for the new directory
	// too, emitting package main with no func main() there, which can
	// never build. The fix ensures a directory-scoped module ("newpkg")
	// instead of borrowing testproj's identity, so this now builds.
	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body:   "func NewPkgFunc() int { return 1 }",
		File:   "internal/newpkg/x.go",
		Module: "testproj",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created") {
		t.Fatalf("expected 'Created', got: %s", text)
	}

	d, err := db.GetDefinitionByName("NewPkgFunc", "")
	if err != nil {
		t.Fatalf("def not found: %v", err)
	}
	if d.SourceFile != "internal/newpkg/x.go" {
		t.Errorf("SourceFile = %q, want %q", d.SourceFile, "internal/newpkg/x.go")
	}

	src, err := os.ReadFile(filepath.Join(projDir, "internal/newpkg/x.go"))
	if err != nil {
		t.Fatalf("read emitted file: %v", err)
	}
	if !strings.Contains(string(src), "package newpkg") {
		t.Errorf("#13: expected the new directory's own package name (newpkg), not testproj's \"main\" -- got:\n%s", src)
	}
}

// TestHandleCreateStillErrorsWhenNeitherFileNorModuleResolve guards the
// #225 fix's error path: if file: doesn't resolve AND the caller's
// module: also doesn't exist, this must still fail loudly rather than
// silently falling through to "first module found".
func TestHandleCreateStillErrorsWhenNeitherFileNorModuleResolve(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body:   "func NewPkgFunc() int { return 1 }",
		File:   "internal/newpkg/x.go",
		Module: "does-not-exist",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "not found") {
		t.Fatalf("expected a module-not-found error, got: %s", text)
	}
	if _, err := db.GetDefinitionByName("NewPkgFunc", ""); err == nil {
		t.Fatal("NewPkgFunc should not have been created")
	}
}

// TestHandleApply_AddImportAlreadyPresentIsNoOp covers the idempotent
// path through the #233 fix: batching the same import twice (or
// against one already on disk) must not error or duplicate the import.
func TestHandleApply_AddImportAlreadyPresentIsNoOp(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	for i := 0; i < 2; i++ {
		result, _, err := s.handleApply(context.Background(), nil, applyParam{
			Operations: []applyOp{
				{Op: "add-import", File: "main.go", ImportPath: "hash/fnv"},
			},
		})
		if err != nil {
			t.Fatalf("handleApply iteration %d: %v", i, err)
		}
		text := resultText(t, result)
		if strings.Contains(text, "rolled back") {
			t.Fatalf("iteration %d: unexpected failure: %s", i, text)
		}
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go: %v", ferr)
	}
	if strings.Count(string(final), "\"hash/fnv\"") != 1 {
		t.Errorf("expected hash/fnv imported exactly once, got:\n%s", final)
	}
}

// TestHandleApply_AddImportOnRootFileLandsOnDisk is the #233
// regression: handleApply's batch "add-import" case computed
// dir := file (not "") for a root-level file with no "/", so
// FindDefinitionsByFile never matched any module and the whole batch
// errored with "add-import: no defs in \"main.go\"" -- mirrors #221's
// already-fixed bug in the singleton handleAddImport path. Also
// verifies the deeper #221 mechanism applies here too: the DB's
// per-module imports table alone never reaches disk (mergeDeclsIntoSource
// doesn't touch import blocks), so the fix must patch the file directly
// via patchImportOnDisk after commit, not just fix the dir bug.
func TestHandleApply_AddImportOnRootFileLandsOnDisk(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "add-import", File: "main.go", ImportPath: "hash/fnv"},
		},
	})
	if err != nil {
		t.Fatalf("handleApply: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "no defs in") {
		t.Fatalf("add-import failed on root-level file: %s", text)
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go: %v", ferr)
	}
	if !strings.Contains(string(final), "\"hash/fnv\"") {
		t.Errorf("expected hash/fnv import to land on disk via batch apply, got:\n%s", final)
	}
}

// TestSuggestMissingImportFixes_CrossPackageHint verifies the
// diagnostic-to-code-action lift: an "undefined: X" build failure where X
// is defined in a different package gets a ready-to-use add-import hint,
// the way an LSP quick-fix would offer inline.
func TestSuggestMissingImportFixes_CrossPackageHint(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	mod, err := db.(*store.SQLiteDB).EnsureModule("testproj/otherpkg", "otherpkg", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Helper", Kind: "function", Exported: true,
		Signature: "func Helper() string", Body: "func Helper() string {\n\treturn \"x\"\n}",
		SourceFile: "otherpkg/helper.go",
	}); err != nil {
		t.Fatal(err)
	}

	hint := s.suggestMissingImportFixes("# testproj\n./main.go:12:2: undefined: Helper\n")
	if !strings.Contains(hint, "add-import") || !strings.Contains(hint, "testproj/otherpkg") {
		t.Errorf("expected add-import hint mentioning testproj/otherpkg, got: %q", hint)
	}
	if !strings.Contains(hint, `file:"main.go"`) {
		t.Errorf("expected hint's file param normalized (no ./ prefix), got: %q", hint)
	}
}

// TestSuggestMissingImportFixes_SamePackageNoHint verifies no hint fires
// when the undefined name resolves to the SAME package as the failing
// file -- that's a typo or a real bug, not a missing import.
func TestSuggestMissingImportFixes_SamePackageNoHint(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	hint := s.suggestMissingImportFixes("./main.go:12:2: undefined: Greet\n")
	if hint != "" {
		t.Errorf("expected no hint for same-package undefined, got: %q", hint)
	}
}

// TestHandleOverview_CollapsesGeneratedFile verifies a file carrying
// Go's standard "// Code generated ... DO NOT EDIT." marker gets
// collapsed to a one-line summary in overview instead of listing every
// individual def -- autogenerated getters/setters otherwise clutter an
// orientation call that's rarely what someone wants when asking for one.
func TestHandleOverview_CollapsesGeneratedFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	generatedBody := "// Code generated by protoc-gen-go. DO NOT EDIT.\npackage main\n\nfunc GeneratedGetFoo() string { return \"\" }\nfunc GeneratedGetBar() string { return \"\" }\n"
	if err := os.WriteFile(filepath.Join(projDir, "generated.pb.go"), []byte(generatedBody), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.IngestFile(db, projDir, filepath.Join(projDir, "generated.pb.go")); err != nil {
		t.Fatal("ingest generated.pb.go:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleOverview(context.Background(), nil, codeParam{File: "testproj"})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	text := resultText(t, result)

	if !strings.Contains(text, "generated, 2 definitions") {
		t.Errorf("expected generated.pb.go collapsed to a one-line summary, got:\n%s", text)
	}
	if strings.Contains(text, "GeneratedGetFoo") || strings.Contains(text, "GeneratedGetBar") {
		t.Errorf("expected generated defs NOT individually listed, got:\n%s", text)
	}
	// Hand-written main.go must still list individually, unaffected.
	if !strings.Contains(text, "### main.go") {
		t.Errorf("expected main.go still individually listed, got:\n%s", text)
	}
}

// TestHandleFileDefs_CapsLargeFile verifies file-defs truncates a file
// with more defs than fileDefsCap instead of dumping all of them.
func TestHandleFileDefs_CapsLargeFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	var sb strings.Builder
	sb.WriteString("package main\n\n")
	const n = 60
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "func Big%d() {}\n", i)
	}
	bigPath := filepath.Join(projDir, "big.go")
	if err := os.WriteFile(bigPath, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.IngestFile(db, projDir, bigPath); err != nil {
		t.Fatal("ingest big.go:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, err := s.handleFileDefs(context.Background(), nil, codeParam{File: "big.go"})
	if err != nil {
		t.Fatalf("file-defs: %v", err)
	}
	text := resultText(t, result)

	if !strings.Contains(text, fmt.Sprintf("%d of %d definitions", fileDefsCap, n)) {
		t.Errorf("expected cap message '%d of %d definitions', got:\n%s", fileDefsCap, n, text)
	}
	if strings.Count(text, "\"name\":") > fileDefsCap {
		t.Errorf("expected at most %d entries in output, got more:\n%s", fileDefsCap, text)
	}
}

// TestHandlePragmas_CapsManyMatches verifies pragmas truncates a match
// set larger than pragmasCap instead of dumping all of them.
func TestHandlePragmas_CapsManyMatches(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	var sb strings.Builder
	sb.WriteString("package main\n\n")
	const n = 35
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "//test:pragma\nfunc PragmaTarget%d() {}\n\n", i)
	}
	pragmaPath := filepath.Join(projDir, "pragmas.go")
	if err := os.WriteFile(pragmaPath, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, err := s.handlePragmas(context.Background(), nil, codeParam{Pattern: "test:pragma"})
	if err != nil {
		t.Fatalf("pragmas: %v", err)
	}
	text := resultText(t, result)

	if !strings.Contains(text, fmt.Sprintf("showing %d of %d", pragmasCap, n)) {
		t.Errorf("expected cap message 'showing %d of %d', got:\n%s", pragmasCap, n, text)
	}
	if got := strings.Count(text, "`test:pragma`"); got > pragmasCap {
		t.Errorf("expected at most %d pragma lines, got %d:\n%s", pragmasCap, got, text)
	}
}

// TestHandleTraverse_CapsManyCallers verifies traverse's default
// (markdown) rendering truncates when a def has more callers than
// traverseResultCap, while format:"json" remains the uncapped escape
// hatch (mirroring handleImpact's convention).
func TestHandleTraverse_CapsManyCallers(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	var sb strings.Builder
	sb.WriteString("package main\n\nfunc Target() {}\n\n")
	const n = 55
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "func Wrap%d() { Target() }\n", i)
	}
	wrapPath := filepath.Join(projDir, "wrappers.go")
	if err := os.WriteFile(wrapPath, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, err := s.handleTraverse(context.Background(), nil, codeParam{Name: "Target", Direction: "callers", Depth: 1})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	text := resultText(t, result)

	if !strings.Contains(text, "more results omitted") {
		t.Errorf("expected a cap/omission note for %d callers (cap %d), got:\n%s", n, traverseResultCap, text)
	}
	if got := strings.Count(text, "Wrap"); got > traverseResultCap {
		t.Errorf("expected at most %d rendered callers, got %d:\n%s", traverseResultCap, got, text)
	}

	// format:"json" remains uncapped -- the full-data escape hatch.
	jsonResult, _, err := s.handleTraverse(context.Background(), nil, codeParam{Name: "Target", Direction: "callers", Depth: 1, Format: "json"})
	if err != nil {
		t.Fatalf("traverse json: %v", err)
	}
	jsonText := resultText(t, jsonResult)
	if got := strings.Count(jsonText, "\"name\":\"Wrap"); got != n {
		t.Errorf("expected format:json to return all %d callers uncapped, got %d in:\n%s", n, got, jsonText)
	}
}

// TestIsStaleProjectDirError_RealGoListFailure reproduces the real
// failure (a directory with no go.mod, matching what happens when
// s.projectDir was captured at startup and the project has since been
// moved/renamed) rather than asserting against a hand-typed string.
func TestIsStaleProjectDirError_RealGoListFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	// goload.LoadAll itself returns a nil error here -- go/packages
	// reports "no module" per-package (pkg.Errors), not as a top-level
	// Load error. The real failure winze hit surfaces one layer up, from
	// ingest.IngestPackages walking those per-package errors.
	pkgs, err := goload.LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: unexpected top-level error: %v", err)
	}

	db, _ := setupTestDB(t)
	err = ingest.IngestPackages(db, pkgs, dir)
	if err == nil {
		t.Fatal("expected IngestPackages to fail on a directory with no go.mod")
	}
	if !isStaleProjectDirError(err) {
		t.Errorf("expected isStaleProjectDirError(true) for: %v", err)
	}

	if isStaleProjectDirError(nil) {
		t.Error("expected isStaleProjectDirError(nil) to be false")
	}
	if isStaleProjectDirError(fmt.Errorf("some unrelated error")) {
		t.Error("expected isStaleProjectDirError to be false for an unrelated error")
	}
}

// TestProjectOverview_ExcludesFieldsFromCountAndExemplars is the
// regression test for #11's blast-radius fix: struct fields are real
// definitions now, but projectOverview must keep excluding them from
// its def count and exemplar preview so the project-wide orientation
// view isn't crowded with field names instead of real API surface.
func TestProjectOverview_ExcludesFieldsFromCountAndExemplars(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "fieldproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

type Config struct {
	AlphaField string
	BetaField  int
}

func main() {}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}

	s := &server{backend: db}
	result, _, _ := s.projectOverview(context.Background())
	text := resultText(t, result)

	if strings.Contains(text, "AlphaField") || strings.Contains(text, "BetaField") {
		t.Errorf("expected field names excluded from project overview exemplars, got %q", text)
	}
	if !strings.Contains(text, "1 exported") {
		t.Errorf("expected exactly 1 exported def (Config; fields excluded), got %q", text)
	}
}

// TestHandleApply_BuildFailureRollsBackBothDBAndFile is the #12
// regression test: a batch whose build genuinely fails must leave
// NEITHER the DB nor the on-disk file changed, closing the gap where
// a build failure previously left the DB committed (phantom/stale
// defs discoverable via a later outline/read) even though the file
// itself was correctly left alone or reverted.
func TestHandleApply_BuildFailureRollsBackBothDBAndFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("setup: Greet not found: %v", err)
	}
	originalBody := before.Body

	mainPath := filepath.Join(projDir, "main.go")
	originalFile, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "edit", Name: "Greet", NewBody: "func Greet(name string) string { return undefinedHelperFunc(name) }"},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("expected a build failure (undefinedHelperFunc doesn't exist), got: %s", text)
	}

	after, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("Greet vanished after failed build: %v", err)
	}
	if after.Body != originalBody {
		t.Errorf("#12: Greet's DB body changed to %q despite the build failing -- commitOrRollbackOnBuild did not roll back the DB", after.Body)
	}

	finalFile, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go after failed build: %v", err)
	}
	if string(finalFile) != string(originalFile) {
		t.Errorf("#12: main.go changed on disk despite the build failing -- expected the pre-batch content to be restored\nbefore:\n%s\nafter:\n%s", originalFile, finalFile)
	}
}

// TestHandleCreate_BuildFailureRollsBackBothDBAndFile was the #12
// regression test for handleCreate's original bug: it wrote straight to
// s.backend with no transaction at all, so a build failure left a
// phantom def in the DB with no corresponding on-disk content -- DB and
// file diverged from each other. That divergence guarantee still holds
// (this test now proves it), but the specific case it originally used to
// force a failure (an undefined-reference call) no longer fails at all:
// create's default now skips the real go build (deliberate tradeoff,
// 2026-08-20 -- see handleCreate's own comment), so DB and file land
// together, in sync, with the new (semantically broken until the next
// test/apply) function. What still MUST hold is that they never diverge
// from each other, which this proves by checking both got the new def.
func TestHandleCreate_BuildFailureRollsBackBothDBAndFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func BrokenNewFunc() string { return undefinedHelperFunc() }",
		File: "main.go",
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("create's default now defers the real build -- an undefined-reference body should land, not roll back, got: %s", text)
	}

	if _, err := db.GetDefinitionByName("BrokenNewFunc", ""); err != nil {
		t.Error("BrokenNewFunc should exist in the DB -- create's build gate is deferred by default, not skipped entirely")
	}

	mainPath := filepath.Join(projDir, "main.go")
	finalFile, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go after create: %v", err)
	}
	if !strings.Contains(string(finalFile), "BrokenNewFunc") {
		t.Errorf("DB and file must land together -- BrokenNewFunc is in the DB but missing from disk:\n%s", finalFile)
	}
}

// TestHandleDelete_BuildFailureRollsBackBothDBAndFile is the #12
// regression test for handleDelete's non-force path: deleting a def
// that's still referenced elsewhere in a non-test caller breaks the
// build, and (without force:true) that must leave neither the DB nor
// the file changed, instead of committing the delete unconditionally.
func TestHandleDelete_BuildFailureRollsBackBothDBAndFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Greet is called by Farewell in setupTestDB's fixture, so deleting
	// it (bypassing the safe-delete refusal via force, to reach the
	// build gate rather than the earlier caller-check) breaks the build.
	mainPath := filepath.Join(projDir, "main.go")
	originalFile, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	result, _, _ := s.handleDelete(context.Background(), nil, nameParam{Name: "Greet", Force: true})
	text := resultText(t, result)
	if !strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("expected a build failure (Farewell still calls Greet), got: %s", text)
	}

	// force:true intentionally commits regardless (see handleDelete's
	// #12 comment) -- Greet really is gone here. This test exists to
	// document that force still bypasses the gate, not to assert a
	// rollback for the forced path. The real #12 coverage for the
	// non-force path is TestHandleDelete_SafeRefusesWhenReferenced,
	// which never reaches the build gate at all (refused earlier).
	if _, err := db.GetDefinitionByName("Greet", ""); err == nil {
		t.Error("expected Greet to be gone: force:true commits unconditionally by design")
	}
	_ = originalFile
}

// TestHandleEdit_SignatureChangedBuildFailureRollsBackBothDBAndFile is
// the #12 regression test for handleEdit's signature-changed path
// (commitOrRollbackOnBuild): previously this wrote straight to
// s.backend with no transaction, so a build failure left the DB
// committed with no corresponding on-disk content. The signature-
// stable path (commitOrRollbackOnEmit) shares the same underlying
// commitOrRollbackOn mechanism already covered by the apply/create
// regression tests, so it isn't re-tested per handler here -- its
// rollback trigger is an emit-level WARNING, not a build failure,
// since #148 deliberately never runs go build on that path.
func TestHandleEdit_SignatureChangedBuildFailureRollsBackBothDBAndFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("setup: Greet not found: %v", err)
	}
	originalBody := before.Body
	originalSignature := before.Signature

	mainPath := filepath.Join(projDir, "main.go")
	originalFile, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	// Changing the signature (extra int param) forces sigStable=false,
	// so this reaches commitOrRollbackOnBuild, not the emit-only path.
	// The undefined helper also makes the build genuinely fail.
	result, _, _ := s.handleEdit(context.Background(), nil, editParam{
		Name:    "Greet",
		NewBody: "func Greet(name string, extra int) string { return undefinedHelperFunc(name, extra) }",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("expected a build failure (undefinedHelperFunc doesn't exist), got: %s", text)
	}

	after, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("Greet vanished after failed build: %v", err)
	}
	if after.Body != originalBody {
		t.Errorf("#12: Greet's DB body changed despite the build failing -- got %q, want %q", after.Body, originalBody)
	}
	if after.Signature != originalSignature {
		t.Errorf("#12: Greet's DB signature changed despite the build failing -- got %q, want %q", after.Signature, originalSignature)
	}

	finalFile, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go after failed build: %v", err)
	}
	if string(finalFile) != string(originalFile) {
		t.Errorf("#12: main.go changed on disk despite the build failing\nbefore:\n%s\nafter:\n%s", originalFile, finalFile)
	}
}

// TestHandleEdit_ModuleDisambiguatesSameNamedType is the gemot dispatch
// regression: op:edit name:"Engine" module:"testproj/bft" corrupted an
// unrelated "Engine" struct in a different package (testproj/chess)
// instead of updating the intended one, and reported success. Same
// #219 gap as receiver, just for module:/file: disambiguating a
// same-named non-method type across packages instead of a same-named
// method across receivers.
func TestHandleEdit_ModuleDisambiguatesSameNamedType(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "bft"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "chess"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	// chess's Engine deliberately has more references than bft's --
	// GetDefinitionByName's module-blind fallback is a blast-radius
	// (most-callers) tiebreak, so a fixture with equal (zero) refs on
	// both wouldn't actually stress it. This shape is what let gemot's
	// real report happen: the wrong, more-referenced Engine won.
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte(`package bft

type Engine struct{ Replica string }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleEdit(context.Background(), nil, editParam{
		Name:    "Engine",
		Module:  "testproj/bft",
		NewBody: "type Engine struct {\n\tReplica string\n\tTransport string\n}",
	})
	if err != nil {
		t.Fatalf("handleEdit: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("expected a clean edit, got: %s", text)
	}

	bftSrc, err := os.ReadFile(filepath.Join(projDir, "bft", "engine.go"))
	if err != nil {
		t.Fatalf("read bft/engine.go: %v", err)
	}
	if !strings.Contains(string(bftSrc), "Transport string") {
		t.Errorf("expected bft's Engine to gain the Transport field, got:\n%s", bftSrc)
	}

	chessSrc, err := os.ReadFile(filepath.Join(projDir, "chess", "engine.go"))
	if err != nil {
		t.Fatalf("read chess/engine.go: %v", err)
	}
	if !strings.Contains(string(chessSrc), "Protocol string") || strings.Contains(string(chessSrc), "Transport") {
		t.Errorf("gemot dispatch: chess's unrelated Engine was corrupted by an edit scoped to module:\"testproj/bft\", got:\n%s", chessSrc)
	}
}

// TestHandleGetDefinition_ModuleDisambiguatesSameNamedType is issue
// #15: the read path (handleGetDefinition, reached via op:"read")
// called GetDefinitionByName(name, "") directly, ignoring nameParam's
// Module/File even when set -- unlike handleEdit, which #219/gemot
// already fixed via resolveEditTarget. Same bft/chess Engine fixture:
// chess's Engine has more references, so the pre-fix blast-radius
// tiebreak would silently return chess's body for a read scoped to
// module:"testproj/bft".
func TestHandleGetDefinition_ModuleDisambiguatesSameNamedType(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "bft"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "chess"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte(`package bft

type Engine struct{ Replica string }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	// Prove the fixture actually stresses the bug: a module-blind lookup
	// (what handleGetDefinition called before the #15 fix) must resolve
	// to chess's more-referenced Engine, not bft's -- otherwise the
	// assertions below would pass for the wrong reason.
	if blind, err := db.GetDefinitionByName("Engine", ""); err != nil || blind == nil || !strings.Contains(blind.Body, "Protocol") {
		t.Fatalf("fixture sanity check: module-blind GetDefinitionByName(\"Engine\", \"\") should resolve to chess's Engine (Protocol field) via the blast-radius tiebreak, got: %+v, err=%v", blind, err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{
		Name:   "Engine",
		Module: "testproj/bft",
		Full:   true,
	})
	if err != nil {
		t.Fatalf("handleGetDefinition: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Replica string") {
		t.Errorf("expected bft's Engine (Replica field), got:\n%s", text)
	}
	if strings.Contains(text, "Protocol string") {
		t.Errorf("read scoped to module:\"testproj/bft\" returned chess's Engine instead:\n%s", text)
	}
}

// TestHandleExpand_ModuleDisambiguatesSameNamedType is issue #15's
// expand-path counterpart: handleExpand receives the full codeParam
// (Module/File already in scope) but called GetDefinitionByName(name,
// "") directly, discarding them.
func TestHandleExpand_ModuleDisambiguatesSameNamedType(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "bft"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "chess"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte(`package bft

type Engine struct{ Replica string }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{
		Name:    "Engine",
		Module:  "testproj/bft",
		Include: []string{"body"},
	})
	if err != nil {
		t.Fatalf("handleExpand: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Replica string") {
		t.Errorf("expected bft's Engine (Replica field), got:\n%s", text)
	}
	if strings.Contains(text, "Protocol string") {
		t.Errorf("expand scoped to module:\"testproj/bft\" returned chess's Engine instead:\n%s", text)
	}
}

// TestHandleFragmentEdit_ModuleDisambiguatesSameNamedType is
// TestHandleEdit_ModuleDisambiguatesSameNamedType's fragment-edit
// counterpart: handleFragmentEdit is a separate handler (the
// old_fragment/new_fragment shape never funnels through handleEdit)
// and had the identical gap -- module:/file: (and even receiver:)
// were available on its own codeParam but never consulted.
func TestHandleFragmentEdit_ModuleDisambiguatesSameNamedType(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "bft"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "chess"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte(`package bft

type Engine struct{ Replica string }
`), 0644)
	// chess's Engine has more references than bft's for the same
	// blast-radius-tiebreak reason as the handleEdit test.
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:          "edit",
		Name:        "Engine",
		Module:      "testproj/bft",
		OldFragment: "Replica string",
		NewFragment: "Replica string\n\tTransport string",
	})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("expected a clean edit, got: %s", text)
	}

	bftSrc, err := os.ReadFile(filepath.Join(projDir, "bft", "engine.go"))
	if err != nil {
		t.Fatalf("read bft/engine.go: %v", err)
	}
	if !strings.Contains(string(bftSrc), "Transport string") {
		t.Errorf("expected bft's Engine to gain the Transport field, got:\n%s", bftSrc)
	}

	chessSrc, err := os.ReadFile(filepath.Join(projDir, "chess", "engine.go"))
	if err != nil {
		t.Fatalf("read chess/engine.go: %v", err)
	}
	if !strings.Contains(string(chessSrc), "Protocol string") || strings.Contains(string(chessSrc), "Transport") {
		t.Errorf("chess's unrelated Engine was corrupted by a fragment edit scoped to module:\"testproj/bft\", got:\n%s", chessSrc)
	}
}

// TestHandleFragmentEdit_BuildFailureRollsBackBothDBAndFile is the #12
// regression test for handleFragmentEdit: this handler was missed
// entirely when #12 fixed handleEdit/handleCreate/handleDelete/apply,
// since fragment edits (old_fragment/new_fragment) are a separate
// handler that never funnels through handleEdit. It wrote straight to
// s.backend with no transaction, so a build failure left a phantom
// edit committed with no corresponding on-disk change.
func TestHandleFragmentEdit_BuildFailureRollsBackBothDBAndFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("setup: Greet not found: %v", err)
	}
	originalBody := before.Body

	mainPath := filepath.Join(projDir, "main.go")
	originalFile, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:          "edit",
		Name:        "Greet",
		OldFragment: `return "Hello, " + name`,
		NewFragment: "return undefinedHelperFunc(name)",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("expected a build failure (undefinedHelperFunc doesn't exist), got: %s", text)
	}

	after, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("Greet vanished after failed build: %v", err)
	}
	if after.Body != originalBody {
		t.Errorf("#12: Greet's DB body changed to %q despite the build failing -- got %q", after.Body, originalBody)
	}

	finalFile, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go after failed build: %v", err)
	}
	if string(finalFile) != string(originalFile) {
		t.Errorf("#12: main.go changed on disk despite the build failing\nbefore:\n%s\nafter:\n%s", originalFile, finalFile)
	}
}

// TestHandleReplaceHunk_ModuleDisambiguatesSameNamedType covers the
// same gap TestHandleEdit_ModuleDisambiguatesSameNamedType fixed, for
// the standalone projection-op handlers (replace-hunk, replace-slice,
// insert-precondition, wrap-in-defer, rename-param). All five called
// GetDefinitionByName(name, "") directly -- module:/file:/receiver:
// were already present on their shared codeParam but never consulted.
// Found while fixing #edit's gemot-reported case; all five now share
// resolveEditTarget, so this one stands in for the group rather than
// duplicating the same fixture five times.
func TestHandleReplaceHunk_ModuleDisambiguatesSameNamedType(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "bft"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "chess"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte(`package bft

func Run() string {
	return "bft-original"
}
`), 0644)
	// chess's Run has more references than bft's, same blast-radius
	// rationale as the edit/fragment-edit tests.
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

func Run() string {
	return "chess-original"
}

func UseA() string { return Run() }
func UseB() string { return Run() }
func UseC() string { return Run() }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:     "replace-hunk",
		Name:   "Run",
		Module: "testproj/bft",
		Old:    `"bft-original"`,
		New:    `"bft-updated"`,
	})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	_ = resultText(t, result)

	bftSrc, err := os.ReadFile(filepath.Join(projDir, "bft", "engine.go"))
	if err != nil {
		t.Fatalf("read bft/engine.go: %v", err)
	}
	if !strings.Contains(string(bftSrc), "bft-updated") {
		t.Errorf("expected bft's Run to be updated, got:\n%s", bftSrc)
	}

	chessSrc, err := os.ReadFile(filepath.Join(projDir, "chess", "engine.go"))
	if err != nil {
		t.Fatalf("read chess/engine.go: %v", err)
	}
	if !strings.Contains(string(chessSrc), "chess-original") {
		t.Errorf("chess's unrelated Run was corrupted by a replace-hunk scoped to module:\"testproj/bft\", got:\n%s", chessSrc)
	}
}

// TestHandleCode_ReadScopedByModuleBypassesBodyServedShortcut is a
// follow-up finding from an independent review of #15: the respCache
// "already read this session" shortcut (bodyServedEpochsAgo/
// markBodyServed) was keyed by bare name only, with no module/file
// component. Without a bypass, an earlier unscoped read(full:true)
// resolving to chess's Engine would mark bare "Engine" as served, and
// a LATER correctly-disambiguated read(module:"testproj/bft") would
// get short-circuited to the stale "already read" stub instead of
// ever reaching resolveEditTarget -- reopening the exact silent
// wrong-resolution bug #15 closed, one layer up in handleCode's
// dispatch rather than in the handler itself. Goes through
// s.handleCode (not the handler directly), since that's where the
// shortcut lives.
func TestHandleCode_ReadScopedByModuleBypassesBodyServedShortcut(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "bft"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "chess"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte(`package bft

type Engine struct{ Replica string }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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

	// Unscoped full read: resolves to chess's Engine (blast-radius
	// tiebreak) and, pre-fix, would mark bare "Engine" as body-served.
	first, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "Engine", Full: true})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	firstText := resultText(t, first)
	if !strings.Contains(firstText, "Protocol string") {
		t.Fatalf("expected the unscoped read to resolve to chess's Engine, got:\n%s", firstText)
	}

	// Same-session, correctly-disambiguated follow-up: NOT full:true,
	// since the shortcut only fires for plain (non-full) reads -- must
	// resolve to bft's Engine via resolveEditTarget, not get
	// short-circuited to the "already read" stub. Mode:"body" forces
	// body content into the response regardless of summary-mode
	// defaults, so the field-name assertions below are meaningful.
	second, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "Engine", Module: "testproj/bft", Mode: "body"})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	secondText := resultText(t, second)
	if strings.Contains(secondText, "already read in this session") {
		t.Fatalf("module-scoped read was short-circuited by the bare-name bodyServed shortcut instead of resolving via resolveEditTarget:\n%s", secondText)
	}
	if !strings.Contains(secondText, "Replica string") {
		t.Errorf("expected bft's Engine (Replica field), got:\n%s", secondText)
	}
	if strings.Contains(secondText, "Protocol string") {
		t.Errorf("module-scoped read returned chess's Engine instead of bft's:\n%s", secondText)
	}
}

func TestHandleCreateFailedBuildDoesNotOrphanModule(t *testing.T) {
	// Regression test for task #239: handleCreate's "new directory"
	// branch used to call EnsureModule on s.backend directly, outside
	// the transaction guarding the definition write. A build failure
	// rolled back the definition but left the module row committed -- a
	// permanent zero-def module pointing at a real directory.
	// emitModule's zero-defs cleanup then guessed a filename from that
	// orphan and deleted whatever real file already lived there on the
	// next unscoped emit. This is exactly how a real bench run lost
	// grpc-go's resolver/passthrough/passthrough.go and four other
	// untouched files.
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	// create's default now defers the real build (see handleCreate) --
	// this test needs a genuine build failure to exercise the #239
	// module-orphan-on-rollback path, so force the real build via the
	// same escape hatch commitOrRollbackOnEmit already respects.
	t.Setenv("DEFN_STRICT_BUILD", "1")

	// A real, pre-existing file defn never ingested or wrote -- stands
	// in for e.g. resolver/passthrough/passthrough.go.
	preexistDir := filepath.Join(projDir, "internal", "willfail")
	if err := os.MkdirAll(preexistDir, 0755); err != nil {
		t.Fatal(err)
	}
	preexistContent := []byte("package willfail\n\n// PreExisting was never touched by defn.\nfunc PreExisting() int { return 1 }\n")
	preexistPath := filepath.Join(preexistDir, "willfail.go")
	if err := os.WriteFile(preexistPath, preexistContent, 0644); err != nil {
		t.Fatal(err)
	}

	// Body is syntactically valid (passes inference) but references an
	// undefined symbol, so the build fails and the tx rolls back.
	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body:   "func WillFail() int { return undefinedSymbolXYZ }",
		File:   "internal/willfail/x.go",
		Module: "testproj",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "undefinedSymbolXYZ") && !strings.Contains(strings.ToLower(text), "build") {
		t.Fatalf("expected the undefined-symbol build failure to be reported, got: %s", text)
	}

	if _, err := db.GetDefinitionByName("WillFail", ""); err == nil {
		t.Fatal("WillFail should not have persisted -- the build failed and the tx should have rolled back")
	}

	mods, err := db.ListModules()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mods {
		if strings.Contains(m.Path, "willfail") {
			t.Fatalf("orphaned zero-def module survived the rollback: %+v", m)
		}
	}

	// The real pre-existing file must be untouched by the failed create.
	got, err := os.ReadFile(preexistPath)
	if err != nil {
		t.Fatalf("pre-existing file vanished: %v", err)
	}
	if string(got) != string(preexistContent) {
		t.Fatalf("pre-existing file was modified:\n%s", got)
	}
}

func TestHandleCode_DeleteReceiverDisambiguatesThroughDispatch(t *testing.T) {
	// Regression test for a real, severe bug found in a head-to-head-go
	// trajectory: code(op:"delete", name:"Equal", receiver:"matcher")
	// silently deleted an unrelated, well-referenced (*lbConfig).Equal
	// instead, because receiver was dropped somewhere between the raw
	// JSON args and the actual delete. Two separate gaps combined to
	// cause this:
	//  1. nameParam had no Receiver field at all, so handleDelete always
	//     called resolveEditTarget with receiver="", which falls back to
	//     GetDefinitionByName's blast-radius tiebreak -- it picks
	//     whichever same-named def has the most references, not the one
	//     actually requested.
	//  2. Even after adding Receiver to nameParam and wiring handleDelete
	//     to read it, handleCode's dispatch switch constructed
	//     nameParam{Name: args.Name, Force: args.Force, Module: ...,
	//     File: ...} WITHOUT copying args.Receiver -- so a handler-level
	//     test calling s.handleDelete directly with a hand-built
	//     nameParam{Receiver: "x"} would have passed while the real
	//     entry point (a raw code(op:"delete", receiver:"x") call, which
	//     always goes through handleCode first) still silently dropped
	//     it. This test goes through handleCode, not handleDelete
	//     directly, specifically to catch that class of gap.
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	mod, err := db.GetModuleByPath("testproj")
	if err != nil {
		t.Fatalf("find testproj module: %v", err)
	}

	// The decoy: well-referenced, must survive.
	decoyID, err := db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Equal", Kind: "method", Receiver: "*Decoy",
		Body: "func (d *Decoy) Equal(o *Decoy) bool { return true }", SourceFile: "main.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		callerID, err := db.UpsertDefinition(&store.Definition{
			ModuleID: mod.ID, Name: fmt.Sprintf("CallsDecoy%d", i), Kind: "function",
			Body: "func CallsDecoy() bool { return true }", SourceFile: "main.go",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetReferences(callerID, []store.Reference{{FromDef: callerID, ToDef: decoyID, Kind: "call"}}); err != nil {
			t.Fatal(err)
		}
	}

	// The real target: zero references, the one actually meant.
	targetID, err := db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Equal", Kind: "method", Receiver: "*Target",
		Body: "func (tg *Target) Equal(o *Target) bool { return true }", SourceFile: "main.go",
	})
	if err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op: "delete", Name: "Equal", Receiver: "*Target", Force: true,
	})
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected delete to succeed, got error: %s", text)
	}

	if _, err := db.GetDefinition(targetID); err == nil {
		t.Fatalf("(*Target).Equal should have been deleted, still present")
	}
	if _, err := db.GetDefinition(decoyID); err != nil {
		t.Fatalf("(*Decoy).Equal should NOT have been touched (0 refs vs its 3), but it's gone: %v", err)
	}
}

func TestHandleApply_RolledBackBatchDoesNotClaimSuccess(t *testing.T) {
	// Same bug as TestHandleCreate_RolledBackCreateDoesNotClaimSuccess,
	// at apply's batch scale: each successful per-op upsert wrote a
	// "+ created"/"~ edited" line to the response DURING the loop, before
	// the batch-wide build gate ran at the tail. If that gate then rolled
	// the whole transaction back, those lines stayed in the response with
	// no indication any of it was undone.
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "create", File: "main.go", Body: "func BrokenNewFunc() string { return undefinedHelperFunc() }"},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "+ created BrokenNewFunc") {
		t.Fatalf("response claims BrokenNewFunc was created despite the rolled-back batch: %s", text)
	}
	if !strings.Contains(text, "rolled back") {
		t.Errorf("expected the response to say the batch was rolled back, got: %s", text)
	}

	if _, err := db.GetDefinitionByName("BrokenNewFunc", ""); err == nil {
		t.Error("BrokenNewFunc exists in the DB despite the build failing -- apply was not rolled back")
	}
}

func TestHandleCreate_RolledBackCreateDoesNotClaimSuccess(t *testing.T) {
	// Regression test for a real bug found by reading a head-to-head-go
	// trajectory: handleCreate always wrote "Created X (id=N) in Y" even
	// when the build failed afterward and the whole transaction --
	// including that insert -- got rolled back. The id was never
	// durable, but the message read as "created, and also something
	// else is broken" rather than "nothing was saved". A real agent
	// created three same-named-but-different-receiver methods in a row,
	// each build-failing except the last, and concluded from three
	// identical-looking "Created ... (id=N)" responses that defn had a
	// def-id collision bug -- when in fact only the last one had ever
	// actually landed. Burned ~10 calls "fixing" a bug that didn't exist.
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// create's default now defers the real build (see handleCreate) --
	// force it back on to exercise the rolled-back-doesn't-claim-success
	// path this test guards.
	t.Setenv("DEFN_STRICT_BUILD", "1")

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func BrokenNewFunc() string { return undefinedHelperFunc() }",
		File: "main.go",
	})
	text := resultText(t, result)
	if strings.Contains(text, "Created BrokenNewFunc") {
		t.Fatalf("response claims BrokenNewFunc was created despite the rolled-back build failure: %s", text)
	}
	if !strings.Contains(text, "rolled back") {
		t.Errorf("expected the response to say the create was rolled back, got: %s", text)
	}
}

// TestHandleOutline_ReflectsRenameOfCompoundReceiverName mirrors a real
// bench trajectory (chi rate-limit task, defn-forced arm, 2026-08-07):
// after renaming a pointer-receiver method via the "(*T).old" compound
// name string, an immediate outline lookup by "(*T).new" returned the
// OLD signature and doc, as if the rename hadn't happened. Reproduces
// the exact call shape from that trajectory to check whether it's live.
func TestHandleOutline_ReflectsRenameOfCompoundReceiverName(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "type tokenBucket struct{ tokens int }",
		File: "bucket.go",
	}); err != nil {
		t.Fatalf("setup: create tokenBucket: %v", err)
	}
	createResult, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func (b *tokenBucket) allow() bool { if b.tokens <= 0 { return false }; b.tokens--; return true }",
		File: "bucket.go",
	})
	if !strings.Contains(resultText(t, createResult), "Created") {
		t.Fatalf("setup: create allow failed: %s", resultText(t, createResult))
	}

	renameResult, _, err := s.handleRename(context.Background(), nil, renameParam{
		OldName: "(*tokenBucket).allow",
		NewName: "acquire",
	})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renameResult == nil || !strings.Contains(resultText(t, renameResult), "Renamed") {
		t.Fatalf("rename did not report success: %v", resultText(t, renameResult))
	}

	outlineResult, _, err := s.handleOutline(context.Background(), nil, nameParam{Name: "(*tokenBucket).acquire"})
	if err != nil {
		t.Fatalf("outline after rename: %v", err)
	}
	out := resultText(t, outlineResult)
	if strings.Contains(out, "allow()") {
		t.Errorf("outline after rename still shows the OLD name/signature:\n%s", out)
	}
	if !strings.Contains(out, "acquire()") {
		t.Errorf("outline after rename does not show the NEW name/signature:\n%s", out)
	}
}

// TestHandleRename_CompoundOldNameStillUpdatesCallers is the caller-side
// counterpart to TestHandleOutline_ReflectsRenameOfCompoundReceiverName:
// same root cause (astRename needs a bare identifier but got the
// receiver-qualified "(*T).method" old_name), different symptom -- a
// real caller referencing the old method name silently kept calling it,
// because `strings.Contains(caller.Body, args.OldName)` never matched
// (the caller body has `b.allow()`, not the literal compound string) so
// the caller wasn't even selected for update, let alone renamed.
func TestHandleRename_CompoundOldNameStillUpdatesCallers(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "type tokenBucket struct{ tokens int }",
		File: "bucket.go",
	}); err != nil {
		t.Fatalf("setup: create tokenBucket: %v", err)
	}
	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "func (b *tokenBucket) allow() bool { if b.tokens <= 0 { return false }; b.tokens--; return true }",
		File: "bucket.go",
	}); err != nil {
		t.Fatalf("setup: create allow: %v", err)
	}
	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "func tryOnce(b *tokenBucket) bool { return b.allow() }",
		File: "bucket.go",
	}); err != nil {
		t.Fatalf("setup: create tryOnce: %v", err)
	}

	renameResult, _, err := s.handleRename(context.Background(), nil, renameParam{
		OldName: "(*tokenBucket).allow",
		NewName: "acquire",
	})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	out := resultText(t, renameResult)
	if strings.Contains(out, "Updated 0 callers") {
		t.Errorf("rename reported 0 callers updated, expected tryOnce to be counted:\n%s", out)
	}

	caller, err := db.GetDefinitionByName("tryOnce", "")
	if err != nil {
		t.Fatalf("lookup tryOnce: %v", err)
	}
	if strings.Contains(caller.Body, "b.allow()") {
		t.Errorf("caller still calls the OLD method name:\n%s", caller.Body)
	}
	if !strings.Contains(caller.Body, "b.acquire()") {
		t.Errorf("caller was not updated to call the NEW method name:\n%s", caller.Body)
	}
}

// TestHandleApply_CreateMultiDeclWithFile is the code(op:"apply") counterpart
// to TestHandleCreateMultiDeclWithFile -- handleApply's own "create" case had
// a separate, stricter countTopLevelDecls check that unconditionally
// rejected bodies with >1 decl, never delegating to the same file:-aware
// multi-decl path handleCreate uses. Caught via a real head-to-head-go
// pilot trajectory (2026-08-07/08): an agent batched one edit + one
// multi-decl create (file: set) into a single apply call -- the exact
// pattern this project's own CLAUDE.md recommends -- and got rejected with
// "split into 2 create ops", burning an extra round-trip to retry as two
// separate create ops.
func TestHandleApply_CreateMultiDeclWithFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{
				Op:   "create",
				File: "main.go",
				Body: "func alpha() int { return 1 }\n\nfunc beta() int { return 2 }",
			},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "created alpha") || !strings.Contains(text, "created beta") {
		t.Fatalf("expected both decls created, got: %s", text)
	}

	final, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(final)
	if !strings.Contains(src, "func alpha()") || !strings.Contains(src, "func beta()") {
		t.Errorf("emitted main.go missing one or both new funcs:\n%s", src)
	}
}

// TestHandleCode_ReadOutlineFileOnlyErrorSuggestsOverview covers a gap seen
// independently in two real head-to-head-go trajectories (2026-08-07/08):
// an agent tried op:"read"/op:"outline" with file: set and no name:,
// intending "show me this whole file". read gave a bare "name is required"
// and outline (which had no upfront name validation at all -- it isn't in
// handleCode's name-required op group) fell through to a downstream lookup
// and returned the far more confusing "definition \"\" not found". Both
// wasted a round-trip on a request op:"overview" already serves correctly.
func TestHandleCode_ReadOutlineFileOnlyErrorSuggestsOverview(t *testing.T) {
	s := &server{backend: nil}

	for _, op := range []string{"read", "outline"} {
		t.Run(op, func(t *testing.T) {
			result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: op, File: "pkg/x.go"})
			text := resultText(t, result)
			if !strings.Contains(text, "name is required") {
				t.Errorf("%s: expected \"name is required\", got: %s", op, text)
			}
			if !strings.Contains(text, `op:"overview"`) || !strings.Contains(text, "pkg/x.go") {
				t.Errorf("%s: expected a pointer to op:\"overview\" with the file, got: %s", op, text)
			}
		})
	}
}

// TestHandleTest_OnTestFunctionSuggestsTestParam covers a gap seen
// independently in three real head-to-head-go trajectories: an agent
// calling op:"test", name:"TestFoo" on a test function it just wrote,
// expecting it to run. handleTest's name param means "what tests cover
// this def", so for a test function itself (nothing calls a test) it
// always returned the generic "No tests cover TestFoo. Nothing to run." --
// indistinguishable from a real dead-code case, forcing a second call with
// the differently-named test: param to actually run it.
func TestHandleTest_OnTestFunctionSuggestsTestParam(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	create, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:   "create",
		File: "main.go",
		Body: "func TestSomethingNew(t *testing.T) {}",
	})
	if strings.Contains(resultText(t, create), "rolled back") {
		t.Fatalf("setup create failed: %s", resultText(t, create))
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "test", Name: "TestSomethingNew"})
	text := resultText(t, result)
	if !strings.Contains(text, `test:"TestSomethingNew"`) {
		t.Errorf("expected a pointer to test:\"TestSomethingNew\", got: %s", text)
	}
	if strings.Contains(text, "No tests cover") {
		t.Errorf("still using the generic dead-code message for a test function: %s", text)
	}
}

// TestHandleEdit_RejectsMultiDeclNewBody replicates a real head-to-head-go
// trajectory bug (2026-08-08 pilot digging): new_body concatenated 3
// function declarations as one string. edit had no multi-decl check
// (unlike create's countTopLevelDecls guard) -- it parsed fine (Go allows
// several func decls in one string) and passed the identity check (which
// only looks at the first decl), so the whole blob got stored verbatim as
// the target definition's Body. A later sync/re-ingest of the emitted
// file then split the extra two decls into duplicate definitions,
// producing a "redeclared in this block" build failure that took the
// real agent over a dozen confused follow-up calls (a failed apply, four
// failing replace-hunk attempts, a pointless rename-and-rename-back) to
// work around.
func TestHandleEdit_RejectsMultiDeclNewBody(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:   "edit",
		Name: "Greet",
		NewBody: "func Greet(name string) string { return helper(name) }\n\n" +
			"func helper(name string) string { return name }",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "top-level declarations") {
		t.Fatalf("expected a multi-decl rejection, got: %s", text)
	}

	// Must not have been silently stored as a mangled body.
	read, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Greet"})
	if strings.Contains(resultText(t, read), "func helper") {
		t.Errorf("rejected edit still landed the extra decl into Greet's body: %s", resultText(t, read))
	}
}

// TestHandleApply_EditRejectsMultiDeclNewBody is the apply-batched
// counterpart to TestHandleEdit_RejectsMultiDeclNewBody -- same gap,
// reached via handleApply's own "edit" case instead of the standalone
// handleEdit path (both are exercised by real trajectories).
func TestHandleApply_EditRejectsMultiDeclNewBody(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{
				Op:   "edit",
				Name: "Greet",
				NewBody: "func Greet(name string) string { return helper(name) }\n\n" +
					"func helper(name string) string { return name }",
			},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "top-level declarations") {
		t.Fatalf("expected a multi-decl rejection, got: %s", text)
	}

	read, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Greet"})
	if strings.Contains(resultText(t, read), "func helper") {
		t.Errorf("rejected batched edit still landed the extra decl into Greet's body: %s", resultText(t, read))
	}
}

// TestHandleFragmentEdit_RejectsMultiDeclResult is the old_fragment/
// new_fragment counterpart to TestHandleEdit_RejectsMultiDeclNewBody --
// same gap, since a fragment replacement that inserts a whole extra
// declaration produces the identical silently-mangled-body outcome.
func TestHandleFragmentEdit_RejectsMultiDeclResult(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:          "edit",
		Name:        "Greet",
		OldFragment: `return "Hello, " + name`,
		NewFragment: "return helper(name)\n}\n\nfunc helper(name string) string {\n\treturn name",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "top-level declarations") {
		t.Fatalf("expected a multi-decl rejection, got: %s", text)
	}

	read, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Greet"})
	if strings.Contains(resultText(t, read), "func helper") {
		t.Errorf("rejected fragment edit still landed the extra decl into Greet's body: %s", resultText(t, read))
	}
}

// TestHandleSimilar_NoLongerUsesRetiredSignatureFallback covers the
// handleSimilarBySignature retirement: that fallback used a signature
// LIKE prefilter -- the exact anti-pattern flagged in the calque revert
// postmortem ("blocking on signature LIKE guarantees the misses") -- and
// silently served structurally different, lower-quality results for any
// def whose body was too short to shingle. Every def now gets a real
// MinHash from store.ComputeMinHashForDef (body when long enough, else
// signature), so there's one consistent scoring path.
func TestHandleSimilar_NoLongerUsesRetiredSignatureFallback(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "similar", Name: "Greet"})
	if err != nil {
		t.Fatalf("similar: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "body-less fallback") || strings.Contains(text, "similar signatures to") {
		t.Errorf("still surfacing the retired signature-LIKE fallback's output shape: %s", text)
	}
}

// TestHandleApply_EditReceiverDisambiguatesSameNamedMethod replicates a
// real head-to-head-go trajectory (2026-08-08): applyOp had no receiver
// field at all, so a same-named method on a different type couldn't be
// disambiguated inside a batch -- unlike every standalone handler
// (handleEdit, handleDelete, etc via nameParam/editParam's Receiver).
// The real agent tried passing receiver: anyway and got a schema
// rejection ("unexpected additional properties [\"receiver\"]"), then
// burned ~10 more apply retries working around it with stale
// old_fragments and confused identity-check failures. Two types here
// both declare a Handle method with different bodies -- editing one via
// apply must not silently hit (or fail to find) the other.
func TestHandleApply_EditReceiverDisambiguatesSameNamedMethod(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	create, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:   "create",
		File: "handlers.go",
		Body: "type A struct{}\n\nfunc (a A) Handle() string { return \"a\" }\n\n" +
			"type B struct{}\n\nfunc (b B) Handle() string { return \"b\" }",
	})
	if strings.Contains(resultText(t, create), "rolled back") {
		t.Fatalf("setup create failed: %s", resultText(t, create))
	}

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "edit", Name: "Handle", Receiver: "B", NewBody: "func (b B) Handle() string { return \"b-edited\" }"},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "not found") || strings.Contains(text, "rolled back") {
		t.Fatalf("expected the receiver-qualified edit to resolve B.Handle, got: %s", text)
	}

	readB, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Handle", Receiver: "B"})
	if !strings.Contains(resultText(t, readB), "b-edited") {
		t.Errorf("B.Handle was not updated: %s", resultText(t, readB))
	}
	readA, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Handle", Receiver: "A"})
	if !strings.Contains(resultText(t, readA), `"a"`) {
		t.Errorf("A.Handle should be untouched, got: %s", resultText(t, readA))
	}
}

// TestHandleEdit_BuildFailureMessageDoesNotClaimSuccess is the message-
// wording counterpart to TestHandleEdit_SignatureChangedBuildFailureRollsBackBothDBAndFile:
// that test only checked DB/file state, not the response text. A real
// trajectory (2026-08-08) hit this exact shape three times in one
// session -- "Updated X (id=N, hash=H)\n\nBUILD FAILED: ..." reads as
// "it saved, but something else is also broken" rather than "nothing
// was saved," the same misleading-message bug handleCreate already got
// fixed for.
func TestHandleEdit_BuildFailureMessageDoesNotClaimSuccess(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleEdit(context.Background(), nil, editParam{
		Name:    "Greet",
		NewBody: "func Greet(name string, extra int) string { return undefinedHelperFunc(name, extra) }",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back — nothing was saved") {
		t.Errorf("expected an explicit rollback message, got: %s", text)
	}
	if strings.Contains(text, "Updated Greet (id=") {
		t.Errorf("still claims success alongside the build failure: %s", text)
	}
}

// TestHandleFragmentEdit_BuildFailureMessageDoesNotClaimSuccess is the
// fragment-edit counterpart to TestHandleEdit_BuildFailureMessageDoesNotClaimSuccess.
func TestHandleFragmentEdit_BuildFailureMessageDoesNotClaimSuccess(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleFragmentEdit(context.Background(), nil, codeParam{
		Op:          "edit",
		Name:        "Greet",
		OldFragment: `return "Hello, " + name`,
		NewFragment: `return undefinedHelperFunc(name)`,
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back — nothing was saved") {
		t.Errorf("expected an explicit rollback message, got: %s", text)
	}
	if strings.Contains(text, "Edited Greet — replaced") {
		t.Errorf("still claims success alongside the build failure: %s", text)
	}
}

// TestHandleApply_ReplaceHunkAcceptsFragmentFieldNames is the apply-
// batched counterpart to TestHandleCode_ReplaceHunkAcceptsFragmentFieldNames
// -- same gap, hit in a real trajectory (grpc-2631) inside an apply batch
// that mixed an edit (fragment mode) and replace-hunk ops together.
func TestHandleApply_ReplaceHunkAcceptsFragmentFieldNames(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "replace-hunk", Name: "Greet", OldFragment: `return "Hello, " + name`, NewFragment: `return "Hi, " + name`},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "old is required") || strings.Contains(text, "not found") || strings.Contains(text, "rolled back") {
		t.Fatalf("old_fragment/new_fragment should be accepted as old/new aliases, got: %s", text)
	}

	read, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Greet"})
	if !strings.Contains(resultText(t, read), `"Hi, "`) {
		t.Errorf("Greet was not updated: %s", resultText(t, read))
	}
}

// TestHandleCode_ReplaceHunkAcceptsFragmentFieldNames replicates a real
// trajectory (2026-08-08, cli-1069): an agent called op:"replace-hunk"
// with old_fragment/new_fragment -- edit's fragment-mode field names for
// the identical "before/after text" concept -- and got rejected with
// "replace-hunk: old is required", wasting a round-trip before retrying
// with the correct old/new names. Now accepted as aliases.
func TestHandleCode_ReplaceHunkAcceptsFragmentFieldNames(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:          "replace-hunk",
		Name:        "Greet",
		OldFragment: `return "Hello, " + name`,
		NewFragment: `return "Hi, " + name`,
	})
	text := resultText(t, result)
	if strings.Contains(text, "old is required") || strings.Contains(text, "new is required") {
		t.Fatalf("old_fragment/new_fragment should be accepted as old/new aliases, got: %s", text)
	}
	if !strings.Contains(text, "replaced hunk") {
		t.Errorf("expected a successful hunk replacement, got: %s", text)
	}
}

// TestHandleApply_RenamePointerReceiverMethodThenEditSameBatch reproduces
// the exact shape from the 2026-08-07 defn-forced pilot trajectory
// (bench/session-cumulative/2026-08-07-raw/out/defn-forced/turn-01.json):
// one apply batch renames a pointer-receiver method AND edits that same
// renamed method's body AND creates new defs in the same file, all in one
// transaction. On that trajectory this produced "WARNING: ... could not
// be matched to an on-disk declaration" (old name left on disk, DB moved
// on to the new name) followed by a sync that reverted the DB's rename,
// forcing the agent into a create+delete cleanup dance (~12 extra tool
// calls). If commitOrRollbackOnBuild's #218 contract (a WARNING is
// failure-equivalent, so the whole batch rolls back) is honored here, this
// exact combination must either land cleanly (old name gone, new name and
// new body present) or fail atomically -- never partially diverge.
func TestHandleApply_RenamePointerReceiverMethodThenEditSameBatch(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "type tokenBucket struct{ tokens int }",
		File: "main.go",
	}); err != nil {
		t.Fatalf("setup: create tokenBucket type: %v", err)
	}
	createResult, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func (b *tokenBucket) allow() bool { return b.tokens > 0 }",
		File: "main.go",
	})
	if !strings.Contains(resultText(t, createResult), "Created") {
		t.Fatalf("setup: create allow failed: %s", resultText(t, createResult))
	}

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "edit", Name: "tokenBucket", NewBody: "// holds tokens\ntype tokenBucket struct{ tokens int }"},
			{Op: "rename", Name: "(*tokenBucket).allow", NewName: "acquire"},
			{Op: "edit", Name: "(*tokenBucket).acquire", NewBody: "func (b *tokenBucket) acquire() bool { return b.tokens > 0 }"},
			{Op: "create", File: "main.go", Name: "release", Body: "func (b *tokenBucket) release() { b.tokens++ }"},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "WARNING") && !strings.Contains(text, "apply rolled back") {
		t.Fatalf("apply reported a WARNING but did not roll back the whole batch -- #218 contract violated, DB and disk are now allowed to diverge:\n%s", text)
	}

	final, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(final)

	if strings.Contains(text, "apply rolled back") {
		// Atomic failure is an acceptable outcome per #218 -- just make
		// sure disk truly reflects the pre-batch state (old method only).
		if !strings.Contains(src, "func (b *tokenBucket) allow()") {
			t.Errorf("apply rolled back but disk lost the original allow() method:\n%s", src)
		}
		if strings.Contains(src, "func (b *tokenBucket) acquire()") {
			t.Errorf("apply rolled back but disk has the new acquire() method anyway:\n%s", src)
		}
		return
	}

	// No rollback reported -- the batch must have landed COMPLETELY and
	// consistently: old name gone, new name present with its edited body,
	// new method present. Any partial state here is the divergence bug.
	if strings.Contains(src, "func (b *tokenBucket) allow()") {
		t.Errorf("apply reported success but old allow() method is still on disk (should have been renamed away):\n%s", src)
	}
	if !strings.Contains(src, "func (b *tokenBucket) acquire()") {
		t.Errorf("apply reported success but new acquire() method is missing from disk:\n%s", src)
	}
	if !strings.Contains(src, "func (b *tokenBucket) release()") {
		t.Errorf("apply reported success but new release() method is missing from disk:\n%s", src)
	}

	after, err := db.GetDefinitionByName("acquire", "")
	if err != nil {
		t.Fatalf("acquire not found in DB after apply reported success: %v", err)
	}
	if after.Body != "func (b *tokenBucket) acquire() bool { return b.tokens > 0 }" {
		t.Errorf("DB's acquire body doesn't match the edit applied in the same batch: %q", after.Body)
	}
}

// TestAlreadyFreshlyIngested guards the #241 fix: newMCPServer's startup
// goroutine must skip its redundant full packages.Load+ingest+resolve
// when the DB already covers every .go file on disk (e.g. right after
// `defn init`/`defn ingest .`), and must NOT skip it otherwise -- a
// false positive here would leave a genuinely stale or never-ingested
// DB silently marked ready, which is worse than the "may be stale"
// warning it replaces. Root-cause trajectory: a real grpc-go-2630 run
// where the first `search` call raced the always-on reingest and
// returned unrelated defs, leading the agent to edit the wrong function.
func TestAlreadyFreshlyIngested(t *testing.T) {
	// #332: .defn lives INSIDE the project directory in real usage
	// (defn init/ingest nest it there, and newMCPServer is always
	// invoked with the real project root) -- build fixtures with that
	// same nesting rather than reusing setupTestDB's separate sibling
	// .defn/testproj layout, since alreadyFreshlyIngested now compares
	// against the DB file's own on-disk mtime under projectDir/.defn.
	setup := func(t *testing.T) (store.Backend, string) {
		t.Helper()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
		os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc Greet(name string) string { return \"Hello, \" + name }\n\nfunc main() {}\n"), 0644)
		db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
		if err != nil {
			t.Fatal(err)
		}
		if err := ingest.Ingest(db, dir); err != nil {
			t.Fatal("ingest:", err)
		}
		if err := resolve.Resolve(db, dir); err != nil {
			t.Fatal("resolve:", err)
		}
		return db, dir
	}

	t.Run("fresh right after ingest", func(t *testing.T) {
		db, projDir := setup(t)
		defer db.Close()
		if !alreadyFreshlyIngested(db, projDir) {
			t.Error("expected fresh DB right after ingest to be reported as already fresh")
		}
	})

	t.Run("stale after a file is touched", func(t *testing.T) {
		db, projDir := setup(t)
		defer db.Close()

		dbInfo, err := os.Stat(filepath.Join(projDir, ".defn", "defn.db"))
		if err != nil {
			t.Fatalf("stat defn.db: %v", err)
		}

		mainGo := filepath.Join(projDir, "main.go")
		future := dbInfo.ModTime().Add(10 * time.Second)
		if err := os.Chtimes(mainGo, future, future); err != nil {
			t.Fatalf("chtimes: %v", err)
		}

		if alreadyFreshlyIngested(db, projDir) {
			t.Error("expected a file modified after the DB was written to be reported as NOT fresh")
		}
	})

	t.Run("never ingested", func(t *testing.T) {
		dir := t.TempDir()
		db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)

		if alreadyFreshlyIngested(db, dir) {
			t.Error("expected a DB with no defn.db on disk yet to never be reported as fresh")
		}
	})

	t.Run("fresh despite a clone-fresh mtime on go files (#332)", func(t *testing.T) {
		// The exact bug: git clone stamps every checked-out file with
		// the clone's OWN wall-clock time regardless of content, then a
		// previously-ingested .defn/ gets restored (e.g. from a cache
		// tarball) SEPARATELY and LATER. The old mtime-vs-stored-value
		// check saw every .go file as "newer than last_ingest" and
		// forced a full reingest on every single run despite the DB
		// already being 100% current. Simulate that ordering: ingest
		// first (so file content matches the DB), bump the .go file's
		// mtime forward as a clone would, then restore-in-place the
		// .defn dir by touching defn.db even later -- the real
		// harness's tar-extract-after-clone sequence.
		db, projDir := setup(t)
		defer db.Close()

		now := time.Now()
		mainGo := filepath.Join(projDir, "main.go")
		cloneTime := now.Add(5 * time.Second)
		if err := os.Chtimes(mainGo, cloneTime, cloneTime); err != nil {
			t.Fatalf("chtimes main.go: %v", err)
		}
		dbFile := filepath.Join(projDir, ".defn", "defn.db")
		restoreTime := now.Add(10 * time.Second)
		if err := os.Chtimes(dbFile, restoreTime, restoreTime); err != nil {
			t.Fatalf("chtimes defn.db: %v", err)
		}

		if !alreadyFreshlyIngested(db, projDir) {
			t.Error("expected a DB restored AFTER a fresh clone's mtime bump to still be reported as fresh")
		}
	})
}

// TestHandleCode_TestOpWithTestFieldDoesNotRequireName guards the #241
// fix: op:"test" with test:"TestX" (named-test reproduction, documented
// as the way to replicate a bug report before touching any code) was
// rejected with "test: name is required" because the pre-dispatch
// validation switch grouped "test" with name-required ops (read/
// outline/impact/delete/history/similar) without checking whether
// test: had already been supplied.
func TestHandleCode_TestOpWithTestFieldDoesNotRequireName(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "test", Test: "TestGreet"})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "name is required") {
		t.Errorf("op:test with test:\"TestGreet\" should not require name, got: %s", text)
	}
}

// TestHandleSearch_FileScopesResults guards the #241 fix: search's
// file: param was accepted but silently ignored, so every search ran
// repo-wide regardless of the hint -- root-caused via a real
// grpc-go-2630 trajectory where search(pattern:"drop", file:"grpclb")
// returned unrelated repo-wide results instead of scoping to the
// grpclb package, contributing to a wrong-function edit.
func TestHandleSearch_FileScopesResults(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.MkdirAll(filepath.Join(projDir, "beta"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha.go"), []byte(`package alpha

// Widget handles the banana queue.
func Widget() {}
`), 0644)
	os.WriteFile(filepath.Join(projDir, "beta", "beta.go"), []byte(`package beta

// Gadget handles the banana crate.
func Gadget() {}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	// Unscoped: both Widget and Gadget match "banana".
	result, _, err := s.handleSearch(context.Background(), nil, codeParam{Pattern: "banana"})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Widget") || !strings.Contains(text, "Gadget") {
		t.Fatalf("expected both Widget and Gadget unscoped, got: %s", text)
	}

	// Scoped to alpha/: only Widget should survive.
	result, _, err = s.handleSearch(context.Background(), nil, codeParam{Pattern: "banana", File: "alpha"})
	if err != nil {
		t.Fatalf("handleSearch with file: %v", err)
	}
	text = resultText(t, result)
	if !strings.Contains(text, "Widget") {
		t.Errorf("file:\"alpha\" should still include Widget (in alpha/), got: %s", text)
	}
	if strings.Contains(text, "Gadget") {
		t.Errorf("file:\"alpha\" should have excluded Gadget (in beta/), got: %s", text)
	}
}

// TestHandleTestByName_ModuleScopesGoTestTarget guards the #241 fix:
// test:"TestX" with module:/file: previously ran `go test ./...`
// across the WHOLE repo regardless, silently ignoring the scope hint.
// On a large repo (real trajectory: go-zero) this is both a real
// wire-cost tax -- every unrelated package's build+test output ships
// back, most of it "no tests to run" -- and confusing enough that the
// agent retried the same named test 5+ times with different -run
// variations, unable to spot its own result in the flood. module:/
// file: now scope the go test invocation to that package's directory.
func TestHandleTestByName_ModuleScopesGoTestTarget(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.MkdirAll(filepath.Join(projDir, "beta"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha.go"), []byte(`package alpha

func Widget() bool { return true }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha_test.go"), []byte(`package alpha

import "testing"

func TestWidget(t *testing.T) {
	if !Widget() {
		t.Fatal("false")
	}
}
`), 0644)
	os.WriteFile(filepath.Join(projDir, "beta", "beta.go"), []byte(`package beta

func Gadget() bool { return true }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "beta", "beta_test.go"), []byte(`package beta

import "testing"

func TestGadget(t *testing.T) {
	if !Gadget() {
		t.Fatal("false")
	}
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.handleTestByName(context.Background(), nil, "TestWidget", "", "alpha")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "./alpha/...") {
		t.Errorf("expected the go test target to be scoped to ./alpha/..., got: %s", text)
	}
	if strings.Contains(text, "beta") {
		t.Errorf("scoped run should never touch the beta package at all, got: %s", text)
	}
	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("expected TestWidget to pass, got: %s", text)
	}
}

// TestHandleTestByName_InfersScopeFromPatternWhenNoHintGiven guards the
// #241 followup: test:"TestX" with NO module:/file: hint still ran
// go test ./... across the whole repo. Real trajectory (cli-2671): a
// pre-existing, unrelated compile error in a sibling package (never
// imported by the package actually being edited) made every such call
// fail regardless of whether the agent's own edit was correct. Since
// the pattern is very often the literal test name, resolve it via the
// DB and scope to ITS OWN package instead of guessing the caller meant
// to test everything.
func TestHandleTestByName_InfersScopeFromPatternWhenNoHintGiven(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha.go"), []byte(`package alpha

func Widget() bool { return true }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha_test.go"), []byte(`package alpha

import "testing"

func TestWidget(t *testing.T) {
	if !Widget() {
		t.Fatal("false")
	}
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	// beta is added to disk AFTER ingest, deliberately never re-ingested
	// -- it stands in for "some other package in a large repo doesn't
	// build," which a scope inferred from TestWidget's own package
	// (alpha) must never need to care about. Untracked-by-the-DB is the
	// realistic shape here: what actually broke the real cli-2671
	// trajectory was defn's OWN emit corrupting an unrelated file the
	// agent never touched (see the emitModule basename-collision fix),
	// which is exactly "a package the DB doesn't expect to be broken."
	os.MkdirAll(filepath.Join(projDir, "beta"), 0755)
	os.WriteFile(filepath.Join(projDir, "beta", "beta.go"), []byte(`package beta

func Broken() {
	undefinedFunc()
}
`), 0644)

	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.handleTestByName(context.Background(), nil, "TestWidget", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "./alpha/...") {
		t.Errorf("expected the go test target to be inferred as ./alpha/..., got: %s", text)
	}
	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("expected TestWidget to pass without beta's broken package poisoning the build, got: %s", text)
	}
}

// TestHandleEdit_RollbackNamesTheBrokenCallerForApplyBatching guards the
// #241 fix: a build-failure rollback now names which definition owns
// the broken call site instead of leaving the agent to reverse-engineer
// a bare file:line back into a def name. Real trajectory (cli-1069):
// the agent had already seen "1 direct caller" via a prior impact call,
// edited a return-arity-changing signature alone anyway, hit exactly
// this shape of build failure, and spent one extra call figuring out
// it needed to batch the edit with its caller via apply.
func TestHandleEdit_RollbackNamesTheBrokenCallerForApplyBatching(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// setupTestDB's fixture: Farewell calls Greet(name) with one arg.
	// Changing Greet to require a second param breaks that call site
	// without touching Farewell at all -- the exact coupled-change shape.
	result, _, _ := s.handleEdit(context.Background(), nil, editParam{
		Name:    "Greet",
		NewBody: "func Greet(name string, extra int) string { return name }",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back — nothing was saved") {
		t.Fatalf("expected an explicit rollback message, got: %s", text)
	}
	if !strings.Contains(text, "Farewell") {
		t.Errorf("expected the rollback to name Farewell (the broken caller), got: %s", text)
	}
	if !strings.Contains(text, "apply") {
		t.Errorf("expected a suggestion to batch via apply, got: %s", text)
	}
}

// TestHandleImpact_TipsCoupledChangeWhenProdCallersExist guards the
// #241 fix: impact already reported caller counts, but only ever
// framed them as a test-coverage risk ("no test coverage — a change
// here may break code no test will catch"). A real trajectory called
// impact, saw "1 direct caller: titleBodySurvey," and still edited a
// return-arity-changing signature alone -- the caller-count fact was
// there, but nothing connected it to "you're about to break this call
// site." Now impact says that explicitly whenever production callers
// exist, regardless of test coverage.
func TestHandleImpact_TipsCoupledChangeWhenProdCallersExist(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Farewell is a production caller of Greet in setupTestDB's fixture.
	result, _, _ := s.handleImpact(context.Background(), nil, codeParam{Name: "Greet"})
	text := resultText(t, result)
	if !strings.Contains(text, "batch it with its production caller") {
		t.Errorf("expected a coupled-change tip when production callers exist, got: %s", text)
	}
}

// TestHandleGetDefinition_ResolvesDottedQualifiedName guards the #241
// fix: a bare-name lookup failure now retries Go's own natural
// "pkg.Symbol"/"pkg/path.Symbol" qualified-name convention before
// giving up. Real trajectory (go-zero-2283):
// read(name:"rest/internal/cors.Middleware") came back "not found"
// even though Middleware exists right there (also as an unrelated type
// of the same name in a different package), forcing an extra
// outline+file: round trip to recover.
func TestHandleGetDefinition_ResolvesDottedQualifiedName(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.MkdirAll(filepath.Join(projDir, "beta"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	// Widget exists in BOTH packages under the same bare name -- a bare
	// lookup alone is ambiguous; the dotted form must disambiguate.
	os.WriteFile(filepath.Join(projDir, "alpha", "widget.go"), []byte(`package alpha

func Widget() string { return "alpha" }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "beta", "widget.go"), []byte(`package beta

func Widget() string { return "beta" }
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "beta.Widget"})
	if err != nil {
		t.Fatalf("handleGetDefinition: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, `"beta"`) {
		t.Errorf("expected beta.Widget's own body (returns \"beta\"), got: %s", text)
	}
	if strings.Contains(text, `"alpha"`) {
		t.Errorf("resolved to the wrong package's Widget, got: %s", text)
	}
}

func TestHandleCode_DeleteDryRunDoesNotDelete(t *testing.T) {
	// Regression test for a real bug found in a head-to-head-go
	// trajectory: code(op:"delete", name:"X", dry_run:true) silently
	// executed a real delete instead of previewing it. Root cause:
	// handleCode's dispatch built nameParam{Name, Force, Receiver,
	// Module, File} for the delete case without copying args.DryRun,
	// and nameParam had no DryRun field for handleDelete to read even
	// if it had been copied. apply's dry_run already previews deletes
	// correctly ("- would delete X"); delete's standalone dry_run was a
	// complete no-op. Goes through handleCode, not handleDelete
	// directly, to catch the dispatch-level gap (same shape as
	// TestHandleCode_DeleteReceiverDisambiguatesThroughDispatch).
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	mod, err := db.GetModuleByPath("testproj")
	if err != nil {
		t.Fatalf("find testproj module: %v", err)
	}

	targetID, err := db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Standalone", Kind: "function",
		Body: "func Standalone() bool { return true }", SourceFile: "main.go",
	})
	if err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op: "delete", Name: "Standalone", DryRun: true,
	})
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected dry-run delete to succeed, got error: %s", text)
	}
	if !strings.Contains(text, "would delete") || !strings.Contains(text, "dry run") {
		t.Fatalf("expected dry-run preview text, got: %s", text)
	}

	if _, err := db.GetDefinition(targetID); err != nil {
		t.Fatalf("Standalone should NOT have been deleted by a dry run, but it's gone: %v", err)
	}
}

func TestHandleCreateMultiDecl_NewNestedPackageGetsOwnModule(t *testing.T) {
	// Regression test for a real bug hit while authoring a brand-new
	// package via multi-decl create: when file: points at a directory
	// with no existing module (findModuleByFile returns nil),
	// handleCreateMultiDecl fell back to "the existing module with the
	// shortest registered path" instead of creating a module scoped to
	// the new directory -- unlike handleCreate's own #13 fix for the same
	// situation. In a large multi-package repo that shortest-path module
	// is some ARBITRARY, unrelated package -- every def in the new file
	// got silently attributed to it, and a name that happened to already
	// exist there (a common local helper name like testDB) falsely
	// refused as "already exists" against a completely unrelated def.
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// Seed an unrelated existing def with a common local-helper name in
	// the pre-existing (and, being the only module, shortest-path)
	// module -- the collision this bug would trigger.
	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "func helper() int { return 1 }",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	body := `func helper() int { return 2 }

func Other() int { return 3 }`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "pkg/newthing/file.go",
	})
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected new-package create to succeed, got error: %s", text)
	}
	if !strings.Contains(text, "Created 2 defs") {
		t.Fatalf("expected 'Created 2 defs', got: %s", text)
	}
	if strings.Contains(text, "testproj)") {
		t.Fatalf("new nested package must NOT be attributed to the pre-existing root module, got: %s", text)
	}
	if !strings.Contains(text, "newthing") {
		t.Fatalf("expected the new module's path to mention the new directory, got: %s", text)
	}

	defs, err := db.FindDefinitions("helper")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected both the original and the new 'helper' to coexist as distinct defs, got %d: %+v", len(defs), defs)
	}
	if defs[0].ModuleID == defs[1].ModuleID {
		t.Fatalf("new package's helper must not share ModuleID with the unrelated pre-existing module")
	}
}

func TestHandleApply_CreateMultiDeclNewNestedPackageGetsOwnModule(t *testing.T) {
	// Same regression as TestHandleCreateMultiDecl_NewNestedPackageGetsOwnModule,
	// but through apply's own independent multi-decl "create" implementation
	// -- a third sibling of the same concept (handleCreate, handleCreateMultiDecl,
	// apply's inline create case), which had the identical shortest-path
	// fallback bug and non-receiver-aware collision check.
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "func helper() int { return 1 }",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	body := `func helper() int { return 2 }

func Other() int { return 3 }`

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "create", Body: body, File: "pkg/newthing/file.go"}},
	})
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected new-package apply create to succeed, got error: %s", text)
	}
	if strings.Contains(text, "Errors") {
		t.Fatalf("expected no errors, got: %s", text)
	}

	defs, err := db.FindDefinitions("helper")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected both the original and the new 'helper' to coexist as distinct defs, got %d: %+v", len(defs), defs)
	}
	if defs[0].ModuleID == defs[1].ModuleID {
		t.Fatalf("new package's helper must not share ModuleID with the unrelated pre-existing module")
	}
}

// TestHandleCode_EditDryRunDoesNotEdit is a regression test for a real
// bug found in a grpc-go head-to-head-go trajectory:
// code(op:"edit", name:"X", new_body:"...", dry_run:true) silently
// performed a REAL edit instead of previewing it -- worse than an
// error, since nothing signaled the mistake. Root cause: editParam had
// no DryRun field, and handleCode's dispatch built editParam{Name,
// NewBody, Receiver, Module, File} for the edit case without copying
// args.DryRun. delete's equivalent dry_run gap was already fixed
// (TestHandleCode_DeleteDryRunDoesNotDelete); edit had the same gap
// and was never covered.
func TestHandleCode_EditDryRunDoesNotEdit(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}
	oldBody := before.Body

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:   "edit",
		Name: "Greet",
		NewBody: `func Greet(name string) string {
	return "Hi, " + name
}`,
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected dry-run edit to succeed, got error: %s", text)
	}
	if !strings.Contains(text, "would update") || !strings.Contains(text, "dry run") {
		t.Fatalf("expected dry-run preview text, got: %s", text)
	}

	after, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != oldBody {
		t.Fatalf("Greet's body should NOT have changed on a dry run, but changed from:\n%s\nto:\n%s", oldBody, after.Body)
	}

	onDisk, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), `"Hi, "`) {
		t.Fatalf("dry-run edit should not have written to disk, but main.go now contains the new body:\n%s", onDisk)
	}
}

// TestHandleCode_FailedSyncStillInvalidatesCache is a regression test
// for a real bug found in a grpc-go head-to-head-go trajectory:
// handleCode's deferred cache bookkeeping gated ALL invalidation on
// result.IsError == false, so a write op that failed never
// invalidated the session's response cache -- even though defn's own
// SQLite writes commit as they happen (no staged/uncommitted working
// set to roll back to; see autoCommit's doc comment), so a
// partially-applied write op can leave the DB genuinely changed
// despite reporting an error. A later read of the same name then got
// served defn's own "already read this session -- nothing has changed"
// shortcut with false confidence.
func TestHandleCode_FailedSyncStillInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

func Make() string {
	return "widget"
}
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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

	first, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "Make", Full: true})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if !strings.Contains(resultText(t, first), "widget") {
		t.Fatalf("expected first read to show Make's body, got: %s", resultText(t, first))
	}

	// Break main.go's syntax on disk (outside defn), then sync just that
	// file -- a write op (isWriteOp("sync") == true) that fails.
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

func Make() string {
	return "widget-v2" +++ broken syntax
}
`), 0644)
	syncResult, _, err := s.handleCode(ctx, req, codeParam{Op: "sync", File: "main.go"})
	if err != nil {
		t.Fatalf("sync call itself errored at the Go level: %v", err)
	}
	if !syncResult.IsError {
		t.Fatalf("expected the syntax-broken sync to report an error, got: %s", resultText(t, syncResult))
	}

	// A follow-up plain read must NOT be served the stale "already read"
	// shortcut -- the failed sync must still have invalidated it.
	second, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "Make"})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	secondText := resultText(t, second)
	if strings.Contains(secondText, "already read in this session") {
		t.Fatalf("a failed sync did not invalidate the response cache -- read was short-circuited to the stale shortcut:\n%s", secondText)
	}
}

// TestHandleCode_ReplaceHunkBuildFailureShowsFullDiagnostics is a
// regression test for a real bug found in a head-to-head-go
// trajectory: applyEditTerse (the shared response path for
// replace-hunk, replace-slice, insert-precondition, wrap-in-defer, and
// rename-param) kept only the first line of a build failure -- "build
// failed:" -- discarding the actual compiler diagnostic (undefined
// symbol, file/line) that handleEdit's own failure path shows in full
// for the exact same underlying error. Forced an agent to blind-guess
// or fall back to the verbose edit op whenever a projection-op edit
// failed to build. DEFN_STRICT_BUILD=1 forces the real go build gate
// that applyEditTerse normally skips for its AST-guaranteed-sig-stable
// fast path -- without it there's no BUILD FAILED text to truncate.
func TestHandleCode_ReplaceHunkBuildFailureShowsFullDiagnostics(t *testing.T) {
	t.Setenv("DEFN_STRICT_BUILD", "1")
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:   "replace-hunk",
		Name: "Greet",
		Old:  `return "Hello, " + name`,
		New:  `return undefinedHelperFunc(name)`,
	})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "undefinedHelperFunc") {
		t.Fatalf("expected the full build diagnostic naming the undefined symbol, got only:\n%s", text)
	}
}

// TestHandleSync_ModuleScopesToThatModuleOnly is a regression test for
// a real bug found in a grpc-go head-to-head-go trajectory:
// code(op:"sync", module:"...") was accepted by the schema but
// handleSync only ever checked args.File, silently falling through to
// a whole-repo ingestAndResolve() -- which loads and type-checks every
// package in the module via packages.Load. An unrelated, unbuildable
// sibling package elsewhere in the repo then failed the ENTIRE sync,
// even though the caller only asked to resync one specific,
// perfectly buildable module.
func TestHandleSync_ModuleScopesToThatModuleOnly(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "widgets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "broken"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "widgets", "widgets.go"), []byte(`package widgets

func Make() string {
	return "widget"
}
`), 0644)
	os.WriteFile(filepath.Join(projDir, "broken", "broken.go"), []byte(`package broken

func Break() string {
	return "ok"
}
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	// Break the "broken" package on disk AFTER the initial successful
	// ingest, without going through defn -- simulates an external edit
	// that a full-repo resync would trip over. Update widgets.go too, so
	// the module-scoped sync under test has something real to pick up.
	os.WriteFile(filepath.Join(projDir, "broken", "broken.go"), []byte(`package broken

func Break() string {
	return undefinedSymbolHere
}
`), 0644)
	os.WriteFile(filepath.Join(projDir, "widgets", "widgets.go"), []byte(`package widgets

func Make() string {
	return "widget-v2"
}
`), 0644)

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:     "sync",
		Module: "widgets",
	})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	if result.IsError {
		t.Fatalf("module-scoped sync should not fail due to an unrelated broken sibling package, got: %s", resultText(t, result))
	}

	d, err := db.GetDefinitionByName("Make", "")
	if err != nil {
		t.Fatalf("Make should still be findable: %v", err)
	}
	if !strings.Contains(d.Body, "widget-v2") {
		t.Errorf("expected Make's body to reflect the on-disk update after module-scoped sync, got:\n%s", d.Body)
	}
}

// TestHandleApply_EditRefusesAmbiguousBareNameAcrossModules is the
// #248 regression for the apply-batch path specifically: apply's
// edit/delete/rename/projection sub-ops all resolve their target via
// resolveApplyTarget, a completely separate function from
// resolveEditTarget/resolveWriteTarget that had its own independent,
// unguarded ambiguous bare-name lookup.
func TestHandleApply_EditRefusesAmbiguousBareNameAcrossModules(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "bft"), 0755)
	os.MkdirAll(filepath.Join(projDir, "chess"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte("package bft\n\ntype Engine struct{ Replica string }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{Operations: []applyOp{
		{Op: "edit", Name: "Engine", NewBody: "type Engine struct {\n\tReplica string\n\tTransport string\n}"},
	}})
	text := resultText(t, result)
	if strings.Contains(text, "~ edited Engine") {
		t.Fatalf("expected the ambiguous edit to be refused, not silently applied, got: %s", text)
	}

	bftSrc, _ := os.ReadFile(filepath.Join(projDir, "bft", "engine.go"))
	if strings.Contains(string(bftSrc), "Transport") {
		t.Errorf("bft's Engine should NOT have been touched, got:\n%s", bftSrc)
	}
	chessSrc, _ := os.ReadFile(filepath.Join(projDir, "chess", "engine.go"))
	if strings.Contains(string(chessSrc), "Transport") || strings.Contains(string(chessSrc), "Replica") {
		t.Errorf("chess's Engine should NOT have been corrupted, got:\n%s", chessSrc)
	}
}

// TestHandleEdit_RefusesAmbiguousBareNameAcrossModules is the #248
// regression: a bare-name edit (no receiver/module/file) used to
// silently resolve via GetDefinitionByName's blast-radius tiebreak and
// could write into the WRONG same-named definition in an unrelated
// package. Live-reproduced this session: code(op:"edit", name:"Backend")
// with no module: clobbered internal/summary's small Backend interface
// with a copy of internal/store's much larger one. Same bft/chess
// Engine fixture as TestHandleEdit_ModuleDisambiguatesSameNamedType,
// but this time WITHOUT the disambiguating module: -- must refuse, not
// guess.
func TestHandleEdit_RefusesAmbiguousBareNameAcrossModules(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "bft"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "chess"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte("package bft\n\ntype Engine struct{ Replica string }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleEdit(context.Background(), nil, editParam{
		Name:    "Engine",
		NewBody: "type Engine struct {\n\tReplica string\n\tTransport string\n}",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "ambiguous") {
		t.Fatalf("expected an ambiguity refusal, got: %s", text)
	}

	bftSrc, _ := os.ReadFile(filepath.Join(projDir, "bft", "engine.go"))
	if strings.Contains(string(bftSrc), "Transport") {
		t.Errorf("bft's Engine should NOT have been touched by a refused ambiguous edit, got:\n%s", bftSrc)
	}
	chessSrc, _ := os.ReadFile(filepath.Join(projDir, "chess", "engine.go"))
	if strings.Contains(string(chessSrc), "Transport") || strings.Contains(string(chessSrc), "Replica") {
		t.Errorf("chess's Engine should NOT have been corrupted by a refused ambiguous edit, got:\n%s", chessSrc)
	}
}

// TestHandleRename_ModuleDisambiguatesSameNamedType is the positive
// control for TestHandleRename_RefusesAmbiguousBareNameAcrossModules:
// renameParam now carries receiver/module/file, so the same ambiguous
// fixture must succeed and target only the intended def when module:
// is given.
func TestHandleRename_ModuleDisambiguatesSameNamedType(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "bft"), 0755)
	os.MkdirAll(filepath.Join(projDir, "chess"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte("package bft\n\ntype Engine struct{ Replica string }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleRename(context.Background(), nil, renameParam{OldName: "Engine", NewName: "EngineV2", Module: "testproj/bft"})
	if err != nil {
		t.Fatalf("handleRename: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "ambiguous") {
		t.Fatalf("module: should have disambiguated, got: %s", text)
	}

	bftSrc, _ := os.ReadFile(filepath.Join(projDir, "bft", "engine.go"))
	if !strings.Contains(string(bftSrc), "EngineV2") {
		t.Errorf("expected bft's Engine to be renamed to EngineV2, got:\n%s", bftSrc)
	}
	chessSrc, _ := os.ReadFile(filepath.Join(projDir, "chess", "engine.go"))
	if !strings.Contains(string(chessSrc), "type Engine struct") || strings.Contains(string(chessSrc), "EngineV2") {
		t.Errorf("chess's Engine should be untouched by a module-scoped rename, got:\n%s", chessSrc)
	}
}

// TestHandleRename_RefusesAmbiguousBareNameAcrossModules is the #248
// regression for rename: renameParam previously had no
// receiver/module/file fields at all, and handleRename resolved
// old_name via a raw GetDefinitionByName(name, "") call -- the worst
// case of the ambiguous-write bug, since a caller couldn't disambiguate
// even if it wanted to.
func TestHandleRename_RefusesAmbiguousBareNameAcrossModules(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "bft"), 0755)
	os.MkdirAll(filepath.Join(projDir, "chess"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte("package bft\n\ntype Engine struct{ Replica string }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{OldName: "Engine", NewName: "EngineV2"})
	text := resultText(t, result)
	if !strings.Contains(text, "ambiguous") {
		t.Fatalf("expected an ambiguity refusal, got: %s", text)
	}

	bftSrc, _ := os.ReadFile(filepath.Join(projDir, "bft", "engine.go"))
	if !strings.Contains(string(bftSrc), "type Engine struct") || strings.Contains(string(bftSrc), "EngineV2") {
		t.Errorf("bft's Engine should be untouched, got:\n%s", bftSrc)
	}
	chessSrc, _ := os.ReadFile(filepath.Join(projDir, "chess", "engine.go"))
	if !strings.Contains(string(chessSrc), "type Engine struct") || strings.Contains(string(chessSrc), "EngineV2") {
		t.Errorf("chess's Engine should be untouched, got:\n%s", chessSrc)
	}
}

// TestBodyScanResult_FileScopesHits is the #248 regression: stage-3
// body-scan never had a file parameter, so search(file:X) silently
// lost its scoping whenever stages 1-2 (name-LIKE, FTS) came up empty
// and fell through to bodyScanResult -- a regression-adjacent variant
// of the already-fixed #241 file: bug, just in the third search stage.
func TestBodyScanResult_FileScopesHits(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	os.MkdirAll(filepath.Join(projDir, "sub"), 0755)
	subPath := filepath.Join(projDir, "sub", "sub.go")
	os.WriteFile(subPath, []byte("package sub\n\nfunc SubGreet() string {\n\treturn \"Hello, sub\"\n}\n"), 0644)
	if _, err := ingest.IngestFile(db, projDir, subPath); err != nil {
		t.Fatal("ingest sub.go:", err)
	}

	s := &server{backend: db}
	result, _, err := s.bodyScanResult("Hello, ", 100, "sub")
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, `"name":"SubGreet"`) {
		t.Errorf("expected file:\"sub\" scoped body-scan to find SubGreet, got: %s", text)
	}
	if strings.Contains(text, `"name":"Greet"`) {
		t.Errorf("expected file:\"sub\" to exclude main.go's Greet, got: %s", text)
	}
}

// TestHandleCode_AmbiguityNoteOnBareNameReadAndOutline is the #248
// read-side disclosure fix: a bare-name read/outline that resolves
// via GetDefinitionByName's best-effort tiebreak now says so, instead
// of silently returning one of several same-named candidates with no
// indication another exists (unlike search, which lists every match).
func TestHandleCode_AmbiguityNoteOnBareNameReadAndOutline(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "bft"), 0755)
	os.MkdirAll(filepath.Join(projDir, "chess"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte("package bft\n\ntype Engine struct{ Replica string }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	for _, op := range []string{"read", "outline"} {
		t.Run(op, func(t *testing.T) {
			result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: op, Name: "Engine"})
			if err != nil {
				t.Fatalf("handleCode %s: %v", op, err)
			}
			text := resultText(t, result)
			if !strings.Contains(text, "2 definitions share the name") {
				t.Errorf("expected an ambiguity note, got:\n%s", text)
			}
		})
	}
}

// TestHandleCode_SearchQueryAliasesToPattern is the #248 regression:
// query: is a real codeParam field, but search only ever read
// pattern:, so search(query:"X") silently matched nearly everything
// (empty pattern -> "%%") and returned a caller-count-ranked list that
// looked like a real, relevant result.
func TestHandleCode_SearchQueryAliasesToPattern(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "search", Query: "Greet"})
	if err != nil {
		t.Fatalf("handleCode search via query: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, `"name":"Greet"`) {
		t.Errorf("expected query: to alias to pattern: and find Greet, got: %s", text)
	}
}

// TestHandleGetDefinition_SummaryModeSkipsStubPlaceholder is the #248
// regression, now exercised via explicit mode:"summary" (#313 made
// summary opt-in, no longer a silent default): a Stub-backend
// placeholder ("TODO: <Name>") isn't a real summary, and must still
// fall back to the full body rather than being surfaced as if it
// were a genuine intent line.
func TestHandleGetDefinition_SummaryModeSkipsStubPlaceholder(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	d, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("setup: Greet not found: %v", err)
	}
	if err := db.SetDefSummary(d.ID, &store.DefSummary{
		OneLine:  "TODO: Greet",
		BodyHash: store.HashBodyStructural(d.Body),
		Model:    "stub",
	}); err != nil {
		t.Fatalf("SetDefSummary: %v", err)
	}

	// #313: mode:"summary" is now explicit opt-in (no longer a silent
	// default) -- pass it directly to exercise the #248 stub-skip path.
	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet", Mode: "summary"})
	text := resultText(t, result)
	if strings.Contains(text, "TODO: Greet") {
		t.Errorf("expected the stub placeholder NOT to be surfaced anywhere, got:\n%s", text)
	}
	if !strings.Contains(text, "return \"Hello, \"") {
		t.Errorf("expected fallback to the full body when only a stub summary exists, got:\n%s", text)
	}
}

// TestHandleOverview_CapsLargeDirectory is the #248 regression:
// directory/module-scoped overview had no cap, unlike file-defs'
// fileDefsCap -- a real trajectory hit a hard "exceeds maximum
// allowed tokens" failure on a single overview call against a large
// package.
func TestHandleOverview_CapsLargeDirectory(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	var sb strings.Builder
	sb.WriteString("package main\n\n")
	const n = 60
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "func BigOv%d() {}\n", i)
	}
	bigPath := filepath.Join(projDir, "bigoverview.go")
	if err := os.WriteFile(bigPath, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.IngestFile(db, projDir, bigPath); err != nil {
		t.Fatal("ingest bigoverview.go:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, err := s.handleOverview(context.Background(), nil, codeParam{File: "bigoverview.go"})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("showing %d of %d definitions", overviewDefsCap, n)) {
		t.Errorf("expected a cap/truncation message, got:\n%s", text)
	}
	if strings.Count(text, "BigOv") > overviewDefsCap {
		t.Errorf("expected at most %d defs listed, got more:\n%s", overviewDefsCap, text)
	}
}

// TestHandleTestByName_ScopesRootPackageTest is the #248 regression
// for testScopeTarget's root-package edge case: a hint resolving to
// the module's root package (dir==".") used to leave target at the
// "./..." default instead of narrowing to ".", silently re-flooding
// the exact whole-repo output #241 scoped this op to avoid.
func TestHandleTestByName_ScopesRootPackageTest(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "sub"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc RootFunc() string { return \"root\" }\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestRootFunc(t *testing.T) {\n\tif RootFunc() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "sub", "sub.go"), []byte("package sub\n\nfunc SubFunc() string { return \"sub\" }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "sub", "sub_test.go"), []byte("package sub\n\nimport \"testing\"\n\nfunc TestSubFunc(t *testing.T) {\n\tif SubFunc() == \"\" {\n\t\tt.Fatal(\"sub-package-marker\")\n\t}\n}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleTestByName(context.Background(), nil, "TestRootFunc", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "across .:") {
		t.Errorf("expected root-package test to scope to \".\", got:\n%s", text)
	}
	if strings.Contains(text, "sub-package-marker") || strings.Contains(text, "TestSubFunc") {
		t.Errorf("expected sub package NOT to have run, got:\n%s", text)
	}
	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("expected the root test to pass, got:\n%s", text)
	}
}

// TestHandleTest_ScopesToDefinitionsPackage is the #248 regression for
// handleTest (the name:-based coverage-run path): it previously ran
// `go test ./...` unconditionally, unlike its sibling handleTestByName
// which already got #241's package scoping. Verifies both the
// root-package and subpackage cases now scope correctly.
func TestHandleTest_ScopesToDefinitionsPackage(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "sub"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc RootFunc() string { return \"root\" }\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestRootFunc(t *testing.T) {\n\tif RootFunc() == \"\" {\n\t\tt.Fatal(\"root-package-marker\")\n\t}\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "sub", "sub.go"), []byte("package sub\n\nfunc SubFunc() string { return \"sub\" }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "sub", "sub_test.go"), []byte("package sub\n\nimport \"testing\"\n\nfunc TestSubFunc(t *testing.T) {\n\tif SubFunc() == \"\" {\n\t\tt.Fatal(\"sub-package-marker\")\n\t}\n}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	rootResult, _, err := s.handleTest(context.Background(), nil, nameParam{Name: "RootFunc"})
	if err != nil {
		t.Fatalf("handleTest(RootFunc): %v", err)
	}
	rootText := resultText(t, rootResult)
	if !strings.Contains(rootText, "across .:") {
		t.Errorf("expected RootFunc's coverage run to scope to \".\", got:\n%s", rootText)
	}
	if strings.Contains(rootText, "sub-package-marker") {
		t.Errorf("expected sub package NOT to have run for RootFunc, got:\n%s", rootText)
	}

	subResult, _, err := s.handleTest(context.Background(), nil, nameParam{Name: "SubFunc"})
	if err != nil {
		t.Fatalf("handleTest(SubFunc): %v", err)
	}
	subText := resultText(t, subResult)
	if !strings.Contains(subText, "across ./sub/...:") {
		t.Errorf("expected SubFunc's coverage run to scope to ./sub/..., got:\n%s", subText)
	}
	if strings.Contains(subText, "root-package-marker") {
		t.Errorf("expected root package NOT to have run for SubFunc, got:\n%s", subText)
	}
}

// TestProjectOverview_NoModulesMessageMentionsSync is the #248
// regression for the ingest-hint message: an MCP-only agent (no shell
// access) can't act on "run defn ingest ." -- the message must point
// at code(op:"sync"), the actual remedy available through the same
// tool.
func TestProjectOverview_NoModulesMessageMentionsSync(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := &server{backend: db}
	result, _, err := s.projectOverview(context.Background())
	if err != nil {
		t.Fatalf("projectOverview: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, `code(op:"sync")`) {
		t.Errorf("expected the no-modules message to point at code(op:\"sync\"), got: %s", text)
	}
}

// TestHandleTestByName_ReportsNoTestsMatchedInsteadOfFalsePass guards
// against a false-positive "ALL TESTS PASSED" when a -run pattern
// matches zero tests. go test exits 0 in that case (just warns "no
// tests to run"), which used to be indistinguishable from a real pass
// -- a real trajectory targeted a grpctest-suite method by its bare
// name (addressed by go test as a subtest, Test/TestFoo, not TestFoo),
// silently matched nothing, and the agent moved on with false
// confidence that nothing needed further investigation.
func TestHandleTestByName_ReportsNoTestsMatchedInsteadOfFalsePass(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha.go"), []byte(`package alpha

func Widget() bool { return true }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha_test.go"), []byte(`package alpha

import "testing"

func TestWidget(t *testing.T) {
	if !Widget() {
		t.Fatal("false")
	}
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.handleTestByName(context.Background(), nil, "TestDoesNotExist", "", "alpha/alpha.go")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("a pattern matching zero tests must NOT report ALL TESTS PASSED (that's a false positive -- nothing was verified), got: %s", text)
	}
	if !strings.Contains(text, "NO TESTS MATCHED") {
		t.Errorf("expected an explicit NO TESTS MATCHED signal, got: %s", text)
	}
}

// TestHandleApply_CreateSingleDeclRefusesNameCollision guards handleApply's
// single-declaration "create" branch (the common case -- most create ops
// in a batch are single declarations), which used to skip the same
// same-module name+receiver collision check handleCreate and the
// multi-decl branch both have. Without it, a collision only surfaced as
// a raw Go compiler error ("X redeclared in this block") at the build
// stage, rolling back the whole batch instead of defn's own clear
// "already exists" message.
func TestHandleApply_CreateSingleDeclRefusesNameCollision(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "create", Body: "func Greet(name string) string { return \"Hi \" + name }", File: "other.go"},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "already exists") {
		t.Fatalf("expected an 'already exists' collision message instead of a raw build-stage failure, got: %s", text)
	}

	if _, err := os.Stat(filepath.Join(projDir, "other.go")); !os.IsNotExist(err) {
		t.Errorf("expected other.go to never be written since the collision should be caught before the build stage, got err=%v", err)
	}
}

// TestHandleExpand_AmbiguityNoteOnBareName is the expand-path sibling of
// TestHandleCode_AmbiguityNoteOnBareNameReadAndOutline. The circuit
// breaker (turn_state.go) silently redirects blocked bare-name
// read/outline calls through handleExpand, which resolves via the same
// best-effort tiebreak as handleCode's direct "read"/"outline" cases --
// but until this fix, expand never disclosed it, so an ambiguous name
// auto-batched through expand resolved silently with no indication
// another same-named candidate existed.
func TestHandleExpand_AmbiguityNoteOnBareName(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "bft"), 0755)
	os.MkdirAll(filepath.Join(projDir, "chess"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte("package bft\n\ntype Engine struct{ Replica string }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{Name: "Engine"})
	if err != nil {
		t.Fatalf("handleExpand: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "2 definitions share the name") {
		t.Errorf("expected an ambiguity note from expand, got:\n%s", text)
	}
}

// TestHandleSearch_IncludeParamNotedAsIgnored guards the #250 fix:
// search accepted include: (a real codeParam field, but only wired up
// for expand's graph-hop selection) with zero effect and zero signal --
// a real go-zero-1964 trajectory tried include:["rest"] expecting
// package scoping (by analogy with expand), got unfiltered repo-wide
// results twice, and gave up on search entirely.
func TestHandleSearch_IncludeParamNotedAsIgnored(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.handleSearch(context.Background(), nil, codeParam{Pattern: "Greet", Include: []string{"rest"}})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "include") || !strings.Contains(text, "ignored") {
		t.Errorf("expected a note that include: has no effect on search, got: %s", text)
	}
}

// TestTestBuildFailed guards the #250 fix: handleTest/handleTestByName
// used to report the same generic "SOME TESTS FAILED" for a genuine
// test failure and for a compile/vet error that ran zero tests -- a
// real cli-3997 trajectory hit exactly this with a pre-existing vet
// error, indistinguishable from the agent's own edit breaking a test.
func TestTestBuildFailed(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"build failed", "# pkg\nfile.go:1:1: undefined: Foo\nFAIL\tpkg [build failed]\n", true},
		{"setup failed", "FAIL\tpkg [setup failed]\n", true},
		{"real test failure", "--- FAIL: TestX (0.00s)\nFAIL\n", false},
		{"pass", "PASS\nok  \tpkg\t0.010s\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := testBuildFailed(c.out); got != c.want {
				t.Errorf("testBuildFailed(%q) = %v, want %v", c.out, got, c.want)
			}
		})
	}
}

// TestHandleFileDefs_LimitOverridesDefaultCap guards the #250 fix:
// file-defs accepted a limit: param but silently ignored it, always
// truncating to the fixed fileDefsCap with no way to ask for more.
func TestHandleFileDefs_LimitOverridesDefaultCap(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	var sb strings.Builder
	sb.WriteString("package main\n\n")
	const n = 10
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "func Small%d() {}\n", i)
	}
	smallPath := filepath.Join(projDir, "small.go")
	if err := os.WriteFile(smallPath, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.IngestFile(db, projDir, smallPath); err != nil {
		t.Fatal("ingest small.go:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, err := s.handleFileDefs(context.Background(), nil, codeParam{File: "small.go", Limit: 3})
	if err != nil {
		t.Fatalf("file-defs: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("3 of %d definitions", n)) {
		t.Errorf("expected limit:3 to cap the result to 3 of %d, got:\n%s", n, text)
	}
	if strings.Count(text, "\"name\":") > 3 {
		t.Errorf("expected at most 3 entries with limit:3, got more:\n%s", text)
	}
}

// TestHandleLiterals_LimitAndFileScopeResults guards the #250 fix:
// literals accepted limit:/file: params but silently ignored both --
// results were always capped at a fixed 200 and never scoped to a
// file, same silent-drop class as #241 (search's file:).
func TestHandleLiterals_LimitAndFileScopeResults(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.MkdirAll(filepath.Join(projDir, "beta"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha.go"), []byte(`package alpha

type Config struct{ Name string }

func New() Config { return Config{Name: "alpha"} }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "beta", "beta.go"), []byte(`package beta

type Config struct{ Name string }

func New() Config { return Config{Name: "beta"} }
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	// Unscoped: both alpha's and beta's Config literal should appear.
	result, _, err := s.handleLiterals(context.Background(), nil, codeParam{Pattern: "Config"})
	if err != nil {
		t.Fatalf("handleLiterals: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "`alpha`") || !strings.Contains(text, "`beta`") {
		t.Fatalf("expected both alpha and beta literal values unscoped, got: %s", text)
	}

	// Scoped to alpha/: only the alpha literal should survive.
	result, _, err = s.handleLiterals(context.Background(), nil, codeParam{Pattern: "Config", File: "alpha"})
	if err != nil {
		t.Fatalf("handleLiterals with file: %v", err)
	}
	text = resultText(t, result)
	if !strings.Contains(text, "`alpha`") {
		t.Errorf("file:\"alpha\" should still include the alpha literal, got: %s", text)
	}
	if strings.Contains(text, "`beta`") {
		t.Errorf("file:\"alpha\" should have excluded the beta literal, got: %s", text)
	}

	// limit:1 must cap the unscoped result to a single row.
	result, _, err = s.handleLiterals(context.Background(), nil, codeParam{Pattern: "Config", Limit: 1})
	if err != nil {
		t.Fatalf("handleLiterals with limit: %v", err)
	}
	text = resultText(t, result)
	if strings.Contains(text, "`alpha`") && strings.Contains(text, "`beta`") {
		t.Errorf("limit:1 should have capped to a single result, got both: %s", text)
	}
}

// TestHandlePragmas_LimitOverridesDefaultCap guards the #250 fix:
// pragmas accepted a limit: param but silently ignored it, always
// truncating to the fixed pragmasCap with no way to ask for fewer or
// more.
func TestHandlePragmas_LimitOverridesDefaultCap(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	var sb strings.Builder
	sb.WriteString("package main\n\n")
	const n = 10
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "//test:pragma\nfunc PragmaTarget%d() {}\n\n", i)
	}
	pragmaPath := filepath.Join(projDir, "pragmas.go")
	if err := os.WriteFile(pragmaPath, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, err := s.handlePragmas(context.Background(), nil, codeParam{Pattern: "test:pragma", Limit: 2})
	if err != nil {
		t.Fatalf("pragmas: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("showing 2 of %d", n)) {
		t.Errorf("expected limit:2 to cap the result to 2, got:\n%s", text)
	}
	if got := strings.Count(text, "`test:pragma`"); got > 2 {
		t.Errorf("expected at most 2 pragma lines with limit:2, got %d:\n%s", got, text)
	}
}

// TestBodyScanResult_PipePatternHintsNotRegex guards the pilot8b fix:
// search(pattern:"A|B|C") reads like regex alternation but search has
// no regex support anywhere in its path (LIKE + FTS + substring, all
// literal) -- a real go-zero-2283 trajectory got a generic "no
// matches" for exactly this shape even though one of the terms
// existed, found trivially by a follow-up single-term search.
func TestBodyScanResult_PipePatternHintsNotRegex(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.bodyScanResult("NoSuchThing|AlsoNotThere", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "not regex") {
		t.Errorf("expected a hint that '|' is literal, not regex alternation, got:\n%s", text)
	}
}

// TestHandleCode_AmbiguityNoteOnOtherReadShapedOps extends the #248
// disclosure fix to every other read-shaped op that resolves a bare
// name via resolveEditTarget's identical best-effort tiebreak but
// (unlike read/outline/expand) never surfaced it: impact, explain,
// similar, slice, test (name-based coverage lookup), test-coverage,
// and traverse. Found via a systematic sweep of resolveEditTarget's
// 11 production callers after the pilot8b digging pass turned up the
// same gap on handleTestByName specifically.
func TestHandleCode_AmbiguityNoteOnOtherReadShapedOps(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "bft"), 0755)
	os.MkdirAll(filepath.Join(projDir, "chess"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte("package bft\n\nfunc Engine() bool { return true }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

func Engine() bool { return false }

func UseEngine() bool { return Engine() }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	cases := []struct {
		op    string
		extra codeParam
	}{
		{op: "impact"},
		{op: "explain"},
		{op: "similar"},
		{op: "slice", extra: codeParam{Slice: "signature"}},
		{op: "test"},
		{op: "test-coverage"},
		{op: "traverse", extra: codeParam{Direction: "callers"}},
	}
	for _, c := range cases {
		t.Run(c.op, func(t *testing.T) {
			args := c.extra
			args.Op = c.op
			args.Name = "Engine"
			result, _, err := s.handleCode(context.Background(), nil, args)
			if err != nil {
				t.Fatalf("handleCode %s: %v", c.op, err)
			}
			text := resultText(t, result)
			if !strings.Contains(text, "2 definitions share the name") {
				t.Errorf("%s: expected an ambiguity note, got:\n%s", c.op, text)
			}
		})
	}
}

// TestHandleExpand_BodyNotReservedWhenAlreadyServedThisSession guards
// the pilot8b fix: renderExpandSection never checked bodyServedEpochsAgo
// before honoring include:["body"], so a name whose full body was
// already read via read(full:true) got it dumped in full again the
// next time it was swept into an expand call (including the circuit
// breaker's own auto-batch redirect) -- pure wasted tokens, confirmed
// via a real grpc-go-3119 trajectory.
func TestHandleExpand_BodyNotReservedWhenAlreadyServedThisSession(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
	s.ready.Store(true)
	req := &sdkmcp.CallToolRequest{Session: &sdkmcp.ServerSession{}}

	if _, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Greet", Full: true}); err != nil {
		t.Fatalf("read full: %v", err)
	}

	result, _, err := s.handleExpand(context.Background(), req, codeParam{Name: "Greet", Include: []string{"outline", "body", "callers"}})
	if err != nil {
		t.Fatalf("handleExpand: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, `"Hello, "`) {
		t.Errorf("expected Greet's body (already served via read(full:true)) to be omitted from expand, got:\n%s", text)
	}
	if !strings.Contains(text, "already read in this session") {
		t.Errorf("expected a note explaining why the body was omitted, got:\n%s", text)
	}
}

// TestHandleTestByName_AmbiguousPatternDisclosesTiebreak guards the
// pilot8b fix: test:"TestX" with no module:/file: hint used to
// silently scope to whichever same-named test's package won
// GetDefinitionByName's best-effort caller-count tiebreak, with zero
// indication another package had a same-named test too -- a real
// cli-5503 trajectory got a false "PASS" from an unrelated sibling
// package's TestNewCmdList/TestListRun and only caught it by noticing
// the printed subtest names looked wrong.
func TestHandleTestByName_AmbiguousPatternDisclosesTiebreak(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.MkdirAll(filepath.Join(projDir, "beta"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha.go"), []byte("package alpha\n\nfunc Widget() bool { return true }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha_test.go"), []byte(`package alpha

import "testing"

func TestDup(t *testing.T) {
	if !Widget() {
		t.Fatal("false")
	}
}
`), 0644)
	os.WriteFile(filepath.Join(projDir, "beta", "beta.go"), []byte("package beta\n\nfunc Gadget() bool { return true }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "beta", "beta_test.go"), []byte(`package beta

import "testing"

func TestDup(t *testing.T) {
	if !Gadget() {
		t.Fatal("false")
	}
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.handleTestByName(context.Background(), nil, "TestDup", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "2 definitions share the name") {
		t.Errorf("expected an ambiguity note disclosing the tiebreak, got:\n%s", text)
	}
	if strings.Contains(text, "./...") {
		t.Errorf("expected scoping to one specific package's own test, not a whole-repo ./... run, got:\n%s", text)
	}
}

// TestHandleTestByName_PanicReportedAsBinaryPanicNotTestFailure is the
// end-to-end version of TestTestPanicked: a real test-binary crash
// (not a normal assertion failure) must be labeled distinctly from
// "SOME TESTS FAILED", the same way a build failure already is.
func TestHandleTestByName_PanicReportedAsBinaryPanicNotTestFailure(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "crash_test.go"), []byte(`package main

import "testing"

func TestCrashes(t *testing.T) {
	panic("boom")
}
`), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.handleTestByName(context.Background(), nil, "TestCrashes", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "TEST BINARY PANICKED") {
		t.Errorf("expected a distinct panic label, got:\n%s", text)
	}
	if strings.Contains(text, "SOME TESTS FAILED") {
		t.Errorf("a binary panic must not also be labeled as a generic test failure:\n%s", text)
	}
}

// TestTestPanicked guards the pilot8b fix: a test binary crash (e.g. a
// duplicate flag/command registration panic shared across an entire
// package's tests) used to get the same generic "SOME TESTS FAILED"
// label as a genuine assertion failure -- confirmed via a real cli-405
// trajectory (panic: create flag redefined: draft) that invited
// suspicion of an unrelated, correct edit.
func TestTestPanicked(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"real panic with trace", "panic: create flag redefined: draft\n\ngoroutine 1 [running]:\nmain.main()\n", true},
		{"build failed", "# pkg\nfile.go:1:1: undefined: Foo\nFAIL\tpkg [build failed]\n", false},
		{"real test failure", "--- FAIL: TestX (0.00s)\nFAIL\n", false},
		{"pass", "PASS\nok  \tpkg\t0.010s\n", false},
		{"mentions panic without a trace", "--- FAIL: TestX\n    x_test.go:10: expected panic recovery, got nil\nFAIL\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := testPanicked(c.out); got != c.want {
				t.Errorf("testPanicked(%q) = %v, want %v", c.out, got, c.want)
			}
		})
	}
}

// TestHandleCode_MethodsFileParamThreadedThroughDispatch guards the
// dispatch-layer half of the same fix: handleCode's "methods" case used
// to silently drop file:/module: before ever calling handleMethods --
// passing them had zero effect. Confirmed end-to-end through
// handleCode, not just the handler directly.
func TestHandleCode_MethodsFileParamThreadedThroughDispatch(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}
	s.ready.Store(true)

	modA, err := db.EnsureModule("example.com/a", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	modB, err := db.EnsureModule("example.com/b", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(mod *store.Module, file, methodName string) {
		typeDef := &store.Definition{ModuleID: mod.ID, Name: "Engine", Kind: "type", SourceFile: file, Body: "type Engine struct{}"}
		typeDef.Hash = store.HashBody(typeDef.Body)
		if _, err := db.UpsertDefinition(typeDef); err != nil {
			t.Fatal(err)
		}
		m := &store.Definition{ModuleID: mod.ID, Name: methodName, Kind: "method", Receiver: "*Engine",
			SourceFile: file, Body: "func (e *Engine) " + methodName + "() {}", Signature: "func (e *Engine) " + methodName + "()"}
		m.Hash = store.HashBody(m.Body)
		if _, err := db.UpsertDefinition(m); err != nil {
			t.Fatal(err)
		}
	}
	seed(modA, "a/engine.go", "Drive")
	seed(modB, "b/engine.go", "Fly")

	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "methods", Name: "Engine", File: "b/engine.go"})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Fly") || strings.Contains(text, "Drive") {
		t.Errorf("expected file: to actually scope through handleCode's dispatch, got:\n%s", text)
	}
}

// TestHandleMethods_SameNamedTypesAcrossFilesGetMergeWarning guards the
// pilot8b sweep fix: methods were collected by receiver-type-NAME alone
// with no module/file scoping, so two completely unrelated types
// sharing a name in different packages had their method sets silently
// merged into one list -- indistinguishable from a genuine single
// type's method set. An unscoped call now warns; file: now actually
// scopes to one.
func TestHandleMethods_SameNamedTypesAcrossFilesGetMergeWarning(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	modA, err := db.EnsureModule("example.com/a", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	modB, err := db.EnsureModule("example.com/b", "b", "")
	if err != nil {
		t.Fatal(err)
	}

	seed := func(mod *store.Module, file, methodName string) {
		typeDef := &store.Definition{ModuleID: mod.ID, Name: "Engine", Kind: "type", SourceFile: file, Body: "type Engine struct{}"}
		typeDef.Hash = store.HashBody(typeDef.Body)
		if _, err := db.UpsertDefinition(typeDef); err != nil {
			t.Fatal(err)
		}
		m := &store.Definition{ModuleID: mod.ID, Name: methodName, Kind: "method", Receiver: "*Engine",
			SourceFile: file, Body: "func (e *Engine) " + methodName + "() {}", Signature: "func (e *Engine) " + methodName + "()"}
		m.Hash = store.HashBody(m.Body)
		if _, err := db.UpsertDefinition(m); err != nil {
			t.Fatal(err)
		}
	}
	seed(modA, "a/engine.go", "Drive")
	seed(modB, "b/engine.go", "Fly")

	// Unscoped: both methods merged, with a warning disclosing it.
	result, _, err := s.handleMethods(context.Background(), nil, nameParam{Name: "Engine"})
	if err != nil {
		t.Fatalf("handleMethods: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Drive") || !strings.Contains(text, "Fly") {
		t.Fatalf("expected both unrelated types' methods merged (documenting the pre-fix behavior), got:\n%s", text)
	}
	if !strings.Contains(text, "UNRELATED same-named types") {
		t.Errorf("expected a merge warning when methods came from >1 file with no scoping, got:\n%s", text)
	}

	// Scoped via file: -- only that file's method, no warning.
	scoped, _, err := s.handleMethods(context.Background(), nil, nameParam{Name: "Engine", File: "a/engine.go"})
	if err != nil {
		t.Fatalf("handleMethods scoped: %v", err)
	}
	scopedText := resultText(t, scoped)
	if !strings.Contains(scopedText, "Drive") || strings.Contains(scopedText, "Fly") {
		t.Errorf("file: should scope to just that file's methods, got:\n%s", scopedText)
	}
	if strings.Contains(scopedText, "UNRELATED same-named types") {
		t.Errorf("a file:-scoped call should not warn about merging, got:\n%s", scopedText)
	}
}

// TestCoupledChangeHint_GrammaticallyCorrectMessage guards the stale8
// fix: the template combined with pluralizeCallers's output produced
// duplicated/broken grammar -- "Tip: a direct caller has a direct
// caller (Foo)" for n==1, "Tip: direct callers has a direct caller
// (Foo, Bar)" for n>1 (also subject/verb disagreement). Verified
// directly against the pre-fix source before fixing.
func TestCoupledChangeHint_GrammaticallyCorrectMessage(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// setupTestDB's fixture has a real caller relationship: Farewell
	// calls Greet, populated via real ingest+resolve.
	d, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}
	hint := s.coupledChangeHint(d.ID)
	if hint == "" {
		t.Fatal("expected a non-empty hint since Farewell calls Greet")
	}
	if !strings.Contains(hint, "Farewell") {
		t.Errorf("expected Farewell named as the caller, got: %q", hint)
	}
	if strings.Contains(hint, "caller has a direct caller") {
		t.Errorf("regression: duplicated 'has a direct caller' phrase, got: %q", hint)
	}
	if strings.Contains(hint, "callers has a direct caller") {
		t.Errorf("regression: subject/verb disagreement, got: %q", hint)
	}
	if !strings.Contains(hint, "this def has") {
		t.Errorf("expected the fixed template's 'this def has' phrasing, got: %q", hint)
	}
}

// TestHandleAddImport_NoDefinitionsFoundSuggestsRecovery is
// TestHandleOverview_NoDefinitionsFoundSuggestsRecovery's counterpart
// for add-import's own zero-match error, found in the same sweep.
func TestHandleAddImport_NoDefinitionsFoundSuggestsRecovery(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleAddImport(context.Background(), nil, codeParam{File: "nonexistent/path.go", ImportPath: "fmt"})
	if err != nil {
		t.Fatalf("handleAddImport: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "search") && !strings.Contains(text, "overview") {
		t.Errorf("expected a recovery hint pointing at search/overview, got:\n%s", text)
	}
}

// TestHandleFileDefs_EmptyFileReturnsEmptyArrayNotNull guards the sweep
// fix: `var results []defSummary` stayed nil for a file with zero
// top-level declarations (e.g. a doc.go with just a package clause),
// so the response read "0 definitions in doc.go:\n\nnull" instead of
// an empty array.
func TestHandleFileDefs_EmptyFileReturnsEmptyArrayNotNull(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "doc.go"), []byte("// Package testproj does nothing interesting.\npackage testproj\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package testproj\n\nfunc main() {}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}

	s := &server{backend: db}
	result, _, err := s.handleFileDefs(context.Background(), nil, codeParam{File: "doc.go"})
	if err != nil {
		t.Fatalf("handleFileDefs: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "null") {
		t.Errorf("regression: bare 'null' embedded in the response, got:\n%s", text)
	}
	if !strings.Contains(text, "0 definitions in doc.go") {
		t.Errorf("expected the zero-count header, got:\n%s", text)
	}
}

// TestHandleFind_NoDefinitionsFoundSuggestsRecovery is
// TestHandleOverview_NoDefinitionsFoundSuggestsRecovery's counterpart
// for find's own zero-match error, found in the same sweep.
func TestHandleFind_NoDefinitionsFoundSuggestsRecovery(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleFind(context.Background(), nil, findParam{File: "nonexistent/path.go"})
	if err != nil {
		t.Fatalf("handleFind: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "search") && !strings.Contains(text, "overview") {
		t.Errorf("expected a recovery hint pointing at search/overview, got:\n%s", text)
	}
}

// TestHandleOverview_NoDefinitionsFoundSuggestsRecovery guards the
// stale8 sweep fix: overview's zero-match error was the one dead-end
// message in the file with no recovery hint, unlike every sibling
// zero-match path (search suggests overview; read/outline's
// file-without-name error suggests overview too).
func TestHandleOverview_NoDefinitionsFoundSuggestsRecovery(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}
	s.ready.Store(true)

	result, _, err := s.handleOverview(context.Background(), nil, codeParam{File: "nonexistent/path.go"})
	if err != nil {
		t.Fatalf("handleOverview: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "search") && !strings.Contains(text, "overview") {
		t.Errorf("expected a recovery hint pointing at search/overview, got:\n%s", text)
	}
}

// TestHandleQuery_ZeroRowsReturnsEmptyArrayNotNull guards the sweep
// fix: internal/store's Query left `var results []map[string]any` nil
// when a SELECT matched zero rows, so a perfectly normal empty result
// set JSON-marshaled to a bare top-level "null" instead of "[]" --
// indistinguishable from a crash.
func TestHandleQuery_ZeroRowsReturnsEmptyArrayNotNull(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleQuery(context.Background(), nil, sqlParam{SQL: "SELECT id FROM definitions WHERE id = -1"})
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	text := resultText(t, result)
	if strings.TrimSpace(text) == "null" {
		t.Errorf("regression: bare 'null' response for a zero-row query, got:\n%s", text)
	}
	if !strings.Contains(text, "[]") {
		t.Errorf("expected an empty JSON array, got:\n%s", text)
	}
}

// TestHandleSearch_DottedQualifiedNameRetriesWithBareSymbol guards the
// stale8 fix: search never applied Go's own "pkg.Symbol" convention
// that read/outline/edit already resolve via resolveDottedQualifiedName
// -- a real zero-1907 trajectory got "no matches" for
// search(pattern:"zrpc.WithUnaryClientInterceptor") even though the
// bare symbol existed and was found trivially on the next call.
func TestHandleSearch_DottedQualifiedNameRetriesWithBareSymbol(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleSearch(context.Background(), nil, codeParam{Pattern: "somepkg.Greet"})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Greet") {
		t.Errorf("expected the qualified-name retry to find Greet via its bare symbol, got:\n%s", text)
	}
	if !strings.Contains(text, "retried with the bare symbol") {
		t.Errorf("expected a note disclosing the retry, got:\n%s", text)
	}
}

// TestHandleSearch_MidStringWildcardNoMatchReturnsEmptyArrayNotNull
// guards the stale8 fix: a pattern with '%' in the MIDDLE (e.g. a
// printf-style path like "runs/%d/jobs") skipped the Stage-3 no-match
// fallback entirely (strings.Trim only strips edges) and fell through
// to toJSON(nil-slice), producing a bare top-level "null" instead of
// an explanatory no-match response. Reproduced live against the
// pre-fix build; this locks in the fix.
func TestHandleSearch_MidStringWildcardNoMatchReturnsEmptyArrayNotNull(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleSearch(context.Background(), nil, codeParam{Pattern: "NoSuch%DefinitionXYZ123"})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	text := resultText(t, result)
	if strings.TrimSpace(text) == "null" {
		t.Errorf("regression: bare 'null' response for a mid-string wildcard with no matches")
	}
}

// TestHandleSearch_ResultsIncludeSourceFile guards the stale8 fix:
// search's summary struct never included the definition's file
// location, unlike read/outline/impact -- forcing a caller who wanted
// to jump to `overview file:"<the file>"` to guess the path, which
// directly caused a wasted round-trip in a real grpc-go-3476
// trajectory.
func TestHandleSearch_ResultsIncludeSourceFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleSearch(context.Background(), nil, codeParam{Pattern: "Greet"})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, `"file"`) {
		t.Errorf("expected search results to include the source file, got:\n%s", text)
	}
	if !strings.Contains(text, "main.go") {
		t.Errorf("expected the actual file path in results, got:\n%s", text)
	}
}

// TestHandleTestCoverage_ZeroTestsReturnsEmptyArrayNotNull guards the
// sweep fix: `var tests []testInfo` stayed nil for a def with zero
// covering tests, so the JSON response read `"tests": null` instead
// of `"tests": []` -- wrong shape for any consumer expecting an array.
func TestHandleTestCoverage_ZeroTestsReturnsEmptyArrayNotNull(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}

	// main() in setupTestDB's fixture is called by nothing and covered
	// by no TestX function -- zero covering tests, unlike Greet/Farewell.
	result, _, err := s.handleTestCoverage(context.Background(), nil, codeParam{Name: "main"})
	if err != nil {
		t.Fatalf("handleTestCoverage: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, `"tests":null`) {
		t.Errorf("regression: tests field is null instead of an empty array, got:\n%s", text)
	}
	if !strings.Contains(text, `"tests":[]`) {
		t.Errorf("expected \"tests\":[] for a def with zero covering tests, got:\n%s", text)
	}
}

// TestRankedSearchResult_ResultsIncludeSourceFile is
// TestHandleSearch_ResultsIncludeSourceFile's counterpart for the
// ranked-results path (search's OTHER result shape, hit when the
// candidate set exceeds limit or rank:true is passed) -- it had the
// identical missing-file-location gap in its own rankedSummary struct.
func TestRankedSearchResult_ResultsIncludeSourceFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	defs, err := db.FindDefinitions("%")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) == 0 {
		t.Fatal("expected at least one definition in the fixture")
	}
	result, _, err := s.rankedSearchResult("Greet", defs, 1)
	if err != nil {
		t.Fatalf("rankedSearchResult: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, `"file"`) {
		t.Errorf("expected ranked search results to include the source file, got:\n%s", text)
	}
}

// TestHandleCode_RemovedDoltOpsGetSpecificAnswerNotUnknownOp guards a real
// trajectory failure (prometheus-18712, 2026-08-10): op:"status" and
// op:"diff" both fell through to the generic "unknown op" default, whose
// own error message listed "status"/"diff" in its "valid:" whitelist while
// rejecting them -- self-contradictory and misleading. Both ops (along
// with the rest of the Dolt-era git-semantics family) were removed in the
// v0.27 SQLite migration and were never coming back; the fix is a
// specific, honest "not supported" answer instead of the generic
// fallthrough.
func TestHandleCode_RemovedDoltOpsGetSpecificAnswerNotUnknownOp(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	for _, op := range []string{"status", "diff", "branch", "checkout", "merge", "commit", "conflicts", "resolve", "merge-abort", "diff-defs", "history"} {
		t.Run(op, func(t *testing.T) {
			result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: op, Name: "x", Body: "x"})
			if err != nil {
				t.Fatalf("handleCode(%q): %v", op, err)
			}
			text := resultText(t, result)
			if strings.Contains(text, "unknown op") {
				t.Errorf("op:%q got the generic unknown-op fallthrough, want a specific not-supported answer:\n%s", op, text)
			}
			if !strings.Contains(text, "not supported") {
				t.Errorf("op:%q expected a \"not supported\" explanation, got:\n%s", op, text)
			}
			// The self-contradiction: an error must not list its own rejected
			// op name inside a "valid:" whitelist.
			if strings.Contains(text, "valid:") && strings.Contains(text, op) {
				idx := strings.Index(text, "valid:")
				if strings.Contains(text[idx:], op) {
					t.Errorf("op:%q self-contradicts: appears in its own \"valid:\" whitelist:\n%s", op, text)
				}
			}
		})
	}
}

// TestHandleCode_StaleWarningAppliesToErrorResultsToo guards a real
// trajectory failure (prometheus-18972, 2026-08-10): wrapStale only
// prepended "[startup ingest in progress]" to non-error results, so
// overview(file:...) during the startup race silently said "no
// definitions found" with no hint the index might just be incomplete --
// the agent had no signal to retry via op:"sync" instead of concluding
// the path was wrong, which cost most of that task's 859s runtime.
// search's "no matches" got the warning (it returns a plain text result,
// not an error) while overview(file:)'s "no definitions found" (an error
// result) didn't -- same underlying stale-index condition, inconsistent
// signaling depending on which op hit it.
func TestHandleCode_StaleWarningAppliesToErrorResultsToo(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: t.TempDir()} // ready left false: simulates startup ingest still running

	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "overview", File: "nonexistent/path.go"})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected an error result for a nonexistent file, got a success")
	}
	text := resultText(t, result)
	if !strings.Contains(text, "startup ingest in progress") {
		t.Errorf("error result during startup race should carry the stale-index warning, got:\n%s", text)
	}
}

// TestHandleImpact_JSONCapsLargeTestList guards a real trajectory failure
// (prometheus-18652, 2026-08-10): impactJSON had no size guard at all,
// unlike the markdown path (capped at impactCallerCap=15 with a "pass
// format:\"json\" for full list" hint). A def with 1,314 covering tests
// produced a 243,019-character/9,473-line response that blew past the
// harness's own tool-result size limit and got redirected to a file the
// agent never successfully paged through -- the JSON "full list" escape
// hatch had no cap of its own to escape TO.
func TestHandleImpact_JSONCapsLargeTestList(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	m, _ := db.EnsureModule("example.com/lib", "lib", "")
	target := &store.Definition{
		ModuleID: m.ID, Name: "T", Kind: "function", Exported: true,
		Body: "func T() {}", Signature: "func T()",
	}
	target.Hash = store.HashBody(target.Body)
	targetID, _ := db.UpsertDefinition(target)

	const n = impactJSONTestsCap + 5
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("TestT_%d", i)
		c := &store.Definition{
			ModuleID: m.ID, Name: name, Kind: "function", Test: true,
			Body: fmt.Sprintf("func %s(t *testing.T) { T() }", name),
		}
		c.Hash = store.HashBody(c.Body)
		id, _ := db.UpsertDefinition(c)
		_ = db.SetReferences(id, []store.Reference{{FromDef: id, ToDef: targetID, Kind: "call"}})
	}

	result, _, err := s.handleImpact(context.Background(), nil, codeParam{Name: "T", Format: "json"})
	if err != nil {
		t.Fatalf("handleImpact: %v", err)
	}
	text := resultText(t, result)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("impactJSON output is not valid JSON: %v\n%s", err, text)
	}

	tests, ok := parsed["tests"].([]any)
	if !ok {
		t.Fatalf("expected \"tests\" array in JSON, got: %v", parsed["tests"])
	}
	if len(tests) != impactJSONTestsCap {
		t.Errorf("expected tests capped at %d, got %d", impactJSONTestsCap, len(tests))
	}
	testsTotal, _ := parsed["tests_total"].(float64)
	if int(testsTotal) != n {
		t.Errorf("expected tests_total=%d (uncapped true count), got %v", n, parsed["tests_total"])
	}
	if _, ok := parsed["truncated"]; !ok {
		t.Errorf("expected a \"truncated\" field when the cap was hit, got none. Full response:\n%s", text)
	}
	if len(text) > 50000 {
		t.Errorf("impactJSON response is %d bytes -- capping didn't bound the actual response size", len(text))
	}
}

// TestHandleBatchImpact_ModuleDisambiguatesSameNamedType guards a real bug
// class found sweeping for explain's #248-style resolution bug (2026-08-10):
// handleBatchImpact received the full codeParam (Module/File/Receiver
// already in scope) but called GetDefinitionByName(name, "") directly for
// each name in the batch, discarding them -- an ambiguous name had no way
// to be disambiguated even though the caller supplied the means to.
func TestHandleBatchImpact_ModuleDisambiguatesSameNamedType(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "bft"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "chess"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte(`package bft

func Engine() {}

func UseBft() { Engine() }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

func Engine() {}

func UseA() { Engine() }
func UseB() { Engine() }
func UseC() { Engine() }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleBatchImpact(context.Background(), nil, codeParam{
		Names:  []string{"Engine"},
		Module: "testproj/bft",
	})
	if err != nil {
		t.Fatalf("handleBatchImpact: %v", err)
	}
	text := resultText(t, result)
	// bft's Engine has 1 direct caller (UseBft); chess's has 3 (UseA/B/C).
	// A wrong resolution to chess's Engine would report combined_callers=3.
	if !strings.Contains(text, `"direct_callers":1`) {
		t.Errorf("module:\"testproj/bft\" should have resolved to bft's Engine (1 caller), got:\n%s", text)
	}
	if strings.Contains(text, `"direct_callers":3`) {
		t.Errorf("batch-impact scoped to module:\"testproj/bft\" returned chess's Engine (3 callers) instead:\n%s", text)
	}
}

// TestHandleValidatePlan_ReceiverDisambiguatesSameNamedMethod guards the
// same #248-class bug found in the 2026-08-10 sweep: Mutation carries
// Receiver, but handleValidatePlan called plain GetDefinitionByName,
// ignoring it -- validating a plan mutation for (*Foo).Bar could silently
// resolve to an unrelated same-named method on a different receiver type.
func TestHandleValidatePlan_ReceiverDisambiguatesSameNamedMethod(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	mod, _ := db.EnsureModule("example.com/lib", "lib", "")
	foo := &store.Definition{ModuleID: mod.ID, Name: "Bar", Kind: "method", Receiver: "*Foo", Body: "func (f *Foo) Bar() {}"}
	foo.Hash = store.HashBody(foo.Body)
	db.UpsertDefinition(foo)
	baz := &store.Definition{ModuleID: mod.ID, Name: "Bar", Kind: "method", Receiver: "*Baz", Body: "func (b *Baz) Bar() {}"}
	baz.Hash = store.HashBody(baz.Body)
	bazID, _ := db.UpsertDefinition(baz)
	// Give Baz.Bar 2 callers, Foo.Bar 0 -- if the receiver is ignored, a
	// blast-radius tiebreak resolves "Bar" to Baz's (more callers), which
	// would silently validate the WRONG mutation.
	for _, callerName := range []string{"CallerA", "CallerB"} {
		c := &store.Definition{ModuleID: mod.ID, Name: callerName, Kind: "function", Body: "func " + callerName + "() { (&Baz{}).Bar() }"}
		c.Hash = store.HashBody(c.Body)
		cID, _ := db.UpsertDefinition(c)
		_ = db.SetReferences(cID, []store.Reference{{FromDef: cID, ToDef: bazID, Kind: "call"}})
	}

	result, _, err := s.handleValidatePlan(context.Background(), nil, codeParam{
		Mutations: []store.Mutation{{Type: "edit", Name: "Bar", Receiver: "*Foo"}},
	})
	if err != nil {
		t.Fatalf("handleValidatePlan: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, `"direct_callers":0`) {
		t.Errorf("receiver:\"*Foo\" should have resolved to Foo's Bar (0 callers), got:\n%s", text)
	}
	if strings.Contains(text, `"direct_callers":2`) {
		t.Errorf("validate-plan for receiver:\"*Foo\" resolved to Baz's Bar (2 callers) instead:\n%s", text)
	}
}

// TestHandleTestByName_ResolvesFullModulePathHint guards a real bug
// found via a prometheus-18534 trajectory (2026-08-10): the agent
// explicitly passed module:"github.com/prometheus/prometheus/promql"
// -- the same full-import-path shape "module:" takes everywhere else
// in this API -- but testScopeTarget only substring-matched against
// repo-relative source_file paths, which a full module path never
// matches. The hint was silently ignored 3 calls in a row, each
// falling back to a whole-repo `go test ./...` that exhausted the
// box's disk compiling every unrelated cloud-SDK dependency.
func TestHandleTestByName_ResolvesFullModulePathHint(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "promql"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "promql", "functions.go"), []byte("package promql\n\nfunc Functions() string { return \"fn\" }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "promql", "functions_test.go"), []byte("package promql\n\nimport \"testing\"\n\nfunc TestFunctions(t *testing.T) {\n\tif Functions() == \"\" {\n\t\tt.Fatal(\"promql-marker\")\n\t}\n}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleTestByName(context.Background(), nil, "TestFunctions", "testproj/promql", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "across ./promql/...:") {
		t.Errorf("expected module: hint to scope to \"./promql/...\", got:\n%s", text)
	}
	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("expected TestFunctions to pass, got:\n%s", text)
	}
}

// TestHandleTestByName_ShortHintPrefersPackageRootOverSubdirectory guards
// a real bug found via a prometheus-19114 trajectory (2026-08-11):
// DistinctSourceFiles has no ORDER BY, so testScopeTarget's first-match
// substring search picked whichever match SQLite happened to return
// first. A short hint like "tsdb" substring-matches files in
// tsdb/encoding/*.go just as well as tsdb/*.go itself, so it could
// resolve to an arbitrary, wrong subdirectory instead of the package
// the hint actually names -- the real trajectory scoped to
// "./tsdb/encoding/..." and never found its target test, living in
// tsdb/db_test.go, burning several dead test calls before giving up.
func TestHandleTestByName_ShortHintPrefersPackageRootOverSubdirectory(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "tsdb", "encoding"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "tsdb", "db.go"), []byte("package tsdb\n\nfunc Open() string { return \"open\" }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "tsdb", "db_test.go"), []byte("package tsdb\n\nimport \"testing\"\n\nfunc TestQuerier(t *testing.T) {\n\tif Open() == \"\" {\n\t\tt.Fatal(\"tsdb-root-marker\")\n\t}\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "tsdb", "encoding", "enc.go"), []byte("package encoding\n\nfunc Encode() string { return \"enc\" }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "tsdb", "encoding", "enc_test.go"), []byte("package encoding\n\nimport \"testing\"\n\nfunc TestEncodingUnrelated(t *testing.T) {\n\tif Encode() == \"\" {\n\t\tt.Fatal(\"encoding-marker\")\n\t}\n}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleTestByName(context.Background(), nil, "TestQuerier", "tsdb", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "across ./tsdb/...:") {
		t.Errorf("expected hint \"tsdb\" to scope to the package root \"./tsdb/...\", not a subdirectory, got:\n%s", text)
	}
	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("expected TestQuerier to be found and pass, got:\n%s", text)
	}
	if strings.Contains(text, "encoding-marker") {
		t.Errorf("test run leaked into the unrelated tsdb/encoding subpackage:\n%s", text)
	}
}

// TestTestMatchedNothing_IgnoresSiblingPackageNoise is the direct unit
// test for the prometheus-19114 false positive: a recursive "./pkg/..."
// scope prints "no tests to run" for every sibling subpackage lacking
// the pattern, even when the target package's test ran and passed.
func TestTestMatchedNothing_IgnoresSiblingPackageNoise(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "real pass alongside a sibling's no-op",
			out: "=== RUN   TestQuerier\n" +
				"--- PASS: TestQuerier (0.00s)\n" +
				"PASS\n" +
				"ok  \ttestproj/tsdb\t0.001s\n" +
				"testing: warning: no tests to run\n" +
				"PASS\n" +
				"ok  \ttestproj/tsdb/encoding\t0.001s [no tests to run]\n",
			want: false,
		},
		{
			name: "real failure alongside a sibling's no-op",
			out: "=== RUN   TestQuerier\n" +
				"--- FAIL: TestQuerier (0.00s)\n" +
				"FAIL\n" +
				"testing: warning: no tests to run\n" +
				"PASS\n" +
				"ok  \ttestproj/tsdb/encoding\t0.001s [no tests to run]\n",
			want: false,
		},
		{
			name: "genuinely zero tests ran anywhere",
			out: "testing: warning: no tests to run\n" +
				"PASS\n" +
				"ok  \ttestproj/tsdb\t0.001s [no tests to run]\n" +
				"testing: warning: no tests to run\n" +
				"PASS\n" +
				"ok  \ttestproj/tsdb/encoding\t0.001s [no tests to run]\n",
			want: true,
		},
		{
			name: "no \"no tests to run\" text at all",
			out:  "=== RUN   TestQuerier\n--- PASS: TestQuerier (0.00s)\nPASS\n",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := testMatchedNothing(tc.out); got != tc.want {
				t.Errorf("testMatchedNothing() = %v, want %v\noutput:\n%s", got, tc.want, tc.out)
			}
		})
	}
}

// TestHandleCode_OpAliasesDispatchToAddImport guards opAliases: common
// near-miss guesses for "add-import" (underscore convention, or the
// bare short name) hit real trajectories repeatedly (prometheus-17395
// tried it twice, plus 18765/18972/18841) and should dispatch exactly
// like the real op instead of round-tripping an "unknown op" error.
func TestHandleCode_OpAliasesDispatchToAddImport(t *testing.T) {
	for _, alias := range []string{"import", "add_import", "addimport", "import_path"} {
		t.Run(alias, func(t *testing.T) {
			db, projDir := setupTestDB(t)
			defer db.Close()
			s := &server{backend: db, projectDir: projDir}
			s.ready.Store(true)

			result, _, err := s.handleCode(context.Background(), nil, codeParam{
				Op:         alias,
				File:       "main.go",
				ImportPath: "hash/fnv",
			})
			if err != nil {
				t.Fatalf("handleCode(op:%q): %v", alias, err)
			}
			text := resultText(t, result)
			if strings.Contains(text, "unknown op") {
				t.Fatalf("op:%q was not recognized as an add-import alias, got: %s", alias, text)
			}
			if !strings.Contains(text, "added import") {
				t.Errorf("expected 'added import' in response for op:%q, got: %s", alias, text)
			}

			final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
			if ferr != nil {
				t.Fatalf("read main.go: %v", ferr)
			}
			if !strings.Contains(string(final), "\"hash/fnv\"") {
				t.Errorf("expected hash/fnv import to land on disk for op:%q, got:\n%s", alias, final)
			}
		})
	}
}

func TestTestTimeoutFor_ScalesForLargeRunsRespectsExplicitOverride(t *testing.T) {
	t.Run("small run keeps the default", func(t *testing.T) {
		got := testTimeoutFor(10, "./tsdb/...")
		if got != testTimeout {
			t.Errorf("got %v, want default %v", got, testTimeout)
		}
	})
	t.Run("many affected tests scales up", func(t *testing.T) {
		got := testTimeoutFor(200, "./tsdb/...")
		if got <= testTimeout {
			t.Errorf("got %v, want > default %v for 200 affected tests", got, testTimeout)
		}
	})
	t.Run("whole-repo scope scales up even with few tests", func(t *testing.T) {
		got := testTimeoutFor(1, "./...")
		if got <= testTimeout {
			t.Errorf("got %v, want > default %v for a whole-repo scope", got, testTimeout)
		}
	})
	t.Run("scaled timeout is capped", func(t *testing.T) {
		got := testTimeoutFor(1_000_000, "./...")
		if got > 5*time.Minute {
			t.Errorf("got %v, want capped at 5m", got)
		}
	})
	t.Run("an operator-set DEFN_TEST_TIMEOUT disables scaling entirely", func(t *testing.T) {
		// testTimeout itself is resolved once at package init, so setting
		// the env var here doesn't change ITS value -- what this actually
		// verifies is that testTimeoutFor treats the env var's presence
		// (checked fresh at call time) as "an operator made an explicit
		// choice, don't second-guess it with scaling", not that it
		// re-parses a new duration on every call.
		t.Setenv("DEFN_TEST_TIMEOUT", "45s")
		got := testTimeoutFor(1_000_000, "./...")
		if got != testTimeout {
			t.Errorf("got %v, want the unscaled package-level testTimeout %v -- an explicit env var must disable scaling, not just cap it", got, testTimeout)
		}
	})
}

// TestHandleReplaceHunk_NotFoundSuggestsWhitespaceNormalizedMatch guards
// suggestClosestFragmentHint: a real trajectory (prometheus-18712)
// burned 17 of 34 replace-hunk calls on bare "hunk not found in body"
// errors with zero diagnostic information, each retry narrowing old to
// a smaller guess rather than converging -- replace-hunk's success
// response never shows the resulting body, so an agent making many
// sequential edits to one large function has no way to see what its
// remembered fragment's whitespace/indentation actually looks like now.
// When old differs from the real body only in whitespace, the error
// should show the exact real bytes to copy instead of a bare miss.
func TestHandleReplaceHunk_NotFoundSuggestsWhitespaceNormalizedMatch(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Greet's real body line is exactly `\treturn "Hello, " + name` --
	// pass a whitespace-mangled version (extra space, no tab) that will
	// never byte-exact match.
	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:   "replace-hunk",
		Name: "Greet",
		Old:  `return  "Hello, " + name`,
		New:  `return "Hi, " + name`,
	})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "hunk not found") {
		t.Fatalf("expected the base 'not found' error to still be present, got: %s", text)
	}
	if !strings.Contains(text, `return "Hello, " + name`) {
		t.Errorf("expected the hint to show the real body text to copy verbatim, got: %s", text)
	}
}

// TestHandleCreate_NewFileInNestedModuleUsesNestedGoMod is a
// regression for the etcd multi-module bench findings: handleCreate's
// "brand new directory" path derived the new module's path via
// emit.DetectModuleRoot(mods) + "/" + dir -- a DB-derived guess at
// the repo's single common module root, wrong whenever the new file
// actually sits inside its own nested go.mod (etcd's server/, tests/,
// etcdctl/ each declare one). The new directory must resolve against
// the real filesystem go.mod nearest it, not the DB's single-module
// assumption.
func TestHandleCreate_NewFileInNestedModuleUsesNestedGoMod(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	nestedDir := filepath.Join(projDir, "sub")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "go.mod"), []byte("module sub.example/v2\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body:   "func NewSubFunc() int { return 1 }",
		File:   "sub/newpkg/y.go",
		Module: "testproj",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created") {
		t.Fatalf("expected 'Created', got: %s", text)
	}

	d, err := db.GetDefinitionByName("NewSubFunc", "")
	if err != nil {
		t.Fatalf("def not found: %v", err)
	}
	mod, err := db.GetModuleByPath("sub.example/v2/newpkg")
	if err != nil || mod == nil {
		t.Fatalf("expected module %q, got module=%v err=%v", "sub.example/v2/newpkg", mod, err)
	}
	if d.ModuleID != mod.ID {
		t.Errorf("NewSubFunc landed in module id=%d, want id=%d (sub.example/v2/newpkg)", d.ModuleID, mod.ID)
	}

	if bogus, err := db.GetModuleByPath("testproj/sub/newpkg"); err == nil && bogus != nil {
		t.Errorf("also created the bogus root-prefixed module %q (id=%d) -- new nested-module directories must resolve against their own go.mod, not the repo root's", "testproj/sub/newpkg", bogus.ID)
	}
}

// TestHandleCreate_NestedModuleAddDoesNotLoseSiblingDefs is a
// regression probe for an etcd bench trajectory (etcd-io__etcd-20342,
// v2 rerun with the ModuleForDir/runScopedBuild fixes already
// applied): the agent successfully created a new function in an
// already-synced nested-module file (server/embed/config.go, 37
// existing defs), the build succeeded, but immediately afterward a
// sibling definition in that SAME file that existed before the
// create (configFromFile) came back "not found" -- and a fresh
// search for it returned zero matches, as if the whole file's defs
// had been dropped from the index. This test reproduces the shape
// with a minimal nested-module fixture to determine whether creating
// a new def in an already-ingested nested-module file corrupts its
// existing sibling defs.
func TestHandleCreate_NestedModuleAddDoesNotLoseSiblingDefs(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	nestedDir := filepath.Join(projDir, "sub")
	if err := os.MkdirAll(filepath.Join(nestedDir, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "go.mod"), []byte("module sub.example/v2\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	xGoPath := filepath.Join(nestedDir, "pkg", "x.go")
	if err := os.WriteFile(xGoPath, []byte("package pkg\n\nfunc FuncA() int { return 1 }\n\nfunc FuncB() int { return 2 }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Mirror what code(op:"sync", file:...) does: register the
	// nested-module file's existing defs before any create touches it.
	if _, err := ingest.IngestFile(db, projDir, xGoPath); err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	if before, err := db.GetDefinitionByName("FuncA", "sub.example/v2/pkg"); err != nil || before == nil {
		t.Fatalf("precondition: FuncA should be findable right after IngestFile, got err=%v", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body:   "func FuncC() int { return 3 }",
		File:   "sub/pkg/x.go",
		Module: "sub.example/v2/pkg",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created") {
		t.Fatalf("expected 'Created', got: %s", text)
	}

	if _, err := db.GetDefinitionByName("FuncA", "sub.example/v2/pkg"); err != nil {
		t.Errorf("FuncA vanished after creating a sibling def in the same nested-module file: %v", err)
	}
	if _, err := db.GetDefinitionByName("FuncB", "sub.example/v2/pkg"); err != nil {
		t.Errorf("FuncB vanished after creating a sibling def in the same nested-module file: %v", err)
	}
}

// TestHandleEdit_SuccessSuggestsExistingUntouchedTestFile is a
// regression for the etcd/traefik/caddy bench investigation: the
// single largest driver of defn's correctness gap vs plain Edit/Write
// was agents fixing the source file correctly, then never touching
// the paired _test.go -- confirmed via real trajectories (e.g.
// caddyserver__caddy-7729) where the agent ran existing tests to
// verify nothing broke but never looked at the test file to add a new
// case. setupTestDB's fixture project has main_test.go alongside
// main.go; editing a non-test def there without touching main_test.go
// should surface a hint pointing at it.
func TestHandleEdit_SuccessSuggestsExistingUntouchedTestFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	newBody := `func Greet(name string) string {
	return "Hi, " + name
}`
	result, _, _ := s.handleEdit(context.Background(), nil, editParam{Name: "Greet", NewBody: newBody})
	text := resultText(t, result)

	if !strings.Contains(text, "main_test.go") {
		t.Errorf("expected a hint pointing at the untouched main_test.go, got: %s", text)
	}
}

// TestHandleEdit_NoTestCoverageHintWhenEditingATestFunctionItself
// checks testCoverageHint's isTest guard: editing a test function
// itself shouldn't suggest touching "a test file" -- that's what's
// already being edited.
func TestHandleEdit_NoTestCoverageHintWhenEditingATestFunctionItself(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	newBody := `func TestGreet(t *testing.T) {
	if Greet("x") == "" {
		t.Fatal("empty")
	}
}`
	result, _, _ := s.handleEdit(context.Background(), nil, editParam{Name: "TestGreet", NewBody: newBody})
	text := resultText(t, result)

	if strings.Contains(text, "Tip: this package has an existing test file") {
		t.Errorf("expected no test-coverage hint when editing a test function itself, got: %s", text)
	}
}

// TestHandleCreate_SuccessSuggestsExistingUntouchedTestFile mirrors
// TestHandleEdit_SuccessSuggestsExistingUntouchedTestFile for the
// create path: setupTestDB's module already has main_test.go, so
// creating a new non-test function without touching it should surface
// the same hint.
func TestHandleCreate_SuccessSuggestsExistingUntouchedTestFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func NewHelper() string { return \"help\" }",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "main_test.go") {
		t.Errorf("expected a hint pointing at the untouched main_test.go, got: %s", text)
	}
}

// TestHandleCreate_NoTestCoverageHintWhenNoModuleGiven checks
// testCoverageHint degrades silently (empty string, not a crash) when
// ListFileSources has nothing to offer -- e.g. module ID 0 for a
// created def whose module lookup came back empty.
func TestHandleCreate_NoTestCoverageHintWhenNoModuleGiven(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	hint := s.testCoverageHint(0, "")
	if hint != "" {
		t.Errorf("expected empty hint for a module with no file sources, got: %q", hint)
	}
}

// TestHandleFragmentEdit_SuccessSuggestsExistingUntouchedTestFile is
// the fragment-edit/replace-hunk-shaped counterpart to
// TestHandleEdit_SuccessSuggestsExistingUntouchedTestFile -- found
// missing via a real confirmatory rerun (caddyserver__caddy-7734):
// the agent used the fragment-edit write path, not handleEdit, so the
// hint wired only into handleEdit never fired.
func TestHandleFragmentEdit_SuccessSuggestsExistingUntouchedTestFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleFragmentEdit(context.Background(), nil, codeParam{
		Name:        "Greet",
		OldFragment: "Hello",
		NewFragment: "Hey",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "main_test.go") {
		t.Errorf("expected a hint pointing at the untouched main_test.go, got: %s", text)
	}
}

// TestHandleReplaceHunk_SuccessSuggestsExistingUntouchedTestFile
// covers applyEditTerse (the shared success path for replace-hunk,
// replace-slice, insert-precondition, wrap-in-defer, rename-param) --
// the single most commonly used write path per CLAUDE.md's own
// guidance, and the one a real confirmatory rerun (caddyserver__caddy-7734)
// showed missing the test-coverage hint entirely.
func TestHandleReplaceHunk_SuccessSuggestsExistingUntouchedTestFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:   "replace-hunk",
		Name: "Greet",
		Old:  `return "Hello, " + name`,
		New:  `return "Hi, " + name`,
	})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "main_test.go") {
		t.Errorf("expected a hint pointing at the untouched main_test.go, got: %s", text)
	}
}

// TestHandleApply_SuccessSuggestsExistingUntouchedTestFile covers the
// apply-batch write path -- a batch that edits a non-test def without
// touching any of the module's existing test files should surface the
// same test-coverage hint as the single-op handlers.
func TestHandleApply_SuccessSuggestsExistingUntouchedTestFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "edit", Name: "Greet", NewBody: `func Greet(name string) string {
	return "Hi, " + name
}`},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "main_test.go") {
		t.Errorf("expected a hint pointing at the untouched main_test.go, got: %s", text)
	}
}

// TestFindModuleByFile_VersionedNestedModuleResolvesViaFilesystem is a
// regression for the etcd bench findings (issue #20342): findModuleByFile
// matched a file's directory against module Paths via string-suffix
// comparison, which silently breaks whenever the module's declared import
// path has a semantic-version segment inserted mid-path -- e.g.
// "example.com/server/v3/embed" does not end with "/server/embed" because
// of the "/v3" in between. A real etcd trajectory hit this on every
// create/edit against server/embed/config.go: findModuleByFile (and
// everything downstream: handleCreate, handleApply, resolveEditTarget,
// resolveApplyTarget) falsely reported the file unresolvable, forcing the
// agent into a cascade of confused, wrong file:/module: retries.
func TestFindModuleByFile_VersionedNestedModuleResolvesViaFilesystem(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	serverDir := filepath.Join(projDir, "server")
	if err := os.MkdirAll(filepath.Join(serverDir, "embed"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "go.mod"), []byte("module example.com/server/v3\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "embed", "config.go"), []byte("package embed\n\nfunc Existing() int { return 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureModule("example.com/server/v3/embed", "embed", ""); err != nil {
		t.Fatal(err)
	}

	s := &server{backend: db, projectDir: projDir}
	mod := s.findModuleByFile("server/embed/config.go")
	if mod == nil {
		t.Fatal("findModuleByFile returned nil for a real file under a versioned nested module")
	}
	if mod.Path != "example.com/server/v3/embed" {
		t.Errorf("got module %q, want %q", mod.Path, "example.com/server/v3/embed")
	}
}

// TestHandleApply_CreateSingleDeclFallsBackToModuleWhenFileUnresolved is a
// regression: handleApply's single-decl "create" case bailed out with
// "does not map to any known module" the instant findModuleByFile(file:)
// returned nil, never trying an explicit module: the caller also passed --
// unlike handleCreate's standalone path, which already fell through to
// module: on a file: miss. A real etcd trajectory (#20342) hit this
// directly: apply's create sub-op passed both a (mistyped) file: and a
// correct module:, and got the same "does not map to any known module"
// error even though the module unambiguously existed and module: alone
// would have resolved it.
func TestHandleApply_CreateSingleDeclFallsBackToModuleWhenFileUnresolved(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	pkgDir := filepath.Join(projDir, "pkg")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "pkg.go"), []byte("package pkg\n\nfunc Existing() int { return 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, err := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{
				Op:     "create",
				File:   "totally/bogus/path/file.go",
				Module: "testproj/pkg",
				Body:   "func NewApplyFallback() int { return 1 }",
			},
		},
	})
	if err != nil {
		t.Fatalf("handleApply error: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "does not map to any known module") {
		t.Fatalf("create should have fallen back to the explicit module: when file: didn't resolve, got: %s", text)
	}
	if !strings.Contains(text, "created NewApplyFallback") {
		t.Fatalf("expected successful create, got: %s", text)
	}
	if _, err := db.GetDefinitionByName("NewApplyFallback", "testproj/pkg"); err != nil {
		t.Fatalf("def not persisted under testproj/pkg: %v", err)
	}
}

// TestHandleOverview_VersionedNestedModuleBareDirectoryResolves is a
// regression for the etcd bench findings (issue #20342, follow-on sweep):
// handleOverview's bare-directory shape (file: with no .go suffix, e.g.
// "server/embed" rather than the full import path) resolved definitions
// via FindDefinitionsByFile's `m.path LIKE %fileSuffix%` fallback, which
// breaks whenever the module's import path has a semantic-version segment
// inserted mid-path -- "go.etcd.io/etcd/server/v3/embed" does not contain
// the contiguous substring "server/embed" because "v3" sits in between.
// overview(file:"server/embed") on such a module silently returned "no
// definitions found" even though the module was fully ingested with dozens
// of real definitions -- the same failure shape as the already-fixed
// findModuleByFile and FindDefinitionsByFile bugs, just reached through
// handleOverview's bare-directory path instead.
func TestHandleOverview_VersionedNestedModuleBareDirectoryResolves(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	serverDir := filepath.Join(projDir, "server")
	if err := os.MkdirAll(filepath.Join(serverDir, "embed"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "go.mod"), []byte("module example.com/server/v3\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "embed", "config.go"), []byte("package embed\n\nfunc Existing() int { return 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mod, err := db.EnsureModule("example.com/server/v3/embed", "embed", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertDefinition(&store.Definition{
		ModuleID:   mod.ID,
		Name:       "Existing",
		Kind:       "function",
		SourceFile: "server/embed/config.go",
		StartLine:  3,
		EndLine:    3,
	}); err != nil {
		t.Fatal(err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, err := s.handleOverview(context.Background(), nil, codeParam{File: "server/embed"})
	if err != nil {
		t.Fatalf("handleOverview error: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "no definitions found") {
		t.Fatalf("overview should have resolved the versioned nested module via the filesystem, got: %s", text)
	}
	if !strings.Contains(text, "Existing") {
		t.Fatalf("expected Existing in overview output, got: %s", text)
	}
}

func TestHandleTestByName_VersionedNestedModuleHintScopesToRealDirectory(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	serverDir := filepath.Join(projDir, "server")
	if err := os.MkdirAll(filepath.Join(serverDir, "embed"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "go.mod"), []byte("module example.com/server/v3\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "embed", "config.go"), []byte("package embed\n\nfunc Widget() string { return \"embed\" }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mod, err := db.EnsureModule("example.com/server/v3/embed", "embed", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertDefinition(&store.Definition{
		ModuleID:   mod.ID,
		Name:       "Widget",
		Kind:       "function",
		Body:       "func Widget() string { return \"embed\" }",
		SourceFile: "server/embed/config.go",
		StartLine:  3,
		EndLine:    3,
	}); err != nil {
		t.Fatal(err)
	}

	s := &server{backend: db, projectDir: projDir}
	got := s.testScopeTarget("example.com/server/v3/embed")
	if got != "./server/embed/..." {
		t.Errorf("testScopeTarget(%q) = %q, want %q (the real on-disk directory, not an import-path reconstruction)",
			"example.com/server/v3/embed", got, "./server/embed/...")
	}
}

// TestHandleTestByName_DoesNotRewriteUnrelatedFiles mirrors
// TestHandleTest_DoesNotRewriteUnrelatedFiles for handleTestByName's own
// separate "ensure files are current" emit call, scoped by module:/file:
// rather than by a resolved def.
func TestHandleTestByName_DoesNotRewriteUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "other"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc RootFunc() string { return \"root\" }\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestRootFunc(t *testing.T) {\n\tif RootFunc() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n"), 0644)
	otherSrc := "package other\n\nimport (\n\t\"strings\"\n\t\"fmt\"\n)\n\nfunc OtherFunc() string { return fmt.Sprintf(\"%s\", strings.ToUpper(\"x\")) }\n"
	otherPath := filepath.Join(projDir, "other", "other.go")
	os.WriteFile(otherPath, []byte(otherSrc), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}

	result, _, err := s.handleTestByName(context.Background(), nil, "TestRootFunc", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	if !strings.Contains(resultText(t, result), "ALL TESTS PASSED") {
		t.Fatalf("expected TestRootFunc to pass, got: %s", resultText(t, result))
	}

	after, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("handleTestByName rewrote an unrelated file it never needed to touch:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestHandleTest_DoesNotRewriteUnrelatedFiles is a regression for a real
// etcd bench trajectory (2026-08-17): handleTest's "ensure files are
// current" step called the fully unscoped emit.Emit, which re-serializes
// and goimports-normalizes EVERY file in the whole project on every
// single code(op:"test") call -- not just the file(s) relevant to what's
// being tested. On a real multi-module repo this silently rewrote the
// import grouping of unrelated generated files nothing about the task
// ever touched, corrupting an otherwise-exact edit's file-touch
// precision. This fixture's "other" package has an on-disk import order
// that doesn't match goimports' canonical grouping (deliberately, to
// detect any full-project goimports pass) -- handleTest for RootFunc
// must leave it untouched, byte for byte.
func TestHandleTest_DoesNotRewriteUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "other"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc RootFunc() string { return \"root\" }\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestRootFunc(t *testing.T) {\n\tif RootFunc() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n"), 0644)
	// Non-canonical import grouping (stdlib imports NOT alphabetized/grouped
	// the way goimports would write them) -- any full-project goimports pass
	// would rewrite this file's import block.
	otherSrc := "package other\n\nimport (\n\t\"strings\"\n\t\"fmt\"\n)\n\nfunc OtherFunc() string { return fmt.Sprintf(\"%s\", strings.ToUpper(\"x\")) }\n"
	otherPath := filepath.Join(projDir, "other", "other.go")
	os.WriteFile(otherPath, []byte(otherSrc), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}

	result, _, err := s.handleTest(context.Background(), nil, nameParam{Name: "RootFunc"})
	if err != nil {
		t.Fatalf("handleTest: %v", err)
	}
	if !strings.Contains(resultText(t, result), "ALL TESTS PASSED") {
		t.Fatalf("expected RootFunc's test to pass, got: %s", resultText(t, result))
	}

	after, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("handleTest rewrote an unrelated file it never needed to touch:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestHandleRename_StructFieldRenamesDeclarationAndCallers is the
// positive regression: struct fields are excluded from emit by design
// (#11), so a field rename has to rewrite the enclosing TYPE's own Body
// (a separate DB row) in addition to the field's own row and its
// callers. Verifies the struct declaration on disk actually changes,
// both selector and keyed-composite-literal callers are rewritten, and
// the result still builds (handleFieldRename pays for a real build gate
// unlike the rest of rename).
func TestHandleRename_StructFieldRenamesDeclarationAndCallers(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "fieldproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package fieldproj

type Opts struct {
	Count bool
}

func readSelector(o Opts) bool {
	return o.Count
}

func buildLiteral() Opts {
	return Opts{Count: true}
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName:  "Count",
		NewName:  "CountOnly",
		Receiver: "Opts",
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "not supported") {
		t.Fatalf("expected rename to succeed, got: %s", text)
	}
	if !strings.Contains(text, "struct declaration") {
		t.Errorf("expected the response to confirm the struct declaration was updated, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if strings.Contains(src, "Count bool") {
		t.Errorf("expected the struct declaration to be renamed, still has old field:\n%s", src)
	}
	if !strings.Contains(src, "CountOnly bool") {
		t.Errorf("expected the struct declaration to declare CountOnly, got:\n%s", src)
	}
	if !strings.Contains(src, "o.CountOnly") {
		t.Errorf("expected the selector-expression caller to be rewritten, got:\n%s", src)
	}
	if !strings.Contains(src, "Opts{CountOnly: true}") {
		t.Errorf("expected the keyed composite literal caller to be rewritten, got:\n%s", src)
	}
}

// TestHandleApply_RenameStructFieldRenamesDeclarationAndCallers is
// handleApply's rename case's half of the same regression as
// TestHandleRename_StructFieldRenamesDeclarationAndCallers --
// handleApply duplicates handleRename's rename logic inline rather than
// calling it, so the same struct-declaration-rewrite fix has to be
// applied there too.
func TestHandleApply_RenameStructFieldRenamesDeclarationAndCallers(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "fieldproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package fieldproj

type Opts struct {
	Count bool
}

func readSelector(o Opts) bool {
	return o.Count
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "rename", Name: "Count", Receiver: "Opts", NewName: "CountOnly"},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("expected rename to succeed, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if strings.Contains(src, "Count bool") {
		t.Errorf("expected the struct declaration to be renamed, still has old field:\n%s", src)
	}
	if !strings.Contains(src, "CountOnly bool") {
		t.Errorf("expected the struct declaration to declare CountOnly, got:\n%s", src)
	}
	if !strings.Contains(src, "o.CountOnly") {
		t.Errorf("expected the selector-expression caller to be rewritten, got:\n%s", src)
	}
}

// TestHandleRename_StructFieldNameCollisionInCallerRollsBackNotCorrupts
// is the safety-net regression for the exact failure mode found via a
// live etcd bench trajectory: astRename's caller-body rewrite is bare-
// identifier-based, so a caller that references TWO different types'
// same-named field (e.g. a.Count and b.Count, two unrelated structs)
// gets BOTH renamed when only one was the actual target -- corrupting
// a reference to a field that was never supposed to change. Before
// handleFieldRename added a real build gate for struct field renames
// (unlike the rest of rename, which skips it for perf), this shipped
// silently: defn reported success while the emitted code no longer
// compiled. Must roll back cleanly instead.
func TestHandleRename_StructFieldNameCollisionInCallerRollsBackNotCorrupts(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "fieldproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldproj\n\ngo 1.26\n"), 0644)
	src := `package fieldproj

type A struct {
	Count bool
}

type B struct {
	Count int
}

func combine(a A, b B) bool {
	return a.Count == (b.Count > 0)
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName:  "Count",
		NewName:  "CountOnly",
		Receiver: "A",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the build-breaking collision to roll back the whole rename, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be byte-identical after a rolled-back rename, got:\n%s", raw)
	}
}

// TestHandleApply_DeleteRefusesStructField mirrors
// TestHandleDelete_RefusesStructField but through handleApply's inline
// delete case, which re-implements handleDelete's logic rather than
// calling it -- the same duplication that let the original field-rename
// bug diverge between the two paths. Verifies the batch is rolled back
// and reports the field-specific refusal rather than deleting the row.
func TestHandleApply_DeleteRefusesStructField(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "fieldproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldproj\n\ngo 1.26\n"), 0644)
	const src = `package fieldproj

type Opts struct {
	Count bool
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "delete", Name: "Count", Receiver: "Opts"}},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "does not support struct fields") {
		t.Errorf("expected a struct-field refusal message, got: %s", text)
	}

	if _, err := db.GetDefinitionByNameAndReceiver("Count", "fieldproj", "Opts"); err != nil {
		t.Errorf("expected the field's DB row to still exist after the rolled-back apply, got: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be untouched, got:\n%s", string(raw))
	}
}

// TestHandleDelete_RefusesStructField is the regression for the bug the
// op/kind policy layer was built to close: handleDelete had no
// field-kind check at all, so deleting a struct field's DB row (via
// DeleteDefinition) succeeded, reported "Deleted", and left the struct
// declaration on disk completely untouched -- the field kept existing
// in the emitted file with no corresponding DB row, silently diverging
// the two. Verifies the op is now refused up front, before either the
// DB row or the file is touched.
func TestHandleDelete_RefusesStructField(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "fieldproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldproj\n\ngo 1.26\n"), 0644)
	const src = `package fieldproj

type Opts struct {
	Count bool
	Other int
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleDelete(context.Background(), nil, nameParam{
		Name:     "Count",
		Receiver: "Opts",
		Force:    true,
	})
	text := resultText(t, result)
	if strings.Contains(text, "Deleted") {
		t.Fatalf("expected delete to be refused, got: %s", text)
	}
	if !strings.Contains(text, "does not support struct fields") {
		t.Errorf("expected a struct-field refusal message, got: %s", text)
	}

	if _, err := db.GetDefinitionByNameAndReceiver("Count", "fieldproj", "Opts"); err != nil {
		t.Errorf("expected the field's DB row to still exist after the refused delete, got: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be untouched, got:\n%s", string(raw))
	}
}

// TestHandlePatch_RefusesStructField is the second confirmed silent-
// corruption path found while scoping the op/kind policy layer:
// handlePatch has no syntax-validation guard at all (unlike edit/
// fragment-edit/insert), so patching a struct field's body text wrote
// straight into the DB row via UpsertDefinition and reported success,
// while the struct declaration on disk -- the actual source of truth
// for emit, since fields are excluded from emit by design (#11) --
// never changed.
func TestHandlePatch_RefusesStructField(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "fieldproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldproj\n\ngo 1.26\n"), 0644)
	const src = `package fieldproj

type Opts struct {
	Count bool
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handlePatch(context.Background(), nil, codeParam{
		Name:     "Count",
		Receiver: "Opts",
		OldName:  "Count",
		NewName:  "CountX",
	})
	text := resultText(t, result)
	if strings.Contains(text, "Patched") {
		t.Fatalf("expected patch to be refused, got: %s", text)
	}
	if !strings.Contains(text, "does not support struct fields") {
		t.Errorf("expected a struct-field refusal message, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be untouched, got:\n%s", string(raw))
	}
}

// TestUnsupportedFieldOp is the policy-layer table: struct fields are
// excluded from emit by design (#11), so every write op that resolves a
// field as its target must either know how to rewrite the parent type's
// Body alongside it (today, only rename does) or refuse explicitly
// instead of silently diverging the DB from the file. Locks in the
// policy so a new op added later doesn't quietly reopen the gap.
func TestUnsupportedFieldOp(t *testing.T) {
	for _, op := range []string{"delete", "edit", "patch", "insert", "move", "insert-precondition", "replace-slice", "replace-hunk", "wrap-in-defer", "rename-param"} {
		if msg := unsupportedFieldOp("field", op); msg == "" {
			t.Errorf("unsupportedFieldOp(%q, %q) = \"\", want a refusal message", "field", op)
		}
	}
	if msg := unsupportedFieldOp("field", "rename"); msg != "" {
		t.Errorf("unsupportedFieldOp(field, rename) = %q, want \"\" (rename has real field support)", msg)
	}
	for _, kind := range []string{"func", "method", "type", "var", "const"} {
		if msg := unsupportedFieldOp(kind, "delete"); msg != "" {
			t.Errorf("unsupportedFieldOp(%q, delete) = %q, want \"\" (only field kind is restricted)", kind, msg)
		}
	}
}

// TestHandleRename_RefusesInterfaceBreakingMethodRename is the
// regression for a live-confirmed bug: #148 skips rename's build gate
// on the assumption that renaming a method is always dispatch-safe.
// That assumption is false when the method's receiver type also
// satisfies an interface declaring a method under the OLD name --
// interface methods live inline in the interface's own Body (same
// #11 shape as struct fields), so nothing rewrote the interface's
// text, and the caller-rewrite loop happily turned a valid
// interface-dispatch call site (r.Bar()) into one that doesn't
// compile (r.Baz undefined). Verifies the rename now routes through a
// real build gate in this case and rolls back cleanly with the actual
// compiler diagnostic instead of silently shipping broken code.
func TestHandleRename_RefusesInterfaceBreakingMethodRename(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "ifaceproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module ifaceproj\n\ngo 1.26\n"), 0644)
	const src = `package ifaceproj

type Reader interface {
	Bar() int
}

type Foo struct{}

func (f Foo) Bar() int { return 1 }

func use(r Reader) int {
	return r.Bar()
}

func direct() int {
	f := Foo{}
	return f.Bar()
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName:  "Bar",
		NewName:  "Baz",
		Receiver: "Foo",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the rename to be rolled back, got: %s", text)
	}
	if !strings.Contains(text, "Baz undefined") {
		t.Errorf("expected the real compiler diagnostic to be surfaced, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be untouched, got:\n%s", string(raw))
	}

	if d, err := db.GetDefinitionByNameAndReceiver("Bar", "ifaceproj", "Foo"); err != nil || d == nil {
		t.Errorf("expected Foo.Bar's DB row to still exist after rollback, got def=%v err=%v", d, err)
	}
}

// TestHandleRename_RefusesEmbeddedInterfaceBreakingMethodRename covers
// a variant of TestHandleRename_RefusesInterfaceBreakingMethodRename
// where the method is declared on an EMBEDDED interface (Reader embeds
// BaseReader, which declares Bar()) rather than directly. This works
// not because methodRenameRisksInterfaceBreak understands embedding
// syntax, but because resolve() stages an "implements" edge for every
// interface a type structurally satisfies independently -- Foo
// satisfies BaseReader on its own merits, so that edge exists
// alongside the one to the composite Reader, and BaseReader's own body
// directly declares Bar(). Locks in that the embedded case is covered.
func TestHandleRename_RefusesEmbeddedInterfaceBreakingMethodRename(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "ifaceproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module ifaceproj\n\ngo 1.26\n"), 0644)
	const src = `package ifaceproj

type BaseReader interface {
	Bar() int
}

type Reader interface {
	BaseReader
}

type Foo struct{}

func (f Foo) Bar() int { return 1 }

func use(r Reader) int {
	return r.Bar()
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName:  "Bar",
		NewName:  "Baz",
		Receiver: "Foo",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the rename to be rolled back, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be untouched, got:\n%s", string(raw))
	}
}

// TestHandleReplaceHunk_SignatureTypeChangeGatesOnRealBuild is the
// regression for a live-confirmed bug: applyEditTerse (the shared
// response path for all 5 projection ops) assumed every projection op
// is "AST-guaranteed sig-stable" and unconditionally skipped the build
// gate. That's true for insert-precondition/wrap-in-defer (body-
// statement-only) and rename-param (renames an identifier, never a
// type), but replace-hunk is deliberately content-addressed and
// kind-agnostic -- it can target a function's own signature line
// directly. Renaming a parameter's TYPE (not its name/receiver, so the
// existing identity check never fires) reported "replaced hunk" as a
// clean success while shipping a caller that no longer type-checks.
// Verifies this now gates on a real build and rolls back cleanly.
func TestHandleReplaceHunk_SignatureTypeChangeGatesOnRealBuild(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "sigproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module sigproj\n\ngo 1.26\n"), 0644)
	const src = `package sigproj

func double(x int) int {
	return x * 2
}

func use() int {
	return double(5)
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleReplaceHunk(context.Background(), nil, codeParam{
		Name: "double",
		Old:  "x int) int {\n\treturn x * 2",
		New:  "x string) int {\n\treturn len(x) * 2",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("expected the build failure to be surfaced, got: %s", text)
	}
	if !strings.Contains(text, "cannot use 5") {
		t.Errorf("expected the real compiler diagnostic, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be untouched, got:\n%s", string(raw))
	}
}

// TestHandleReplaceSlice_SignatureKindTypeChangeGatesOnRealBuild covers
// the same applyEditTerse gap via replace-slice's documented
// slice:"signature" kind, which -- like replace-hunk -- can change a
// parameter or return type directly.
func TestHandleReplaceSlice_SignatureKindTypeChangeGatesOnRealBuild(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "sigproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module sigproj\n\ngo 1.26\n"), 0644)
	const src = `package sigproj

func double(x int) int {
	return x * 2
}

func use() int {
	return double(5)
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleReplaceSlice(context.Background(), nil, codeParam{
		Name:  "double",
		Slice: "signature",
		New:   "func double(x string) int",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("expected the build failure to be surfaced, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be untouched, got:\n%s", string(raw))
	}
}

// TestHandleEdit_InterfaceMethodRemovalGatesOnRealBuild is the
// regression for the highest-leverage bug found this session:
// extractSignature's *ast.TypeSpec case collapses to just "type
// <Name>" regardless of the type's actual shape, so handleEdit's
// oldSignature==newSignature sig-stable check was always true for a
// type/interface-kind edit no matter what changed inside the braces.
// Removing a method from an interface via the single most commonly
// used write op reported "Updated Reader" as a clean success and
// wrote it to disk while a caller still invoking that method no
// longer compiled, with zero warning. Verifies this now gates on a
// real build and rolls back cleanly with the actual diagnostic.
func TestHandleEdit_InterfaceMethodRemovalGatesOnRealBuild(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "ifaceproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module ifaceproj\n\ngo 1.26\n"), 0644)
	const src = `package ifaceproj

type Reader interface {
	Bar() int
	Qux() string
}

func use(r Reader) string {
	return r.Qux()
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleEdit(context.Background(), nil, editParam{
		Name:    "Reader",
		NewBody: "type Reader interface {\n\tBar() int\n}",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the edit to be rolled back, got: %s", text)
	}
	if !strings.Contains(text, "Qux undefined") {
		t.Errorf("expected the real compiler diagnostic, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be untouched, got:\n%s", string(raw))
	}
}

// TestHandleEdit_StructFieldRemovalGatesOnRealBuild is the struct-kind
// variant of TestHandleEdit_InterfaceMethodRemovalGatesOnRealBuild --
// same extractSignature blind spot, this time removing a struct field
// still referenced by a composite literal elsewhere.
func TestHandleEdit_StructFieldRemovalGatesOnRealBuild(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "structproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module structproj\n\ngo 1.26\n"), 0644)
	const src = `package structproj

type Opts struct {
	Count int
}

func use() Opts {
	return Opts{Count: 5}
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleEdit(context.Background(), nil, editParam{
		Name:    "Opts",
		NewBody: "type Opts struct {\n}",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the edit to be rolled back, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be untouched, got:\n%s", string(raw))
	}
}

// TestHandleReplaceHunk_InterfaceMethodRemovalGatesOnRealBuild covers
// the same extractSignature blind spot via applyEditTerse (the shared
// response path for the 5 projection ops), confirming replace-hunk
// landing directly on an interface's method list also gates on a real
// build now instead of the sig-stable fast path.
func TestHandleReplaceHunk_InterfaceMethodRemovalGatesOnRealBuild(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "ifaceproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module ifaceproj\n\ngo 1.26\n"), 0644)
	const src = `package ifaceproj

type Reader interface {
	Bar() int
	Qux() string
}

func use(r Reader) string {
	return r.Qux()
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleReplaceHunk(context.Background(), nil, codeParam{
		Name: "Reader",
		Old:  "Bar() int\n\tQux() string",
		New:  "Bar() int",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("expected the build failure to be surfaced, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != src {
		t.Errorf("expected main.go to be untouched, got:\n%s", string(raw))
	}
}

// TestHandleCreateScaffoldFile_RoutesThroughTransactionAndStillSucceeds
// is the regression for wrapping handleCreateScaffoldFile's write in a
// transaction (#12-class fix -- every sibling write handler already had
// this): confirms the DB-side behavior is unaffected by the fix -- the
// file_sources row is durably committed and reports success. See
// TestHandleCreateScaffoldFile_ActuallyWritesFileToDisk for the
// separate #276 fix (now shipped) confirming the content actually
// lands on disk, not just in file_sources.
func TestHandleCreateScaffoldFile_RoutesThroughTransactionAndStillSucceeds(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(filepath.Join(projDir, "pkg"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "pkg", "existing.go"), []byte("package pkg\n\nfunc Real() int {\n\treturn 1\n}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Blank import: goimports (which now actually runs against this
	// file post-#276) legitimately strips a genuinely unused named
	// import, but keeps a blank one.
	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:   "create",
		File: "pkg/newfile.go",
		Body: "import _ \"fmt\"\n",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Scaffolded") {
		t.Fatalf("expected a successful scaffold, got: %s", text)
	}

	mods, err := db.ListModules()
	if err != nil {
		t.Fatal(err)
	}
	var modID int64
	for _, m := range mods {
		if m.Path == "proj/pkg" || strings.HasSuffix(m.Path, "/pkg") {
			modID = m.ID
		}
	}
	if modID == 0 {
		t.Fatalf("could not find pkg module among: %+v", mods)
	}
	src, err := db.GetFileSource(modID, "pkg/newfile.go")
	if err != nil || src == "" {
		t.Fatalf("scaffold content not committed to file_sources: err=%v src=%q", err, src)
	}
	if !strings.Contains(src, "_ \"fmt\"") {
		t.Errorf("committed file_sources missing expected content: %q", src)
	}

	// The pre-existing Real() def must be untouched.
	real, err := db.GetDefinitionByName("Real", "")
	if err != nil || real == nil {
		t.Fatalf("Real definition should still exist: %v", err)
	}
}

// TestHandleDelete_ForceWithBuildFailureStillRunsPostDeleteCleanup is the
// regression for the shared buildResult=="" gate incorrectly covering
// both the normal and force:true paths: on the force path, commit()
// always runs regardless of buildResult (force is an explicit
// acknowledgment the delete may break the build), so gating the
// post-delete idf.Invalidate()/autoCommit() step on buildResult=="" too
// skipped it whenever force:true's delete happened to also break the
// build -- even though the delete was already durable. This confirms
// the delete itself lands (durability, the directly observable half of
// the fix) under exactly that force+build-failure combination.
func TestHandleDelete_ForceWithBuildFailureStillRunsPostDeleteCleanup(t *testing.T) {
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

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Force-delete Helper while Caller still references it -- the
	// build will fail (undefined: Helper), but force:true means the
	// delete must still land.
	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:    "delete",
		Name:  "Helper",
		Force: true,
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Deleted") {
		t.Fatalf("expected the force delete to report Deleted despite the build failure, got: %s", text)
	}

	if _, err := db.GetDefinitionByName("Helper", ""); err == nil {
		t.Fatal("Helper should be gone from the DB after a force delete")
	}
}

// TestHandleApply_EditResolvesDottedQualifiedName is the regression for
// resolveApplyTarget missing the dotted-qualified-name fallback
// resolveEditTarget already has (#241): a batched edit for
// "beta.Widget" used to fail with resolveApplyTarget's own ambiguous-
// name refusal (two Widgets share the bare name across packages) even
// though the dotted form unambiguously names one of them -- the exact
// same shape code(op:"edit", name:"beta.Widget") already resolves
// outside a batch.
func TestHandleApply_EditResolvesDottedQualifiedName(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.MkdirAll(filepath.Join(projDir, "beta"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "widget.go"), []byte(`package alpha

func Widget() string { return "alpha" }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "beta", "widget.go"), []byte(`package beta

func Widget() string { return "beta" }
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op: "apply",
		Operations: []applyOp{
			{Op: "edit", Name: "beta.Widget", NewBody: `func Widget() string { return "beta-edited" }`},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "ambiguous") || strings.Contains(text, "not found") {
		t.Fatalf("expected the dotted name to disambiguate, got: %s", text)
	}

	betaWidget, err := db.GetDefinitionByName("Widget", "testproj/beta")
	if err != nil {
		t.Fatalf("lookup beta.Widget: %v", err)
	}
	if !strings.Contains(betaWidget.Body, "beta-edited") {
		t.Errorf("beta.Widget was not edited, body: %s", betaWidget.Body)
	}

	alphaWidget, err := db.GetDefinitionByName("Widget", "testproj/alpha")
	if err != nil {
		t.Fatalf("lookup alpha.Widget: %v", err)
	}
	if strings.Contains(alphaWidget.Body, "beta-edited") {
		t.Errorf("alpha.Widget was wrongly edited instead of beta.Widget: %s", alphaWidget.Body)
	}
}

// TestRunPlanSteps_ResolvesDottedQualifiedName is the regression for
// runPlanSteps resolving step targets via a bare GetDefinitionByName
// instead of resolveEditTarget: a step naming "beta.Widget" (Go's own
// qualified-name convention) used to report not-found even though the
// identical name resolves fine via read/outline/every other op, since
// the bare lookup has no dotted-name fallback and Widget is ambiguous
// by bare name alone (two packages each declare one).
func TestRunPlanSteps_ResolvesDottedQualifiedName(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.MkdirAll(filepath.Join(projDir, "beta"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "widget.go"), []byte(`package alpha

func Widget() string { return "alpha" }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "beta", "widget.go"), []byte(`package beta

func Widget() string { return "beta" }
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.runPlanSteps([]planformat.Step{
		{Target: "beta.Widget", Field: "outline"},
	})
	if err != nil {
		t.Fatalf("runPlanSteps: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "not found") {
		t.Fatalf("expected beta.Widget to resolve, got: %s", text)
	}
	if !strings.Contains(text, "Widget") {
		t.Errorf("expected Widget's outline in the result, got: %s", text)
	}
}

// TestExtractQueryTokensLower_MatchesProjectionMirrorOnNonASCII is the
// regression for the calque-found drift between this function and its
// declared mirror, internal/projection's extractQueryTokens (#157):
// this copy checked ASCII-only 'a'-'z'/'0'-'9' ranges while the
// projection version used unicode.IsLetter/IsDigit, so a non-ASCII
// identifier rune (a legal Go identifier character) was silently
// treated as a token separator here but not there. See
// TestExtractQueryTokens_MatchesMCPMirrorOnNonASCII in
// internal/projection for the same expected literal asserted against
// the other copy -- extractQueryTokens is unexported, so the two
// can't call each other across packages; asserting both against the
// same literal is the cross-check.
func TestExtractQueryTokensLower_MatchesProjectionMirrorOnNonASCII(t *testing.T) {
	got := extractQueryTokensLower("café über λambda")
	want := []string{"café", "über", "λambda"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractQueryTokensLower(%q) = %v, want %v", "café über λambda", got, want)
	}
}

// TestHandleEdit_DiskIOFailureRollsBackNotSilentSuccess is the
// verification for #275: does a genuine OS-level write failure (not
// just a data-loss safety-net refusal) actually propagate as a
// rollback, or can it be silently swallowed? Simulates external
// interference by replacing the target file with a directory on disk
// after ingest -- writeFile/safeWriteGoFile's os.ReadFile hits a real
// "is a directory" error, not os.IsNotExist, so it must propagate as
// writeFile's own error return (not just the softer data-loss warning
// string), all the way up through emitModule/emitWithOpts/
// emitAndBuildAgainst to a rolled-back commitOrRollbackOnBuild.
func TestHandleEdit_DiskIOFailureRollsBackNotSilentSuccess(t *testing.T) {
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
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc F() int {\n\treturn 1\n}\n\nfunc main() {}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Simulate external interference: the target file is now a
	// directory, not a regular file. os.ReadFile inside
	// safeWriteGoFile will get a real I/O error here, not ENOENT.
	os.Remove(filepath.Join(projDir, "main.go"))
	os.MkdirAll(filepath.Join(projDir, "main.go"), 0755)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:      "edit",
		Name:    "F",
		NewBody: "func F() int {\n\treturn 2\n}",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected the edit to be refused/rolled back when the target path is a directory, got: %s", text)
	}

	// The DB must not durably believe F's body changed either.
	f, err := db.GetDefinitionByName("F", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.Body, "return 2") {
		t.Errorf("DB durably committed the edit despite the disk write failing: %s", f.Body)
	}
}

// TestHandleCreateScaffoldFile_ActuallyWritesFileToDisk is the
// regression for #276: emitModule built its write set exclusively from
// Definitions, so a scaffold-only file (zero defs, just a raw
// file_sources row) was invisible to it in EVERY scenario -- a module
// with other defs never put a def-less file in byFile at all, and a
// module with zero defs total hit the len(defs)==0 bailout before
// byFile was even built. handleCreateScaffoldFile reported "Scaffolded
// ... N bytes" success while the file was never actually created on
// disk. This confirms the file now genuinely lands, in a module that
// also has an existing real definition (the shape that hit the bug --
// a module with SOME defs, just not for this specific new file).
func TestHandleCreateScaffoldFile_ActuallyWritesFileToDisk(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(filepath.Join(projDir, "pkg"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "pkg", "existing.go"), []byte("package pkg\n\nfunc Real() int {\n\treturn 1\n}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Blank import: goimports legitimately strips a genuinely unused
	// named import ("fmt" referenced nowhere), but a blank import is
	// kept -- it's the realistic shape a real scaffold body would use.
	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:   "create",
		File: "pkg/newfile.go",
		Body: "import _ \"fmt\"\n",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Scaffolded") {
		t.Fatalf("expected a successful scaffold, got: %s", text)
	}

	src, err := os.ReadFile(filepath.Join(projDir, "pkg", "newfile.go"))
	if err != nil {
		t.Fatalf("scaffolded file was NOT written to disk despite reporting success: %v", err)
	}
	if !strings.Contains(string(src), "package pkg") || !strings.Contains(string(src), "_ \"fmt\"") {
		t.Errorf("scaffolded file on disk missing expected content:\n%s", src)
	}

	// Real() must still be intact -- this write must be purely
	// additive, not disturb the sibling file.
	realSrc, err := os.ReadFile(filepath.Join(projDir, "pkg", "existing.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(realSrc), "func Real()") {
		t.Errorf("existing.go lost Real() as a side effect of scaffolding a sibling file:\n%s", realSrc)
	}
}

// TestHandleCode_DryRunNeverWritesForAnyWriteOp is the single
// authoritative table for dry_run:true across every write-capable op.
// It exists because dry_run turned out to be a "silently ignored
// param" class of bug, not a one-off: edit and delete each had their
// own independent fix at different times (#246 and its sibling), and
// this session found the SAME gap freshly in create, add-import, and
// all five projection ops (insert-precondition, replace-slice,
// replace-hunk, wrap-in-defer, rename-param) -- confirmed live in a
// real trajectory (prometheus-18712, v4 mining round) where 30+
// replace-hunk calls with dry_run:true silently wrote for real. Rather
// than let each op accumulate its own bespoke dry-run test (the same
// "slightly different duplicated guardrails" risk as the bug itself),
// every op that accepts dry_run gets one row here. Adding a new write
// op should mean adding one row, not writing a new test from scratch.
func TestHandleCode_DryRunNeverWritesForAnyWriteOp(t *testing.T) {
	cases := []struct {
		name string
		args codeParam
	}{
		{"edit", codeParam{Op: "edit", Name: "Greet", NewBody: "func Greet(name string) string {\n\treturn \"Hi, \" + name\n}", DryRun: true}},
		{"delete", codeParam{Op: "delete", Name: "Greet", Force: true, DryRun: true}},
		{"create", codeParam{Op: "create", Body: "func Zorp() int {\n\treturn 1\n}", File: "main.go", DryRun: true}},
		{"add-import", codeParam{Op: "add-import", File: "main.go", ImportPath: "fmt", DryRun: true}},
		{"insert-precondition", codeParam{Op: "insert-precondition", Name: "Greet", Condition: `name == ""`, Ret: `return ""`, DryRun: true}},
		{"replace-slice", codeParam{Op: "replace-slice", Name: "Greet", Slice: "return", New: `return "Hi, " + name`, DryRun: true}},
		{"replace-hunk", codeParam{Op: "replace-hunk", Name: "Greet", Old: "Hello, ", New: "Hi, ", DryRun: true}},
		{"wrap-in-defer", codeParam{Op: "wrap-in-defer", Name: "Greet", DeferBody: `println("done")`, DryRun: true}},
		{"rename-param", codeParam{Op: "rename-param", Name: "Greet", OldParam: "name", NewParam: "n", DryRun: true}},
		{"patch", codeParam{Op: "patch", Name: "Greet", OldName: "Hello, ", NewName: "Hi, ", DryRun: true}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, projDir := setupTestDB(t)
			defer db.Close()
			s := &server{backend: db, projectDir: projDir}
			s.ready.Store(true)

			before, err := db.GetDefinitionByName("Greet", "")
			if err != nil {
				t.Fatal(err)
			}
			diskBefore, err := os.ReadFile(filepath.Join(projDir, "main.go"))
			if err != nil {
				t.Fatal(err)
			}

			result, _, err := s.handleCode(context.Background(), nil, c.args)
			if err != nil {
				t.Fatalf("handleCode: %v", err)
			}
			text := resultText(t, result)
			if result.IsError {
				t.Fatalf("expected dry-run %s to succeed, got error: %s", c.name, text)
			}
			if !strings.Contains(text, "dry run") {
				t.Errorf("expected dry-run preview text for %s, got: %s", c.name, text)
			}

			after, err := db.GetDefinitionByName("Greet", "")
			if err != nil {
				t.Fatal(err)
			}
			if after.Body != before.Body {
				t.Errorf("%s: Greet's body changed under dry_run:true\nbefore:\n%s\nafter:\n%s", c.name, before.Body, after.Body)
			}

			diskAfter, err := os.ReadFile(filepath.Join(projDir, "main.go"))
			if err != nil {
				t.Fatal(err)
			}
			if string(diskAfter) != string(diskBefore) {
				t.Errorf("%s: main.go changed on disk under dry_run:true\nbefore:\n%s\nafter:\n%s", c.name, diskBefore, diskAfter)
			}

			if c.name == "create" {
				if _, err := db.GetDefinitionByName("Zorp", ""); err == nil {
					t.Errorf("create: Zorp should NOT have been created under dry_run:true, but it exists in the DB")
				}
			}
		})
	}
}

// TestHandleCode_VersionReportsBuildIdentity locks in op:"version": a
// cheap, no-param self-check for "is the process answering my tool
// calls actually running current code." Added after a real friction
// point in this session -- defn's auto-sharing HTTP architecture
// means reconnecting the MCP client can re-attach to an
// already-running, stale serve process instead of spawning a fresh
// one from a just-rebuilt binary, and the only prior signal for that
// was an indirect, confusing schema-validation failure on a
// brand-new param. This gives a direct one-call answer instead.
func TestHandleCode_VersionReportsBuildIdentity(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "version"})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected op:version to succeed, got error: %s", text)
	}
	if !strings.Contains(text, "defn "+Version) {
		t.Errorf("expected version string %q in response, got: %s", Version, text)
	}
	if !strings.Contains(text, "pid:") {
		t.Errorf("expected pid in response, got: %s", text)
	}
	if !strings.Contains(text, "commit:") {
		t.Errorf("expected a commit: line (see CommitInfo) in response, got: %s", text)
	}
}

// TestHandleRename_StructFieldWithSameNameOnAnotherTypeInSameFileResolvesCorrectly
// is the collision case TestHandleRename_StructFieldRenamesDeclarationAndCallers
// doesn't cover: two DIFFERENT types in the SAME file each have a field
// named "Count". Real-world shape (etcd, mvcc/kv.go): RangeOptions.Count
// and RangeResult.Count sit side by side; a rename targeting
// RangeOptions.Count via receiver:"RangeOptions" must resolve to that
// field specifically, update only ITS declaration and ITS own caller,
// and leave the other type's same-named field and caller completely
// untouched. If receiver-scoped resolution for a field target is
// broken, this either targets the wrong field (renaming Wrong's Count
// instead, leaving Target.Count and its real caller stale) or reports
// success with zero callers updated despite a real caller existing.
func TestHandleRename_StructFieldWithSameNameOnAnotherTypeInSameFileResolvesCorrectly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "collideproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module collideproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package collideproj

type Target struct {
	Count bool
}

type Wrong struct {
	Count int
}

func useTarget(t Target) bool {
	return t.Count
}

func useWrong(w Wrong) int {
	return w.Count
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName:  "Count",
		NewName:  "CountOnly",
		Receiver: "Target",
		File:     "main.go",
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "not supported") {
		t.Fatalf("expected rename to succeed, got: %s", text)
	}
	if !strings.Contains(text, "struct declaration") {
		t.Errorf("expected the response to confirm a struct declaration was updated (field-rename path taken), got: %s", text)
	}
	if strings.Contains(text, "Updated 0 callers") {
		t.Errorf("expected Target's real caller (useTarget) to be counted, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if !strings.Contains(src, "CountOnly bool") {
		t.Errorf("expected Target's field to be renamed to CountOnly, got:\n%s", src)
	}
	if !strings.Contains(src, "t.CountOnly") {
		t.Errorf("expected useTarget's selector to be rewritten to t.CountOnly, got:\n%s", src)
	}
	if !strings.Contains(src, "Count int") {
		t.Errorf("expected Wrong's field to be left completely untouched, got:\n%s", src)
	}
	if !strings.Contains(src, "w.Count") {
		t.Errorf("expected useWrong's selector to be left completely untouched, got:\n%s", src)
	}
}

// TestHandleTestByName_NonexistentLiteralNameFailsFastInsteadOfWholeRepoSweep
// is the negative counterpart to InfersScopeFromPatternWhenNoHintGiven:
// when the pattern is a bare identifier (no regex metachars) that
// matches NO definition anywhere in the project's index, go test's
// -run can never match it in any scope either -- running a full
// "./..." compile+scan to confirm that is pure waste. Real trajectory
// (prometheus-12024, Opus): test(test:"TestTargetScraper") -- a name
// that was never a real function anywhere in that codebase -- spent
// 120.8s running across the whole repo only to report "no tests to
// run". This locks in the fast-fail instead: no go test subprocess at
// all, just an immediate, honest "not found" pointing at op:"sync" as
// the likely fix if the test was just created.
func TestHandleTestByName_NonexistentLiteralNameFailsFastInsteadOfWholeRepoSweep(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha.go"), []byte(`package alpha

func Widget() bool { return true }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha_test.go"), []byte(`package alpha

import "testing"

func TestWidget(t *testing.T) {
	if !Widget() {
		t.Fatal("false")
	}
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	start := time.Now()
	result, _, err := s.handleTestByName(context.Background(), nil, "TestDoesNotExistAnywhereInThisRepo", "", "")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "No test named") || !strings.Contains(text, "project's index") {
		t.Errorf("expected an immediate not-found message, got: %s", text)
	}
	if strings.Contains(text, "./...") {
		t.Errorf("must not have run a whole-repo go test sweep, got: %s", text)
	}
	// A real go test invocation (even a fast one) takes real subprocess
	// spawn + compile time; a fast-fail should be near-instant. Generous
	// bound to avoid flaking on a loaded CI box while still catching a
	// regression back to actually shelling out.
	if elapsed > 2*time.Second {
		t.Errorf("expected a near-instant fast-fail with no subprocess, took %s", elapsed)
	}
}

// TestHandleReadFile_LineRangeNarrowsToOverlappingDefs locks in the
// read-file line_range extension: real trajectory motivation
// (prometheus-18534, prometheus-18358, Opus) -- the model called
// read-file(line_range:...) on a large file expecting the same
// narrowing op:"read" already supports, and silently got the WHOLE
// file back every time (once erroring "exceeds maximum allowed
// tokens" on a 3,485-line file) because read-file never wired the
// param up at all.
func TestHandleReadFile_LineRangeNarrowsToOverlappingDefs(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	// Fixture (setupTestDB's main.go): Greet at lines 3-6 (doc+func),
	// Farewell at lines 8-11. Requesting 8-11 should keep only Farewell.
	result, _, err := s.handleReadFile(context.Background(), nil, codeParam{File: "main.go", LineRange: "8-11"})
	if err != nil {
		t.Fatalf("read-file: %v", err)
	}
	text := resultText(t, result)

	if !strings.Contains(text, "## Farewell") {
		t.Errorf("expected Farewell's definition header in output, got: %s", text)
	}
	if strings.Contains(text, "## Greet") {
		t.Errorf("expected Greet's definition header to be excluded (out of range), got: %s", text)
	}
	if strings.Contains(text, "Hello, ") {
		t.Errorf("expected Greet's own body text to be excluded, got: %s", text)
	}
	if !strings.Contains(text, "line_range read-file") {
		t.Errorf("expected a line_range hint header, got: %s", text)
	}
}

// TestHandleReadFile_LineRangeNoOverlapReturnsHelpfulMessage confirms a
// range with no overlapping definitions fails gracefully instead of
// silently returning the whole file or an empty body.
func TestHandleReadFile_LineRangeNoOverlapReturnsHelpfulMessage(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleReadFile(context.Background(), nil, codeParam{File: "main.go", LineRange: "500-600"})
	if err != nil {
		t.Fatalf("read-file: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "no definitions overlap") {
		t.Errorf("expected a no-overlap message, got: %s", text)
	}
	if result.IsError {
		t.Errorf("no-overlap should be a plain informative result, not an error result: %s", text)
	}
}

// TestHandleSync_PrunesStaleDefSoSubsequentWritesToSameFileSucceed is
// the end-to-end regression for the real prometheus-18712 (Opus)
// trajectory: a stale def orphaned by an out-of-band file edit made
// EVERY subsequent write to that same file fail with "could not be
// matched to an on-disk declaration" -- and the warning's own
// suggested remedy (code(op:"sync", file:...)) didn't actually clear
// it, because IngestFile never pruned. This confirms the fix at the
// level a caller actually experiences it: sync the file, then edit an
// UNRELATED def in it, and the edit must land cleanly with no warning.
func TestHandleSync_PrunesStaleDefSoSubsequentWritesToSameFileSucceed(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	mod, err := db.GetModuleByPath("testproj")
	if err != nil {
		t.Fatalf("find testproj module: %v", err)
	}

	// Simulate a stale def: a row in the DB with no corresponding
	// on-disk declaration in main.go (as if it had been removed by an
	// edit outside defn's own write path).
	if _, err := db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "agentOnlyFlags", Kind: "var",
		Body: "agentOnlyFlags = \"hidden\"", SourceFile: "main.go",
	}); err != nil {
		t.Fatal(err)
	}

	// code(op:"sync", file:"main.go") -- the exact remedy the real
	// warning message suggests.
	if _, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "sync", File: "main.go"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := db.GetDefinitionByName("agentOnlyFlags", "testproj"); err == nil {
		t.Fatal("expected the stale def to be pruned by sync, but it still exists")
	}

	// Now edit a real, unrelated def in the SAME file. Before the fix,
	// this would roll back with "could not be matched to an on-disk
	// declaration... [agentOnlyFlags]" even though this edit has
	// nothing to do with that name.
	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "edit", Name: "Greet",
		NewBody: `func Greet(name string) string {
	return "Hi, " + name
}`,
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "could not be matched") || strings.Contains(text, "rolled back") {
		t.Errorf("expected the edit to land cleanly after the stale def was pruned, got: %s", text)
	}
	if !strings.Contains(text, "Updated") {
		t.Errorf("expected a normal success response, got: %s", text)
	}
}

func TestHandleCode_DeleteFileOnlyBulkDeletesAllDefsInFileWhenReferencesAreInternal(t *testing.T) {
	// main_test.go's TestGreet/TestFarewell call Greet/Farewell in main.go
	// (internal to their own def, not to main_test.go), and nothing calls
	// INTO either test function -- a clean "delete this whole file" case
	// that shouldn't need force:true, and doesn't touch main.go's func
	// main so the project still builds afterward.
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "delete", File: "main_test.go"})
	text := resultText(t, result)
	if result.IsError || strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("bulk delete of main_test.go should succeed cleanly: %s", text)
	}
	if !strings.Contains(text, "Deleted 2 definition(s)") {
		t.Errorf("expected both defs reported deleted, got: %s", text)
	}

	remaining, err := db.FindDefinitionsByFile("", "main_test.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected 0 defs remaining in main_test.go, got %d", len(remaining))
	}
}

func TestHandleCode_DeleteFileOnlyDryRunDoesNotDelete(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "delete", File: "main_test.go", DryRun: true})
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("dry run should not error: %s", text)
	}
	if !strings.Contains(text, "would delete 2 definition(s)") {
		t.Errorf("expected dry-run preview naming the count, got: %s", text)
	}

	remaining, err := db.FindDefinitionsByFile("", "main_test.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Errorf("dry run must not delete anything -- expected 2 defs still in main_test.go, got %d", len(remaining))
	}
}

func TestHandleCode_DeleteFileOnlyForceBypassesExternalCallerCheck(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "delete", File: "main.go", Force: true})
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("force:true should bypass the external-caller refusal: %s", text)
	}

	remaining, err := db.FindDefinitionsByFile("", "main.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected 0 defs remaining in main.go after forced bulk delete, got %d", len(remaining))
	}
}

func TestHandleCode_DeleteFileOnlyNoDefsFoundGivesHelpfulError(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "delete", File: "nonexistent.go"})
	text := resultText(t, result)
	if !result.IsError {
		t.Fatalf("expected error for a file with no definitions, got: %s", text)
	}
	if !strings.Contains(text, "no definitions found") {
		t.Errorf("expected a helpful message, got: %s", text)
	}
}

func TestHandleCode_DeleteFileOnlyRefusedWhenExternalCallersExist(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "delete", File: "main.go"})
	text := resultText(t, result)
	if !result.IsError {
		t.Fatalf("expected refusal -- main.go's Greet/Farewell are called from main_test.go, got: %s", text)
	}
	if !strings.Contains(text, "external caller") {
		t.Errorf("expected external-caller refusal message, got: %s", text)
	}

	remaining, err := db.FindDefinitionsByFile("", "main.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 3 {
		t.Errorf("refused delete should not touch the DB -- expected 3 defs still in main.go, got %d", len(remaining))
	}
}

func TestHandleCode_DeleteValidationRequiresNameOrFile(t *testing.T) {
	s := &server{backend: nil}
	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "delete"})
	text := resultText(t, result)
	if !strings.Contains(text, "name or file is required") {
		t.Errorf("expected \"name or file is required\", got: %s", text)
	}
}

func TestHandleEdit_MultiNameVarSpecInGroupedBlockDoesNotFalselyBlockUnrelatedEdit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/proj\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := `package proj

import "fmt"

var (
	agentOnlyFlags, serverOnlyFlags []string
)

func agentOnlyFlag(name string) {
	agentOnlyFlags = append(agentOnlyFlags, "--"+name)
}

func serverOnlyFlag(name string) {
	serverOnlyFlags = append(serverOnlyFlags, "--"+name)
}

func reloadConfig(start int) {
	fmt.Println("duration", start)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, dir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: dir}
	s.ready.Store(true)

	// #284-adjacent finding: multi-name specs (var x, y int) are stored
	// once under the first name by design (ingestValueSpec's own doc
	// comment) -- serverOnlyFlags is NOT independently queryable, its
	// text lives inside agentOnlyFlags' Body. Confirm that's still true
	// here rather than assuming it, then focus on the real question:
	// does an unrelated edit round-trip cleanly against this shape?
	if _, err := db.GetDefinitionByName("agentOnlyFlags", "example.com/proj"); err != nil {
		t.Fatalf("expected agentOnlyFlags to be ingested: %v", err)
	}
	if _, err := db.GetDefinitionByName("serverOnlyFlags", "example.com/proj"); err == nil {
		t.Fatalf("serverOnlyFlags unexpectedly has its own def row -- multi-name spec storage changed, update this test's assumptions")
	}

	// Same remedy the real trajectory called after its first failure.
	if _, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "sync", File: "main.go"}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Now edit reloadConfig -- a function that has nothing to do with
	// either flags var -- in the SAME file. Before a fix, this would
	// roll back with "could not be matched to an on-disk declaration...
	// [agentOnlyFlags]" despite the edit never touching that name.
	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "edit", Name: "reloadConfig",
		NewBody: `func reloadConfig(start int) {
	fmt.Println("duration", start, "seconds")
}`,
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "could not be matched") || strings.Contains(text, "rolled back") {
		t.Errorf("expected the edit to land cleanly, got: %s", text)
	}
	if !strings.Contains(text, "Updated") {
		t.Errorf("expected a normal success response, got: %s", text)
	}

	// agentOnlyFlags' spec must still resolve correctly after the round trip.
	if _, err := db.GetDefinitionByName("agentOnlyFlags", "example.com/proj"); err != nil {
		t.Errorf("agentOnlyFlags should still exist after the edit: %v", err)
	}
}

// TestHandleCode_EditRefusesAmbiguousReceiverQualifiedName is the
// write-path counterpart, and the more severe half of #287: without
// the fix, code(op:"edit", name:"(*Config).UnmarshalYAML", ...) with
// no receiver:/module:/file: would silently resolve via the same
// blast-radius tiebreak and overwrite whichever package's Config
// happened to win it -- exactly the class of destructive-write
// corruption #248 (resolveWriteTarget) was built to prevent for the
// bare-name case, just reached through a syntax #248 never covered.
func TestHandleCode_EditRefusesAmbiguousReceiverQualifiedName(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "pkga"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "pkgb"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "pkga", "config.go"), []byte(`package pkga

type Config struct{ A string }

func (c *Config) UnmarshalYAML(unmarshal func(any) error) error { return nil }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "pkgb", "config.go"), []byte(`package pkgb

type Config struct{ B string }

func (c *Config) UnmarshalYAML(unmarshal func(any) error) error { return nil }
func UseA(c *Config) string { return c.B }
func UseB(c *Config) string { return c.B }
func UseC(c *Config) string { return c.B }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:      "edit",
		Name:    "(*Config).UnmarshalYAML",
		NewBody: `func (c *Config) UnmarshalYAML(unmarshal func(any) error) error { return errors.New("nope") }`,
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected a refusal for the ambiguous receiver-qualified name, got success: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "ambiguous") {
		t.Errorf("expected an ambiguity refusal message, got: %s", text)
	}

	// Neither Config's UnmarshalYAML should have been touched.
	pkgaDef, err := db.GetDefinitionByNameAndReceiver("UnmarshalYAML", "testproj/pkga", "*Config")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pkgaDef.Body, "nope") {
		t.Errorf("pkga's Config.UnmarshalYAML was overwritten despite the refusal")
	}
	pkgbDef, err := db.GetDefinitionByNameAndReceiver("UnmarshalYAML", "testproj/pkgb", "*Config")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pkgbDef.Body, "nope") {
		t.Errorf("pkgb's Config.UnmarshalYAML was overwritten despite the refusal")
	}
}

// TestHandleExpand_AmbiguityNoteFiresForReceiverQualifiedName is the
// direct regression for the prometheus-18652 bug: a caller can embed
// the receiver directly in a name using Go's own "(*Recv).Method"
// convention (e.g. via expand's names:[] field, which has no separate
// receiver param) instead of the dedicated receiver: param. Before
// #287, ambiguityNote's CountDefinitionsByName(name) checked the
// literal, unparsed string ("(*Config).UnmarshalYAML"), which never
// matches a stored def's Name column -- so this exact cross-package
// ambiguity class produced no warning at all, even though the
// underlying resolution silently picked one of several candidates via
// the same blast-radius tiebreak a bare name uses.
func TestHandleExpand_AmbiguityNoteFiresForReceiverQualifiedName(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "pkga"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "pkgb"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "pkga", "config.go"), []byte(`package pkga

type Config struct{ A string }

func (c *Config) UnmarshalYAML(unmarshal func(any) error) error { return nil }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "pkgb", "config.go"), []byte(`package pkgb

type Config struct{ B string }

func (c *Config) UnmarshalYAML(unmarshal func(any) error) error { return nil }
func UseA(c *Config) string { return c.B }
func UseB(c *Config) string { return c.B }
func UseC(c *Config) string { return c.B }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:      "expand",
		Names:   []string{"(*Config).UnmarshalYAML"},
		Include: []string{"body"},
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "note:") || !strings.Contains(text, "ambiguous") && !strings.Contains(text, "best-effort tiebreak") {
		t.Errorf("expected an ambiguity note for the receiver-qualified name, got:\n%s", text)
	}
	if !strings.Contains(text, "UnmarshalYAML") {
		t.Errorf("expected the resolved def's body to still be returned, got:\n%s", text)
	}
}

// TestHandleReadFile_LineRangeWithFullTrueStaysNarrowOnLargeFile is the
// exact-args regression for the prometheus-18534 trajectory:
// read-file(file:"promql/functions.go", line_range:"700:760",
// full:true) returned 112,831 characters across 3,485 lines -- the
// model had passed full:true specifically because a PRIOR capped
// read-file response on this same file suggested it as the way to
// bypass the signature-only cap, not realizing (before this fix)
// line_range wasn't wired up at all, so full:true dumped the entire
// file. TestHandleReadFile_LineRangeNarrowsToOverlappingDefs already
// locks in line_range alone; this confirms full:true composes safely
// with it instead of re-enabling the whole-file dump on a large file,
// which is the exact combination the real trajectory used.
func TestHandleReadFile_LineRangeWithFullTrueStaysNarrowOnLargeFile(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)

	// A file with 60 small functions (~10 lines each, ~600 lines total)
	// -- large enough that dumping every def's body would dwarf a
	// narrowly-scoped few-def response, mirroring the real file's shape
	// (81 definitions, 3,485 lines).
	var src strings.Builder
	src.WriteString("package big\n\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&src, "// Func%d does something.\nfunc Func%d() int {\n\tx := %d\n\ty := x * 2\n\tz := y + 1\n\treturn z\n}\n\n", i, i, i)
	}
	if err := os.WriteFile(filepath.Join(projDir, "big.go"), []byte(src.String()), 0644); err != nil {
		t.Fatal(err)
	}

	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}

	s := &server{backend: db}

	// Full, uncapped whole-file baseline for comparison.
	fullResult, _, err := s.handleReadFile(context.Background(), nil, codeParam{File: "big.go", Full: true})
	if err != nil {
		t.Fatalf("read-file (whole file): %v", err)
	}
	fullText := resultText(t, fullResult)

	// Narrow window + full:true, same as the real trajectory's exact args.
	narrowResult, _, err := s.handleReadFile(context.Background(), nil, codeParam{File: "big.go", LineRange: "50:65", Full: true})
	if err != nil {
		t.Fatalf("read-file (line_range+full): %v", err)
	}
	narrowText := resultText(t, narrowResult)

	if len(narrowText) >= len(fullText)/2 {
		t.Errorf("line_range:\"50:65\"+full:true returned %d bytes, not meaningfully narrower than the whole file's %d bytes -- full:true is bypassing line_range narrowing", len(narrowText), len(fullText))
	}
	if strings.Contains(narrowText, "Func0()") {
		t.Errorf("expected Func0 (far outside the requested range) to be excluded, got it in:\n%s", narrowText)
	}
}

// TestHandleImpact_JSONTestsCapIndependentOfCallerCap guards #279
// (etcd-21620, 2026-08-19): impactJSON used a single impactJSONCap (200)
// for callers, interface-dispatch callers, AND tests alike. A
// high-blast-radius def with many callers but far more covering tests
// (the common shape for a widely-used, widely-tested type) still
// returned all 200 test names in one call -- 45,410 bytes in the real
// trajectory, 55% of that task's entire defn tool-result byte total,
// for a list the agent only needed as a coverage sanity check. Tests now
// cap at the much smaller impactJSONTestsCap independent of the caller
// lists, with a truncation note pointing at op:"test-coverage" (the
// dedicated deep-dive) instead of op:"query".
func TestHandleImpact_JSONTestsCapIndependentOfCallerCap(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{backend: db}

	m, _ := db.EnsureModule("example.com/lib", "lib", "")
	target := &store.Definition{
		ModuleID: m.ID, Name: "T", Kind: "function", Exported: true,
		Body: "func T() {}", Signature: "func T()",
	}
	target.Hash = store.HashBody(target.Body)
	targetID, _ := db.UpsertDefinition(target)

	// Well under impactJSONCap -- callers should NOT be truncated.
	const callerCount = 10
	for i := 0; i < callerCount; i++ {
		name := fmt.Sprintf("Caller_%d", i)
		c := &store.Definition{
			ModuleID: m.ID, Name: name, Kind: "function",
			Body: fmt.Sprintf("func %s() { T() }", name),
		}
		c.Hash = store.HashBody(c.Body)
		id, _ := db.UpsertDefinition(c)
		_ = db.SetReferences(id, []store.Reference{{FromDef: id, ToDef: targetID, Kind: "call"}})
	}

	// Well over impactJSONTestsCap but under impactJSONCap.
	const testCount = impactJSONTestsCap + 30
	for i := 0; i < testCount; i++ {
		name := fmt.Sprintf("TestT_%d", i)
		c := &store.Definition{
			ModuleID: m.ID, Name: name, Kind: "function", Test: true,
			Body: fmt.Sprintf("func %s(t *testing.T) { T() }", name),
		}
		c.Hash = store.HashBody(c.Body)
		id, _ := db.UpsertDefinition(c)
		_ = db.SetReferences(id, []store.Reference{{FromDef: id, ToDef: targetID, Kind: "call"}})
	}

	result, _, err := s.handleImpact(context.Background(), nil, codeParam{Name: "T", Format: "json"})
	if err != nil {
		t.Fatalf("handleImpact: %v", err)
	}
	text := resultText(t, result)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("impactJSON output is not valid JSON: %v\n%s", err, text)
	}

	// direct_callers includes every caller regardless of test-ness (tests
	// that call T() are callers too, just also separately surfaced in the
	// "tests" list) -- callerCount+testCount total, still well under
	// impactJSONCap.
	callers, _ := parsed["direct_callers"].([]any)
	wantCallers := callerCount + testCount
	if len(callers) != wantCallers {
		t.Errorf("expected all %d callers (under impactJSONCap), got %d", wantCallers, len(callers))
	}

	tests, _ := parsed["tests"].([]any)
	if len(tests) != impactJSONTestsCap {
		t.Errorf("expected tests capped at impactJSONTestsCap=%d, got %d", impactJSONTestsCap, len(tests))
	}
	testsTotal, _ := parsed["tests_total"].(float64)
	if int(testsTotal) != testCount {
		t.Errorf("expected tests_total=%d (uncapped true count), got %v", testCount, parsed["tests_total"])
	}

	truncated, ok := parsed["truncated"].(string)
	if !ok {
		t.Fatalf("expected a \"truncated\" field since the tests cap was hit, got none. Full response:\n%s", text)
	}
	if !strings.Contains(truncated, "test-coverage") {
		t.Errorf("expected truncation note to point at op:\"test-coverage\" for the full test list, got: %s", truncated)
	}
	if strings.Contains(truncated, "caller lists capped") {
		t.Errorf("callers were NOT truncated (%d, under the cap) -- truncation note shouldn't claim they were: %s", callerCount, truncated)
	}
}

// TestHandleInsertHeader_DryRunDoesNotWrite guards the same
// silently-dropped dry_run gap fixed repeatedly elsewhere in this file
// (create, add-import, delete) -- dry_run:true must preview without
// touching disk.
func TestHandleInsertHeader_DryRunDoesNotWrite(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, berr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if berr != nil {
		t.Fatalf("read main.go before: %v", berr)
	}

	result, _, err := s.handleInsertHeader(context.Background(), nil, codeParam{
		File:   "main.go",
		Body:   "// Copyright 2026 Example Authors",
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("handleInsertHeader dry_run: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "would prepend") {
		t.Errorf("expected a dry-run preview mentioning 'would prepend', got: %s", text)
	}

	after, aerr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if aerr != nil {
		t.Fatalf("read main.go after: %v", aerr)
	}
	if string(after) != string(before) {
		t.Errorf("dry_run modified the file on disk")
	}
}

// TestHandleInsertHeader_IdempotentOnRepeat guards against a second
// insert-header call (e.g. a retried tool call) duplicating the header
// instead of recognizing it's already present.
func TestHandleInsertHeader_IdempotentOnRepeat(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	header := "// Copyright 2026 Example Authors"
	args := codeParam{File: "main.go", Body: header}
	if _, _, err := s.handleInsertHeader(context.Background(), nil, args); err != nil {
		t.Fatalf("first insertHeader: %v", err)
	}
	result, _, err := s.handleInsertHeader(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("second insertHeader: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "no-op") {
		t.Errorf("expected 'no-op' on repeat call, got: %s", text)
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go: %v", ferr)
	}
	if strings.Count(string(final), header) != 1 {
		t.Errorf("expected header to appear exactly once, got %d times:\n%s", strings.Count(string(final), header), final)
	}
}

// TestHandleInsertHeader_PrependsToFile is the #292 feature test: op:
// "insert-header" is the first defn write op that can touch content
// outside any definition's tracked byte range (a license/copyright
// header before `package`). Guards the real gap found in
// prometheus-19236/17395 (2026-08-19): no existing op could do this at
// all.
func TestHandleInsertHeader_PrependsToFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, berr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if berr != nil {
		t.Fatalf("read main.go before: %v", berr)
	}

	header := "// Copyright 2026 Example Authors\n// Licensed under the Apache License, Version 2.0."
	result, _, err := s.handleInsertHeader(context.Background(), nil, codeParam{
		File: "main.go",
		Body: header,
	})
	if err != nil {
		t.Fatalf("handleInsertHeader: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "inserted") {
		t.Errorf("expected 'inserted' in response, got: %s", text)
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go after: %v", ferr)
	}
	if !strings.HasPrefix(string(final), header) {
		t.Errorf("expected file to start with the header, got:\n%s", final)
	}
	if !strings.HasSuffix(string(final), string(before)) {
		t.Errorf("expected original content preserved byte-for-byte after the header, got:\n%s", final)
	}
}

// TestHandleInsertHeader_RejectsBodyThatBreaksParsing guards against a
// body that isn't pure comment text (e.g. accidentally includes real
// code) silently corrupting the file -- the insertion must be rejected
// before anything is written to disk.
func TestHandleInsertHeader_RejectsBodyThatBreaksParsing(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, berr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if berr != nil {
		t.Fatalf("read main.go before: %v", berr)
	}

	result, _, err := s.handleInsertHeader(context.Background(), nil, codeParam{
		File: "main.go",
		Body: "var oops = 1", // not a comment -- would land before `package`
	})
	if err == nil && (result == nil || !result.IsError) {
		t.Fatalf("expected an error rejecting a non-comment body, got success: %v", resultText(t, result))
	}

	after, aerr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if aerr != nil {
		t.Fatalf("read main.go after: %v", aerr)
	}
	if string(after) != string(before) {
		t.Errorf("file was modified despite the rejected insertion:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestHandleApply_InsertHeaderCombinedWithEditDoesNotRollBackBatch guards
// the #296 bug found reviewing e65af5d: insert-header was wired into
// writeTargets/isWriteOp but never into handleApply's own op-dispatch
// switches, so any apply batch containing insert-header hit "unknown op:
// insert-header" and rolled back the ENTIRE batch -- including other
// valid ops bundled alongside it. This reproduces exactly that shape: an
// edit + an insert-header in one batch, both must land.
func TestHandleApply_InsertHeaderCombinedWithEditDoesNotRollBackBatch(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "edit", Name: "Greet", NewBody: `func Greet(name string) string {
	return "Hi, " + name
}`},
			{Op: "insert-header", File: "main.go", Body: "// Copyright 2026 Example Authors"},
		},
	})
	if err != nil {
		t.Fatalf("handleApply: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "unknown op") {
		t.Fatalf("batch should not have rolled back, got: %s", text)
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go: %v", ferr)
	}
	if !strings.HasPrefix(string(final), "// Copyright 2026 Example Authors") {
		t.Errorf("expected header to land on disk, got:\n%s", final)
	}
	if !strings.Contains(string(final), `"Hi, " + name`) {
		t.Errorf("expected edit to also land on disk, got:\n%s", final)
	}
}

// TestHandleApply_InsertHeaderDryRunPreviewsWithoutWriting guards the
// apply-batch dry-run path for insert-header (#296).
func TestHandleApply_InsertHeaderDryRunPreviewsWithoutWriting(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, berr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if berr != nil {
		t.Fatalf("read main.go before: %v", berr)
	}

	result, _, err := s.handleApply(context.Background(), nil, applyParam{
		DryRun: true,
		Operations: []applyOp{
			{Op: "insert-header", File: "main.go", Body: "// Copyright 2026 Example Authors"},
		},
	})
	if err != nil {
		t.Fatalf("handleApply dry_run: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "would prepend header") {
		t.Errorf("expected dry-run preview, got: %s", text)
	}

	after, aerr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if aerr != nil {
		t.Fatalf("read main.go after: %v", aerr)
	}
	if string(after) != string(before) {
		t.Errorf("dry_run modified the file on disk")
	}
}

// TestHandleInsertHeader_IdempotencyDoesNotFalsePositiveOnPrefixCollision
// guards the #297 bug found reviewing e65af5d: the idempotency check used
// a bare strings.HasPrefix, so a header that's merely a PREFIX of
// unrelated existing content (not the header itself) was wrongly treated
// as "already present" and silently skipped -- reported success with no
// actual insertion.
func TestHandleInsertHeader_IdempotencyDoesNotFalsePositiveOnPrefixCollision(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Seed main.go with content that STARTS WITH the header text as a
	// substring, but isn't actually the header (continues on the same
	// line instead of breaking).
	original, rerr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if rerr != nil {
		t.Fatalf("read main.go: %v", rerr)
	}
	seeded := "// Copyright 2026-2027 Foo Corp, all rights reserved\n\n" + string(original)
	if err := os.WriteFile(filepath.Join(projDir, "main.go"), []byte(seeded), 0644); err != nil {
		t.Fatalf("seed main.go: %v", err)
	}

	result, _, err := s.handleInsertHeader(context.Background(), nil, codeParam{
		File: "main.go",
		Body: "// Copyright 2026",
	})
	if err != nil {
		t.Fatalf("handleInsertHeader: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "no-op") {
		t.Fatalf("expected the header to actually be inserted (prefix collision, not a real match), got no-op: %s", text)
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go after: %v", ferr)
	}
	if !strings.HasPrefix(string(final), "// Copyright 2026\n\n// Copyright 2026-2027 Foo Corp") {
		t.Errorf("expected the new header prepended above the existing similar-looking comment, got:\n%s", final)
	}
}

// TestHandleTestByName_AlternationPatternScopesEmitToBothResolvedFiles
// guards the #298 bug found root-causing prometheus-12024/18972's fresh
// F1 losses: test:"TestFoo|TestBar" (no file:/module: hint -- the shape
// produced whenever a model verifies several just-created tests
// together) isn't a literal test name, so it fell through to the
// "hint == \"\"" branch and testScopeTarget("") returned "./..." for the
// pre-test emit gate, triggering a FULLY UNSCOPED emit.Emit() that
// re-normalized every file in the whole project -- including files
// nothing about the test ever touched. Confirmed live: both fresh
// trajectories show promql/parser/generated_parser.y.go's import block
// silently reordered by exactly this kind of call. Each alternated name
// is now resolved independently to scope the emit to the union of their
// actual files, even though the go test invocation itself still uses
// "./..." when they span different packages (unchanged, safe).
func TestHandleTestByName_AlternationPatternScopesEmitToBothResolvedFiles(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	// A third, unrelated file that must NOT be touched by this test run.
	unrelatedDir := filepath.Join(projDir, "unrelated")
	if err := os.MkdirAll(unrelatedDir, 0755); err != nil {
		t.Fatal(err)
	}
	unrelatedSrc := "package unrelated\n\nimport (\n\t\"fmt\"\n\n\t\"strings\"\n)\n\nfunc F() string { return fmt.Sprint(strings.ToUpper(\"x\")) }\n"
	if err := os.WriteFile(filepath.Join(unrelatedDir, "unrelated.go"), []byte(unrelatedSrc), 0644); err != nil {
		t.Fatal(err)
	}
	before, berr := os.ReadFile(filepath.Join(unrelatedDir, "unrelated.go"))
	if berr != nil {
		t.Fatal(berr)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)
	if err := s.ingestAndResolve(); err != nil {
		t.Fatalf("ingest unrelated.go: %v", err)
	}

	result, _, err := s.handleTestByName(context.Background(), nil, "TestGreet|TestFarewell", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "emit:") {
		t.Fatalf("emit failed: %s", text)
	}

	after, aerr := os.ReadFile(filepath.Join(unrelatedDir, "unrelated.go"))
	if aerr != nil {
		t.Fatal(aerr)
	}
	if string(after) != string(before) {
		t.Errorf("unrelated.go was rewritten by an alternation-pattern test run that never referenced it:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestHandleDeleteFile_RemoveFileActuallyRemovesFileFromDisk guards the
// #301 fix: op:"delete", file:"x.go", remove_file:true now purges every
// def in the file AND removes the file itself. Confirmed on two real
// trajectories (prometheus-12024, prometheus-19017) that the pre-fix
// behavior (defs purged, file left behind as an empty stub) left a
// model with no way to actually get rid of a throwaway file it created
// -- burning 40-90 tool calls across delete/move/patch/emit/gc before
// giving up, since none of those ops remove a file either.
func TestHandleDeleteFile_RemoveFileActuallyRemovesFileFromDisk(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// main_test.go has no callers from outside itself (TestGreet/TestFarewell
	// aren't called by anything), so this is a clean bulk-delete case.
	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "delete", File: "main_test.go", RemoveFile: true,
	})
	if err != nil {
		t.Fatalf("handleCode delete: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Also removed main_test.go from disk") {
		t.Errorf("expected confirmation the file was removed, got: %s", text)
	}

	if _, statErr := os.Stat(filepath.Join(projDir, "main_test.go")); !os.IsNotExist(statErr) {
		t.Errorf("expected main_test.go to be removed from disk, stat err: %v", statErr)
	}
}

// TestHandleDeleteFile_RemoveFileDryRunDoesNotRemove guards dry_run
// composing correctly with remove_file:true -- preview only, no actual
// deletion of defs or the file.
func TestHandleDeleteFile_RemoveFileDryRunDoesNotRemove(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "delete", File: "main_test.go", RemoveFile: true, DryRun: true,
	})
	if err != nil {
		t.Fatalf("handleCode delete dry_run: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "remove main_test.go itself") {
		t.Errorf("expected dry-run preview to mention removing the file, got: %s", text)
	}

	if _, statErr := os.Stat(filepath.Join(projDir, "main_test.go")); statErr != nil {
		t.Errorf("dry_run must not have actually removed the file, stat err: %v", statErr)
	}
}

// TestHandleDeleteFile_RemoveFileOnAlreadyEmptyFileStillRemoves guards
// the #301 case where every def in a file was already purged by a
// prior delete call without remove_file -- a second delete(file:,
// remove_file:true) call on the now-empty stub must still remove the
// leftover file, not error "no definitions found".
func TestHandleDeleteFile_RemoveFileOnAlreadyEmptyFileStillRemoves(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	if _, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "delete", File: "main_test.go",
	}); err != nil {
		t.Fatalf("first delete (purge defs): %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(projDir, "main_test.go")); statErr != nil {
		t.Fatalf("expected the empty stub to still exist after first delete: %v", statErr)
	}

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "delete", File: "main_test.go", RemoveFile: true,
	})
	if err != nil {
		t.Fatalf("second delete (remove empty stub): %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "removed the leftover file") {
		t.Errorf("expected confirmation the leftover stub was removed, got: %s", text)
	}
	if _, statErr := os.Stat(filepath.Join(projDir, "main_test.go")); !os.IsNotExist(statErr) {
		t.Errorf("expected main_test.go to be gone, stat err: %v", statErr)
	}
}

// TestHandleDeleteFile_WithoutRemoveFileKeepsFileOnDisk is the default-
// behavior regression guard for #301: omitting remove_file must keep
// the pre-existing #284 behavior unchanged (file survives with its defs
// purged).
func TestHandleDeleteFile_WithoutRemoveFileKeepsFileOnDisk(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "delete", File: "main_test.go",
	})
	if err != nil {
		t.Fatalf("handleCode delete: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "not removed") {
		t.Errorf("expected the default no-remove message, got: %s", text)
	}

	if _, statErr := os.Stat(filepath.Join(projDir, "main_test.go")); statErr != nil {
		t.Errorf("expected main_test.go to still exist on disk (remove_file not set), stat err: %v", statErr)
	}
}

// TestHandleApply_ReplaceHunkReplaceAllAcrossBatch reproduces the exact
// #302 failure shape from a real trajectory (prometheus-19338): the OLD
// approach of batching several replace-hunk ops with sequential
// index=1..N against the same repeated pattern breaks because indices
// shift as earlier matches are consumed within the same apply
// transaction. A single replace-hunk op with replace_all:true replaces
// every occurrence in one shot instead.
func TestHandleApply_ReplaceHunkReplaceAllAcrossBatch(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	seed, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "edit", Name: "Farewell",
		NewBody: `func Farewell(name string) string {
	return Greet(name) + Greet(name) + Greet(name)
}`,
	})
	if err != nil || seed.IsError {
		t.Fatalf("seed edit: %v / %s", err, resultText(t, seed))
	}

	result, _, err := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "replace-hunk", Name: "Farewell", Old: "Greet(name)", New: `Greet(name + "!")`, ReplaceAll: true},
		},
	})
	if err != nil {
		t.Fatalf("handleApply: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "exceeds") {
		t.Fatalf("expected replace_all to succeed in one op, got: %s", text)
	}

	final, ferr := os.ReadFile(filepath.Join(projDir, "main.go"))
	if ferr != nil {
		t.Fatalf("read main.go: %v", ferr)
	}
	if strings.Count(string(final), `Greet(name+"!")`) != 3 {
		t.Errorf("expected all 3 occurrences replaced, got:\n%s", final)
	}
	if strings.Contains(string(final), "Greet(name) + Greet") {
		t.Errorf("expected no remaining unreplaced Greet(name) occurrences in Farewell, got:\n%s", final)
	}
}

// TestHandleReplaceHunk_ReplaceAllHandlesMultipleIdenticalOccurrences
// guards the #302 fix: replace_all:true replaces every occurrence of
// old in one call, sidestepping the index-shifting problem entirely for
// the common real shape (several identical sites all needing the same
// replacement).
func TestHandleReplaceHunk_ReplaceAllHandlesMultipleIdenticalOccurrences(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "edit", Name: "Farewell",
		NewBody: `func Farewell(name string) string {
	return Greet(name) + Greet(name) + Greet(name)
}`,
	})
	if err != nil {
		t.Fatalf("seed edit: %v", err)
	}
	if result.IsError {
		t.Fatalf("seed edit failed: %s", resultText(t, result))
	}

	result, _, err = s.handleCode(context.Background(), nil, codeParam{
		Op: "replace-hunk", Name: "Farewell",
		Old: "Greet(name)", New: "Hello(name)", ReplaceAll: true,
	})
	if err != nil {
		t.Fatalf("replace-hunk replace_all: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("replace-hunk replace_all failed: %s", text)
	}
}

// TestHandleTestByName_GenuineTimeoutStillReportsTimedOut is the
// regression test for the #304 fix: the TIMED OUT case switched from
// `ctx.Err() == context.DeadlineExceeded` to `err != nil &&
// ctx.Err() == context.DeadlineExceeded`, to stop a genuinely-passing
// run (finishing right at the deadline) from getting a spurious TIMED
// OUT banner appended after real PASS output (confirmed live on
// prometheus-19017: a force:true rerun printed a clean PASS block,
// then still appended "TIMED OUT after 1m0s"). This test proves the
// straightforward, non-racy case -- a test that genuinely hangs past
// the deadline -- still reports TIMED OUT after that change; a real
// process-kill race at the exact deadline boundary isn't practical to
// force deterministically in a unit test.
func TestHandleTestByName_GenuineTimeoutStillReportsTimedOut(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte("package main\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestSlowHang(t *testing.T) {\n\ttime.Sleep(2 * time.Second)\n}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	orig := testTimeout
	testTimeout = 200 * time.Millisecond
	t.Cleanup(func() { testTimeout = orig })

	result, _, err := s.handleTestByName(context.Background(), nil, "TestSlowHang", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "TIMED OUT") {
		t.Errorf("expected a genuine hang past the deadline to still report TIMED OUT, got:\n%s", text)
	}
}

// TestHandleDelete_RemoveFileKeepsFileWhenDefinitionsRemain guards the
// #310 fix from the other direction: remove_file:true on a name-scoped
// delete must NOT remove the file while sibling definitions still live
// in it.
func TestHandleDelete_RemoveFileKeepsFileWhenDefinitionsRemain(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "delete", Name: "TestFarewell", RemoveFile: true,
	})
	if err != nil {
		t.Fatalf("handleCode delete: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "removed") {
		t.Errorf("should not report removal while TestGreet remains in the file: %s", text)
	}
	if _, statErr := os.Stat(filepath.Join(projDir, "main_test.go")); statErr != nil {
		t.Errorf("main_test.go should still exist (TestGreet remains): %v", statErr)
	}
}

// TestHandleDelete_RemoveFileRemovesFileWhenLastDefinitionDeleted guards
// the #310 fix: op:"delete", name:"X", remove_file:true previously
// silently dropped remove_file on the name-scoped path (only
// handleDeleteFile's file:-only bulk mode honored it) -- a real
// trajectory (prometheus-19236) deleted the last def in a throwaway test
// file expecting the file to go with it, and burned ~8 extra tool calls
// discovering it had to reissue the delete as file:-only instead.
func TestHandleDelete_RemoveFileRemovesFileWhenLastDefinitionDeleted(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	if _, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "delete", Name: "TestGreet"}); err != nil {
		t.Fatalf("handleCode delete TestGreet: %v", err)
	}

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "delete", Name: "TestFarewell", RemoveFile: true,
	})
	if err != nil {
		t.Fatalf("handleCode delete TestFarewell: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Also removed main_test.go from disk") {
		t.Errorf("expected confirmation the file was removed, got: %s", text)
	}
	if _, statErr := os.Stat(filepath.Join(projDir, "main_test.go")); !os.IsNotExist(statErr) {
		t.Errorf("expected main_test.go to be removed from disk, stat err: %v", statErr)
	}
}

// TestHandleDelete_WithoutRemoveFileKeepsEmptiedFileOnDisk confirms
// default behavior is unchanged: deleting every def in a file via the
// name-scoped path without remove_file:true leaves the (now defless)
// file on disk, same as before the #310 fix.
func TestHandleDelete_WithoutRemoveFileKeepsEmptiedFileOnDisk(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	if _, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "delete", Name: "TestGreet"}); err != nil {
		t.Fatalf("handleCode delete TestGreet: %v", err)
	}
	if _, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "delete", Name: "TestFarewell"}); err != nil {
		t.Fatalf("handleCode delete TestFarewell: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(projDir, "main_test.go")); statErr != nil {
		t.Errorf("without remove_file, the emptied stub file should remain on disk: %v", statErr)
	}
}

// TestHandleTestByName_NestedGeneratedFileNotReformattedByParentScope
// guards the remaining gap from prometheus-18534: a hint resolving to a
// package (e.g. "promql") legitimately scopes the pre-test emit to the
// WHOLE recursive build target so `go test ./promql/...` compiles
// cleanly, including files in nested subdirectories like
// promql/parser -- but a GENERATED file living in that subtree should
// still never have its import block reordered by goimports just for
// being in scope, mirroring commit 9cc5175's fix for the unscoped emit
// path. That fix didn't fully close this: it's keyed on
// Opts.TouchedFiles, which handleTestByName deliberately sets broadly
// (every file in the recursive scope) for build-freshness reasons, so
// the "unless explicitly touched" guard never actually excluded a
// generated file living under a resolved package's subdirectory.
// Confirmed live: promql/parser/generated_parser.y.go's import block
// reordered by a test call that never referenced promql/parser at all.
func TestHandleTestByName_NestedGeneratedFileNotReformattedByParentScope(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "pkg", "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "pkg", "root.go"), []byte("package pkg\n\nfunc RootFunc() string { return \"root\" }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "pkg", "root_test.go"), []byte("package pkg\n\nimport \"testing\"\n\nfunc TestRootFunc(t *testing.T) {\n\tif RootFunc() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// A generated file in a NESTED subdirectory of the resolved package
	// -- import order is deliberately "wrong" (fmt after strings) so a
	// goimports pass would visibly reorder it if run.
	// Single unsorted import group (no blank-line separator) -- goimports
	// treats blank-line-separated groups as intentional and never merges
	// or reorders across them, so a two-group fixture (as an earlier
	// version of this test used) never actually exercised a reorder and
	// passed regardless of whether the generated-file skip fired. One
	// group in the wrong alphabetical order is what goimports actually
	// rewrites.
	genSrc := "// Code generated by a tool. DO NOT EDIT.\n\npackage sub\n\nimport (\n\t\"strings\"\n\t\"fmt\"\n)\n\nfunc Gen() string { return fmt.Sprint(strings.ToUpper(\"x\")) }\n"
	genPath := filepath.Join(projDir, "pkg", "sub", "gen.go")
	if err := os.WriteFile(genPath, []byte(genSrc), 0644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatal(err)
	}

	result, _, err := s.handleTestByName(context.Background(), nil, "TestRootFunc", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	if !strings.Contains(resultText(t, result), "ALL TESTS PASSED") {
		t.Fatalf("expected TestRootFunc to pass, got: %s", resultText(t, result))
	}

	after, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("generated file in a nested subdirectory of the resolved package was reformatted by a test call that never referenced it:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestHandleCreateScaffoldFile_GetsInsertHeaderHint mirrors
// TestHandleCreate_NewFileGetsInsertHeaderHint for the scaffold-file
// path (body is imports/package-only, no decls yet) -- always a
// brand-new file by construction.
func TestHandleCreateScaffoldFile_GetsInsertHeaderHint(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "create", File: "scaffolded.go",
		Body: "package main\n\nimport \"fmt\"\n",
	})
	if err != nil {
		t.Fatalf("handleCode create: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("scaffold create failed: %s", text)
	}
	if !strings.Contains(text, "insert-header") {
		t.Errorf("expected a scaffolded new file to hint at insert-header, got: %s", text)
	}
}

// TestHandleCreate_ExistingFileGetsNoInsertHeaderHint confirms the hint
// is scoped to genuinely new files -- adding one more def to a file
// that already exists on disk shouldn't suggest re-adding a header.
func TestHandleCreate_ExistingFileGetsNoInsertHeaderHint(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "create", File: "main.go",
		Body: "func AnotherThing() int { return 2 }",
	})
	if err != nil {
		t.Fatalf("handleCode create: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("create failed: %s", text)
	}
	if strings.Contains(text, "insert-header") {
		t.Errorf("did not expect an insert-header hint when adding to an existing file, got: %s", text)
	}
}

// TestHandleCreate_NewFileGetsInsertHeaderHint guards #313: models
// repeatedly didn't know code(op:"insert-header") exists, still writing
// stale "no way to add a header" memory notes even on a binary where
// the op had existed for a while (prometheus-19236). A brand-new file
// created via op:"create" now gets a one-line nudge toward
// insert-header in the success message.
func TestHandleCreate_NewFileGetsInsertHeaderHint(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "create", File: "brandnew.go",
		Body: "func NewThing() int { return 1 }",
	})
	if err != nil {
		t.Fatalf("handleCode create: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("create failed: %s", text)
	}
	if !strings.Contains(text, "insert-header") {
		t.Errorf("expected a new-file create to hint at insert-header, got: %s", text)
	}
}

func TestHandleDelete_RemoveFileStillRunsWhenForcedDeleteLeavesBuildWarning(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	if _, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "create", File: "solo.go", Body: "func Solo() int { return 42 }",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	// An unrelated, already-broken file written directly to disk
	// (bypassing defn edits, which would otherwise gate on a successful
	// build). solo.go's own deletion leaves zero defs in that file --
	// emit's zero-def policy never touches such a file, so its on-disk
	// content (and thus the build) would otherwise stay clean regardless
	// of the delete. This unrelated compile error is what forces
	// emitAndBuildAgainst's real `go build` to return a non-empty
	// buildResult even though force:true already committed the delete.
	brokenPath := filepath.Join(projDir, "broken.go")
	if err := os.WriteFile(brokenPath, []byte("package main\n\nfunc Broken() int {\n\treturn undefinedThing\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "delete", Name: "Solo", Force: true, RemoveFile: true,
	})
	if err != nil {
		t.Fatalf("handleCode delete: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "undefinedThing") {
		t.Fatalf("expected the pre-existing unrelated build failure to surface in buildResult, got: %s", text)
	}
	if !strings.Contains(text, "Also removed solo.go from disk") {
		t.Fatalf("expected remove_file to still run despite the unrelated build failure, got: %s", text)
	}
	if _, statErr := os.Stat(filepath.Join(projDir, "solo.go")); !os.IsNotExist(statErr) {
		t.Errorf("expected solo.go to be removed from disk, stat err: %v", statErr)
	}
}

// TestCommitInfo_ReturnsRevisionWhenBuiltInAGitCheckout locks in
// CommitInfo's self-describing-binary contract: a test binary built by
// `go test` inside this repo's git checkout gets VCS info stamped
// automatically (no ldflags needed), so this should never fall back to
// "unknown" in normal CI/dev runs -- only when built with
// GOFLAGS=-buildvcs=false or outside a git repo entirely.
func TestCommitInfo_ReturnsRevisionWhenBuiltInAGitCheckout(t *testing.T) {
	got := CommitInfo()
	if got == "" {
		t.Fatal("CommitInfo returned an empty string, want a revision or \"unknown\"")
	}
	if got == "unknown" {
		t.Skip("VCS info not stamped -- built with -buildvcs=false or outside a git checkout")
	}
}

// TestHandleGetDefinition_OutlineDowngradeMentionsFreshSummaryAsOption
// is the #313 followup regression: #174 used to make a fresh cached
// summary the silent DEFAULT for a bare read, substituting an LLM
// paraphrase for a code request without being asked. Outline and
// summary aren't the same kind of "compact" -- outline is ground
// truth (just less transcribed), summary is an inference that can be
// subtly wrong even when hash-fresh. A bare read on a large def must
// still show the outline (ground truth, the safe default), but now
// mentions a fresh summary as an explicit OPTION the caller can reach
// for, rather than silently receiving one instead of what they asked
// for.
func TestHandleGetDefinition_OutlineDowngradeMentionsFreshSummaryAsOption(t *testing.T) {
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
	d, err := db.GetDefinitionByName("BigFunc", "")
	if err != nil {
		t.Fatalf("setup: BigFunc not found: %v", err)
	}
	if err := db.SetDefSummary(d.ID, &store.DefSummary{
		OneLine:  "Builds a padded greeting string.",
		BodyHash: store.HashBodyStructural(d.Body),
		Model:    "test",
	}); err != nil {
		t.Fatalf("SetDefSummary: %v", err)
	}
	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "BigFunc"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Outline shown") {
		t.Errorf("expected outline (ground truth) to remain the default even with a fresh summary present, got: %s", text)
	}
	if strings.Contains(text, "padding to push body") {
		t.Errorf("body must not leak into the outline-downgraded response, got: %s", text)
	}
	if strings.Contains(text, "Builds a padded greeting string.") {
		t.Errorf("summary text must NOT be silently substituted for the outline, got: %s", text)
	}
	if !strings.Contains(text, `mode:"summary"`) {
		t.Errorf("expected the response to mention mode:\"summary\" as an available option since a fresh summary exists, got: %s", text)
	}
}

// TestHandleGetDefinition_ExplicitSummaryModeReturnsSummary confirms
// the #313 opt-in path itself still works: passing mode:"summary"
// explicitly on a def with a fresh cached summary returns the compact
// summary rendering, not the full body -- summary is no longer a
// silent default, but it's still a real, working choice when asked
// for directly.
func TestHandleGetDefinition_ExplicitSummaryModeReturnsSummary(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	d, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatalf("setup: Greet not found: %v", err)
	}
	if err := db.SetDefSummary(d.ID, &store.DefSummary{
		OneLine:  "Returns a greeting for the given name.",
		BodyHash: store.HashBodyStructural(d.Body),
		Model:    "test",
	}); err != nil {
		t.Fatalf("SetDefSummary: %v", err)
	}

	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet", Mode: "summary"})
	if err != nil {
		t.Fatalf("read mode:summary: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Returns a greeting for the given name.") {
		t.Errorf("expected explicit mode:\"summary\" to return the summary rendering, got:\n%s", text)
	}
	if strings.Contains(text, "return \"Hello, \"") {
		t.Errorf("expected the full body NOT to be present in an explicit summary-mode response, got:\n%s", text)
	}

	// A bare read (no mode:) on the SAME def must NOT return the
	// summary -- confirming summary really is opt-in only now, not a
	// silent default that just happened to be masked by the outline
	// threshold in the other test (Greet's body is small).
	bare, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	if err != nil {
		t.Fatalf("bare read: %v", err)
	}
	bareText := resultText(t, bare)
	if !strings.Contains(bareText, "return \"Hello, \"") {
		t.Errorf("expected a bare read to return the full body (summary is opt-in only), got:\n%s", bareText)
	}
}

// TestHandleTestByName_AlternationOfSamePackageTestsNarrowsScope guards
// the #313 fix: test:"TestA|TestB" already resolved each segment to a
// source file for emit-scoping purposes, but never used that
// resolution to narrow the actual `go test` target -- it stayed
// "./..." even when every resolved test lived in the same package.
// Confirmed via a real prometheus-19017 trajectory (four alternated
// test names, all in promql/, still ran across the whole repo).
func TestHandleTestByName_AlternationOfSamePackageTestsNarrowsScope(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha.go"), []byte(`package alpha

func Widget() bool { return true }
func Gadget() bool { return true }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha_test.go"), []byte(`package alpha

import "testing"

func TestWidget(t *testing.T) {
	if !Widget() {
		t.Fatal("false")
	}
}

func TestGadget(t *testing.T) {
	if !Gadget() {
		t.Fatal("false")
	}
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.handleTestByName(context.Background(), nil, "TestWidget|TestGadget", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "./alpha/...") {
		t.Errorf("expected the go test target to narrow to ./alpha/... since both alternated tests live there, got: %s", text)
	}
	if strings.Contains(text, "across ./...") {
		t.Errorf("expected scoping to avoid the whole-repo ./... fallback, got: %s", text)
	}
	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("expected both tests to pass, got: %s", text)
	}
}

// TestHandleTestByName_SubtestPatternInfersScopeFromTopLevelName guards
// the #313 fix: test:"TestWidget/some_subtest" (Go's -run syntax for
// targeting a t.Run subtest) failed testNamePattern's bare-identifier
// match (contains "/") and fell straight to the "./..." whole-repo
// scope even though "TestWidget" alone is a real, resolvable top-level
// test -- confirmed via a real prometheus-19017 trajectory
// (test:"TestEvaluations/testdata/start_timestamps.test").
func TestHandleTestByName_SubtestPatternInfersScopeFromTopLevelName(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "alpha"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha.go"), []byte(`package alpha

func Widget() bool { return true }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "alpha", "alpha_test.go"), []byte(`package alpha

import "testing"

func TestWidget(t *testing.T) {
	t.Run("some_subtest", func(t *testing.T) {
		if !Widget() {
			t.Fatal("false")
		}
	})
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	result, _, err := s.handleTestByName(context.Background(), nil, "TestWidget/some_subtest", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "./alpha/...") {
		t.Errorf("expected the go test target to be inferred as ./alpha/... from the subtest's top-level name, got: %s", text)
	}
	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("expected the subtest to pass, got: %s", text)
	}
}

func TestTruncateTestOutput_CollapsesNoTestsToRunEvenWhenItFillsTheHead(t *testing.T) {
	var out strings.Builder
	for i := 0; i < 100; i++ {
		out.WriteString("testing: warning: no tests to run\n")
		out.WriteString("PASS\n")
		out.WriteString(fmt.Sprintf("ok  \tgithub.com/example/pkg%d\t0.010s [no tests to run]\n", i))
	}
	out.WriteString("=== RUN   TestReal\n--- PASS: TestReal (0.00s)\nPASS\nok  \tgithub.com/example/real\t0.020s\n")
	if len(out.String()) <= testOutputCap {
		t.Fatalf("test fixture must exceed testOutputCap (%d) to exercise truncation; got %d bytes", testOutputCap, len(out.String()))
	}

	got := truncateTestOutput(out.String())
	if !strings.Contains(got, "TestReal") {
		t.Errorf("expected the real test's own output to survive truncation despite 100 leading noise blocks, got:\n%s", got)
	}
	if !strings.Contains(got, "100 other package(s) matched nothing") {
		t.Errorf("expected the noise blocks collapsed into a single count, got:\n%s", got)
	}
	if strings.Count(got, "no tests to run") > 1 {
		t.Errorf("expected \"no tests to run\" to appear at most once (in the collapsed summary), got %d times in:\n%s", strings.Count(got, "no tests to run"), got)
	}
}

// TestHandleOverview_CollapsesStructFieldsIntoParentTypeCount guards
// the #313 fix: a struct field is just a name+type, no logic -- it
// carries no standalone information an orientation view needs, yet
// each got its own full bullet line. Confirmed on a real
// prometheus-19184 trajectory: a config struct's dozen fields each
// produced a near-identical "(Config).X (field) — N callers, 0
// callees" bullet, consuming most of a single overview call's
// definition-cap budget on structurally uninteresting bulk. Fields
// now roll into their parent type's own line as a count instead.
func TestHandleOverview_CollapsesStructFieldsIntoParentTypeCount(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	body := `package main

type Config struct {
	Region     string
	AccessKey  string
	SecretKey  string
	RoleARN    string
	ExternalID string
}
`
	if err := os.WriteFile(filepath.Join(projDir, "config.go"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.IngestFile(db, projDir, filepath.Join(projDir, "config.go")); err != nil {
		t.Fatal("ingest config.go:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleOverview(context.Background(), nil, codeParam{File: "config.go"})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	text := resultText(t, result)

	if !strings.Contains(text, "Config (type)") {
		t.Errorf("expected Config's own line present, got:\n%s", text)
	}
	if !strings.Contains(text, "5 fields") {
		t.Errorf("expected Config's line to show a collapsed \"5 fields\" count, got:\n%s", text)
	}
	for _, field := range []string{"Region", "AccessKey", "SecretKey", "RoleARN", "ExternalID"} {
		if strings.Contains(text, "(Config)."+field+" (field)") {
			t.Errorf("expected field %q NOT individually listed, got:\n%s", field, text)
		}
	}
}

// TestHandleGetDefinition_DoesNotDuplicateDocAlreadyInBody guards the
// #313 fix: a definition's doc comment is part of its own body span
// (round-trip losslessness), so showing d.Doc as separate prose AND
// the raw body right after it duplicated the same text verbatim.
// Confirmed on a real prometheus-19338 trajectory across every
// full-body read/expand call in the corpus.
func TestHandleGetDefinition_DoesNotDuplicateDocAlreadyInBody(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	body := `package main

// Widget does a thing, verified by callers.
func Widget() string { return "widget" }
`
	if err := os.WriteFile(filepath.Join(projDir, "widget.go"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.IngestFile(db, projDir, filepath.Join(projDir, "widget.go")); err != nil {
		t.Fatal("ingest widget.go:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Widget"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := resultText(t, result)

	if strings.Count(text, "Widget does a thing, verified by callers.") != 1 {
		t.Errorf("expected the doc text to appear exactly once (in the body's own comment), got %d times in:\n%s",
			strings.Count(text, "Widget does a thing, verified by callers."), text)
	}
	if !strings.Contains(text, "// Widget does a thing") {
		t.Errorf("expected the doc comment still visible inside the code block, got:\n%s", text)
	}
}

// TestHandleCode_ReadWithLineRangeNoNameSuggestsReadFile guards a real
// bench finding: an agent tried op:"read", file:, line_range: with no
// name (wanting a line-range slice of a file, not one specific def) and
// got the generic "use op:\"overview\"" suggestion, which doesn't support
// line ranges -- a wasted retry, hit in 6 of 15 tasks in a real v8
// head-to-head run. read-file already supports exactly this; the error
// should point there instead.
func TestHandleCode_ReadWithLineRangeNoNameSuggestsReadFile(t *testing.T) {
	s := &server{backend: nil}
	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:        "read",
		File:      "promql/query_logger.go",
		LineRange: "25-50",
	})
	text := resultText(t, result)
	if !strings.Contains(text, `op:"read-file"`) {
		t.Errorf("expected a pointer to op:\"read-file\", got: %s", text)
	}
	if !strings.Contains(text, "promql/query_logger.go") || !strings.Contains(text, "25-50") {
		t.Errorf("expected the file and line_range echoed back, got: %s", text)
	}
}

// TestEmitAndBuildAgainst_TimeoutReportsTimedOutNotEmptyBuildFailed guards
// the fix for a real v8 bench finding: a build killed by buildTimeout
// returned an empty "BUILD FAILED:\n" with no diagnostic (out hadn't been
// written yet when the process was killed) -- indistinguishable from a
// real compile error, so the agent had no way to tell "this is slow" from
// "your edit is wrong" and burned repeated blind retries on the same edit
// (confirmed on a real prometheus-18712 trajectory: 3 empty BUILD FAILED
// messages in a row on cmd/prometheus/main.go). Mirrors
// TestHandleTestByName_GenuineTimeoutStillReportsTimedOut's pattern of
// overriding the package-level timeout var to force a fast, deterministic
// repro instead of relying on a genuinely slow build. Both buildTimeout
// and buildTimeoutEscalated are forced tiny here so the #327 escalated
// retry also times out -- this guards the "still slow even after
// escalation" path, not the (separate, happy-path) recovery.
func TestEmitAndBuildAgainst_TimeoutReportsTimedOutNotEmptyBuildFailed(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	origTimeout, origEscalated := buildTimeout, buildTimeoutEscalated
	buildTimeout = 1 * time.Nanosecond
	buildTimeoutEscalated = 1 * time.Nanosecond
	t.Cleanup(func() { buildTimeout, buildTimeoutEscalated = origTimeout, origEscalated })

	got := s.emitAndBuildAgainst(db, emit.Opts{TouchedFiles: []string{"main.go"}})
	if strings.Contains(got, "BUILD FAILED:\n\n") || strings.Contains(got, "BUILD FAILED:\n\n\n") {
		t.Errorf("expected the timeout to be distinguished from an empty BUILD FAILED, got:\n%q", got)
	}
	if !strings.Contains(got, "BUILD TIMED OUT") {
		t.Errorf("expected a killed-by-deadline build to report BUILD TIMED OUT, got:\n%q", got)
	}
	if strings.Contains(got, "DEFN_BUILD_TIMEOUT") {
		t.Errorf("expected the message to NOT suggest an env var the caller cannot set mid-session, got:\n%q", got)
	}
	if !strings.Contains(got, "apply") {
		t.Errorf("expected the message to suggest batching into apply instead of blind retry, got:\n%q", got)
	}
	if !s.buildSlowConfirmed.Load() {
		t.Errorf("expected buildSlowConfirmed to be set after a real timeout")
	}
}

// TestHandleExpand_AutoBatchBodyOverrideRespectsSizeThreshold guards a v9
// (sonnet) bench finding: BodyNames (used by the circuit-breaker
// auto-batch redirect) forced a name's FULL body regardless of size,
// even when a solo `read` on that same name would have auto-downgraded
// to outline via readAutoOutlineThreshold. trackReadShapedName tracks a
// name as "body wanted" on every op:"read" call, with no visibility into
// whether that read actually served full body or got downgraded -- so a
// large def read once (and correctly downgraded) got its full body
// forced back in by a later auto-batch purely because the name had been
// seen via op:"read". A real trajectory showed this balloon a single
// auto-batch response by ~24KB. The auto-batch path (bodyOverride) must
// respect the same size threshold a solo read would; a direct explicit
// expand(include:["body"]) call is a different, unaffected code path
// (bodyOverride is nil there) and should still force the full body
// regardless of size, since that's an explicit ask.
func TestHandleExpand_AutoBatchBodyOverrideRespectsSizeThreshold(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0o755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0o644)
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

	// Auto-batch shape: Include is outline+callers (no "body"), BodyNames
	// restricts which of the batched names get body forced in -- this is
	// exactly the shape handleCode's circuit-breaker redirect uses.
	autoBatch, _, err := s.handleExpand(context.Background(), nil, codeParam{
		Names:     []string{"BigFunc"},
		Include:   []string{"outline", "callers"},
		BodyNames: []string{"BigFunc"},
	})
	if err != nil {
		t.Fatalf("expand (auto-batch shape): %v", err)
	}
	autoText := resultText(t, autoBatch)
	if strings.Contains(autoText, "this is padding") {
		t.Errorf("auto-batch BodyNames override should respect the size threshold and NOT force full body for an oversized def; got: %s", autoText)
	}

	// Direct explicit expand(include:["body"]) call -- no BodyNames
	// override (bodyOverride is nil) -- must still force full body
	// regardless of size, since this is an explicit ask.
	direct, _, err := s.handleExpand(context.Background(), nil, codeParam{
		Name:    "BigFunc",
		Include: []string{"body"},
	})
	if err != nil {
		t.Fatalf("expand (direct explicit body): %v", err)
	}
	directText := resultText(t, direct)
	if !strings.Contains(directText, "this is padding") {
		t.Errorf("direct explicit expand(include:[\"body\"]) must still return full body regardless of size; got: %s", directText)
	}
}

// TestHandleTest_OnTestFunctionActuallyRunsIt tightens
// TestHandleTest_OnTestFunctionSuggestsTestParam: a v9 (sonnet) bench dig
// found this exact shape still costs one full wasted round-trip every
// time (4/4 sampled occurrences) -- the corrective note pointed at
// test:"X" but made the caller ask again instead of just running it,
// since the target IS the test the caller almost certainly wants. This
// asserts op:"test", name:"X" on a test function now actually executes
// it in the same call, not just describes how to.
func TestHandleTest_OnTestFunctionActuallyRunsIt(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	create, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:   "create",
		File: "main_test.go",
		Body: "func TestSomethingNewRuns(t *testing.T) {}",
	})
	if strings.Contains(resultText(t, create), "rolled back") {
		t.Fatalf("setup create failed: %s", resultText(t, create))
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "test", Name: "TestSomethingNewRuns"})
	text := resultText(t, result)
	if !strings.Contains(text, "ran it directly") {
		t.Errorf("expected the note to say it ran the test directly, got: %s", text)
	}
	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("expected the test to have actually executed and passed, got: %s", text)
	}
}

// TestHandleCode_EditRejectsImportPathAlias guards #298: import_path/
// alias are real codeParam fields, but only op:"add-import" reads them --
// the "edit" dispatch below constructs a narrower editParam that
// structurally can't carry them, so op:"edit" used to silently accept
// and drop both with a normal success response. A real prometheus-17395
// trajectory retried this 3 times before finding add-import. This must
// be rejected instead of silently no-opping.
func TestHandleCode_EditRejectsImportPathAlias(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:         "edit",
		Name:       "Greet",
		NewBody:    "func Greet(name string) string { return \"Hi, \" + name }",
		ImportPath: "fmt",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "import_path") || !strings.Contains(text, "add-import") {
		t.Fatalf("expected a rejection pointing at add-import, got: %s", text)
	}

	read, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Greet", Full: true})
	if strings.Contains(resultText(t, read), "Hi, ") {
		t.Errorf("rejected edit still landed its new_body: %s", resultText(t, read))
	}
}

// TestHandleApply_EditRejectsImportPathAlias is the apply-batched
// counterpart to TestHandleCode_EditRejectsImportPathAlias -- same gap,
// reached via handleApply's own "edit" case. A real prometheus-17395
// trajectory hit this specific path (call #173, inside an apply batch)
// after already failing twice on the standalone edit path.
func TestHandleApply_EditRejectsImportPathAlias(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{
				Op:         "edit",
				Name:       "Greet",
				NewBody:    "func Greet(name string) string { return \"Hi, \" + name }",
				ImportPath: "fmt",
			},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "import_path") || !strings.Contains(text, "add-import") {
		t.Fatalf("expected a rejection pointing at add-import, got: %s", text)
	}

	read, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Greet", Full: true})
	if strings.Contains(resultText(t, read), "Hi, ") {
		t.Errorf("rejected batched edit still landed its new_body: %s", resultText(t, read))
	}

	dryRun, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun: true,
		Operations: []applyOp{
			{Op: "edit", Name: "Greet", NewBody: "func Greet(name string) string { return name }", ImportPath: "fmt"},
		},
	})
	dryText := resultText(t, dryRun)
	if !strings.Contains(dryText, "import_path") || !strings.Contains(dryText, "add-import") {
		t.Fatalf("expected dry-run to also reject import_path/alias, got: %s", dryText)
	}
}

// TestBodyScanResult_EmptyMentionsGoDocForExternalDeps guards the fix
// for a repeated real bench pattern (prometheus-17395/12024/18765/19236):
// search only ever covers THIS project's own ingested source, never a
// third-party dependency's -- agents needing to check an imported SDK's
// symbol got a bare "no matches" and burned 5-8 blind Glob guesses
// against ~/go/pkg/mod. The empty-result message should point at
// `go doc` as the path-free way to resolve an external symbol instead.
func TestBodyScanResult_EmptyMentionsGoDocForExternalDeps(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.bodyScanResult("no-such-string-anywhere", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "go doc") {
		t.Errorf("expected a `go doc` hint for external/third-party symbols; got %q", text)
	}
}

// TestHandleApply_DeleteRefusesWhenCallersExist guards a real safety gap
// found auditing the same silent-drop bug class as import_path/alias:
// handleApply's "delete" case had NO #105 safe-delete check at all --
// unlike the singleton handleDelete, it deleted unconditionally
// regardless of live callers or the force field's value, silently
// behaving as if force:true were always set.
func TestHandleApply_DeleteRefusesWhenCallersExist(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Greet has a real caller (Farewell calls Greet) in setupTestDB's fixture.
	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "delete", Name: "Greet"}},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "refused") || !strings.Contains(text, "Farewell") {
		t.Fatalf("expected a caller-safety refusal naming Farewell, got: %s", text)
	}

	read, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Greet"})
	if strings.Contains(resultText(t, read), "not found") {
		t.Errorf("Greet should still exist after the refused batch delete, got: %s", resultText(t, read))
	}
}

func TestHandleApply_DeleteForceOverridesCallerSafety(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "delete", Name: "Greet", Force: true}},
	})
	text := resultText(t, result)
	if strings.Contains(text, "refused") {
		t.Fatalf("force:true should bypass the caller-safety refusal, got: %s", text)
	}
	// Greet's own caller (Farewell) still references it in source, so the
	// batch legitimately fails to BUILD afterward (a genuine compile
	// error, not the safety check this test targets) and rolls back --
	// that's expected apply-batch semantics, not a regression in this fix.
	if !strings.Contains(text, "undefined: Greet") {
		t.Fatalf("expected the deletion to proceed past the safety check into a real build failure (Farewell still calls Greet), got: %s", text)
	}
}

// TestHandleApply_DeleteDryRunRefusesWhenCallersExist is the dry-run
// counterpart -- the preview loop had the same missing check, so
// "- would delete Greet" never warned about Farewell's dependency.
func TestHandleApply_DeleteDryRunRefusesWhenCallersExist(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun:     true,
		Operations: []applyOp{{Op: "delete", Name: "Greet"}},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "refused") || !strings.Contains(text, "Farewell") {
		t.Fatalf("expected dry-run to also surface the caller-safety refusal naming Farewell, got: %s", text)
	}
	if strings.Contains(text, "would delete Greet") {
		t.Errorf("dry-run should not claim it would delete Greet when refused, got: %s", text)
	}
}

func TestLooksLikeExactIdentifier(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		{"NewRoundTripperFromConfig", true},
		{"DoRequest", true},
		{"lowercaseStart", false},
		{"%Auth%", false},
		{"has space", false},
		{"pkg/path", false},
		{"a.b.c", false},
		{"", false},
		{"_Underscore", false},
	}
	for _, tc := range tests {
		if got := looksLikeExactIdentifier(tc.pattern); got != tc.want {
			t.Errorf("looksLikeExactIdentifier(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

// TestHandleSearch_ExternalSymbolHintWhenNameMismatchButFTSNoise guards
// #299 followup: bodyScanResult's go-doc hint only fired on a hard
// zero-match, but a real prometheus-12024 trajectory hit the same
// external-dependency confusion behind NON-zero, name-mismatched FTS
// noise -- an exact PascalCase symbol from a vendored package returned
// unrelated body-text-coincidence hits with no indication those weren't
// a real answer, burning 9 blind Glob/Grep round-trips.
func TestHandleSearch_ExternalSymbolHintWhenNameMismatchButFTSNoise(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "extsym")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module extsym\n\ngo 1.26\n"), 0644)
	const src = `package extsym

// DoRequest builds a client, similar in spirit to NewRoundTripperFromConfig
// upstream, but implemented locally instead of importing it.
func DoRequest() error {
	return nil
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleSearch(context.Background(), nil, codeParam{Pattern: "NewRoundTripperFromConfig"})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "go doc") {
		t.Fatalf("expected the external-dependency hint since DoRequest's own name doesn't match the pattern, got: %s", text)
	}

	// Sanity: a pattern that DOES match a real def's name must not get
	// the external-dependency hint -- it's a genuine project-local hit.
	clean, _, err := s.handleSearch(context.Background(), nil, codeParam{Pattern: "DoRequest"})
	if err != nil {
		t.Fatal(err)
	}
	cleanText := resultText(t, clean)
	if strings.Contains(cleanText, "go doc") {
		t.Errorf("did not expect the external-dependency hint for a genuine name match, got: %s", cleanText)
	}
}

// TestHandleGetDefinition_SurfacesSiblingConstsInSameBlock guards #300:
// reading one member of a grouped const block (an enum-like family) used
// to show only that one value, with no indication siblings existed --
// a real prometheus-19338 trajectory pattern reused a generic existing
// constant instead of adding a new sibling matching house style, because
// nothing surfaced the family it belonged to.
func TestHandleGetDefinition_SurfacesSiblingConstsInSameBlock(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "constproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module constproj\n\ngo 1.26\n"), 0644)
	const src = `package constproj

type WarningCategory string

const (
	WarningCategoryOther WarningCategory = "other"
	WarningCategoryLabelNameCollision WarningCategory = "label_name_collision"
	WarningCategoryHistogramZeroCountNonZeroSum WarningCategory = "histogram_zero_count_non_zero_sum"
)

// Standalone, unrelated to the block above.
const StandaloneThing = "standalone"

func Use() WarningCategory {
	return WarningCategoryOther
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "WarningCategoryOther"})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Siblings in this block") {
		t.Fatalf("expected a siblings line, got: %s", text)
	}
	for _, want := range []string{"WarningCategoryLabelNameCollision", "WarningCategoryHistogramZeroCountNonZeroSum"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected sibling %s in output, got: %s", want, text)
		}
	}
	if strings.Contains(text, "StandaloneThing") {
		t.Errorf("standalone const outside the block must not appear as a sibling, got: %s", text)
	}

	// The standalone const itself must NOT report any siblings.
	standalone, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "StandaloneThing"})
	if err != nil {
		t.Fatal(err)
	}
	standaloneText := resultText(t, standalone)
	if strings.Contains(standaloneText, "Siblings in this block") {
		t.Errorf("standalone const should not report siblings, got: %s", standaloneText)
	}
}

// TestHandleCode_SiblingSpecsWorkForVarBlockAndOutlineOp checks the
// sibling-spec fix also covers grouped var blocks (not just const) and
// that op:"outline" (which redirects small bodies to the read view)
// surfaces the same siblings line.
func TestHandleCode_SiblingSpecsWorkForVarBlockAndOutlineOp(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "varproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module varproj\n\ngo 1.26\n"), 0644)
	const src = `package varproj

var (
	ErrNotFound = "not found"
	ErrTimeout  = "timeout"
)

func Use() string {
	return ErrNotFound
}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	for _, op := range []string{"read", "outline"} {
		result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: op, Name: "ErrNotFound"})
		if err != nil {
			t.Fatalf("op=%s: %v", op, err)
		}
		text := resultText(t, result)
		if !strings.Contains(text, "Siblings in this block") || !strings.Contains(text, "ErrTimeout") {
			t.Errorf("op=%s: expected ErrTimeout as a sibling, got: %s", op, text)
		}
	}
}

// TestHandleGetDefinition_SiblingsDoNotCrossIntoAdjacentBlock guards a
// real bug found reviewing the first version of this feature: inferring
// block membership from adjacent DB rows (same Kind + bare-spec body
// shape) couldn't distinguish two separate "const (...)" blocks of the
// same kind declared back-to-back -- a very ordinary Go pattern, not a
// contrived edge case -- and merged both blocks' names into one false
// family. The AST-based rewrite parses the real file to find the exact
// enclosing GenDecl instead of inferring it.
func TestHandleGetDefinition_SiblingsDoNotCrossIntoAdjacentBlock(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "twoblocks")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module twoblocks\n\ngo 1.26\n"), 0644)
	const src = `package twoblocks

const (
	StatusOK       = 200
	StatusNotFound = 404
)

const (
	Debug = "debug"
	Info  = "info"
)
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(src), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "StatusNotFound"})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "StatusOK") {
		t.Errorf("expected StatusOK (same block) as a sibling, got: %s", text)
	}
	if strings.Contains(text, "Debug") || strings.Contains(text, "Info") {
		t.Errorf("Debug/Info belong to a SEPARATE adjacent block and must not appear, got: %s", text)
	}
}

// TestHandleCode_StarterBundleFiresOnFirstReadNotJustSearchOrOverview
// guards #302: the one-shot starter bundle (a context()-shaped orient
// bundle, #203) was only wired to search/overview's dispatch cases.
// Measured across v10-v12 bench trajectories: it still fired 44/45
// tasks overall (a later search/overview call usually triggers it
// eventually), but for the ~29% of tasks whose FIRST move is read/
// outline/impact/expand instead, the bundle arrived a beat late --
// after those solo calls already happened. Widening the wiring costs
// nothing on later calls (appendStarter's own one-shot flag no-ops
// them) and lets it fire on whichever op genuinely comes first.
func TestHandleCode_StarterBundleFiresOnFirstReadNotJustSearchOrOverview(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
	s.ready.Store(true)
	req := &sdkmcp.CallToolRequest{Session: &sdkmcp.ServerSession{}}

	for _, tc := range []struct {
		op   string
		args codeParam
	}{
		{"read", codeParam{Op: "read", Name: "Greet"}},
		{"outline", codeParam{Op: "outline", Name: "main"}},
		{"impact", codeParam{Op: "impact", Name: "Farewell"}},
		{"expand", codeParam{Op: "expand", Name: "Greet"}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			s2 := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
			s2.ready.Store(true)
			req2 := &sdkmcp.CallToolRequest{Session: &sdkmcp.ServerSession{}}
			r, _, err := s2.handleCode(context.Background(), req2, tc.args)
			if err != nil {
				t.Fatalf("op=%s: %v", tc.op, err)
			}
			text := resultText(t, r)
			if !strings.Contains(text, "starter bundle") {
				t.Errorf("op=%s: expected the one-shot starter bundle to fire on the FIRST call, got: %s", tc.op, text)
			}
		})
	}

	// Sanity: a second call of any kind in the SAME session must not
	// fire it again (the one-shot flag is per-session, not per-op).
	first, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Greet"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(t, first), "starter bundle") {
		t.Fatalf("expected the bundle on the first call of this session, got: %s", resultText(t, first))
	}
	second, _, err := s.handleCode(context.Background(), req, codeParam{Op: "outline", Name: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resultText(t, second), "starter bundle") {
		t.Errorf("the one-shot bundle must not fire twice in one session, got: %s", resultText(t, second))
	}
}

// TestHandleCode_StarterBundleDoesNotPoisonDedupHash guards #303: the
// one-shot starter bundle used to be appended BEFORE dedup hashed the
// response text, so the very first call of a session (which is exactly
// when the bundle fires) stored a hash that included bundle text no
// later call would ever reproduce -- permanently missing its own dedup
// entry for the rest of the session. A second identical read of the
// SAME def, later, must still hit the dedup stub.
func TestHandleCode_StarterBundleDoesNotPoisonDedupHash(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
	s.ready.Store(true)
	req := &sdkmcp.CallToolRequest{Session: &sdkmcp.ServerSession{}}

	first, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Greet"})
	if err != nil {
		t.Fatal(err)
	}
	firstText := resultText(t, first)
	if !strings.Contains(firstText, "starter bundle") {
		t.Fatalf("expected the starter bundle on the session's first call, got: %s", firstText)
	}

	second, _, err := s.handleCode(context.Background(), req, codeParam{Op: "read", Name: "Greet"})
	if err != nil {
		t.Fatal(err)
	}
	secondText := resultText(t, second)
	if !strings.Contains(secondText, "cached") {
		t.Fatalf("expected an identical repeat read to hit the dedup stub, got: %s", secondText)
	}
}

// TestHandleApply_RenameTypeUpdatesMethodReceiversAcrossFiles is the
// apply-batched counterpart to TestHandleRename_TypeWithMethodsSplitAcrossFiles
// -- handleApply's own "rename" case never had the #148-class
// sibling-method-receiver fix ported to it, so an apply-batch rename of
// any type with methods declared in other files left stale receiver
// clauses behind. Since apply always builds when a file is touched,
// those stale receivers are a compile error that rolls back the WHOLE
// batch -- an apply-batch rename of any type with methods failed
// unconditionally, where the singleton path succeeded correctly.
func TestHandleApply_RenameTypeUpdatesMethodReceiversAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(filepath.Join(projDir, "splitm"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nimport \"proj/splitm\"\n\nfunc main() {\n\tw := splitm.Widget{N: 1}\n\t_ = w.MethodA()\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "splitm", "types.go"), []byte("package splitm\n\ntype Widget struct {\n\tN int\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "splitm", "a.go"), []byte("package splitm\n\nfunc (w *Widget) MethodA() int {\n\treturn w.N\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "splitm", "b.go"), []byte("package splitm\n\nfunc (w *Widget) MethodB() int {\n\treturn w.N * 2\n}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "rename", Name: "Widget", NewName: "WidgetR0"}},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("apply rename failed: %s", text)
	}
	if !strings.Contains(text, "method receiver") {
		t.Errorf("expected the report to mention updating method receivers, got: %s", text)
	}

	for _, f := range []string{"a.go", "b.go"} {
		src, err := os.ReadFile(filepath.Join(projDir, "splitm", f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "Widget)") && !strings.Contains(string(src), "WidgetR0)") {
			t.Errorf("%s still has a stale receiver clause after apply rename:\n%s", f, src)
		}
		if !strings.Contains(string(src), "WidgetR0") {
			t.Errorf("%s missing the renamed receiver:\n%s", f, src)
		}
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed after apply rename:\n%s", out)
	}
}

// TestHandleApply_RenameQualifiedOldNameActuallyRewritesBodyAndCallers
// guards #305: handleApply's rename case used op.Name (which may be the
// receiver-qualified "(*T).Method" form resolveApplyTarget/
// GetDefinitionByName accept) directly in astRename calls instead of
// the resolved bare d.Name -- astRename never matches a real *ast.Ident
// against that qualified string, so the def's own body and every
// caller's body silently kept the OLD identifier even though
// RenameDefinition updated the DB's Name column to the new one. Same
// bug class handleRename's own oldBareName fix guards against.
func TestHandleApply_RenameQualifiedOldNameActuallyRewritesBodyAndCallers(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "qualproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module qualproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package qualproj

type Widget struct{ N int }

func (w *Widget) OldMethod() int {
	return w.N
}

func Use(w *Widget) int {
	return w.OldMethod()
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	// Qualified "(*Widget).OldMethod" form, no receiver: set separately --
	// resolveApplyTarget/GetDefinitionByName accept this convention.
	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "rename", Name: "(*Widget).OldMethod", NewName: "NewMethod"}},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("apply rename failed: %s", text)
	}

	src, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(src)
	if strings.Contains(got, "OldMethod") {
		t.Errorf("OldMethod should be fully renamed (def body + caller), got:\n%s", got)
	}
	if !strings.Contains(got, "func (w *Widget) NewMethod() int") || !strings.Contains(got, "w.NewMethod()") {
		t.Errorf("expected both the method decl and its caller to use NewMethod, got:\n%s", got)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed after apply rename:\n%s", out)
	}
}

// TestHandleApply_CreateSingleDeclNewNestedPackageGetsOwnModule guards
// #306: apply's single-decl "create" branch was missing the #13-style
// new-package EnsureModule fallback entirely (present in handleCreate,
// handleCreateMultiDecl, AND apply's own multi-decl create branch) --
// it hard-errored "file does not map to any known module" instead,
// so a single-op create targeting a brand-new package inside an apply
// batch failed outright while the identical request via standalone
// code(op:"create") or apply's multi-decl branch succeeded.
func TestHandleApply_CreateSingleDeclNewNestedPackageGetsOwnModule(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "create", Body: "func NewSingleFunc() int { return 1 }", File: "pkg/newthing/file.go"}},
	})
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected new-package apply create to succeed, got error: %s", text)
	}
	if strings.Contains(text, "Errors") || strings.Contains(text, "does not map to any known module") {
		t.Fatalf("expected no errors, got: %s", text)
	}

	defs, err := db.FindDefinitions("NewSingleFunc")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected exactly 1 NewSingleFunc, got %d", len(defs))
	}

	mods, err := db.ListModules()
	if err != nil {
		t.Fatal(err)
	}
	var testprojID int64
	for _, m := range mods {
		if m.Path == "testproj" {
			testprojID = m.ID
		}
	}
	if defs[0].ModuleID == testprojID {
		t.Errorf("new package's def must not share ModuleID with the unrelated pre-existing testproj module")
	}
}

// TestHandleApply_MoveRelocatesToTargetModuleDirectory is the
// apply-batch counterpart to TestHandleMove_RelocatesToTargetModuleDirectory
// -- #307: handleApply had no "move" op at all (a confirmed capability
// gap, not a bug: an agent could never batch a move with other
// operations in one atomic transaction). Same fixture, same assertions,
// through code(op:"apply") instead of the standalone op:"move".
func TestHandleApply_MoveRelocatesToTargetModuleDirectory(t *testing.T) {
	s, projDir, _ := setupMoveTestProject(t)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "move", Name: "Bar", Module: "other"}},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "WARNING") || strings.Contains(text, "Errors") {
		t.Fatalf("apply move reported failure/warning: %s", text)
	}

	subSrc, err := os.ReadFile(filepath.Join(projDir, "sub", "sub.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(subSrc), "func Bar") {
		t.Errorf("sub/sub.go still contains Bar after apply move:\n%s", subSrc)
	}
	if !strings.Contains(string(subSrc), "func Foo") {
		t.Errorf("sub/sub.go lost unrelated Foo after apply move:\n%s", subSrc)
	}

	relocated := filepath.Join(projDir, "other", "sub.go")
	newSrc, err := os.ReadFile(relocated)
	if err != nil {
		t.Fatalf("expected %s to exist after apply move: %v", relocated, err)
	}
	if !strings.Contains(string(newSrc), "package other") {
		t.Errorf("relocated file has wrong package clause:\n%s", newSrc)
	}
	if !strings.Contains(string(newSrc), "func Bar() int") {
		t.Errorf("relocated file missing Bar's body:\n%s", newSrc)
	}

	goBuild(t, projDir)
}

// TestHandleApply_MoveBatchedWithEditLandsAtomically is the actual
// point of #307: an agent can now batch a move together with an
// unrelated edit in one atomic transaction, instead of needing two
// separate round-trips with no shared rollback protection.
func TestHandleApply_MoveBatchedWithEditLandsAtomically(t *testing.T) {
	s, projDir, _ := setupMoveTestProject(t)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "move", Name: "Bar", Module: "other"},
			{Op: "edit", Name: "Baz", NewBody: "func Baz() { println(\"baz\") }"},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "Errors") {
		t.Fatalf("batched move+edit failed: %s", text)
	}

	relocated := filepath.Join(projDir, "other", "sub.go")
	if _, err := os.Stat(relocated); err != nil {
		t.Fatalf("expected %s to exist after batched move: %v", relocated, err)
	}
	otherSrc, err := os.ReadFile(filepath.Join(projDir, "other", "other.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(otherSrc), `println("baz")`) {
		t.Errorf("expected Baz's edit to also land in the same batch:\n%s", otherSrc)
	}

	goBuild(t, projDir)
}

// TestHandleApply_MoveRequiresModuleAndTargetMustExist covers apply's
// move validation errors, both in dry-run and live, mirroring
// handleMove's own error shapes.
func TestHandleApply_MoveRequiresModuleAndTargetMustExist(t *testing.T) {
	s, _, _ := setupMoveTestProject(t)

	// module: is required.
	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "move", Name: "Bar"}},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "module is required") {
		t.Errorf("expected a module-required error, got: %s", text)
	}

	// Target module must exist.
	result2, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "move", Name: "Bar", Module: "nonexistent"}},
	})
	text2 := resultText(t, result2)
	if !strings.Contains(text2, "not found") {
		t.Errorf("expected a target-module-not-found error, got: %s", text2)
	}

	// Same validation in dry-run.
	dryRun, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun:     true,
		Operations: []applyOp{{Op: "move", Name: "Bar", Module: "other"}},
	})
	dryText := resultText(t, dryRun)
	if !strings.Contains(dryText, "would move Bar to other") {
		t.Errorf("expected a dry-run preview of the move, got: %s", dryText)
	}
}

// TestHandleApply_DryRunProjectionOpsActuallyValidate guards #308:
// apply's dry-run for the projEdit-shared group (insert-precondition/
// replace-slice/replace-hunk/wrap-in-defer/rename-param) only checked
// resolution and kind support, never actually running the real
// projection function -- so a dry-run could report a clean "~ would
// wrap-in-defer on Greet" preview for a request whose real call fails
// outright (e.g. an out-of-range stmt_index).
func TestHandleApply_DryRunProjectionOpsActuallyValidate(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun:     true,
		Operations: []applyOp{{Op: "wrap-in-defer", Name: "Greet", StmtIndex: 9999, DeferBody: "cleanup()"}},
	})
	text := resultText(t, result)
	if strings.Contains(text, "would wrap-in-defer") {
		t.Fatalf("expected dry-run to catch the out-of-range stmt_index instead of a false-positive preview: %s", text)
	}
	if !strings.Contains(text, "Errors") {
		t.Errorf("expected an error for the invalid stmt_index, got: %s", text)
	}
}

// TestHandleApply_DryRunAddImportValidatesFileResolution guards #308:
// apply's add-import dry-run only checked import_path != "", skipping
// the file/module resolution the real call performs -- so a dry-run
// could falsely report success for a file with no defs at all, where
// the real call would immediately error with "no defs in ...".
func TestHandleApply_DryRunAddImportValidatesFileResolution(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun:     true,
		Operations: []applyOp{{Op: "add-import", ImportPath: "fmt", File: "nonexistent.go"}},
	})
	text := resultText(t, result)
	if strings.Contains(text, "would add import") {
		t.Fatalf("expected dry-run to catch the unresolvable file instead of a false-positive preview: %s", text)
	}
	if !strings.Contains(text, "no defs in") {
		t.Errorf("expected a 'no defs in' error, got: %s", text)
	}
}

// TestHandleApply_InsertAppliesAnchorSplice is the apply-batched
// counterpart to TestHandleInsert -- #309: handleApply had no "insert"
// case at all, so an agent that wanted to splice text after an anchor
// inside a batch had to fall back to a separate handleInsert round-trip
// with no shared rollback protection against the rest of the batch.
func TestHandleApply_InsertAppliesAnchorSplice(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "insert", Name: "Greet", After: "Hello", Body: " there"},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "Errors") {
		t.Fatalf("insert failed: %s", text)
	}

	d, _ := db.GetDefinitionByName("Greet", "")
	if !strings.Contains(d.Body, "Hello there") {
		t.Errorf("insert not applied, got body: %s", d.Body)
	}
}

// TestHandleApply_InsertBatchedWithEditLandsAtomically proves the
// actual point of #309: insert can now be batched with an unrelated
// edit in one atomic transaction.
func TestHandleApply_InsertBatchedWithEditLandsAtomically(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "insert", Name: "Greet", After: "Hello", Body: " there"},
			{Op: "edit", Name: "Farewell", NewBody: `func Farewell(name string) string {
	return Greet(name) + " and adieu"
}`},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "Errors") {
		t.Fatalf("batched insert+edit failed: %s", text)
	}

	greet, _ := db.GetDefinitionByName("Greet", "")
	if !strings.Contains(greet.Body, "Hello there") {
		t.Errorf("expected Greet's insert to land, got: %s", greet.Body)
	}
	farewell, _ := db.GetDefinitionByName("Farewell", "")
	if !strings.Contains(farewell.Body, "and adieu") {
		t.Errorf("expected Farewell's edit to also land in the same batch, got: %s", farewell.Body)
	}
}

// TestHandleApply_InsertDryRunCatchesBadAnchor guards the same #308-class
// gap for insert: dry-run must actually locate the anchor and validate
// the resulting syntax, not just resolve the target and report a
// false-positive "would insert" preview.
func TestHandleApply_InsertDryRunCatchesBadAnchor(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun:     true,
		Operations: []applyOp{{Op: "insert", Name: "Greet", After: "NOPE_ANCHOR_XYZ", Body: " there"}},
	})
	text := resultText(t, result)
	if strings.Contains(text, "would insert") {
		t.Fatalf("expected dry-run to catch the missing anchor instead of a false-positive preview: %s", text)
	}
	if !strings.Contains(text, "Errors") {
		t.Errorf("expected an error for the missing anchor, got: %s", text)
	}
}

// TestHandleApply_RetargetFieldValueBatchedWithEditLandsAtomically proves
// the actual point of #309: retarget-field-value can now be batched with
// an unrelated edit in one atomic transaction.
func TestHandleApply_RetargetFieldValueBatchedWithEditLandsAtomically(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "retarget-field-value", Name: "NoSuchType", Field: "X", Old: "a", New: "b"},
			{Op: "edit", Name: "Farewell", NewBody: `func Farewell(name string) string {
	return Greet(name) + " and adieu"
}`},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "Errors") {
		t.Fatalf("batched retarget-field-value+edit failed: %s", text)
	}
	if !strings.Contains(text, "0 def(s)") {
		t.Errorf("expected '0 def(s)' for a type with no matches, got %q", text)
	}

	farewell, _ := db.GetDefinitionByName("Farewell", "")
	if !strings.Contains(farewell.Body, "and adieu") {
		t.Errorf("expected Farewell's edit to also land in the same batch, got: %s", farewell.Body)
	}
}

// TestHandleApply_RetargetFieldValueDryRunRequiresFieldAndValue guards
// the same #308-class gap for retarget-field-value: dry-run must
// actually validate name/field/old/new instead of always reporting a
// clean preview regardless of the request's validity.
func TestHandleApply_RetargetFieldValueDryRunRequiresFieldAndValue(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun:     true,
		Operations: []applyOp{{Op: "retarget-field-value", Name: "Claim"}},
	})
	text := resultText(t, result)
	if strings.Contains(text, "would retarget") {
		t.Fatalf("expected dry-run to reject missing field/old/new instead of a false-positive preview: %s", text)
	}
	if !strings.Contains(text, "Errors") {
		t.Errorf("expected an error for the missing field, got: %s", text)
	}
}

// TestHandleApply_RetargetFieldValueRewritesMatchingComposites is the
// apply-batched counterpart to
// TestHandleRetargetFieldValue_RewritesMatchingComposites -- #309:
// handleApply had no "retarget-field-value" case at all.
func TestHandleApply_RetargetFieldValueRewritesMatchingComposites(t *testing.T) {
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
var C2 = Claim{Subject: "s2", Object: "OldTarget"}
var C3 = Claim{Subject: "s3", Object: "Different"}

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

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "retarget-field-value", Name: "Claim", Field: "Object", Old: "OldTarget", New: "NewTarget"},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "Errors") {
		t.Fatalf("retarget-field-value failed: %s", text)
	}
	if !strings.Contains(text, "2 def(s)") {
		t.Errorf("expected '2 def(s)' updated, got %q", text)
	}

	c1, _ := db.GetDefinitionByName("C1", "")
	if !strings.Contains(c1.Body, "NewTarget") || strings.Contains(c1.Body, "OldTarget") {
		t.Errorf("C1 not retargeted: %s", c1.Body)
	}
	c3, _ := db.GetDefinitionByName("C3", "")
	if !strings.Contains(c3.Body, "Different") {
		t.Errorf("C3 should be untouched: %s", c3.Body)
	}
}

// TestHandleApply_PatchBatchedWithEditLandsAtomically proves the actual
// point of #310: patch can now be batched with an unrelated edit in one
// atomic transaction.
func TestHandleApply_PatchBatchedWithEditLandsAtomically(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "patch", Name: "Greet", OldName: "Hello", NewName: "Hi"},
			{Op: "edit", Name: "Farewell", NewBody: `func Farewell(name string) string {
	return Greet(name) + " and adieu"
}`},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "Errors") {
		t.Fatalf("batched patch+edit failed: %s", text)
	}

	greet, _ := db.GetDefinitionByName("Greet", "")
	if !strings.Contains(greet.Body, `"Hi, "`) {
		t.Errorf("expected Greet's patch to land, got: %s", greet.Body)
	}
	farewell, _ := db.GetDefinitionByName("Farewell", "")
	if !strings.Contains(farewell.Body, "and adieu") {
		t.Errorf("expected Farewell's edit to also land in the same batch, got: %s", farewell.Body)
	}
}

// TestHandleApply_PatchDryRunCatchesMissingOldText guards the same
// #308-class gap for patch: dry-run must actually check the old text
// exists in the body, not just resolve the target and report a
// false-positive "would patch" preview.
func TestHandleApply_PatchDryRunCatchesMissingOldText(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun:     true,
		Operations: []applyOp{{Op: "patch", Name: "Greet", OldName: "NOPE_NOT_IN_BODY", NewName: "Hi"}},
	})
	text := resultText(t, result)
	if strings.Contains(text, "would patch") {
		t.Fatalf("expected dry-run to catch the missing old text instead of a false-positive preview: %s", text)
	}
	if !strings.Contains(text, "Errors") {
		t.Errorf("expected an error for the missing old text, got: %s", text)
	}
}

// TestHandleApply_PatchReplacesFirstOccurrence is the apply-batched
// counterpart to handlePatch's own basic replace path -- #310:
// handleApply had no "patch" case at all. Fits the same projEdit
// pattern used for insert (#309).
func TestHandleApply_PatchReplacesFirstOccurrence(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "patch", Name: "Greet", OldName: "Hello", NewName: "Hi"},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "Errors") {
		t.Fatalf("patch failed: %s", text)
	}

	d, _ := db.GetDefinitionByName("Greet", "")
	if !strings.Contains(d.Body, `"Hi, "`) {
		t.Errorf("patch not applied, got body: %s", d.Body)
	}
}

// TestCollapseAllPassLines covers #311's collapse of a long all-pass
// test run: many PASS lines (including indented subtests) with zero
// failures collapse to one count line; any failure, or too few PASS
// lines to matter, must stay untouched.
func TestCollapseAllPassLines(t *testing.T) {
	var manyPass []string
	for i := 0; i < 25; i++ {
		manyPass = append(manyPass, fmt.Sprintf("--- PASS: TestFoo_%d (0.00s)", i))
	}
	manyPass = append(manyPass, "    --- PASS: TestFoo_0/sub (0.00s)")
	manyPass = append(manyPass, "PASS")
	manyPass = append(manyPass, "ok  \tpkg\t0.500s")

	got := collapseAllPassLines(manyPass)
	gotText := strings.Join(got, "|")
	if strings.Contains(gotText, "TestFoo_0 ") {
		t.Errorf("expected individual PASS names to be collapsed, got: %s", gotText)
	}
	if !strings.Contains(gotText, "26 tests, all passed") {
		t.Errorf("expected a collapsed count of 26 (25 top-level + 1 subtest), got: %s", gotText)
	}
	if !strings.Contains(gotText, "ok  \tpkg\t0.500s") {
		t.Errorf("expected package result line to survive collapse, got: %s", gotText)
	}

	var withFail []string
	for i := 0; i < 25; i++ {
		withFail = append(withFail, fmt.Sprintf("--- PASS: TestFoo_%d (0.00s)", i))
	}
	withFail = append(withFail, "--- FAIL: TestFoo_25 (0.01s)")
	gotFail := collapseAllPassLines(withFail)
	if strings.Join(gotFail, "|") != strings.Join(withFail, "|") {
		t.Errorf("a run with any failure must not be collapsed, got: %s", strings.Join(gotFail, "|"))
	}

	var fewPass []string
	for i := 0; i < 5; i++ {
		fewPass = append(fewPass, fmt.Sprintf("--- PASS: TestFoo_%d (0.00s)", i))
	}
	gotFew := collapseAllPassLines(fewPass)
	if strings.Join(gotFew, "|") != strings.Join(fewPass, "|") {
		t.Errorf("too few PASS lines to bother collapsing must stay untouched, got: %s", strings.Join(gotFew, "|"))
	}
}

// TestHandleCode_ImpactWithFileNoNameStillErrors guards the scope of
// #314's fix: impact/similar have no whole-file interpretation (unlike
// read/outline), so file:+no name must still be a plain error pointing
// at overview, not a redirect.
func TestHandleCode_ImpactWithFileNoNameStillErrors(t *testing.T) {
	s := &server{backend: nil}
	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "impact", File: "main.go"})
	text := resultText(t, result)
	if !strings.Contains(text, "name is required") {
		t.Errorf("expected impact with file, no name to still error, got: %s", text)
	}
	if !strings.Contains(text, `op:"overview"`) {
		t.Errorf("expected the error to still point at op:\"overview\", got: %s", text)
	}
}

// TestHandleCode_OutlineWithFileNoNameRedirectsToOverview is #314: a
// real v12 bench trajectory pattern -- an agent calls outline(file:)
// with no name, wanting to see everything in that file, and used to
// just get told to call op:"overview" separately instead. Confirmed
// hitting 16 times across 11 of 15 tasks in that corpus, always the
// same wasted round-trip shape. overview(file:) already computes
// exactly what was wanted, so this redirects instead of erroring.
func TestHandleCode_OutlineWithFileNoNameRedirectsToOverview(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "outline", File: "main.go"})
	if err != nil {
		t.Fatalf("outline with file, no name: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "name is required") {
		t.Fatalf("expected a redirect to overview instead of an error, got: %s", text)
	}
	for _, name := range []string{"Greet", "Farewell", "main"} {
		if !strings.Contains(text, name) {
			t.Errorf("expected the overview redirect to list %s, got: %s", name, text)
		}
	}
	if !strings.Contains(text, "showing every def") {
		t.Errorf("expected a note explaining the redirect, got: %s", text)
	}
}

// TestHandleCode_ReadWithFileNoNameRedirectsToOverview is read's
// counterpart to the outline redirect above.
func TestHandleCode_ReadWithFileNoNameRedirectsToOverview(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "read", File: "main.go"})
	if err != nil {
		t.Fatalf("read with file, no name: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "name is required") {
		t.Fatalf("expected a redirect to overview instead of an error, got: %s", text)
	}
	for _, name := range []string{"Greet", "Farewell", "main"} {
		if !strings.Contains(text, name) {
			t.Errorf("expected the overview redirect to list %s, got: %s", name, text)
		}
	}
}

// TestHandleCreate_TrulyUnparseableBodyGetsGenericMessage is the
// control for #315: garbage that ISN'T a var/const block entry either
// must keep the original generic message, not the new specific hint.
func TestHandleCreate_TrulyUnparseableBodyGetsGenericMessage(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		File: "main.go",
		Body: `this is not valid go at all {{{`,
	})
	text := resultText(t, result)
	if strings.Contains(text, "var/const block") {
		t.Errorf("expected the generic message for truly unparseable input, got the var/const hint instead: %s", text)
	}
	if !strings.Contains(text, "make sure it starts with func/type/const/var") {
		t.Errorf("expected the generic message, got: %s", text)
	}
}

// TestHandleCreate_VarBlockEntryGetsSpecificHint is #315: a body like
// "Foo = fmt.Errorf(...)" (valid only inside an existing var/const
// block, never standalone) used to fail inferFromBody's parse with no
// hint at why, giving the generic "make sure it starts with
// func/type/const/var" message even though the body DOES start with
// neither by design (it's meant to be a block entry). Confirmed via a
// real prometheus-18534 trajectory.
func TestHandleCreate_VarBlockEntryGetsSpecificHint(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		File: "main.go",
		Body: `SomeSentinelError = fmt.Errorf("boom")`,
	})
	text := resultText(t, result)
	if !strings.Contains(text, "var/const block") {
		t.Errorf("expected the var/const block hint, got: %s", text)
	}
	if !strings.Contains(text, `"var "`) || !strings.Contains(text, `"const "`) {
		t.Errorf("expected both var and const prefix suggestions, got: %s", text)
	}
}

// TestHandleApply_CreateVarBlockEntryGetsSpecificHint is #315's apply
// counterpart, covering both the real and dry-run create cases -- both
// call inferFromBody independently, the same class of singleton-vs-
// apply divergence risk found repeatedly elsewhere in apply this
// session, so both need the shared inferFailureHint wired in.
func TestHandleApply_CreateVarBlockEntryGetsSpecificHint(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	body := `SomeSentinelError = fmt.Errorf("boom")`

	dryRun, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun:     true,
		Operations: []applyOp{{Op: "create", File: "main.go", Body: body}},
	})
	dryText := resultText(t, dryRun)
	if !strings.Contains(dryText, "var/const block") {
		t.Errorf("dry-run: expected the var/const block hint, got: %s", dryText)
	}

	real, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "create", File: "main.go", Body: body}},
	})
	realText := resultText(t, real)
	if !strings.Contains(realText, "var/const block") {
		t.Errorf("real: expected the var/const block hint, got: %s", realText)
	}
}

// TestEmitAndBuildAgainst_BuildSlowConfirmedSkipsWastedFirstAttempt guards
// the session-level memoization half of #327: once buildSlowConfirmed is
// set, later builds start at buildTimeoutEscalated directly rather than
// paying the short buildTimeout's wait again first. Proven here by making
// buildTimeoutEscalated the SHORT one and buildTimeout the generous one --
// if firstTimeout selection ignored buildSlowConfirmed, this build would
// succeed (using the generous buildTimeout); since it's expected to
// exercise the tiny escalated value instead, it must time out.
func TestEmitAndBuildAgainst_BuildSlowConfirmedSkipsWastedFirstAttempt(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.buildSlowConfirmed.Store(true)

	origTimeout, origEscalated := buildTimeout, buildTimeoutEscalated
	buildTimeout = origTimeout                  // generous -- must NOT be the one used
	buildTimeoutEscalated = 1 * time.Nanosecond // tiny -- must be the one actually used
	t.Cleanup(func() { buildTimeout, buildTimeoutEscalated = origTimeout, origEscalated })

	got := s.emitAndBuildAgainst(db, emit.Opts{TouchedFiles: []string{"main.go"}})
	if !strings.Contains(got, "BUILD TIMED OUT") {
		t.Errorf("expected firstTimeout to use buildTimeoutEscalated when buildSlowConfirmed is set, got:\n%q", got)
	}
}

// TestEmitAndBuildAgainst_EscalatedRetrySucceedsOnSlowButValidBuild guards
// the actual #327 fix: a build that's merely slow (not broken) should
// succeed via the automatic escalated retry within the same call, instead
// of rolling back forever at a fixed 30s ceiling regardless of
// correctness (the real failure mode on the prometheus-18712 trajectory).
func TestEmitAndBuildAgainst_EscalatedRetrySucceedsOnSlowButValidBuild(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	origTimeout, origEscalated := buildTimeout, buildTimeoutEscalated
	buildTimeout = 1 * time.Nanosecond    // forces the first attempt to time out
	buildTimeoutEscalated = origEscalated // generous default -- the trivial build finishes well within it
	t.Cleanup(func() { buildTimeout, buildTimeoutEscalated = origTimeout, origEscalated })

	got := s.emitAndBuildAgainst(db, emit.Opts{TouchedFiles: []string{"main.go"}})
	if got != "" {
		t.Errorf("expected the escalated retry to succeed on a valid build with no error, got:\n%q", got)
	}
	if !s.buildSlowConfirmed.Load() {
		t.Errorf("expected buildSlowConfirmed to be set even though the retry ultimately succeeded")
	}
}

func TestHandleTest_NoTestsMatchedHeaderDoesNotClaimTestsRan(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "sub"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "root.go"), []byte("package testproj\n\nfunc RootFunc() string { return \"root\" }\n\nfunc unrelatedMarker() string { return \"unrelated\" }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "root_test.go"), []byte("package testproj\n\nimport \"testing\"\n\nfunc TestUnrelated(t *testing.T) {\n\tif unrelatedMarker() == \"\" {\n\t\tt.Fatal(\"root-unrelated-marker\")\n\t}\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "sub", "sub_test.go"), []byte("package sub\n\nimport (\n\t\"testing\"\n\n\t\"testproj\"\n)\n\nfunc TestSubFunc(t *testing.T) {\n\tif testproj.RootFunc() == \"\" {\n\t\tt.Fatal(\"sub-package-marker\")\n\t}\n}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	// Simulate a stale DB: the covering test is renamed on disk (e.g. an
	// external edit, or a sibling session, that hasn't synced yet) after
	// ingest already captured it as "TestSubFunc". This is now the only
	// realistic way to reach "none ran" for handleTest -- a package-scope
	// mismatch no longer produces it, since handleTest now scopes to
	// every package that actually houses a covering test, not just the
	// edited def's own package (see
	// TestHandleTest_CoveringTestInDifferentPackageStillRuns).
	os.WriteFile(filepath.Join(projDir, "sub", "sub_test.go"), []byte("package sub\n\nimport (\n\t\"testing\"\n\n\t\"testproj\"\n)\n\nfunc TestSubFuncRenamed(t *testing.T) {\n\tif testproj.RootFunc() == \"\" {\n\t\tt.Fatal(\"sub-package-marker\")\n\t}\n}\n"), 0644)

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleTest(context.Background(), nil, nameParam{Name: "RootFunc"})
	if err != nil {
		t.Fatalf("handleTest(RootFunc): %v", err)
	}
	text := resultText(t, result)
	t.Logf("output:\n%s", text)

	if !strings.Contains(text, "NO TESTS MATCHED") {
		t.Fatalf("test setup didn't produce the intended no-match scenario (covering test must live outside this def's own-package scope), got: %s", text)
	}
	if strings.HasPrefix(text, "Running") {
		t.Errorf("header still asserts a definite \"Running N of N tests\" count even though nothing matched -- must say none ran instead, got: %s", text)
	}
	if !strings.Contains(text, "but none ran") {
		t.Errorf("expected the header to say tests were attempted but none ran, got: %s", text)
	}
}

// TestHandleTestByName_DoesNotRestoreDivergedSiblingInSamePackage is
// #330: the pre-test "make the target package buildable" emit matched
// EVERY file the DB tracks for the whole target package/directory, not
// just files genuinely missing from disk, so any sibling file in that
// SAME package whose disk content had diverged from the DB out-of-band
// (a git stash, a manual revert, a hand edit) got silently overwritten
// back to stale DB content the next time an unrelated test in that
// package ran -- even though the file was still present and would have
// compiled fine as-is. Reproduced live via winze's independent report
// (git stash on files in the same package as an unrelated edit) and
// directly on this repo (deleting scratch files, then running an
// unrelated test in the same package silently restored them). A file
// that still EXISTS on disk is not a compilation blocker and must be
// left alone; only a file genuinely ABSENT needs restoring.
func TestHandleTestByName_DoesNotRestoreDivergedSiblingInSamePackage(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc RootFunc() string { return \"root\" }\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestRootFunc(t *testing.T) {\n\tif RootFunc() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n"), 0644)
	siblingPath := filepath.Join(projDir, "sibling.go")
	origSibling := "package main\n\nfunc SiblingFunc() string { return \"original\" }\n"
	os.WriteFile(siblingPath, []byte(origSibling), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	// Simulate an out-of-band change (git stash revert, manual edit) to
	// sibling.go AFTER ingest -- the DB still holds origSibling, but
	// disk now has something different. The file still EXISTS.
	divergedSibling := "package main\n\nfunc SiblingFunc() string { return \"user-reverted-this-on-purpose\" }\n"
	if err := os.WriteFile(siblingPath, []byte(divergedSibling), 0644); err != nil {
		t.Fatal(err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleTestByName(context.Background(), nil, "TestRootFunc", "", "")
	if err != nil {
		t.Fatalf("handleTestByName: %v", err)
	}
	if !strings.Contains(resultText(t, result), "ALL TESTS PASSED") {
		t.Fatalf("expected TestRootFunc to pass, got: %s", resultText(t, result))
	}

	after, err := os.ReadFile(siblingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != divergedSibling {
		t.Errorf("handleTestByName silently overwrote a sibling file's out-of-band content back to stale DB state.\nwant (out-of-band content preserved):\n%s\ngot:\n%s", divergedSibling, after)
	}
}

// TestHandleTest_DisclosesOtherUnrunTestsInSamePackage is #331: a real
// prometheus-18358 v17 bench trajectory ran code(op:"test", name:X),
// got "ALL TESTS PASSED", and treated that as proof nothing else in
// the package broke -- but a golden-file test coupled to the change
// only via a subprocess invocation (not a Go reference) was never in
// the call-graph-derived "affected by X" set and never ran, silently
// missing a required doc update. The success message must disclose
// when other tests exist in the same package that this narrowed
// selection didn't cover.
func TestHandleTest_DisclosesOtherUnrunTestsInSamePackage(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "root.go"), []byte("package testproj\n\nfunc RootFunc() string { return \"root\" }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "root_test.go"), []byte("package testproj\n\nimport \"testing\"\n\nfunc TestRootFunc(t *testing.T) {\n\tif RootFunc() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n\nfunc TestUnrelatedGoldenFile(t *testing.T) {\n\t// Not statically coupled to RootFunc via any call/reference edge.\n}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleTest(context.Background(), nil, nameParam{Name: "RootFunc"})
	if err != nil {
		t.Fatalf("handleTest(RootFunc): %v", err)
	}
	text := resultText(t, result)
	t.Logf("output:\n%s", text)

	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Fatalf("expected the affected test to pass, got: %s", text)
	}
	if !strings.Contains(text, "1 other test(s) exist in this package that were NOT run") {
		t.Errorf("expected a disclosure that TestUnrelatedGoldenFile wasn't covered by this narrowed run, got: %s", text)
	}
}

// TestHandleEdit_ImpactNudgeIncludesReceiverWhenPresent is #333: a real
// prometheus-19017 v17 bench trajectory edited (ActiveQueryTracker).Insert
// (correctly disambiguated via receiver:), got the "FYI: N callers, M
// tests affected. Run code(op:"test", name:"Insert") to verify." nudge
// with the receiver dropped, then followed that exact suggestion --
// which silently resolved to an unrelated tsdb/chunkenc.Insert (highest
// caller-count tiebreak) instead of the just-edited method, running 1697
// unrelated tests and reporting "ALL TESTS PASSED" with zero real
// verification of the actual change. The ambiguity was disclosed
// correctly (ambiguityNote fired), but the model didn't re-add
// receiver: on the follow-up call. The suggested command must include
// the receiver it already knows, so there's nothing to forget.
func TestHandleEdit_ImpactNudgeIncludesReceiverWhenPresent(t *testing.T) {
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
	os.WriteFile(filepath.Join(projDir, "a.go"), []byte(`package testproj

type Foo struct{}

func (f Foo) Insert(x int) int { return x }

func UseFoo(f Foo) int { return f.Insert(1) }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "a_test.go"), []byte(`package testproj

import "testing"

func TestFooInsert(t *testing.T) {
	if (Foo{}).Insert(1) != 1 {
		t.Fatal("wrong")
	}
}
`), 0644)
	os.WriteFile(filepath.Join(projDir, "b.go"), []byte(`package testproj

type Bar struct{}

func (b Bar) Insert(x int) int { return x + 1 }
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:       "edit",
		Name:     "Insert",
		Receiver: "Foo",
		NewBody:  "func (f Foo) Insert(x int) int { return x + 100 }",
	})
	if err != nil {
		t.Fatalf("handleCode edit: %v", err)
	}
	text := resultText(t, result)
	t.Logf("output:\n%s", text)

	if !strings.Contains(text, `receiver:"Foo"`) {
		t.Errorf(`expected the FYI suggestion to include receiver:"Foo" so a follow-up test call resolves to the just-edited method, not an unrelated same-named one, got: %s`, text)
	}
}

// TestHandleCreate_DoubleEscapedBodyWithLeadingCommentDoesNotSilentlySucceedAsScaffold
// reproduces the more dangerous shape from the same prometheus-19017
// trajectory (messages 70/76): a leading doc comment used real
// newlines, but everything after it -- including the func declaration
// itself -- was double-escaped. Go's line-comment rule (a `//`
// comment runs to the next REAL newline) silently swallowed the whole
// func into the comment, so isImportsOnlyBody saw a "valid,
// comment-only" body and the OLD behavior reported a successful
// scaffold with none of the intended content ever written -- worse
// than an outright error since it looked like progress. This must be
// rejected instead.
func TestHandleCreate_DoubleEscapedBodyWithLeadingCommentDoesNotSilentlySucceedAsScaffold(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	body := "// TestFoo checks something.\n// crashing the process.\\nfunc TestFoo(t *testing.T) {\\n\\tx := 1\\n\\t_ = x\\n}"

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		File: "extra_test.go",
		Body: body,
	})
	text := resultText(t, result)

	if strings.Contains(text, "Scaffolded") {
		t.Fatalf("double-escaped body must not silently succeed as a comment-only scaffold, got: %s", text)
	}
	if !strings.Contains(text, "JSON-escaped twice") {
		t.Errorf("expected a hint about double-escaped JSON, got: %s", text)
	}
}

// TestHandleCreate_DoubleEscapedNewlinesRejectedWithHint is the
// regression for a real prometheus-19017 trajectory: the model's
// tool-call argument was JSON-escaped twice, so the body's control
// characters arrived as the literal two-character sequences \n/\t
// instead of real newline/tab bytes. inferFromBody can't find a name
// in that (it's a syntax error outside a string literal), and the
// generic "make sure it starts with func/type/const/var" message gave
// no signal toward the real problem -- it took the model 4 attempts to
// land real newlines. doubleEscapedHint should name the actual cause
// on the first attempt.
func TestHandleCreate_DoubleEscapedNewlinesRejectedWithHint(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	body := "func TestSomething(t *testing.T) {\\n\\tx := 1\\n\\t_ = x\\n}"

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
	})
	text := resultText(t, result)

	if !strings.Contains(text, "JSON-escaped twice") {
		t.Errorf("expected a hint about double-escaped JSON, got: %s", text)
	}
	if strings.Contains(text, "Created") {
		t.Errorf("double-escaped body must not silently succeed, got: %s", text)
	}
}

// TestHandleCreate_LegitimateEscapedNewlineInStringLiteralNotFlagged
// guards doubleEscapedHint's false-positive boundary: a real, valid
// one-liner containing an actual escaped-newline STRING LITERAL
// (legitimate Go, not double-escaped JSON) must still create normally.
func TestHandleCreate_LegitimateEscapedNewlineInStringLiteralNotFlagged(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func NewSeparator() string { return \"a\\nb\" }",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created") {
		t.Errorf("expected legitimate body with one string-literal \\n escape to create normally, got: %s", text)
	}
}

// TestResolveEditTarget_FileDisambiguatesSameNamedFunctionInOneModule is
// issue #339: file was only ever used to derive a MODULE scope via
// findModuleByFile, which frequently resolves to nothing for a plain
// subdirectory of a single flat root module (findModuleByFile's suffix
// matching only succeeds for nested-module layouts) -- so a
// file-scoped lookup often falls through to GetDefinitionByName(name,
// "") completely unscoped. Two same-named, zero-caller functions in
// different files/packages of one module is a genuine tie for that
// fallback's blast-radius query: it can satisfy a request for EITHER
// file, but not both in the same run, since it returns one fixed
// row regardless of which file was actually asked for. An exact
// source_file match (this fix) is the only way to correctly satisfy
// both directions.
func TestResolveEditTarget_FileDisambiguatesSameNamedFunctionInOneModule(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "pkga"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "pkgb"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "pkga", "a.go"), []byte(`package pkga

func NewThing() string { return "a" }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "pkgb", "b.go"), []byte(`package pkgb

func NewThing() string { return "b" }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	da, err := s.resolveEditTarget("NewThing", "", "", filepath.Join("pkga", "a.go"))
	if err != nil {
		t.Fatalf("resolveEditTarget(pkga/a.go): %v", err)
	}
	if da.SourceFile != filepath.Join("pkga", "a.go") {
		t.Errorf("file:%q should resolve to pkga's NewThing, got source_file=%q", filepath.Join("pkga", "a.go"), da.SourceFile)
	}

	db2, err := s.resolveEditTarget("NewThing", "", "", filepath.Join("pkgb", "b.go"))
	if err != nil {
		t.Fatalf("resolveEditTarget(pkgb/b.go): %v", err)
	}
	if db2.SourceFile != filepath.Join("pkgb", "b.go") {
		t.Errorf("file:%q should resolve to pkgb's NewThing, got source_file=%q", filepath.Join("pkgb", "b.go"), db2.SourceFile)
	}
}

// TestHandleCode_AmbiguityNoteStillFiresWhenFileGivenButDoesNotDisambiguate
// is issue #339: ambiguityNote used to bail out unconditionally whenever
// file was non-empty, on the assumption that file: always narrows
// resolution. But findModuleByFile only resolves file to a MODULE
// scope, and its suffix-matching only succeeds for nested-module
// layouts -- for a file in a subdirectory of one flat root module
// (this fixture, and the real prometheus-19236 trajectory), it
// resolves to nothing and the lookup falls through completely
// unscoped. Passing file: should not silently suppress the warning
// that a project-wide name collision exists.
func TestHandleCode_AmbiguityNoteStillFiresWhenFileGivenButDoesNotDisambiguate(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "bft"), 0755)
	os.MkdirAll(filepath.Join(projDir, "chess"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "bft", "engine.go"), []byte("package bft\n\ntype Engine struct{ Replica string }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "chess", "engine.go"), []byte(`package chess

type Engine struct{ Protocol string }

func NewEngine() *Engine { return &Engine{} }
func UseA(e *Engine) string { return e.Protocol }
func UseB(e *Engine) string { return e.Protocol }
func UseC(e *Engine) string { return e.Protocol }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op: "outline", Name: "Engine", File: filepath.Join("bft", "engine.go"),
	})
	if err != nil {
		t.Fatalf("handleCode outline: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "2 definitions share the name") {
		t.Errorf("expected an ambiguity note even with file: set, got:\n%s", text)
	}
}

// TestRunBuildIn_MainPackageDirNameDoesNotCollideWithOutput guards a real
// bug (etcd-io/etcd-15760, 2026-08-23): building a single main-package
// target like "./etcdutl" without an explicit -o makes `go build` write
// an executable named "etcdutl" into the working directory by default --
// colliding with the etcdutl/ directory itself when the repo has a
// top-level directory of the same name (routine for CLI tools with
// subcommand packages). That produced a false "BUILD FAILED: go: build
// output \"etcdutl\" already exists and is a directory" even though the
// edit compiled cleanly.
func TestRunBuildIn_MainPackageDirNameDoesNotCollideWithOutput(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module example.com/x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(tmpDir, "etcdutl")
	if err := os.Mkdir(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainSrc := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &server{}
	out, err := s.runBuildIn(context.Background(), tmpDir, []string{"./etcdutl"})
	if err != nil {
		t.Fatalf("runBuildIn failed on a package dir name that collides with go build's default output path: %s", out)
	}

	if _, err := os.Stat(pkgDir); err != nil {
		t.Fatalf("etcdutl/ directory was disturbed by the build: %v", err)
	}
	if info, err := os.Stat(pkgDir); err == nil && !info.IsDir() {
		t.Fatalf("etcdutl was overwritten by the build output, no longer a directory")
	}
}

// TestHandleEdit_UnrelatedStaleDefDoesNotBlockUnrelatedEdit guards the
// #350 fix: a writeFile "could not be matched to an on-disk
// declaration" warning about some OTHER, unrelated definition in the
// same file must not roll back an edit that itself landed cleanly.
// Real trajectory (caddyserver/caddy-6179/7870, 2026-08-23): a single
// permanently-stale def (a duplicate ingested "init" method) blocked 5
// different edit/create/apply attempts targeting unrelated definitions
// in the same file, none of which the warning was ever actually about.
func TestHandleEdit_UnrelatedStaleDefDoesNotBlockUnrelatedEdit(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()

	greet, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}

	// Insert a definition that will never match anything on disk --
	// simulating the permanently-stale def class of bug without needing
	// to reproduce its ingest-level root cause. Sharing Greet's
	// SourceFile means any write to that file re-triggers writeFile's
	// "could not be matched" warning for this def, every time,
	// regardless of what's actually being edited.
	if _, err := db.UpsertDefinition(&store.Definition{
		ModuleID:   greet.ModuleID,
		Name:       "GhostMethod",
		Kind:       "method",
		Receiver:   "*Ghost",
		Body:       "func (g *Ghost) GhostMethod() {}",
		SourceFile: greet.SourceFile,
	}); err != nil {
		t.Fatal(err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleEdit(context.Background(), nil, editParam{
		Name:    "Greet",
		NewBody: "func Greet(name string) string {\n\treturn \"Hi, \" + name\n}",
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("Greet's own edit should not roll back due to an unrelated stale def (GhostMethod), got: %s", text)
	}
	if !strings.Contains(text, "Updated") {
		t.Errorf("expected a success message, got: %s", text)
	}

	onDisk, err := os.ReadFile(filepath.Join(projDir, greet.SourceFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), `"Hi, " + name`) {
		t.Errorf("Greet's edit was not actually written to disk despite a non-rollback response:\n%s", onDisk)
	}
}

// TestResolveEditTarget_FileScopedLookupSkipsStructFieldSharingTypeName
// is a regression for a real bench trajectory (caddyserver/caddy-7870,
// 2026-08-23): reverseproxy/hosts.go declares
// "type ActiveHealthChecks struct { Upstream *Upstream }" before
// "type Upstream struct {...}" -- Go's own self-referencing field
// idiom. resolveEditTarget's #339 "exact file match is unambiguous"
// shortcut assumed a file can't have two non-method top-level decls
// sharing a Name, which is true, but doesn't account for FIELDS (not
// top-level decls) sharing a Name with an unrelated top-level type --
// it took the FIRST name+file match by source line, which was the
// earlier-declared field, and returned it silently. The live agent
// then spent ~20 tool calls trying to edit what it believed was the
// type, always refused as "doesn't support struct fields," never
// realizing the type itself was never actually targeted.
func TestResolveEditTarget_FileScopedLookupSkipsStructFieldSharingTypeName(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "fieldproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "hosts.go"), []byte(`package fieldproj

type ActiveHealthChecks struct {
	Upstream *Upstream
}

type Upstream struct {
	Dial string
}

func (a *ActiveHealthChecks) Target() string {
	return a.Upstream.Dial
}
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	d, err := s.resolveEditTarget("Upstream", "", "", "hosts.go")
	if err != nil {
		t.Fatalf("resolveEditTarget: %v", err)
	}
	if d.Kind == "field" {
		t.Fatalf("resolveEditTarget(name:\"Upstream\", file:\"hosts.go\") resolved to struct field %s.%s instead of the top-level type -- file: scoping made an ambiguous bare name WORSE, not better, since it bypassed the (already kind-aware) fallback path entirely", d.Receiver, d.Name)
	}
	if d.Kind != "type" {
		t.Fatalf("expected the top-level type, got kind=%q", d.Kind)
	}
}

// TestResolveWriteTarget_FieldSharingTypeNameNoLongerFalselyAmbiguous
// is a #352 followup: GetDefinitionByName now excludes struct fields
// from bare-name lookups, so a receiverless write to a type sharing
// its name with an unrelated field resolves deterministically. But
// CountDefinitionsByName (which resolveWriteTarget's ambiguity refusal
// uses) was never updated to match -- it still counts the field, so
// the exact same bare name GetDefinitionByName now resolves cleanly
// gets refused as "ambiguous" anyway, immediately after resolving it
// correctly. A receiverless write to a type must not be refused just
// because an unrelated struct field happens to share its name.
func TestResolveWriteTarget_FieldSharingTypeNameNoLongerFalselyAmbiguous(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "fieldproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "hosts.go"), []byte(`package fieldproj

type ActiveHealthChecks struct {
	Upstream *Upstream
}

type Upstream struct {
	Dial string
}

func (a *ActiveHealthChecks) Target() string {
	return a.Upstream.Dial
}
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	d, err := s.resolveWriteTarget("Upstream", "", "", "")
	if err != nil {
		t.Fatalf("resolveWriteTarget(\"Upstream\") with no receiver/module/file: %v (should have resolved deterministically to the type, since GetDefinitionByName now excludes the field)", err)
	}
	if d.Kind != "type" {
		t.Fatalf("expected the top-level type, got kind=%q", d.Kind)
	}
}

// TestResolveDottedQualifiedName_SkipsStructFieldSharingTypeName is a
// third #352-class site: resolveDottedQualifiedName resolves Go's own
// "pkg.Symbol" convention (e.g. "hosts.Upstream") via
// FilterDefinitions(bare, kind:"", file, limit:1) -- kind:"" means no
// kind filter, and limit:1 takes whatever ORDER BY d.name (a no-op
// tiebreak among same-named rows) returns first. The same
// "Foo *Upstream" self-referencing field idiom that broke
// resolveEditTarget and GetDefinitionByName can make this path return
// a struct field instead of the top-level type too.
func TestResolveDottedQualifiedName_SkipsStructFieldSharingTypeName(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "fieldproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "hosts.go"), []byte(`package fieldproj

type ActiveHealthChecks struct {
	Upstream *Upstream
}

type Upstream struct {
	Dial string
}

func (a *ActiveHealthChecks) Target() string {
	return a.Upstream.Dial
}
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	d, err := s.resolveDottedQualifiedName(db, "hosts.Upstream")
	if err != nil {
		t.Fatalf("resolveDottedQualifiedName: %v", err)
	}
	if d == nil {
		t.Fatal("expected a match, got nil")
	}
	if d.Kind == "field" {
		t.Fatalf("resolveDottedQualifiedName(\"hosts.Upstream\") resolved to struct field %s.%s instead of the top-level type", d.Receiver, d.Name)
	}
	if d.Kind != "type" {
		t.Fatalf("expected the top-level type, got kind=%q", d.Kind)
	}
}

// TestCoupledChangeHint_IncludesTestCallersNotCaughtByBuildGate is the
// #353 regression: a real caddy-7870 trajectory (2026-08-23) needed a
// coupled signature change across a production caller AND two _test.go
// callers. coupledChangeHint's build-failure tip only ever named the
// production caller (test callers were deliberately excluded, on the
// assumption test-coverage risk was already flagged elsewhere) -- but
// go build (the check gating every write op) never compiles _test.go
// files at all, so the test callers got ZERO signal from either the
// hint or the build gate itself. The model in that trajectory only
// avoided shipping a test-breaking change because it happened to
// remember, ~60 calls earlier in the same session, that a test file
// called the def -- luck, not a guarantee. Test callers must now be
// surfaced too, clearly labeled as build-gate-blind.
func TestCoupledChangeHint_IncludesTestCallersNotCaughtByBuildGate(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "coupledproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module coupledproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package coupledproj

func Fetch(url string) string { return url }

func Use() string { return Fetch("http://example.com") }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte(`package coupledproj

import "testing"

func TestFetch(t *testing.T) {
	Fetch("http://example.com")
}
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db}
	d, err := db.GetDefinitionByName("Fetch", "")
	if err != nil {
		t.Fatal(err)
	}
	hint := s.coupledChangeHint(d.ID)

	if !strings.Contains(hint, "Use") {
		t.Errorf("expected the production caller Use named, got: %q", hint)
	}
	if !strings.Contains(hint, "TestFetch") {
		t.Errorf("expected the test caller TestFetch named, got: %q", hint)
	}
	if !strings.Contains(hint, "build check") {
		t.Errorf("expected a note that test callers aren't caught by the build check, got: %q", hint)
	}
	if !strings.Contains(hint, "this def has") {
		t.Errorf("expected the production-caller phrasing to still be present, got: %q", hint)
	}
}

// TestHandleSearch_MCPDebugTracesEachStage verifies the DEFN_MCP_DEBUG=1
// instrumentation added 2026-08-23 to help root-cause a still-open
// mystery: a real caddy-7870 trajectory (post-#352-fix) showed
// search(pattern:"hosts.LoadOrStore") return two completely different,
// non-overlapping result sets on two calls in the same read-only
// session, with no edits in between -- not reordering, a genuine
// content swap. A direct static replay against the persisted database
// afterward could NOT reproduce it (deterministic, single stable
// result across 5 repeated runs), meaning whatever caused the live
// divergence is tied to transient session/server state a static
// re-query can't see. This trace (stage1/stage2/merged/file-filtered/
// final, each with per-def IDs, not just names) is the tool for
// catching the next live occurrence -- see debugDefIDs's own doc
// comment for why IDs specifically matter here (two same-named-but-
// different-row results would look identical without them).
func TestHandleSearch_MCPDebugTracesEachStage(t *testing.T) {
	t.Setenv("DEFN_MCP_DEBUG", "1")

	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	result, _, _ := s.handleSearch(context.Background(), nil, codeParam{Pattern: "Greet"})
	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	text := resultText(t, result)
	if !strings.Contains(text, "Greet") {
		t.Fatal("expected Greet in search results -- fixture broken, trace assertions below would be meaningless")
	}

	for _, want := range []string{
		"[mcp-debug] search pattern=\"Greet\" stage1(FindDefinitions):",
		"[mcp-debug] search pattern=\"Greet\" stage2(SearchDefinitions/FTS):",
		"[mcp-debug] search pattern=\"Greet\" merged:",
		"[mcp-debug] search pattern=\"Greet\" FINAL returned",
		"Greet(id=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected debug output to contain %q, got:\n%s", want, out)
		}
	}
}

// TestHandleRename_TypeRenameDoesNotCorruptFieldNamedAfterItsOwnType is
// the #355 regression: found live via the field_named_after_own_type
// fuzzer hazard (added for #352's "Foo *Foo" self-referencing field
// idiom) -- FuzzMutationSequence reported "go build failed after a
// mutation defn reported as successful: a.Upstream undefined" in under
// a second, fully deterministic. Root cause: renaming the type
// "Upstream" correctly identifies ActiveHealthChecks as a real caller
// (its field's TYPE position genuinely references Upstream) and
// rewrites that struct's body via astRename -- which, before this fix,
// renamed BOTH the field's type reference AND its own declared NAME
// (since "Upstream *Upstream" is textually the same identifier twice),
// even though a field's name is never supposed to be touched by
// anything except the dedicated field-rename path. This silently broke
// every untouched caller referencing the field, like Target()'s
// a.Upstream.Dial, with defn reporting full success throughout.
func TestHandleRename_TypeRenameDoesNotCorruptFieldNamedAfterItsOwnType(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "fieldtype")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module fieldtype\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "hosts.go"), []byte(`package fieldtype

type Upstream struct {
	Dial string
}

type ActiveHealthChecks struct {
	Upstream *Upstream
}

func (a *ActiveHealthChecks) Target() string {
	return a.Upstream.Dial
}
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleRename(context.Background(), nil, renameParam{
		OldName: "Upstream",
		NewName: "Backend",
	})
	if err != nil {
		t.Fatalf("handleRename: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("expected rename to succeed, got: %s", text)
	}

	raw, err := os.ReadFile(filepath.Join(projDir, "hosts.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if !strings.Contains(src, "type Backend struct") {
		t.Errorf("expected the type declaration renamed to Backend, got:\n%s", src)
	}
	if !strings.Contains(src, "Upstream *Backend") {
		t.Errorf("expected the field's TYPE updated to *Backend while its NAME stays Upstream, got:\n%s", src)
	}
	if !strings.Contains(src, "a.Upstream.Dial") {
		t.Errorf("expected Target()'s untouched field access a.Upstream.Dial preserved, got:\n%s", src)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed after the rename:\n%s", out)
	}
}

// TestHandleReadFile_LineRangeOnHugeDefSuggestsQueryFilter is the #356
// regression: two real bench trajectories (caddy-13474, traefik-13041)
// showed a model paging through a huge single def (a >1000-line
// table-driven test function) via repeated read-file line_range
// guesses -- one even with the exact target line number already in
// hand from a test failure trace -- instead of read(name:X,
// query:"<keyword>"), which jumps straight to matching statements in
// one call. Both tools already existed; the model just never reached
// for the one built for this. Surface it directly whenever the
// requested range is small relative to a def's own full span, the
// shape that signals "hunting for a needle in a huge def" rather than
// "reading a known slice".
func TestHandleReadFile_LineRangeOnHugeDefSuggestsQueryFilter(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "hugefile")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module hugefile\n\ngo 1.26\n"), 0644)

	var b strings.Builder
	b.WriteString("package hugefile\n\nimport \"testing\"\n\nfunc TestHuge(t *testing.T) {\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "\t_ = %d // padding line %d\n", i, i)
	}
	b.WriteString("}\n")
	os.WriteFile(filepath.Join(projDir, "huge_test.go"), []byte(b.String()), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	result, _, err := s.handleReadFile(context.Background(), nil, codeParam{File: "huge_test.go", LineRange: "200-210"})
	if err != nil {
		t.Fatalf("read-file: %v", err)
	}
	text := resultText(t, result)

	if !strings.Contains(text, "query:\"<keyword>\"") {
		t.Errorf("expected a hint pointing at read(name:X, query:...) for a narrow range inside a huge def, got: %s", text)
	}
	if !strings.Contains(text, "TestHuge") {
		t.Errorf("expected the hint to name the huge def, got: %s", text)
	}
}

// TestHandleTest_CoveringTestInDifferentPackageStillRuns is the cli-405
// regression: handleTest used to scope go test to ONLY the edited
// definition's own package (testScopeTarget(d.SourceFile)), even though
// impact.Tests can legitimately live in a different package entirely (a
// caller in a sibling package, or interface-dispatch coverage). That left
// the covering test's real package completely unscanned, so `go test`
// matched nothing there and this handler reported a confusing "NO TESTS
// MATCHED" even though the covering test is real -- confirmed live on a
// cli/cli bench trajectory where RenderMarkdown's actual covering tests
// never ran and the model shipped an unverified fix (F1=0.00). Verifies
// the covering test's own package gets included in the scope even when it
// differs from the def's own package.
func TestHandleTest_CoveringTestInDifferentPackageStillRuns(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(filepath.Join(projDir, "helper"), 0755)
	os.MkdirAll(filepath.Join(projDir, "caller"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "helper", "helper.go"), []byte("package helper\n\nfunc Helper() string { return \"x\" }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "caller", "caller.go"), []byte("package caller\n\nimport \"testproj/helper\"\n\nfunc UseHelper() string { return helper.Helper() }\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "caller", "caller_test.go"), []byte("package caller\n\nimport \"testing\"\n\nfunc TestUseHelper(t *testing.T) {\n\tif UseHelper() == \"\" {\n\t\tt.Fatal(\"caller-package-marker\")\n\t}\n}\n"), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleTest(context.Background(), nil, nameParam{Name: "Helper"})
	if err != nil {
		t.Fatalf("handleTest(Helper): %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "NO TESTS MATCHED") {
		t.Fatalf("expected Helper's covering test (in a different package) to actually run, got NO TESTS MATCHED:\n%s", text)
	}
	if !strings.Contains(text, "ALL TESTS PASSED") {
		t.Errorf("expected covering test to run and pass, got:\n%s", text)
	}
	if !strings.Contains(text, "./caller/...") {
		t.Errorf("expected scope to include caller's own package ./caller/..., got:\n%s", text)
	}
}

// TestResolveEditTarget_FileScopedExactMatchPopulatesBody guards a real
// cli/cli-2671 bench trajectory: resolveEditTarget's #339 exact-file-match
// fast path resolves the RIGHT definition (confirmed via its correct
// SourceFile/Module in the response) but returns it straight from
// FindDefinitionsByFile, whose query never selects/joins the bodies
// table -- every definition it returns has a permanently empty Body
// field. Every caller of resolveEditTarget trusts d.Body directly with
// no re-fetch (handleGetDefinition renders it straight into the code
// fence), so a file:-disambiguated read/outline of an ambiguously-named
// def silently came back with a blank body, and edit/replace-hunk's
// old_fragment matching against that empty body always failed with
// "not found in body" -- confirmed live: five same-named createRun
// functions across cli/cli's command packages, file:-disambiguated to
// the right one every time, body empty every time.
func TestResolveEditTarget_FileScopedExactMatchPopulatesBody(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(filepath.Join(projDir, "pkga"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "pkgb"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "pkga", "a.go"), []byte(`package pkga

func NewThing() string { return "a-marker" }
`), 0644)
	os.WriteFile(filepath.Join(projDir, "pkgb", "b.go"), []byte(`package pkgb

func NewThing() string { return "b-marker" }
`), 0644)

	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	da, err := s.resolveEditTarget("NewThing", "", "", filepath.Join("pkga", "a.go"))
	if err != nil {
		t.Fatalf("resolveEditTarget(pkga/a.go): %v", err)
	}
	if !strings.Contains(da.Body, "a-marker") {
		t.Errorf("file:-disambiguated NewThing (pkga) should have its real body populated (containing %q), got body=%q", "a-marker", da.Body)
	}

	db2, err := s.resolveEditTarget("NewThing", "", "", filepath.Join("pkgb", "b.go"))
	if err != nil {
		t.Fatalf("resolveEditTarget(pkgb/b.go): %v", err)
	}
	if !strings.Contains(db2.Body, "b-marker") {
		t.Errorf("file:-disambiguated NewThing (pkgb) should have its real body populated (containing %q), got body=%q", "b-marker", db2.Body)
	}
}

// TestHandleApply_CreateNameParamRejected is the apply-batched
// counterpart to TestHandleCode_CreateNameParamRejected -- same gap,
// reached via handleApply's own "create" case (both the dry-run preview
// and the real path each have their own independent create branch).
func TestHandleApply_CreateNameParamRejected(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "create", Name: "WantedName", Body: "func ActualName() string { return \"x\" }"},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "doesn't match") {
		t.Fatalf("expected a rejection explaining name: doesn't match body's declared name, got: %s", text)
	}

	dryRun, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun: true,
		Operations: []applyOp{
			{Op: "create", Name: "WantedName", Body: "func ActualName() string { return \"x\" }"},
		},
	})
	dryText := resultText(t, dryRun)
	if !strings.Contains(dryText, "doesn't match") {
		t.Fatalf("expected dry-run to also reject a name: mismatch, got: %s", dryText)
	}
}

// TestHandleApply_RenameAcceptsOldNameAlias guards apply's rename sub-op
// accepting old_name as an alias for name when name is empty -- the
// standalone op:"rename" (renameParam/codeParam) names this same value
// old_name, so a caller batching rename inside apply naturally reaches
// for the same field and previously got a generic "both name and
// new_name are required" error, since apply's own OldName field is
// separately bound to patch's old-text.
func TestHandleApply_RenameAcceptsOldNameAlias(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	dryRun, _, _ := s.handleApply(context.Background(), nil, applyParam{
		DryRun: true,
		Operations: []applyOp{
			{Op: "rename", OldName: "Greet", NewName: "Salute"},
		},
	})
	dryText := resultText(t, dryRun)
	if strings.Contains(dryText, "required") {
		t.Fatalf("expected old_name alias to satisfy the dry-run rename validation, got: %s", dryText)
	}
	if !strings.Contains(dryText, "Greet") || !strings.Contains(dryText, "Salute") {
		t.Fatalf("expected a would-rename preview naming Greet and Salute, got: %s", dryText)
	}

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "rename", OldName: "Greet", NewName: "Salute"},
		},
	})
	text := resultText(t, result)
	if strings.Contains(text, "required") {
		t.Fatalf("expected old_name alias to satisfy the real rename validation, got: %s", text)
	}

	read, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Salute", Full: true})
	readText := resultText(t, read)
	if !strings.Contains(readText, "func Salute") {
		t.Fatalf("expected Greet to actually be renamed to Salute via the old_name alias, got: %s", readText)
	}
}

// TestHandleCode_CreateNameParamRejected guards against create's silent
// name: no-op: createParam has no Name field at all, so a caller
// passing name: alongside body (reasonably expecting it to name the new
// def, the way it names the target for every other op) got it silently
// dropped -- the name inferred from body's own declaration won the
// tiebreak with no error and no note. Same silent-no-op class as #298
// (edit's import_path/alias).
func TestHandleCode_CreateNameParamRejected(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:   "create",
		Name: "WantedName",
		Body: "func ActualName() string { return \"x\" }",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "doesn't match") {
		t.Fatalf("expected a rejection explaining name: doesn't match body's declared name, got: %s", text)
	}

	// #241's own "no matches for %q" message echoes the queried pattern
	// back verbatim, so a bare Contains(text, "WantedName") on that
	// message would false-positive here even with no def created --
	// check for the search summary's actual JSON name field instead.
	searchResult, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "search", Pattern: "WantedName"})
	if strings.Contains(resultText(t, searchResult), "\"name\":\"WantedName\"") {
		t.Errorf("rejected create must not have created a def under the ignored name: param")
	}
}

// TestHandleSearch_ReceiverFiltersResults guards against search silently
// ignoring receiver:, a real codeParam field used to disambiguate
// same-named methods across types everywhere else (read/outline/edit/
// impact/rename/move/test/similar all honor it) -- search returned every
// matching def regardless of receiver, the same silent-no-op class file:
// itself had before #241.
func TestHandleSearch_ReceiverFiltersResults(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

type Alpha struct{}

// Run executes the alpha banana workflow.
func (a *Alpha) Run() {}

type Beta struct{}

// Run executes the beta banana workflow.
func (b *Beta) Run() {}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}

	// Unscoped: both Alpha.Run and Beta.Run match "banana".
	result, _, err := s.handleSearch(context.Background(), nil, codeParam{Pattern: "banana"})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Alpha") || !strings.Contains(text, "Beta") {
		t.Fatalf("expected both Alpha and Beta unscoped, got: %s", text)
	}

	// Scoped to receiver Alpha: only Alpha.Run should survive.
	result, _, err = s.handleSearch(context.Background(), nil, codeParam{Pattern: "banana", Receiver: "Alpha"})
	if err != nil {
		t.Fatalf("handleSearch with receiver: %v", err)
	}
	text = resultText(t, result)
	if !strings.Contains(text, "Alpha") {
		t.Errorf("receiver:\"Alpha\" should still include Alpha.Run, got: %s", text)
	}
	if strings.Contains(text, "Beta") {
		t.Errorf("receiver:\"Alpha\" should have excluded Beta.Run, got: %s", text)
	}

	// Pointer-prefixed receiver form should match the same way.
	result, _, err = s.handleSearch(context.Background(), nil, codeParam{Pattern: "banana", Receiver: "*Alpha"})
	if err != nil {
		t.Fatalf("handleSearch with pointer receiver: %v", err)
	}
	text = resultText(t, result)
	if !strings.Contains(text, "Alpha") || strings.Contains(text, "Beta") {
		t.Errorf("receiver:\"*Alpha\" should match the same as bare \"Alpha\", got: %s", text)
	}
}

// TestHandleCode_CreateNameParamMatchingBodyIsAllowed guards the other
// side of TestHandleCode_CreateNameParamRejected: name: that merely
// echoes what body already declares must NOT be rejected -- a real
// recorded trajectory (see TestHandleApply_RenamePointerReceiver-
// MethodThenEditSameBatch) passes name: on a create op inside a batch
// alongside sibling ops that key off name:, and it happens to agree
// with body. Only a genuine disagreement is a mistake worth flagging.
func TestHandleCode_CreateNameParamMatchingBodyIsAllowed(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:   "create",
		Name: "Zorp",
		Body: "func Zorp() string { return \"x\" }",
		File: "main.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created") {
		t.Fatalf("expected create to succeed when name: matches body's declared name, got: %s", text)
	}
}

// TestHandleCode_ExplainBareNamesLoopsOverEachName guards explain's bare
// (non-question) path silently dropping names: -- the validation switch
// accepts names: as an acceptable scope for explain (mirroring the
// question-driven path's own Names support), but handleExplain takes
// nameParam, which has no Names field. A caller passing names:["A","B"]
// with no question: got args.Name silently empty, producing a confusing
// `definition "" not found` with no signal names: was the problem.
func TestHandleCode_ExplainBareNamesLoopsOverEachName(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:    "explain",
		Names: []string{"Greet", "Farewell"},
	})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected bare explain with names: to succeed, got error: %s", text)
	}
	if !strings.Contains(text, "Greet") || !strings.Contains(text, "Farewell") {
		t.Errorf("expected explain blocks for both Greet and Farewell, got: %s", text)
	}
	if strings.Contains(text, "not found") {
		t.Errorf("names: should have resolved both defs, not fallen through to a not-found path: %s", text)
	}
}

// TestHandleCreate_MultiDeclDryRunDoesNotWrite guards handleCreateMultiDecl,
// which is dispatched from handleCreate BEFORE its own "#dry-run-create"
// check ever runs (the multi-decl branch returns early) -- dry_run:true
// on a multi-decl body silently wrote every declaration for real.
func TestHandleCreate_MultiDeclDryRunDoesNotWrite(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:     "create",
		File:   "extra/multi.go",
		Body:   "func FirstFn() int { return 1 }\n\nfunc SecondFn() int { return 2 }",
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected dry-run multi-decl create to succeed, got error: %s", text)
	}
	if !strings.Contains(text, "dry run") {
		t.Errorf("expected dry-run preview text, got: %s", text)
	}

	if _, err := db.GetDefinitionByName("FirstFn", ""); err == nil {
		t.Errorf("FirstFn should NOT have been created under dry_run:true, but it exists in the DB")
	}
	if _, err := db.GetDefinitionByName("SecondFn", ""); err == nil {
		t.Errorf("SecondFn should NOT have been created under dry_run:true, but it exists in the DB")
	}
	if _, statErr := os.Stat(filepath.Join(projDir, "extra/multi.go")); statErr == nil {
		t.Errorf("extra/multi.go should NOT have been written to disk under dry_run:true, but it exists")
	}
}

// TestHandleCreate_ScaffoldFileDryRunDoesNotWrite guards
// handleCreateScaffoldFile, which is dispatched from handleCreate BEFORE
// its own "#dry-run-create" check ever runs (the scaffold branch returns
// early) -- dry_run:true on an imports-only/package-only body silently
// wrote the scaffold file for real.
func TestHandleCreate_ScaffoldFileDryRunDoesNotWrite(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, err := s.handleCode(context.Background(), nil, codeParam{
		Op:     "create",
		File:   "extra/scaffold.go",
		Body:   "package extra\n\nimport \"fmt\"\n",
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("handleCode: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected dry-run scaffold create to succeed, got error: %s", text)
	}
	if !strings.Contains(text, "dry run") {
		t.Errorf("expected dry-run preview text, got: %s", text)
	}

	if _, statErr := os.Stat(filepath.Join(projDir, "extra/scaffold.go")); statErr == nil {
		t.Errorf("extra/scaffold.go should NOT have been written to disk under dry_run:true, but it exists")
	}
}

// TestHandleApply_BuildFailureDoesNotRevertDriftedGoMod is the #356
// regression: two real trajectories (2026-08-28/29 bug reports) found
// that any failed create/edit/apply call reverts go.mod to defn's
// stale DB blob -- even for a go.mod change already committed to HEAD
// -- because emit's project-files loop overwrites go.mod on every
// emit, unconditionally, including the emit that runs as part of a
// build check that then rolls back. This locks in that a manual
// go.mod edit (simulating a real dependency add + `go mod tidy`, with
// no follow-up full ingest/sync) survives an unrelated operation's
// build failure intact.
func TestHandleApply_BuildFailureDoesNotRevertDriftedGoMod(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	goModPath := filepath.Join(projDir, "go.mod")
	driftedGoMod := "module testproj\n\ngo 1.26\n\nrequire example.com/newdep v1.0.0\n"
	if err := os.WriteFile(goModPath, []byte(driftedGoMod), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	result, _, _ := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{
			{Op: "edit", Name: "Greet", NewBody: "func Greet(name string) string { return undefinedHelperFunc(name) }"},
		},
	})
	text := resultText(t, result)
	if !strings.Contains(text, "BUILD FAILED") {
		t.Fatalf("expected a build failure, got: %s", text)
	}

	finalGoMod, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read go.mod after failed build: %v", err)
	}
	if string(finalGoMod) != driftedGoMod {
		t.Errorf("#356: go.mod reverted to defn's stale DB blob despite the manual edit and unrelated build failure -- got:\n%s\nwant (unchanged):\n%s", finalGoMod, driftedGoMod)
	}
}

// TestHandleApply_CreateCommentSwallowedDeclRejectedWithHint is the
// same #356 regression as TestHandleCreate_CommentSwallowedDeclRejectedWithHint,
// but through op:"apply"'s own separate, independently-implemented
// create case -- which had NEITHER doubleEscapedHint NOR this new
// check wired in at all, unlike the standalone code(op:"create") path.
func TestHandleApply_CreateCommentSwallowedDeclRejectedWithHint(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	body := "func FuncA() {\n\treturn\n}\n\n// trailing docstring for FuncB\\nfunc FuncB() {\\n\treturn\\n}\n"
	result, _, err := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "create", Body: body, File: "swallow.go"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "swallowed") {
		t.Fatalf("expected a comment-swallowed-declaration hint, got: %s", text)
	}

	if _, lookErr := db.GetDefinitionByName("FuncB", ""); lookErr == nil {
		t.Fatalf("FuncB should never have been created as a real definition -- it was swallowed into a comment")
	}
}

// TestHandleCreate_CommentSwallowedDeclRejectedWithHint is the #356
// regression (2026-08-28 gemot bug report): a create body whose TAIL
// got double-escaped (a literal \n instead of a real newline) while
// the rest of the body used real newlines throughout used to slip
// past doubleEscapedHint's whole-body ratio heuristic entirely --
// there were plenty of real newlines elsewhere in the body. Go's `//`
// comment rule then silently swallowed the intended second function
// into one harmless-looking comment line, and create reported success
// with zero error even though the intended function was never really
// declared -- only caught later by `go build: undefined: FuncB`.
func TestHandleCreate_CommentSwallowedDeclRejectedWithHint(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	body := "func FuncA() {\n\treturn\n}\n\n// trailing docstring for FuncB\\nfunc FuncB() {\\n\treturn\\n}\n"
	result, _, err := s.handleCreate(context.Background(), nil, createParam{Body: body, File: "swallow.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "swallowed") {
		t.Fatalf("expected a comment-swallowed-declaration hint, got: %s", text)
	}

	if _, lookErr := db.GetDefinitionByName("FuncB", ""); lookErr == nil {
		t.Fatalf("FuncB should never have been created as a real definition -- it was swallowed into a comment")
	}
}

// TestHandleApply_CreateMultiDeclNewHyphenatedDirUsesBodyPackageName is
// the #357 regression through op:"apply"'s own independently
// implemented multi-decl create branch, which has the identical
// filepath.Base(dir)-derived package name bug as handleCreateMultiDecl.
func TestHandleApply_CreateMultiDeclNewHyphenatedDirUsesBodyPackageName(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	body := "package memtool\n\nfunc Run() {}\n\nfunc Stop() {}\n"
	result, _, err := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "create", Body: body, File: "integrations/anthropic-memtool/executor.go"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "Errors:") {
		t.Fatalf("expected a clean create, got: %s", text)
	}

	written, readErr := os.ReadFile(filepath.Join(projDir, "integrations", "anthropic-memtool", "executor.go"))
	if readErr != nil {
		t.Fatalf("read created file: %v", readErr)
	}
	if !strings.Contains(string(written), "package memtool") {
		t.Fatalf("#357: expected on-disk package clause to match the body's own `package memtool`, got:\n%s", written)
	}
}

// TestHandleApply_CreateSingleDeclNewHyphenatedDirUsesBodyPackageName is
// the #357 regression through op:"apply"'s independently implemented
// single-decl create branch, which has the identical
// filepath.Base(dir)-derived package name bug.
func TestHandleApply_CreateSingleDeclNewHyphenatedDirUsesBodyPackageName(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	body := "package memtool\n\nfunc Run() {}\n"
	result, _, err := s.handleApply(context.Background(), nil, applyParam{
		Operations: []applyOp{{Op: "create", Body: body, File: "integrations/anthropic-memtool/executor.go"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "Errors:") {
		t.Fatalf("expected a clean create, got: %s", text)
	}

	written, readErr := os.ReadFile(filepath.Join(projDir, "integrations", "anthropic-memtool", "executor.go"))
	if readErr != nil {
		t.Fatalf("read created file: %v", readErr)
	}
	if !strings.Contains(string(written), "package memtool") {
		t.Fatalf("#357: expected on-disk package clause to match the body's own `package memtool`, got:\n%s", written)
	}
}

// TestHandleCreateMultiDecl_NewHyphenatedDirUsesBodyPackageName is the
// #357 regression (2026-08-29 winze bug report): creating a whole new
// file (multi-decl body) into a not-yet-existing, hyphenated directory
// derived the new module's package name from the directory's basename
// ("anthropic-memtool", not a valid Go identifier) instead of the
// body's own `package memtool` line, producing an uncompilable file.
func TestHandleCreateMultiDecl_NewHyphenatedDirUsesBodyPackageName(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	body := "package memtool\n\nfunc Run() {}\n\nfunc Stop() {}\n"
	result, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "integrations/anthropic-memtool/executor.go",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("expected a clean create, got: %s", text)
	}

	written, readErr := os.ReadFile(filepath.Join(projDir, "integrations", "anthropic-memtool", "executor.go"))
	if readErr != nil {
		t.Fatalf("read created file: %v", readErr)
	}
	if !strings.Contains(string(written), "package memtool") {
		t.Fatalf("#357: expected on-disk package clause to match the body's own `package memtool`, got:\n%s", written)
	}
}

// TestHandleCreate_NewHyphenatedDirSingleDeclUsesBodyPackageName is the
// same #357 regression as TestHandleCreateMultiDecl_NewHyphenatedDirUsesBodyPackageName,
// but through handleCreate's single-decl new-directory path (a body
// with exactly one top-level decl, still led by its own `package X`
// line) rather than the multi-decl path.
func TestHandleCreate_NewHyphenatedDirSingleDeclUsesBodyPackageName(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	body := "package memtool\n\nfunc Run() {}\n"
	result, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "integrations/anthropic-memtool/executor.go",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("expected a clean create, got: %s", text)
	}

	written, readErr := os.ReadFile(filepath.Join(projDir, "integrations", "anthropic-memtool", "executor.go"))
	if readErr != nil {
		t.Fatalf("read created file: %v", readErr)
	}
	if !strings.Contains(string(written), "package memtool") {
		t.Fatalf("#357: expected on-disk package clause to match the body's own `package memtool`, got:\n%s", written)
	}
}

// TestHandleCreate_LeadingPackageDocCommentDoesNotBreakInference is the
// #357 regression (2026-08-29 winze bug report): a create body led by
// an idiomatic `// Package X ...` GoDoc comment, then `package X`, then
// declarations failed name inference outright ("couldn't infer
// definition name from body") -- stripLeadingPackageDecl only
// recognized a package clause as the very first thing in the body, so
// the synthetic "package x\n" prefix inferFromBody adds for parsing
// landed BEFORE the real package line instead of replacing it, and the
// comment sat between the two, producing invalid Go with two package
// clauses. Per-decl doc comments (not attached to the package clause)
// already worked fine -- only this specific package-doc-comment shape
// broke.
func TestHandleCreate_LeadingPackageDocCommentDoesNotBreakInference(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	body := "// Package memtool provides tool execution helpers.\npackage memtool\n\nfunc Run() {}\n"
	result, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "integrations/memtool/executor.go",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "couldn't infer") || strings.Contains(text, "rolled back") {
		t.Fatalf("#357: leading package-doc-comment broke create, got: %s", text)
	}
	if _, lookErr := db.GetDefinitionByName("Run", ""); lookErr != nil {
		t.Fatalf("Run should have been created: %v", lookErr)
	}

	written, readErr := os.ReadFile(filepath.Join(projDir, "integrations", "memtool", "executor.go"))
	if readErr != nil {
		t.Fatalf("read created file: %v", readErr)
	}
	if !strings.Contains(string(written), "package memtool") {
		t.Fatalf("#357: expected on-disk package clause to be memtool (from the body's own package line, past the doc comment), got:\n%s", written)
	}
}

// TestEnsureFresh_HealsWholeDirectoryDeletion is a repro for the 4th,
// unfiled bug in the 2026-08-29 winze dispatch message: after creating
// a package then deleting every file under its directory (and rmdir'ing
// the now-empty directory) from a live, never-restarted defn serve,
// outline/read/overview calls kept resolving to a fully phantom module
// with the old content. ensureFresh's own doc comment claims deleted
// files ARE healed (pruned via DeleteFile) -- this locks in that claim
// against the exact directory-deletion shape reported, rather than just
// a single file's removal.
func TestEnsureFresh_HealsWholeDirectoryDeletion(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "package widget\n\nfunc Run() {}\n\nfunc Stop() {}\n",
		File: "widget/exec.go",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, lookErr := db.GetDefinitionByName("Run", ""); lookErr != nil {
		t.Fatalf("Run should exist after create: %v", lookErr)
	}

	if err := os.RemoveAll(filepath.Join(projDir, "widget")); err != nil {
		t.Fatalf("remove widget dir: %v", err)
	}

	s.ensureFresh(nil)

	if _, lookErr := db.GetDefinitionByName("Run", ""); lookErr == nil {
		t.Fatalf("#357: Run still present in DB after its file+directory were deleted from disk -- phantom module bug")
	}

	outResult, _, outErr := s.handleCode(context.Background(), nil, codeParam{Op: "overview"})
	if outErr != nil {
		t.Fatalf("overview: %v", outErr)
	}
	overviewText := resultText(t, outResult)
	if strings.Contains(overviewText, "widget") {
		t.Fatalf("#357: overview still lists phantom widget module after directory deletion, got: %s", overviewText)
	}
}

// TestHandleCreateRejectsUnknownFile is the #357 update to the original
// "Bug C" fix: file: naming a NESTED directory with no known module is
// no longer an error -- #13 (added after Bug C) intentionally supports
// this by creating a module scoped to the new directory, and #357 fixed
// the bug that made #13's fallback dead code (an earlier unconditional
// check always returned first). Bug C's actual concern -- never
// silently fall back to some UNRELATED existing module -- is preserved:
// a file: with no directory component to scope a new module to still
// errors instead of guessing (see the newDirCandidate guard in
// handleCreate).
func TestHandleCreateRejectsUnknownFile(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func Nope() int { return 0 }",
		File: "no/such/newpkg/file.go",
	})
	text := resultText(t, result)
	if strings.Contains(text, "does not map to any known module") {
		t.Fatalf("expected create to succeed by scoping a new module to the new nested directory (#13/#357), got: %s", text)
	}
	if _, err := db.GetDefinitionByName("Nope", ""); err != nil {
		t.Fatalf("Nope should have been created in a new module scoped to no/such/newpkg: %v", err)
	}
}
