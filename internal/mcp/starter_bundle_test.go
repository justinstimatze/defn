package mcp

import (
	"testing"
)

// TestHasIdentifierShapedToken locks in the #369 gate: pure filler and
// bench-harness task preamble must NOT look identifier-shaped, but a
// real question naming an actual Go symbol must.
func TestHasIdentifierShapedToken(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"good call do it", false},
		{"You are working in a Go repository. Please solve the following issue.", false},
		{"ok", false},
		{"why does ParseVector panic on empty input", true},
		{"what does handleEdit do", true},
		{"check some_func for the bug", true},
		{"HTTPServer times out under load", true},
	}
	for _, c := range cases {
		if got := hasIdentifierShapedToken(c.text); got != c.want {
			t.Errorf("hasIdentifierShapedToken(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
