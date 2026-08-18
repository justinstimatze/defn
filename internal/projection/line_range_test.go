package projection

import (
	"testing"
)

func TestParseLineRange(t *testing.T) {
	cases := []struct {
		spec               string
		wantStart, wantEnd int
		wantErr            bool
	}{
		{"700-820", 700, 820, false},
		{"700:820", 700, 820, false},
		{" 700 - 820 ", 700, 820, false},
		{"820-700", 700, 820, false}, // reversed, swapped
		{"0-10", 1, 10, false},       // clamped up to 1
		{"-5-10", 0, 0, true},        // ambiguous leading sign, malformed
		{"abc-10", 0, 0, true},
		{"10", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, c := range cases {
		start, end, err := ParseLineRange(c.spec)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseLineRange(%q): want error, got start=%d end=%d", c.spec, start, end)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseLineRange(%q): unexpected error: %v", c.spec, err)
			continue
		}
		if start != c.wantStart || end != c.wantEnd {
			t.Errorf("ParseLineRange(%q) = (%d, %d), want (%d, %d)", c.spec, start, end, c.wantStart, c.wantEnd)
		}
	}
}

func TestExtractLineRange(t *testing.T) {
	// body occupies file lines 100-104 (5 lines, bodyStartLine=100).
	body := "line100\nline101\nline102\nline103\nline104"

	t.Run("middle slice", func(t *testing.T) {
		got, actualStart, actualEnd, ok := ExtractLineRange(body, 100, 101, 102)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if got != "line101\nline102" {
			t.Errorf("got %q", got)
		}
		if actualStart != 101 || actualEnd != 102 {
			t.Errorf("got actual range (%d, %d), want (101, 102)", actualStart, actualEnd)
		}
	})

	t.Run("clamped past end", func(t *testing.T) {
		got, actualStart, actualEnd, ok := ExtractLineRange(body, 100, 103, 999)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if got != "line103\nline104" {
			t.Errorf("got %q", got)
		}
		if actualStart != 103 || actualEnd != 104 {
			t.Errorf("got actual range (%d, %d), want (103, 104)", actualStart, actualEnd)
		}
	})

	t.Run("clamped before start", func(t *testing.T) {
		got, actualStart, actualEnd, ok := ExtractLineRange(body, 100, 1, 101)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if got != "line100\nline101" {
			t.Errorf("got %q", got)
		}
		if actualStart != 100 || actualEnd != 101 {
			t.Errorf("got actual range (%d, %d), want (100, 101)", actualStart, actualEnd)
		}
	})

	t.Run("whole body", func(t *testing.T) {
		got, _, _, ok := ExtractLineRange(body, 100, 100, 104)
		if !ok || got != body {
			t.Errorf("got %q, ok=%v, want full body", got, ok)
		}
	})

	t.Run("entirely before body", func(t *testing.T) {
		_, _, _, ok := ExtractLineRange(body, 100, 1, 50)
		if ok {
			t.Errorf("want ok=false for range entirely before body")
		}
	})

	t.Run("entirely after body", func(t *testing.T) {
		_, _, _, ok := ExtractLineRange(body, 100, 200, 300)
		if ok {
			t.Errorf("want ok=false for range entirely after body")
		}
	})
}

func TestBodyStartLine(t *testing.T) {
	cases := []struct {
		name               string
		body               string
		startLine, endLine int
		want               int
	}{
		{
			name:      "no doc comment",
			body:      "func F() {\n\treturn\n}",
			startLine: 100, endLine: 102,
			want: 100,
		},
		{
			name:      "one-line doc comment",
			body:      "// F does X.\nfunc F() {\n\treturn\n}",
			startLine: 101, endLine: 103,
			want: 100,
		},
		{
			name:      "multi-line doc comment",
			body:      "// F does X.\n// And Y.\n// And Z.\nfunc F() {\n\treturn\n}",
			startLine: 103, endLine: 105,
			want: 100,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BodyStartLine(c.body, c.startLine, c.endLine)
			if got != c.want {
				t.Errorf("BodyStartLine() = %d, want %d", got, c.want)
			}
		})
	}
}
