package projection

import (
	"go/format"
	"strings"
	"testing"
)

type renameParamFixture struct {
	name    string
	body    string
	oldName string
	newName string
	after   string
}

var renameParamFixtures = []renameParamFixture{
	{
		name:    "simple_value_param",
		body:    `func F(x int) int { return x }`,
		oldName: "x", newName: "y",
		after: `func F(y int) int { return y }`,
	},
	{
		name: "first_of_multi_param",
		body: `func F(a, b int) int {
	return a + b
}`,
		oldName: "a", newName: "x",
		after: `func F(x, b int) int {
	return x + b
}`,
	},
	{
		name: "pointer_receiver",
		body: `func (s *Server) Handle(r *Request) error {
	return s.process(r)
}`,
		oldName: "s", newName: "srv",
		after: `func (srv *Server) Handle(r *Request) error {
	return srv.process(r)
}`,
	},
	{
		name: "value_receiver",
		body: `func (p Point) X() int {
	return p.x
}`,
		oldName: "p", newName: "pt",
		after: `func (pt Point) X() int {
	return pt.x
}`,
	},
	{
		name: "param_used_in_range",
		body: `func F(items []int) int {
	total := 0
	for _, item := range items {
		total += item
	}
	return total
}`,
		oldName: "items", newName: "xs",
		after: `func F(xs []int) int {
	total := 0
	for _, item := range xs {
		total += item
	}
	return total
}`,
	},
	{
		name: "param_used_in_closure",
		body: `func F(x int) int {
	f := func() int { return x + 1 }
	return f()
}`,
		oldName: "x", newName: "n",
		after: `func F(n int) int {
	f := func() int { return n + 1 }
	return f()
}`,
	},
	{
		name: "param_used_in_composite_literal",
		body: `func F(name string) Person {
	return Person{Name: name, Age: 0}
}`,
		oldName: "name", newName: "n",
		after: `func F(n string) Person {
	return Person{Name: n, Age: 0}
}`,
	},
	{
		name: "param_used_in_address_of",
		body: `func F(cfg Config) *Config {
	return &cfg
}`,
		oldName: "cfg", newName: "c",
		after: `func F(c Config) *Config {
	return &c
}`,
	},
	{
		name: "param_shadowed_in_inner_block",
		body: `func F(x int) int {
	{
		x := 100
		_ = x
	}
	return x
}`,
		oldName: "x", newName: "y",
		after: `func F(y int) int {
	{
		x := 100
		_ = x
	}
	return y
}`,
	},
	{
		name: "map_param_used_in_range",
		body: `func F(m map[string]int) int {
	total := 0
	for k, v := range m {
		if k != "" {
			total += v
		}
	}
	return total
}`,
		oldName: "m", newName: "dict",
		after: `func F(dict map[string]int) int {
	total := 0
	for k, v := range dict {
		if k != "" {
			total += v
		}
	}
	return total
}`,
	},
}

// gofmtBody canonicalizes a body via go/format so tests can assert
// ≡_gofmt equivalence without whitespace flakiness.
func gofmtBody(t *testing.T, body string) string {
	t.Helper()
	src := "package p\n" + body
	out, err := format.Source([]byte(src))
	if err != nil {
		t.Fatalf("gofmt of body failed: %v\nbody:\n%s", err, body)
	}
	s := string(out)
	for _, p := range []string{"package p\n\n", "package p\n"} {
		if strings.HasPrefix(s, p) {
			s = s[len(p):]
			break
		}
	}
	return strings.TrimRight(s, "\n")
}

func TestRenameParam_GofmtEquivPUTGET(t *testing.T) {
	for _, tc := range renameParamFixtures {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenameParam(tc.body, tc.oldName, tc.newName)
			if err != nil {
				t.Fatalf("RenameParam: %v", err)
			}
			gotFmt := gofmtBody(t, got)
			wantFmt := gofmtBody(t, tc.after)
			if gotFmt != wantFmt {
				t.Errorf("≡_gofmt PUTGET failed for %q\n--- want (gofmt) ---\n%s\n--- got (gofmt) ---\n%s", tc.name, wantFmt, gotFmt)
			}
		})
	}
}

func TestRenameParam_ErrorCases(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		oldName string
		newName string
		want    string
	}{
		{"empty_body", "", "x", "y", "body is empty"},
		{"empty_old", "func F() {}", "", "y", "old_param is required"},
		{"empty_new", "func F() {}", "x", "", "new_param is required"},
		{"unparseable", "not go", "x", "y", "parse body"},
		{"not_a_func", "type T struct{}", "x", "y", "not a function declaration"},
		{"no_such_param", `func F() {}`, "x", "y", `no param named "x"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RenameParam(tc.body, tc.oldName, tc.newName)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q did not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestRenameParam_NoOpSameName(t *testing.T) {
	body := `func F(x int) int { return x }`
	got, err := RenameParam(body, "x", "x")
	if err != nil {
		t.Fatalf("RenameParam no-op: %v", err)
	}
	if got != body {
		t.Errorf("expected no-op to return body verbatim\nwant: %q\ngot:  %q", body, got)
	}
}
