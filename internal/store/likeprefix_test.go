package store

import "testing"

func TestLikePrefixRange(t *testing.T) {
	cases := []struct {
		pattern string
		lo, hi  string
		ok      bool
		why     string
	}{
		{"Note00042%", "Note00042", "Note00043", true, "anchored prefix"},
		{"a%", "a", "b", true, "single char"},
		{"%Provenance%", "", "", false, "unanchored — no range equivalent"},
		{"%foo", "", "", false, "suffix match"},
		{"Note_0042%", "", "", false, "interior single-char wildcard"},
		{"a%b%", "", "", false, "interior wildcard"},
		{"%", "", "", false, "bare wildcard"},
		{"", "", "", false, "empty"},
		{"plain", "", "", false, "no wildcard at all — exact match, not a prefix"},
		{"esc\\%ape%", "", "", false, "escape present"},
	}
	for _, c := range cases {
		lo, hi, ok := likePrefixRange(c.pattern)
		if ok != c.ok || lo != c.lo || hi != c.hi {
			t.Errorf("likePrefixRange(%q) = (%q,%q,%v), want (%q,%q,%v) — %s",
				c.pattern, lo, hi, ok, c.lo, c.hi, c.ok, c.why)
		}
	}
}

// The upper bound must be the byte successor: SQLite's BINARY collation is
// memcmp, and UTF-8-encoding the incremented rune would sort wrong.
func TestLikePrefixRangeIsByteWise(t *testing.T) {
	lo, hi, ok := likePrefixRange("caf\xc3\xa9%") // "café" in UTF-8
	if !ok {
		t.Fatal("valid prefix rejected")
	}
	if len(hi) != len(lo) {
		t.Errorf("upper bound changed length: %q (%d) vs %q (%d) — rune-encoded instead of byte-incremented",
			hi, len(hi), lo, len(lo))
	}
	if hi <= lo {
		t.Errorf("upper bound %q does not sort above %q", hi, lo)
	}
	// 0xFF cannot be incremented within one byte; fall back rather than wrap.
	if _, _, ok := likePrefixRange("a\xff%"); ok {
		t.Error("0xFF terminal byte should fall back to LIKE")
	}
}

// Every value the range matches must be one LIKE would have matched.
func TestLikePrefixRangeAgreesWithLike(t *testing.T) {
	lo, hi, ok := likePrefixRange("Note0004%")
	if !ok {
		t.Fatal("prefix rejected")
	}
	inRange := func(s string) bool { return s >= lo && s < hi }
	for _, s := range []string{"Note0004", "Note00040", "Note0004Z", "Note0004\xff"} {
		if !inRange(s) {
			t.Errorf("%q should be in range [%q,%q)", s, lo, hi)
		}
	}
	for _, s := range []string{"Note0003", "Note0005", "Note000", "XNote0004"} {
		if inRange(s) {
			t.Errorf("%q should NOT be in range [%q,%q)", s, lo, hi)
		}
	}
}
