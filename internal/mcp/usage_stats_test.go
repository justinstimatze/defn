package mcp

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
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
