# Influences & Citations

All dependencies and inspirations are MIT or BSD-2 licensed.
defn is MIT licensed.

## Direct Lineage

- **adit-code** (MIT) — Measured structural properties of source code that
  predict AI editing cost. Validated against SWE-bench agent trajectories
  (1,289 files, 20 repos). Found file size is the strongest baseline predictor
  (Spearman +0.457), with blast radius (+0.819), unnecessary reads (+0.769),
  and grep noise (+0.742) all surviving partial correlation controlling for
  size. defn exists because adit proved the cost is real — and that the root
  cause is storing code as files.

## Code-as-Database Systems

- **Smalltalk image** — Entire runtime is a persistent object database. No
  files. Definitions are individually addressable objects. The original
  proof that code doesn't need to live in files. Limitation: binary image
  format, tooling ecosystem effectively dead.

- **Unison** ([unison-lang.org](https://www.unison-lang.org/), MIT) —
  Content-addressed by AST hash. Names are labels, not identifiers. No files.
  The closest modern system to defn's philosophy. Limitation: Haskell-like
  language only, small community, can't retrofit onto existing languages.

- **Dark** — "Infrastructure as database." Code stored in structured backend,
  edited through a custom UI. Proved the workflow is viable for small teams.
  Shut down; was a full platform, not a composable tool.

- **JetBrains MPS** ([jetbrains.com/mps](https://www.jetbrains.com/mps/),
  Apache 2.0) — Projectional editing. The AST is the source of truth; text
  is a rendered view. Closest to defn's emit model. Limitation: DSL-focused,
  not general-purpose languages.

## Database & Versioning

- **Dolt** ([dolthub.com](https://www.dolthub.com/), Apache 2.0) — Git
  semantics over SQL. Branch, merge, diff on structured data. Considered as
  defn's storage layer before choosing SQLite for simplicity. May revisit
  for native version control.

- **SQLite** ([sqlite.org](https://sqlite.org/), public domain) — defn's
  current storage engine. Single-file, zero-config, WAL mode for concurrent
  reads. The right choice for a tool that runs locally alongside a compiler.

## Parsing & Code Intelligence

- **Tree-sitter** ([tree-sitter.github.io](https://tree-sitter.github.io/tree-sitter/),
  MIT) — Universal incremental parser. Parses any language into a concrete
  syntax tree. defn currently uses `go/ast` for Go, but tree-sitter is the
  path to multi-language support.

- **Sourcegraph SCIP** ([github.com/sourcegraph/scip](https://github.com/sourcegraph/scip),
  Apache 2.0) — Cross-repository code intelligence protocol. Stores
  definitions and references as structured data. Similar goals at the index
  level; defn goes further by making the database the source of truth, not
  just an index.

- **Aider RepoMap** ([aider.chat](https://aider.chat/docs/repomap.html)) —
  Builds a map of definitions and references to help AI agents navigate code.
  Solves the same navigation problem defn solves, but as a layer on top of
  files rather than replacing them.

## Academic

- Borg et al., ["Code for Machines, Not Just Humans,"](https://arxiv.org/abs/2601.02200)
  FORGE/ICSE 2026. CodeHealth predicts AI refactoring success. Validates
  that code structure affects AI agent performance.

- ["Rethinking Code Complexity Through the Lens of Large Language Models,"](https://arxiv.org/abs/2602.07882)
  Feb 2026. After controlling for code length, classical complexity metrics
  show no consistent correlation with LLM performance. Supports definitions
  (not files) as the right unit of code.

- ["Tokenomics: Quantifying Where Tokens Are Used in Agentic Software
  Engineering,"](https://arxiv.org/abs/2601.14470) Jan 2026. Code review
  consumes 59.4% of tokens; input tokens are 53.9% of total. defn reduces
  input tokens by eliminating file-level reads.

- ["LocAgent: Graph-Guided LLM Agents for Code Localization,"](https://arxiv.org/abs/2503.09089)
  ACL 2025. Dependency graph-based localization achieved 92.7% file-level
  accuracy and reduced costs ~86%. Validates that structured references
  (what defn stores natively) beat grep-based navigation.

- ["LoCoBench-Agent,"](https://arxiv.org/abs/2511.13998) Salesforce, Nov
  2025. Negative correlation between thorough file exploration and efficiency.
  Agents that traverse more files spend more tokens. defn eliminates file
  traversal entirely.

## Principles

- Kent C. Dodds, ["Colocation"](https://kentcdodds.com/blog/colocation).
  Keep things that change together close together. In defn, "close" means
  "in the same query result," not "in the same file."

- Carson Gross, ["Locality of Behaviour"](https://htmx.org/essays/locality-of-behaviour/).
  The behaviour of a unit of code should be obvious from that unit alone.
  defn achieves this by making each definition self-contained with explicit
  references.

## Go Tooling Compatibility

defn stores code as definitions in a database, but `defn emit` produces
standard .go files. This means all standard Go tools work on emitted output:

- `go build` / `go test` — compilation and testing
- `golangci-lint` — linting (see `.golangci.yml`)
- `go vet` — static analysis
- `gofmt` / `goimports` — formatting
- `go doc` — documentation

The database is the source of truth; files are a build artifact. But the
artifact is standard Go, so the entire Go ecosystem remains available.
