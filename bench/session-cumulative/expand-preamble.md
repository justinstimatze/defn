# Expand-arm preamble

Appended via `--append-system-prompt` to the defn-arm. Companion to
`.mcp.json` — same MCP config, just adds strong prescription for the
`expand` op.

Rationale: prior session bench (2026-07-11) showed defn used
additively, not as substitute. Prompt tuning on `apply` was exhausted
(+28% best case). Root cause: multi-hop read patterns
(read→impact→read) cost 3× turn count. Expand collapses these into
one tool call.

---

**MANDATORY: Use `code(op:"expand")` for multi-hop reads.**

Whenever you would run two or more of these in sequence on the same
definition, use ONE `expand` call instead:

- `code(op:"read", name:X)` followed by `code(op:"impact", name:X)`
- `code(op:"impact", name:X)` followed by `code(op:"read", name:Y)`
  where Y is any caller returned by impact
- `code(op:"read", name:X)` followed by another `read` on a caller
  or callee of X

The replacement is:

    code(op:"expand", name:X, include:["body", "callers"])

`include:` values available in v1: `body`, `callers`. Default (empty
`include:`) is `["body", "callers"]`.

**Explicit anti-patterns to eliminate:**

- Don't call `read` then `impact` on the same def — use `expand`.
- Don't call `impact` then `read` on any caller it returned — the
  caller list already tells you who's affected.
- Don't call `read` twice on defs in the same call-graph neighborhood
  when you're trying to "understand the code" — reach for `expand`.

**Every avoided round-trip saves the entire accumulated cache-read
prefix from being re-billed.** Turn count is the dominant cost
driver in a multi-turn session — one `expand` beats N `read`s on
weighted cost.
