package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
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
// History: this was originally shipped as #77 (22b70fa) and reverted in
// 43f5c7c because it *coincided* with a real bug (withUsage was silently
// stripping response bodies via StructuredContent). That bug was actually
// unrelated; #96 fixed it separately. Restored + extended per #152 to
// cover impact/overview/expand/methods on top of the original scope.
//
// Scope: read, outline, slice, read-file, file-defs, impact, overview,
// expand, methods, explain. Excluded: search (result varies with pattern
// — repeats are rare and args.Rank shifts output shape), similar (MinHash-
// sorted, ranking may drift), test (build output changes with each run).
type respCache struct {
	mu       sync.Mutex
	sessions map[*sdkmcp.ServerSession]*sessionCache
}

type sessionCache struct {
	seq             int64
	entries         map[string]cacheEntry
	starterInjected bool   // #203: true after first orient op has appended the starter bundle
	turnToken       string // #209: last-seen turn-token; a change means a new turn started
	readShapedCount int    // #209: individual read-shaped calls made so far this turn
	// compactionEpoch is the last-seen value of .defn/.compaction-epoch,
	// bumped once per PreCompact hook fire (see checkCompactionEpoch).
	// cacheEntry.epoch and bodyServed's values are stamped with whatever
	// this was at creation time, so dedup/subsumption can tell how many
	// compactions have happened since -- a compaction can silently drop
	// content from the caller's actual working context even though this
	// server-side cache entry survives untouched.
	compactionEpoch int64
	bodyServed      map[string]int64 // #176/#227: name -> compaction epoch when its full body (read full:true) was served this session
	justMutated     map[string]bool  // names changed by the most recent write op; consumed once by the next read to force full body instead of the summary-mode default
	// pendingReadNames accumulates the resolved name of every nameable
	// read-shaped call (see nameableReadOps) since the last reset, so a
	// circuit-breaker block can auto-batch them via expand instead of
	// just refusing. Reset alongside readShapedCount.
	pendingReadNames []string
	// pendingWantsBody is true when any call folded into pendingReadNames
	// this turn was op:"read" (the one nameable op whose whole point is
	// the source body) -- so an auto-batch redirect through expand can
	// include "body" instead of silently downgrading a read into an
	// outline+callers-only response. See #250.
	pendingWantsBody bool
}

type cacheEntry struct {
	hash     string
	servedAt int64
	size     int
	epoch    int64 // compaction epoch active when this entry was created; see sessionCache.compactionEpoch
}

// dedupMinBytes is the smallest response size we bother deduping. #209:
// lowered from 512 -- the original floor let small repeated responses
// (e.g. the same auto-downgrade "outline shown" note served twice
// verbatim) slip past dedup entirely, so a model blindly retrying a
// call that told it nothing new got zero signal to stop. 200 stays
// safely under any real outline/read/impact payload while still well
// above the stub message's own size, so dedup is still always a wire-
// cost win, just with a much lower bar for when it kicks in.
const dedupMinBytes = 200

func newRespCache() *respCache {
	return &respCache{sessions: map[*sdkmcp.ServerSession]*sessionCache{}}
}

// dedupOpKey returns (op, argKey, true) if args.Op is a read-side op we
// should dedup. argKey is a compact canonical key that co-identifies the
// request within its op namespace. Returns ok=false for ops we don't cache.
func dedupOpKey(args codeParam) (string, string, bool) {
	switch args.Op {
	case "read":
		// #274-mining finding: Query activates #153's query-adaptive
		// narrowing (return only body statements matching a token) --
		// a full-body read followed by a query-scoped read of the same
		// def used to collide on one key, serving the stale full-body
		// stub for what should have been genuinely different, smaller
		// output. Confirmed live in a real trajectory (prometheus-18712).
		key := args.Name
		if args.Full {
			key += "|full"
		}
		if args.Query != "" {
			key += "|query:" + args.Query
		}
		if args.LineRange != "" {
			key += "|range:" + args.LineRange
		}
		return "read", key, true
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
	// 2026-08-04: search/find were exempt from dedup despite showing real
	// repeat patterns in a corpus read-locality analysis (e.g. the same
	// search pattern re-run, or find re-run on the same file, within one
	// session). Both are deterministic given the DB's current content, so
	// the same correctness argument as the other read-family ops applies.
	case "search":
		return "search", fmt.Sprintf("%s|%d|%v", args.Pattern, args.Limit, args.Rank), true
	case "find":
		return "find", fmt.Sprintf("%s|%d", args.File, args.Line), true
	// #152 extensions: structural / summary ops the model re-asks when
	// verifying a plan across turns. Same session-scoped dedup shape.
	case "impact":
		return "impact", args.Name, true
	case "overview":
		if args.File != "" {
			return "overview", "file:" + args.File, true
		}
		return "overview", "project", true
	case "expand":
		// Include array is order-sensitive; encoded verbatim.
		key := args.Name
		if len(args.Include) > 0 {
			key += "|" + fmt.Sprintf("%v", args.Include)
		}
		return "expand", key, true
	case "methods":
		return "methods", args.Name, true
	case "explain":
		// #186: explain(name:) and explain(question:) route to entirely
		// different handlers (legacy static-context vs the Sonnet Q&A
		// co-processor) and explain(question:) also accepts a multi-def
		// Names scope distinct from Name. Keying on Name alone (as if
		// every explain call were the legacy path) meant two different
		// questions about the same def, or the same question against two
		// different Names scopes, collided on one dedup key -- silently
		// defeating dedup for the question-driven path rather than ever
		// serving a wrong answer (dedup() still hashes the real response
		// before comparing), but interleaved explain-with-question calls
		// on the same def essentially never dedup. Match "context"'s
		// convention below: fold in every dimension the response actually
		// depends on.
		key := args.Name
		if len(args.Names) > 0 {
			key += "|" + fmt.Sprintf("%v", args.Names)
		}
		if args.Question != "" {
			key += "|" + args.Question
		}
		return "explain", key, true
	// 2026-08-05: context was defn's most expensive uncovered op --
	// potentially a top-N-defs outline + refs graph + Sonnet synthesis
	// bundle -- yet had zero dedup coverage. Confirmed against a real
	// transcript before adding this: 18 context calls in one session,
	// only 15 distinct questions, one exact question asked 4 times.
	// Keyed on the raw question text, same convention as search's
	// pattern -- exact match only, no normalization (no evidence yet of
	// near-duplicate-but-not-identical questions being common enough to
	// justify that extra complexity).
	case "context":
		return "context", args.Question, true
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
		"sync", "resolve", "merge", "checkout", "commit", "merge-abort",
		"retarget-field-value":
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
		sc.bodyServed = nil
	}
}

func (c *respCache) dedup(sess *sdkmcp.ServerSession, op, argKey string, r *sdkmcp.CallToolResult) *sdkmcp.CallToolResult {
	if sess == nil || r == nil || r.IsError || len(r.Content) == 0 || stripped("dedup") {
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
		// #227: a compaction can silently drop this content from the
		// caller's actual working context even though the entry survives
		// here (it lives in this server process, not the model's
		// context). Past staleEpochThreshold compactions since the entry
		// was created, don't bet on "they still have it" -- let the
		// already-computed real content (r) through instead of the stub.
		if sc.compactionEpoch-prev.epoch <= staleEpochThreshold {
			stub := fmt.Sprintf(
				"[cached: identical %s response already served in this session at call #%d (hash=%s, %d bytes saved). Nothing has changed since — no need to re-request. If you need a fresh read after external changes, call `code(op:\"sync\")` first. If the earlier response was an outline/size-only note, pass full:true to get the full body instead of repeating this call.]",
				op, prev.servedAt, hash, prev.size,
			)
			return textResult(stub)
		}
	}
	sc.entries[key] = cacheEntry{hash: hash, servedAt: sc.seq, size: len(tc.Text), epoch: sc.compactionEpoch}
	return r
}

// hasBodyServed is #176's cross-def context reuse: reports whether
// name's full body has already been served via read(full:true) in
// this session. Used to suppress a now-redundant outline() call --
// outline's signature+doc+callers/callees is a strict subset of what
// a full body read already includes, so re-deriving and
// re-transmitting it would be pure waste. Only read(full:true) marks
// this (see markBodyServed) -- a plain read() can be silently
// downgraded to summary or auto-outline (#174/#184), so it is not a
// reliable signal that the caller actually has the full body.
func (c *respCache) hasBodyServed(sess *sdkmcp.ServerSession, name string) bool {
	_, ok := c.bodyServedEpochsAgo(sess, name)
	return ok
}

// bodyServedEpochsAgo reports how many compactions have happened since
// name's full body was served via read(full:true) this session, and
// whether it was served at all (ok=false means never -- a normal miss
// for subsumption purposes). Callers use epochsAgo to decide whether
// suppressing a later outline/read/slice with a bare stub is still
// trustworthy (see staleEpochThreshold) or whether the claim has
// survived too many compactions to bet on.
func (c *respCache) bodyServedEpochsAgo(sess *sdkmcp.ServerSession, name string) (epochsAgo int64, ok bool) {
	if sess == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sc := c.sessions[sess]
	if sc == nil || sc.bodyServed == nil {
		return 0, false
	}
	epoch, hit := sc.bodyServed[name]
	if !hit {
		return 0, false
	}
	return sc.compactionEpoch - epoch, true
}

// markBodyServed records that name's full body was served this
// session. Cleared by invalidate() alongside dedup entries -- any
// write op means the def's shape could have changed.
func (c *respCache) markBodyServed(sess *sdkmcp.ServerSession, name string) {
	if sess == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sc := c.getSession(sess)
	if sc.bodyServed == nil {
		sc.bodyServed = map[string]int64{}
	}
	sc.bodyServed[name] = sc.compactionEpoch
}

// markMutated records that name was just changed by a write op this
// session. Consumed once by takeMutated -- the very next read of name
// gets the full body instead of the summary-mode default, since a read
// immediately following a mutation is almost always "show me what I
// just did," not "what does this do."
func (c *respCache) markMutated(sess *sdkmcp.ServerSession, name string) {
	if sess == nil || name == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sc := c.getSession(sess)
	if sc.justMutated == nil {
		sc.justMutated = map[string]bool{}
	}
	sc.justMutated[name] = true
}

// takeMutated reports whether name was just mutated this session and
// clears the flag -- a one-shot signal so only the immediate follow-up
// read is affected, not every future read of name.
func (c *respCache) takeMutated(sess *sdkmcp.ServerSession, name string) bool {
	if sess == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sc := c.sessions[sess]
	if sc == nil || !sc.justMutated[name] {
		return false
	}
	delete(sc.justMutated, name)
	return true
}

// invalidateNames clears only the dedup and bodyServed entries anchored
// on the given names and files, plus the project-wide overview (its
// content spans every def, so any determinable-blast-radius write still
// invalidates it). This is the scoped counterpart to invalidate -- use
// it whenever writeTargets can determine the write's blast radius;
// fall back to invalidate (full wipe) when it can't.
func (c *respCache) invalidateNames(sess *sdkmcp.ServerSession, names, files []string) {
	if sess == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sc, ok := c.sessions[sess]
	if !ok {
		return
	}
	delete(sc.entries, "overview|project")
	for key := range sc.entries {
		// search's cache key is pattern text, not a def/file identity --
		// any determinable write could shift what a pattern matches
		// anywhere in the DB, so (like overview|project) it's always
		// cleared rather than attempting per-pattern staleness analysis.
		if strings.HasPrefix(key, "search|") {
			delete(sc.entries, key)
			continue
		}
		// context's key is free-text question, modeled explicitly on
		// search's own "any determinable write could shift the answer"
		// reasoning (its own case comment says so) -- give it the same
		// blanket-clear treatment. Without this, a cached context bundle
		// was untouched by every scoped write (edit/rename/create/delete/
		// apply) and only ever cleared by a full invalidate (sync/resolve/
		// merge), unlike search which explicitly gets this.
		if strings.HasPrefix(key, "context|") {
			delete(sc.entries, key)
			continue
		}
		for _, name := range names {
			for _, op := range readOpsWithNameKey {
				prefix := op + "|" + name
				if key == prefix || strings.HasPrefix(key, prefix+"|") {
					delete(sc.entries, key)
				}
			}
		}
		for _, file := range files {
			if key == "read-file|"+file || key == "file-defs|"+file || key == "overview|file:"+file || strings.HasPrefix(key, "find|"+file+"|") {
				delete(sc.entries, key)
			}
		}
	}
	for _, name := range names {
		delete(sc.bodyServed, name)
	}
}

// readOpsWithNameKey lists dedup ops whose cache key is anchored on a
// def name, possibly followed by a "|"-separated suffix (read's
// "|full" variant, slice's "|<kind>[|<index>]", expand's "|<include>")
// -- used by invalidateNames to recognize which keys belong to a given
// name without needing to reconstruct dedupOpKey's exact suffix rules.
var readOpsWithNameKey = []string{"read", "outline", "slice", "impact", "expand", "methods", "explain"}

// writeTargets returns the def names and files a write op is known to
// touch, so invalidate can be scoped to just those dedup/bodyServed
// entries instead of wiping the whole session cache on every mutation.
// ok=false means the op's blast radius can't be determined from args
// alone (sync/resolve/merge/checkout/commit/merge-abort -- rarer,
// structurally significant ops where a full invalidate is the safer
// default) or an apply batch contains an op this function doesn't
// recognize -- callers should fall back to a full invalidate in that case.
//
// Measured motivation (2026-08-04): a read-locality analysis of 257 real
// sessions on this repo found edits interleaved constantly with reads of
// OTHER, unrelated defs -- a blanket per-write invalidate was erasing the
// dedup benefit for all of them on every single mutation, not just the
// touched one.
func writeTargets(args codeParam) (names, files []string, ok bool) {
	switch args.Op {
	case "edit", "insert", "insert-precondition", "replace-slice",
		"replace-hunk", "wrap-in-defer", "rename-param", "delete",
		"retarget-field-value", "patch":
		if args.Name == "" {
			return nil, nil, false
		}
		return []string{args.Name}, nil, true
	case "rename":
		// Unlike the other name-scoped ops above, a rename's real blast
		// radius isn't just OldName/NewName -- handleRename also rewrites
		// every caller's body (astRename + UpsertDefinition) and, for a
		// type rename, every sibling method's receiver clause. None of
		// those names are knowable from args alone (they only exist after
		// tx.GetCallers/GetModuleDefinitions runs inside the handler), so
		// a scoped invalidate here would leave a caller's stale bodyServed
		// entry in place even though its source text just changed --
		// confirmed reachable: the bodyServed short-circuit in dedup.go is
		// not hash-gated, so a later read of that caller could report
		// "nothing has changed since" when it, in fact, has. Report the
		// blast radius as undeterminable so the caller falls back to a
		// full invalidate, same as sync/resolve/merge do.
		return nil, nil, false
	case "move":
		if args.Name == "" {
			return nil, nil, false
		}
		return []string{args.Name}, nil, true
	case "create", "add-import":
		if args.File == "" {
			return nil, nil, false
		}
		return nil, []string{args.File}, true
	case "apply":
		var allNames, allFiles []string
		for _, op := range args.Operations {
			switch op.Op {
			case "edit", "insert", "insert-precondition", "replace-slice",
				"replace-hunk", "wrap-in-defer", "rename-param", "delete",
				"retarget-field-value", "patch":
				if op.Name == "" {
					return nil, nil, false
				}
				allNames = append(allNames, op.Name)
			case "rename":
				// Same reasoning as the top-level "rename" case above --
				// a rename's real blast radius (rewritten callers, and for
				// type renames sibling method receivers) isn't knowable
				// from op.Name/op.NewName alone. Undetermined blast radius
				// for one op in the batch means the whole batch falls back
				// to a full invalidate.
				return nil, nil, false
			case "move":
				if op.Name == "" {
					return nil, nil, false
				}
				allNames = append(allNames, op.Name)
			case "create", "add-import":
				if op.File == "" {
					return nil, nil, false
				}
				allFiles = append(allFiles, op.File)
			default:
				return nil, nil, false
			}
		}
		return allNames, allFiles, true
	}
	return nil, nil, false
}

// staleEpochThreshold is how many compactions a cached "you already have
// this" claim survives before dedup/subsumption stop trusting it and
// either let real content through (dedup) or redirect to a richer,
// guaranteed-correct bundle instead of a stub (subsumption).
//
// 2026-08-05: raised from 1 to 4 after a real-data sweep
// (bench/cache-sim/sweep_stale_threshold.py) against 347 real repeat
// occurrences from a production transcript. Both known real failures
// (2026-08-04: a user-visible wrong answer, and a 3-extra-call
// workaround scramble) occurred at epoch_distance 5 and 54 -- 4 is the
// highest value that still avoids both, and it cuts repeat-related
// token cost ~44% versus the prior threshold=1 (193,214 -> 108,930
// tokens on the swept transcript). This is an evidence-bounded choice,
// not a proof of safety: "no confirmed failure at distance <=4" is an
// absence-of-evidence argument from a small sample (n=2 known
// failures), not confirmation that distance 2-4 is actually safe --
// risk plausibly increases smoothly with distance rather than stepping
// cleanly at some threshold. 4 is simply the tightest bound the direct
// evidence supports; revisit if a larger sample of real repeat-hits
// surfaces a failure below distance 5.
const staleEpochThreshold = 4
