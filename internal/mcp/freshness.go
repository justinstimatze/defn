package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func freshnessFingerprintPath(projectDir string) string {
	return filepath.Join(projectDir, ".defn", "fingerprint.json")
}

func freshnessPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// hashFileOnDisk reads path and returns its (hash, size, mtimeNano). ok is
// false when the file can't be read right now (permissions, mid-write) --
// callers should leave the fingerprint entry untouched and retry next probe.
func hashFileOnDisk(path string) (hash string, size int64, mtime int64, ok bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, 0, false
	}
	return store.HashBody(string(data)), info.Size(), info.ModTime().UnixNano(), true
}

func readFreshnessFingerprint(projectDir string) *freshnessFingerprint {
	data, err := os.ReadFile(freshnessFingerprintPath(projectDir))
	if err != nil {
		return nil
	}
	var fp freshnessFingerprint
	if json.Unmarshal(data, &fp) != nil || fp.Files == nil {
		return nil
	}
	return &fp
}

func writeFreshnessFingerprint(projectDir string, fp *freshnessFingerprint) {
	data, err := json.Marshal(fp)
	if err != nil {
		return
	}
	_ = os.WriteFile(freshnessFingerprintPath(projectDir), data, 0o644)
}

// ensureFresh probes the working tree against the fingerprint left by the
// last probe/ingest and re-ingests any file whose content actually changed
// before the calling op reads the database. $0 and offline: stat-only on
// the common unchanged path (a size+mtime match is trusted without a
// re-read), one re-hash for a suspect file, and a real re-ingest+resolve
// only for files whose hash disagrees. Never fatal -- any error along the
// way just leaves the DB as it was and the op answers from that.
//
// This exists because defn otherwise requires an explicit op:"sync" after
// any edit made outside the code tool (Edit/Write on a .go file via the
// documented escape hatch, a git checkout, a stash pop) -- forgetting it
// means every subsequent read/outline/search/context/impact silently
// answers from a stale DB with no signal anything is wrong. Modeled on
// Graft's ensureFreshGraph (github.com/NanoNets/Graft, MIT-licensed): every
// retrieval call re-validates freshness instead of trusting the caller to
// remember. Deliberately narrower than Graft's version: it heals files
// already known to defn (changed or deleted), not brand-new files defn has
// never ingested -- discovering those still requires op:"sync", exactly as
// today, so this closes the common "forgot to sync after editing" case
// without taking on a full repo-wide file walk on every call.
//
// req may be nil (some test/measurement call paths construct a server
// without a real session) -- session-cache invalidation below is skipped
// in that case, same as every other req-nil-safe site in this file.
func (s *server) ensureFresh(req *sdkmcp.CallToolRequest) string {
	if s.backend == nil || s.projectDir == "" {
		return ""
	}
	s.freshnessMu.Lock()
	defer s.freshnessMu.Unlock()

	// The ground truth to diff the working tree against is always the
	// DB's OWN recorded hashes, fetched fresh every probe -- never the
	// fingerprint cache's own Hash field. That matters on the very first
	// probe after a file was edited before defn ever looked at it: if the
	// cache instead trusted a fresh stat paired with a stale hash, the
	// pairing would wrongly read back as "unchanged" forever after,
	// permanently hiding real drift. Comparing against the DB each time
	// closes that hole; the fingerprint only ever caches "stat as of the
	// moment we last confirmed disk matched the DB".
	knownHashes, err := s.backend.AllFileHashes()
	if err != nil {
		return ""
	}
	fp := readFreshnessFingerprint(s.projectDir)
	if fp == nil {
		fp = &freshnessFingerprint{Files: make(map[string]freshnessPrint, len(knownHashes))}
	}

	// dirty tracks whether fp actually changed this probe -- distinct from
	// changed/removed (which drive healing). A steady-state probe where
	// every file is stat-trusted mutates nothing and must not pay a full
	// JSON marshal+write just to re-persist an unchanged fingerprint.
	dirty := false
	var changed, removed []string
	seen := make(map[string]bool, len(knownHashes))
	for rel, dbHash := range knownHashes {
		seen[rel] = true
		abs := rel
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(s.projectDir, rel)
		}
		info, statErr := os.Stat(abs)
		if statErr != nil {
			// Only a genuine "no such file" means the file is actually
			// gone. Any other stat error -- EACCES from an AV/backup
			// scan, an NFS/EIO blip, a symlink loop, a stat racing an
			// atomic-rename save -- is not evidence of deletion, and
			// treating it as such would permanently prune real,
			// still-existing definitions via DeleteFile below. Leave the
			// cached print alone and let the next probe retry.
			if os.IsNotExist(statErr) {
				removed = append(removed, rel)
			}
			continue
		}
		if print, ok := fp.Files[rel]; ok &&
			print.Size == info.Size() && print.MTime == info.ModTime().UnixNano() && print.Hash == dbHash {
			continue // stat-trusted: confirmed clean last probe, disk hasn't moved since
		}
		// Suspect: no cached print, a stat mismatch, or the DB's own hash
		// moved since we last cached (e.g. another process synced this
		// file). Confirm by bytes -- a touch or a checkout restoring
		// identical content shouldn't cost a re-ingest.
		diskHash, size, mtime, ok := hashFileOnDisk(abs)
		if !ok {
			continue // unreadable right now -- leave it to the next probe
		}
		if diskHash == dbHash {
			fp.Files[rel] = freshnessPrint{Size: size, MTime: mtime, Hash: dbHash}
			dirty = true
			continue
		}
		changed = append(changed, rel)
	}
	// A file the fingerprint cache still remembers but the DB no longer
	// lists (pruned by an explicit sync elsewhere) is stale cache, not
	// drift to heal -- just forget it.
	for rel := range fp.Files {
		if !seen[rel] {
			delete(fp.Files, rel)
			dirty = true
		}
	}

	if len(changed) == 0 && len(removed) == 0 {
		if dirty {
			writeFreshnessFingerprint(s.projectDir, fp)
		}
		return ""
	}

	healed := 0
	for _, rel := range changed {
		abs := rel
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(s.projectDir, rel)
		}
		if _, err := ingest.IngestFile(s.backend, s.projectDir, abs); err != nil {
			continue
		}
		if err := resolve.ResolveFile(s.backend, s.projectDir, abs); err != nil {
			continue
		}
		healed++
		if hash, size, mtime, ok := hashFileOnDisk(abs); ok {
			fp.Files[rel] = freshnessPrint{Size: size, MTime: mtime, Hash: hash}
		}
	}
	pruned := 0
	for _, rel := range removed {
		if err := s.backend.DeleteFile(rel); err == nil {
			pruned++
		}
		delete(fp.Files, rel)
	}

	if healed > 0 || pruned > 0 {
		_ = s.autoCommit()
		// The heal just changed what the DB says for these files' defs --
		// any session-cached "already read, hasn't changed" state
		// (respCache's bodyServed/dedup/readDowngraded) or reachability
		// snapshot (s.reach) now describes a world that no longer exists.
		// Without this, the very next read of a def this heal just
		// updated can still hit the pre-heal dedup/bodyServed stub and
		// silently serve the STALE body in the same breath this note
		// claims something changed -- defeating the whole feature. Full
		// wipe (not the write-ops' narrower invalidateNames) because a
		// heal's blast radius is per-FILE, and invalidateNames' file
		// param is currently a no-op (see dedup.go) -- there is no scoped
		// alternative to fall back to here.
		if req != nil && s.respCache != nil {
			s.respCache.invalidate(req.Session)
		}
		if s.reach != nil {
			s.reach.invalidate()
		}
	}
	writeFreshnessFingerprint(s.projectDir, fp)

	if healed == 0 && pruned == 0 {
		return ""
	}
	var parts []string
	if healed > 0 {
		parts = append(parts, fmt.Sprintf("%d file%s changed", healed, freshnessPlural(healed)))
	}
	if pruned > 0 {
		parts = append(parts, fmt.Sprintf("%d file%s removed", pruned, freshnessPlural(pruned)))
	}
	return fmt.Sprintf("[defn: auto-synced (%s) before answering -- the working tree had drifted from the index]\n\n", strings.Join(parts, ", "))
}

type freshnessFingerprint struct {
	Files map[string]freshnessPrint `json:"files"`
}

// freshnessPrint is one file's recorded (size, mtime, hash) as of the last
// probe -- a stat-first, hash-fallback shape: size+mtime agreeing with the
// file on disk is trusted without a re-read, disagreeing costs exactly one
// re-hash.
type freshnessPrint struct {
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // UnixNano
	Hash  string `json:"hash"`
}
