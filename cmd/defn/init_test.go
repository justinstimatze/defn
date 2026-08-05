package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteMCPConfigForProject_DefaultNoLLMOpsFlag confirms the
// common case (no --no-summaries) writes no DEFN_LLM_OPS entry at
// all, preserving today's default-enabled behavior.
func TestWriteMCPConfigForProject_DefaultNoLLMOpsFlag(t *testing.T) {
	dir := t.TempDir()
	writeMCPConfigForProject(dir, "/usr/local/bin/defn", filepath.Join(dir, ".defn", "defn.db"), false)

	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal .mcp.json: %v", err)
	}
	env := cfg["mcpServers"].(map[string]any)["defn"].(map[string]any)["env"].(map[string]any)
	if _, ok := env["DEFN_LLM_OPS"]; ok {
		t.Errorf("expected no DEFN_LLM_OPS entry without --no-summaries, got: %+v", env)
	}
	if env["DEFN_DB"] == "" {
		t.Errorf("expected DEFN_DB to still be set, got: %+v", env)
	}
}

// TestWriteMCPConfigForProject_NoSummariesSetsEnvFlag is the #201
// regression: `defn init --no-summaries` should persist
// DEFN_LLM_OPS=0 into .mcp.json so every future spawned `defn serve`
// opts out of LLM-backed ops without the env var being set by hand.
func TestWriteMCPConfigForProject_NoSummariesSetsEnvFlag(t *testing.T) {
	dir := t.TempDir()
	writeMCPConfigForProject(dir, "/usr/local/bin/defn", filepath.Join(dir, ".defn", "defn.db"), true)

	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal .mcp.json: %v", err)
	}
	defnServer := cfg["mcpServers"].(map[string]any)["defn"].(map[string]any)
	env := defnServer["env"].(map[string]any)
	if env["DEFN_LLM_OPS"] != "0" {
		t.Errorf("expected DEFN_LLM_OPS=0 in .mcp.json env, got: %+v", env)
	}
}

// TestWriteMCPConfigForProject_NoSummariesStickyAcrossPlainIngest is
// the other half of #201: a later call with noSummaries=false (the
// path `defn ingest` always takes, since its CLI doesn't parse
// --no-summaries) must NOT clear a previously-set DEFN_LLM_OPS=0 --
// that would silently re-enable a paid feature the user opted out of.
func TestWriteMCPConfigForProject_NoSummariesStickyAcrossPlainIngest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn", "defn.db")
	writeMCPConfigForProject(dir, "/usr/local/bin/defn", dbPath, true)
	writeMCPConfigForProject(dir, "/usr/local/bin/defn", dbPath, false)

	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal .mcp.json: %v", err)
	}
	env := cfg["mcpServers"].(map[string]any)["defn"].(map[string]any)["env"].(map[string]any)
	if env["DEFN_LLM_OPS"] != "0" {
		t.Errorf("expected DEFN_LLM_OPS=0 to survive a later noSummaries=false call, got: %+v", env)
	}
}

// TestMaybeWriteIngestConfig_ReindexSkipsWrites is the regression test
// for --reindex: reindex=true must write nothing, reindex=false must
// write the same config a plain ingest always has.
func TestMaybeWriteIngestConfig_ReindexSkipsWrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn", "defn.db")

	maybeWriteIngestConfig(true, dir, dbPath)
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); !os.IsNotExist(err) {
		t.Errorf("reindex=true: expected no .mcp.json, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("reindex=true: expected no CLAUDE.md, stat err: %v", err)
	}

	maybeWriteIngestConfig(false, dir, dbPath)
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Errorf("reindex=false: expected .mcp.json to be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Errorf("reindex=false: expected CLAUDE.md to be written: %v", err)
	}
}
