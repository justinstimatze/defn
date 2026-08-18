package mcp

import (
	"context"
	"strings"

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

func (s *server) appendStarter(r *sdkmcp.CallToolResult, o any, err error, req *sdkmcp.CallToolRequest, question string) (*sdkmcp.CallToolResult, any, error) {
	if err != nil || r == nil || r.IsError {
		return r, o, err
	}
	// #209: prefer the real user question (stashed by
	// hooks/defn-capture-question.sh on UserPromptSubmit) over the
	// op-specific fallback -- an intent-blind bundle (e.g. overview's
	// literal "project structure" placeholder) returns content
	// unrelated to what's actually being asked, wasting the one
	// starter-bundle shot this session gets.
	if real := s.lastUserQuestion(); real != "" {
		question = real
	}
	starter := s.maybeAppendStarterBundle(req, question)
	if starter == "" {
		return r, o, err
	}
	for _, block := range r.Content {
		if tc, ok := block.(*sdkmcp.TextContent); ok {
			tc.Text += starter
			break
		}
	}
	return r, o, err
}
