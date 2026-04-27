package main

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
		logBackend("using DEFN_DSN=" + sanitizeDSN(dsn))
		return dsn
	}
	// Explicit DB path honored as-is (may be filesystem path or DSN).
	if p := os.Getenv("DEFN_DB"); p != "" {
		logBackend("using DEFN_DB=" + sanitizeDSN(p))
		return p
	}
	// Worktree marker: this dir is a worktree on a specific branch of
	// a shared defn server. Marker sets DSN + branch pin.
	if dsn, branch := readWorktreeMarker("."); dsn != "" {
		if branch != "" && os.Getenv("DEFN_BRANCH") == "" {
			os.Setenv("DEFN_BRANCH", branch)
		}
		logBackend(fmt.Sprintf("using worktree dsn=%s branch=%s", sanitizeDSN(dsn), branch))
		return dsn
	}
	// Auto-detect a running dolt sql-server so the CLI falls back to it
	// when the local .defn/ is missing or corrupted, matching the
	// behavior of the db/ library's Open.
	if dsn := detectRunningServer(".defn"); dsn != "" {
		logBackend("using dolt server " + dsnHostDisplay(dsn))
		return dsn
	}
	logBackend("using embedded .defn/")
	return ".defn"
}

// readWorktreeMarker reads .defn-worktree.json in dir if present, returning
// the (dsn, branch) it pins to.
func readWorktreeMarker(dir string) (dsn, branch string) {
	data, err := os.ReadFile(filepath.Join(dir, ".defn-worktree.json"))
	if err != nil {
		return "", ""
	}
	var m struct {
		DSN    string `json:"dsn"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", ""
	}
	return m.DSN, m.Branch
}

// logBackend prints a one-line backend selection notice to stderr so
// users can tell whether they're hitting a running server or the
// embedded store (different performance characteristics). Silenced by
// DEFN_QUIET=1.
func logBackend(msg string) {
	if os.Getenv("DEFN_QUIET") != "" {
		return
	}
	fmt.Fprintln(os.Stderr, "defn: "+msg)
}

// sanitizeDSN strips passwords from a MySQL DSN before logging.
func sanitizeDSN(dsn string) string {
	i := strings.Index(dsn, "@")
	if i < 0 {
		return dsn
	}
	if c := strings.Index(dsn[:i], ":"); c >= 0 {
		return dsn[:c+1] + "***" + dsn[i:]
	}
	return dsn
}

// dsnHostDisplay pulls the addr out of a DSN for a terse log line.
func dsnHostDisplay(dsn string) string {
	if i := strings.Index(dsn, "tcp("); i >= 0 {
		rest := dsn[i+4:]
		if j := strings.Index(rest, ")"); j >= 0 {
			return rest[:j]
		}
	}
	return sanitizeDSN(dsn)
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
		if isCorruptDBError(err) && !strings.Contains(dbPath, "@") {
			fatal(fmt.Errorf("%w\n\n.defn/ appears to be corrupted. run 'defn repair %s' to rebuild from source",
				err, modulePath))
		}
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

	// Compact the noms store. A fresh ingest writes ~200 MB of journal
	// chunks that GC folds into a ~1.5 MB packed file. Costs <1s; skip
	// for DSN-backed DBs since the server manages its own GC.
	compactEmbedded(db, dbPath)

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
	checkEmbeddedAvailable(dbPath)
	db, err := store.Open(dbPath)
	if err != nil {
		if isCorruptDBError(err) && !strings.Contains(dbPath, "@") {
			fatal(fmt.Errorf("%w\n\n.defn/ appears to be corrupted. run 'defn repair %s' to rebuild from source",
				err, modulePath))
		}
		fatal(err)
	}
	defer db.Close()

	announceStaleIngest(db, modulePath)
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

	if err := db.Commit("reingest"); err != nil {
		fatal(err)
	}
	compactEmbedded(db, dbPath)

	fmt.Fprintf(os.Stderr, "done. root hash: %s\n", hash[:16])
}

// isCorruptDBError reports whether err looks like a corrupt embedded
// Dolt database (manifest/journal damaged). These are the cases where
// `defn repair` is the right escape hatch — the error text is the only
// signal Dolt gives us.
func isCorruptDBError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, pat := range []string{
		"corrupt manifest",
		"journal index is malformed",
		"bad index checksum",
		"malformed journal",
		"failed to load database",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}

// compactEmbedded runs DOLT_GC on embedded (filesystem-path) databases.
// Skips DSN-backed databases where the running sql-server owns GC.
// After a fresh ingest this typically reclaims 99% of the journal
// chunks (e.g. 193 MB → 1.5 MB on defn itself).
//
// Respects DEFN_SKIP_GC=1 for ingest flows that want to skip the
// post-ingest compact (e.g. scripted workflows that run many ingests
// in sequence and GC once at the end).
func compactEmbedded(db *store.DB, dbPath string) {
	if strings.Contains(dbPath, "@") {
		return
	}
	if os.Getenv("DEFN_SKIP_GC") != "" {
		return
	}
	if err := db.GC(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: compact failed: %v\n", err)
	}
}

// cmdSync re-ingests source into the database after on-disk edits.
// With no argument, runs a full re-ingest + reference rebuild (same as
// 'defn ingest .'). With a single file argument, uses the fast single-
// file path (~10 ms) that skips packages.Load and only re-parses that
// file — matches the MCP 'sync' op with a file parameter.
//
// The single-file path updates definitions/bodies/signatures but does
// not rebuild the reference graph. Use full sync after structural
// changes that affect cross-file calls.
func cmdSync(file string) {
	if file == "" {
		cmdIngest(".", false)
		return
	}
	dbPath := getDBPath()
	checkEmbeddedAvailable(dbPath)
	db, err := store.Open(dbPath)
	if err != nil {
		if isCorruptDBError(err) && !strings.Contains(dbPath, "@") {
			fatal(fmt.Errorf("%w\n\n.defn/ appears to be corrupted. run 'defn repair .' to rebuild from source", err))
		}
		fatal(err)
	}
	defer db.Close()

	absFile, err := filepath.Abs(file)
	if err != nil {
		fatal(err)
	}
	modulePath, err := findModuleRoot(absFile)
	if err != nil {
		fatal(err)
	}

	n, err := ingest.IngestFile(db, modulePath, absFile)
	if err != nil {
		fatal(err)
	}
	// Re-resolve refs for the affected package. Without this, embed and
	// implements refs drift away from source as files are edited.
	if err := resolve.ResolveFile(db, modulePath, absFile); err != nil {
		fatal(fmt.Errorf("resolve file: %w", err))
	}
	if err := db.Commit("sync " + file); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "synced %s: %d definitions updated\n", file, n)
}

// findModuleRoot walks up from filePath looking for go.mod.
func findModuleRoot(filePath string) (string, error) {
	dir := filepath.Dir(filePath)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", filePath)
		}
		dir = parent
	}
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

	checkEmbeddedAvailable(defnDir)
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

	mods, _ := db.ListModules()
	defs, _ := db.FindDefinitions("%")
	hash, err := db.ComputeRootHash()
	if err != nil {
		fatal(fmt.Errorf("compute root hash: %w", err))
	}

	if err := db.Commit("repair (reingest)"); err != nil {
		fatal(err)
	}
	compactEmbedded(db, filepath.Join(absModulePath, ".defn"))

	fmt.Fprintf(os.Stderr, "done. %d modules, %d definitions, root hash: %s\n",
		len(mods), len(defs), hash[:16])
}

// serveLock records a running defn serve process holding the embedded
// .defn/ lock. Other CLI commands read this before opening embedded so
// they can surface an actionable error instead of a bare "database is
// read only" from Dolt.
//
// Liveness is enforced via syscall.Flock on the lockfile, not a PID
// alive-check — the kernel releases the flock when the serve process
// dies, so stale lockfiles from crashes become automatically reapable
// and PID recycling can't produce false positives.
type serveLock struct {
	PID      int    `json:"pid"`
	HTTPAddr string `json:"http_addr,omitempty"`
	Started  int64  `json:"started"` // unix seconds
}

// writeServeLock records this process as the owner of the embedded DB
// at dbPath, holding an exclusive flock on the lockfile for the
// lifetime of the process. Returns a cleanup func that releases the
// flock (by closing the fd) and removes the file. Idempotent via
// sync.Once; safe to call from both a defer and a signal handler.
//
// If another defn serve already holds the flock, the process aborts
// via fatal — two serves on the same embedded DB would corrupt noms.
func writeServeLock(dbPath, httpAddr string) func() {
	if strings.Contains(dbPath, "@") {
		return func() {} // DSN-backed: no embedded lock to advertise
	}
	lockPath := filepath.Join(dbPath, "serve.pid")
	// Open without O_TRUNC: a racing second serve must not empty the
	// winning serve's content on its way to failing the flock. Truncate
	// is delayed until after we've proven we're the flock holder.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "defn: could not open serve lockfile %s: %v\n", lockPath, err)
		return func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		fatal(fmt.Errorf("another defn serve is already holding %s (flock: %w)", lockPath, err))
	}
	// Safe to overwrite stale content now that we own the flock.
	if err := f.Truncate(0); err != nil {
		fmt.Fprintf(os.Stderr, "defn: truncate serve lockfile: %v\n", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		fmt.Fprintf(os.Stderr, "defn: seek serve lockfile: %v\n", err)
	}
	data, _ := json.Marshal(serveLock{
		PID:      os.Getpid(),
		HTTPAddr: httpAddr,
		Started:  time.Now().Unix(),
	})
	if _, err := f.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "defn: could not write serve lockfile: %v\n", err)
	}

	var once sync.Once
	stopCh := make(chan struct{})
	cleanup := func() {
		once.Do(func() {
			close(stopCh)
			f.Close() // releases the flock
			os.Remove(lockPath)
		})
	}

	// Signal handler: SIGINT/SIGTERM should also release the lock before
	// the process exits. On normal return, cleanup runs from defer and
	// closes stopCh so this goroutine exits instead of parking forever.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigs:
			f.Close()
			os.Remove(lockPath)
			os.Exit(130)
		case <-stopCh:
			signal.Stop(sigs)
		}
	}()
	return cleanup
}

// readServeLock returns the active serve lockfile for dbPath, or nil
// if none exists, the file is malformed, or the recorded serve has
// died (flock no longer held).
//
// When the flock is releasable, the lockfile is reaped as a side
// effect so the next caller sees a clean slate. Doing this under the
// shared-lock check window means concurrent readers agree on the
// verdict without a dedicated coordinator.
func readServeLock(dbPath string) *serveLock {
	lockPath := filepath.Join(dbPath, "serve.pid")
	f, err := os.Open(lockPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	// LOCK_SH|LOCK_NB: succeeds iff no one holds LOCK_EX. If the serve
	// is alive, it holds LOCK_EX and we'll see EWOULDBLOCK.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		os.Remove(lockPath)
		return nil
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return nil
	}
	var lock serveLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil
	}
	return &lock
}

// embeddedLockStatus returns the error message a CLI should print and
// whether to abort. Extracted from checkEmbeddedAvailable so the
// decision is testable without os.Exit side effects.
func embeddedLockStatus(dbPath string) (msg string, held bool) {
	if strings.Contains(dbPath, "@") {
		return "", false
	}
	lock := readServeLock(dbPath)
	if lock == nil {
		return "", false
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "defn: %s is held by a running defn serve (pid %d)\n", dbPath, lock.PID)
	if lock.HTTPAddr != "" {
		fmt.Fprintf(&sb, "  MCP endpoint: http://%s/sse\n", lock.HTTPAddr)
	}
	sb.WriteString("  to run this command concurrently, either:\n")
	sb.WriteString("    - stop the server and retry\n")
	sb.WriteString("    - run against an isolated worktree: defn worktree <branch> <path>\n")
	sb.WriteString("    - set DEFN_DSN to a running dolt sql-server\n")
	return sb.String(), true
}

// fetchServerVersion GETs http://<addr>/version on the running MCP
// serve. Returns "" on any error so callers can silently skip when the
// running server predates the /version endpoint. 2s timeout — long
// enough that a serve under load (e.g. mid-ingest) still responds,
// short enough that a wedged or dead socket doesn't stall `defn
// status` for a human's attention window.
func fetchServerVersion(addr string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s/version", addr), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// checkEmbeddedAvailable aborts with an actionable message if a defn
// serve process is holding the embedded DB at dbPath. Called by CLI
// commands that want to open embedded before they pay for a Dolt
// manifest error.
func checkEmbeddedAvailable(dbPath string) {
	if msg, held := embeddedLockStatus(dbPath); held {
		fmt.Fprint(os.Stderr, msg)
		os.Exit(1)
	}
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
		dbPath := getDBPath()
		db, err := store.Open(dbPath)
		if err != nil {
			fatal(err)
		}
		defer db.Close()
		defer writeServeLock(dbPath, httpAddr)()
		projDir := serveProjectDir()
		if err := mcpserver.RunHTTP(ctx, db, projDir, httpAddr); err != nil {
			fatal(err)
		}
		return
	}

	// Auto-sharing: derive a port from the database path. If another
	// defn serve is already listening on that port FOR THIS PROJECT,
	// proxy to it (~5 MB). Otherwise, start both HTTP and stdio (first
	// session pays full cost, subsequent sessions are lightweight
	// proxies).
	//
	// FNV hashing has a small collision probability (1/580 per pair).
	// Without identity verification, a collision would silently route
	// reads/writes to the wrong project's database. We probe the
	// hash port and a linear sequence after it, checking each one's
	// /identity endpoint until we find our own project's serve or
	// a free slot.
	dbPath := getDBPath()
	projDir := serveProjectDir()
	wantIdentity, _ := filepath.Abs(projDir)
	primary := portForDB(dbPath)

	const maxProbes = 16
	for offset := 0; offset < maxProbes; offset++ {
		port := primary + offset
		if port > 9999 {
			port = 9420 + (port-9420)%580
		}
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		sseURL := fmt.Sprintf("http://%s/sse", addr)

		if !isDefnListening(addr) {
			// Free — claim it as the shared backend for this project.
			db, err := store.Open(dbPath)
			if err != nil {
				fatal(err)
			}
			defer db.Close()
			defer writeServeLock(dbPath, addr)()
			if offset > 0 {
				fmt.Fprintf(os.Stderr, "defn: hash collision on %d, using %d\n",
					primary, port)
			}
			if err := mcpserver.RunShared(ctx, db, projDir, addr); err != nil {
				fatal(err)
			}
			return
		}

		// Something's there — is it our project?
		gotIdentity := defnIdentityAt(addr)
		if gotIdentity == wantIdentity {
			fmt.Fprintf(os.Stderr, "defn: proxying to shared server on %s\n", addr)
			if err := mcpserver.RunProxy(ctx, sseURL); err != nil {
				fatal(err)
			}
			return
		}
		// Different project on this port (FNV collision or a serve
		// started without /identity support). Try the next port.
		fmt.Fprintf(os.Stderr, "defn: %s holds port %d (need %s) — probing next\n",
			gotIdentity, port, wantIdentity)
	}
	fatal(fmt.Errorf("auto-sharing: no free port in %d-port window starting at %d", maxProbes, primary))
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

// defnIdentityAt fetches /identity from a running defn HTTP server and
// returns the absolute project directory it's pinned to. Returns "" if
// the endpoint is unreachable or absent (older defn versions without
// /identity — treated as "unknown identity, don't trust").
func defnIdentityAt(addr string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s/identity", addr), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func cmdEmit(outDir string) {
	dbPath := getDBPath()
	checkEmbeddedAvailable(dbPath)
	db, err := store.Open(dbPath)
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

// countStaleFiles reports how many .go files under projectDir have
// been modified since the last ingest. Returns (0, "") when the DB
// has no last_ingest meta (older DBs) or nothing is stale. sample is
// the first stale path encountered, for user-facing messages.
//
// Walks projectDir skipping .defn/, .git/, vendor/, node_modules/,
// and testdata/. Shared backend for warnIfStale and announceStaleIngest.
func countStaleFiles(db *store.DB, projectDir string) (count int, sample string) {
	lastIngestStr, err := db.GetMeta("last_ingest")
	if err != nil || lastIngestStr == "" {
		return 0, ""
	}
	lastIngest, err := strconv.ParseInt(lastIngestStr, 10, 64)
	if err != nil {
		return 0, ""
	}
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
	return count, sample
}

// warnIfStale prints a stderr notice when the working tree has .go
// files newer than the last ingest — signaling that read ops (query,
// status) may return stale results. Silent when up to date or when
// the DB predates last_ingest.
func warnIfStale(db *store.DB, projectDir string) {
	count, sample := countStaleFiles(db, projectDir)
	if count == 0 {
		return
	}
	if count == 1 {
		fmt.Fprintf(os.Stderr, "defn: 1 file modified since last ingest (%s) — results may be stale (run 'defn ingest .')\n", sample)
	} else {
		fmt.Fprintf(os.Stderr, "defn: %d files modified since last ingest — results may be stale (run 'defn ingest .')\n", count)
	}
}

// announceStaleIngest prints a one-line notice of pending stale files
// on entry to ingest/sync so the user sees staleness was detected and
// is being resolved by the current operation — closing the loop that
// `warnIfStale` opens on read paths.
func announceStaleIngest(db *store.DB, projectDir string) {
	count, sample := countStaleFiles(db, projectDir)
	if count == 0 {
		return
	}
	if count == 1 {
		fmt.Fprintf(os.Stderr, "defn: 1 stale file detected (%s), ingesting...\n", sample)
	} else {
		fmt.Fprintf(os.Stderr, "defn: %d stale files detected, ingesting...\n", count)
	}
}

func cmdWorktree(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: defn worktree <branch> [<path>]")
		fmt.Fprintln(os.Stderr, "  creates a worktree (new file tree) pinned to <branch> on the defn server.")
		fmt.Fprintln(os.Stderr, "  default path is ../<cwd-basename>-<branch>")
		os.Exit(1)
	}
	branchName := args[0]

	srcPath := getDBPath()
	isServer := strings.Contains(srcPath, "@")
	if !isServer {
		fatal(fmt.Errorf("worktree requires server mode — run 'defn init . --server' first"))
	}

	cwd, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	var wtPath string
	if len(args) >= 2 {
		wtPath = args[1]
		if !filepath.IsAbs(wtPath) {
			wtPath = filepath.Join(cwd, wtPath)
		}
	} else {
		wtPath = filepath.Join(filepath.Dir(cwd), filepath.Base(cwd)+"-"+branchName)
	}
	if _, err := os.Stat(wtPath); err == nil {
		fatal(fmt.Errorf("worktree path %s already exists", wtPath))
	}

	// Create branch on server if it doesn't already exist.
	db, err := store.Open(srcPath)
	if err != nil {
		fatal(err)
	}
	branches, _ := db.ListBranches()
	exists := false
	for _, b := range branches {
		if b == branchName {
			exists = true
			break
		}
	}
	if !exists {
		if err := db.Branch(branchName); err != nil {
			db.Close()
			fatal(fmt.Errorf("create branch: %w", err))
		}
		fmt.Fprintf(os.Stderr, "created branch %s\n", branchName)
	}
	db.Close()

	// Re-open with the branch pinned so emit sees the branch's state.
	_ = os.Setenv("DEFN_BRANCH", branchName)
	db2, err := store.Open(srcPath)
	if err != nil {
		fatal(fmt.Errorf("reopen pinned to %s: %w", branchName, err))
	}
	defer db2.Close()

	if err := os.MkdirAll(wtPath, 0755); err != nil {
		fatal(fmt.Errorf("create worktree dir: %w", err))
	}
	fmt.Fprintf(os.Stderr, "emitting branch %s to %s...\n", branchName, wtPath)
	if err := emit.Emit(db2, wtPath); err != nil {
		fatal(err)
	}

	marker := struct {
		DSN    string `json:"dsn"`
		Branch string `json:"branch"`
	}{DSN: srcPath, Branch: branchName}
	data, _ := json.MarshalIndent(marker, "", "  ")
	if err := os.WriteFile(filepath.Join(wtPath, ".defn-worktree.json"), data, 0644); err != nil {
		fatal(fmt.Errorf("write worktree marker: %w", err))
	}

	fmt.Fprintf(os.Stderr, "worktree ready at %s (branch %s)\n", wtPath, branchName)
	fmt.Fprintf(os.Stderr, "  cd %s && defn serve\n", wtPath)
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

// cmdCheck runs consistency diagnostics against the ingested database.
// Surfaces counts by kind (so you can tell at a glance if an entire
// category like 'var' wasn't ingested) and orphaned literal-field
// type_names (literals referencing types not in the definitions table,
// which usually means the containing var/type was filtered out or not
// loaded).
func cmdCheck() {
	db, err := store.Open(getDBPath())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	fmt.Println("Definitions by kind:")
	kindRows, err := db.Query(`
		SELECT kind, COUNT(*) AS n
		FROM definitions
		GROUP BY kind
		ORDER BY n DESC`)
	if err != nil {
		fatal(err)
	}
	total := 0
	for _, r := range kindRows {
		fmt.Printf("  %-12s %v\n", r["kind"], r["n"])
		if n, ok := r["n"].(int64); ok {
			total += int(n)
		}
	}
	fmt.Printf("  %-12s %d\n", "TOTAL", total)

	fmt.Println("\nOrphan literal-field type_names (top 20):")
	orphans, err := db.Query(`
		SELECT lf.type_name, COUNT(*) AS n
		FROM literal_fields lf
		LEFT JOIN definitions d
		  ON d.name = SUBSTRING_INDEX(lf.type_name, '.', -1)
		WHERE d.id IS NULL
		GROUP BY lf.type_name
		ORDER BY n DESC
		LIMIT 20`)
	if err != nil {
		fatal(err)
	}
	if len(orphans) == 0 {
		fmt.Println("  (none — all literal type_names resolve to a definition)")
	} else {
		for _, r := range orphans {
			fmt.Printf("  %-60s %v\n", r["type_name"], r["n"])
		}
		fmt.Fprintln(os.Stderr, "\nnote: orphan type_names are usually external types (ok) or a sign that some definitions weren't ingested. run 'defn check --orphans-internal' to filter to in-module types only.")
	}
}

// statusReport is the structured form of `defn status`. JSON output
// marshals this directly; human output renders it via formatters
// below. Kept tight so scripts (polecat, slimemold) can depend on a
// stable shape.
type statusReport struct {
	RunningServe *runningServeInfo `json:"running_serve,omitempty"`
	VersionSkew  *versionSkewInfo  `json:"version_skew,omitempty"`
	Database     *databaseInfo     `json:"database,omitempty"`
	// Freshness is nil when we couldn't check (e.g. the embedded DB is
	// locked by the running serve). Scripts should treat a missing
	// field as "unknown", not "up to date".
	Freshness *freshnessInfo `json:"freshness,omitempty"`
}

type runningServeInfo struct {
	PID           int    `json:"pid"`
	HTTPAddr      string `json:"http_addr,omitempty"`
	StartedUnix   int64  `json:"started_unix,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds,omitempty"`
	Version       string `json:"version,omitempty"`
}

type versionSkewInfo struct {
	Running string `json:"running"`
	OnDisk  string `json:"on_disk"`
}

type databaseInfo struct {
	Path        string `json:"path"`
	Branch      string `json:"branch"`
	Modules     int    `json:"modules"`
	Definitions int    `json:"definitions"`
}

type freshnessInfo struct {
	UpToDate   bool   `json:"up_to_date"`
	StaleCount int    `json:"stale_count"`
	Sample     string `json:"sample,omitempty"`
}

func cmdStatus(jsonOut bool) {
	r := collectStatus(getDBPath())
	if jsonOut {
		out, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			fatal(err)
		}
		fmt.Println(string(out))
		return
	}
	printStatus(r)
}

// collectStatus gathers the full status report. Doesn't print; the
// caller renders in either human or JSON form.
func collectStatus(dbPath string) statusReport {
	var r statusReport

	if lock := readServeLock(dbPath); lock != nil {
		info := &runningServeInfo{
			PID:         lock.PID,
			HTTPAddr:    lock.HTTPAddr,
			StartedUnix: lock.Started,
		}
		if lock.Started > 0 {
			info.UptimeSeconds = int64(time.Since(time.Unix(lock.Started, 0)).Seconds())
		}
		if lock.HTTPAddr != "" {
			if v := fetchServerVersion(lock.HTTPAddr); v != "" {
				info.Version = v
				if v != mcpserver.Version {
					r.VersionSkew = &versionSkewInfo{Running: v, OnDisk: mcpserver.Version}
				}
			}
		}
		r.RunningServe = info
	}

	// Skip DB stats when the flock is held on an embedded path — we
	// can't open it without racing Dolt's own manifest lock.
	embeddedAndLocked := r.RunningServe != nil && !strings.Contains(dbPath, "@")
	if embeddedAndLocked {
		return r
	}

	db, err := store.Open(dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	branch, _ := db.GetCurrentBranch()
	mods, _ := db.ListModules()
	defs, _ := db.FindDefinitions("%")
	r.Database = &databaseInfo{
		Path: dbPath, Branch: branch,
		Modules: len(mods), Definitions: len(defs),
	}

	count, sample := countStaleFiles(db, ".")
	r.Freshness = &freshnessInfo{
		UpToDate:   count == 0,
		StaleCount: count,
		Sample:     sample,
	}
	return r
}

func printStatus(r statusReport) {
	if s := r.RunningServe; s != nil {
		fmt.Println("Running serve:")
		fmt.Printf("  pid:        %d\n", s.PID)
		if s.HTTPAddr != "" {
			fmt.Printf("  mcp:        http://%s/sse\n", s.HTTPAddr)
		}
		if s.StartedUnix > 0 {
			uptime := time.Duration(s.UptimeSeconds) * time.Second
			fmt.Printf("  started:    %s ago (%s)\n", uptime,
				time.Unix(s.StartedUnix, 0).Format(time.RFC3339))
		}
		if s.Version != "" {
			if r.VersionSkew != nil {
				fmt.Printf("  version:    %s (on-disk defn: %s)\n", s.Version, r.VersionSkew.OnDisk)
			} else {
				fmt.Printf("  version:    %s\n", s.Version)
			}
		}
		fmt.Println()
		if r.VersionSkew != nil {
			fmt.Fprintf(os.Stderr,
				"Version skew: running serve is %s but $(which defn) is %s.\n"+
					"  restart 'defn serve' to pick up the new binary.\n\n",
				r.VersionSkew.Running, r.VersionSkew.OnDisk)
		}
	}

	if r.Database == nil {
		fmt.Fprintln(os.Stderr, "Skipping DB stats (embedded DB is held by the running serve).")
		return
	}

	fmt.Printf("On branch %s\n", r.Database.Branch)
	fmt.Printf("Database: %s\n", r.Database.Path)
	fmt.Printf("%d modules, %d definitions\n", r.Database.Modules, r.Database.Definitions)
	fmt.Fprintln(os.Stderr)

	if r.Freshness == nil {
		return
	}
	switch {
	case r.Freshness.StaleCount == 0:
		fmt.Fprintln(os.Stderr, "Database is up to date")
	case r.Freshness.StaleCount == 1:
		fmt.Fprintf(os.Stderr, "Database may be stale: %s modified since last ingest\n", r.Freshness.Sample)
		fmt.Fprintln(os.Stderr, "  run: defn ingest .")
	default:
		fmt.Fprintf(os.Stderr, "Database may be stale: %d files modified since last ingest (e.g. %s)\n",
			r.Freshness.StaleCount, r.Freshness.Sample)
		fmt.Fprintln(os.Stderr, "  run: defn ingest .")
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
