package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/emit"
	"github.com/justinstimatze/defn/internal/lint"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

const defaultDB = "defn.db"

func cmdIngest(modulePath string) {
	db, err := store.Open(defaultDB)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	fmt.Fprintf(os.Stderr, "ingesting %s...\n", modulePath)
	if err := ingest.Ingest(db, modulePath); err != nil {
		fatal(err)
	}

	fmt.Fprintf(os.Stderr, "resolving references...\n")
	if err := resolve.Resolve(db, modulePath); err != nil {
		fatal(err)
	}

	hash, err := db.ComputeRootHash()
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "done. root hash: %s\n", hash[:16])
}

func cmdServe() {
	// TODO: wire up mcp-go stdio transport
	fmt.Fprintln(os.Stderr, "MCP server not yet wired — see internal/mcp for tool definitions")
	os.Exit(1)
}

func cmdEmit(outDir string) {
	db, err := store.Open(defaultDB)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	fmt.Fprintf(os.Stderr, "emitting to %s...\n", outDir)
	if err := emit.Emit(db, outDir); err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stderr, "done.")
}

func cmdQuery(sql string) {
	db, err := store.Open(defaultDB)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	results, err := db.Query(sql)
	if err != nil {
		fatal(err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(results)
}

func cmdLint() {
	db, err := store.Open(defaultDB)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	diags, err := lint.Run(db)
	if err != nil {
		fatal(err)
	}

	if len(diags) == 0 {
		fmt.Fprintln(os.Stderr, "no issues found.")
		return
	}

	for _, d := range diags {
		fmt.Println(d.String())
	}
	os.Exit(1)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
