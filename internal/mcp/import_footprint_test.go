package mcp

import (
	"testing"
)

func TestBodyImportFootprintUnchanged(t *testing.T) {
	cases := []struct {
		name string
		old  string
		new  string
		want bool
	}{
		{
			name: "identical_body",
			old:  "func F() { fmt.Println(\"a\") }",
			new:  "func F() { fmt.Println(\"a\") }",
			want: true,
		},
		{
			name: "same_packages_different_calls",
			old:  "func F() { fmt.Println(\"a\") }",
			new:  "func F() { fmt.Println(\"b\") }",
			want: true,
		},
		{
			name: "adds_new_package",
			old:  "func F() { fmt.Println(\"a\") }",
			new:  "func F() { fmt.Println(\"a\"); strings.TrimSpace(\"b\") }",
			want: false,
		},
		{
			name: "drops_only_usage_of_a_package",
			old:  "func F() { fmt.Println(\"a\"); strings.TrimSpace(\"b\") }",
			new:  "func F() { fmt.Println(\"a\") }",
			want: false,
		},
		{
			name: "shadowed_local_var_named_like_package",
			old:  "func F() { fmt.Println(\"a\") }",
			new:  "func F() { fmt := 1; _ = fmt }",
			want: false,
		},
		{
			name: "new_local_var_with_selector_is_excluded_not_flagged",
			old:  "func F() { fmt.Println(\"a\") }",
			new:  "func F() { fmt.Println(\"a\"); cfg := config{}; cfg.Set() }",
			want: true,
		},
		{
			name: "non_func_decl_is_conservative",
			old:  "var X = 1",
			new:  "var X = 2",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bodyImportFootprintUnchanged(c.old, c.new)
			if got != c.want {
				t.Errorf("bodyImportFootprintUnchanged(%q, %q) = %v, want %v", c.old, c.new, got, c.want)
			}
		})
	}
}
