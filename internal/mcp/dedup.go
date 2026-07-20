package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// respCache dedupes identical read-side responses within a session. When
// the same (op, argKey) is called twice with a byte-identical result, the
// second call returns a compact "already served at call #N" stub instead
// of re-emitting the payload — a wire-cost win when the model forgets it
// already read a def.
//
// Invalidation: any write op (edit/create/delete/rename/move/apply) clears
// the whole session cache; the next read is a clean miss and re-hydrates.
// Coarse but correct — after mutations, we can't cheaply reason about
// which reads are still valid, and false-clean stubs would be a
// correctness bug.
//
// v1 scope: read, outline, slice, read-file, file-defs. Excluded: search
// (result varies with pattern — argKey already covers it, but repeats are
// rare), impact (structural, seldom exact-repeated), overview (project-wide
// summary, worth re-serving).
type respCache struct {
	mu       sync.Mutex
	sessions map[*sdkmcp.ServerSession]*sessionCache
}

type sessionCache struct {
	seq     int64
	entries map[string]cacheEntry
}

type cacheEntry struct {
	hash     string
	servedAt int64
	size     int
}

// dedupMinBytes is the smallest response size we bother deduping. The stub
// message is ~250 bytes; on smaller payloads a "cache hit" would actually
// inflate wire cost. Set well above stub size so dedup is always a win.
const dedupMinBytes = 512

func newRespCache() *respCache {
	return &respCache{sessions: map[*sdkmcp.ServerSession]*sessionCache{}}
}

// dedupOpKey returns (op, argKey, true) if args.Op is a read-side op we
// should dedup. argKey is a compact canonical key that co-identifies the
// request within its op namespace. Returns ok=false for ops we don't cache.
func dedupOpKey(args codeParam) (string, string, bool) {
	switch args.Op {
	case "read":
		if args.Full {
			return "read", args.Name + "|full", true
		}
		return "read", args.Name, true
	case "outline":
		return "outline", args.Name, true
	case "slice":
		if args.Index != 0 {
			return "slice", fmt.Sprintf("%s|%s|%d", args.Name, args.Slice, args.Index), true
		}
		return "slice", args.Name + "|" + args.Slice, true
	case "read-file":
		return "read-file", args.File, true
	case "file-defs":
		return "file-defs", args.File, true
	}
	return "", "", false
}

// isWriteOp reports whether args.Op mutates the DB and should therefore
// invalidate the session's response cache on success.
func isWriteOp(op string) bool {
	switch op {
	case "edit", "insert", "create", "delete", "rename", "move", "apply",
		"insert-precondition", "replace-slice", "replace-hunk",
		"wrap-in-defer", "rename-param", "add-import", "patch",
		"sync", "resolve", "merge", "checkout", "commit", "merge-abort":
		return true
	}
	return false
}

func (c *respCache) getSession(sess *sdkmcp.ServerSession) *sessionCache {
	sc := c.sessions[sess]
	if sc == nil {
		sc = &sessionCache{entries: map[string]cacheEntry{}}
		c.sessions[sess] = sc
	}
	return sc
}

func (c *respCache) invalidate(sess *sdkmcp.ServerSession) {
	if sess == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if sc, ok := c.sessions[sess]; ok {
		sc.entries = map[string]cacheEntry{}
	}
}

// dedup inspects r; if this session has already returned an identical
// response for (op, argKey), replace r's text with a compact stub and
// return that. Otherwise record the response and return r unchanged.
// Sessions with a nil session pointer (uncommon; happens in unit tests
// invoking handlers directly) are pass-through.
func (c *respCache) dedup(sess *sdkmcp.ServerSession, op, argKey string, r *sdkmcp.CallToolResult) *sdkmcp.CallToolResult {
	if sess == nil || r == nil || r.IsError || len(r.Content) == 0 {
		return r
	}
	tc, ok := r.Content[0].(*sdkmcp.TextContent)
	if !ok {
		return r
	}
	if len(tc.Text) < dedupMinBytes {
		return r
	}
	sum := sha256.Sum256([]byte(tc.Text))
	hash := hex.EncodeToString(sum[:8])

	c.mu.Lock()
	defer c.mu.Unlock()
	sc := c.getSession(sess)
	sc.seq++
	key := op + "|" + argKey
	if prev, hit := sc.entries[key]; hit && prev.hash == hash {
		stub := fmt.Sprintf(
			"[cached: identical %s response already served in this session at call #%d (hash=%s, %d bytes saved). Nothing has changed since — no need to re-request. If you need a fresh read after external changes, call `code(op:\"sync\")` first.]",
			op, prev.servedAt, hash, prev.size,
		)
		return textResult(stub)
	}
	sc.entries[key] = cacheEntry{hash: hash, servedAt: sc.seq, size: len(tc.Text)}
	return r
}
