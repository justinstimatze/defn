package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A chain is a multi-op / multi-file mutation case. Unlike `mutation`,
// which writes one fixture and expects one file to satisfy a post-
// condition, a chain writes N fixtures and expects N post-conditions
// (potentially different) across them.
//
// This is the workload defn was designed for: cross-file renames touch
// the def and every caller; a multi-op edit on one def batches through
// `apply` in one turn. Files-mode has to Read every touched file and
// carefully sequence edits; defn touches the DB once.
type chain struct {
	name     string
	fixtures []chainFixture
	prompt   string
	// mustContain: map of relative file path → substrings the file must
	// contain after the edit lands. Whitespace-canonicalized.
	mustContain map[string][]string
	// mustNotContain: same shape, negated.
	mustNotContain map[string][]string
}

type chainFixture struct {
	path    string // relative path in scratch dir
	content string
}

var chains = []chain{
	{
		name: "triple-op-one-def",
		fixtures: []chainFixture{
			{
				path: "process.go",
				content: `package fixture

import (
	"bytes"
	"fmt"
	"log"
)

// Process consumes data and returns the count.
func Process(data []byte, verbose bool) int {
	log.Printf("start: %d bytes", len(data))
	if verbose {
		fmt.Printf("verbose: %d\n", len(data))
	}
	trimmed := bytes.TrimSpace(data)
	return len(trimmed)
}
`,
			},
		},
		prompt: `In process.go, make three related edits to the Process function:
  1. Rename the parameter "data" to "payload" throughout the signature and body.
  2. Wrap the first statement in a deferred log.Println("done") call — so log.Println is deferred, THEN the current first statement runs.
  3. Insert a precondition at the very top: if len(payload) == 0 { return 0 }.

Do not touch anything outside Process. Do not add extra prints or comments.`,
		mustContain: map[string][]string{
			"process.go": {
				`payload []byte`,
				`len(payload)`,
				`bytes.TrimSpace(payload)`,
				`defer log.Println("done")`,
				`if len(payload) == 0`,
				`return 0`,
			},
		},
		mustNotContain: map[string][]string{
			"process.go": {
				`data []byte`,
				`bytes.TrimSpace(data)`,
			},
		},
	},
	{
		name: "rename-across-callers",
		fixtures: []chainFixture{
			{
				path: "core.go",
				content: `package fixture

// ProcessData is the core operation.
func ProcessData(x int) int {
	return x * 2
}
`,
			},
			{
				path: "caller_a.go",
				content: `package fixture

func RunA(x int) int {
	return ProcessData(x) + 1
}
`,
			},
			{
				path: "caller_b.go",
				content: `package fixture

func RunB(x int) int {
	total := 0
	for i := 0; i < x; i++ {
		total += ProcessData(i)
	}
	return total
}
`,
			},
		},
		prompt: `Rename the function ProcessData to Handle everywhere in the fixture. It's called from RunA and RunB, so all call sites need to update too. Do not change any other behavior.`,
		mustContain: map[string][]string{
			"core.go":     {`func Handle(x int)`},
			"caller_a.go": {`Handle(x)`},
			"caller_b.go": {`Handle(i)`},
		},
		mustNotContain: map[string][]string{
			"core.go":     {`func ProcessData(`},
			"caller_a.go": {`ProcessData(x)`},
			"caller_b.go": {`ProcessData(i)`},
		},
	},
}

// runChainBench runs every chain case in both files-mode and defn-mode
// against a fresh scratch repo, git-resetting between runs. Mirrors
// runMutationBench but writes multiple fixtures per case.
func runChainBench(defnBin string) {
	scratch, err := os.MkdirTemp("", "defn-bench-chain-*")
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

	fmt.Printf("\n=== Running %d chain cases in both modes ===\n\n", len(chains))

	var filesResults, defnResults []mutationResult
	for i, c := range chains {
		fmt.Printf("[%d/%d] %s (%d fixture files)\n", i+1, len(chains), c.name, len(c.fixtures))

		rFiles := runChainCase(scratch, defnBin, c, "files")
		filesResults = append(filesResults, rFiles)
		fmt.Printf("  files:  %d calls, %s, in/out/cache=%d/%d/%d tok, correct=%v\n",
			rFiles.toolCalls, rFiles.duration.Round(time.Second),
			rFiles.inputTokens, rFiles.outputTokens, rFiles.cachedTokens, rFiles.correct)

		rDefn := runChainCase(scratch, defnBin, c, "defn")
		defnResults = append(defnResults, rDefn)
		fmt.Printf("  defn:   %d calls, %s, in/out/cache=%d/%d/%d tok, correct=%v\n",
			rDefn.toolCalls, rDefn.duration.Round(time.Second),
			rDefn.inputTokens, rDefn.outputTokens, rDefn.cachedTokens, rDefn.correct)
		fmt.Println()
	}

	fmt.Println("=== Chain summary ===")
	fmt.Printf("%-24s %6s %6s %8s %8s %8s %8s %6s %6s\n",
		"Case", "F.cls", "D.cls", "F.inTok", "D.inTok", "F.outTok", "D.outTok", "F.ok", "D.ok")
	fmt.Println(strings.Repeat("-", 96))
	var fCalls, dCalls int
	var fIn, dIn, fOut, dOut int
	var fCorrect, dCorrect int
	for i := range chains {
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
			chains[i].name, f.toolCalls, d.toolCalls,
			f.inputTokens, d.inputTokens, f.outputTokens, d.outputTokens,
			f.correct, d.correct)
	}
	fmt.Println(strings.Repeat("-", 96))
	fmt.Printf("%-24s %6d %6d %8d %8d %8d %8d %6s %6s\n",
		"TOTAL", fCalls, dCalls, fIn, dIn, fOut, dOut,
		fmt.Sprintf("%d/%d", fCorrect, len(chains)),
		fmt.Sprintf("%d/%d", dCorrect, len(chains)))
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

func runChainCase(scratch, defnBin string, c chain, mode string) mutationResult {
	resetCmd := exec.Command("git", "reset", "--hard", "-q")
	resetCmd.Dir = scratch
	if err := resetCmd.Run(); err != nil {
		return mutationResult{name: c.name, mode: mode, rawOutput: "git reset failed"}
	}
	cleanCmd := exec.Command("git", "clean", "-fdq")
	cleanCmd.Dir = scratch
	_ = cleanCmd.Run()

	for _, f := range c.fixtures {
		p := filepath.Join(scratch, f.path)
		if err := os.WriteFile(p, []byte(f.content), 0644); err != nil {
			return mutationResult{name: c.name, mode: mode, rawOutput: fmt.Sprintf("write fixture %s: %v", f.path, err)}
		}
	}

	syncCmd := exec.Command(defnBin, "ingest", ".")
	syncCmd.Dir = scratch
	_ = syncCmd.Run()

	prompt := c.prompt
	if mode == "defn" {
		prompt = mutationDefnPreamble + "\n\n---\n\n" + c.prompt
	} else {
		prompt = mutationFilesPreamble + "\n\n---\n\n" + c.prompt
	}

	start := time.Now()
	cmd := exec.Command("claude", "-p", "--mcp-config", ".mcp.json", "--verbose", "--output-format", "stream-json")
	cmd.Dir = scratch
	cmd.Env = append(os.Environ(), "CLAUDE_ALLOW_GO_EDIT=1")
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)

	res := mutationResult{name: c.name, mode: mode, duration: dur, rawOutput: string(out)}
	if err != nil {
		return res
	}
	stats := parseStreamJSON(out)
	res.toolCalls = stats.ToolCalls
	res.inputTokens = stats.InputTokens
	res.outputTokens = stats.OutputTokens
	res.cachedTokens = stats.CachedTokens
	res.correct = checkChainPostCondition(scratch, c)

	// On failure, preserve the raw stream-json AND the fixture files so the
	// next debug run can diff intent vs reality. Cheap insurance; the file
	// stays around until the user cleans /tmp.
	if !res.correct {
		if dumpDir, dumpErr := os.MkdirTemp("", "defn-bench-fail-"+c.name+"-"+mode+"-*"); dumpErr == nil {
			_ = os.WriteFile(filepath.Join(dumpDir, "stream.jsonl"), out, 0644)
			for _, f := range c.fixtures {
				finalBytes, ferr := os.ReadFile(filepath.Join(scratch, f.path))
				if ferr == nil {
					_ = os.WriteFile(filepath.Join(dumpDir, "final-"+strings.ReplaceAll(f.path, "/", "_")), finalBytes, 0644)
				}
			}
			fmt.Fprintf(os.Stderr, "    (failure dump: %s)\n", dumpDir)
		}
	}
	return res
}

func checkChainPostCondition(scratch string, c chain) bool {
	canon := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for path, wants := range c.mustContain {
		final, err := os.ReadFile(filepath.Join(scratch, path))
		if err != nil {
			return false
		}
		fc := canon(string(final))
		for _, w := range wants {
			if !strings.Contains(fc, canon(w)) {
				return false
			}
		}
	}
	for path, forbids := range c.mustNotContain {
		final, err := os.ReadFile(filepath.Join(scratch, path))
		if err != nil {
			return false
		}
		fc := canon(string(final))
		for _, f := range forbids {
			if strings.Contains(fc, canon(f)) {
				return false
			}
		}
	}
	return true
}
