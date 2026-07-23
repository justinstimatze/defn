package projection

import (
	"strings"
	"testing"
)

// Contract fixtures for FilterBodyByQuery (task #153). The projection
// keeps only top-level statements whose source contains any query
// token (case-insensitive, ≥2 chars). Non-matching runs collapse to
// one elision comment.

func TestFilterBodyByQuery_KeepsMatchingBranch(t *testing.T) {
	body := `func handleRequest(w http.ResponseWriter, r *http.Request) {
	if !authenticated(r) {
		w.WriteHeader(401)
		return
	}
	if r.Method == "POST" {
		handlePost(w, r)
		return
	}
	if r.Method == "GET" {
		handleGet(w, r)
		return
	}
	w.WriteHeader(405)
}`
	out, kept, elided := FilterBodyByQuery(body, "401")
	if kept != 1 || elided != 3 {
		t.Errorf("expected kept=1 elided=3, got kept=%d elided=%d", kept, elided)
	}
	if !strings.Contains(out, "401") {
		t.Errorf("output should retain the 401 branch\n---\n%s", out)
	}
	for _, dropped := range []string{"handlePost", "handleGet", "405"} {
		if strings.Contains(out, dropped) {
			t.Errorf("output should NOT contain %q\n---\n%s", dropped, out)
		}
	}
	if !strings.Contains(out, "elided") {
		t.Errorf("output should include an elision placeholder\n---\n%s", out)
	}
}

func TestFilterBodyByQuery_TokenSplitting(t *testing.T) {
	body := `func f() {
	go doA()
	go doB()
	go doC()
	go doD()
}`
	// Query is multi-word; should match statements containing "doB" OR "doD".
	out, kept, _ := FilterBodyByQuery(body, "doB doD")
	if kept != 2 {
		t.Errorf("want kept=2 for 2-word query, got %d\n---\n%s", kept, out)
	}
	if !strings.Contains(out, "doB()") || !strings.Contains(out, "doD()") {
		t.Errorf("output should keep both matching branches\n---\n%s", out)
	}
}

func TestFilterBodyByQuery_NoTokensReturnsUnchanged(t *testing.T) {
	body := `func f() { doA(); doB() }`
	out, kept, elided := FilterBodyByQuery(body, "")
	if out != body {
		t.Errorf("empty query should return body verbatim\n---\n%s", out)
	}
	if kept != 0 || elided != 0 {
		t.Errorf("expected 0/0 counts on no-op, got kept=%d elided=%d", kept, elided)
	}
}

func TestFilterBodyByQuery_AllMatchIsNoOp(t *testing.T) {
	body := `func f() {
	auth1()
	auth2()
	auth3()
}`
	out, kept, elided := FilterBodyByQuery(body, "auth")
	// No-op contract: kept=elided=0 when filter didn't apply.
	if kept != 0 || elided != 0 {
		t.Errorf("all-match no-op should return kept=0 elided=0, got kept=%d elided=%d", kept, elided)
	}
	if out != body {
		t.Errorf("all-match should return body unchanged\n---got---\n%s\n---want---\n%s", out, body)
	}
}

func TestFilterBodyByQuery_ShortBodyPassThrough(t *testing.T) {
	body := `func f() { return }`
	out, _, elided := FilterBodyByQuery(body, "nonexistent")
	if out != body || elided != 0 {
		t.Errorf("body with <2 stmts should return unchanged (got elided=%d)\n---\n%s", elided, out)
	}
}

func TestFilterBodyByQuery_Unparseable(t *testing.T) {
	body := `not a valid go body`
	out, _, _ := FilterBodyByQuery(body, "anything")
	if out != body {
		t.Errorf("unparseable body should pass through unchanged")
	}
}

// Compression sanity: on a branch-heavy body, filtering by a specific
// token should meaningfully shrink the output.
func TestFilterBodyByQuery_Compresses(t *testing.T) {
	body := `func dispatch(cmd string) error {
	switch cmd {
	case "auth":
		return handleAuth()
	}
	if cmd == "start" {
		return handleStart()
	}
	if cmd == "stop" {
		return handleStop()
	}
	if cmd == "restart" {
		return handleRestart()
	}
	return errors.New("unknown: " + cmd)
}`
	out, _, elided := FilterBodyByQuery(body, "auth")
	if elided < 1 {
		t.Fatalf("expected some elision, got %d\n---\n%s", elided, out)
	}
	if len(out) >= len(body) {
		t.Errorf("filtered output should be shorter; got %d vs original %d\n---\n%s", len(out), len(body), out)
	}
}

func TestExtractQueryTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"handles 401?", []string{"handles", "401"}},
		{"", nil},
		{"a", nil}, // too short
		{"do X and Y", []string{"do", "and"}},
		{"snake_case", []string{"snake_case"}},
		{"Kebab-case", []string{"kebab", "case"}},
		{"CamelCase", []string{"camelcase"}},
	}
	for _, tc := range cases {
		got := extractQueryTokens(tc.in)
		if !stringSlicesEqual(got, tc.want) {
			t.Errorf("extractQueryTokens(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
