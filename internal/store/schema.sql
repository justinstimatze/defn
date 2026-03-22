-- defn: Code Database Schema
-- Source of truth for Go source code, queryable by AI agents.

CREATE TABLE IF NOT EXISTS modules (
    id         INTEGER PRIMARY KEY,
    path       TEXT UNIQUE NOT NULL,   -- e.g. "github.com/justinstimatze/defn/internal/store"
    name       TEXT NOT NULL           -- e.g. "store"
);

CREATE TABLE IF NOT EXISTS definitions (
    id          INTEGER PRIMARY KEY,
    module_id   INTEGER NOT NULL REFERENCES modules(id),
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL,          -- 'function', 'method', 'type', 'const', 'var', 'interface'
    exported    BOOLEAN NOT NULL,
    receiver    TEXT,                   -- for methods: receiver type name (e.g. "Store", "*Store")
    signature   TEXT,                   -- func signature or type definition header
    body        TEXT NOT NULL,          -- full source text of the definition
    doc         TEXT,                   -- godoc comment
    start_line  INTEGER,               -- line in emitted file (for debugger mapping)
    end_line    INTEGER,
    hash        TEXT NOT NULL,          -- sha256 of body (content-addressing)
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    modified_at TEXT NOT NULL DEFAULT (datetime('now')),

    UNIQUE(module_id, name, kind, receiver)
);

CREATE TABLE IF NOT EXISTS references (
    from_def  INTEGER NOT NULL REFERENCES definitions(id),
    to_def    INTEGER NOT NULL REFERENCES definitions(id),
    kind      TEXT NOT NULL,            -- 'call', 'type_ref', 'embed', 'field_access', 'impl'

    PRIMARY KEY (from_def, to_def, kind)
);

CREATE TABLE IF NOT EXISTS imports (
    module_id       INTEGER NOT NULL REFERENCES modules(id),
    imported_path   TEXT NOT NULL,       -- the import path
    alias           TEXT,                -- local alias if renamed

    PRIMARY KEY (module_id, imported_path)
);

-- Versioning: each commit is a snapshot of all definition hashes.
CREATE TABLE IF NOT EXISTS commits (
    id          INTEGER PRIMARY KEY,
    parent_id   INTEGER REFERENCES commits(id),
    root_hash   TEXT NOT NULL,           -- hash of all definition hashes (merkle root)
    message     TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Fast lookups
CREATE INDEX IF NOT EXISTS idx_def_name ON definitions(name);
CREATE INDEX IF NOT EXISTS idx_def_module ON definitions(module_id);
CREATE INDEX IF NOT EXISTS idx_def_kind ON definitions(kind);
CREATE INDEX IF NOT EXISTS idx_def_hash ON definitions(hash);
CREATE INDEX IF NOT EXISTS idx_ref_from ON references(from_def);
CREATE INDEX IF NOT EXISTS idx_ref_to ON references(to_def);
