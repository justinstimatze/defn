package mcp

import (
	"strings"
	"testing"
)

// TestTopLinesOfBody covers the four shapes the helper needs to handle:
// empty body, body shorter than n lines, body exactly n lines, body
// longer than n lines (elision marker appended).
func TestTopLinesOfBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		n    int
		want string
	}{
		{name: "empty", body: "", n: 5, want: ""},
		{name: "n_zero", body: "func F() {}", n: 0, want: ""},
		{name: "shorter", body: "line1\nline2", n: 5, want: "line1\nline2"},
		{name: "exact", body: "l1\nl2\nl3", n: 3, want: "l1\nl2\nl3"},
		{name: "longer",
			body: "l1\nl2\nl3\nl4\nl5\nl6",
			n:    3,
			want: "l1\nl2\nl3\n…"},
		{name: "long_single_line", body: strings.Repeat("x", 200), n: 5,
			want: strings.Repeat("x", 200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := topLinesOfBody(tc.body, tc.n)
			if got != tc.want {
				t.Errorf("topLinesOfBody(%q, %d) = %q, want %q",
					tc.body, tc.n, got, tc.want)
			}
		})
	}
}
