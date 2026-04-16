package main

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/justinstimatze/defn/internal/emit"
	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/lint"
	mcpserver "github.com/justinstimatze/defn/internal/mcp"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"

	_ "github.com/go-sql-driver/mysql"
)

func init() {
	// Load config from defn.toml if present (simple key=value, no TOML parser needed).
	// Config file sets defaults; env vars take precedence.
	for _, name := range []string{"defn.toml", ".defn.toml"} {
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
				continue
			}
			if idx := strings.Index(line, "="); idx > 0 {
				key := strings.TrimSpace(line[:idx])
				val := strings.TrimSpace(line[idx+1:])
				// Strip inline comments (# after value).
				if ci := strings.Index(val, " #"); ci >= 0 {
					val = strings.TrimSpace(val[:ci])
				}
				val = strings.Trim(val, "\"'")
				envKey := "DEFN_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
				if os.Getenv(envKey) == "" {
					os.Setenv(envKey, val)
				}
			}
		}
		break
	}
}

func getDBPath() string {
	// Explicit DSN always wins.
	if dsn := os.Getenv("DEFN_DSN"); dsn != "" {
		return dsn
	}
	// Explicit DB path honored as-is (may be filesystem path or DSN).
	if p := os.Getenv("DEFN_DB"); p != "" {
		return p
	}
	// Auto-detect a running dolt sql-server so the CLI falls back to it
	// when the local .defn/ is missing or corrupted, matching the
	// behavior of the db/ library's Open.
	if dsn := detectRunningServer(".defn"); dsn != "" {
		return dsn
	}
	return ".defn"
}

// detectRunningServer returns a DSN for a running dolt sql-server hosting
// this project's database, or "" if none is reachable. Checks
// .defn/server.port for a custom port, falling back to 3307.
//
// Uses a short TCP dial first to avoid driver-level timeouts when
// nothing is listening.
func detectRunningServer(dbPath string) string {
	port := "3307"
	if data, err := os.ReadFile(filepath.Join(dbPath, "server.port")); err == nil {
		port = strings.TrimSpace(string(data))
	}
	addr := "127.0.0.1:" + port
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err != nil {
		return ""
	}
	conn.Close()

	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return ""
	}
	dbName := filepath.Base(absPath)
	for _, user := range []string{"defn", "root"} {
		dsn := fmt.Sprintf("%s@tcp(%s)/%s", user, addr, dbName)
		sqlDB, err := sql.Open("mysql", dsn)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		// Verify this is actually a defn database — a random MySQL server
		// on the same port wouldn't have a definitions table.
		var n int
		err = sqlDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM definitions").Scan(&n)
		cancel()
		sqlDB.Close()
		if err == nil {
			return dsn
		}
	}
	return ""
}

func cmdInitServer(modulePath string) {
	// Check dolt is installed.
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		fatal(fmt.Errorf("dolt not found — install with:\n  curl -L https://github.com/dolthub/dolt/releases/latest/download/install.sh | bash"))
	}

	absModulePath, _ := filepath.Abs(modulePath)
	serverDir := filepath.Join(absModulePath, ".defn-server")
	pidFile := filepath.Join(absModulePath, ".defn-server.pid")
	port := os.Getenv("DEFN_PORT")
	if port == "" {
		port = "3307" // avoid conflict with system MySQL on 3306
	}
	dbName := filepath.Base(absModulePath)

	// Create data directory and dolt init if needed.
	if _, err := os.Stat(serverDir); os.IsNotExist(err) {
		if err := os.MkdirAll(serverDir, 0755); err != nil {
			fatal(err)
		}
		cmd := exec.Command(doltBin, "init")
		cmd.Dir = serverDir
		if out, err := cmd.CombinedOutput(); err != nil {
			fatal(fmt.Errorf("dolt init: %s", out))
		}
	}

	// Start dolt sql-server in background.
	cmd := exec.Command(doltBin, "sql-server", "--host", "127.0.0.1", "--port", port)
	cmd.Dir = serverDir
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		fatal(fmt.Errorf("start dolt server: %w", err))
	}
	pid := cmd.Process.Pid
	fmt.Fprintf(os.Stderr, "starting dolt server (pid %d) on 127.0.0.1:%s...\n", pid, port)

	// Wait for server to be ready (poll, don't sleep).
	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%s)/%s", port, dbName)
	ready := false
	for range 30 {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		cmd.Process.Kill()
		fatal(fmt.Errorf("dolt server failed to start on port %s after 15s", port))
	}

	// Write pidfile only after server is confirmed ready.
	os.WriteFile(pidFile, fmt.Appendf(nil, "%d", pid), 0644)
	fmt.Fprintf(os.Stderr, "dolt server ready\n")

	// Create a defn user with a random password (don't use root/no-password).
	// Generate a cryptographically random password.
	var randBytes [16]byte
	if _, err := cryptoRand.Read(randBytes[:]); err != nil {
		fatal(fmt.Errorf("generate password: %w", err))
	}
	password := fmt.Sprintf("%x", randBytes)
	rootConn, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%s)/", port))
	if err == nil {
		rootConn.Exec(fmt.Sprintf("CREATE USER IF NOT EXISTS 'defn'@'%%' IDENTIFIED BY '%s'", password))
		escapedDB := strings.ReplaceAll(dbName, "`", "``")
		rootConn.Exec(fmt.Sprintf("GRANT ALL ON `%s`.* TO 'defn'@'%%'", escapedDB))
		rootConn.Close()
		dsn = fmt.Sprintf("defn:%s@tcp(127.0.0.1:%s)/%s", password, port, dbName)
		fmt.Fprintf(os.Stderr, "created user 'defn' with generated password\n")
	}

	// Set DEFN_DB so the rest of init uses server mode.
	os.Setenv("DEFN_DB", dsn)

	// Add .defn-server/ and pidfile to gitignore.
	gitignorePath := filepath.Join(absModulePath, ".gitignore")
	gitignoreContent, _ := os.ReadFile(gitignorePath)
	if !strings.Contains(string(gitignoreContent), ".defn-server") {
		f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			if len(gitignoreContent) > 0 && !strings.HasSuffix(string(gitignoreContent), "\n") {
				f.WriteString("\n")
			}
			f.WriteString(".defn-server/\n.defn-server.pid\n")
			f.Close()
		}
	}

	// Run normal init with server-mode DEFN_DB.
	cmdInit(modulePath)
}

func cmdServer(action string) {
	absPath, _ := filepath.Abs(".")
	pidFile := filepath.Join(absPath, ".defn-server.pid")

	switch action {
	case "start":
		doltBin, err := exec.LookPath("dolt")
		if err != nil {
			fatal(fmt.Errorf("dolt not found"))
		}
		serverDir := filepath.Join(absPath, ".defn-server")
		if _, err := os.Stat(serverDir); os.IsNotExist(err) {
			fatal(fmt.Errorf(".defn-server not found — run defn init --server first"))
		}
		port := os.Getenv("DEFN_PORT")
		if port == "" {
			port = "3307"
		}
		cmd := exec.Command(doltBin, "sql-server", "--host", "127.0.0.1", "--port", port)
		cmd.Dir = serverDir
		if err := cmd.Start(); err != nil {
			fatal(fmt.Errorf("start: %w", err))
		}
		os.WriteFile(pidFile, fmt.Appendf(nil, "%d", cmd.Process.Pid), 0644)
		fmt.Fprintf(os.Stderr, "started dolt server (pid %d) on 127.0.0.1:%s\n", cmd.Process.Pid, port)

	case "stop":
		data, err := os.ReadFile(pidFile)
		if err != nil {
			fatal(fmt.Errorf("no pidfile — server not running?"))
		}
		var pid int
		fmt.Sscanf(string(data), "%d", &pid)
		proc, err := os.FindProcess(pid)
		if err != nil {
			fatal(fmt.Errorf("find process %d: %w", pid, err))
		}
		proc.Signal(os.Interrupt)
		os.Remove(pidFile)
		fmt.Fprintf(os.Stderr, "stopped dolt server (pid %d)\n", pid)

	case "status":
		data, err := os.ReadFile(pidFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "not running")
			return
		}
		var pid int
		fmt.Sscanf(string(data), "%d", &pid)
		proc, err := os.FindProcess(pid)
		if err != nil || proc.Signal(os.Signal(nil)) != nil {
			fmt.Fprintln(os.Stderr, "not running (stale pidfile)")
			os.Remove(pidFile)
			return
		}
		fmt.Fprintf(os.Stderr, "running (pid %d)\n", pid)

	default:
		fatal(fmt.Errorf("unknown action %q — use start, stop, or status", action))
	}
}

func cmdClean() {
	absPath, _ := filepath.Abs(".")

	// Stop server if running.
	pidFile := filepath.Join(absPath, ".defn-server.pid")
	if data, err := os.ReadFile(pidFile); err == nil {
		var pid int
		fmt.Sscanf(string(data), "%d", &pid)
		if proc, err := os.FindProcess(pid); err == nil {
			proc.Signal(os.Interrupt)
			fmt.Fprintf(os.Stderr, "stopped dolt server (pid %d)\n", pid)
		}
	}

	// Remove defn artifacts.
	removed := 0
	for _, name := range []string{
		".defn",
		".defn-server",
		".defn-server.pid",
	} {
		path := filepath.Join(absPath, name)
		if _, err := os.Stat(path); err == nil {
			os.RemoveAll(path)
			fmt.Fprintf(os.Stderr, "removed %s\n", name)
			removed++
		}
	}

	// Remove defn entry from .mcp.json (preserve other servers).
	mcpPath := filepath.Join(absPath, ".mcp.json")
	if data, err := os.ReadFile(mcpPath); err == nil {
		var config map[string]any
		if err := json.Unmarshal(data, &config); err != nil {
			fmt.Fprintf(os.Stderr, "warning: .mcp.json is malformed, removing: %v\n", err)
			os.Remove(mcpPath)
			removed++
		} else if config != nil {
			if servers, ok := config["mcpServers"].(map[string]any); ok {
				if _, exists := servers["defn"]; exists {
					delete(servers, "defn")
					removed++
					if len(servers) == 0 {
						os.Remove(mcpPath)
						fmt.Fprintf(os.Stderr, "removed .mcp.json (no servers left)\n")
					} else {
						updated, _ := json.MarshalIndent(config, "", "  ")
						os.WriteFile(mcpPath, updated, 0644)
						fmt.Fprintf(os.Stderr, "removed defn from .mcp.json (other servers preserved)\n")
					}
				}
			}
		}
	}

	// Remove .codex/config.toml defn entry (or whole dir if only defn).
	codexPath := filepath.Join(absPath, ".codex", "config.toml")
	if _, err := os.Stat(codexPath); err == nil {
		os.RemoveAll(filepath.Join(absPath, ".codex"))
		fmt.Fprintf(os.Stderr, "removed .codex/\n")
		removed++
	}

	if removed == 0 {
		fmt.Fprintln(os.Stderr, "nothing to clean")
	} else {
		fmt.Fprintln(os.Stderr, "defn cleaned. CLAUDE.md was left in place (may contain your edits).")
	}
}

func cmdInit(modulePath string) {
	dbPath := getDBPath()
	db, err := store.Open(dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	fmt.Fprintf(os.Stderr, "ingesting %s...\n", modulePath)
	pkgs, err := goload.LoadAll(modulePath)
	if err != nil {
		fatal(err)
	}
	if err := ingest.IngestPackages(db, pkgs, modulePath); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "resolving references...\n")
	if err := resolve.ResolvePackages(db, pkgs, modulePath); err != nil {
		fatal(err)
	}

	hash, err := db.ComputeRootHash()
	if err != nil {
		fatal(err)
	}

	if err := db.Commit("initial ingest"); err != nil {
		fatal(err)
	}

	mods, _ := db.ListModules()
	defs, _ := db.FindDefinitions("%")

	fmt.Fprintf(os.Stderr, "done. %d modules, %d definitions, root hash: %s\n",
		len(mods), len(defs), hash[:16])

	// Get absolute paths for the MCP config.
	absDB, _ := filepath.Abs(dbPath)
	absModulePath, _ := filepath.Abs(modulePath)
	absBin, _ := os.Executable()
	if absBin == "" {
		if p, err := exec.LookPath("defn"); err == nil {
			absBin = p
		} else {
			absBin = "defn" // fallback
		}
	}
	// Resolve symlinks so the path is stable.
	if resolved, err := filepath.EvalSymlinks(absBin); err == nil {
		absBin = resolved
	}

	// Write .mcp.json at the project root (Claude Code's project-level MCP config).
	mcpPath := filepath.Join(absModulePath, ".mcp.json")

	// Read existing config if present, or start fresh.
	mcpConfig := map[string]any{}
	if data, err := os.ReadFile(mcpPath); err == nil {
		json.Unmarshal(data, &mcpConfig)
	}

	// Set/update the defn MCP server entry.
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
		fmt.Fprintf(os.Stderr, "manually create .mcp.json:\n\n")
		fmt.Fprintln(os.Stderr, string(mcpJSON))
	} else {
		fmt.Fprintf(os.Stderr, "wrote MCP config to %s\n", mcpPath)
	}

	// Write .codex/config.toml for OpenAI Codex.
	codexDir := filepath.Join(absModulePath, ".codex")
	codexPath := filepath.Join(codexDir, "config.toml")
	if _, err := os.Stat(codexPath); os.IsNotExist(err) {
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

	// Write or update the defn section in CLAUDE.md.
	claudeMDPath := filepath.Join(absModulePath, "CLAUDE.md")
	defnSection := defnClaudeMDSection()

	// Add .defn/ to .gitignore if not already there.
	gitignorePath := filepath.Join(absModulePath, ".gitignore")
	gitignoreContent, _ := os.ReadFile(gitignorePath)
	if !strings.Contains(string(gitignoreContent), ".defn") {
		f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			if len(gitignoreContent) > 0 && !strings.HasSuffix(string(gitignoreContent), "\n") {
				f.WriteString("\n")
			}
			f.WriteString("\n# defn database\n.defn/\n.codex/\n")
			f.Close()
		}
	}

	// Write or update the defn section in CLAUDE.md.
	// Sentinel markers allow updating the section on re-init without
	// disturbing user-written content.
	if existing, err := os.ReadFile(claudeMDPath); err == nil {
		content := string(existing)
		const beginMarker = "<!-- defn:begin -->"
		const endMarker = "<!-- defn:end -->"
		if bi := strings.Index(content, beginMarker); bi >= 0 {
			if ei := strings.Index(content[bi:], endMarker); ei >= 0 {
				// Replace existing defn section.
				after := content[bi+ei+len(endMarker):]
				content = content[:bi] + defnSection + after
				os.WriteFile(claudeMDPath, []byte(content), 0644)
				fmt.Fprintf(os.Stderr, "updated defn section in %s\n", claudeMDPath)
			} else {
				fmt.Fprintf(os.Stderr, "warning: found <!-- defn:begin --> but no <!-- defn:end --> in %s — skipping update\n", claudeMDPath)
			}
		} else {
			// CLAUDE.md exists but has no defn section — append.
			sep := "\n\n"
			if strings.HasSuffix(content, "\n\n") {
				sep = ""
			} else if strings.HasSuffix(content, "\n") {
				sep = "\n"
			}
			os.WriteFile(claudeMDPath, []byte(content+sep+defnSection), 0644)
			fmt.Fprintf(os.Stderr, "appended defn section to %s\n", claudeMDPath)
		}
	} else {
		os.WriteFile(claudeMDPath, []byte(defnSection), 0644)
		fmt.Fprintf(os.Stderr, "wrote %s\n", claudeMDPath)
	}

	fmt.Fprintln(os.Stderr, "start a new AI coding session in this directory to use defn.")
}

func defnClaudeMDSection() string {
	return `<!-- defn:begin -->
## Code Navigation and Editing

This project is indexed in defn. Use the ` + "`code`" + ` MCP tool for **Go code**:

` + "```" + `
code(op: "read", name: "handleEdit")           -- full source by name
code(op: "read", name: "server.go:272")        -- or by file:line
code(op: "impact", name: "Render")             -- blast radius + test coverage
code(op: "edit", name: "Foo", new_body: "...") -- edit, auto-emit + build
code(op: "search", pattern: "%Auth%")          -- name pattern (% wildcard)
code(op: "search", pattern: "authentication")  -- body text search
code(op: "test", name: "Render")               -- run affected tests only
code(op: "sync")                               -- re-ingest after file edits
` + "```" + `

All ops: read, search, impact, explain, untested, edit, create, delete, rename, move, test, apply, diff, history, find, sync, query, overview, patch.

**Both editing paths work.** ` + "`code(op:\"edit\")`" + ` updates the database, emits files, and rebuilds references automatically. File tools (Read, Edit) work too — call ` + "`code(op:\"sync\")`" + ` after editing Go files.

Prefer defn for Go code (fewer steps, auto-build verification). Use Read/Edit/Grep for non-Go files.

**Rule of thumb:** Always run impact before modifying an existing definition. Skip it for brand-new definitions.
<!-- defn:end -->
`
}

func cmdIngest(modulePath string, serverMode bool) {
	dbPath := getDBPath()
	if serverMode {
		dsn := detectRunningServer(".defn")
		if dsn == "" {
			fatal(fmt.Errorf("--server: no dolt sql-server reachable on 127.0.0.1:3307 — start one with 'defn server start' or 'defn init <path> --server'"))
		}
		dbPath = dsn
		fmt.Fprintf(os.Stderr, "using server: %s\n", dsn)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	fmt.Fprintf(os.Stderr, "ingesting %s...\n", modulePath)
	pkgs, err := goload.LoadAll(modulePath)
	if err != nil {
		fatal(err)
	}
	if err := ingest.IngestPackages(db, pkgs, modulePath); err != nil {
		fatal(err)
	}

	fmt.Fprintf(os.Stderr, "resolving references...\n")
	if err := resolve.ResolvePackages(db, pkgs, modulePath); err != nil {
		fatal(err)
	}

	hash, err := db.ComputeRootHash()
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "done. root hash: %s\n", hash[:16])
}

// cmdRepair deletes the embedded .defn/ database and re-ingests from
// source. Useful when the Dolt journal or indexes get corrupted — since
// .defn/ is a derived artifact, it can always be rebuilt from .go files.
//
// Preserves .mcp.json, CLAUDE.md, .codex/, and .defn-server/ (server
// mode has its own repair path — drop the database and reingest via
// 'defn ingest --server').
func cmdRepair(modulePath string) {
	// Refuse to repair a server-backed DB — Dolt server has its own
	// tools for that; our job here is just the embedded cache.
	if dsn := os.Getenv("DEFN_DSN"); dsn != "" {
		fatal(fmt.Errorf("repair is for embedded .defn/ only — unset DEFN_DSN to repair embedded, or use dolt tooling to repair the server"))
	}
	if p := os.Getenv("DEFN_DB"); p != "" && strings.Contains(p, "@") {
		fatal(fmt.Errorf("repair is for embedded .defn/ only — DEFN_DB points to a DSN (%s)", p))
	}

	absModulePath, err := filepath.Abs(modulePath)
	if err != nil {
		fatal(err)
	}
	defnDir := filepath.Join(absModulePath, ".defn")

	if _, err := os.Stat(defnDir); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "no .defn/ found — running a fresh ingest")
	} else {
		fmt.Fprintf(os.Stderr, "removing %s...\n", defnDir)
		if err := os.RemoveAll(defnDir); err != nil {
			fatal(fmt.Errorf("remove .defn: %w", err))
		}
	}

	// Always open the embedded path directly so auto-detection doesn't
	// redirect us to a running server — repair rebuilds the embedded copy.
	db, err := store.Open(filepath.Join(absModulePath, ".defn"))
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	fmt.Fprintf(os.Stderr, "ingesting %s...\n", absModulePath)
	pkgs, err := goload.LoadAll(absModulePath)
	if err != nil {
		fatal(err)
	}
	if err := ingest.IngestPackages(db, pkgs, absModulePath); err != nil {
		fatal(err)
	}

	fmt.Fprintf(os.Stderr, "resolving references...\n")
	if err := resolve.ResolvePackages(db, pkgs, absModulePath); err != nil {
		fatal(err)
	}

	if err := db.Commit("repair (reingest)"); err != nil {
		fatal(err)
	}

	mods, _ := db.ListModules()
	defs, _ := db.FindDefinitions("%")
	hash, _ := db.ComputeRootHash()
	fmt.Fprintf(os.Stderr, "done. %d modules, %d definitions, root hash: %s\n",
		len(mods), len(defs), hash[:16])
}

func cmdServe(httpAddr string) {
	// Cap Go heap so Dolt's embedded caches scale down (v1.86.2+ reads
	// GOMEMLIMIT via its memlimit package). 1 GiB is plenty for the MCP
	// server; without this the noms chunk store + prolly node cache +
	// memtable balloon to ~544 MB at defaults.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(1 << 30) // 1 GiB
	}

	ctx := context.Background()

	// Explicit --http mode: just start the HTTP server.
	if httpAddr != "" {
		db, err := store.Open(getDBPath())
		if err != nil {
			fatal(err)
		}
		defer db.Close()
		projDir := serveProjectDir()
		if err := mcpserver.RunHTTP(ctx, db, projDir, httpAddr); err != nil {
			fatal(err)
		}
		return
	}

	// Auto-sharing: derive a port from the database path. If another
	// defn serve is already listening on that port, proxy to it (~5 MB).
	// Otherwise, start both HTTP and stdio (first session pays full cost,
	// subsequent sessions are lightweight proxies).
	port := portForDB(getDBPath())
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	sseURL := fmt.Sprintf("http://%s/sse", addr)

	if isDefnListening(addr) {
		fmt.Fprintf(os.Stderr, "defn: proxying to shared server on %s\n", addr)
		if err := mcpserver.RunProxy(ctx, sseURL); err != nil {
			fatal(err)
		}
		return
	}

	// First session: start shared HTTP + serve this client over stdio.
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	projDir := serveProjectDir()
	if err := mcpserver.RunShared(ctx, db, projDir, addr); err != nil {
		fatal(err)
	}
}

// serveProjectDir returns the project root from the DEFN_DB path.
func serveProjectDir() string {
	projDir := filepath.Dir(getDBPath())
	if projDir == "." {
		projDir, _ = os.Getwd()
	}
	return projDir
}

// portForDB derives a deterministic port from the database path.
// Range: 9420-9999 (580 ports, collision unlikely for typical dev machines).
func portForDB(dbPath string) int {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		abs = dbPath
	}
	// FNV-style hash.
	var h uint32
	for _, b := range []byte(abs) {
		h ^= uint32(b)
		h *= 16777619
	}
	return 9420 + int(h%580)
}

// isDefnListening checks if a defn HTTP/SSE server is already on addr.
// Verifies content-type to avoid proxying to an unrelated service.
func isDefnListening(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s/sse", addr), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.Header.Get("Content-Type") == "text/event-stream"
}

func cmdEmit(outDir string) {
	db, err := store.Open(getDBPath())
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

func cmdGC() {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	fmt.Fprint(os.Stderr, "Running garbage collection...")
	if err := db.GC(); err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stderr, " done.")
}

func cmdQuery(sql string) {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	warnIfStale(db, ".")

	results, err := db.Query(sql)
	if err != nil {
		fatal(err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(results)
}

// warnIfStale prints a warning to stderr if any .go file under projectDir
// is newer than the last recorded ingest timestamp. Silent if the
// database has no last_ingest recorded (older DBs) or nothing is stale.
func warnIfStale(db *store.DB, projectDir string) {
	lastIngestStr, err := db.GetMeta("last_ingest")
	if err != nil || lastIngestStr == "" {
		return
	}
	lastIngest, err := strconv.ParseInt(lastIngestStr, 10, 64)
	if err != nil {
		return
	}
	var count int
	var sample string
	_ = filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".defn" || name == ".git" || name == "vendor" ||
				name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Unix() > lastIngest {
			count++
			if sample == "" {
				sample = path
			}
		}
		return nil
	})
	if count == 0 {
		return
	}
	if count == 1 {
		fmt.Fprintf(os.Stderr, "defn: 1 file modified since last ingest (%s) — results may be stale (run 'defn ingest .')\n", sample)
	} else {
		fmt.Fprintf(os.Stderr, "defn: %d files modified since last ingest — results may be stale (run 'defn ingest .')\n", count)
	}
}

func cmdWorktree(branchName string) {
	srcPath := getDBPath()

	// Server mode: branches are shared — no worktree copy needed.
	if strings.Contains(srcPath, "@") {
		fmt.Fprintln(os.Stderr, "server mode: use 'defn branch' and 'defn checkout' directly.")
		fmt.Fprintln(os.Stderr, "Each agent session has its own branch via CALL DOLT_CHECKOUT.")
		fmt.Fprintf(os.Stderr, "\n  defn branch %s && defn checkout %s\n", branchName, branchName)
		return
	}

	// Embedded mode: copy directory and set up branch.
	dstPath := srcPath + "-" + branchName

	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		fatal(fmt.Errorf("database %s not found — run defn init first", srcPath))
	}
	if _, err := os.Stat(dstPath); err == nil {
		fatal(fmt.Errorf("worktree %s already exists", dstPath))
	}
	cmd := exec.Command("cp", "-r", srcPath, dstPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		fatal(fmt.Errorf("copy database: %s", out))
	}

	// Open the copy and create the branch.
	db, err := store.Open(dstPath)
	if err != nil {
		fatal(err)
	}

	if err := db.Branch(branchName); err != nil {
		fatal(fmt.Errorf("create branch: %w", err))
	}
	db.Close()

	// Set the default branch in Dolt's repo state so future connections
	// open on the right branch (embedded Dolt starts on the default branch).
	repoStatePath := filepath.Join(dstPath, ".dolt", "repo_state.json")
	repoState, err := os.ReadFile(repoStatePath)
	if err != nil {
		fatal(fmt.Errorf("read repo state: %w", err))
	}
	var state map[string]any
	if err := json.Unmarshal(repoState, &state); err != nil {
		fatal(fmt.Errorf("parse repo state: %w", err))
	}
	state["head"] = "refs/heads/" + branchName
	updated, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		fatal(fmt.Errorf("marshal repo state: %w", err))
	}
	if err := os.WriteFile(repoStatePath, updated, 0644); err != nil {
		fatal(fmt.Errorf("write repo state: %w", err))
	}

	fmt.Fprintf(os.Stderr, "created worktree %s on branch %s\n", dstPath, branchName)
	fmt.Fprintf(os.Stderr, "push back: DEFN_DB=%s defn push origin %s\n", dstPath, branchName)
	fmt.Fprintf(os.Stderr, "serve:     DEFN_DB=%s defn serve\n", dstPath)
}

func cmdPush(remote, branch string) {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if err := db.Push(remote, branch); err != nil {
		fatal(fmt.Errorf("push: %w", err))
	}
	fmt.Fprintf(os.Stderr, "pushed %s to %s\n", branch, remote)
}

func cmdPull(remote, branch string) {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if err := db.Pull(remote, branch); err != nil {
		fatal(fmt.Errorf("pull: %w", err))
	}
	fmt.Fprintf(os.Stderr, "pulled %s from %s\n", branch, remote)
}

func cmdCommit(message string) {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if err := db.Commit(message); err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stderr, "committed.")
}

func cmdDiff() {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	status, err := db.Diff()
	if err != nil {
		fatal(err)
	}
	if len(status) == 0 {
		fmt.Fprintln(os.Stderr, "no changes.")
		return
	}
	for _, s := range status {
		fmt.Printf("  %s  %s\n", s["status"], s["table"])
	}
}

func cmdStatus() {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	branch, _ := db.GetCurrentBranch()
	mods, _ := db.ListModules()
	defs, _ := db.FindDefinitions("%")
	fmt.Printf("On branch %s\n", branch)
	fmt.Printf("Database: %s\n", getDBPath())
	fmt.Printf("%d modules, %d definitions\n", len(mods), len(defs))

	// Check freshness: compare newest .go file against DB modtime.
	dbPath := getDBPath()
	dbStat, err := os.Stat(filepath.Join(dbPath, ".dolt", "noms"))
	if err != nil {
		return
	}
	dbTime := dbStat.ModTime()

	var newestFile string
	var newestTime time.Time
	filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			base := filepath.Base(path)
			if base == ".defn" || base == ".defn-server" || base == ".git" || base == "vendor" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newestFile = path
		}
		return nil
	})

	if newestTime.IsZero() {
		return
	}
	if newestTime.After(dbTime) {
		fmt.Fprintf(os.Stderr, "\nDatabase may be stale: %s is newer than DB\n", newestFile)
		fmt.Fprintf(os.Stderr, "  run: defn ingest .\n")
	} else {
		fmt.Fprintf(os.Stderr, "\nDatabase is up to date\n")
	}
}

func cmdBranch(args []string) {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if len(args) == 0 {
		current, _ := db.GetCurrentBranch()
		branches, err := db.ListBranches()
		if err != nil {
			fatal(err)
		}
		for _, name := range branches {
			marker := "  "
			if name == current {
				marker = "* "
			}
			fmt.Printf("%s%s\n", marker, name)
		}
		return
	}

	// Create a branch.
	if err := db.Branch(args[0]); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "created branch %s\n", args[0])
}

func cmdCheckout(branchName string) {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if err := db.Checkout(branchName); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "switched to branch %s\n", branchName)
}

func cmdMerge(branchName string) {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if err := db.Merge(branchName); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "merged %s\n", branchName)
}

func cmdLog() {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	entries, err := db.Log(20)
	if err != nil {
		fatal(err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "no commits.")
		return
	}
	for _, e := range entries {
		hash := fmt.Sprint(e["hash"])
		if len(hash) > 12 {
			hash = hash[:12]
		}
		fmt.Printf("%s  %s  %s\n", hash, e["date"], e["message"])
	}
}

func cmdImpact(name string, jsonOutput bool) {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	// Find the definition.
	d, err := db.GetDefinitionByName(name, "")
	if err != nil {
		fatal(fmt.Errorf("definition %q not found", name))
	}

	impact, err := db.GetImpact(d.ID)
	if err != nil {
		fatal(err)
	}

	if jsonOutput {
		type defRef struct {
			Name       string `json:"name"`
			Kind       string `json:"kind"`
			Receiver   string `json:"receiver,omitempty"`
			SourceFile string `json:"source_file"`
			StartLine  int    `json:"start_line,omitempty"`
			Test       bool   `json:"test,omitempty"`
		}
		toRef := func(d store.Definition) defRef {
			return defRef{Name: d.Name, Kind: d.Kind, Receiver: d.Receiver, SourceFile: d.SourceFile, StartLine: d.StartLine, Test: d.Test}
		}

		blastRadius := "low"
		if impact.TransitiveCount > 20 {
			blastRadius = "high"
		} else if impact.TransitiveCount > 5 {
			blastRadius = "medium"
		}

		callers := make([]defRef, 0, len(impact.DirectCallers))
		for _, c := range impact.DirectCallers {
			callers = append(callers, toRef(c))
		}
		ifaceDispatch := make([]defRef, 0, len(impact.InterfaceDispatchCallers))
		for _, c := range impact.InterfaceDispatchCallers {
			ifaceDispatch = append(ifaceDispatch, toRef(c))
		}
		tests := make([]defRef, 0, len(impact.Tests))
		for _, t := range impact.Tests {
			tests = append(tests, toRef(t))
		}

		result := map[string]any{
			"definition":                 toRef(impact.Definition),
			"module":                     impact.Module,
			"direct_callers":             callers,
			"interface_dispatch_callers": ifaceDispatch,
			"transitive_count":           impact.TransitiveCount,
			"tests":                      tests,
			"uncovered_by":               impact.UncoveredBy,
			"blast_radius":               blastRadius,
		}
		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			fatal(err)
		}
		fmt.Println(string(b))
		return
	}

	// Module.pkg.Name (kind)
	parts := strings.Split(impact.Module, "/")
	pkg := parts[len(parts)-1]
	receiver := ""
	if impact.Definition.Receiver != "" {
		receiver = "(" + impact.Definition.Receiver + ") "
	}

	fmt.Printf("%s.%s%s (%s)\n", pkg, receiver, impact.Definition.Name, impact.Definition.Kind)
	fmt.Printf("  module: %s\n", impact.Module)
	fmt.Println()

	// Direct callers.
	fmt.Printf("  direct callers: %d\n", len(impact.DirectCallers))
	for _, c := range impact.DirectCallers {
		marker := "  "
		if c.Test {
			marker = "T "
		}
		recv := ""
		if c.Receiver != "" {
			recv = "(" + c.Receiver + ")."
		}
		fmt.Printf("    %s%s%s\n", marker, recv, c.Name)
	}
	fmt.Println()

	// Transitive impact.
	fmt.Printf("  transitive callers: %d\n", impact.TransitiveCount)
	fmt.Println()

	// Test coverage.
	fmt.Printf("  tests covering this: %d\n", len(impact.Tests))
	for _, t := range impact.Tests {
		fmt.Printf("    %s\n", t.Name)
	}
	if len(impact.Tests) == 0 {
		fmt.Println("    (none — this definition has no test coverage)")
	}
	fmt.Println()

	// Uncovered callers.
	if impact.UncoveredBy > 0 {
		fmt.Printf("  direct callers without test coverage: %d\n", impact.UncoveredBy)
	}
}

func cmdUntested() {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	defs, err := db.GetUntested()
	if err != nil {
		fatal(err)
	}

	if len(defs) == 0 {
		fmt.Fprintln(os.Stderr, "all exported definitions have test coverage.")
		return
	}

	fmt.Fprintf(os.Stderr, "%d exported definitions without direct test coverage:\n\n", len(defs))
	for _, d := range defs {
		recv := ""
		if d.Receiver != "" {
			recv = "(" + d.Receiver + ")."
		}
		fmt.Printf("  %s%s (%s)\n", recv, d.Name, d.Kind)
	}
}

func cmdWatch(modulePath string) {
	fmt.Fprintln(os.Stderr, "watching for changes... (Ctrl+C to stop)")
	absPath, _ := filepath.Abs(modulePath)

	// Simple poll-based watcher: check go.mod mtime every 2 seconds.
	// A proper fsnotify watcher would be better but adds a dependency.
	var lastMod int64
	for {
		info, err := os.Stat(filepath.Join(absPath, "go.mod"))
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		mod := info.ModTime().UnixNano()

		// Also check if any .go file is newer than the database.
		var newestGo int64
		filepath.Walk(absPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				if strings.Contains(path, ".defn") || strings.Contains(path, "vendor") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && info.ModTime().UnixNano() > newestGo {
				newestGo = info.ModTime().UnixNano()
			}
			return nil
		})

		if newestGo > lastMod && lastMod > 0 {
			fmt.Fprintf(os.Stderr, "change detected, re-ingesting...\n")
			db, err := store.Open(getDBPath())
			if err != nil {
				fmt.Fprintf(os.Stderr, "  error: %v\n", err)
			} else {
				pkgs, loadErr := goload.LoadAll(absPath)
				if loadErr != nil {
					fmt.Fprintf(os.Stderr, "  load error: %v\n", loadErr)
				} else if err := ingest.IngestPackages(db, pkgs, absPath); err != nil {
					fmt.Fprintf(os.Stderr, "  ingest error: %v\n", err)
				} else if err := resolve.ResolvePackages(db, pkgs, absPath); err != nil {
					fmt.Fprintf(os.Stderr, "  resolve error: %v\n", err)
				} else {
					db.Commit("auto-ingest")
					fmt.Fprintf(os.Stderr, "  done.\n")
				}
				db.Close()
			}
		}
		lastMod = newestGo
		_ = mod
		time.Sleep(2 * time.Second)
	}
}

func cmdLint() {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	diags, err := lint.Check(db)
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

func printPrefixed(body, prefix string) {
	for line := range strings.SplitSeq(body, "\n") {
		fmt.Printf("%s%s\n", prefix, line)
	}
}

// printUnifiedDiff shells out to diff(1) for proper unified output
// matching git's format. Falls back to simple display if diff unavailable.
func printUnifiedDiff(oldBody, newBody string) {
	dir, err := os.MkdirTemp("", "defn-diff-*")
	if err != nil {
		printSimpleDiff(oldBody, newBody)
		return
	}
	defer os.RemoveAll(dir)

	oldFile := filepath.Join(dir, "old")
	newFile := filepath.Join(dir, "new")
	os.WriteFile(oldFile, []byte(oldBody+"\n"), 0644)
	os.WriteFile(newFile, []byte(newBody+"\n"), 0644)

	cmd := exec.Command("diff", "-u", "--label=old", "--label=new", oldFile, newFile)
	out, _ := cmd.Output()
	// diff exits 1 when files differ — that's expected.
	if len(out) == 0 {
		fmt.Println("    (no text difference)")
		return
	}

	// Skip the first two header lines (--- old / +++ new) and print
	// the hunks indented.
	lines := strings.SplitSeq(string(out), "\n")
	for line := range lines {
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			continue
		}
		fmt.Printf("    %s\n", line)
	}
}

func printSimpleDiff(oldBody, newBody string) {
	fmt.Println("    --- old")
	for line := range strings.SplitSeq(oldBody, "\n") {
		fmt.Printf("    -%s\n", line)
	}
	fmt.Println("    +++ new")
	for line := range strings.SplitSeq(newBody, "\n") {
		fmt.Printf("    +%s\n", line)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
