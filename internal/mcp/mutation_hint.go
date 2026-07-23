package mcp

import (
	"fmt"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mutationHint tracks per-session per-file mutation counts and returns
// an "apply batching" nudge when the model does N sequential mutations
// to one file — the highest-frequency n-gram in the Multi-SWE-bench Go
// trajectory corpus (1,780 str_replace→str_replace bigrams, 959
// str_replace×3 trigrams). One `apply` call collapses N mutations into
// one emit+build with atomic rollback.
//
// Reset per session (session identity = *sdkmcp.ServerSession pointer,
// same pattern as [[respCache]]).
type mutationHint struct {
	mu       sync.Mutex
	sessions map[*sdkmcp.ServerSession]map[string]int // session → file → count
}

func newMutationHint() *mutationHint {
	return &mutationHint{sessions: map[*sdkmcp.ServerSession]map[string]int{}}
}

// mutationHintThreshold is the count at which we emit the nudge. Set to
// 3 so the model has ALREADY paid the cost of serial mutations twice
// before we suggest batching — the nudge triggers exactly when it would
// have paid off. Lower thresholds risk noise on legitimate single-file
// iteration; higher misses the window before the model moves on.
const mutationHintThreshold = 3

// note records that a mutation happened to sourceFile in this session.
// Returns a hint string when the file crosses the threshold, empty
// otherwise. The hint fires exactly once per file per session (at the
// threshold count) so we don't spam every subsequent mutation.
func (m *mutationHint) note(session *sdkmcp.ServerSession, sourceFile string) string {
	if session == nil || sourceFile == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	files := m.sessions[session]
	if files == nil {
		files = map[string]int{}
		m.sessions[session] = files
	}
	files[sourceFile]++
	if files[sourceFile] == mutationHintThreshold {
		return fmt.Sprintf(
			"tip: %d mutations to %s this session — future multi-op edits "+
				"to one file can batch into one `apply({operations: [...]})` "+
				"call for one emit+build + atomic rollback.\n",
			mutationHintThreshold, sourceFile)
	}
	return ""
}

// sessionOf returns req.Session or nil if req itself is nil. Nil-safe
// helper for handlers whose req is optionally set (Measure* paths).
func sessionOf(req *sdkmcp.CallToolRequest) *sdkmcp.ServerSession {
	if req == nil {
		return nil
	}
	return req.Session
}

// clear drops a session's counters. Called from the write-op branch that
// already invalidates respCache — same session-lifetime shape.
func (m *mutationHint) clear(session *sdkmcp.ServerSession) {
	if session == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, session)
}
