// expand-bytes-measure — cheap test #2 for the expand op design.
//
// For each target definition, prints:
//
//	files_bytes       — whole source file (files-mode Read equivalent)
//	read_bytes        — defn handleGetDefinition output shape
//	impact_bytes      — defn handleImpact output shape
//	read_plus_impact  — read_bytes + impact_bytes (today's multi-hop cost)
//	expand_bytes      — defn handleExpand v1 output shape (body+callers)
//
// Ratios to look at:
//
//	expand_bytes / files_bytes     — is expand competitive with files-mode?
//	expand_bytes / read_plus_impact — is expand an improvement over 2 calls?
//
// Landed 2026-07-17 alongside the fix that removed the buggy
// getInterfaceDispatchCallers heuristic; kept so anyone can rerun the
// before/after comparison on their own repo ingest.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
)

type target struct {
	Name     string
	Receiver string
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: expand-bytes-measure <project-root> [--print] <target1> [target2 ...]")
		fmt.Fprintln(os.Stderr, "  --print: dump the expand output text for each target, not just byte counts")
		os.Exit(2)
	}
	root := os.Args[1]
	printMode := false
	args := os.Args[2:]
	if len(args) > 0 && args[0] == "--print" {
		printMode = true
		args = args[1:]
	}
	rawTargets := args

	dbPath := filepath.Join(root, ".defn")
	db, err := store.Open(dbPath)
	if err != nil {
		fatalf("open db %s: %v", dbPath, err)
	}
	defer db.Close()

	mods, _ := db.ListModules()
	moduleByID := map[int64]string{}
	for _, m := range mods {
		moduleByID[m.ID] = m.Path
	}

	if !printMode {
		fmt.Printf("%-35s  %8s  %8s  %8s  %8s  %8s  %10s  %10s\n",
			"target", "files", "read", "impact", "r+i", "expand",
			"exp/files", "exp/r+i")
	}

	for _, name := range rawTargets {
		d, err := db.GetDefinitionByName(name, "")
		if err != nil {
			fmt.Printf("%-35s  NOT FOUND (%v)\n", name, err)
			continue
		}
		modulePath := moduleByID[d.ModuleID]

		fileBytes := readFileBytes(root, d.SourceFile)
		readOut := renderRead(d, modulePath)
		impact, err := db.GetImpact(d.ID)
		if err != nil {
			fmt.Printf("%-35s  impact err: %v\n", name, err)
			continue
		}
		impactOut := renderImpact(d, impact, modulePath)
		expandOut := renderExpand(d, impact, modulePath)

		if printMode {
			fmt.Printf("\n===== %s (files=%d, expand=%d bytes) =====\n%s\n",
				name, fileBytes, len(expandOut), expandOut)
			continue
		}
		fmt.Printf("%-35s  %8d  %8d  %8d  %8d  %8d  %10.2f  %10.2f\n",
			name,
			fileBytes,
			len(readOut),
			len(impactOut),
			len(readOut)+len(impactOut),
			len(expandOut),
			ratio(len(expandOut), fileBytes),
			ratio(len(expandOut), len(readOut)+len(impactOut)),
		)
	}
}

func readFileBytes(root, srcFile string) int {
	if srcFile == "" {
		return 0
	}
	p := srcFile
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, srcFile)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return -1
	}
	return len(b)
}

func formatReceiver(r string) string {
	if r == "" {
		return ""
	}
	return "(" + r + ") "
}

// renderRead mirrors handleGetDefinition's markdown output (non-D branch).
func renderRead(d *store.Definition, modulePath string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", formatReceiver(d.Receiver), d.Name, d.Kind))
	sb.WriteString(fmt.Sprintf("Module: %s\n\n", modulePath))
	if d.Doc != "" {
		sb.WriteString(d.Doc + "\n\n")
	}
	sb.WriteString("```go\n")
	sb.WriteString(d.Body)
	sb.WriteString("\n```\n")
	return sb.String()
}

// renderImpact mirrors handleImpact's markdown output.
func renderImpact(d *store.Definition, impact *store.Impact, modulePath string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", formatReceiver(d.Receiver), d.Name, d.Kind))
	sb.WriteString(fmt.Sprintf("Module: %s\n\n", modulePath))
	var prod, tests []store.Definition
	for _, c := range impact.DirectCallers {
		if c.Test {
			tests = append(tests, c)
		} else {
			prod = append(prod, c)
		}
	}
	sb.WriteString(fmt.Sprintf("Direct callers: %d (%d production, %d test)\n",
		len(impact.DirectCallers), len(prod), len(tests)))
	for _, c := range prod {
		name := formatReceiver(c.Receiver) + c.Name
		if c.SourceFile != "" && c.StartLine > 0 {
			sb.WriteString(fmt.Sprintf("  %s  (%s:%d)\n", name, c.SourceFile, c.StartLine))
		} else {
			sb.WriteString(fmt.Sprintf("  %s\n", name))
		}
	}
	sb.WriteString(fmt.Sprintf("Transitive callers: %d\n", impact.TransitiveCount))
	sb.WriteString(fmt.Sprintf("Tests covering this: %d\n", len(impact.Tests)))
	if impact.UncoveredBy > 0 {
		sb.WriteString(fmt.Sprintf("Uncovered direct callers: %d\n", impact.UncoveredBy))
	}
	return sb.String()
}

// renderExpand mirrors handleExpand v1's markdown output with body+callers.
func renderExpand(d *store.Definition, impact *store.Impact, modulePath string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", formatReceiver(d.Receiver), d.Name, d.Kind))
	if modulePath != "" {
		sb.WriteString(fmt.Sprintf("Module: %s\n", modulePath))
	}
	sb.WriteString("\n")

	sb.WriteString("### body\n")
	if d.Doc != "" {
		sb.WriteString(d.Doc + "\n\n")
	}
	sb.WriteString("```go\n")
	sb.WriteString(d.Body)
	sb.WriteString("\n```\n\n")

	var prod, tests []store.Definition
	for _, c := range impact.DirectCallers {
		if c.Test {
			tests = append(tests, c)
		} else {
			prod = append(prod, c)
		}
	}
	sb.WriteString(fmt.Sprintf("### callers (%d — %d production, %d test)\n",
		len(impact.DirectCallers), len(prod), len(tests)))
	for _, c := range prod {
		name := formatReceiver(c.Receiver) + c.Name
		if c.SourceFile != "" && c.StartLine > 0 {
			sb.WriteString(fmt.Sprintf("- %s  (%s:%d)\n", name, c.SourceFile, c.StartLine))
		} else {
			sb.WriteString(fmt.Sprintf("- %s\n", name))
		}
	}
	for _, c := range tests {
		name := formatReceiver(c.Receiver) + c.Name
		if c.SourceFile != "" && c.StartLine > 0 {
			sb.WriteString(fmt.Sprintf("- %s _(test)_  (%s:%d)\n", name, c.SourceFile, c.StartLine))
		} else {
			sb.WriteString(fmt.Sprintf("- %s _(test)_\n", name))
		}
	}
	if len(impact.DirectCallers) == 0 {
		sb.WriteString("(none)\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func fatalf(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(1)
}
