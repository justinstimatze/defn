# defn — Natively Executable Code Database

**Status**: Pre-project. Concept emerged from building adit-code, which
measures how expensive file-based source code is for AI agents to navigate.
defn eliminates the problems adit detects by replacing files with a queryable,
executable database.

## Origin

adit-code validated against SWE-bench agent trajectories (541 files, 28 repos)
that file size is the strongest predictor of AI editing cost (Spearman +0.457).
Every other structural metric — grep noise, blast radius, co-location — is
either a size proxy or adds only weak independent signal. The root cause: AI
tools navigate code by grepping and reading text files. Every import is a file
read. Every ambiguous name is a noisy grep. Every large file is multiple
partial reads where the AI loses coherence.

These are symptoms of storing code as text files. In a database, they don't
exist:
- No file size problem — definitions are individually addressable
- No co-location problem — everything is queryable, imports are joins
- No grep ambiguity — `WHERE name = X AND module = Y` is exact
- No blast radius surprise — the references table is explicit
- No import cycles — foreign key constraints prevent them

The name: a **defn** is a definition — the atomic unit of code. adit finds
problems in file-based code. defn eliminates them by making definitions
individually addressable.

## The Idea

Source code is stored as a structured database (dolt, SQLite, or custom) where:
- Each function/method/class/type is a row with its AST, rendered text, and metadata
- References between definitions are explicit rows in a references table
- The runtime reads definitions from the database to execute
- "Compilation" is a materialized view
- Version history is database-native (dolt gives you git semantics over SQL)
- AI agents edit by definition ID, not string replacement

## What Exists Already

| System | Approach | Limitation |
|--------|----------|-----------|
| **Smalltalk image** | Entire runtime is a persistent object database. No files. | Binary image format, tooling ecosystem dead |
| **Unison** | Content-addressed by AST hash. Names are labels. No files. | Haskell-like language only, small community |
| **JetBrains MPS** | Projectional editing. AST is source of truth. | DSL-focused, not general-purpose |
| **Dark** | "Infrastructure as database." Code stored in structured backend. | Shut down, was a full platform not a tool |
| **Dolt** | Git-for-data. SQL database with branch/merge/diff. MIT licensed. | Stores data, not executable code |
| **Tree-sitter** | Universal parser producing concrete syntax trees. | Parse step only, no persistence or execution |

No existing system combines: general-purpose language + database storage +
native execution + AI-agent-optimized access.

## Possible Schema

```sql
CREATE TABLE modules (
    id UUID PRIMARY KEY,
    name TEXT UNIQUE,
    package TEXT
);

CREATE TABLE definitions (
    id UUID PRIMARY KEY,
    module_id UUID REFERENCES modules,
    name TEXT,
    kind ENUM('function', 'method', 'class', 'type', 'constant'),
    signature TEXT,
    body_ast JSONB,       -- the actual code as AST
    body_text TEXT,        -- rendered text (cached projection)
    line_count INT,
    nesting_depth INT,
    created_at TIMESTAMP,
    modified_at TIMESTAMP
);

CREATE TABLE references (
    from_def UUID REFERENCES definitions,
    to_def UUID REFERENCES definitions,
    kind ENUM('call', 'type_ref', 'inherit', 'field_access')
);

CREATE TABLE tests (
    id UUID PRIMARY KEY,
    target_def UUID REFERENCES definitions,
    body_ast JSONB,
    last_result ENUM('pass', 'fail', 'skip'),
    last_run TIMESTAMP
);
```

## How an AI Agent Would Use It

```sql
-- Find the function to edit (no grep, no ambiguity)
SELECT body_text FROM definitions
WHERE name = 'validate_payment' AND module_id = (
    SELECT id FROM modules WHERE name = 'handlers'
);

-- Blast radius (who calls this?)
SELECT d.name, m.name FROM definitions d
JOIN references r ON r.from_def = d.id
JOIN modules m ON d.module_id = m.id
WHERE r.to_def = ? AND r.kind = 'call';

-- Edit by ID, not string replacement
UPDATE definitions SET body_ast = ?, body_text = ?
WHERE id = ?;

-- All tests for this function
SELECT * FROM tests WHERE target_def = ?;
```

No Read tool calls. No Grep tool calls. No file boundaries. No partial reads.
The entire adit problem space disappears.

## Hard Questions

1. **How does execution work?** The runtime needs to load definitions from
   the database into memory. For compiled languages, "compilation" is a
   materialized view that assembles definitions into an executable. For
   interpreted languages, the interpreter reads definitions on demand.

2. **Debugging?** Stack traces need to reference something. In a file-based
   world, that's `file:line`. In a database world, it's `module.function`
   which is arguably better. But every debugger assumes files.

3. **Interop with existing tools?** git, CI, editors, formatters all assume
   files. The transition cost is enormous. A pragmatic approach: the database
   is the source of truth, but files can be projected from it for tooling
   that requires them. `defn emit` produces files; `defn ingest` imports
   them back.

4. **Merge conflicts?** Database-level conflicts (two people edited the same
   definition) are cleaner than text-level conflicts (two people edited
   nearby lines). Dolt handles this natively.

5. **Do you need a new language?** Unison built a new language. That's a huge
   adoption barrier. Could this work with existing languages (Python, TS, Go)
   by storing their ASTs and projecting text? Tree-sitter parses all of them.

6. **Performance?** Reading definitions from a database on every function
   call would be slow. Caching / ahead-of-time compilation is necessary.
   The database is the authoring format, not the execution format.

7. **Who is this for?** AI-only codebases where no human hand-edits source.
   The human reviews diffs (which are database diffs, showing definition
   changes) and approves. The AI never deals with files.

## Possible v0.1

The most pragmatic starting point: a tool that:
1. Ingests a Python project's source files
2. Parses them with tree-sitter into a dolt database
3. Exposes an MCP server with SQL-based tools for AI agents
4. Emits files back out for running the actual code
5. Tracks changes as database diffs (dolt branches/commits)

This avoids the "new runtime" problem entirely — Python runs from emitted
files, but the AI edits the database. The database is the source of truth;
files are a build artifact.

## Relationship to adit-code

adit measures the cost of file-based code for AI agents. defn is the answer
to the question: what if we didn't use files?

adit's validated findings inform defn's design:
- File size (Spearman +0.457) → definitions are individually addressable
- Nesting depth (+0.371) → AST depth is queryable metadata
- Comments help (partial -0.137) → comments are first-class fields on definitions
- Grep noise (+0.233 median) → SQL WHERE eliminates ambiguity
- Import co-location → joins replace imports
