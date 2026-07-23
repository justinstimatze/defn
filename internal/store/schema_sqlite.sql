-- defn: Code Database Schema (SQLite)
-- Mirror of schema.sql (Dolt/MySQL flavor), translated for SQLite:
--   * INT PRIMARY KEY AUTO_INCREMENT       -> INTEGER PRIMARY KEY AUTOINCREMENT
--   * VARCHAR(N) / LONGTEXT / MEDIUMTEXT   -> TEXT
--   * BOOLEAN                              -> INTEGER (0/1)
--   * CREATE FULLTEXT INDEX ... ON t(col)  -> deferred to task #137 (FTS5 tokenizer)
--   * Reserved-name columns (`key`, `value`) quoted with "..."

CREATE TABLE IF NOT EXISTS modules (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT UNIQUE NOT NULL,
    name TEXT NOT NULL,
    doc  TEXT
);

CREATE TABLE IF NOT EXISTS definitions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    module_id   INTEGER NOT NULL,
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    exported    INTEGER NOT NULL,
    test        INTEGER NOT NULL DEFAULT 0,
    receiver    TEXT,
    signature   TEXT,
    doc         TEXT,
    start_line  INTEGER,
    end_line    INTEGER,
    source_file TEXT DEFAULT '',
    hash        TEXT NOT NULL,
    UNIQUE(module_id, name, kind, receiver, test),
    FOREIGN KEY (module_id) REFERENCES modules(id)
);

CREATE TABLE IF NOT EXISTS bodies (
    def_id INTEGER PRIMARY KEY,
    body   TEXT NOT NULL,
    FOREIGN KEY (def_id) REFERENCES definitions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS refs (
    from_def INTEGER NOT NULL,
    to_def   INTEGER NOT NULL,
    kind     TEXT NOT NULL,
    PRIMARY KEY (from_def, to_def, kind),
    FOREIGN KEY (from_def) REFERENCES definitions(id),
    FOREIGN KEY (to_def)   REFERENCES definitions(id)
);

CREATE TABLE IF NOT EXISTS imports (
    module_id     INTEGER NOT NULL,
    imported_path TEXT NOT NULL,
    alias         TEXT,
    PRIMARY KEY (module_id, imported_path),
    FOREIGN KEY (module_id) REFERENCES modules(id)
);

CREATE TABLE IF NOT EXISTS project_files (
    path    TEXT PRIMARY KEY,
    content TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS file_sources (
    module_id   INTEGER NOT NULL,
    source_file TEXT NOT NULL,
    raw         TEXT NOT NULL,
    file_hash   TEXT NOT NULL,
    PRIMARY KEY (module_id, source_file),
    FOREIGN KEY (module_id) REFERENCES modules(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_file_sources_hash ON file_sources(file_hash);

CREATE TABLE IF NOT EXISTS upstream_fingerprints (
    module_path TEXT NOT NULL,
    version     TEXT NOT NULL,
    def_name    TEXT NOT NULL,
    kind        TEXT NOT NULL,
    receiver    TEXT NOT NULL DEFAULT '',
    fingerprint TEXT NOT NULL,
    signature   TEXT,
    doc         TEXT,
    PRIMARY KEY (module_path, version, def_name, kind, receiver)
);
CREATE INDEX IF NOT EXISTS idx_upstream_fingerprint ON upstream_fingerprints(fingerprint);
CREATE INDEX IF NOT EXISTS idx_upstream_lookup ON upstream_fingerprints(module_path, def_name, kind, receiver);

-- merge_conflicts kept for schema parity during migration; the SQLite
-- backend does not implement branch/checkout/merge (Category A), so this
-- table stays empty. Will be removed in Phase 4.
CREATE TABLE IF NOT EXISTS merge_conflicts (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    name     TEXT NOT NULL,
    kind     TEXT NOT NULL,
    module   TEXT NOT NULL,
    ours     TEXT NOT NULL,
    theirs   TEXT NOT NULL,
    base     TEXT NOT NULL DEFAULT '',
    resolved INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_def_name ON definitions(name);
CREATE INDEX IF NOT EXISTS idx_def_module ON definitions(module_id);
CREATE INDEX IF NOT EXISTS idx_def_kind ON definitions(kind);
CREATE INDEX IF NOT EXISTS idx_def_hash ON definitions(hash);
CREATE INDEX IF NOT EXISTS idx_ref_from ON refs(from_def);
CREATE INDEX IF NOT EXISTS idx_ref_to ON refs(to_def);
CREATE INDEX IF NOT EXISTS idx_def_exported ON definitions(name, module_id);
CREATE INDEX IF NOT EXISTS idx_def_location ON definitions(module_id, start_line, end_line);
CREATE INDEX IF NOT EXISTS idx_def_source_file ON definitions(source_file);

-- FTS5 with trigram tokenizer: substring-friendly matching over doc + body.
-- Trigram is the right pick for Go source: it collapses camelCase, snake_case,
-- and dotted paths into a single substring index (unicode61's word tokens
-- miss camelCase entirely — `handleEdit` becomes one token, not searchable
-- by "handle" or "edit"). Trigram also matches Dolt-era winze behavior on
-- `_` (treats it as content, not a wildcard — kills the underscore-guard
-- workaround in handleSearch stage-2).
--
-- Storage overhead: ~1-2x the indexed text. For defn-self (~4MB) that adds
-- maybe 2MB; for winze's ~1GB monolith that's ~1GB. Acceptable to start;
-- swap to content='' external-content if needed.
CREATE VIRTUAL TABLE IF NOT EXISTS bodies_fts USING fts5(
    body,
    tokenize='trigram'
);
CREATE VIRTUAL TABLE IF NOT EXISTS definitions_fts USING fts5(
    doc,
    tokenize='trigram'
);

-- Sync triggers: keep the FTS index in lockstep with source tables.
-- bodies.def_id and definitions.id are the FTS rowid — same integer used
-- as the join key in SearchDefinitions.
CREATE TRIGGER IF NOT EXISTS bodies_ai AFTER INSERT ON bodies BEGIN
    INSERT INTO bodies_fts(rowid, body) VALUES (new.def_id, new.body);
END;
CREATE TRIGGER IF NOT EXISTS bodies_ad AFTER DELETE ON bodies BEGIN
    DELETE FROM bodies_fts WHERE rowid = old.def_id;
END;
CREATE TRIGGER IF NOT EXISTS bodies_au AFTER UPDATE ON bodies BEGIN
    UPDATE bodies_fts SET body = new.body WHERE rowid = new.def_id;
END;

CREATE TRIGGER IF NOT EXISTS definitions_ai AFTER INSERT ON definitions BEGIN
    INSERT INTO definitions_fts(rowid, doc) VALUES (new.id, COALESCE(new.doc, ''));
END;
CREATE TRIGGER IF NOT EXISTS definitions_ad AFTER DELETE ON definitions BEGIN
    DELETE FROM definitions_fts WHERE rowid = old.id;
END;
CREATE TRIGGER IF NOT EXISTS definitions_au AFTER UPDATE OF doc ON definitions BEGIN
    UPDATE definitions_fts SET doc = COALESCE(new.doc, '') WHERE rowid = new.id;
END;

CREATE TABLE IF NOT EXISTS comments (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    def_id       INTEGER,
    source_file  TEXT NOT NULL,
    line         INTEGER NOT NULL,
    text         TEXT NOT NULL,
    kind         TEXT NOT NULL,
    pragma_key   TEXT,
    pragma_value TEXT,
    FOREIGN KEY (def_id) REFERENCES definitions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_comment_def ON comments(def_id);
CREATE INDEX IF NOT EXISTS idx_comment_pragma ON comments(pragma_key);
CREATE INDEX IF NOT EXISTS idx_comment_file ON comments(source_file, line);

CREATE TABLE IF NOT EXISTS literal_fields (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    def_id      INTEGER NOT NULL,
    type_name   TEXT NOT NULL,
    field_name  TEXT NOT NULL,
    field_value TEXT NOT NULL,
    line        INTEGER NOT NULL,
    FOREIGN KEY (def_id) REFERENCES definitions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_litfield_type ON literal_fields(type_name);
CREATE INDEX IF NOT EXISTS idx_litfield_field ON literal_fields(field_name);
CREATE INDEX IF NOT EXISTS idx_litfield_def ON literal_fields(def_id);
CREATE INDEX IF NOT EXISTS idx_litfield_type_field ON literal_fields(type_name, field_name);

CREATE TABLE IF NOT EXISTS defn_meta (
    "key"   TEXT PRIMARY KEY,
    "value" TEXT NOT NULL
);

-- Precomputed def summaries — task #151. One row per definition,
-- keyed by def_id. minhash is a fixed-width blob (numHashes × 4 bytes)
-- storing MinHash-32 signatures over 5-char body shingles. Enables
-- sub-linear approximate similarity ("similar" op) without recomputing
-- from bodies at read time. Sparse to keep the schema readable — add
-- more precomputed columns here as follow-ups (one_line_summary,
-- ast_kinds_bag, cyclomatic, etc.) without touching definitions.
CREATE TABLE IF NOT EXISTS def_summaries (
    def_id  INTEGER PRIMARY KEY,
    minhash BLOB,
    FOREIGN KEY (def_id) REFERENCES definitions(id) ON DELETE CASCADE
);
