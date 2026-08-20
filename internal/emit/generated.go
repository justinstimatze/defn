package emit

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
)

// generatedFileMarker matches Go's own generated-code convention
// (https://go.dev/s/generatedcode): a line matching this pattern exactly
// marks a file as machine-generated. Recognized by goimports, golint,
// and most other Go tooling -- reusing it here rather than inventing a
// new convention.
var generatedFileMarker = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// isGeneratedFile reports whether the file at path carries Go's standard
// generated-code marker near its top. Only the first ~4KB is read -- the
// convention requires the marker near the top of the file, and capping
// the read keeps this cheap even for huge generated files (parser
// tables, protobuf output).
func IsGeneratedFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(f, buf)
	scanner := bufio.NewScanner(bytes.NewReader(buf[:n]))
	for i := 0; scanner.Scan() && i < 10; i++ {
		if generatedFileMarker.MatchString(strings.TrimRight(scanner.Text(), "\r")) {
			return true
		}
	}
	return false
}
