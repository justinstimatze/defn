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
	"time"
)

// turnMetrics captures the cost and tool-use signal for a single
// claude -p turn as parsed from a stream-json file. Populated by
// parseTurnStreamJSON.
//
// #183 batch-efficiency additions: DefnOpsByName histograms the `op`
// values inside mcp__defn__code calls (read/outline/expand/apply/...)
// so we can see whether the model actually uses expand/apply or just
// chains sequential reads. ApplyBatchSizes tracks how many ops each
// apply call bundled — a large mean means the model is genuinely
// batching, a mean near 1 means apply is misused. SequentialReadChainMax
// is the longest run of consecutive read-family calls with no batch or
// non-defn tool interspersed — a high value flags adoption failure
// even when read counts look reasonable.
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

	// #183 batch-efficiency signal.
	DefnOpsByName          map[string]int `json:"defn_ops_by_name,omitempty"`
	ApplyBatchSizes        []int          `json:"apply_batch_sizes,omitempty"`
	SequentialReadChainMax int            `json:"sequential_read_chain_max,omitempty"`
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
//
// #183: also inspects mcp__defn__code call inputs to build a defn-op
// histogram, capture per-apply batch sizes, and compute the longest
// run of consecutive read-family calls (adoption-failure signal).
func parseTurnStreamJSON(path string) (turnMetrics, error) {
	f, err := os.Open(path)
	if err != nil {
		return turnMetrics{}, err
	}
	defer f.Close()

	m := turnMetrics{ToolCallsByName: map[string]int{}, DefnOpsByName: map[string]int{}}
	// #183 sequential-read chain: track streak length across the ordered
	// tool_use blocks of THIS turn. Reset on any non-read-family call.
	// Read family = read/read-file/outline/expand/impact/search/overview.
	readFamily := map[string]bool{
		"read": true, "read-file": true, "outline": true, "expand": true,
		"impact": true, "search": true, "overview": true,
	}
	curStreak := 0
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
				if cb["type"] != "tool_use" {
					continue
				}
				name, _ := cb["name"].(string)
				m.ToolCallCount++
				m.ToolCallsByName[name]++
				var defnOp string
				if name == "mcp__defn__code" {
					inp, _ := cb["input"].(map[string]any)
					defnOp, _ = inp["op"].(string)
					if defnOp != "" {
						m.DefnOpsByName[defnOp]++
					}
					if defnOp == "apply" {
						ops, _ := inp["operations"].([]any)
						m.ApplyBatchSizes = append(m.ApplyBatchSizes, len(ops))
					}
				}
				if name == "mcp__defn__code" && readFamily[defnOp] {
					curStreak++
					if curStreak > m.SequentialReadChainMax {
						m.SequentialReadChainMax = curStreak
					}
				} else {
					curStreak = 0
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
	arm := analyzeArm{Name: name, Path: dir, Totals: turnMetrics{
		ToolCallsByName: map[string]int{},
		DefnOpsByName:   map[string]int{},
	}}
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
		for k, v := range m.DefnOpsByName {
			arm.Totals.DefnOpsByName[k] += v
		}
		arm.Totals.ApplyBatchSizes = append(arm.Totals.ApplyBatchSizes, m.ApplyBatchSizes...)
		if m.SequentialReadChainMax > arm.Totals.SequentialReadChainMax {
			arm.Totals.SequentialReadChainMax = m.SequentialReadChainMax
		}
	}
	return arm, nil
}

// renderMarkdownReport writes an at-a-glance summary followed by a
// per-arm per-turn table, tool-call breakdown, and (#183) batch-
// efficiency KPIs so adoption failures ("35 defn calls, 0 apply
// calls, sequential-read chain of 12") jump out visually.
// Format designed to paste into gap-decomp memos.
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
		renderBatchEfficiency(w, a)
	}
}

// cmdAnalyzeSession is the entry point for `defn analyze-session`.
// Parses stream-json output from a session-cumulative bench run and
// prints a per-arm, per-turn cost/cache breakdown so gap-decomp is a
// CLI call, not an ad-hoc python script every time. See #178.
//
// #199: --warming-usd and --amortize-over add a three-bucket cost
// receipt (session / warming / amortized-over-N) so async precompute
// (#160 summaries, #197 semantic bridge, etc.) is honestly counted.
// Warming is default-on per #201; hiding its cost would misread the
// numbers.
//
// #179: --watch polls dir every 2s and prints a running per-arm total
// as new turn-NN.json files appear, instead of requiring the bench to
// finish before any signal is visible. Useful for a live bench run in
// another terminal/background job -- see one number tick up instead
// of waiting the full 10-20 minutes for completion before finding out
// something's off.
func cmdAnalyzeSession(args []string) {
	jsonOut := false
	watch := false
	dir := ""
	warmingUSD := 0.0
	amortizeOver := 10
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--watch":
			watch = true
		case a == "--warming-usd":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--warming-usd requires a value")
				os.Exit(1)
			}
			var v float64
			if _, err := fmt.Sscanf(args[i+1], "%f", &v); err != nil {
				fmt.Fprintf(os.Stderr, "--warming-usd: bad value %q: %v\n", args[i+1], err)
				os.Exit(1)
			}
			warmingUSD = v
			i++
		case a == "--amortize-over":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--amortize-over requires a value")
				os.Exit(1)
			}
			var v int
			if _, err := fmt.Sscanf(args[i+1], "%d", &v); err != nil || v <= 0 {
				fmt.Fprintf(os.Stderr, "--amortize-over: bad value %q\n", args[i+1])
				os.Exit(1)
			}
			amortizeOver = v
			i++
		case strings.HasPrefix(a, "--"):
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			os.Exit(1)
		default:
			if dir != "" {
				fmt.Fprintln(os.Stderr, "usage: defn analyze-session [--json] [--watch] [--warming-usd F] [--amortize-over N] <dir>")
				os.Exit(1)
			}
			dir = a
		}
		i++
	}
	if dir == "" {
		fmt.Fprintln(os.Stderr, "usage: defn analyze-session [--json] [--watch] [--warming-usd F] [--amortize-over N] <dir>")
		os.Exit(1)
	}
	if watch {
		watchSession(dir)
		return
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
	if warmingUSD > 0 {
		renderCostAccounting(os.Stdout, arms, warmingUSD, amortizeOver)
	}
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

// renderBatchEfficiency emits the #183 batch-efficiency KPIs for one
// arm: defn op histogram, apply batch stats, longest sequential-read
// chain. Suppressed for arms that never called mcp__defn__code so the
// files baseline stays clean.
func renderBatchEfficiency(w io.Writer, a analyzeArm) {
	if len(a.Totals.DefnOpsByName) == 0 && a.Totals.SequentialReadChainMax == 0 && len(a.Totals.ApplyBatchSizes) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "batch efficiency (#183):")
	if len(a.Totals.DefnOpsByName) > 0 {
		type kv struct {
			K string
			V int
		}
		var pairs []kv
		for k, v := range a.Totals.DefnOpsByName {
			pairs = append(pairs, kv{k, v})
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].V > pairs[j].V })
		parts := make([]string, 0, len(pairs))
		for _, p := range pairs {
			parts = append(parts, fmt.Sprintf("%s×%d", p.K, p.V))
		}
		fmt.Fprintf(w, "- defn ops: %s\n", strings.Join(parts, ", "))
	}
	fmt.Fprintf(w, "- apply calls: %d", len(a.Totals.ApplyBatchSizes))
	if len(a.Totals.ApplyBatchSizes) > 0 {
		sum := 0
		maxBatch := 0
		for _, n := range a.Totals.ApplyBatchSizes {
			sum += n
			if n > maxBatch {
				maxBatch = n
			}
		}
		mean := float64(sum) / float64(len(a.Totals.ApplyBatchSizes))
		fmt.Fprintf(w, " (mean batch=%.1f, max=%d, total ops batched=%d)", mean, maxBatch, sum)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "- longest sequential-read chain: %d", a.Totals.SequentialReadChainMax)
	if a.Totals.SequentialReadChainMax >= 5 {
		fmt.Fprintf(w, " ⚠ adoption failure — expand/apply would collapse this")
	}
	fmt.Fprintln(w)
}

// renderCostAccounting emits the #199 three-bucket cost receipt when
// warming was applied to this project. Session cost comes from the
// stream-json result events; warming cost is passed by the caller
// (per #199a we don't yet auto-attribute it from the usage log).
// Amortized = session + warming/N, where N is the assumed number of
// sessions the warming will benefit. Print BOTH the raw and the
// amortized so nobody's fooled either direction.
//
// Only prints for arms whose name looks like a defn arm (contains
// "defn"). Files arms don't have warming cost by definition.
func renderCostAccounting(w io.Writer, arms []analyzeArm, warmingUSD float64, amortizeOver int) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Cost accounting (#199)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "_Warming cost: $%.4f — one-time async precompute (Sonnet summaries etc.); amortized over %d sessions._\n\n", warmingUSD, amortizeOver)
	fmt.Fprintln(w, "| arm | session cost | + warming (raw) | + warming (amortized) |")
	fmt.Fprintln(w, "|---|---:|---:|---:|")
	for _, a := range arms {
		isDefn := strings.Contains(strings.ToLower(a.Name), "defn")
		w1 := 0.0
		w2 := 0.0
		if isDefn {
			w1 = warmingUSD
			w2 = warmingUSD / float64(amortizeOver)
		}
		fmt.Fprintf(w, "| %s | $%.4f | $%.4f | $%.4f |\n",
			a.Name, a.Totals.CostUSD, a.Totals.CostUSD+w1, a.Totals.CostUSD+w2)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "_Session cost is per-run raw. Warming (raw) shows the honest first-run cost. Amortized shows the steady-state cost for a repeat-use project. Compare vs files-mode session cost to judge whether defn is cheaper for your use pattern._")
}

// watchSession polls dir every 2s for new/changed turn-*.json files
// and prints one line per arm whenever its running total changes --
// #179's live-visibility mode. Runs until interrupted (Ctrl-C); a
// missing/not-yet-created dir is tolerated (discoverArms erroring)
// since a bench harness may not have written its first turn file yet
// when --watch is started.
func watchSession(dir string) {
	fmt.Printf("[watch] polling %s every 2s -- Ctrl-C to stop\n", dir)
	last := map[string]turnMetrics{}
	for {
		arms, err := discoverArms(dir)
		if err == nil {
			for _, arm := range arms {
				prev, ok := last[arm.Name]
				if !ok || prev.CostUSD != arm.Totals.CostUSD || prev.ToolCallCount != arm.Totals.ToolCallCount {
					fmt.Printf("[%s] %-16s turn %d, %d tool calls, $%.4f\n",
						time.Now().Format("15:04:05"), arm.Name, len(arm.Turns), arm.Totals.ToolCallCount, arm.Totals.CostUSD)
					last[arm.Name] = arm.Totals
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
}
