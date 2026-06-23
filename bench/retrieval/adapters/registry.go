package adapters

import (
	"os/exec"

	"github.com/justinstimatze/defn/bench/retrieval/benchtype"
)

// All returns every adapter known to the harness. The harness filters out
// any adapter whose dependency is not on PATH.
func All() []benchtype.Adapter {
	return []benchtype.Adapter{
		NewDefn(),
		NewDefnRanked(),
		NewGrep(),
	}
}

func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// Available returns the subset of All() whose external dependencies exist.
func Available() []benchtype.Adapter {
	out := make([]benchtype.Adapter, 0, 2)
	for _, a := range All() {
		switch a.Name() {
		case "defn", "defn-ranked":
			if hasCommand("defn") {
				out = append(out, a)
			}
		case "grep":
			if hasCommand("rg") {
				out = append(out, a)
			}
		default:
			out = append(out, a)
		}
	}
	return out
}
