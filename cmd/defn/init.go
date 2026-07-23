package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// writeProjectConfig writes/updates project-level configuration
// files that make defn discoverable to AI coding tools running in
// this directory. Idempotent — safe to call on every ingest/init.
// See #168 receipt: chi-explore session bench measured a −18% cost
// win from having CLAUDE.md steer the model toward mcp__defn__code,
// vs zero defn calls when CLAUDE.md was absent.
//
// modulePath must be an absolute path; caller is responsible for
// the chdir into it. absBin is the path to the defn executable
// (used in the emitted .mcp.json / .codex config). absDB is the
// absolute path to .defn/defn.db.
func writeProjectConfig(modulePath, absBin, absDB string) {
	writeMCPConfigForProject(modulePath, absBin, absDB)
	writeCodexConfig(modulePath, absBin, absDB)
	writeGitignore(modulePath)
	writeCLAUDEMDSection(modulePath)
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
	f.WriteString("\n# defn database\n.defn/\n.codex/\n")
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
// — preserves other MCP servers already declared in the file.
func writeMCPConfigForProject(modulePath, absBin, absDB string) {
	mcpPath := filepath.Join(modulePath, ".mcp.json")
	mcpConfig := map[string]any{}
	if data, err := os.ReadFile(mcpPath); err == nil {
		json.Unmarshal(data, &mcpConfig)
	}
	mcpServers, _ := mcpConfig["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}
	mcpServers["defn"] = map[string]any{
		"command": absBin,
		"args":    []string{"serve"},
		"env": map[string]string{
			"DEFN_DB": absDB,
		},
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
