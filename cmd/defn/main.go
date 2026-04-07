// Command defn is an AI-native code database for Go source code.
// Stores definitions in Dolt (SQL database with git semantics).
package main

import (
	"fmt"
	"os"
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
	case "ingest":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn ingest <module-path>")
			os.Exit(1)
		}
		cmdIngest(os.Args[2])
	case "serve":
		cmdServe()
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
		cmdStatus()
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
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn worktree <branch-name>")
			os.Exit(1)
		}
		cmdWorktree(os.Args[2])
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
  defn ingest <module-path>    Parse Go source → Dolt database
  defn serve                   MCP server for Claude Code
  defn emit <output-dir>       Dolt → .go files
  defn impact <name>           Blast radius + test coverage
  defn untested                Definitions without test coverage
  defn lint                    Lint with diagnostics → definitions
  defn status                  Current branch + stats
  defn branch [name]           List or create branches
  defn checkout <branch>       Switch branch
  defn merge <branch>          Merge a branch (Dolt 3-way merge)
  defn commit <message>        Dolt commit
  defn diff                    Show uncommitted changes
  defn log                     Commit history
  defn query <sql>             Read-only SQL query
  defn worktree <branch>       Clone DB on a branch (for multi-agent)
  defn push <remote> <branch>  Push branch to remote
  defn pull <remote> <branch>  Pull from remote`)
}
