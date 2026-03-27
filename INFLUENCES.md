# Influences & Citations

All dependencies and inspirations are Apache 2.0, MIT, or BSD-2 licensed unless noted.
defn is Apache 2.0 licensed.

## Direct Lineage

- **[adit-code](https://github.com/justinstimatze/adit-code)** (Apache 2.0) — Measured structural
  properties of source code that predict AI editing cost. Validated against
  SWE-bench agent trajectories (1,840 files, 49 repos). Blast radius is the
  strongest predictor (median Spearman +0.474, positive on all 49 repos).
  defn exists because adit proved the cost is real — and that the root cause
  is storing code as files.

## Code-as-Database Systems

- **Smalltalk image** (Xerox PARC, 1972-1983) — Entire runtime is a persistent
  object database. No files. Definitions are individually addressable objects.
  The original proof that code doesn't need to live in files. Limitation:
  binary image format, tooling ecosystem effectively dead.

- **Unison** ([unison-lang.org](https://www.unison-lang.org/), MIT, 1.0
  released 2024) — Content-addressed by AST hash. Names are labels, not
  identifiers. No files. The closest modern system to defn's core idea —
  code stored in a database, definitions as the unit, content hashing for
  identity. Limitation: requires a new Haskell-like language; can't retrofit
  onto existing languages. defn applies the same storage model to existing Go.

- **Dark** ([darklang.com](https://darklang.com/), now open-sourcing) —
  "Infrastructure as database." Code stored in structured backend, deployed
  in 50ms, edited through a custom browser-based editor. Proved the workflow
  is viable for a real product. Was a closed platform, not a composable tool.

- **JetBrains MPS** ([jetbrains.com/mps](https://www.jetbrains.com/mps/),
  Apache 2.0) — Projectional editing. The AST is the source of truth (stored
  as XML); text is a rendered view. Closest to defn's emit model. The most
  mature projectional editor. Limitation: DSL-focused, not general-purpose
  languages. No AI agent interface.

## Code-as-Queryable-Database

- **CodeQL** ([codeql.github.com](https://codeql.github.com/), proprietary
  with free tiers) — Semmle, acquired by GitHub 2019. Extracts a codebase
  into a relational database (AST, control flow, data flow, call graph).
  Queries written in QL (Datalog-inspired). The most direct precedent for
  "code as a relational database." Limitation: read-only analysis artifact.
  Files remain the source of truth. No write path, no versioning, no merge.

- **Google Kythe** ([kythe.io](https://kythe.io/)) — Language-agnostic graph
  of source code relationships. Powers Google's internal code search over
  billions of lines. Stores exactly the kind of reference graph defn stores.
  Limitation: read-only index derived from files.

- **Codebase-Memory-MCP** ([github.com/deusdata/codebase-memory-mcp](https://github.com/deusdata/codebase-memory-mcp))
  — MCP server that indexes source code into a SQLite knowledge graph
  (functions, classes, call chains). 11 MCP tools. Uses tree-sitter.
  Strikingly similar to defn's MCP layer. Limitation: read-only index.
  defn's write path (update_definition, emit, merge) is the key difference.

## Database & Versioning

- **Dolt** ([dolthub.com](https://www.dolthub.com/), Apache 2.0) — Git
  semantics over SQL. Branch, merge, diff, pull requests on structured data.
  defn uses Dolt as its storage engine, getting native branch/merge/diff/commit
  semantics on structured code data. Steve Yegge's
  [Wasteland/Gas Town](https://medium.com/@steve.yegge) (March 2026) uses Dolt
  as the foundation for a federated AI agent work marketplace, validating that
  SQL + git semantics is the right substrate for AI agent coordination. Yegge:
  "Dolt predicted the Wasteland, because there could not be a more perfect
  technology for it."

## Parsing & Code Intelligence

- **gopls** ([golang.org/x/tools/gopls](https://pkg.go.dev/golang.org/x/tools/gopls),
  BSD-3-Clause) — The official Go language server. Uses `go/types` for
  type-checked analysis — the same type checker defn uses for reference
  resolution. gopls answers the same navigation questions defn answers, but
  as an on-demand in-memory service over LSP. It does not persist the
  reference graph, does not support definition-level merge or diff, and
  cannot compose blast radius + test coverage + transitive callers into a
  single query.

- **Tree-sitter** ([tree-sitter.github.io](https://tree-sitter.github.io/tree-sitter/),
  MIT) — Universal incremental parser. defn currently uses `go/ast` for Go;
  tree-sitter is the path to multi-language support.

- **Sourcegraph SCIP** ([github.com/sourcegraph/scip](https://github.com/sourcegraph/scip),
  Apache 2.0) — Cross-repository code intelligence protocol. Stores
  definitions and references as structured data. Similar goals at the index
  level; defn goes further by making the database the source of truth.

- **Aider RepoMap** ([aider.chat](https://aider.chat/docs/repomap.html)) —
  Builds a map of definitions and references to help AI agents navigate code.
  Same navigation problem, but as a layer on top of files.

- **SemanticMerge** ([plasticscm.com/semanticmerge](https://www.plasticscm.com/semanticmerge),
  Codice Software, 2013) — Diffs and merges based on code structure (classes,
  methods) rather than text lines. Solves the same merge problem as defn but
  as a layer on top of files, re-parsing each time. defn stores definitions
  persistently, making merge a hash comparison.

## Test Impact Analysis

Build systems and CI platforms solve test impact at varying granularity.
defn's reference graph enables function-level targeting — finer grained
than package-level tools, without requiring coverage instrumentation.

- **Bazel** ([bazel.build](https://bazel.build/), Apache 2.0) — Build-level
  dependency graph. Only rebuilds and retests affected build targets. The
  industry standard for large monorepos. Operates at target/package
  granularity, not function level.

- **Pants** ([pantsbuild.org](https://www.pantsbuild.org/), Apache 2.0) —
  Similar to Bazel. Fine-grained dependency-aware build system. Package-level
  test selection.

- **Nx** ([nx.dev](https://nx.dev/), MIT) — Monorepo build system with
  "affected" command that runs only tests for changed projects. Package/project
  granularity.

- **Harness Test Intelligence** ([harness.io](https://www.harness.io/)) —
  CI platform with ML-based test selection from code changes. File-level
  granularity.

- **Symflower** ([symflower.com](https://symflower.com/en/company/blog/2024/test-impact-analysis/))
  — Go-specific test impact at package granularity. Uses git diff + import
  graph. Average 29% reduction in test time.

- **Datadog Test Impact Analysis** ([docs.datadoghq.com](https://docs.datadoghq.com/tests/test_impact_analysis/setup/go/))
  — CI service using coverage instrumentation per test. Test-function
  granularity but requires Datadog and a coverage run.

defn's approach: the reference graph is pre-computed and persistent in
Dolt. "Which tests call this definition?" is a SQL query traversing the
`references` table — no coverage instrumentation, no build graph rebuild,
no CI service. Function-level, not package-level.

## Autonomous Software Engineering

- **DARPA MUSE** (Mining and Understanding Software Enclaves) — Built a
  persistent graph database of code facts/inferences from billions of lines
  of open source. The closest DARPA analog to "a code database."

- **DARPA BRASS** (Building Resource Adaptive Software Systems) — 4-year
  program for software that survives 100+ years by automatically adapting
  to ecosystem changes. The closest DARPA analog to the self-healing vision.

- **DARPA AIxCC** (AI Cyber Challenge, 2023-2025) — LLM-based autonomous
  vulnerability detection/patching. Final results at DEF CON 33: teams
  processed 54M lines, found 18 real zero-days, patched 68% of synthetic
  vulnerabilities. ([darpa.mil/news/2025/aixcc-results](https://www.darpa.mil/news/2025/aixcc-results))

- **DARPA Cyber First Aid** — Uses LLMs to generate patches, formal methods
  to verify safety, then hotpatches directly into running memory.

- **SWE-agent** (Yang et al., NeurIPS 2024,
  [github.com/SWE-agent/SWE-agent](https://github.com/SWE-agent/SWE-agent))
  — Standard benchmark agent for autonomous code repair. Operates entirely
  on file-based repos.

- **AutoCodeRover** (Zhang et al., ISSTA 2024,
  [arxiv.org/abs/2404.05427](https://arxiv.org/abs/2404.05427)) — Uses
  program structure (search by class/method name) rather than text search.
  Key insight aligns with defn: structure beats grep.

- **RepairAgent** (Bouzenia et al., ICSE 2025,
  [arxiv.org/abs/2403.17134](https://arxiv.org/abs/2403.17134)) — First
  fully autonomous LLM-based agent for program repair.

- **Live-SWE-agent** (2025,
  [arxiv.org/abs/2511.13646](https://arxiv.org/abs/2511.13646)) — First
  self-evolving software agent. Starts with minimal tools and recursively
  improves its own scaffold. 77.4% on SWE-bench Verified.

- **Facebook SapFix/Getafix** (ICSE-SEIP 2019, OOPSLA 2019) — First
  production-deployed automated bug fixing. Learns fix patterns from past
  human fixes.

## MCP Context Optimization

- **Dynamic Context Loading** (Moncef Abboud, 2026,
  [cefboud.com](https://cefboud.com/posts/dynamic-context-loading-llm-mcp/))
  — Tiered tool activation: loader tool with summaries → on-demand schema
  loading → full tool execution. defn uses a single `code` tool with an
  `op` dispatch field instead of 17 separate tools, reducing context
  overhead from ~500 tokens of tool schemas to ~80.

- **Claude Tool Search** (Anthropic, 2025-2026) — Deferred tool loading
  in Claude Code. Tools marked as deferred are not loaded into context
  until explicitly searched for. defn's single-tool approach eliminates
  the need for deferred loading entirely.

## Multi-Agent Code Systems

- **ChatDev** (Qian et al., ACL 2024,
  [arxiv.org/abs/2307.07924](https://arxiv.org/abs/2307.07924)) — Virtual
  software company with 7 specialized LLM agents collaborating on code.

- **MetaGPT** (Hong et al., ICLR 2024) — Multi-agent framework following
  software engineering SOPs.

- **Claude Code Agent Teams** (Anthropic, Feb 2026) — Peer-to-peer agent
  communication. Multiple independent Claude sessions that coordinate,
  message each other, claim tasks, and divide work in parallel. The
  infrastructure that makes defn's parallel agent workflow practical.

- **Cursor Background Agents** (Cursor, 2025-2026) — Background agents
  that run autonomously, making changes and creating PRs. Validates
  "agents run continuously" as a shipping commercial product.

## Academic

- Borg et al., ["Code for Machines, Not Just Humans,"](https://arxiv.org/abs/2601.02200)
  FORGE/ICSE 2026. CodeHealth predicts AI refactoring success.

- ["Rethinking Code Complexity Through the Lens of Large Language Models,"](https://arxiv.org/abs/2602.07882)
  Feb 2026. After controlling for code length, classical complexity metrics
  show no consistent correlation with LLM performance.

- ["Tokenomics: Quantifying Where Tokens Are Used in Agentic Software
  Engineering,"](https://arxiv.org/abs/2601.14470) Jan 2026. Code review
  consumes 59.4% of tokens; input tokens are 53.9% of total.

- ["LocAgent: Graph-Guided LLM Agents for Code Localization,"](https://arxiv.org/abs/2503.09089)
  ACL 2025. Graph-based localization reduced costs ~86%.

- ["LoCoBench-Agent,"](https://arxiv.org/abs/2511.13998) Salesforce, Nov
  2025. Agents that traverse more files spend more tokens.

- Allamanis et al., ["Learning Natural Coding Conventions,"](https://dl.acm.org/doi/10.1145/2635868.2635883)
  FSE 2014. First tool to learn coding conventions from a codebase using
  NLP. Foundation for the "self-improving: learn conventions" aspect.

- ["SE 3.0: The Rise of AI Teammates,"](https://arxiv.org/abs/2507.15003)
  2025. Frames the current era as autonomous AI agents operating at the
  task level — the paradigm defn is built for.

- ["Self-Healing Software Systems: Lessons from Nature, Powered by AI,"](https://arxiv.org/abs/2504.20093)
  2025. Framework for AI-driven self-healing. Proposes observability +
  diagnosis + repair agents.

- GenProg (Le Goues et al., IEEE TSE 2012) — Foundational work on
  automated program repair using genetic programming.

- ["CoreCodeBench,"](https://arxiv.org/abs/2507.05281) 2025. Validates
  function-level granularity as the right unit for code intelligence tasks.

## Principles

- Kent C. Dodds, ["Colocation"](https://kentcdodds.com/blog/colocation).
  In defn, "close" means "in the same query result," not "in the same file."

- Carson Gross, ["Locality of Behaviour"](https://htmx.org/essays/locality-of-behaviour/).
  defn achieves this by making each definition self-contained with explicit
  references.

- Robert C. Martin, *Clean Code* Ch. 2, "Use Searchable Names." The
  qualitative advice that defn's SQL WHERE eliminates the need for.

## Go Tooling Compatibility

defn stores code as definitions in a database, but `defn emit` produces
standard .go files. This means all standard Go tools work on emitted output:

- `go build` / `go test` — compilation and testing
- `golangci-lint` — linting (see `.golangci.yml`)
- `go vet` — static analysis
- `gofmt` / `goimports` — formatting (goimports required for emit)
- `go doc` — documentation

The database is the source of truth; files are a build artifact. But the
artifact is standard Go, so the entire Go ecosystem remains available.
