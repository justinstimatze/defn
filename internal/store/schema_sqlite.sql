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

-- FTS5 virtual tables are deferred to task #137 (tokenizer quality on
-- Go-source strings). Phase 1 uses LIKE-based SearchDefinitions instead.
-- When FTS5 lands, add external-content virtual tables + AFTER
-- INSERT/UPDATE/DELETE triggers on definitions/bodies/comments/
-- literal_fields to keep the FTS index in sync.

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
