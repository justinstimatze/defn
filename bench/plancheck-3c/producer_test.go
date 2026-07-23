package plancheck3c

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ExecutionPlan matches the plancheck MCP tool's plan_json schema.
// Kept identical field names so the marshaled JSON is directly usable
// by mcp__plancheck__check_plan.
type ExecutionPlan struct {
	Objective     string   `json:"objective"`
	FilesToRead   []string `json:"filesToRead"`
	FilesToModify []string `json:"filesToModify"`
	FilesToCreate []string `json:"filesToCreate"`
	Steps         []string `json:"steps"`
}

// runProducer is the "task -> plan JSON" step. Two backends:
//
//   - real=false (shakedown): stub that echoes the ground truth back
//     as filesToRead. Recall should be 1.0 for both conditions; any
//     lower means the harness itself has a bug.
//
//   - real=true: spawns a `claude -p` subprocess with per-condition
//     .mcp.json, parses the ExecutionPlan JSON block from the final
//     assistant message. Returns real cost/tokens.
func runProducer(task Task, env map[string]string, real bool) (ExecutionPlan, int, float64, error) {
	if !real {
		return stubProducer(task), 0, 0, nil
	}
	return realProducer(task, env)
}

func stubProducer(task Task) ExecutionPlan {
	return ExecutionPlan{
		Objective:   task.Objective,
		FilesToRead: append([]string{}, task.GroundTruthFiles...),
		Steps:       []string{"stub producer — plan echoes ground truth"},
	}
}

// realProducer spawns a `claude -p` subprocess with a per-condition
// .mcp.json (defn env DEFN_SUMMARY_READ_DEFAULT toggled from env
// map), feeds it the objective, and parses an ExecutionPlan JSON
// block from the final assistant message.
//
// Returns (plan, total_tokens, cost_usd, err). Tokens = input +
// output + cache_read + cache_creation from the terminal "result"
// event. Cost is claude's reported total_cost_usd for the session.
// Timeout hardcoded at 3 min per task.
func realProducer(task Task, env map[string]string) (ExecutionPlan, int, float64, error) {
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		return ExecutionPlan{}, 0, 0, fmt.Errorf("claude cli not on PATH: %w", err)
	}

	workDir, err := os.MkdirTemp("", "plancheck3c-*")
	if err != nil {
		return ExecutionPlan{}, 0, 0, fmt.Errorf("workdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	mcpConfigPath := filepath.Join(workDir, "mcp.json")
	if err := writeMCPConfig(mcpConfigPath, env["DEFN_SUMMARY_READ"] == "1"); err != nil {
		return ExecutionPlan{}, 0, 0, err
	}

	sessionID := randHex(16)
	prompt := buildPrompt(task)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, claudeBin,
		"-p",
		"--session-id", sessionID,
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--mcp-config", mcpConfigPath,
		"--strict-mcp-config",
		"--", prompt,
	)
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	cmd.Dir = repoRoot

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ExecutionPlan{}, 0, 0, err
	}
	cmd.Stderr = os.Stderr

	var reader io.Reader = stdout
	if debugDir := os.Getenv("PLANCHECK_3C_DEBUG_DIR"); debugDir != "" {
		_ = os.MkdirAll(debugDir, 0o755)
		dumpPath := filepath.Join(debugDir,
			fmt.Sprintf("%s-%s.jsonl", task.ID, env["DEFN_SUMMARY_READ"]))
		if f, ferr := os.Create(dumpPath); ferr == nil {
			defer f.Close()
			reader = io.TeeReader(stdout, f)
		}
	}

	if err := cmd.Start(); err != nil {
		return ExecutionPlan{}, 0, 0, fmt.Errorf("start claude: %w", err)
	}

	finalText, tokens, cost, parseErr := parseClaudeStream(reader)
	waitErr := cmd.Wait()
	if parseErr != nil {
		return ExecutionPlan{}, tokens, cost, fmt.Errorf("stream parse: %w (wait: %v)", parseErr, waitErr)
	}
	if waitErr != nil {
		return ExecutionPlan{}, tokens, cost, fmt.Errorf("claude wait: %w", waitErr)
	}

	plan, err := extractPlanJSON(finalText)
	if err != nil {
		return ExecutionPlan{}, tokens, cost, fmt.Errorf("extract plan: %w (final text: %q)", err, truncateStr(finalText, 400))
	}
	if plan.Objective == "" {
		plan.Objective = task.Objective
	}
	return plan, tokens, cost, nil
}

// writeMCPConfig writes a .mcp.json for the claude subprocess.
// summaryDefault toggles defn's DEFN_SUMMARY_READ_DEFAULT env — the
// server-side read of that flips the read op's default mode.
// Only wires the defn MCP; plancheck is invoked via CLI in scoring.
func writeMCPConfig(path string, summaryDefault bool) error {
	defnBin := "/home/justin/go/bin/defn"
	defnDB := "/home/justin/Documents/defn/.defn"
	summary := "0"
	if summaryDefault {
		summary = "1"
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"defn": map[string]any{
				"command": defnBin,
				"args":    []string{"serve"},
				"env": map[string]string{
					"DEFN_DB":                   defnDB,
					"DEFN_SUMMARY_READ_DEFAULT": summary,
				},
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write mcp config: %w", err)
	}
	return nil
}

// buildPrompt renders the task prompt. Format is deliberately terse
// and explicit about output shape so the JSON parse step is reliable.
func buildPrompt(task Task) string {
	return fmt.Sprintf(`You are studying a Go codebase to produce an ExecutionPlan for a proposed change. Do NOT implement anything; produce only the plan.

Task: %s

Use the defn MCP tools (code op:overview / op:search / op:read / op:impact etc.) to understand the code. When you have enough understanding, respond with a single fenced JSON block matching this schema, then stop:

`+"```json"+`
{
  "objective": "<restate the change in one line>",
  "filesToRead": ["path/to/file.go", "..."],
  "filesToModify": ["path/to/file.go", "..."],
  "filesToCreate": [],
  "steps": ["short step 1", "short step 2"]
}
`+"```"+`

Paths must be repo-relative. Do not include any text after the closing fence. Keep the plan concise; only list files you would actually touch.`, task.Objective)
}

// extractPlanJSON pulls the last fenced ```json ... ``` block from
// the assistant text and unmarshals it as an ExecutionPlan. Falls
// back to any raw {...} substring if no fence is present.
func extractPlanJSON(text string) (ExecutionPlan, error) {
	var raw string
	if idx := strings.LastIndex(text, "```json"); idx >= 0 {
		rest := text[idx+len("```json"):]
		if end := strings.Index(rest, "```"); end >= 0 {
			raw = strings.TrimSpace(rest[:end])
		}
	}
	if raw == "" {
		if start := strings.Index(text, "{"); start >= 0 {
			if end := strings.LastIndex(text, "}"); end > start {
				raw = text[start : end+1]
			}
		}
	}
	if raw == "" {
		return ExecutionPlan{}, fmt.Errorf("no JSON block found")
	}
	var plan ExecutionPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return ExecutionPlan{}, fmt.Errorf("unmarshal plan: %w (raw=%q)", err, truncateStr(raw, 200))
	}
	return plan, nil
}

// randHex returns a UUIDv4 string — claude --session-id requires
// canonical 8-4-4-4-12 form, not raw hex.
func randHex(_ int) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// parseClaudeStream reads claude's --output-format stream-json output.
// Each line is one JSON event. Captures the last assistant message's
// text (where the plan JSON block lives) and pulls totals from the
// terminal "result" event — the per-turn assistant.usage records
// carry deltas, not totals, so summing them is wrong.
//
// Returns finalText, totalTokens (sum of all input+output+cache
// components), and costUSD (from result.total_cost_usd).
func parseClaudeStream(r io.Reader) (finalText string, totalTokens int, costUSD float64, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			TotalCostUSD float64 `json:"total_cost_usd"`
		}
		if uerr := json.Unmarshal(line, &ev); uerr != nil {
			continue
		}
		if ev.Type == "assistant" {
			for _, c := range ev.Message.Content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					finalText = c.Text
				}
			}
		}
		if ev.Type == "result" {
			totalTokens = ev.Usage.InputTokens +
				ev.Usage.OutputTokens +
				ev.Usage.CacheReadInputTokens +
				ev.Usage.CacheCreationInputTokens
			costUSD = ev.TotalCostUSD
		}
	}
	if serr := scanner.Err(); serr != nil {
		return finalText, totalTokens, costUSD, fmt.Errorf("scanner: %w", serr)
	}
	return finalText, totalTokens, costUSD, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
