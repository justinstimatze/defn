package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func writeProjectConfig(modulePath, absBin, absDB string, noSummaries bool) {
	writeMCPConfigForProject(modulePath, absBin, absDB, noSummaries)
	writeCodexConfig(modulePath, absBin, absDB)
	writeGitignore(modulePath)
	writeCLAUDEMDSection(modulePath)
	writeClaudeHooks(modulePath)
}

func writeCodexConfig(modulePath, absBin, absDB string) {
	codexDir := filepath.Join(modulePath, ".codex")
	codexPath := filepath.Join(codexDir, "config.toml")
	if _, err := os.Stat(codexPath); !os.IsNotExist(err) {
		return
	}
	os.MkdirAll(codexDir, 0755)
	codexConfig := fmt.Sprintf(`[mcp_servers.defn]
command = %q
args = ["serve"]

[mcp_servers.defn.env]
DEFN_DB = %q
`, absBin, absDB)
	if err := os.WriteFile(codexPath, []byte(codexConfig), 0644); err == nil {
		fmt.Fprintf(os.Stderr, "wrote Codex config to %s\n", codexPath)
	}
}

func writeGitignore(modulePath string) {
	gitignorePath := filepath.Join(modulePath, ".gitignore")
	gitignoreContent, _ := os.ReadFile(gitignorePath)
	if strings.Contains(string(gitignoreContent), ".defn") {
		return
	}
	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(gitignoreContent) > 0 && !strings.HasSuffix(string(gitignoreContent), "\n") {
		f.WriteString("\n")
	}
	// #328: settings.local.json is where writeClaudeHooks installs defn's
	// own hook entry -- Claude Code's own convention treats it as
	// machine-local and gitignored, but that's not guaranteed to already
	// be true in every consuming repo, so defn's own scaffolding
	// shouldn't depend on the project already having the right rule.
	f.WriteString("\n# defn database\n.defn/\n.codex/\n.claude/settings.local.json\n")
}

// writeCLAUDEMDSection ensures the CLAUDE.md at modulePath contains
// the current defn section. Sentinel markers <!-- defn:begin --> and
// <!-- defn:end --> bound the tool-owned block so re-invocation
// replaces the section in place without disturbing user content
// outside it.
func writeCLAUDEMDSection(modulePath string) {
	claudeMDPath := filepath.Join(modulePath, "CLAUDE.md")
	defnSection := defnClaudeMDSection()
	existing, err := os.ReadFile(claudeMDPath)
	if err != nil {
		os.WriteFile(claudeMDPath, []byte(defnSection), 0644)
		fmt.Fprintf(os.Stderr, "wrote %s\n", claudeMDPath)
		return
	}
	content := string(existing)
	const beginMarker = "<!-- defn:begin -->"
	const endMarker = "<!-- defn:end -->"
	bi := strings.Index(content, beginMarker)
	if bi < 0 {
		sep := "\n\n"
		if strings.HasSuffix(content, "\n\n") {
			sep = ""
		} else if strings.HasSuffix(content, "\n") {
			sep = "\n"
		}
		os.WriteFile(claudeMDPath, []byte(content+sep+defnSection), 0644)
		fmt.Fprintf(os.Stderr, "appended defn section to %s\n", claudeMDPath)
		return
	}
	ei := strings.Index(content[bi:], endMarker)
	if ei < 0 {
		fmt.Fprintf(os.Stderr, "warning: found %s but no %s in %s — skipping update\n", beginMarker, endMarker, claudeMDPath)
		return
	}
	after := content[bi+ei+len(endMarker):]
	content = content[:bi] + defnSection + after
	os.WriteFile(claudeMDPath, []byte(content), 0644)
	fmt.Fprintf(os.Stderr, "updated defn section in %s\n", claudeMDPath)
}

// writeMCPConfigForProject writes/updates .mcp.json at modulePath
// to include the defn server pointed at absDB via absBin. Idempotent
// — preserves other MCP servers already declared in the file, and
// preserves any env vars already set on the defn entry itself (so a
// prior --no-summaries opt-out survives a later plain `ingest`
// rewrite instead of being silently clobbered).
//
// noSummaries sets DEFN_LLM_OPS=0 (#201). Sticky: passing false on a
// later call does NOT clear a previously-set DEFN_LLM_OPS -- ingest's
// idempotent config rewrite must never silently re-enable a paid
// feature the user explicitly opted out of.
func writeMCPConfigForProject(modulePath, absBin, absDB string, noSummaries bool) {
	mcpPath := filepath.Join(modulePath, ".mcp.json")
	mcpConfig := map[string]any{}
	if data, err := os.ReadFile(mcpPath); err == nil {
		json.Unmarshal(data, &mcpConfig)
	}
	mcpServers, _ := mcpConfig["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}

	env := map[string]string{}
	if existing, ok := mcpServers["defn"].(map[string]any); ok {
		if existingEnv, ok := existing["env"].(map[string]any); ok {
			for k, v := range existingEnv {
				if s, ok := v.(string); ok {
					env[k] = s
				}
			}
		}
	}
	env["DEFN_DB"] = absDB
	if noSummaries {
		env["DEFN_LLM_OPS"] = "0"
	}

	mcpServers["defn"] = map[string]any{
		"command": absBin,
		"args":    []string{"serve"},
		"env":     env,
	}
	mcpConfig["mcpServers"] = mcpServers
	mcpJSON, _ := json.MarshalIndent(mcpConfig, "", "  ")
	if err := os.WriteFile(mcpPath, mcpJSON, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", mcpPath, err)
		return
	}
	fmt.Fprintf(os.Stderr, "wrote MCP config to %s\n", mcpPath)
}

// defnBinaryPath returns the absolute path to the defn executable
// running this process, resolving symlinks so the path is stable
// across upgrades.
func defnBinaryPath() string {
	absBin, _ := os.Executable()
	if absBin == "" {
		if p, err := exec.LookPath("defn"); err == nil {
			absBin = p
		} else {
			absBin = "defn"
		}
	}
	if resolved, err := filepath.EvalSymlinks(absBin); err == nil {
		absBin = resolved
	}
	return absBin
}

// writeClaudeHooks installs the UserPromptSubmit hook that grounds the
// #203 starter bundle in the real user question instead of a weak
// per-op fallback (a bare search pattern, an op name) -- see
// lastUserQuestion's doc comment in internal/mcp for the full
// mechanism. #241: this was previously dev-only tooling
// (hooks/defn-capture-question.sh, wired only in this repo's own
// .claude/settings.local.json), never installed for consuming
// projects -- every real defn init user got the weaker fallback
// silently. Root-caused via a real grpc-go-2630 head-to-head-go
// trajectory where the starter bundle grounded on the literal search
// term "drop" instead of the actual problem statement, surfaced an
// unrelated same-named "drop" feature elsewhere in the package, and
// contributed to a wrong-function edit.
//
// #328 (external report, winze, reproduced on a throwaway module):
// writes to .claude/settings.local.json, defn's own gitignored
// per-machine config, not .claude/settings.json -- the latter is
// tracked in many consuming repos, and committing an absolute
// filesystem path into it both leaks the local path and registers a
// hook no other checkout can resolve. The hook command itself uses
// ${CLAUDE_PROJECT_DIR} (a Claude Code-documented path placeholder,
// expanded to the project root at hook-run time) instead of the
// absolute hookPath, so the entry is portable across machines and
// checkouts even if it were committed.
//
// Idempotent -- preserves any other hooks already declared in
// settings.local.json (own or third-party), and does not add a second
// UserPromptSubmit entry for defn's hook on repeat init/ingest calls.
// Writes the script to .defn/hooks/ (gitignored, like the rest of
// .defn/) rather than the project's own hooks/ directory, so it never
// collides with a user-authored script of the same name.
func writeClaudeHooks(modulePath string) {
	hookDir := filepath.Join(modulePath, ".defn", "hooks")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create %s: %v\n", hookDir, err)
		return
	}
	hookPath := filepath.Join(hookDir, "defn-capture-question.sh")
	if err := os.WriteFile(hookPath, []byte(captureQuestionHookScript), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", hookPath, err)
		return
	}

	settingsPath := filepath.Join(modulePath, ".claude", "settings.local.json")
	settings := map[string]any{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		json.Unmarshal(data, &settings)
	}
	hooksSection, _ := settings["hooks"].(map[string]any)
	if hooksSection == nil {
		hooksSection = map[string]any{}
	}
	submitGroups, _ := hooksSection["UserPromptSubmit"].([]any)

	command := "bash ${CLAUDE_PROJECT_DIR}/.defn/hooks/defn-capture-question.sh"
	for _, g := range submitGroups {
		group, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := group["hooks"].([]any)
		for _, e := range entries {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if cmd, _ := entry["command"].(string); cmd == command {
				return // already installed
			}
		}
	}

	submitGroups = append(submitGroups, map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": command},
		},
	})
	hooksSection["UserPromptSubmit"] = submitGroups
	settings["hooks"] = hooksSection

	claudeDir := filepath.Join(modulePath, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create %s: %v\n", claudeDir, err)
		return
	}
	settingsJSON, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(settingsPath, settingsJSON, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", settingsPath, err)
		return
	}
	fmt.Fprintf(os.Stderr, "wrote Claude Code hook config to %s\n", settingsPath)
}

//go:embed assets/defn-capture-question.sh
var captureQuestionHookScript string
