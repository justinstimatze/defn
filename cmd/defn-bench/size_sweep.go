package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The size-sweep bench runs one fixed mutation (add-import) across a
// range of fixture LOC targets, in both files-mode and defn-mode. The
// goal is not "does defn win on average" (the single-op mutation
// bench answers that) — it's "at what fixture size does defn's read-
// tax advantage cross over from losing to winning?"
//
// Answering that question is a marketing prerequisite: crossover-
// curve plots are the honest way to show the shape, per the "show
// crossover, not peak" playbook move in bench/README.md.
//
// We fix the mutation so per-size deltas are apples-to-apples. The
// target site (the import block) is always at the top of the file,
// so files-mode's Read cost scales with LOC even though the actual
// edit doesn't.
var sweepSizes = []int{10, 25, 50, 100, 200, 400, 800}

// buildSweepFile emits a self-contained package with an import block
// at the top and enough padding functions to hit approximately loc
// lines. Every declared import is used so `defn emit` + build won't
// fail on "imported and not used" after the mutation lands.
//
// The mutation target — the import block header — is always at line
// 3, independent of loc. Only surrounding padding grows.
func buildSweepFile(loc int) string {
	var b strings.Builder
	b.WriteString("package fixture\n\nimport (\n")
	for _, p := range []string{
		"bytes", "context", "encoding/json", "errors", "fmt",
	} {
		fmt.Fprintf(&b, "\t%q\n", p)
	}
	b.WriteString(")\n\nvar ErrEmpty = errors.New(\"empty\")\n\n")
	// Header + imports + trailing helpers ≈ 22 lines fixed. Each
	// padding function below is exactly 11 lines (doc + sig + 3-
	// line if + 2 buf lines + buf.Write + return + close-brace +
	// trailing blank). Choose n so total lands near loc.
	n := (loc - 22) / 11
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "// Op%d performs step %d in the pipeline.\n", i, i)
		fmt.Fprintf(&b, "func Op%d(ctx context.Context, in []byte) ([]byte, error) {\n", i)
		b.WriteString("\tif len(in) == 0 {\n\t\treturn nil, ErrEmpty\n\t}\n")
		fmt.Fprintf(&b, "\tbuf := bytes.NewBuffer(nil)\n\tbuf.WriteString(fmt.Sprintf(%q, %d))\n", "op-%d:", i)
		b.WriteString("\tbuf.Write(in)\n")
		b.WriteString("\treturn buf.Bytes(), nil\n}\n\n")
	}
	// Guarantee json + context are actually referenced even at the
	// smallest loc so the file always type-checks.
	b.WriteString(`func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func WithBudget(ctx context.Context) context.Context {
	return ctx
}
`)
	return b.String()
}

// runSizeSweepBench runs the add-import mutation at every sweep size,
// samples times per (size, mode), and writes the result table + a CSV
// row-per-invocation to csvPath (or ./size-sweep.csv if empty).
func runSizeSweepBench(defnBin string, samples int, csvPath string) {
	if samples < 1 {
		samples = 1
	}
	if csvPath == "" {
		csvPath = "size-sweep.csv"
	}
	scratch, err := os.MkdirTemp("", "defn-bench-sweep-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "scratch dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(scratch)

	fmt.Printf("scratch: %s\n", scratch)
	fmt.Printf("samples per (size, mode): %d\n", samples)
	fmt.Printf("sizes: %v\n\n", sweepSizes)

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

	f, err := os.Create(csvPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create csv %s: %v\n", csvPath, err)
		os.Exit(1)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{
		"mutation", "loc_target", "loc_actual",
		"sample", "mode", "tool_calls",
		"input_tokens", "output_tokens", "cached_tokens",
		"duration_ms", "correct",
	})

	total := len(sweepSizes) * samples * 2
	step := 0
	type key struct {
		size, sample int
		mode         string
	}
	agg := map[key]mutationResult{}
	for _, size := range sweepSizes {
		fixtureContents := buildSweepFile(size)
		actualLOC := strings.Count(fixtureContents, "\n")
		m := mutation{
			name:            fmt.Sprintf("add-import-loc-%d", size),
			fixtureFile:     fmt.Sprintf("sweep_%d.go", size),
			fixtureContents: fixtureContents,
			prompt:          fmt.Sprintf(`In the file sweep_%d.go, add the "hash/fnv" standard-library import. Do not modify any function. Do not add any other imports.`, size),
			mustContain: []string{
				`"hash/fnv"`,
				`"fmt"`,
			},
		}
		for sample := 0; sample < samples; sample++ {
			for _, mode := range []string{"files", "defn"} {
				step++
				fmt.Printf("[%d/%d] size=%d sample=%d mode=%s\n", step, total, size, sample, mode)
				r := runMutationCase(scratch, defnBin, m, mode)
				agg[key{size, sample, mode}] = r
				fmt.Printf("  %d calls, %s, in/out/cache=%d/%d/%d tok, correct=%v\n",
					r.toolCalls, r.duration.Round(time.Second),
					r.inputTokens, r.outputTokens, r.cachedTokens, r.correct)
				_ = w.Write([]string{
					m.name,
					strconv.Itoa(size),
					strconv.Itoa(actualLOC),
					strconv.Itoa(sample),
					mode,
					strconv.Itoa(r.toolCalls),
					strconv.Itoa(r.inputTokens),
					strconv.Itoa(r.outputTokens),
					strconv.Itoa(r.cachedTokens),
					strconv.FormatInt(r.duration.Milliseconds(), 10),
					strconv.FormatBool(r.correct),
				})
				w.Flush()
			}
		}
	}

	fmt.Println("\n=== Size sweep — mean per (size, mode) ===")
	fmt.Printf("%6s %6s %8s %8s %8s %6s\n", "size", "mode", "in.tok", "out.tok", "calls", "ok/n")
	fmt.Println(strings.Repeat("-", 52))
	for _, size := range sweepSizes {
		for _, mode := range []string{"files", "defn"} {
			var inSum, outSum, callSum, okCount int
			for sample := 0; sample < samples; sample++ {
				r := agg[key{size, sample, mode}]
				inSum += r.inputTokens
				outSum += r.outputTokens
				callSum += r.toolCalls
				if r.correct {
					okCount++
				}
			}
			fmt.Printf("%6d %6s %8d %8d %8d %6s\n",
				size, mode,
				inSum/samples, outSum/samples, callSum/samples,
				fmt.Sprintf("%d/%d", okCount, samples))
		}
	}
	fmt.Println()
	fmt.Printf("CSV written to %s (%d rows)\n", csvPath, len(sweepSizes)*samples*2)
	fmt.Println("Plot column: input_tokens by (loc_actual, mode). Crossover = smallest loc_actual where mean(defn) < mean(files).")
}
