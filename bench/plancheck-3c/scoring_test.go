package plancheck3c

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// plancheckResult is the compact JSON that mcp__plancheck__check_plan
// returns. We only pull out the fields the bench actually aggregates
// on — leaving room for the tool to grow without breaking the harness.
type plancheckResult struct {
	Raw       json.RawMessage
	HistoryID string `json:"historyId"`
	// Findings map keys are category names (e.g. "missingFiles",
	// "comodGaps"). Values are counts.
	Findings map[string]int `json:"findings"`
}

func checkPlan(plan ExecutionPlan, cwd string) (plancheckResult, error) {
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return plancheckResult{}, fmt.Errorf("marshal plan: %w", err)
	}

	bin, err := exec.LookPath("plancheck")
	if err != nil {
		return plancheckResult{}, fmt.Errorf("plancheck cli not on PATH — install or wire MCP fallback: %w", err)
	}
	planFile, err := writeTempPlan(planJSON)
	if err != nil {
		return plancheckResult{}, err
	}
	defer removeFile(planFile)
	cmd := exec.Command(bin, "check", planFile, "--cwd", cwd, "--json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return plancheckResult{}, fmt.Errorf("plancheck: %w\n%s", err, out)
	}

	var res plancheckResult
	res.Raw = out
	if err := json.Unmarshal(out, &res); err != nil {
		return plancheckResult{}, fmt.Errorf("parse plancheck output: %w\n%s", err, out)
	}
	return res, nil
}

// computeRecall = |ground_truth ∩ plan_files| / |ground_truth|.
// plan_files = filesToRead ∪ filesToModify ∪ filesToCreate.
// Paths are normalized to forward slashes and made relative to
// the task's repo root before comparison.
func computeRecall(plan ExecutionPlan, groundTruth []string) float64 {
	if len(groundTruth) == 0 {
		return 1.0
	}
	planSet := map[string]bool{}
	for _, f := range plan.FilesToRead {
		planSet[normPath(f)] = true
	}
	for _, f := range plan.FilesToModify {
		planSet[normPath(f)] = true
	}
	for _, f := range plan.FilesToCreate {
		planSet[normPath(f)] = true
	}
	hit := 0
	for _, gt := range groundTruth {
		if planSet[normPath(gt)] {
			hit++
		}
	}
	return float64(hit) / float64(len(groundTruth))
}

func normPath(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}

func writeTempPlan(planJSON []byte) (string, error) {
	f, err := os.CreateTemp("", "plancheck-plan-*.json")
	if err != nil {
		return "", fmt.Errorf("temp plan file: %w", err)
	}
	if _, err := f.Write(planJSON); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write temp plan: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func removeFile(p string) { _ = os.Remove(p) }
