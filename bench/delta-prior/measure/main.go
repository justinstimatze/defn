// bench/delta-prior — measures raw byte savings from D delta-from-prior.
//
// Reads a curated list of target symbols from an ingested project DB.
// For each symbol, records three sizes:
//   files_bytes  — whole file containing the symbol (files-mode Read equivalent)
//   full_bytes   — defn's current body-in-fence rendering (defn-natural)
//   compact_bytes — the sig+doc+provenance-tag form (defn-D active)
//
// Compact size is computed from the same template shape as renderUpstreamMatch
// in internal/mcp/server.go. If that template changes, this measurement drifts.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
)

type target struct {
	Name     string
	Receiver string
	Kind     string
}

func main() {
	dbPath := flag.String("db", "", "path to project root containing .defn/ (required)")
	modulePath := flag.String("module", "", "upstream module path (used for tag rendering, e.g. github.com/go-chi/chi/v5)")
	version := flag.String("version", "v5.1.0", "upstream version tag for rendering")
	targetsFile := flag.String("targets", "", "path to newline-separated target list: Name,Receiver,Kind per line (# for comments)")
	out := flag.String("out", "", "output CSV path (stdout if empty)")
	flag.Parse()

	if *dbPath == "" || *targetsFile == "" {
		fmt.Fprintln(os.Stderr, "usage: measure --db <project-root> --module <path> --version <v> --targets <file>")
		os.Exit(2)
	}

	targets, err := loadTargets(*targetsFile)
	if err != nil {
		fatal(fmt.Errorf("load targets: %w", err))
	}

	dbFile := filepath.Join(*dbPath, ".defn")
	db, err := store.Open(dbFile)
	if err != nil {
		fatal(fmt.Errorf("open db %s: %w", dbFile, err))
	}
	defer db.Close()

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fatal(err)
		}
		defer f.Close()
		w = f
	}
	cw := csv.NewWriter(w)
	defer cw.Flush()

	_ = cw.Write([]string{
		"name", "receiver", "kind",
		"files_bytes", "full_bytes", "compact_bytes", "tag_only_bytes", "adaptive_bytes",
		"D_vs_full_pct", "tag_vs_full_pct", "adaptive_vs_full_pct",
	})

	var totFiles, totFull, totCompact, totTag, totAdapt int
	seenFiles := map[string]int{}

	for _, t := range targets {
		d, err := lookupDef(db, t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "SKIP %s: %v\n", t.Name, err)
			continue
		}

		filesBytes, err := readFileSize(*dbPath, d)
		if err != nil {
			fmt.Fprintf(os.Stderr, "file-size %s: %v\n", t.Name, err)
			filesBytes = 0
		}
		fullBytes := renderFullBytes(d, *modulePath)
		compactBytes := renderCompactBytes(d, *modulePath, *version)
		tagBytes := renderTagOnlyBytes(d, *modulePath, *version)
		adaptiveBytes := fullBytes
		if compactBytes < adaptiveBytes {
			adaptiveBytes = compactBytes
		}
		if tagBytes < adaptiveBytes {
			adaptiveBytes = tagBytes
		}

		// Each source file only counts once in the files-arm sum (agent reads it once, then all symbols in it are free).
		fileKey := d.SourceFile
		if _, seen := seenFiles[fileKey]; !seen {
			seenFiles[fileKey] = filesBytes
			totFiles += filesBytes
		}
		totFull += fullBytes
		totCompact += compactBytes
		totTag += tagBytes
		totAdapt += adaptiveBytes

		dFull := ftoaRatio(fullBytes-compactBytes, fullBytes)
		dTag := ftoaRatio(fullBytes-tagBytes, fullBytes)
		dAdapt := ftoaRatio(fullBytes-adaptiveBytes, fullBytes)

		_ = cw.Write([]string{
			d.Name, d.Receiver, d.Kind,
			itoa(filesBytes), itoa(fullBytes), itoa(compactBytes), itoa(tagBytes), itoa(adaptiveBytes),
			dFull, dTag, dAdapt,
		})
	}

	_ = cw.Write([]string{
		"TOTAL", "", "",
		itoa(totFiles), itoa(totFull), itoa(totCompact), itoa(totTag), itoa(totAdapt),
		ftoaRatio(totFull-totCompact, totFull),
		ftoaRatio(totFull-totTag, totFull),
		ftoaRatio(totFull-totAdapt, totFull),
	})
}

func loadTargets(path string) ([]target, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ts []target
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		t := target{Name: parts[0]}
		if len(parts) > 1 {
			t.Receiver = parts[1]
		}
		if len(parts) > 2 {
			t.Kind = parts[2]
		}
		ts = append(ts, t)
	}
	return ts, nil
}

func lookupDef(db *store.DB, t target) (*store.Definition, error) {
	defs, err := db.FindDefinitions("%" + t.Name + "%")
	if err != nil {
		return nil, err
	}
	var candidates []store.Definition
	for _, d := range defs {
		if d.Name != t.Name {
			continue
		}
		if t.Receiver != "" && strings.TrimPrefix(d.Receiver, "*") != strings.TrimPrefix(t.Receiver, "*") {
			continue
		}
		if t.Kind != "" && d.Kind != t.Kind {
			continue
		}
		if d.Test {
			continue
		}
		candidates = append(candidates, d)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no match for %+v", t)
	}
	best := candidates[0]
	for _, d := range candidates[1:] {
		if len(d.Body) > len(best.Body) {
			best = d
		}
	}
	return &best, nil
}

func readFileSize(root string, d *store.Definition) (int, error) {
	if d.SourceFile == "" {
		return 0, fmt.Errorf("no source file recorded")
	}
	path := filepath.Join(root, d.SourceFile)
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return int(info.Size()), nil
}

// renderFullBytes replicates handleGetDefinition's body-in-fence output shape.
// See internal/mcp/server.go — kept in sync manually.
func renderFullBytes(d *store.Definition, modulePath string) int {
	var sb strings.Builder
	recv := ""
	if d.Receiver != "" {
		recv = fmt.Sprintf("(%s) ", d.Receiver)
	}
	sb.WriteString(fmt.Sprintf("## %s%s (%s)\n", recv, d.Name, d.Kind))
	sb.WriteString(fmt.Sprintf("Module: %s\n\n", modulePath))
	if d.Doc != "" {
		sb.WriteString(d.Doc + "\n\n")
	}
	sb.WriteString("```go\n")
	sb.WriteString(d.Body)
	sb.WriteString("\n```\n")
	return sb.Len()
}

// renderCompactBytes replicates renderUpstreamMatch's compact form as
// shipped (post-2026-07-17 rework). Header line + full:true hint only;
// doc + sig deliberately excluded.
func renderCompactBytes(d *store.Definition, modulePath, version string) int {
	var sb strings.Builder
	recv := ""
	if d.Receiver != "" {
		recv = fmt.Sprintf("(%s) ", d.Receiver)
	}
	sb.WriteString(fmt.Sprintf("## %s%s (%s) — %s @ %s unchanged from upstream\n",
		recv, d.Name, d.Kind, modulePath, version))
	sb.WriteString("(pass `full: true` for body + doc + sig)\n")
	return sb.Len()
}

// renderTagOnlyBytes: minimal provenance form — just the header line + hint.
// Tests whether stripping doc + sig makes the compact form actually smaller
// than defn-natural on small library bodies.
func renderTagOnlyBytes(d *store.Definition, modulePath, version string) int {
	var sb strings.Builder
	recv := ""
	if d.Receiver != "" {
		recv = fmt.Sprintf("(%s) ", d.Receiver)
	}
	sb.WriteString(fmt.Sprintf("%s%s @ %s (unchanged upstream; pass full:true)\n",
		recv, d.Name, version))
	_ = modulePath
	return sb.Len()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func itoa(n int) string    { return fmt.Sprintf("%d", n) }
func ftoa(f float64) string { return fmt.Sprintf("%.1f", f) }
func ftoaRatio(num, denom int) string {
	if denom == 0 {
		return "0.0"
	}
	return ftoa(100.0 * float64(num) / float64(denom))
}
