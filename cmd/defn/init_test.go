package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestWriteClaudeHooks_WritesScriptAndSettings guards the #241 fix:
// defn init previously never installed the UserPromptSubmit hook that
// grounds the #203 starter bundle in the real question -- every
// consuming project silently got the weaker per-op-arg fallback.
func TestWriteClaudeHooks_WritesScriptAndSettings(t *testing.T) {
	dir := t.TempDir()
	writeClaudeHooks(dir)

	hookPath := filepath.Join(dir, ".defn", "hooks", "defn-capture-question.sh")
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook script: %v", err)
	}
	if !strings.Contains(string(data), "UserPromptSubmit") {
		t.Errorf("expected the installed script to be the capture-question hook, got: %s", data)
	}

	if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.json")); err == nil {
		t.Errorf("expected settings.json (commonly tracked in consuming repos) to be left untouched -- #328")
	}

	settingsData, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read settings.local.json: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(settingsData, &settings); err != nil {
		t.Fatalf("unmarshal settings.local.json: %v", err)
	}
	groups := settings["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(groups) != 1 {
		t.Fatalf("expected exactly 1 UserPromptSubmit group, got %d: %+v", len(groups), groups)
	}
	entries := groups[0].(map[string]any)["hooks"].([]any)
	cmd := entries[0].(map[string]any)["command"].(string)
	if strings.Contains(cmd, dir) {
		t.Errorf("expected command to be portable (${CLAUDE_PROJECT_DIR}-relative), not the absolute tempdir path, got %q", cmd)
	}
	if !strings.Contains(cmd, "${CLAUDE_PROJECT_DIR}/.defn/hooks/defn-capture-question.sh") {
		t.Errorf("expected command to reference the portable ${CLAUDE_PROJECT_DIR}-relative hook path, got %q", cmd)
	}
}

// TestWriteClaudeHooks_IdempotentNoDuplicateEntry confirms repeat
// calls (defn init followed by defn ingest, or ingest run twice)
// don't pile up duplicate UserPromptSubmit hook entries.
func TestWriteClaudeHooks_IdempotentNoDuplicateEntry(t *testing.T) {
	dir := t.TempDir()
	writeClaudeHooks(dir)
	writeClaudeHooks(dir)
	writeClaudeHooks(dir)

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read settings.local.json: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("unmarshal settings.local.json: %v", err)
	}
	groups := settings["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(groups) != 1 {
		t.Errorf("expected 1 UserPromptSubmit group after 3 calls, got %d: %+v", len(groups), groups)
	}
}

// TestWriteClaudeHooks_PreservesExistingHooksAndSettings confirms an
// existing .claude/settings.local.json (the user's own hooks, or
// another tool's) survives untouched -- writeClaudeHooks must merge,
// not overwrite. #328: writes to settings.local.json (gitignored),
// not settings.json (commonly tracked), so this seeds the file defn
// actually merges into.
func TestWriteClaudeHooks_PreservesExistingHooksAndSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0755); err != nil {
		t.Fatal(err)
	}
	existing := `{
		"permissions": {"allow": ["Bash(go test *)"]},
		"hooks": {
			"UserPromptSubmit": [
				{"hooks": [{"type": "command", "command": "echo other-tool-hook"}]}
			],
			"PreToolUse": [
				{"matcher": "Write|Edit", "hooks": [{"type": "command", "command": "echo guard"}]}
			]
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	writeClaudeHooks(dir)

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read settings.local.json: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("unmarshal settings.local.json: %v", err)
	}

	perms := settings["permissions"].(map[string]any)
	if perms == nil {
		t.Error("expected existing permissions block to survive")
	}

	preTool := settings["hooks"].(map[string]any)["PreToolUse"]
	if preTool == nil {
		t.Error("expected existing PreToolUse hooks to survive untouched")
	}

	groups := settings["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(groups) != 2 {
		t.Fatalf("expected the other tool's hook PLUS defn's own (2 groups), got %d: %+v", len(groups), groups)
	}
	foundOther := false
	foundDefn := false
	for _, g := range groups {
		entries := g.(map[string]any)["hooks"].([]any)
		for _, e := range entries {
			cmd := e.(map[string]any)["command"].(string)
			if cmd == "echo other-tool-hook" {
				foundOther = true
			}
			if strings.Contains(cmd, "defn-capture-question.sh") {
				foundDefn = true
			}
		}
	}
	if !foundOther {
		t.Error("expected the other tool's UserPromptSubmit hook to survive")
	}
	if !foundDefn {
		t.Error("expected defn's own UserPromptSubmit hook to be added")
	}
}

// TestWriteGitignore_IgnoresClaudeSettingsLocal guards #328: defn's own
// scaffolding must not depend on the consuming repo already ignoring
// .claude/settings.local.json (the file writeClaudeHooks writes its
// hook entry into) -- an uncommitted .gitignore entry would leave that
// file eligible to be accidentally committed.
func TestWriteGitignore_IgnoresClaudeSettingsLocal(t *testing.T) {
	dir := t.TempDir()
	writeGitignore(dir)

	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(data), ".claude/settings.local.json") {
		t.Errorf("expected .gitignore to ignore .claude/settings.local.json, got:\n%s", data)
	}
}
