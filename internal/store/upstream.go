package store

import (
	"database/sql"
	"strings"
)

// UpstreamFingerprint is a row in upstream_fingerprints — a structural
// hash of one definition from a well-known Go module at a tagged version.
type UpstreamFingerprint struct {
	ModulePath  string
	Version     string
	DefName     string
	Kind        string
	Receiver    string
	Fingerprint string
	Signature   string
	Doc         string
}

// InsertUpstreamFingerprint inserts or replaces a row. Called by the
// upstream-ingest tool; not exposed to the MCP read/write path.
func (s *DB) InsertUpstreamFingerprint(u UpstreamFingerprint) error {
	_, err := s.execContext(s.Ctx(), `
		INSERT INTO upstream_fingerprints
		    (module_path, version, def_name, kind, receiver, fingerprint, signature, doc)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE fingerprint = VALUES(fingerprint),
		    signature = VALUES(signature),
		    doc = VALUES(doc)`,
		u.ModulePath, u.Version, u.DefName, u.Kind, u.Receiver,
		u.Fingerprint, u.Signature, u.Doc)
	return err
}

// InsertUpstreamFingerprints inserts many rows atomically. Faster than
// N single inserts when seeding an entire module.
func (s *DB) InsertUpstreamFingerprints(rows []UpstreamFingerprint) error {
	if len(rows) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO upstream_fingerprints
	    (module_path, version, def_name, kind, receiver, fingerprint, signature, doc)
	    VALUES `)
	args := make([]any, 0, len(rows)*8)
	for i, r := range rows {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, r.ModulePath, r.Version, r.DefName, r.Kind,
			r.Receiver, r.Fingerprint, r.Signature, r.Doc)
	}
	sb.WriteString(` ON DUPLICATE KEY UPDATE fingerprint = VALUES(fingerprint),
	    signature = VALUES(signature), doc = VALUES(doc)`)
	_, err := s.execContext(s.Ctx(), sb.String(), args...)
	return err
}

// FindUpstreamMatch returns the first upstream row whose (module_path,
// def_name, kind, receiver) key matches AND whose fingerprint equals the
// given structural hash. Returns nil, nil if no match.
//
// Used by the delta-from-prior projection to decide whether a local def
// is unchanged from a known upstream release. Callers pass the local
// def's HashBodyStructural output as fingerprint.
func (s *DB) FindUpstreamMatch(modulePath, defName, kind, receiver, fingerprint string) (*UpstreamFingerprint, error) {
	row := s.queryRowContext(s.Ctx(), `
		SELECT module_path, version, def_name, kind, receiver, fingerprint,
		       COALESCE(signature, ''), COALESCE(doc, '')
		FROM upstream_fingerprints
		WHERE module_path = ?
		  AND def_name = ?
		  AND kind = ?
		  AND receiver = ?
		  AND fingerprint = ?
		LIMIT 1`,
		modulePath, defName, kind, receiver, fingerprint)
	var u UpstreamFingerprint
	var sigCol, docCol textCol
	err := row.Scan(&u.ModulePath, &u.Version, &u.DefName, &u.Kind,
		&u.Receiver, &u.Fingerprint, &sigCol, &docCol)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Signature, u.Doc = string(sigCol), string(docCol)
	return &u, nil
}

// FindUpstreamVersions returns all known upstream versions for a def,
// regardless of fingerprint match. Used when the read op wants to say
// "diverges from all known versions of X" (helpful diagnostic when the
// user has patched a library locally).
func (s *DB) FindUpstreamVersions(modulePath, defName, kind, receiver string) ([]UpstreamFingerprint, error) {
	rows, err := s.queryContext(s.Ctx(), `
		SELECT module_path, version, def_name, kind, receiver, fingerprint,
		       COALESCE(signature, ''), COALESCE(doc, '')
		FROM upstream_fingerprints
		WHERE module_path = ? AND def_name = ? AND kind = ? AND receiver = ?
		ORDER BY version`,
		modulePath, defName, kind, receiver)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamFingerprint
	for rows.Next() {
		var u UpstreamFingerprint
		var sigCol, docCol textCol
		if err := rows.Scan(&u.ModulePath, &u.Version, &u.DefName, &u.Kind,
			&u.Receiver, &u.Fingerprint, &sigCol, &docCol); err != nil {
			return nil, err
		}
		u.Signature, u.Doc = string(sigCol), string(docCol)
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUpstreamFingerprints returns the total row count. Diagnostic for
// checking whether the corpus is seeded.
func (s *DB) CountUpstreamFingerprints() (int, error) {
	row := s.queryRowContext(s.Ctx(), `SELECT COUNT(*) FROM upstream_fingerprints`)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
