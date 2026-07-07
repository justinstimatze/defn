package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
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

// --- Pure function tests ---

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

// --- Dispatch validation tests ---

func TestHandleCodeValidation(t *testing.T) {
	s := &server{db: nil} // handlers will fail on DB access but validation runs first

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

// --- Integration tests with real DB ---

func setupTestDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.Open(dbPath)
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

func TestHandleEmit(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{db: db, projectDir: projDir}

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
	s := &server{db: db}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "emit"})
	text := resultText(t, result)
	if !strings.Contains(text, "out is required") {
		t.Errorf("expected 'out is required' error, got: %s", text)
	}
}

func TestHandleImpact(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleImpact(context.Background(), nil, codeParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "Greet") {
		t.Error("expected Greet in impact output")
	}
	if !strings.Contains(text, "Direct callers") || !strings.Contains(text, "Farewell") {
		t.Error("expected Farewell as a caller of Greet")
	}
}

func TestHandleImpact_Rank(t *testing.T) {
	// rank=true must not panic, must not lose callers, and must keep
	// the formatted output coherent. Score ordering is exercised
	// directly in internal/rank — here we just verify the wire-up.
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}
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
	s := &server{db: db}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "Hello") {
		t.Error("expected function body containing 'Hello'")
	}
}

func TestHandleEdit(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

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
	s := &server{db: db}

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
	s := &server{db: db}

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
	s := &server{db: db}

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
	s := &server{db: db}

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
	s := &server{db: db}

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
	s := &server{db: db}

	result, _, _ := s.handleSearch(context.Background(), nil, codeParam{Pattern: "%Greet%"})
	text := resultText(t, result)

	if !strings.Contains(text, "Greet") {
		t.Error("expected Greet in search results")
	}
}

func TestHandleCreate(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func NewHelper() string { return \"help\" }",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Created") {
		t.Errorf("expected 'Created', got: %s", text)
	}
}

// Bug C: op:create with multi-decl body must reject instead of silently
// dropping all but the first decl into a single Definition.
func TestHandleCreateRejectsMultiDecl(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	body := `const Limit = 10

func Helper() int { return Limit }

func Other() int { return 0 }`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{Body: body})
	text := resultText(t, result)

	if !strings.Contains(text, "top-level declarations") {
		t.Errorf("expected multi-decl rejection error, got: %s", text)
	}
	// Confirm nothing was created.
	if _, err := db.GetDefinitionByName("Helper", ""); err == nil {
		t.Error("Helper should not have been created when body was rejected")
	}
	if _, err := db.GetDefinitionByName("Other", ""); err == nil {
		t.Error("Other should not have been created when body was rejected")
	}
}

// Bug C: op:create with file: param must route the new def into that file
// (SourceFile populated on the stored Definition).
func TestHandleCreateHonorsFileParam(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

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

// Bug C: when file: maps to no known module, return an error rather than
// silently falling back to the first module.
func TestHandleCreateRejectsUnknownFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func Nope() int { return 0 }",
		File: "no/such/package/file.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "does not map to any known module") {
		t.Errorf("expected unknown-module error, got: %s", text)
	}
	if _, err := db.GetDefinitionByName("Nope", ""); err == nil {
		t.Error("Nope should not have been created when file is unknown")
	}
}

func TestHandleRename(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

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

// --- helpers ---

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
	s := &server{db: db}

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

	s := &server{db: db, projectDir: projDir}
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

// TestExtractSlices, TestExtractSlices_UnknownKind, TestExtractSlices_NoMatches
// were removed as part of Phase C extract-slice consolidation — the pure
// implementation lives in internal/projection, which has richer coverage
// (30 replace-slice goldens + byte-exact invariant asserts). handleSlice
// integration coverage remains via TestHandleSlice_* below.

func TestHandleSlice_MissingArgs(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

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
	s := &server{db: db}

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
