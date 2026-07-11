// Command defn-bench measures tool calls and tokens for standardized code
// understanding questions, with and without defn MCP tools.
//
// It runs claude -p in both modes and compares the results.
//
// Usage: go run ./cmd/defn-bench
package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type question struct {
	project string       // project short name
	kind    questionKind // graph vs lookup — for per-bucket splits in the summary
	repoDir string       // path to cloned repo
	query   string       // the question to ask
	// Expected: what a correct answer should contain
	expectContains []string
}

type result struct {
	question     string
	mode         string // "files" or "defn"
	toolCalls    int
	inputTokens  int
	outputTokens int
	cachedTokens int
	duration     time.Duration
	answerLen    int
	correct      bool // did the answer contain expected strings?
	rawOutput    string
}

// Question corpus for the read-side bench. Every entry carries a
// `kind` so we can split totals into "graph" (callers, blast radius,
// transitive impact — where defn's ref graph should win over Read+
// grep) vs "lookup" (single-file "what does X do" — where files-
// mode should be at least at parity). n=25, roughly balanced.
type questionKind string

const (
	kindGraph  questionKind = "graph"  // ref-graph traversal
	kindLookup questionKind = "lookup" // single-definition read
)

var questions = []question{
	// ── chi ────────────────────────────────────────────────────
	{project: "chi", kind: kindLookup, query: "What does the NewMux function do? Be concise.",
		expectContains: []string{"Mux"}},
	{project: "chi", kind: kindLookup, query: "What does the Mux.ServeHTTP method do? Be concise.",
		expectContains: []string{"request"}},
	{project: "chi", kind: kindLookup, query: "What does the Router.Route method do? Be concise.",
		expectContains: []string{"route"}},
	{project: "chi", kind: kindGraph, query: "Who calls NewMux?",
		expectContains: []string{"NewRouter"}},
	{project: "chi", kind: kindGraph, query: "What's the blast radius of changing InsertRoute? How many functions call it directly or transitively?",
		expectContains: []string{"handle"}},
	{project: "chi", kind: kindGraph, query: "List every function that calls Mux.Handle directly.",
		expectContains: []string{"Handle"}},
	{project: "chi", kind: kindGraph, query: "Who calls the routeContext method?",
		expectContains: []string{"chi"}},

	// ── gin ────────────────────────────────────────────────────
	{project: "gin", kind: kindLookup, query: "What does the Render method on Context do? Be concise.",
		expectContains: []string{"response"}},
	{project: "gin", kind: kindLookup, query: "What does the Engine.Run method do? Be concise.",
		expectContains: []string{"http"}},
	{project: "gin", kind: kindLookup, query: "What does the Context.JSON method do? Be concise.",
		expectContains: []string{"json"}},
	{project: "gin", kind: kindGraph, query: "List all the methods on Context that call Render.",
		expectContains: []string{"JSON"}},
	{project: "gin", kind: kindGraph, query: "What's the blast radius of changing the Render method on Context?",
		expectContains: []string{"caller"}},
	{project: "gin", kind: kindGraph, query: "Who calls Engine.handleHTTPRequest?",
		expectContains: []string{"ServeHTTP"}},
	{project: "gin", kind: kindGraph, query: "List every function that calls Context.Next directly.",
		expectContains: []string{"Next"}},

	// ── mux ────────────────────────────────────────────────────
	{project: "mux", kind: kindLookup, query: "What does NewRouter do? Be concise.",
		expectContains: []string{"Router"}},
	{project: "mux", kind: kindLookup, query: "What does the Router.HandleFunc method do? Be concise.",
		expectContains: []string{"handler"}},
	{project: "mux", kind: kindLookup, query: "What does the Route.PathPrefix method do? Be concise.",
		expectContains: []string{"prefix"}},
	{project: "mux", kind: kindGraph, query: "Who calls HandleFunc on Router?",
		expectContains: []string{"Test"}},
	{project: "mux", kind: kindGraph, query: "Who calls the setMatch function?",
		expectContains: []string{"match"}},
	{project: "mux", kind: kindGraph, query: "List every function that calls Router.NewRoute directly.",
		expectContains: []string{"Route"}},

	// ── toml ───────────────────────────────────────────────────
	{project: "toml", kind: kindLookup, query: "What does the Decode function do? Be concise.",
		expectContains: []string{"TOML"}},
	{project: "toml", kind: kindLookup, query: "What does the Encoder.Encode method do? Be concise.",
		expectContains: []string{"toml"}},
	{project: "toml", kind: kindLookup, query: "What does the DecodeFile function do? Be concise.",
		expectContains: []string{"file"}},
	{project: "toml", kind: kindGraph, query: "Who calls the Decode function?",
		expectContains: []string{"toml"}},
	{project: "toml", kind: kindGraph, query: "List every function that calls unify directly.",
		expectContains: []string{"unify"}},
}

func main() {
	mutOnly := false
	includeMut := false
	chainsOnly := false
	sizeSweep := false
	sizeSweepSamples := 2
	sizeSweepCSV := ""
	sizeSweepSizes := []int(nil)
	sizeSweepMutation := "rename-param"
	readSideCSV := ""
	projectFilter := map[string]bool{}
	yourRepoDir := ""
	yourRepoTask := ""
	argv := os.Args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "--mutations-only":
			mutOnly = true
		case "--mutations":
			includeMut = true
		case "--chains-only":
			chainsOnly = true
		case "--size-sweep":
			sizeSweep = true
		case "--samples":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "--samples requires an integer argument")
				os.Exit(1)
			}
			n, err := strconv.Atoi(argv[i+1])
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "--samples: not a positive integer: %q\n", argv[i+1])
				os.Exit(1)
			}
			sizeSweepSamples = n
			i++
		case "--size-sweep-csv":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "--size-sweep-csv requires a path argument")
				os.Exit(1)
			}
			sizeSweepCSV = argv[i+1]
			i++
		case "--project":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "--project requires a comma-separated list (chi,gin,mux,toml)")
				os.Exit(1)
			}
			for _, name := range strings.Split(argv[i+1], ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					projectFilter[name] = true
				}
			}
			i++
		case "--read-side-csv":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "--read-side-csv requires a path argument")
				os.Exit(1)
			}
			readSideCSV = argv[i+1]
			i++
		case "--sweep-mutation":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "--sweep-mutation requires an argument (rename-param|add-import)")
				os.Exit(1)
			}
			sizeSweepMutation = argv[i+1]
			i++
		case "--sizes":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "--sizes requires a comma-separated list argument (e.g. 10,50,100)")
				os.Exit(1)
			}
			for _, part := range strings.Split(argv[i+1], ",") {
				n, err := strconv.Atoi(strings.TrimSpace(part))
				if err != nil || n < 1 {
					fmt.Fprintf(os.Stderr, "--sizes: bad entry %q\n", part)
					os.Exit(1)
				}
				sizeSweepSizes = append(sizeSweepSizes, n)
			}
			i++
		case "--your-repo":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "--your-repo requires a directory argument")
				os.Exit(1)
			}
			yourRepoDir = argv[i+1]
			i++
		case "--task":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "--task requires a string argument")
				os.Exit(1)
			}
			yourRepoTask = argv[i+1]
			i++
		case "-h", "--help":
			fmt.Println("Usage: defn-bench [--mutations|--mutations-only|--chains-only|--size-sweep|--your-repo <dir> --task <str>]")
			fmt.Println("  (no flags)                          run read-side questions only (existing behavior)")
			fmt.Println("  --mutations                         also run write-side single-op mutation cases")
			fmt.Println("  --mutations-only                    run ONLY the write-side single-op mutation cases")
			fmt.Println("  --chains-only                       run ONLY the multi-op / cross-file chain cases")
			fmt.Println("  --size-sweep                        sweep a fixed mutation across fixture sizes; writes CSV")
			fmt.Println("  --sweep-mutation <name>             mutation family: rename-param (default) | add-import")
			fmt.Println("  --sizes 10,50,100,...               override the default sweep sizes")
			fmt.Println("  --samples N                         samples per (size, mode) in --size-sweep (default 2)")
			fmt.Println("  --size-sweep-csv <path>             output CSV path (default ./size-sweep.csv)")
			fmt.Println("  --project chi[,gin,...]             restrict read-side questions to listed projects (RAM-safe batching)")
			fmt.Println("  --read-side-csv <path>              write per-invocation CSV for the read-side questions run")
			fmt.Println("  --your-repo <dir> --task \"<str>\"    audit defn's read-tax win on YOUR own repo, read-only")
			os.Exit(0)
		}
	}

	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintln(os.Stderr, "claude not found in PATH")
		os.Exit(1)
	}

	// Discover defn binary. Explicit DEFN_BIN wins; else "./defn" if
	// present; else PATH. Whichever we pick, build a fresh copy into
	// a tempdir first — stale binaries were the entire chain-bench
	// rename bug for three sessions. See [[project_rename_bench_bug]].
	//
	// Skip the rebuild with DEFN_BENCH_NO_REBUILD=1 (for CI where the
	// binary was built by an earlier step).
	var defnBin string
	if p := os.Getenv("DEFN_BIN"); p != "" {
		defnBin = p
	} else if abs, err := filepath.Abs("defn"); err == nil {
		if _, statErr := os.Stat(abs); statErr == nil {
			defnBin = abs
		}
	}
	if defnBin == "" {
		defnBin, _ = exec.LookPath("defn")
	}
	if defnBin == "" {
		fmt.Fprintln(os.Stderr, "defn binary not found")
		os.Exit(1)
	}

	if os.Getenv("DEFN_BENCH_NO_REBUILD") != "1" {
		freshDir, err := os.MkdirTemp("", "defn-bench-defn-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "mktemp for defn build: %v\n", err)
			os.Exit(1)
		}
		defer os.RemoveAll(freshDir)
		fresh := filepath.Join(freshDir, "defn")
		build := exec.Command("go", "build", "-o", fresh, "./cmd/defn")
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "go build ./cmd/defn: %v (using pre-built %s)\n", err, defnBin)
		} else {
			defnBin = fresh
			fmt.Fprintf(os.Stderr, "defn-bench: using fresh build at %s\n", defnBin)
		}
	} else {
		info, _ := os.Stat(defnBin)
		fmt.Fprintf(os.Stderr, "defn-bench: using %s (built %s)\n", defnBin, info.ModTime().Format("2006-01-02 15:04"))
	}

	if yourRepoDir != "" || yourRepoTask != "" {
		runYourRepoBench(defnBin, yourRepoDir, yourRepoTask)
		return
	}
	if sizeSweep {
		runSizeSweepBench(defnBin, sizeSweepSamples, sizeSweepCSV, sizeSweepSizes, sizeSweepMutation)
		return
	}
	if chainsOnly {
		runChainBench(defnBin)
		return
	}
	if mutOnly {
		runMutationBench(defnBin)
		return
	}

	benchDir, err := os.MkdirTemp("", "defn-bench-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(benchDir)

	projects := map[string]string{}

	repos := map[string]string{
		"chi":  "github.com/go-chi/chi",
		"gin":  "github.com/gin-gonic/gin",
		"mux":  "github.com/gorilla/mux",
		"toml": "github.com/BurntSushi/toml",
	}
	for name, repo := range repos {
		dir := filepath.Join(benchDir, name)
		projects[name] = dir
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
			fmt.Printf("Cloning %s...\n", repo)
			exec.Command("git", "clone", "--depth", "1", "https://"+repo+".git", dir).Run()
		}
	}

	for name, dir := range projects {
		defnDir := filepath.Join(dir, ".defn")
		if _, err := os.Stat(defnDir); err != nil {
			fmt.Printf("Initializing defn for %s...\n", name)
			cmd := exec.Command(defnBin, "init", dir)
			cmd.Env = append(os.Environ(), "DEFN_DB="+defnDir)
			cmd.Stderr = os.Stderr
			cmd.Run()
		}
	}

	if len(projectFilter) > 0 {
		var filtered []question
		for _, q := range questions {
			if projectFilter[q.project] {
				filtered = append(filtered, q)
			}
		}
		questions = filtered
		if len(questions) == 0 {
			fmt.Fprintln(os.Stderr, "--project filter matched zero questions")
			os.Exit(1)
		}
	}

	// CSV append vs create: batching by --project would clobber prior
	// batches if we always created. Append when the file exists AND
	// a filter is in effect (the append-mode signal for batched runs).
	appendCSV := len(projectFilter) > 0 && readSideCSV != ""
	if appendCSV {
		if _, err := os.Stat(readSideCSV); err != nil {
			appendCSV = false
		}
	}

	fmt.Printf("\n=== Running %d questions in both modes ===\n\n", len(questions))

	var csvWriter *csv.Writer
	var csvFile *os.File
	if readSideCSV != "" {
		var f *os.File
		var err error
		if appendCSV {
			f, err = os.OpenFile(readSideCSV, os.O_APPEND|os.O_WRONLY, 0644)
		} else {
			f, err = os.Create(readSideCSV)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "open --read-side-csv %s: %v\n", readSideCSV, err)
			os.Exit(1)
		}
		csvFile = f
		defer csvFile.Close()
		csvWriter = csv.NewWriter(csvFile)
		defer csvWriter.Flush()
		if !appendCSV {
			_ = csvWriter.Write([]string{
				"project", "kind", "question", "mode",
				"tool_calls", "input_tokens", "output_tokens", "cached_tokens",
				"duration_ms", "correct",
			})
		}
	}

	writeRow := func(q question, r result) {
		if csvWriter == nil {
			return
		}
		_ = csvWriter.Write([]string{
			q.project, string(q.kind), q.query, r.mode,
			strconv.Itoa(r.toolCalls),
			strconv.Itoa(r.inputTokens),
			strconv.Itoa(r.outputTokens),
			strconv.Itoa(r.cachedTokens),
			strconv.FormatInt(r.duration.Milliseconds(), 10),
			strconv.FormatBool(r.correct),
		})
		csvWriter.Flush()
	}

	var filesResults, defnResults []result

	for i, q := range questions {
		dir := projects[q.project]

		fmt.Printf("[%d/%d] %s: %s\n", i+1, len(questions), q.project, truncate(q.query, 60))

		mcpPath := filepath.Join(dir, ".mcp.json")
		claudeMDPath := filepath.Join(dir, "CLAUDE.md")
		mcpBackup, _ := os.ReadFile(mcpPath)
		claudeMDBackup, _ := os.ReadFile(claudeMDPath)
		os.Remove(mcpPath)
		os.Remove(claudeMDPath)

		r1 := runClaude(dir, q, "files")
		filesResults = append(filesResults, r1)
		writeRow(q, r1)
		fmt.Printf("  files:  %d calls, %s, in/out/cache=%d/%d/%d tok, correct=%v\n",
			r1.toolCalls, r1.duration.Round(time.Second),
			r1.inputTokens, r1.outputTokens, r1.cachedTokens, r1.correct)

		if len(mcpBackup) > 0 {
			os.WriteFile(mcpPath, mcpBackup, 0644)
		}
		if len(claudeMDBackup) > 0 {
			os.WriteFile(claudeMDPath, claudeMDBackup, 0644)
		}

		r2 := runClaude(dir, q, "defn")
		defnResults = append(defnResults, r2)
		writeRow(q, r2)
		fmt.Printf("  defn:   %d calls, %s, in/out/cache=%d/%d/%d tok, correct=%v\n",
			r2.toolCalls, r2.duration.Round(time.Second),
			r2.inputTokens, r2.outputTokens, r2.cachedTokens, r2.correct)
		fmt.Println()
	}

	fmt.Println("=== Summary ===")
	fmt.Printf("%-6s %-46s %5s %5s %8s %8s %6s %6s\n",
		"Proj", "Question", "F.cls", "D.cls", "F.inTok", "D.inTok", "F.ok", "D.ok")
	fmt.Println(strings.Repeat("-", 100))

	totalFilesCalls := 0
	totalDefnCalls := 0
	totalFilesIn := 0
	totalDefnIn := 0
	totalFilesOut := 0
	totalDefnOut := 0
	totalFilesTime := time.Duration(0)
	totalDefnTime := time.Duration(0)
	filesCorrect := 0
	defnCorrect := 0

	for i := range questions {
		f := filesResults[i]
		d := defnResults[i]
		totalFilesCalls += f.toolCalls
		totalDefnCalls += d.toolCalls
		totalFilesIn += f.inputTokens
		totalDefnIn += d.inputTokens
		totalFilesOut += f.outputTokens
		totalDefnOut += d.outputTokens
		totalFilesTime += f.duration
		totalDefnTime += d.duration
		if f.correct {
			filesCorrect++
		}
		if d.correct {
			defnCorrect++
		}
		fmt.Printf("%-6s %-46s %5d %5d %8d %8d %6v %6v\n",
			questions[i].project,
			truncate(questions[i].query, 46),
			f.toolCalls, d.toolCalls,
			f.inputTokens, d.inputTokens,
			f.correct, d.correct)
	}

	fmt.Println(strings.Repeat("-", 100))
	fmt.Printf("%-53s %5d %5d %8d %8d %6s %6s\n", "TOTAL",
		totalFilesCalls, totalDefnCalls,
		totalFilesIn, totalDefnIn,
		fmt.Sprintf("%d/%d", filesCorrect, len(questions)),
		fmt.Sprintf("%d/%d", defnCorrect, len(questions)))
	fmt.Printf("Wall time: files=%s, defn=%s\n",
		totalFilesTime.Round(time.Second), totalDefnTime.Round(time.Second))

	if totalFilesCalls > 0 {
		reduction := float64(totalFilesCalls-totalDefnCalls) / float64(totalFilesCalls) * 100
		fmt.Printf("Tool call reduction: %.0f%%\n", reduction)
	}
	if totalFilesIn > 0 {
		reduction := float64(totalFilesIn-totalDefnIn) / float64(totalFilesIn) * 100
		fmt.Printf("Input token reduction: %.1f%%\n", reduction)
	}
	if totalFilesOut > 0 {
		reduction := float64(totalFilesOut-totalDefnOut) / float64(totalFilesOut) * 100
		fmt.Printf("Output token reduction: %.1f%%\n", reduction)
	}
	if totalFilesTime > 0 {
		speedup := float64(totalFilesTime) / float64(totalDefnTime)
		fmt.Printf("Speed improvement: %.1fx\n", speedup)
	}

	// Per-kind split — the honest read of "where does defn win"
	// requires bucketing by question type. graph = callers/blast/
	// transitive-impact (ref-graph traversal); lookup = single-def
	// read. The aggregate reduction number lies if the bucket sizes
	// are lopsided.
	fmt.Println()
	fmt.Println("=== By question kind ===")
	fmt.Printf("%-8s %5s %10s %10s %8s %6s %6s\n",
		"kind", "n", "F.inTok", "D.inTok", "Δ.in", "F.ok", "D.ok")
	fmt.Println(strings.Repeat("-", 60))
	for _, kind := range []questionKind{kindGraph, kindLookup} {
		var fIn, dIn, fOk, dOk, n int
		for i, q := range questions {
			if q.kind != kind {
				continue
			}
			n++
			fIn += filesResults[i].inputTokens
			dIn += defnResults[i].inputTokens
			if filesResults[i].correct {
				fOk++
			}
			if defnResults[i].correct {
				dOk++
			}
		}
		if n == 0 {
			continue
		}
		delta := ""
		if fIn > 0 {
			delta = fmt.Sprintf("%+.1f%%", -float64(fIn-dIn)/float64(fIn)*100)
		}
		fmt.Printf("%-8s %5d %10d %10d %8s %6s %6s\n",
			kind, n, fIn, dIn, delta,
			fmt.Sprintf("%d/%d", fOk, n),
			fmt.Sprintf("%d/%d", dOk, n))
	}

	if includeMut {
		fmt.Println()
		runMutationBench(defnBin)
	}
}

func runClaude(dir string, q question, mode string) result {
	start := time.Now()

	cmd := exec.Command("claude", "-p", "--verbose", "--output-format", "stream-json")
	cmd.Dir = dir

	// In defn mode, prepend CLAUDE.md instructions to the prompt so
	// the agent knows to use defn tools (CLAUDE.md isn't loaded in -p mode).
	prompt := q.query
	if mode == "defn" {
		claudeMD, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
		if len(claudeMD) > 0 {
			prompt = string(claudeMD) + "\n\n---\n\n" + q.query
		}
	}
	cmd.Stdin = strings.NewReader(prompt)

	out, err := cmd.CombinedOutput()
	dur := time.Since(start)

	if err != nil {
		return result{question: q.query, mode: mode, duration: dur, rawOutput: string(out)}
	}

	// Tool calls + per-turn usage totals come from parseStreamJSON
	// (see mutations.go). Extracting the final answer text still needs
	// a separate walk since it looks at the result envelope.
	stats := parseStreamJSON(out)
	var answer string
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg["type"] == "result" {
			if text, ok := msg["result"].(string); ok {
				answer = text
			}
		}
	}

	// Check correctness.
	correct := true
	answerLower := strings.ToLower(answer)
	for _, expected := range q.expectContains {
		if !strings.Contains(answerLower, strings.ToLower(expected)) {
			correct = false
			break
		}
	}

	return result{
		question:     q.query,
		mode:         mode,
		toolCalls:    stats.ToolCalls,
		inputTokens:  stats.InputTokens,
		outputTokens: stats.OutputTokens,
		cachedTokens: stats.CachedTokens,
		duration:     dur,
		answerLen:    len(answer),
		correct:      correct,
		rawOutput:    string(out),
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
