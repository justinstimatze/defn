package mcp

import (
	"strings"
	"testing"
)

// TestLeanToolDescription_AdvertisesReadFileForWholeFileCoverage locks
// in the #369 fix: real trajectory data (2026-09-09) found read-file --
// present in the op list but with zero disambiguating guidance -- was
// the one path that matched native Read's call count exactly, while
// sessions mixing read/outline/expand for the same file used 1.6-2.5x
// more calls. The description must point at it specifically, not just
// list it alongside 17 other read-shaped ops.
func TestLeanToolDescription_AdvertisesReadFileForWholeFileCoverage(t *testing.T) {
	if !strings.Contains(leanToolDescription, "read-file") {
		t.Fatal("expected leanToolDescription to mention read-file")
	}
	if !strings.Contains(leanToolDescription, "cheapest way to cover a file") {
		t.Errorf("expected explicit read-file guidance in leanToolDescription, got: %s", leanToolDescription)
	}
}
