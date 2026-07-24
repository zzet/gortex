# Codebase Structure

**Analysis Date:** 2026-07-24

## Directory Layout

```
gortex/
├── cmd/gortex/                   # CLI entry point and command handlers
│   ├── main.go                   # Build-time version injection, panic handler
│   ├── root.go                   # Cobra root command, telemetry, pre/post-run hooks
│   ├── daemon.go                 # `gortex daemon` (start/stop long-lived process)
│   ├── mcp.go                    # `gortex mcp` (stdio MCP relay)
│   ├── track.go / untrack.go     # Repository management
│   ├── query.go                  # `gortex query` / `gortex call` (query dispatcher)
│   ├── analyze.go                # `gortex analyze` (trigger analyzers on-demand)
│   ├── review.go / audit.go      # PR review, code audit tools
│   ├── clones.go / memory.go     # Clone detection, session memory
│   └── *_test.go                 # Command integration tests
├── internal/
│   ├── daemon/                   # Long-lived process management
│   │   ├── client.go             # IPC client (dial, send, recv)
│   │   ├── federation.go         # Multi-repo graph merging
│   │   ├── autostart.go          # Daemon lifecycle (start, stop, reload)
│   │   └── *.go                  # Event handling, graph persistence
│   ├── parser/                   # Syntax extraction (tree-sitter backed)
│   │   ├── parser.go             # Registry + dispatch logic
│   │   ├── languages/            # Per-language extractors
│   │   │   ├── go.go             # Go parser
│   │   │   ├── python.go         # Python parser
│   │   │   ├── typescript.go     # TypeScript parser
│   │   │   └── ... (100+ more)
│   │   ├── crashpool/            # Crash isolation (SIGSEGV quarantine)
│   │   └── *.go                  # Parse result objects, AST wrappers
│   ├── indexer/                  # File walk → graph mutations
│   │   ├── indexer.go            # Main indexing loop (full + incremental)
│   │   ├── contract_bridge.go    # Extract contracts (signatures)
│   │   ├── capability_edges.go   # Synthesize environment reads, process execs
│   │   ├── clones.go             # Clone detection indexing
│   │   ├── content_*.go          # Content extraction (eliding, splitting)
│   │   └── *.go                  # Per-pass indexing stages
│   ├── resolver/                 # Semantic symbol binding
│   │   ├── backend_resolver.go   # Interface for language-specific resolvers
│   │   ├── cross_repo_edges.go   # Inter-repo call resolution
│   │   ├── class_hierarchy.go    # OOP dispatch binding
│   │   ├── cpp_overload.go       # C++ function overload resolution
│   │   └── ... (many language-specific resolvers)
│   ├── graph/                    # In-memory + persisted graph
│   │   ├── graph.go              # Core data structure (sharded nodes + edges)
│   │   ├── node.go               # Node type definitions
│   │   ├── edge.go               # Edge type definitions
│   │   ├── store.go              # SQLite persistence wrapper
│   │   ├── store_sqlite/         # SQLite schema + queries
│   │   └── ... (overlay, projection, mutation logic)
│   ├── query/                    # Graph traversal
│   │   ├── engine.go             # Symbol search, type lookup
│   │   ├── subgraph.go           # Query result packaging
│   │   ├── walk.go               # BFS/DFS traversal utilities
│   │   └── ... (class hierarchy, closure, etc.)
│   ├── analysis/                 # On-demand graph analysis (pluggable)
│   │   ├── deadcode.go           # Dead code detection
│   │   ├── clones.go             # Clone detection (MinHash + LSH)
│   │   ├── cycles.go             # Cycle detection
│   │   ├── coverage.go           # Code coverage analysis
│   │   ├── communities.go        # Community detection (Leiden algorithm)
│   │   ├── architecture.go       # Architectural layers
│   │   └── ... (30+ other analyzers)
│   ├── mcp/                      # Model Context Protocol (tool registry + handlers)
│   │   ├── manifest.go           # Tool definitions + schema
│   │   ├── tools_search.go       # `search_symbols` tool
│   │   ├── tools_usages.go       # `find_usages` tool
│   │   ├── tools_analyze*.go     # Analyzer tools (analyze kind=deadcode, etc.)
│   │   ├── session_ctx.go        # Per-session state (overlays, memories)
│   │   ├── change_contract.go    # Change verification (guards, architecture)
│   │   └── ... (180+ tools)
│   ├── server/                   # HTTP server + dashboard
│   │   ├── handler.go            # Main HTTP handler (routes, JSON marshalling)
│   │   ├── dashboard.go          # Embedded dashboard (HTML + API)
│   │   ├── conversations.go      # Streaming results over HTTP
│   │   └── *.go                  # CORS, auth, subgraph endpoints
│   ├── search/                   # Full-text indexing
│   │   ├── trigram/              # Trigram index (FTS without full PostgreSQL)
│   │   └── *.go                  # Text normalization
│   ├── contracts/                # Type/signature contracts
│   │   ├── contracts.go          # Contract node definitions
│   │   └── *.go                  # Extraction, matching
│   ├── config/                   # Configuration management
│   │   ├── config.go             # Config struct + defaults
│   │   ├── indexconfig.go        # Index-time options (workers, excludes)
│   │   └── *.go                  # Artifact declarations, LLM providers
│   ├── persistence/              # State management
│   │   ├── sidecar.go            # SQLite ledger for CLI event tracing
│   │   ├── graph_checkpoint.go   # Save/load graph snapshots
│   │   └── *.go                  # Config persistence
│   ├── progress/                 # Terminal progress UI
│   │   ├── progress.go           # Spinner, progress bar
│   │   └── *.go                  # Terminal control
│   ├── semantic/                 # Semantic analysis (scopes, types)
│   │   └── *.go                  # Binding table, type inference
│   ├── embedding/                # Vector embeddings (optional)
│   │   └── *.go                  # Embedding APIs, chunking
│   ├── astquery/                 # AST pattern matching
│   │   └── *.go                  # Tree-sitter query language bindings
│   ├── excludes/                 # File exclusion logic
│   │   └── *.go                  # .gitignore parsing, path matching
│   ├── hooks/                    # Pre/post-index hooks (git, npm, etc.)
│   │   └── *.go                  # Hook executors
│   ├── reach/                    # Reachability analysis
│   │   └── *.go                  # Dominance frontiers, dataflow
│   ├── dataflow/                 # Taint analysis
│   │   └── *.go                  # Value flow tracking
│   ├── tui/                      # Terminal UI (status display)
│   │   └── *.go                  # TUI rendering
│   ├── platform/                 # OS-specific paths
│   │   ├── platform.go           # Home dir layout (~/.gortex/)
│   │   ├── fdlimit.go            # File descriptor limits
│   │   └── *.go                  # Platform detection
│   ├── telemetry/                # Usage tracking (opt-in)
│   │   └── *.go                  # Event recording, consent gating
│   ├── lint/                     # AST linting patterns
│   │   └── *.go                  # Pattern definitions
│   ├── review/                   # Code review helpers
│   │   └── *.go                  # PR diff analysis, verdict logic
│   ├── audit/                    # Audit trails + compliance
│   │   └── *.go                  # Change logging, CFO audit
│   ├── llm/                      # LLM provider integrations
│   │   └── *.go                  # OpenAI, Anthropic, local llama.cpp
│   ├── gitcmd/                   # Git command wrappers
│   │   └── *.go                  # `git diff`, `git log`, etc.
│   ├── workspace/                # Multi-workspace management
│   │   └── *.go                  # Workspace list, switch
│   ├── eval/                     # Evaluation framework (benchmarking)
│   │   └── *.go                  # Benchmark runners, result analysis
│   ├── skill/                    # Skill/agent definitions
│   │   └── *.go                  # Skill parsing, validation
│   ├── version/                  # Version info
│   │   └── *.go                  # Semver parsing, changelog
│   ├── wiki/                     # Knowledge base (artifacts, docs)
│   │   └── *.go                  # Artifact indexing, rendering
│   └── ... (50+ other packages)
├── pkg/gortex/                   # Public Go API
│   └── api.go                    # Exported Engine, New(), Index(), Query()
├── bench/                        # Benchmarks (not unit tests)
│   ├── cmd/                      # Benchmark runners
│   ├── data/                     # Benchmark fixtures (repos)
│   └── *.go                      # Benchmark suites
├── eval/                         # Evaluation data + scripts
│   ├── swe/                      # SWE (software engineer) evaluations
│   ├── agents/                   # Agent evaluation suites
│   └── *.py/*.sh                 # Analysis scripts
├── docs/                         # Documentation
│   ├── cli.md                    # CLI verb reference
│   ├── mcp.md                    # MCP tool reference
│   ├── schema.md                 # Graph schema (node/edge kinds)
│   ├── architecture.md           # High-level design
│   └── ... (50+ docs)
├── examples/                     # Code examples
│   └── *.go                      # Go API usage examples
├── scripts/                      # Build / release scripts
│   ├── build.sh                  # Compile with version injection
│   ├── test.sh                   # Test runner
│   └── release.sh                # Release automation
├── assets/                       # Static assets (dashboard, etc.)
│   ├── dashboard/                # HTML/JS/CSS for web UI
│   └── ...
├── .planning/codebase/           # This analysis (generated by gsd-map-codebase)
│   ├── ARCHITECTURE.md           # This file
│   ├── STRUCTURE.md              # Directory layout guide
│   └── ...
├── go.mod / go.sum               # Go dependencies
├── Makefile                      # Build targets
├── .golangci.yaml                # Linter config
├── .gortex.yaml                  # Gortex's own config (self-referential)
└── README.md                     # Project overview
```

## Directory Purposes

**cmd/gortex/:**
- Purpose: CLI entry point and command handlers
- Contains: Cobra command definitions, daemon control, subcommand execution
- Key files: `main.go`, `root.go`, `daemon.go`, `mcp.go`

**internal/daemon/:**
- Purpose: Long-lived process management, graph lifecycle, IPC server
- Contains: Socket listener, repository tracking, graph persistence
- Key files: `client.go`, `federation.go`, `autostart.go`

**internal/parser/:**
- Purpose: Convert source code into syntax trees; 100+ language support via tree-sitter
- Contains: Language extractors, crash isolation, AST wrappers
- Key files: `parser.go`, `languages/*.go`, `crashpool/*`

**internal/indexer/:**
- Purpose: Walk files and create graph nodes/edges from parsed syntax
- Contains: Full-index and incremental-reindex logic, contract extraction, clone detection
- Key files: `indexer.go`, `contract_bridge.go`, `capability_edges.go`, `clones.go`

**internal/resolver/:**
- Purpose: Semantic symbol binding (match references to definitions across repos/languages)
- Contains: Language-specific resolvers, class hierarchies, cross-repo edges
- Key files: Per-language resolvers (`bare_name_scope_bind.go`, `cpp_overload.go`, etc.)

**internal/graph/:**
- Purpose: In-memory sharded graph + SQLite persistence
- Contains: Node/edge storage, adjacency lists, concurrency control
- Key files: `graph.go`, `node.go`, `edge.go`, `store.go`, `store_sqlite/`

**internal/query/:**
- Purpose: Traverse the graph to answer questions (dependencies, callers, etc.)
- Contains: Symbol search, type lookup, BFS/DFS traversal, closure computation
- Key files: `engine.go`, `subgraph.go`, `walk.go`

**internal/analysis/:**
- Purpose: Compute graph properties on demand (dead code, clones, cycles, etc.)
- Contains: ~30 pluggable analyzers, each with its own algorithm
- Key files: `deadcode.go`, `clones.go`, `cycles.go`, `coverage.go`, etc.

**internal/mcp/:**
- Purpose: Model Context Protocol implementation (180+ tools)
- Contains: Tool definitions, request handlers, result marshalling
- Key files: `manifest.go`, `tools_search.go`, `tools_analyze*.go`, `session_ctx.go`

**internal/server/:**
- Purpose: HTTP server (dashboard, REST endpoints)
- Contains: Request routing, JSON marshalling, CORS, authentication
- Key files: `handler.go`, `dashboard.go`

**internal/search/:**
- Purpose: Full-text indexing via trigram (FTS alternative)
- Contains: Trigram index construction, query parsing, text normalization
- Key files: `trigram/`, `text_*.go`

**internal/semantic/:**
- Purpose: Semantic analysis (scopes, type inference, binding tables)
- Contains: Symbol binding logic, scope resolution
- Key files: Per-language semantic modules

**internal/config/:**
- Purpose: Configuration management (from YAML + environment)
- Contains: Config structs, defaults, artifact declarations
- Key files: `config.go`, `indexconfig.go`

**pkg/gortex/:**
- Purpose: Stable public Go API (for embedding Gortex in other tools)
- Contains: `Engine` struct, `New()`, `Index()`, query methods
- Key files: `api.go`

## Key File Locations

**Entry Points:**
- `cmd/gortex/main.go` — Binary entry point, version injection
- `internal/daemon/client.go` — IPC client (commands dial daemon)
- `internal/server/handler.go` — HTTP handler (editor extensions hit this)
- `cmd/gortex/mcp.go` — MCP relay (stdio JSON-RPC)

**Configuration:**
- `.gortex.yaml` — Gortex's own config (self-referential use of the tool)
- `go.mod` — Dependency list (tree-sitter, zap, etc.)
- `.golangci.yaml` — Linter rules
- `Makefile` — Build targets

**Core Logic:**
- `internal/graph/graph.go` — Sharded in-memory graph (152k lines)
- `internal/graph/store.go` — SQLite persistence wrapper
- `internal/indexer/indexer.go` — Full/incremental index logic
- `internal/query/engine.go` — Symbol search + traversal
- `internal/resolver/` — Per-language symbol binding

**Testing:**
- `internal/graph/graph_test.go` — Graph mutation and concurrency tests
- `internal/indexer/` — Language extraction test fixtures
- `internal/query/` — Query result validation tests
- `cmd/gortex/` — CLI integration tests

## Naming Conventions

**Files:**
- `<verb>.go` — Command handler (e.g., `track.go`, `analyze.go`)
- `<noun>_test.go` — Test file for `<noun>.go`
- `tools_<name>.go` — MCP tool implementation (e.g., `tools_search.go`)
- `<name>_<aspect>.go` — Specialized implementation (e.g., `deadcode.go`, `deadcode_external.go`)

**Directories:**
- `internal/` — Internal packages (not part of public API)
- `cmd/` — Executable entry points
- `pkg/` — Public packages (safe to import from outside repo)
- `bench/` / `eval/` — Evaluation/testing infrastructure
- `docs/` / `examples/` — Documentation and samples

**Functions:**
- Exported (capitalized): `New()`, `Index()`, `Search()`
- Unexported (lowercase): `shardIdx()`, `hashEdgeKey()`
- Test functions: `Test<Name>()` with subtests via `t.Run()`

**Types:**
- Exported: `Graph`, `Node`, `Edge`, `Engine`
- Unexported: `shardMutex`, `edgeKey`, `subGraph`
- Interfaces: `Resolver`, `Analyzer`, `Extractor`

**Constants:**
- Enum values: `EdgeKindCalls`, `NodeKindFunction`
- Limits: `maxShards`, `minShards`, `defaultShardCount`
- Features: `shardCountEnv`, `fnvOffset64` (algorithm parameters)

## Where to Add New Code

**New Feature (Query Tool):**
- Primary code: `internal/mcp/tools_<feature>.go` (handler) + `internal/query/` (if new traversal)
- Tests: `internal/mcp/tools_<feature>_test.go`
- Schema: Update `internal/mcp/manifest.go` (tool definition)
- Documentation: `docs/mcp.md` (tool reference)

**New Analyzer:**
- Implementation: `internal/analysis/<analyzer_name>.go`
- Tests: `internal/analysis/<analyzer_name>_test.go`
- Registration: Update `analyze` tool in `internal/mcp/tools_analyze.go`
- Documentation: `docs/<analyzer_name>.md`

**New Language Support:**
- Parser: `internal/parser/languages/<language>.go`
- Extractor: Implement `parser.Extractor` interface (nodes + edges)
- Resolver: `internal/resolver/<language>*.go` (symbol binding)
- Tests: Fixtures in `internal/indexer/*_<language>_test.go`
- Registration: Add to `parser.RegisterAll()` in `internal/parser/languages/registry.go`

**New Command:**
- Handler: `cmd/gortex/<verb>.go` (uses Cobra)
- Tests: `cmd/gortex/<verb>_test.go`
- Subcommand setup: Register in `cmd/gortex/root.go` (group assignment)
- Help text: Cobra `.Short` and `.Long` fields

**Utilities:**
- Shared helpers: `internal/` subdirectory if used by 3+ callers; else inline in the caller
- Graph mutations: Extend `internal/graph/graph.go` (not a separate file)
- Query patterns: Add to `internal/query/` (keep BFS/DFS traversals there)

## Special Directories

**eval/:**
- Purpose: Evaluation data and benchmark scripts
- Generated: Yes (populated by CI)
- Committed: Partially (fixtures committed, results not)
- Usage: Benchmark runs, SWE evaluation, agent testing

**bench/:**
- Purpose: Micro and macro benchmarks
- Generated: Yes (results written during `go test -bench`)
- Committed: No
- Usage: Performance regression detection

**docs/:**
- Purpose: User-facing and architecture documentation
- Generated: Some files (e.g., schema reference)
- Committed: Yes
- Usage: `gortex --help`, online docs, spec references

**.planning/codebase/:**
- Purpose: Codebase analysis documents (generated by gsd-map-codebase)
- Generated: Yes (from /gsd-map-codebase)
- Committed: No (but can be)
- Usage: Future agent context, GSD planning phases

---

*Structure analysis: 2026-07-24*
