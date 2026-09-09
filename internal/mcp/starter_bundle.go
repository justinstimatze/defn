package mcp

import (
	"context"
	"regexp"
	"strings"
	"unicode"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *server) maybeAppendStarterBundle(req *sdkmcp.CallToolRequest, question string) string {
	if req == nil || s.respCache == nil || stripped("starter-bundle") {
		return ""
	}
	// Check before consuming the one-shot flag below: an empty question
	// means there was nothing for handleContext to work with anyway, so
	// this call shouldn't burn the session's only starter-bundle
	// opportunity on a no-op. Previously the flag was set unconditionally
	// before this check, so a first orient-shaped call that happened to
	// resolve to an empty question permanently forfeited the bundle for
	// every later call in the session, even once a real question showed
	// up.
	if strings.TrimSpace(question) == "" {
		return ""
	}
	// getSession is lock-free by design, safe to call while already
	// holding respCache.mu -- this used to hand-inline getSession's own
	// logic instead of calling it, byte-identical but drifting risk: a
	// future change to sessionCache{}'s zero-value init would be easy to
	// miss updating here.
	s.respCache.mu.Lock()
	sc := s.respCache.getSession(req.Session)
	if sc.starterInjected {
		s.respCache.mu.Unlock()
		return ""
	}
	sc.starterInjected = true
	s.respCache.mu.Unlock()

	// Delegate to context op -- it does the heavy lifting.
	r, _, err := s.handleContext(context.Background(), req, codeParam{
		Op:       "context",
		Question: question,
	})
	if err != nil || r == nil || r.IsError {
		return ""
	}
	body := resultTextRaw(r)
	if body == "" {
		return ""
	}
	return "\n\n---\n_[#203 starter bundle -- first orient op of this session; won't repeat.]_\n\n" + body
}

// hasIdentifierShapedToken reports whether s contains at least one token
// that looks like a real Go identifier reference (an internal case
// transition, e.g. ParseVector/handleEdit, or a snake_case underscore)
// rather than plain conversational English. Gates the starter bundle's
// use of the raw captured user prompt (hooks/defn-capture-question.sh)
// against firing on pure filler ("good call do it") or a bench harness's
// task-preamble boilerplate ("You are working in a Go repository. Please
// solve the following issue.") -- both measured (2026-09-09, real
// prom-opus trajectories + this very session) to match broadly against
// thousands of unrelated defs on common words like "go"/"issue"/"call",
// burning the session's one starter-bundle shot on an irrelevant dump
// instead of falling back to the op's own more specific default (the
// def name actually being read, the search pattern actually used).
func hasIdentifierShapedToken(s string) bool {
	for _, tok := range identifierShapedTokenRe.FindAllString(s, -1) {
		if strings.Contains(tok, "_") && len(tok) > 2 {
			return true
		}
		hasLower, hasUpperNotFirst := false, false
		for i, r := range tok {
			switch {
			case unicode.IsLower(r):
				hasLower = true
			case unicode.IsUpper(r) && i > 0:
				hasUpperNotFirst = true
			}
		}
		if hasLower && hasUpperNotFirst {
			return true
		}
	}
	return false
}

var identifierShapedTokenRe = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\b`)
