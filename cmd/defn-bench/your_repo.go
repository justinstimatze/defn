package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runYourRepoBench runs a single user-supplied task against the user's
// own repo, in both files-mode and defn-mode, and prints a per-mode
// comparison table. Read-only: does not modify the repo (mutation
// support is a follow-up gated on --allow-mutations, tracked separately).
//
// This is marketing playbook move #3 ("Ship the audit workflow as a
// tool"): users verify defn's read-tax claim on their own code rather
// than trust a vendored fixture.
func runYourRepoBench(defnBin, repoDir, task string) {
	if repoDir == "" || task == "" {
		fmt.Fprintln(os.Stderr, "your-repo: --your-repo <dir> and --task <string> are both required")
		os.Exit(1)
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve %s: %v\n", repoDir, err)
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		fmt.Fprintf(os.Stderr, "%s does not contain a go.mod — pick the module root\n", abs)
		os.Exit(1)
	}
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintln(os.Stderr, "claude CLI not found in PATH")
		os.Exit(1)
	}

	fmt.Printf("=== your-repo bench ===\n")
	fmt.Printf("repo:  %s\n", abs)
	fmt.Printf("task:  %s\n\n", task)

	// Ensure .defn exists so defn-mode has something to serve from. If
	// it already exists we leave it; users who don't want persistence
	// can delete it after. If it doesn't we initialize a fresh one
	// (idempotent). Ingest is unconditional so numbers reflect current
	// tree state.
	if _, err := os.Stat(filepath.Join(abs, ".defn")); err != nil {
		fmt.Println("no .defn/ — running `defn init` (idempotent, will be reused)…")
		initCmd := exec.Command(defnBin, "init", abs)
		initCmd.Stderr = os.Stderr
		if err := initCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "defn init failed: %v\n", err)
			os.Exit(1)
		}
	}
	ingestCmd := exec.Command(defnBin, "ingest", ".")
	ingestCmd.Dir = abs
	ingestCmd.Stderr = os.Stderr
	if err := ingestCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "defn ingest failed: %v\n", err)
		os.Exit(1)
	}

	rFiles := runYourRepoCase(abs, task, "files")
	fmt.Printf("files:  %d calls, %s, in/out/cache=%d/%d/%d tok\n",
		rFiles.toolCalls, rFiles.duration.Round(time.Second),
		rFiles.inputTokens, rFiles.outputTokens, rFiles.cachedTokens)

	rDefn := runYourRepoCase(abs, task, "defn")
	fmt.Printf("defn:   %d calls, %s, in/out/cache=%d/%d/%d tok\n\n",
		rDefn.toolCalls, rDefn.duration.Round(time.Second),
		rDefn.inputTokens, rDefn.outputTokens, rDefn.cachedTokens)

	fmt.Println("=== summary ===")
	fmt.Printf("%-8s %6s %8s %8s %8s\n", "mode", "calls", "in.tok", "out.tok", "cache")
	fmt.Println(strings.Repeat("-", 46))
	fmt.Printf("%-8s %6d %8d %8d %8d\n", "files", rFiles.toolCalls, rFiles.inputTokens, rFiles.outputTokens, rFiles.cachedTokens)
	fmt.Printf("%-8s %6d %8d %8d %8d\n", "defn", rDefn.toolCalls, rDefn.inputTokens, rDefn.outputTokens, rDefn.cachedTokens)
	fmt.Println(strings.Repeat("-", 46))
	if rFiles.inputTokens > 0 {
		delta := float64(rFiles.inputTokens-rDefn.inputTokens) / float64(rFiles.inputTokens) * 100
		fmt.Printf("input token Δ vs files: %+.0f%%  (negative = defn cheaper)\n", -delta)
	}
	if rFiles.toolCalls > 0 {
		delta := float64(rFiles.toolCalls-rDefn.toolCalls) / float64(rFiles.toolCalls) * 100
		fmt.Printf("tool call Δ vs files:   %+.0f%%\n", -delta)
	}
	fmt.Println()
	fmt.Println("This is a read-side comparison — no repo files were modified.")
	fmt.Println("Sanity-check both answers by hand; token counts alone don't prove correctness.")
}

// runYourRepoCase runs one claude -p invocation in the given mode. In
// files-mode we temporarily rename .mcp.json + CLAUDE.md aside so the
// agent doesn't see the defn tool or its instructions; in defn-mode
// we ensure both are present. State is restored on return regardless
// of outcome.
func runYourRepoCase(repoDir, task, mode string) mutationResult {
	mcpPath := filepath.Join(repoDir, ".mcp.json")
	claudeMDPath := filepath.Join(repoDir, "CLAUDE.md")
	mcpSaved := mcpPath + ".defn-bench.bak"
	claudeMDSaved := claudeMDPath + ".defn-bench.bak"

	if mode == "files" {
		_ = os.Rename(mcpPath, mcpSaved)
		_ = os.Rename(claudeMDPath, claudeMDSaved)
		defer func() {
			_ = os.Rename(mcpSaved, mcpPath)
			_ = os.Rename(claudeMDSaved, claudeMDPath)
		}()
	}

	prompt := task
	if mode == "defn" {
		if b, err := os.ReadFile(claudeMDPath); err == nil {
			prompt = string(b) + "\n\n---\n\n" + task
		}
	}

	start := time.Now()
	args := []string{"-p", "--verbose", "--output-format", "stream-json"}
	if mode == "defn" {
		args = append(args, "--mcp-config", ".mcp.json")
	}
	cmd := exec.Command("claude", args...)
	cmd.Dir = repoDir
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)

	res := mutationResult{name: "your-repo", mode: mode, duration: dur, rawOutput: string(out)}
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s mode failed: %v\n", mode, err)
		return res
	}
	stats := parseStreamJSON(out)
	res.toolCalls = stats.ToolCalls
	res.inputTokens = stats.InputTokens
	res.outputTokens = stats.OutputTokens
	res.cachedTokens = stats.CachedTokens
	return res
}
