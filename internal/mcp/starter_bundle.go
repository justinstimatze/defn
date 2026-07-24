package mcp

import (
	"context"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// maybeAppendStarterBundle is #203: on the FIRST search/overview call
// per session, appends a compact "Starter" bundle to the response.
// The bundle is a #195-shape context view derived from the current
// call's args — top-N relevant defs (outlined) + Sonnet synthesis
// when co-processor is available. Model gets the "here's what this
// codebase is" bundle without asking. Cuts the ~40-call exploration
// burst that dominates turn-1 cost.
//
// Fires once per session (tracked via sessionCache.starterInjected).
// Skipped when req or session is nil (test paths), or when the
// respCache isn't wired.
//
// Cost: one bigger turn-1 response. Break-even: eliminates ~4
// follow-up calls (per 2026-07-23 receipt analysis).
func (s *server) maybeAppendStarterBundle(req *sdkmcp.CallToolRequest, question string) string {
	if req == nil || s.respCache == nil {
		return ""
	}
	s.respCache.mu.Lock()
	sc := s.respCache.sessions[req.Session]
	if sc == nil {
		sc = &sessionCache{entries: map[string]cacheEntry{}}
		s.respCache.sessions[req.Session] = sc
	}
	if sc.starterInjected {
		s.respCache.mu.Unlock()
		return ""
	}
	sc.starterInjected = true
	s.respCache.mu.Unlock()

	if strings.TrimSpace(question) == "" {
		return ""
	}
	// Delegate to context op — it does the heavy lifting.
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
	return "\n\n---\n_[#203 starter bundle — first orient op of this session; won't repeat.]_\n\n" + body
}

// appendStarter is the small wrapper used by the search/overview
// dispatch cases in handleCode: takes the handler's original result
// and, if the session hasn't received its starter bundle yet, appends
// one to the TextContent. Errors pass through unchanged.
func (s *server) appendStarter(r *sdkmcp.CallToolResult, o any, err error, req *sdkmcp.CallToolRequest, question string) (*sdkmcp.CallToolResult, any, error) {
	if err != nil || r == nil || r.IsError {
		return r, o, err
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
