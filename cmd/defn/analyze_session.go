package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// turnMetrics captures the cost and tool-use signal for a single
// claude -p turn as parsed from a stream-json file. Populated by
// parseTurnStreamJSON.
type turnMetrics struct {
	Turn                     int            `json:"turn"`
	CacheCreationInputTokens int            `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int            `json:"cache_read_input_tokens"`
	OutputTokens             int            `json:"output_tokens"`
	CostUSD                  float64        `json:"cost_usd"`
	ToolCallCount            int            `json:"tool_call_count"`
	ToolOutputBytes          int            `json:"tool_output_bytes"`
	ToolCallsByName          map[string]int `json:"tool_calls_by_name"`
	DurationMS               int            `json:"duration_ms"`
}

// analyzeArm is one arm of a session-cumulative bench ("files" or
// "defn"). Holds the per-turn metrics plus computed totals.
type analyzeArm struct {
	Name   string        `json:"name"`
	Path   string        `json:"path"`
	Turns  []turnMetrics `json:"turns"`
	Totals turnMetrics   `json:"totals"`
}

// analyzeIntField extracts a numeric field from a decoded JSON map,
// tolerating both float64 (stdlib default) and int representations.
// Analyze-scoped name to avoid collision with cmd/defn-bench.intField.
func analyzeIntField(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// parseTurnStreamJSON parses one stream-json turn file (one JSON
// object per line as emitted by claude -p --output-format stream-json)
// and returns the aggregated turnMetrics. Tool calls are counted from
// assistant.content[].type == "tool_use" blocks; tool output bytes
// from user.content[].type == "tool_result" content. Usage totals
// come from the terminal result event, NOT from summing per-iteration
// assistant.usage events (those carry deltas that would double-count).
func parseTurnStreamJSON(path string) (turnMetrics, error) {
	f, err := os.Open(path)
	if err != nil {
		return turnMetrics{}, err
	}
	defer f.Close()

	m := turnMetrics{ToolCallsByName: map[string]int{}}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		var raw map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}
		switch raw["type"] {
		case "assistant":
			msg, _ := raw["message"].(map[string]any)
			content, _ := msg["content"].([]any)
			for _, c := range content {
				cb, _ := c.(map[string]any)
				if cb["type"] == "tool_use" {
					name, _ := cb["name"].(string)
					m.ToolCallCount++
					m.ToolCallsByName[name]++
				}
			}
		case "user":
			msg, _ := raw["message"].(map[string]any)
			content, _ := msg["content"].([]any)
			for _, c := range content {
				cb, _ := c.(map[string]any)
				if cb["type"] != "tool_result" {
					continue
				}
				switch inner := cb["content"].(type) {
				case []any:
					for _, ic := range inner {
						icb, _ := ic.(map[string]any)
						if icb["type"] == "text" {
							txt, _ := icb["text"].(string)
							m.ToolOutputBytes += len(txt)
						}
					}
				case string:
					m.ToolOutputBytes += len(inner)
				}
			}
		case "result":
			u, _ := raw["usage"].(map[string]any)
			m.CacheCreationInputTokens = analyzeIntField(u, "cache_creation_input_tokens")
			m.CacheReadInputTokens = analyzeIntField(u, "cache_read_input_tokens")
			m.OutputTokens = analyzeIntField(u, "output_tokens")
			if v, ok := raw["total_cost_usd"].(float64); ok {
				m.CostUSD = v
			}
			m.DurationMS = analyzeIntField(raw, "duration_ms")
		}
	}
	return m, scanner.Err()
}

// hasTurnFiles reports whether dir contains at least one turn-NN.json
// file. Used to distinguish single-arm dirs from multi-arm parent dirs
// and to skip stray subdirs (session-id.txt logs, err files, etc.).
func hasTurnFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "turn-") && strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".err") {
			return true
		}
	}
	return false
}

// loadArm parses every turn-NN.json file in dir (skipping .err files)
// and folds them into an analyzeArm with per-turn metrics and
// aggregated totals.
func loadArm(name, dir string) (analyzeArm, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return analyzeArm{}, err
	}
	arm := analyzeArm{Name: name, Path: dir, Totals: turnMetrics{ToolCallsByName: map[string]int{}}}
	var turnFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, "turn-") && strings.HasSuffix(n, ".json") && !strings.HasSuffix(n, ".err") {
			turnFiles = append(turnFiles, n)
		}
	}
	sort.Strings(turnFiles)
	for i, tf := range turnFiles {
		m, err := parseTurnStreamJSON(filepath.Join(dir, tf))
		if err != nil {
			return analyzeArm{}, fmt.Errorf("%s: %w", tf, err)
		}
		m.Turn = i + 1
		arm.Turns = append(arm.Turns, m)
		arm.Totals.CacheCreationInputTokens += m.CacheCreationInputTokens
		arm.Totals.CacheReadInputTokens += m.CacheReadInputTokens
		arm.Totals.OutputTokens += m.OutputTokens
		arm.Totals.CostUSD += m.CostUSD
		arm.Totals.ToolCallCount += m.ToolCallCount
		arm.Totals.ToolOutputBytes += m.ToolOutputBytes
		arm.Totals.DurationMS += m.DurationMS
		for k, v := range m.ToolCallsByName {
			arm.Totals.ToolCallsByName[k] += v
		}
	}
	return arm, nil
}

// renderMarkdownReport writes an at-a-glance summary followed by a
// per-arm per-turn table and tool-call breakdown. Format designed to
// paste into gap-decomp memos.
func renderMarkdownReport(w io.Writer, arms []analyzeArm) {
	fmt.Fprintln(w, "# analyze-session")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| arm | cost | cache_creation | cache_read | output | tool_calls | tool_out_bytes | bytes/call |")
	fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|---:|---:|")
	for _, a := range arms {
		bpc := 0
		if a.Totals.ToolCallCount > 0 {
			bpc = a.Totals.ToolOutputBytes / a.Totals.ToolCallCount
		}
		fmt.Fprintf(w, "| %s | $%.4f | %s | %s | %s | %d | %s | %d |\n",
			a.Name, a.Totals.CostUSD,
			commaGroup(a.Totals.CacheCreationInputTokens),
			commaGroup(a.Totals.CacheReadInputTokens),
			commaGroup(a.Totals.OutputTokens),
			a.Totals.ToolCallCount,
			commaGroup(a.Totals.ToolOutputBytes),
			bpc)
	}
	for _, a := range arms {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "## %s (per-turn)\n\n", a.Name)
		fmt.Fprintln(w, "| turn | cost | cc | cr | out | calls | tool_bytes |")
		fmt.Fprintln(w, "|---:|---:|---:|---:|---:|---:|---:|")
		for _, t := range a.Turns {
			fmt.Fprintf(w, "| %d | $%.4f | %s | %s | %s | %d | %s |\n",
				t.Turn, t.CostUSD,
				commaGroup(t.CacheCreationInputTokens),
				commaGroup(t.CacheReadInputTokens),
				commaGroup(t.OutputTokens),
				t.ToolCallCount,
				commaGroup(t.ToolOutputBytes))
		}
		if len(a.Totals.ToolCallsByName) > 0 {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "tool call breakdown:")
			type kv struct {
				K string
				V int
			}
			var pairs []kv
			for k, v := range a.Totals.ToolCallsByName {
				pairs = append(pairs, kv{k, v})
			}
			sort.Slice(pairs, func(i, j int) bool { return pairs[i].V > pairs[j].V })
			for _, p := range pairs {
				fmt.Fprintf(w, "- %s × %d\n", p.K, p.V)
			}
		}
	}
}

// cmdAnalyzeSession is the entry point for `defn analyze-session`.
// Parses stream-json output from a session-cumulative bench run and
// prints a per-arm, per-turn cost/cache breakdown so gap-decomp is a
// CLI call, not an ad-hoc python script every time. See #178.
func cmdAnalyzeSession(args []string) {
	jsonOut := false
	dir := ""
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case strings.HasPrefix(a, "--"):
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			os.Exit(1)
		default:
			if dir != "" {
				fmt.Fprintln(os.Stderr, "usage: defn analyze-session [--json] <dir>")
				os.Exit(1)
			}
			dir = a
		}
	}
	if dir == "" {
		fmt.Fprintln(os.Stderr, "usage: defn analyze-session [--json] <dir>")
		os.Exit(1)
	}
	arms, err := discoverArms(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(arms) == 0 {
		fmt.Fprintf(os.Stderr, "no turn-*.json files found under %s\n", dir)
		os.Exit(1)
	}
	if jsonOut {
		b, _ := json.MarshalIndent(arms, "", "  ")
		fmt.Println(string(b))
		return
	}
	renderMarkdownReport(os.Stdout, arms)
}

// commaGroup formats an int with thousands separators (12345 -> "12,345").
// Local helper — the stdlib doesn't ship one and pulling golang.org/x/text
// for a single number formatter is overkill.
func commaGroup(n int) string {
	if n < 0 {
		return "-" + commaGroup(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

// discoverArms locates arm directories under dir. If dir itself
// contains turn-*.json files, it's treated as a single arm named after
// its basename. Otherwise every subdir that contains turn-*.json is
// an arm. Sorted by name for stable output.
func discoverArms(dir string) ([]analyzeArm, error) {
	if hasTurnFiles(dir) {
		arm, err := loadArm(filepath.Base(dir), dir)
		if err != nil {
			return nil, err
		}
		return []analyzeArm{arm}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var arms []analyzeArm
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if !hasTurnFiles(sub) {
			continue
		}
		arm, err := loadArm(e.Name(), sub)
		if err != nil {
			return nil, err
		}
		arms = append(arms, arm)
	}
	sort.Slice(arms, func(i, j int) bool { return arms[i].Name < arms[j].Name })
	return arms, nil
}
