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
	// #71/#75: winze-shape refactor. Retarget a field value across
	// composite literals scattered across files. defn ships this as
	// op:retarget-field-value (one atomic call); files-mode has to
	// grep, then careful-substitute across every match.
	{
		name: "retarget-field-across-composites",
		fixtures: []chainFixture{
			{
				path: "types.go",
				content: `package fixture

type Claim struct {
	Subject string
	Verb    string
	Object  string
}
`,
			},
			{
				path: "claims_alpha.go",
				content: `package fixture

var ClaimA1 = Claim{Subject: "alice", Verb: "knows", Object: "OldTarget"}
var ClaimA2 = Claim{Subject: "bob", Verb: "knows", Object: "OldTarget"}
var ClaimA3 = Claim{Subject: "carol", Verb: "trusts", Object: "SomeoneElse"}
`,
			},
			{
				path: "claims_beta.go",
				content: `package fixture

var ClaimB1 = Claim{Subject: "dave", Verb: "cites", Object: "OldTarget"}
var ClaimB2 = Claim{Subject: "eve", Verb: "refutes", Object: "OldTarget"}
`,
			},
		},
		prompt: `In this fixture, several Claim values have Object: "OldTarget". Retarget every one of those to Object: "NewTarget". Do not touch Claim values whose Object is something else (like "SomeoneElse"). Do not change Subject or Verb on any claim.`,
		mustContain: map[string][]string{
			"claims_alpha.go": {
				`ClaimA1 = Claim{Subject: "alice", Verb: "knows", Object: "NewTarget"}`,
				`ClaimA2 = Claim{Subject: "bob", Verb: "knows", Object: "NewTarget"}`,
				`ClaimA3 = Claim{Subject: "carol", Verb: "trusts", Object: "SomeoneElse"}`,
			},
			"claims_beta.go": {
				`ClaimB1 = Claim{Subject: "dave", Verb: "cites", Object: "NewTarget"}`,
				`ClaimB2 = Claim{Subject: "eve", Verb: "refutes", Object: "NewTarget"}`,
			},
		},
		mustNotContain: map[string][]string{
			"claims_alpha.go": {`Object: "OldTarget"`},
			"claims_beta.go":  {`Object: "OldTarget"`},
		},
	},
	// #71/#75: safe-delete with orphan cleanup. Removing a def leaves
	// dangling references — safe-delete refuses; the agent must first
	// rewrite callers, then delete. defn: 2 ops (edit callers + delete);
	// files: read every file that MIGHT reference it, edit each.
	{
		name: "safe-delete-with-orphan-fix",
		fixtures: []chainFixture{
			{
				path: "helpers.go",
				content: `package fixture

// LegacyHelper is being retired.
func LegacyHelper(s string) string {
	return "[legacy] " + s
}

// Preferred is the new API.
func Preferred(s string) string {
	return "[preferred] " + s
}
`,
			},
			{
				path: "site_a.go",
				content: `package fixture

func UseA(s string) string {
	return LegacyHelper(s)
}
`,
			},
			{
				path: "site_b.go",
				content: `package fixture

func UseB(s string) string {
	return "prefix:" + LegacyHelper(s)
}
`,
			},
		},
		prompt: `LegacyHelper is being retired. Every call site must switch to Preferred (same signature), then delete LegacyHelper itself. When you're done, LegacyHelper should no longer exist and every previous call site should now call Preferred.`,
		mustContain: map[string][]string{
			"site_a.go": {`Preferred(s)`},
			"site_b.go": {`Preferred(s)`},
		},
		mustNotContain: map[string][]string{
			"helpers.go": {`LegacyHelper`},
			"site_a.go":  {`LegacyHelper`},
			"site_b.go":  {`LegacyHelper`},
		},
	},
	// #71/#75: rename a package-level var across the package. defn:
	// op:rename does def + all callers in one shot (verified in
	// TestHandleRename_PackageLevelVar). files-mode: N reads + N edits.
	{
		name: "rename-package-var",
		fixtures: []chainFixture{
			{
				path: "config.go",
				content: `package fixture

const DefaultTimeoutSecs = 30

var DefaultRetries = 3
`,
			},
			{
				path: "client.go",
				content: `package fixture

import "fmt"

func Connect() string {
	return fmt.Sprintf("timeout=%d retries=%d", DefaultTimeoutSecs, DefaultRetries)
}
`,
			},
			{
				path: "worker.go",
				content: `package fixture

func Retry() int {
	total := 0
	for i := 0; i < DefaultRetries; i++ {
		total += DefaultTimeoutSecs
	}
	return total
}
`,
			},
		},
		prompt: `Rename the package-level var DefaultRetries to MaxAttempts across the whole package. Every reference must be updated. Do not touch DefaultTimeoutSecs — that name stays.`,
		mustContain: map[string][]string{
			"config.go": {`var MaxAttempts = 3`},
			"client.go": {`MaxAttempts`, `DefaultTimeoutSecs`},
			"worker.go": {`i < MaxAttempts`, `total += DefaultTimeoutSecs`},
		},
		mustNotContain: map[string][]string{
			"config.go": {`var DefaultRetries`},
			"client.go": {`DefaultRetries`},
			"worker.go": {`DefaultRetries`},
		},
	},
	// #71/#75: extract a magic-number pattern into a named constant
	// across multiple call sites. Exercises exploration (find the
	// occurrences) + write (add const + rewrite call sites).
	{
		name: "extract-magic-number-constant",
		fixtures: []chainFixture{
			{
				path: "limits.go",
				content: `package fixture

// Existing constants live here.
const HeaderSize = 8
`,
			},
			{
				path: "buffer.go",
				content: `package fixture

func BufferCap(items int) int {
	return items * 1024
}
`,
			},
			{
				path: "stream.go",
				content: `package fixture

func StreamChunks(total int) int {
	if total < 1024 {
		return 1
	}
	return total / 1024
}
`,
			},
		},
		prompt: `The number 1024 appears in buffer.go and stream.go as a magic number for "chunk size". Extract it as a named constant ChunkSize (declared in limits.go alongside HeaderSize), then rewrite every 1024 reference in buffer.go and stream.go to use ChunkSize instead. Do NOT change HeaderSize.`,
		mustContain: map[string][]string{
			"limits.go": {`const HeaderSize = 8`, `ChunkSize = 1024`},
			"buffer.go": {`items * ChunkSize`},
			"stream.go": {`total < ChunkSize`, `total / ChunkSize`},
		},
		mustNotContain: map[string][]string{
			"buffer.go": {`* 1024`},
			"stream.go": {`< 1024`, `/ 1024`},
		},
	},
	// #71/#75: move a def between packages. defn: op:move updates all
	// import sites in one shot; files-mode has to add the new file,
	// remove the old, and edit every importer's import block by hand.
	{
		name: "move-def-between-packages",
		fixtures: []chainFixture{
			{
				path: "go.mod",
				content: "module fixture\n\ngo 1.26\n",
			},
			{
				path: "util/util.go",
				content: `package util

// Slugify normalizes a string for use as an identifier.
func Slugify(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == ' ' {
			out = append(out, '-')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
`,
			},
			{
				path: "app/handler.go",
				content: `package app

import "fixture/util"

func Handle(name string) string {
	return "app/" + util.Slugify(name)
}
`,
			},
			{
				path: "app/router.go",
				content: `package app

import "fixture/util"

func Route(path string) string {
	return "/api/" + util.Slugify(path)
}
`,
			},
		},
		prompt: `Move the Slugify function from the util package to the app package. After the move: (a) util/util.go should not contain Slugify anymore (delete the file if empty), (b) app package should own Slugify, (c) app/handler.go and app/router.go should call Slugify directly (same package now — no import path prefix, no util. qualifier). Do not change Slugify's behavior.`,
		mustContain: map[string][]string{
			"app/handler.go": {`Slugify(name)`},
			"app/router.go":  {`Slugify(path)`},
		},
		mustNotContain: map[string][]string{
			"app/handler.go": {`util.Slugify`},
			"app/router.go":  {`util.Slugify`},
			"util/util.go":   {`func Slugify`},
		},
	},
	// #71/#75: split a monolithic function into extracted helpers.
	// Native tools: cut-and-paste extraction; defn: op:create + op:edit
	// as an atomic apply batch.
	{
		name: "split-monolithic-function",
		fixtures: []chainFixture{
			{
				path: "report.go",
				content: `package fixture

import "fmt"

// GenerateReport does a lot: normalizes the title, formats each row, and
// joins the whole thing with a footer. It's grown too big; split it.
func GenerateReport(title string, rows []string) string {
	normalized := ""
	for _, r := range title {
		if r == ' ' {
			normalized += "_"
		} else {
			normalized += string(r)
		}
	}

	formatted := make([]string, 0, len(rows))
	for i, row := range rows {
		formatted = append(formatted, fmt.Sprintf("[%d] %s", i, row))
	}

	body := ""
	for _, f := range formatted {
		body += f + "\n"
	}
	return normalized + "\n" + body + "-- end --"
}
`,
			},
		},
		prompt: `Split GenerateReport into three helpers in the same file: normalizeTitle(title string) string, formatRows(rows []string) []string, and joinBody(rows []string) string. Rewrite GenerateReport to just call the three helpers plus append "-- end --". Do not change external behavior (same output for the same input).`,
		mustContain: map[string][]string{
			"report.go": {
				`func normalizeTitle(title string) string`,
				`func formatRows(rows []string) []string`,
				`func joinBody(rows []string) string`,
				`normalizeTitle(title)`,
				`formatRows(rows)`,
				`joinBody(`,
				`"-- end --"`,
			},
		},
	},
	// #71/#75: add an import + use it. defn: op:add-import (goimports-
	// canonical) + op:edit. files-mode: careful import-block edit +
	// body edit.
	{
		name: "add-import-and-use",
		fixtures: []chainFixture{
			{
				path: "greet.go",
				content: `package fixture

import "fmt"

func Greet(name string) string {
	return fmt.Sprintf("hello, %s", name)
}
`,
			},
		},
		prompt: `Update Greet so the name is normalized to lowercase before formatting. Use strings.ToLower from the standard library — that requires adding an import for "strings" (imports must stay goimports-canonical: stdlib group, alphabetized). Do not touch the fmt import.`,
		mustContain: map[string][]string{
			"greet.go": {
				`"fmt"`,
				`"strings"`,
				`strings.ToLower(name)`,
			},
		},
	},
	// #71/#75: interface-satisfaction rename. Rename a method on the
	// interface; every concrete implementor must rename its method too
	// or the type stops satisfying. defn: op:rename (with receiver);
	// files: search for interface + all types satisfying it + rename in each.
	{
		name: "rename-interface-method-across-impls",
		fixtures: []chainFixture{
			{
				path: "iface.go",
				content: `package fixture

type Handler interface {
	Handle(msg string) string
}
`,
			},
			{
				path: "impl_shout.go",
				content: `package fixture

import "strings"

type Shout struct{}

func (s Shout) Handle(msg string) string {
	return strings.ToUpper(msg) + "!"
}
`,
			},
			{
				path: "impl_whisper.go",
				content: `package fixture

import "strings"

type Whisper struct{}

func (w Whisper) Handle(msg string) string {
	return "(" + strings.ToLower(msg) + ")"
}
`,
			},
			{
				path: "dispatch.go",
				content: `package fixture

func Dispatch(h Handler, msg string) string {
	return h.Handle(msg)
}
`,
			},
		},
		prompt: `Rename the Handler interface's method from Handle to Process. Every type that satisfies Handler (Shout, Whisper) must rename its method too so they still satisfy Handler. Every call site (Dispatch calls h.Handle) must update. Do not rename the types themselves.`,
		mustContain: map[string][]string{
			"iface.go":         {`Process(msg string) string`},
			"impl_shout.go":    {`func (s Shout) Process(`},
			"impl_whisper.go":  {`func (w Whisper) Process(`},
			"dispatch.go":      {`h.Process(msg)`},
		},
		mustNotContain: map[string][]string{
			"iface.go":        {`Handle(msg`},
			"impl_shout.go":   {`func (s Shout) Handle(`},
			"impl_whisper.go": {`func (w Whisper) Handle(`},
			"dispatch.go":     {`h.Handle(`},
		},
	},
}

// runChainBench runs every chain case in both files-mode and defn-mode.
// Each case gets its own fresh scratch dir so DB state from prior cases
// can't leak into later cases — the shared-scratch v1 shape leaked
// state from case 1's projection-op edits into case 2's rename, making
// the isolated fix look like it wasn't taking effect. Fresh scratch per
// case costs ~500ms of setup (mktemp + git init + defn init) but is
// worth it for correctness signal integrity.
func runChainBench(defnBin string) {
	fmt.Printf("\n=== Running %d chain cases in both modes (fresh scratch per case) ===\n\n", len(chains))

	var filesResults, defnResults []mutationResult
	for i, c := range chains {
		fmt.Printf("[%d/%d] %s (%d fixture files)\n", i+1, len(chains), c.name, len(c.fixtures))

		filesScratch := prepareChainScratch(defnBin)
		rFiles := runChainCase(filesScratch, defnBin, c, "files")
		filesResults = append(filesResults, rFiles)
		fmt.Printf("  files:  %d calls, %s, in/out/cache=%d/%d/%d tok, correct=%v\n",
			rFiles.toolCalls, rFiles.duration.Round(time.Second),
			rFiles.inputTokens, rFiles.outputTokens, rFiles.cachedTokens, rFiles.correct)
		os.RemoveAll(filesScratch)

		defnScratch := prepareChainScratch(defnBin)
		rDefn := runChainCase(defnScratch, defnBin, c, "defn")
		defnResults = append(defnResults, rDefn)
		fmt.Printf("  defn:   %d calls, %s, in/out/cache=%d/%d/%d tok, correct=%v\n",
			rDefn.toolCalls, rDefn.duration.Round(time.Second),
			rDefn.inputTokens, rDefn.outputTokens, rDefn.cachedTokens, rDefn.correct)
		os.RemoveAll(defnScratch)

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

// prepareChainScratch spins up a fresh scratch git+defn repo for a single
// case run. Caller is responsible for os.RemoveAll(returned path) when done.
func prepareChainScratch(defnBin string) string {
	scratch, err := os.MkdirTemp("", "defn-bench-chain-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "scratch dir: %v\n", err)
		os.Exit(1)
	}
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
	return scratch
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

	if !res.correct {
		if dumpDir, dumpErr := os.MkdirTemp("", "defn-bench-fail-"+c.name+"-"+mode+"-*"); dumpErr == nil {
			_ = os.WriteFile(filepath.Join(dumpDir, "stream.jsonl"), out, 0644)
			for _, f := range c.fixtures {
				finalBytes, ferr := os.ReadFile(filepath.Join(scratch, f.path))
				if ferr == nil {
					_ = os.WriteFile(filepath.Join(dumpDir, "final-"+strings.ReplaceAll(f.path, "/", "_")), finalBytes, 0644)
				}
			}
			// Also snapshot the DB state via `defn query` so we can see whether
			// the bug is in the DB (both defs present) or in the emit path
			// (DB clean but disk dirty). Runs against the same scratch dir.
			dbQ := exec.Command(defnBin, "query", "SELECT id, name, kind, source_file FROM definitions ORDER BY id")
			dbQ.Dir = scratch
			if dbOut, dbErr := dbQ.CombinedOutput(); dbErr == nil {
				_ = os.WriteFile(filepath.Join(dumpDir, "db-post-run.txt"), dbOut, 0644)
			}
			// Preserve the .defn dir too so future debug can `defn query`
			// against it later. Copy is cheaper than a symlink dance.
			preservedDefn := filepath.Join(dumpDir, "defn-snapshot")
			cp := exec.Command("cp", "-a", filepath.Join(scratch, ".defn"), preservedDefn)
			_ = cp.Run()
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
