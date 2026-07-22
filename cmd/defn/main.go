// Command defn is an AI-native code database for Go source code.
// Stores definitions in Dolt (SQL database with git semantics).
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
			fmt.Fprintln(os.Stderr, "usage: defn init <path> [--server]")
			os.Exit(1)
		}
		serverMode := false
		for _, a := range os.Args[3:] {
			if a == "--server" {
				serverMode = true
			}
		}
		if serverMode {
			cmdInitServer(os.Args[2])
		} else {
			cmdInit(os.Args[2])
		}
	case "server":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn server <start|stop|status>")
			os.Exit(1)
		}
		cmdServer(os.Args[2])
	case "clean":
		cmdClean()
	case "repair":
		dir := "."
		if len(os.Args) >= 3 {
			dir = os.Args[2]
		}
		cmdRepair(dir)
	case "ingest":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn ingest <path> [--server]")
			os.Exit(1)
		}
		serverMode := false
		for _, a := range os.Args[3:] {
			if a == "--server" {
				serverMode = true
			}
		}
		cmdIngest(os.Args[2], serverMode)
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
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: defn measure-rename <old-name> <new-name>")
			os.Exit(1)
		}
		cmdMeasureRename(os.Args[2], os.Args[3])
	case "measure-edit":
		// #115 symmetric measurement path for the edit thesis. The
		// body-file argument keeps the CLI shell-safe (no need to
		// escape multi-line Go source through argv).
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: defn measure-edit <name> <body-file>")
			os.Exit(1)
		}
		cmdMeasureEdit(os.Args[2], os.Args[3])
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
	case "branch":
		cmdBranch(os.Args[2:])
	case "checkout":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn checkout <branch>")
			os.Exit(1)
		}
		cmdCheckout(os.Args[2])
	case "merge":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn merge <branch>")
			os.Exit(1)
		}
		cmdMerge(os.Args[2])
	case "commit":
		msg := "snapshot"
		if len(os.Args) >= 3 {
			msg = os.Args[2]
		}
		cmdCommit(msg)
	case "diff":
		cmdDiff()
	case "log":
		cmdLog()
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
	case "worktree":
		cmdWorktree(os.Args[2:])
	case "push":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: defn push <remote> <branch>")
			os.Exit(1)
		}
		cmdPush(os.Args[2], os.Args[3])
	case "pull":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: defn pull <remote> <branch>")
			os.Exit(1)
		}
		cmdPull(os.Args[2], os.Args[3])
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
	fmt.Fprintln(os.Stderr, `defn — AI-native code database (powered by Dolt)

Usage:
  defn init <path>              Ingest + commit + configure MCP
  defn init <path> --server    Same, but starts a Dolt server (recommended)
  defn server start|stop       Manage background Dolt server
  defn clean                   Remove all defn files from project
  defn repair [path]           Delete .defn and re-ingest (recovers from corruption)
  defn ingest <path> [--server]  Parse Go source → Dolt database (--server: use running sql-server)
  defn sync [file]             Re-ingest (single file: fast path via IngestFile+ResolveFile; falls back to full ingest above 50 stale files)
  defn serve                   MCP server for Claude Code
  defn emit <output-dir>       Dolt → .go files
  defn impact <name>           Blast radius + test coverage
  defn untested                Definitions without test coverage
  defn lint                    Lint with diagnostics → definitions
  defn status                  Current branch + stats
  defn check                   Consistency diagnostics (defs by kind, orphan literal types)
  defn branch [name]           List or create branches
  defn checkout <branch>       Switch branch
  defn merge <branch>          Merge a branch (Dolt 3-way merge)
  defn commit <message>        Dolt commit
  defn diff                    Show uncommitted changes
  defn log                     Commit history
  defn query <sql>             Read-only SQL query
  defn gc                      Compact Dolt storage (garbage collection)
  defn restart [--all]         Gracefully bounce this project's serve (or all)
  defn worktree <branch>       Clone DB on a branch (for multi-agent)
  defn push <remote> <branch>  Push branch to remote
  defn pull <remote> <branch>  Pull from remote`)
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
