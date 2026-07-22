// Command defn is an AI-native code database for Go source code.
// Stores definitions in SQLite via internal/store.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn init <path>")
			os.Exit(1)
		}
		cmdInit(os.Args[2])
	case "repair":
		dir := "."
		if len(os.Args) >= 3 {
			dir = os.Args[2]
		}
		cmdRepair(dir)
	case "ingest":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn ingest <path>")
			os.Exit(1)
		}
		cmdIngest(os.Args[2])
	case "ingest-upstream":
		cmdIngestUpstream(os.Args[2:])
	case "sync":
		file := ""
		if len(os.Args) >= 3 {
			file = os.Args[2]
		}
		cmdSync(file)
	case "measure-rename":
		// #109 pass 2 measurement path — winze needs a way to time
		// rename against a live .defn without spinning up serve + MCP.
		// #119 --in-place: pre-populate scratch with one full emit
		// (untimed), then time the rename so file-scoped emit + package-
		// scoped build (#117/#118) actually apply. Without this the
		// tempdir starts empty, forcing full emit + full build every time
		// and masking the optimization.
		args, inPlace := extractInPlaceFlag(os.Args[2:])
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: defn measure-rename [--in-place] <old-name> <new-name>")
			os.Exit(1)
		}
		cmdMeasureRename(args[0], args[1], inPlace)
	case "measure-edit":
		// #115 symmetric measurement path for the edit thesis.
		// #119 --in-place: same rationale as measure-rename above.
		args, inPlace := extractInPlaceFlag(os.Args[2:])
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: defn measure-edit [--in-place] <name> <body-file>")
			os.Exit(1)
		}
		cmdMeasureEdit(args[0], args[1], inPlace)
	case "search":
		pattern, rank, jsonFlag, limit, err := parseSearchArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cmdSearch(pattern, rank, jsonFlag, limit)
	case "serve":
		httpAddr := ""
		if len(os.Args) >= 4 && os.Args[2] == "--http" {
			httpAddr = os.Args[3]
		}
		cmdServe(httpAddr)
	case "emit":
		dir := "."
		if len(os.Args) >= 3 {
			dir = os.Args[2]
		}
		cmdEmit(dir)
	case "impact":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn impact [--json] <definition-name>")
			os.Exit(1)
		}
		jsonFlag := false
		impactName := os.Args[2]
		if impactName == "--json" {
			jsonFlag = true
			if len(os.Args) < 4 {
				fmt.Fprintln(os.Stderr, "usage: defn impact [--json] <definition-name>")
				os.Exit(1)
			}
			impactName = os.Args[3]
		}
		cmdImpact(impactName, jsonFlag)
	case "untested":
		cmdUntested()
	case "lint":
		cmdLint()
	case "status":
		jsonFlag := false
		for _, a := range os.Args[2:] {
			if a == "--json" {
				jsonFlag = true
			}
		}
		cmdStatus(jsonFlag)
	case "check":
		cmdCheck()
	case "watch":
		dir := "."
		if len(os.Args) >= 3 {
			dir = os.Args[2]
		}
		cmdWatch(dir)
	case "query":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn query <sql>")
			os.Exit(1)
		}
		cmdQuery(os.Args[2])
	case "gc":
		cmdGC()
	case "restart":
		all := false
		for _, a := range os.Args[2:] {
			if a == "--all" {
				all = true
			}
		}
		cmdRestart(all)
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `defn — AI-native code database (SQLite-backed)

Usage:
  defn init <path>             Ingest + configure MCP
  defn repair [path]           Delete .defn and re-ingest (recovers from corruption)
  defn ingest <path>           Parse Go source → SQLite database
  defn sync [file]             Re-ingest (single file: fast path via IngestFile+ResolveFile; falls back to full ingest above 50 stale files)
  defn serve                   MCP server for Claude Code
  defn emit <output-dir>       Database → .go files
  defn impact <name>           Blast radius + test coverage
  defn untested                Definitions without test coverage
  defn lint                    Lint with diagnostics → definitions
  defn status                  Backend stats
  defn check                   Consistency diagnostics (defs by kind, orphan literal types)
  defn query <sql>             Read-only SQL query
  defn gc                      Compact storage (VACUUM)
  defn restart [--all]         Gracefully bounce this project's serve (or all)
  defn measure-rename [--in-place] <old> <new>    Time a rename against .defn without spinning up serve
  defn measure-edit   [--in-place] <name> <body-file>  Time an edit; body-file keeps multi-line source shell-safe
                               --in-place pre-populates scratch with one full emit before timing so
                               file-scoped emit + package-scoped build actually apply (real interactive
                               cost); without it, fresh-tempdir mode gives the un-optimized ceiling number.`)
}

// extractInPlaceFlag pops --in-place out of argv (if present) and
// returns the remaining positional args plus a bool. Kept here so both
// measure-rename and measure-edit share the same tiny flag parsing.
func extractInPlaceFlag(args []string) ([]string, bool) {
	out := args[:0]
	inPlace := false
	for _, a := range args {
		if a == "--in-place" {
			inPlace = true
			continue
		}
		out = append(out, a)
	}
	return out, inPlace
}

// parseSearchArgs parses the argv tail for `defn search`. Returned err
// carries the usage line so the caller just prints and exits. Extracted
// from the main switch so the flag handling is unit-testable.
func parseSearchArgs(args []string) (pattern string, rank, jsonFlag bool, limit int, err error) {
	const usage = "usage: defn search [--rank] [--json] [--limit N] <pattern>"
	limit = 20
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--rank":
			rank = true
		case a == "--json":
			jsonFlag = true
		case a == "--limit":
			if i+1 >= len(args) {
				return "", false, false, 0, fmt.Errorf("%s", usage)
			}
			var n int
			if _, scanErr := fmt.Sscanf(args[i+1], "%d", &n); scanErr != nil || n <= 0 {
				return "", false, false, 0, fmt.Errorf("--limit requires a positive integer")
			}
			limit = n
			i++
		case strings.HasPrefix(a, "--"):
			return "", false, false, 0, fmt.Errorf("unknown flag %q\n%s", a, usage)
		default:
			if pattern != "" {
				return "", false, false, 0, fmt.Errorf("multiple positional args (%q, %q); pattern must be a single word\n%s", pattern, a, usage)
			}
			pattern = a
		}
	}
	if pattern == "" {
		return "", false, false, 0, fmt.Errorf("%s", usage)
	}
	return pattern, rank, jsonFlag, limit, nil
}
