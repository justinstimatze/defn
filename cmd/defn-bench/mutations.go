package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A mutation is a write-side benchmark case: give the agent a task that
// requires a specific edit, then verify the edit landed. The counterpart to
// the read-side `question` cases: those measure retrieval + explain cost,
// these measure edit cost.
//
// Every mutation is scoped to one file in the fixture repo (a scratch
// directory checked-in as a git repo so `git reset --hard` restores state
// between the two runs). The task description is written the way a real user
// would phrase it — no leading hints about which tool to call — so both
// modes get an equal briefing.
type mutation struct {
	name string // short id printed in the table

	// The fixture file the mutation edits. Written fresh into the scratch
	// repo at the start of every case so that no prior mutation leaks in.
	fixtureFile     string // relative path in scratch dir
	fixtureContents string

	// Human-worded task description.
	prompt string

	// Post-condition check: mustContain is a list of substrings that MUST
	// appear in the final file. mustNotContain is a list that must NOT.
	// Whitespace-flexible: both sides run through canonicalize before match.
	mustContain    []string
	mustNotContain []string
}

var mutations = []mutation{
	{
		name:        "insert-precondition",
		fixtureFile: "add.go",
		fixtureContents: `package fixture

import "errors"

func Add(a, b int) (int, error) {
	return a + b, nil
}

var ErrOverflow = errors.New("overflow")
`,
		prompt: `In the file add.go, modify the Add function so that if a is negative it returns (0, ErrOverflow) immediately at the top of the function, before doing anything else. Do not touch the rest of the function. Do not add or remove any other definitions.`,
		mustContain: []string{
			`if a < 0`,
			`return 0, ErrOverflow`,
			`return a + b, nil`,
		},
	},
	{
		name:        "wrap-in-defer",
		fixtureFile: "cleanup.go",
		fixtureContents: `package fixture

import "sync"

var mu sync.Mutex

func Compute(x int) int {
	mu.Lock()
	return x * 2
}
`,
		prompt: `In the file cleanup.go, wrap the first statement of Compute in a deferred mu.Unlock() call — so mu.Unlock is deferred, then mu.Lock() runs, then the return. Do not touch anything else.`,
		mustContain: []string{
			`defer mu.Unlock()`,
			`mu.Lock()`,
			`return x * 2`,
		},
	},
	{
		name:        "rename-param",
		fixtureFile: "rename.go",
		fixtureContents: `package fixture

func Process(data []byte, verbose bool) int {
	if verbose {
		_ = data
	}
	return len(data)
}
`,
		prompt: `In the file rename.go, rename the parameter "data" to "payload" in the Process function — both in the signature and in every use inside the body. Do not rename "verbose". Do not change the function's behavior.`,
		mustContain: []string{
			`payload []byte`,
			`len(payload)`,
			`_ = payload`,
		},
		mustNotContain: []string{
			`data []byte`,
			`len(data)`,
		},
	},
	{
		name:        "replace-slice-return",
		fixtureFile: "replace.go",
		fixtureContents: `package fixture

import "fmt"

func Greet(name string) string {
	if name == "" {
		return "hello, world"
	}
	return fmt.Sprintf("hello, %s", name)
}
`,
		prompt: `In the file replace.go, replace the LAST return statement in the Greet function with: return "hi, " + name. Leave the first return ("hello, world") untouched.`,
		mustContain: []string{
			`return "hello, world"`,
			`return "hi, " + name`,
		},
		mustNotContain: []string{
			`fmt.Sprintf`,
		},
	},
	{
		name:        "add-import",
		fixtureFile: "importer.go",
		fixtureContents: `package fixture

import (
	"fmt"
)

func Show(x int) {
	fmt.Println(x)
}
`,
		prompt: `In the file importer.go, add the "errors" standard-library import. Do not modify the Show function. Do not add any other imports.`,
		mustContain: []string{
			`"errors"`,
			`"fmt"`,
			`fmt.Println(x)`,
		},
	},
	{
		name:            "big-add-import",
		fixtureFile:     "big_importer.go",
		fixtureContents: buildBigImporterFile(),
		prompt:          `In the file big_importer.go, add the "hash/fnv" standard-library import. Do not modify any function. Do not add any other imports.`,
		mustContain: []string{
			`"hash/fnv"`,
			`"context"`,
			`"encoding/json"`,
		},
	},
	{
		name:            "big-rename-param",
		fixtureFile:     "big_process.go",
		fixtureContents: buildBigProcessFile(),
		prompt:          `In the file big_process.go, rename the parameter "data" to "payload" in the Process function — throughout the signature and body. Do not rename "verbose" or "raw" or any other identifier. Do not change any other behavior.`,
		mustContain: []string{
			`payload []byte`,
			`len(payload)`,
			`bytes.TrimSpace(payload)`,
			`json.Unmarshal(payload,`,
		},
		mustNotContain: []string{
			`data []byte`,
			`len(data)`,
			`bytes.TrimSpace(data)`,
			`json.Unmarshal(data,`,
		},
	},
	{
		name:            "big-replace-slice",
		fixtureFile:     "big_classify.go",
		fixtureContents: buildBigMultiReturnFile(),
		prompt:          `In the file big_classify.go, replace the FINAL return statement of the Classify function (the "unknown" fallthrough one) with: return "other", nil. Leave every other return statement untouched.`,
		mustContain: []string{
			`return "other", nil`,
			`return "url", nil`,
			`return "email", nil`,
			`return "int", nil`,
		},
		mustNotContain: []string{
			`return "unknown", nil`,
		},
	},
}

type mutationResult struct {
	name         string
	mode         string
	toolCalls    int
	inputTokens  int
	outputTokens int
	cachedTokens int
	duration     time.Duration
	correct      bool
	rawOutput    string
}

// runMutationBench runs every mutation case in both files-mode and defn-mode
// against a fresh scratch repo, git-resetting between runs.
func runMutationBench(defnBin string) {
	scratch, err := os.MkdirTemp("", "defn-bench-mut-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "scratch dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(scratch)

	fmt.Printf("scratch: %s\n", scratch)

	run := func(args ...string) error {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = scratch
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	must := func(args ...string) {
		if err := run(args...); err != nil {
			fmt.Fprintf(os.Stderr, "setup %v: %v\n", args, err)
			os.Exit(1)
		}
	}
	must("git", "init", "-q")
	must("git", "config", "user.email", "bench@example.com")
	must("git", "config", "user.name", "bench")
	if err := os.WriteFile(filepath.Join(scratch, "go.mod"), []byte("module fixture\n\ngo 1.26\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "seed go.mod: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(scratch, "README.md"), []byte("bench fixture\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "seed README: %v\n", err)
		os.Exit(1)
	}
	must("git", "add", "go.mod", "README.md")
	must("git", "commit", "-q", "-m", "seed")

	if err := exec.Command(defnBin, "init", scratch).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "defn init: %v\n", err)
		os.Exit(1)
	}
	must("git", "add", ".mcp.json", "CLAUDE.md")
	must("git", "commit", "-q", "-m", "post-defn-init")

	fmt.Printf("\n=== Running %d mutation cases in both modes ===\n\n", len(mutations))

	var filesResults, defnResults []mutationResult
	for i, m := range mutations {
		fmt.Printf("[%d/%d] %s\n", i+1, len(mutations), m.name)

		rFiles := runMutationCase(scratch, defnBin, m, "files")
		filesResults = append(filesResults, rFiles)
		fmt.Printf("  files:  %d calls, %s, in/out/cache=%d/%d/%d tok, correct=%v\n",
			rFiles.toolCalls, rFiles.duration.Round(time.Second),
			rFiles.inputTokens, rFiles.outputTokens, rFiles.cachedTokens, rFiles.correct)

		rDefn := runMutationCase(scratch, defnBin, m, "defn")
		defnResults = append(defnResults, rDefn)
		fmt.Printf("  defn:   %d calls, %s, in/out/cache=%d/%d/%d tok, correct=%v\n",
			rDefn.toolCalls, rDefn.duration.Round(time.Second),
			rDefn.inputTokens, rDefn.outputTokens, rDefn.cachedTokens, rDefn.correct)
		fmt.Println()
	}

	fmt.Println("=== Mutation summary ===")
	fmt.Printf("%-24s %6s %6s %8s %8s %8s %8s %6s %6s\n",
		"Case", "F.cls", "D.cls", "F.inTok", "D.inTok", "F.outTok", "D.outTok", "F.ok", "D.ok")
	fmt.Println(strings.Repeat("-", 96))
	var fCalls, dCalls int
	var fIn, dIn, fOut, dOut int
	var fCorrect, dCorrect int
	for i := range mutations {
		f, d := filesResults[i], defnResults[i]
		fCalls += f.toolCalls
		dCalls += d.toolCalls
		fIn += f.inputTokens
		dIn += d.inputTokens
		fOut += f.outputTokens
		dOut += d.outputTokens
		if f.correct {
			fCorrect++
		}
		if d.correct {
			dCorrect++
		}
		fmt.Printf("%-24s %6d %6d %8d %8d %8d %8d %6v %6v\n",
			mutations[i].name, f.toolCalls, d.toolCalls,
			f.inputTokens, d.inputTokens, f.outputTokens, d.outputTokens,
			f.correct, d.correct)
	}
	fmt.Println(strings.Repeat("-", 96))
	fmt.Printf("%-24s %6d %6d %8d %8d %8d %8d %6s %6s\n",
		"TOTAL", fCalls, dCalls, fIn, dIn, fOut, dOut,
		fmt.Sprintf("%d/%d", fCorrect, len(mutations)),
		fmt.Sprintf("%d/%d", dCorrect, len(mutations)))
	fmt.Println()
	if fCalls > 0 {
		fmt.Printf("Tool call reduction: %.0f%%\n", float64(fCalls-dCalls)/float64(fCalls)*100)
	}
	if fIn > 0 {
		fmt.Printf("Input token reduction:  %.0f%%\n", float64(fIn-dIn)/float64(fIn)*100)
	}
	if fOut > 0 {
		fmt.Printf("Output token reduction: %.0f%%\n", float64(fOut-dOut)/float64(fOut)*100)
	}
}

func runMutationCase(scratch, defnBin string, m mutation, mode string) mutationResult {
	resetCmd := exec.Command("git", "reset", "--hard", "-q")
	resetCmd.Dir = scratch
	if err := resetCmd.Run(); err != nil {
		return mutationResult{name: m.name, mode: mode, rawOutput: "git reset failed"}
	}
	cleanCmd := exec.Command("git", "clean", "-fdq")
	cleanCmd.Dir = scratch
	_ = cleanCmd.Run()

	fixturePath := filepath.Join(scratch, m.fixtureFile)
	if err := os.WriteFile(fixturePath, []byte(m.fixtureContents), 0644); err != nil {
		return mutationResult{name: m.name, mode: mode, rawOutput: fmt.Sprintf("write fixture: %v", err)}
	}

	syncCmd := exec.Command(defnBin, "ingest", ".")
	syncCmd.Dir = scratch
	_ = syncCmd.Run()

	prompt := m.prompt
	if mode == "defn" {
		prompt = mutationDefnPreamble + "\n\n---\n\n" + m.prompt
	} else {
		prompt = mutationFilesPreamble + "\n\n---\n\n" + m.prompt
	}

	start := time.Now()
	cmd := exec.Command("claude", "-p", "--mcp-config", ".mcp.json", "--verbose", "--output-format", "stream-json")
	cmd.Dir = scratch
	cmd.Env = append(os.Environ(), "CLAUDE_ALLOW_GO_EDIT=1")
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)

	res := mutationResult{name: m.name, mode: mode, duration: dur, rawOutput: string(out)}
	if err != nil {
		return res
	}

	stats := parseStreamJSON(out)
	res.toolCalls = stats.ToolCalls
	res.inputTokens = stats.InputTokens
	res.outputTokens = stats.OutputTokens
	res.cachedTokens = stats.CachedTokens

	finalBytes, ferr := os.ReadFile(fixturePath)
	if ferr != nil {
		return res
	}
	res.correct = checkPostCondition(string(finalBytes), m)
	return res
}

// streamStats captures both the tool-call count and the token accounting
// for a single claude -p invocation's stream-json output.
type streamStats struct {
	ToolCalls    int
	InputTokens  int
	OutputTokens int
	CachedTokens int
}

// parseStreamJSON walks a claude -p stream-json output, counting tool_use
// blocks and summing per-turn token usage. Every assistant turn carries
// its own `usage` object; totals are the sum across all assistant turns.
func parseStreamJSON(out []byte) streamStats {
	var s streamStats
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg["type"] != "assistant" {
			continue
		}
		message, ok := msg["message"].(map[string]any)
		if !ok {
			continue
		}
		if content, ok := message["content"].([]any); ok {
			for _, c := range content {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if cm["type"] == "tool_use" {
					s.ToolCalls++
				}
			}
		}
		if usage, ok := message["usage"].(map[string]any); ok {
			s.InputTokens += intField(usage, "input_tokens")
			s.OutputTokens += intField(usage, "output_tokens")
			s.CachedTokens += intField(usage, "cache_read_input_tokens")
		}
	}
	return s
}

func countToolCalls(out []byte) int { return parseStreamJSON(out).ToolCalls }

func intField(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}

func checkPostCondition(final string, m mutation) bool {
	canon := func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}
	fc := canon(final)
	for _, want := range m.mustContain {
		if !strings.Contains(fc, canon(want)) {
			return false
		}
	}
	for _, forbid := range m.mustNotContain {
		if strings.Contains(fc, canon(forbid)) {
			return false
		}
	}
	return true
}

const mutationFilesPreamble = `You are editing a Go file in the current directory. Use the standard Read and Edit tools. Make the edit exactly as described — do not add any extra changes, do not run gofmt, do not touch other files.`

const mutationDefnPreamble = `You are editing a Go file in the current directory. This project uses the defn MCP tool for Go edits — prefer it over Read/Edit for any Go source change.

FIRST STEP: call ToolSearch with query "select:mcp__defn__code" to load the defn "code" tool schema before doing anything else. Deferred MCP tools are not available until their schema is fetched — skipping this makes the rest of the plan impossible.

Then use one of these ops:

- code(op:"insert-precondition", condition:"x < 0", ret:"return err") — inserts if <cond> { <ret> } at function entry (name inferred if only one non-test function)
- code(op:"replace-slice", slice:"return", index:1, new:"return nil") — replaces the Nth match of a slice kind (return, error-branch, loop, signature, body, doc)
- code(op:"wrap-in-defer", stmt_index:1, defer_body:"cleanup()") — inserts defer <body> before the Nth top-level statement
- code(op:"rename-param", old_param:"x", new_param:"n") — renames a param via ast.Object binding; shadowing respected
- code(op:"add-import", import_path:"errors") — adds an import with goimports-canonical grouping (file inferred if only one non-test .go file)

Multi-edit? BATCH with apply:
- code(op:"apply", operations:[{op:"rename-param", old_param:"x", new_param:"n"}, {op:"wrap-in-defer", defer_body:"cleanup()"}]) — atomic, one emit+build for the whole batch, rolls back on any error

Make the edit exactly as described — do not add any extra changes, do not touch other files.`
