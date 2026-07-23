package mcp

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestWithUsagePrefixAndBodyHashes locks in #177: withUsage emits
// prefix_hash_100 and body_sha256 in the stderr JSON so bench
// harnesses can detect cache-drift and cross-call dedup opportunities.
func TestWithUsagePrefixAndBodyHashes(t *testing.T) {
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	body := "## Foo (function)\n\n```go\nfunc Foo() int { return 42 }\n```\n" +
		strings.Repeat("padding\n", 20)
	result := &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: body}},
	}
	withUsage(result, usageStats{Op: "read", BytesReturned: len(body)})

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	var line string
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "defn-usage ") {
			line = strings.TrimPrefix(sc.Text(), "defn-usage ")
			break
		}
	}
	if line == "" {
		t.Fatalf("no defn-usage line emitted; got %q", out)
	}

	var got usageStats
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("bad JSON: %v (%q)", err, line)
	}

	fullExp := sha256.Sum256([]byte(body))
	wantBody := hex.EncodeToString(fullExp[:8])
	if got.BodySHA256 != wantBody {
		t.Errorf("BodySHA256 = %q, want %q", got.BodySHA256, wantBody)
	}
	prefixExp := sha256.Sum256([]byte(body[:100]))
	wantPrefix := hex.EncodeToString(prefixExp[:8])
	if got.PrefixHash100 != wantPrefix {
		t.Errorf("PrefixHash100 = %q, want %q", got.PrefixHash100, wantPrefix)
	}
}

// TestWithUsageShortBodyPrefix verifies the prefix hash uses the full
// body when it's shorter than 100 bytes.
func TestWithUsageShortBodyPrefix(t *testing.T) {
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	body := "tiny"
	result := &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: body}},
	}
	withUsage(result, usageStats{Op: "read", BytesReturned: len(body)})
	w.Close()
	out, _ := io.ReadAll(r)

	var line string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "defn-usage ") {
			line = strings.TrimPrefix(sc.Text(), "defn-usage ")
		}
	}
	var got usageStats
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	fullExp := sha256.Sum256([]byte(body))
	if got.BodySHA256 != hex.EncodeToString(fullExp[:8]) {
		t.Errorf("BodySHA256 wrong: %q", got.BodySHA256)
	}
	if got.PrefixHash100 != got.BodySHA256 {
		t.Errorf("PrefixHash100 should equal BodySHA256 for short body; got %q vs %q",
			got.PrefixHash100, got.BodySHA256)
	}
}

// TestEmitUsageLogFileSink verifies DEFN_USAGE_LOG_FILE redirects the
// stderr JSONL to a file — the plumbing #180 and #174 diagnostics rely
// on. Bench harnesses set this per-arm so the usage stream is captured
// alongside stream-json turn files.
func TestEmitUsageLogFileSink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	t.Setenv("DEFN_USAGE_LOG_FILE", path)

	emitUsageLog(usageStats{Op: "read", BytesReturned: 123, PrefixHash100: "abcd", BodySHA256: "efgh"})
	emitUsageLog(usageStats{Op: "outline", BytesReturned: 45})

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), got)
	}
	for i, want := range []string{`"op":"read"`, `"op":"outline"`} {
		if !strings.HasPrefix(lines[i], "defn-usage ") {
			t.Errorf("line %d missing defn-usage prefix: %q", i, lines[i])
		}
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d missing %q: %q", i, want, lines[i])
		}
	}
}
