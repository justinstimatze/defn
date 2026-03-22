// Command defn is an AI-native code database for Go source code.
//
// Usage:
//
//	defn ingest <module-path>   Import a Go module into the database
//	defn serve                  Start the MCP server for Claude Code
//	defn emit <output-dir>      Emit .go files from the database
//	defn lint                   Lint definitions (emit, lint, remap)
//	defn query <sql>            Run a raw SQL query against the database
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
	case "lint":
		cmdLint()
	case "query":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: defn query <sql>")
			os.Exit(1)
		}
		cmdQuery(os.Args[2])
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `defn — AI-native code database

Usage:
  defn ingest <module-path>   Import a Go module into the database
  defn serve                  Start the MCP server for Claude Code
  defn emit <output-dir>      Emit .go files from the database
  defn lint                   Lint definitions (emit, lint, remap)
  defn query <sql>            Run a raw SQL query`)
}
