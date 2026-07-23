// Package plancheck3c is the #160 stage 3c A/B bench: does enabling
// semantic-summary reads by default improve plan quality without a
// materially worse recall on filesToRead/Modify/Create?
//
// See README.md for the decision this bench flips and the metric
// definitions.
//
// The bench is a Go test so it can slot into the same `go test`
// discipline as bench/retrieval/, but it does NOT run under a normal
// `go test ./...` invocation — TestPlancheck3c gates on the
// PLANCHECK_3C env var. Shakedown runs use the stub producer (no API
// spend); real runs set PLANCHECK_3C_REAL=1 and require
// ANTHROPIC_API_KEY.
package plancheck3c

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v2"
)

type Task struct {
	ID               string   `yaml:"id"`
	Repo             string   `yaml:"repo"`
	Objective        string   `yaml:"objective"`
	GroundTruthFiles []string `yaml:"ground_truth_files"`
	Difficulty       string   `yaml:"difficulty"`
}

type conditionResult struct {
	TaskID      string
	Condition   string
	Plan        ExecutionPlan
	Findings    plancheckResult
	Recall      float64
	WallSec     float64
	Tokens      int
	CostUSD     float64
	ProducerErr string
}

func TestPlancheck3c(t *testing.T) {
	if os.Getenv("PLANCHECK_3C") == "" {
		t.Skip("set PLANCHECK_3C=1 to run (shakedown or real)")
	}
	real := os.Getenv("PLANCHECK_3C_REAL") == "1"

	tasks := loadTasks(t, "tasks.yaml")
	if offStr := os.Getenv("PLANCHECK_3C_OFFSET"); offStr != "" {
		if n, err := strconv.Atoi(offStr); err == nil && n < len(tasks) {
			tasks = tasks[n:]
		}
	}
	if limitStr := os.Getenv("PLANCHECK_3C_LIMIT"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n < len(tasks) {
			tasks = tasks[:n]
		}
	}
	t.Logf("loaded %d tasks (real=%v)", len(tasks), real)

	conditions := []string{"off", "on"}
	if os.Getenv("PLANCHECK_3C_COND") != "" {
		conditions = strings.Split(os.Getenv("PLANCHECK_3C_COND"), ",")
	}

	var maxCost float64
	if s := os.Getenv("PLANCHECK_3C_MAX_USD"); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			maxCost = v
			t.Logf("cost gate: abort if total spend exceeds $%.2f", maxCost)
		}
	}

	var results []conditionResult
	var spent float64
loop:
	for _, task := range tasks {
		for _, cond := range conditions {
			if maxCost > 0 && spent >= maxCost {
				t.Logf("COST GATE tripped at $%.2f — aborting remaining runs", spent)
				break loop
			}
			r := runOne(t, task, cond, real)
			results = append(results, r)
			spent += r.CostUSD
			t.Logf("[%s / %s] recall=%.2f wall=%.1fs tokens=%d cost=$%.4f spent=$%.2f err=%q",
				task.ID, cond, r.Recall, r.WallSec, r.Tokens, r.CostUSD, spent, r.ProducerErr)
		}
	}

	writeCSV(t, results)
	summarize(t, results)
}

func runOne(t *testing.T, task Task, cond string, real bool) conditionResult {
	t.Helper()
	start := time.Now()

	env := map[string]string{
		"DEFN_SUMMARY_READ": boolEnv(cond == "on"),
	}
	plan, tokens, cost, prodErr := runProducer(task, env, real)

	res := conditionResult{
		TaskID:      task.ID,
		Condition:   cond,
		Plan:        plan,
		Tokens:      tokens,
		CostUSD:     cost,
		WallSec:     time.Since(start).Seconds(),
		ProducerErr: errString(prodErr),
	}
	if prodErr != nil {
		return res
	}

	repoPath := absRepo(t, task.Repo)
	findings, err := checkPlan(plan, repoPath)
	if err != nil {
		res.ProducerErr = "plancheck: " + err.Error()
		return res
	}
	res.Findings = findings
	res.Recall = computeRecall(plan, task.GroundTruthFiles)
	return res
}

func loadTasks(t *testing.T, path string) []Task {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	var f taskFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse tasks: %v", err)
	}
	return f.Tasks
}

func absRepo(t *testing.T, repo string) string {
	t.Helper()
	if filepath.IsAbs(repo) {
		return repo
	}
	abs, err := filepath.Abs(filepath.Join("..", "..", repo))
	if err != nil {
		t.Fatalf("abs repo: %v", err)
	}
	return abs
}

func writeCSV(t *testing.T, results []conditionResult) {
	t.Helper()
	ts := os.Getenv("PLANCHECK_3C_STAMP")
	if ts == "" {
		ts = "shakedown"
	}
	path := fmt.Sprintf("results-%s.csv", ts)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create csv: %v", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"task", "condition", "recall", "wall_sec", "tokens", "cost_usd", "err"})
	for _, r := range results {
		_ = w.Write([]string{
			r.TaskID, r.Condition,
			strconv.FormatFloat(r.Recall, 'f', 3, 64),
			strconv.FormatFloat(r.WallSec, 'f', 2, 64),
			strconv.Itoa(r.Tokens),
			strconv.FormatFloat(r.CostUSD, 'f', 4, 64),
			r.ProducerErr,
		})
	}
	t.Logf("wrote %s", path)
}

func summarize(t *testing.T, results []conditionResult) {
	t.Helper()
	type agg struct {
		n         int
		recallSum float64
		wallSum   float64
		tokenSum  int
		costSum   float64
	}
	byCond := map[string]agg{}
	var totalCost float64
	for _, r := range results {
		s := byCond[r.Condition]
		s.n++
		s.recallSum += r.Recall
		s.wallSum += r.WallSec
		s.tokenSum += r.Tokens
		s.costSum += r.CostUSD
		byCond[r.Condition] = s
		totalCost += r.CostUSD
	}
	for cond, s := range byCond {
		if s.n == 0 {
			continue
		}
		t.Logf("cond=%s n=%d avg_recall=%.3f avg_wall=%.1fs total_tokens=%d cost=$%.2f",
			cond, s.n, s.recallSum/float64(s.n), s.wallSum/float64(s.n), s.tokenSum, s.costSum)
	}
	t.Logf("TOTAL COST: $%.2f", totalCost)
}

func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func errString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

type taskFile struct {
	Tasks []Task `yaml:"tasks"`
}
