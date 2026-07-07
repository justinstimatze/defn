package main

import (
	"fmt"
	"strings"
)

// buildBigImporterFile returns a ~200-line Go file with 15 realistic stdlib
// imports and 20 short functions. Used by the big-add-import mutation case
// so files-mode's Read pays a real byte-cost.
func buildBigImporterFile() string {
	var b strings.Builder
	b.WriteString("package fixture\n\nimport (\n")
	for _, p := range []string{
		"bufio", "bytes", "context", "encoding/json", "errors",
		"fmt", "io", "log", "os", "path/filepath",
		"sort", "strconv", "strings", "sync", "time",
	} {
		fmt.Fprintf(&b, "\t%q\n", p)
	}
	b.WriteString(")\n\nvar mu sync.Mutex\nvar startedAt = time.Now()\n\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "// Op%d performs operation number %d.\n", i, i)
		fmt.Fprintf(&b, "func Op%d(ctx context.Context, in []byte) ([]byte, error) {\n", i)
		b.WriteString("\tmu.Lock()\n\tdefer mu.Unlock()\n")
		b.WriteString("\tif len(in) == 0 {\n\t\treturn nil, fmt.Errorf(\"op: empty input\")\n\t}\n")
		fmt.Fprintf(&b, "\tstart := time.Now()\n\tvar buf bytes.Buffer\n")
		fmt.Fprintf(&b, "\tbuf.WriteString(strconv.Itoa(%d))\n", i)
		b.WriteString("\tbuf.Write(in)\n")
		fmt.Fprintf(&b, "\tif buf.Len() > 1024 {\n\t\treturn nil, fmt.Errorf(\"op%d: too large\")\n\t}\n", i)
		b.WriteString("\tlog.Printf(\"op done in %s\", time.Since(start))\n")
		b.WriteString("\treturn buf.Bytes(), nil\n}\n\n")
	}
	// Trailing helpers so every declared import is used (otherwise
	// defn's autoEmitAndBuild would fail on "imported and not used").
	b.WriteString(`
func ReadLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(s, "\n"), nil
}

func WriteJSON(path string, v any) error {
	f, err := os.Create(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(v)
}

func SortStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

var ErrNotFound = errors.New("not found")
`)
	return b.String()
}

// buildBigProcessFile returns a ~50-line function where the parameter `data`
// is used many times, so a rename-param has non-trivial scope. Files-mode
// has to update every use; defn-mode calls rename-param.
func buildBigProcessFile() string {
	var b strings.Builder
	b.WriteString(`package fixture

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Process consumes data, normalizes it, and returns the count.
func Process(data []byte, verbose bool) (int, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("process: empty data")
	}
	if verbose {
		fmt.Printf("process: %d bytes\n", len(data))
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return 0, fmt.Errorf("process: whitespace-only data")
	}
	// Try to decode as JSON — the shape doesn't matter, just the try.
	var raw any
	if err := json.Unmarshal(data, &raw); err == nil {
		if verbose {
			fmt.Printf("process: decoded %d bytes as JSON\n", len(data))
		}
		if arr, ok := raw.([]any); ok {
			return len(arr), nil
		}
	}
	if bytes.HasPrefix(data, []byte("\xff")) {
		return -1, fmt.Errorf("process: unsupported header in data")
	}
	// Fall back to counting non-whitespace runs.
	count := 0
	inRun := false
	for _, b := range data {
		if b == ' ' || b == '\t' || b == '\n' {
			inRun = false
			continue
		}
		if !inRun {
			count++
			inRun = true
		}
	}
	if verbose {
		fmt.Printf("process: fell back to run-counting, got %d runs from %d bytes\n", count, len(data))
	}
	return count, nil
}
`)
	return b.String()
}

// buildBigMultiReturnFile returns a ~60-line function with 8 return statements,
// so the target of replace-slice needs an index to be unambiguous. Files-mode
// has to Read + locate + edit a specific one.
func buildBigMultiReturnFile() string {
	return `package fixture

import (
	"errors"
	"fmt"
	"strings"
)

// Classify examines a string and returns a category. Multiple return
// statements exist for readability; the caller cares about which category
// they get, and the LAST return is the "unknown" branch.
func Classify(s string) (string, error) {
	if s == "" {
		return "", errors.New("empty input")
	}
	if len(s) > 1024 {
		return "", fmt.Errorf("too long: %d chars", len(s))
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return "url", nil
	}
	if strings.HasPrefix(s, "/") {
		return "path", nil
	}
	if strings.Contains(s, "@") && strings.Contains(s, ".") {
		return "email", nil
	}
	if _, err := fmt.Sscanf(s, "%d", new(int)); err == nil {
		return "int", nil
	}
	if strings.HasSuffix(s, ".go") {
		return "go-file", nil
	}
	// Fallthrough: no category matched.
	return "unknown", nil
}
`
}
