// Command defn-bench measures tool calls and tokens for standardized code
// understanding questions, with and without defn MCP tools.
//
// It runs claude -p in both modes and compares the results.
//
// Usage: go run ./cmd/defn-bench
package main

import (
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
	project string // project short name
	repoDir string // path to cloned repo
	query   string // the question to ask
	// Expected: what a correct answer should contain
	expectContains []string
}

type result struct {
	question  string
	mode      string // "files" or "defn"
	toolCalls int
	duration  time.Duration
	answerLen int
	correct   bool // did the answer contain expected strings?
	rawOutput string
}

var questions = []question{
	// Chi
	{project: "chi", query: "What does the NewMux function do? Be concise.",
		expectContains: []string{"Mux", "pool", "sync"}},
	{project: "chi", query: "Who calls NewMux?",
		expectContains: []string{"NewRouter"}},
	{project: "chi", query: "What's the blast radius of changing InsertRoute? How many functions call it directly or transitively?",
		expectContains: []string{"handle"}},

	// Gin
	{project: "gin", query: "What does the Render method on Context do? Be concise.",
		expectContains: []string{"response", "write", "status"}},
	{project: "gin", query: "List all the methods on Context that call Render.",
		expectContains: []string{"JSON", "XML", "HTML"}},
	{project: "gin", query: "What's the blast radius of changing the Render method on Context?",
		expectContains: []string{"caller", "16", "15"}},

	// Mux
	{project: "mux", query: "What does NewRouter do?",
		expectContains: []string{"Router"}},
	{project: "mux", query: "Who calls HandleFunc on Router?",
		expectContains: []string{"Test"}},

	// Toml
	{project: "toml", query: "What does the Decode function do? Be concise.",
		expectContains: []string{"TOML", "decode", "unmarshal"}},
}

func main() {
	mutOnly := false
	includeMut := false
	chainsOnly := false
	sizeSweep := false
	sizeSweepSamples := 2
	sizeSweepCSV := ""
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
			fmt.Println("  --size-sweep                        sweep add-import mutation across fixture sizes; writes CSV")
			fmt.Println("  --samples N                         samples per (size, mode) in --size-sweep (default 2)")
			fmt.Println("  --size-sweep-csv <path>             output CSV path (default ./size-sweep.csv)")
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
		runSizeSweepBench(defnBin, sizeSweepSamples, sizeSweepCSV)
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

	fmt.Printf("\n=== Running %d questions in both modes ===\n\n", len(questions))

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
		fmt.Printf("  files:  %d tool calls, %s, correct=%v\n", r1.toolCalls, r1.duration.Round(time.Second), r1.correct)

		if len(mcpBackup) > 0 {
			os.WriteFile(mcpPath, mcpBackup, 0644)
		}
		if len(claudeMDBackup) > 0 {
			os.WriteFile(claudeMDPath, claudeMDBackup, 0644)
		}

		r2 := runClaude(dir, q, "defn")
		defnResults = append(defnResults, r2)
		fmt.Printf("  defn:   %d tool calls, %s, correct=%v\n", r2.toolCalls, r2.duration.Round(time.Second), r2.correct)
		fmt.Println()
	}

	fmt.Println("=== Summary ===")
	fmt.Printf("%-8s %-60s %6s %6s %6s %6s\n", "Project", "Question", "F.calls", "D.calls", "F.time", "D.time")
	fmt.Println(strings.Repeat("-", 110))

	totalFilesCalls := 0
	totalDefnCalls := 0
	totalFilesTime := time.Duration(0)
	totalDefnTime := time.Duration(0)

	for i := range questions {
		f := filesResults[i]
		d := defnResults[i]
		totalFilesCalls += f.toolCalls
		totalDefnCalls += d.toolCalls
		totalFilesTime += f.duration
		totalDefnTime += d.duration
		fmt.Printf("%-8s %-60s %6d %6d %6s %6s\n",
			questions[i].project,
			truncate(questions[i].query, 60),
			f.toolCalls, d.toolCalls,
			f.duration.Round(time.Second), d.duration.Round(time.Second))
	}

	fmt.Println(strings.Repeat("-", 110))
	fmt.Printf("%-69s %6d %6d %6s %6s\n", "TOTAL",
		totalFilesCalls, totalDefnCalls,
		totalFilesTime.Round(time.Second), totalDefnTime.Round(time.Second))

	if totalFilesCalls > 0 {
		reduction := float64(totalFilesCalls-totalDefnCalls) / float64(totalFilesCalls) * 100
		fmt.Printf("\nTool call reduction: %.0f%%\n", reduction)
	}
	if totalFilesTime > 0 {
		speedup := float64(totalFilesTime) / float64(totalDefnTime)
		fmt.Printf("Speed improvement: %.1fx\n", speedup)
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

	// Parse stream-json to count tool calls and extract answer.
	toolCalls := 0
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

		msgType, _ := msg["type"].(string)

		// Count tool uses from assistant messages.
		// Format: {"type":"assistant","message":{"content":[{"type":"tool_use",...}]}}
		if msgType == "assistant" {
			if message, ok := msg["message"].(map[string]any); ok {
				if content, ok := message["content"].([]any); ok {
					for _, c := range content {
						if cm, ok := c.(map[string]any); ok {
							if cm["type"] == "tool_use" {
								toolCalls++
							}
						}
					}
				}
			}
		}

		// Extract final result text.
		if msgType == "result" {
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
		question:  q.query,
		mode:      mode,
		toolCalls: toolCalls,
		duration:  dur,
		answerLen: len(answer),
		correct:   correct,
		rawOutput: string(out),
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
