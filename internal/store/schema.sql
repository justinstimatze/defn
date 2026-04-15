-- defn: Code Database Schema (Dolt)
-- Source of truth for Go source code, queryable by AI agents.
-- Versioning (branch, merge, diff, commit) handled by Dolt natively.

CREATE TABLE IF NOT EXISTS modules (
    id         INT PRIMARY KEY AUTO_INCREMENT,
    path       VARCHAR(500) UNIQUE NOT NULL,
    name       VARCHAR(255) NOT NULL,
    doc        TEXT
);

CREATE TABLE IF NOT EXISTS definitions (
    id          INT PRIMARY KEY AUTO_INCREMENT,
    module_id   INT NOT NULL,
    name        VARCHAR(255) NOT NULL,
    kind        VARCHAR(50) NOT NULL,
    exported    BOOLEAN NOT NULL,
    test        BOOLEAN NOT NULL DEFAULT FALSE,
    receiver    VARCHAR(255),
    signature   TEXT,
    doc         TEXT,
    start_line  INT,
    end_line    INT,
    source_file VARCHAR(500) DEFAULT '',
    hash        VARCHAR(64) NOT NULL,
    UNIQUE(module_id, name, kind, receiver, test),
    FOREIGN KEY (module_id) REFERENCES modules(id)
);

-- Bodies stored separately so metadata queries skip large text blobs.
CREATE TABLE IF NOT EXISTS bodies (
    def_id  INT PRIMARY KEY,
    body    LONGTEXT NOT NULL,
    FOREIGN KEY (def_id) REFERENCES definitions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS refs (
    from_def  INT NOT NULL,
    to_def    INT NOT NULL,
    kind      VARCHAR(50) NOT NULL,
    PRIMARY KEY (from_def, to_def, kind),
    FOREIGN KEY (from_def) REFERENCES definitions(id),
    FOREIGN KEY (to_def) REFERENCES definitions(id)
);

CREATE TABLE IF NOT EXISTS imports (
    module_id       INT NOT NULL,
    imported_path   VARCHAR(500) NOT NULL,
    alias           VARCHAR(255),
    PRIMARY KEY (module_id, imported_path),
    FOREIGN KEY (module_id) REFERENCES modules(id)
);

-- Project-level files that must survive the round-trip (go.mod, go.sum, etc.).
CREATE TABLE IF NOT EXISTS project_files (
    path     VARCHAR(500) PRIMARY KEY,
    content  LONGTEXT NOT NULL
);

-- Pending merge conflicts. Populated by Dolt merge, surfaced by defn.
CREATE TABLE IF NOT EXISTS merge_conflicts (
    id         INT PRIMARY KEY AUTO_INCREMENT,
    name       VARCHAR(255) NOT NULL,
    kind       VARCHAR(50) NOT NULL,
    module     VARCHAR(500) NOT NULL,
    ours       LONGTEXT NOT NULL,
    theirs     LONGTEXT NOT NULL,
    base       LONGTEXT NOT NULL DEFAULT '',
    resolved   BOOLEAN NOT NULL DEFAULT FALSE
);

-- Indexes
CREATE INDEX idx_def_name ON definitions(name);
CREATE INDEX idx_def_module ON definitions(module_id);
CREATE INDEX idx_def_kind ON definitions(kind);
CREATE INDEX idx_def_hash ON definitions(hash);
CREATE INDEX idx_ref_from ON refs(from_def);
CREATE INDEX idx_ref_to ON refs(to_def);
CREATE INDEX idx_def_exported ON definitions(name, module_id);
CREATE INDEX idx_def_location ON definitions(module_id, start_line, end_line);
CREATE INDEX idx_def_source_file ON definitions(source_file);
CREATE FULLTEXT INDEX idx_def_doc_ft ON definitions(doc);
CREATE FULLTEXT INDEX idx_body_ft ON bodies(body);

-- Comments and pragmas extracted from Go source files.
CREATE TABLE IF NOT EXISTS comments (
    id          INT PRIMARY KEY AUTO_INCREMENT,
    def_id      INT,
    source_file VARCHAR(500) NOT NULL,
    line        INT NOT NULL,
    text        TEXT NOT NULL,
    kind        VARCHAR(50) NOT NULL,
    pragma_key  VARCHAR(255),
    pragma_value TEXT,
    FOREIGN KEY (def_id) REFERENCES definitions(id) ON DELETE CASCADE
);
CREATE INDEX idx_comment_def ON comments(def_id);
CREATE INDEX idx_comment_pragma ON comments(pragma_key);
CREATE INDEX idx_comment_file ON comments(source_file, line);
CREATE FULLTEXT INDEX idx_comment_text_ft ON comments(text);

-- Composite literal field values extracted during resolve.
CREATE TABLE IF NOT EXISTS literal_fields (
    id          INT PRIMARY KEY AUTO_INCREMENT,
    def_id      INT NOT NULL,
    type_name   VARCHAR(500) NOT NULL,
    field_name  VARCHAR(255) NOT NULL,
    field_value TEXT NOT NULL,
    line        INT NOT NULL,
    FOREIGN KEY (def_id) REFERENCES definitions(id) ON DELETE CASCADE
);
CREATE INDEX idx_litfield_type ON literal_fields(type_name);
CREATE INDEX idx_litfield_field ON literal_fields(field_name);
CREATE INDEX idx_litfield_def ON literal_fields(def_id);
CREATE INDEX idx_litfield_type_field ON literal_fields(type_name, field_name);
CREATE FULLTEXT INDEX idx_litfield_value_ft ON literal_fields(field_value);
