package planformat

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v2"
)

// TestCorpusFormatComparison measures JSON/DSL/S-expr byte cost on
// every real retrieval-bench task's ground_truth list
// (bench/retrieval/corpus/tasks) instead of hand-picked toy
// trajectories -- CLAUDE.md's "measure vs real workload" rule applies
// to design prototypes too, not just shipped perf claims. Approx-token
// uses the same bytes/4 rule of thumb used elsewhere in this repo's
// benches (no tokenizer dependency vendored here); treat it as a rough
// ratio signal; the DSL-vs-S-expr-vs-JSON RATIO is what this test is
// actually measuring, not an absolute token count.
func TestCorpusFormatComparison(t *testing.T) {
	root := "../../bench/retrieval/corpus/tasks"
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".yaml" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no corpus task files found")
	}

	var totalJSON, totalDSL, totalSExpr int
	tasksUsed := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var task corpusTask
		if err := yaml.Unmarshal(raw, &task); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		if len(task.GroundTruth) < 2 {
			continue // no multi-step trajectory to compare
		}
		steps := stepsForGroundTruth(task.GroundTruth)

		jsonText, err := RenderJSON(steps)
		if err != nil {
			t.Fatalf("%s: RenderJSON: %v", task.ID, err)
		}
		dslText := RenderDSL(steps)
		sexprText := RenderSExpr(steps)

		// Round-trip correctness -- a format that's dense but doesn't
		// parse back to the same Steps isn't a candidate, full stop.
		if got, err := ParseDSL(dslText); err != nil {
			t.Errorf("%s: ParseDSL round-trip: %v", task.ID, err)
		} else if !stepsEqual(got, steps) {
			t.Errorf("%s: ParseDSL round-trip mismatch: got %+v, want %+v", task.ID, got, steps)
		}
		if got, err := ParseSExpr(sexprText); err != nil {
			t.Errorf("%s: ParseSExpr round-trip: %v", task.ID, err)
		} else if !stepsEqual(got, steps) {
			t.Errorf("%s: ParseSExpr round-trip mismatch: got %+v, want %+v", task.ID, got, steps)
		}

		totalJSON += len(jsonText)
		totalDSL += len(dslText)
		totalSExpr += len(sexprText)
		tasksUsed++
	}

	if tasksUsed == 0 {
		t.Fatal("no corpus tasks had >=2 ground_truth entries")
	}

	t.Logf("corpus format comparison over %d real retrieval-bench tasks (bench/retrieval/corpus/tasks):", tasksUsed)
	t.Logf("  JSON:   %d bytes total (~%d approx tokens)", totalJSON, totalJSON/4)
	t.Logf("  DSL:    %d bytes total (~%d approx tokens) -- %.1f%% of JSON", totalDSL, totalDSL/4, 100*float64(totalDSL)/float64(totalJSON))
	t.Logf("  S-expr: %d bytes total (~%d approx tokens) -- %.1f%% of JSON", totalSExpr, totalSExpr/4, 100*float64(totalSExpr)/float64(totalJSON))
}

func stepsEqual(a, b []Step) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// stepsForGroundTruth turns a real retrieval task's ground_truth list
// into a plausible mixed-field trajectory: outline the entry point,
// alternate body/callers for the rest, exclude test callers every
// third step. This is a deterministic heuristic, not a claim about
// what an agent would actually request -- but it varies the field mix
// realistically enough that no format is favored by an artificial
// all-one-field corpus.
func stepsForGroundTruth(names []string) []Step {
	steps := make([]Step, 0, len(names))
	for i, name := range names {
		switch {
		case i == 0:
			steps = append(steps, Step{Target: name, Field: "outline"})
		case i%3 == 2:
			steps = append(steps, Step{Target: name, Field: "callers", ExcludeTest: true})
		case i%2 == 0:
			steps = append(steps, Step{Target: name, Field: "callers"})
		default:
			steps = append(steps, Step{Target: name, Field: "body"})
		}
	}
	return steps
}

type corpusTask struct {
	ID          string   `yaml:"id"`
	GroundTruth []string `yaml:"ground_truth"`
}
