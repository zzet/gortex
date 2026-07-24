<!-- GSD:project-start source:PROJECT.md -->

## Project

**Gortex Mainframe Engine**

A private fork of [zzet/gortex](https://github.com/zzet/gortex) — the graph-based, multi-language code-intelligence engine (Go, tree-sitter, daemon + MCP/CLI/API) — being evolved into a fully-fledged **graph-based mainframe processing engine**. It ingests a mainframe estate into a queryable graph, layers deterministic analysis on top, then LLM enrichment, and ultimately grows into a digital twin of the estate. Built for one user's modernization work, not for upstream.

**Core Value:** A trustworthy graph representation of a mainframe estate that modernization cutover decisions can be made against — deterministic and reproducible first, enriched and simulated later.

### Constraints

- **Tech stack**: Go + tree-sitter ecosystem, extending gortex's existing architecture — evolve the engine, don't rewrite it
- **Fork hygiene**: keep the ability to pull upstream improvements — prefer additive packages/analyzers over invasive edits to upstream internals where practical
- **Sequencing**: deterministic layer before LLM enrichment before twin — trust and reproducibility are prerequisites for the later stages
- **Ownership**: personal project under the MuiGoku123432 GitHub account

<!-- GSD:project-end -->

<!-- GSD:stack-start source:codebase/STACK.md -->

## Technology Stack

## Languages

- Go 1.26.5 - Core engine for code-intelligence daemon, CLI, HTTP server, and MCP protocol implementation
- TypeScript/React - Web UI (Next.js 15) for graph visualization and dashboards
- Bash/Shell - Build scripts, installation, and benchmarking

## Runtime

- Go 1.26.5 runtime
- Single statically-linked binary for macOS (Intel/ARM64), Linux (x86_64/ARM64), Windows
- Go modules - `go.mod` with 70+ direct dependencies
- Lockfile: `go.sum` (81KB+) - Present and committed

## Frameworks

- Cobra 1.10.2 - CLI command framework and subcommand routing
- Viper 1.21.0 - Configuration management, environment variables, file parsing
- Pflag 1.0.10 - Command-line flag parsing with POSIX compatibility
- Tree-sitter 0.25.0+ - 257 language parsers via tree-sitter-forest (alexaandru/go-sitter-forest)
- AST Query - In-process AST pattern matching and semantic analysis (`internal/astquery/`)
- Bleve v2 6.0.0 - Full-text search with BM25 algorithm
- SQLite 1.54.0 (modernc.org) - Persistent graph snapshots, session data, conversation logs
- bbolt 1.5.0 - Embedded key-value store (BoltDB)
- PostgreSQL pgx/v5 5.10.0 - Optional remote graph storage backend
- Hugot 0.7.5 - Pure-Go ONNX runtime for transformer models; auto-downloads MiniLM-L6-v2
- ONNX Runtime (yalue/onnxruntime_go 1.31.0) - Optional native backend (embeddings_onnx build tag)
- GoMLX + XLA (gomlx/go-xla 0.2.2, gomlx/gomlx 0.27.3) - Optional GPU-accelerated inference (embeddings_gomlx build tag)
- GloVe 50-dimensional word vectors - Default static embedding (3.8MB embedded)
- Tokenizers (pkoukk/tiktoken-go 0.1.8, pkoukk/tiktoken-go-loader 0.0.2) - Token counting and OpenAI-compatible tokenization
- Charmbracelet Bubbletea 1.3.10 - TUI framework (interactive dashboards, progress)
- Charmbracelet Bubbles 1.0.0 - Reusable TUI components (tables, inputs, lists)
- Charmbracelet Lipgloss 1.1.0 - Terminal styling (colors, borders, alignment)
- Charmbracelet x/ansi 0.11.7 - ANSI escape sequence utilities
- Go net/http - Standard library HTTP server
- CORS handling - Custom implementation (`internal/server/cors.go`)
- Streamable HTTP - MCP 2026 protocol support via mark3labs/mcp-go 0.56.0
- mark3labs/mcp-go 0.56.0 - Model Context Protocol server implementation
- STDIO and HTTP transport modes
- Testify v1.11.1 (stretchr) - Assertion and mocking library
- rapid 1.3.0 (pgregory.net) - Property-based testing framework
- Go's built-in `testing` package
- Goreleaser - Cross-platform binary building and release packaging
- golangci-lint 2.11.4 - Multi-linter runner
- GCX1 wire format (gortexhq/gcx-go 0.1.0) - Compact 27% space-efficient serialization
- go.uber.org/zap 1.28.0 - Structured logging with performance optimization
- Telemetry - Optional anonymous tool/command counts (off by default)

## Key Dependencies

- `github.com/tree-sitter/go-tree-sitter v0.25.0` - Tree-sitter C bindings. **Note:** Uses CGO; vendored `go-pointer` shim replaces upstream to unlock multi-goroutine parsing (see `internal/thirdparty/go-pointer`)
- `github.com/blevesearch/bleve/v2 v2.6.0` - Full-text indexing and semantic search engine
- `github.com/coder/hnsw v0.6.1` - Vector similarity search
- `github.com/knights-analytics/hugot v0.7.5` - Model inference and embedding generation
- `modernc.org/sqlite v1.54.0` - Pure-Go SQLite implementation (no external C dependency)
- `github.com/jackc/pgx/v5 v5.10.0` - PostgreSQL driver (optional remote backend)
- `go.etcd.io/bbolt v1.5.0` - Key-value store
- `github.com/google/go-github/v88 v88.0.0` - GitHub API client (PR review, contract detection)
- `github.com/gomlx/go-huggingface v0.3.5` - Hugging Face model API integration
- `github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728` - PDF parsing and extraction
- `github.com/pkoukk/tiktoken-go v0.1.8` - OpenAI tokenizer
- `github.com/pkoukk/tiktoken-go-loader v0.0.2` - Tokenizer vocabulary loader
- `github.com/spf13/cobra v1.10.2` - Command-line interface framework
- `github.com/spf13/viper v1.21.0` - Configuration file parsing
- `github.com/spf13/pflag v1.0.10` - Flag parsing
- `github.com/fsnotify/fsnotify v1.10.1` - File system watcher (for live indexing)
- `github.com/sabhiram/go-gitignore v0.0.0-20210923224102-525f6e181f06` - .gitignore parsing
- `github.com/gofrs/flock v0.13.0` - File locking (daemon lock)
- `github.com/RoaringBitmap/roaring/v2 v2.19.0` - Efficient integer set operations
- `github.com/bits-and-blooms/bitset v1.24.6` - Bitset implementation
- `github.com/zeebo/blake3 v0.2.4` - BLAKE3 hashing
- `github.com/google/uuid v1.6.0` - UUID generation
- `github.com/pelletier/go-toml/v2 v2.4.3` - TOML parsing
- `gopkg.in/yaml.v3 v3.0.1` - YAML parsing
- `github.com/jedib0t/go-pretty/v6 v6.8.3` - Pretty table formatting
- `github.com/toon-format/toon-go v0.0.0-20251202084852-7ca0e27c4e8c` - TOON wire format support

## Configuration

- `.gortex.yaml` - Project-level indexing and query configuration (languages, exclusions, workers, query depth)
- Viper-based config merging (YAML files, env vars, flags)
- `.env` file support via viper (environment-based secrets, API keys)
- CLI flags override config file values
- Makefile with build variants:
- Goreleaser config (`.goreleaser.yml`) - Linux/macOS/Windows cross-compilation with CGO
- LDFLAGS injection: version, commit SHA, build date

## Platform Requirements

- Go 1.26.5+
- CGO enabled for tree-sitter parsing (requires C/C++ compiler)
- On Linux: cross-compile toolchain via goreleaser-cross Docker image
- On macOS: Apple native ld for binary linking (OSX 15+/Tahoe dyld compatibility)
- On Windows: mingw C/C++ toolchain for CGO
- **Deployment target:** Single statically-linked binary (no runtime dependencies)
- **Databases:** SQLite bundled; PostgreSQL optional for multi-instance deployments
- **Disk:** Graph snapshots stored in project-root `.gortex/` directory (configurable via workspace config)
- **Memory:** Scales with repository size; precomputed reach index keeps depth-3 queries O(N) in fan-in/out
- **OS Support:** Linux (glibc 2.29+), macOS 12+, Windows 10+
- **Network:** Optional (daemon-only mode); HTTP server if MCP/web UI enabled

<!-- GSD:stack-end -->

<!-- GSD:conventions-start source:CONVENTIONS.md -->

## Conventions

## Naming Patterns

- Source files: `lowercase_name.go` — descriptive package file names
- Test files: `*_test.go` suffixed to source file or concept name
- Benchmark tests: `*_bench_test.go` suffix
- Examples: `config_cmd.go`, `config_cmd_test.go`, `config_cmd_bench_test.go`
- Exported (public): PascalCase — `AnalyzeResolutionOutcomes()`, `EnrichGraph()`
- Unexported (private): camelCase — `normalizeExtractionMetadata()`, `legacyLocalVariable()`
- Test functions: `TestFunctionName_ContextOrCase` pattern — `TestLooksLikeGlob()`, `TestAnalyze_KindArgAndUniversalFlags()`
- Benchmark functions: `BenchmarkFunctionName` pattern — `BenchmarkIndex_Self()`
- Local/package-level: camelCase — `sourceLines`, `nodesByID`, `testNodes`
- Constants: PascalCase (typed constants) or UPPER_CASE (simple strings) — `KindFile`, `OutcomeAmbiguousMultiMatch`
- Interface/type variables: PascalCase — `EdgeKind`, `NodeKind`
- Exported: PascalCase — `Node`, `Edge`, `ResolutionRow`, `ExtractionResult`
- Unexported: camelCase — `snapshotHeader`, `classVal`
- Constants for kinds: PascalCase with descriptive prefix — `KindFunction`, `EdgeCalls`, `OutcomeStubOnly`

## Code Style

- Standard Go formatting via `gofmt` (implicit in build)
- Line length: typically 80–100 characters; longer lines acceptable for complex logic
- Indentation: tabs (Go standard)
- Tool: golangci-lint v2.11.4 (pinned in CI via `.github/workflows/ci.yml`)
- Config: `.golangci.yaml` — enforces standard + custom exclusions
- Key rules enforced:
- Generated files: Excluded via `lax` preset (auto-detects generated code)
- Package-level: Document the purpose and responsibility
- Types first, then functions that use them
- Exported symbols before unexported
- Related constants grouped with explanatory comments

## Import Organization

- No aliases used; full import paths are explicit
- Internal packages always use the module path: `github.com/zzet/gortex/internal/config`

## Comments

- Every exported function must have a doc comment starting with the function name
- Format: `// FunctionName does X and returns Y` followed by details on purpose, parameters, and return values
- Multi-line comments explain edge cases, preconditions, and postconditions
- `PURPOSE —` explains what the function/constant/section solves
- `RATIONALE —` justifies design decisions
- `KEYWORDS —` lists searchable tags for finding related code
- Example from `resolution_outcomes.go`:
- Every exported const block must explain what each constant represents
- Example from `graph/node.go`:
- Explain complex logic, non-obvious optimizations, and invariants
- Used sparingly; clear code is better than commented code

## Error Handling

- Standard Go: check `if err != nil { return ..., err }` immediately after error-prone calls
- Return errors explicitly, don't ignore them
- Use `fmt.Errorf()` to add context: `fmt.Errorf("no repo entry named %q in global config", name)`
- When ignoring errors is intentional, document why in a comment
- Errors bubble up; don't suppress them silently
- Use `%w` wrapper in `fmt.Errorf()` to preserve error chains for debugging

## Function Design

- Functions typically 15–50 lines (most < 100 lines)
- When logic exceeds 50 lines, consider extracting helpers
- Keep parameter count ≤ 5; use struct if more needed
- Clear, descriptive names: `sourceLines`, `changedFiles`, `reasonFilter`
- No generic single-letter names except loop counters (`i`, `j`) and well-known patterns (`b` for `*testing.B`)
- Exported functions return `(result T, err error)` — error last
- Helpers often return `(count int, err error)` or just `T` for simple operations
- Multiple return values acceptable for (value, error) pairs

## Module Design

- Package exposes only public API; helpers are unexported (lowercase)
- Each package is a cohesive unit: `indexer`, `parser`, `resolver`, `graph`, etc.
- `internal/indexer/` — index construction
- Not used; each package is imported by full path
- No `__init__.go` pattern or re-export files

## Type Definitions

- Fields are exported (PascalCase) when needed by consumers
- JSON field tags map to external formats: 
- Receiver-specific methods (pointer vs. value receivers chosen based on mutability)
- Methods grouped by interface they satisfy
- String constants for enums (NodeKind, EdgeKind) — checked at compile time via type system
- Example:

## Cross-Cutting Concerns

- Uses Go's built-in `log` package or custom `conversationlog` package (for LLM conversation recording)
- No centralized logging framework; each package manages its own observability
- Input validation happens at package boundaries (public functions)
- nil checks for critical pointers with early returns or defensive defaults
- Configuration handled via `.gortex.yaml` and environment variables
- No in-code secrets; all credentials external

<!-- GSD:conventions-end -->

<!-- GSD:architecture-start source:ARCHITECTURE.md -->

## Architecture

## System Overview

```text

```

## Component Responsibilities

| Component | Responsibility | File |
|-----------|----------------|------|
| **CLI Router** | Command-line argument parsing, subcommand dispatch, telemetry | `cmd/gortex/root.go` |
| **Daemon Controller** | IPC server, graph lifecycle, repository management | `internal/daemon/*` |
| **Parser Registry** | Multi-language syntax extraction via tree-sitter | `internal/parser/` |
| **Indexer** | File-tree walk, syntax parsing, node/edge creation | `internal/indexer/` |
| **Symbol Resolver** | Cross-language semantic binding, name resolution | `internal/resolver/` |
| **Graph Store** | Node/edge storage, sharded write concurrency, persistence | `internal/graph/` |
| **Query Engine** | Graph traversal, type resolution, call-chain analysis | `internal/query/` |
| **MCP Handler** | JSON-RPC tool registration and dispatch | `internal/mcp/` |
| **Analysis Engines** | Clone detection, dead code, cycles, coverage, etc. | `internal/analysis/` |
| **HTTP Server** | Dashboard, REST endpoints, CORS | `internal/server/` |
| **Search Index** | Full-text (trigram) and semantic search | `internal/search/` |

## Pattern Overview

- **Persistent graph in SQLite** — Full-index and incremental-reindex paths both populate a persisted knowledge base
- **Sharded write concurrency** — Power-of-two shard count (~16–256 depending on CPU cores) eliminates write-lock contention during indexing
- **Query-time type resolution** — Symbol binding happens at query time, not index time, to handle cross-repo and dynamic dispatch
- **Analyzer-as-a-tool pattern** — Each analysis (clones, dead code, cycles, etc.) is registered as an MCP tool and runs on-demand
- **Lazy evaluation** — Results are computed once and cached; cache invalidates on graph mutation

## Layers

- Purpose: Route user intent to daemon or execute standalone
- Location: `cmd/gortex/`
- Contains: Command definitions, daemon control, subcommand dispatch
- Depends on: `internal/daemon`, `internal/config`, `internal/progress`
- Used by: End users and automation scripts
- Purpose: Long-lived process managing the in-memory graph and responding to queries
- Location: `internal/daemon/`, `internal/server/`
- Contains: Graph lifecycle, repository tracking, IPC/HTTP listeners
- Depends on: `internal/graph`, `internal/indexer`, `internal/mcp`
- Used by: CLI clients (via IPC), editor extensions (via MCP/HTTP), build systems
- Purpose: Convert source code to graph nodes and edges
- Location: `internal/indexer/`, `internal/parser/`, `internal/resolver/`
- Contains: File walk, tree-sitter parsing, semantic binding
- Depends on: `internal/graph`, tree-sitter bindings
- Used by: Daemon (on track/untrack/reload)
- Purpose: Store and retrieve nodes/edges with concurrent write safety
- Location: `internal/graph/`
- Contains: Sharded node storage, edge adjacency lists, SQLite persistence
- Depends on: SQLite driver
- Used by: Indexer (writes), Query (reads), Analysis (walks)
- Purpose: Traverse the graph to answer questions (dependencies, callers, types, etc.)
- Location: `internal/query/`
- Contains: BFS/DFS traversal, type resolution, class hierarchies
- Depends on: `internal/graph`, `internal/resolver`
- Used by: MCP tools, Analysis engines
- Purpose: Compute properties (dead code, clones, cycles, etc.) on demand
- Location: `internal/analysis/`
- Contains: ~30 pluggable analyzers (deadcode, clones, cycles, coverage, etc.)
- Depends on: Query engine, Graph
- Used by: MCP analyzer tool
- Purpose: Expose graph queries as tool calls
- Location: `internal/mcp/`, `internal/server/`, `internal/serverstack/`
- Contains: 180+ MCP tools, HTTP handlers, session management
- Depends on: Query, Analysis
- Used by: Editor extensions, CI systems, other agents

## Data Flow

### Primary Request Path (Lookup Symbol)

### Index Path (Track Repository)

### Query Path (Find Usages)

- **In-memory graph**: `internal/graph/graph.go:Graph` (sharded, thread-safe)
- **Overlay sessions**: Unsaved buffers shadow the base graph per MCP session
- **Analysis caches**: `internal/analysis/` caches reused during query session
- **SQLite backend**: Durable copy for daemon restarts

## Key Abstractions

- Purpose: Represents a code entity (function, class, variable, import, file, etc.)
- Examples: `internal/graph/node.go`
- Pattern: Node carries `Kind` (enum), `ID` (unique), `Name`, `Range` (file+line), `Meta` (language-specific data)
- Purpose: Represents a relationship (calls, references, inherits, imports, etc.)
- Examples: `internal/graph/edge.go`
- Pattern: Edge carries `From`, `To` (node IDs), `Kind` (enum), `FilePath`, `Line` (call site), metadata
- Purpose: Result of a query, sliced and paginated
- Examples: `internal/query/subgraph.go`
- Pattern: Contains a filtered set of nodes + edges, ranked by relevance/fan-in
- Purpose: Computes a graph property on demand (dead code, clones, etc.)
- Examples: `internal/analysis/deadcode.go`, `internal/analysis/clones.go`
- Pattern: Takes graph + options, returns structured result, caches across queries

## Entry Points

- Location: `cmd/gortex/main.go`
- Triggers: `gortex <subcommand> [args]`
- Responsibilities: Build flags, route to daemon or standalone, handle telemetry
- Location: `cmd/gortex/daemon.go`
- Triggers: `gortex daemon` (starts; daemonizes if needed)
- Responsibilities: Initialize graph, load persisted data, listen for IPC/HTTP
- Location: `cmd/gortex/mcp.go` (MCP server starts as daemon child)
- Triggers: `gortex mcp` (stdio MCP relay)
- Responsibilities: Dispatch JSON-RPC tool calls to handlers
- Location: `internal/serverstack/http_stack.go`
- Triggers: `gortex daemon --http-addr <addr>` (optional HTTP listener)
- Responsibilities: Serve dashboard, REST query endpoints

## Architectural Constraints

- **Threading:** Single-threaded event loop within each daemon (Go's goroutine pool handles I/O and parsing workers). Graph mutations are serialized per-shard.
- **Global state:** One `graph.Graph` per daemon, loaded at startup and evicted/reindexed incrementally. No per-tool graph copies.
- **Circular imports:** Resolver intentionally walks the call graph backward to resolve dispatch; forward analysis (dead code, cycles) uses BFS to avoid stack depth issues.
- **Memory:** Sharded graph + SQLite persistence allow multi-repo graphs to exceed RAM (though performance degrades if working set > RAM). Idle-TTL eviction not implemented; graph grows unbounded.
- **IPC Protocol:** JSON lines, Unix domain socket (default) or TCP (controllable). Handshake exchanges version + PID for safety.

## Anti-Patterns

### Eager Analysis

### Graph-Wide Locks for Reads

### Unresolved Symbol Placeholder Nodes

## Error Handling

- **Parse errors:** Quarantine the file (crash-isolation), record as a `Meta["parse_error"]` node, continue indexing. See `internal/parser/crashpool/`.
- **Unresolved references:** Record as edges to `unresolved` nodes (e.g., `missing_type:T`), marked with origin provenance.
- **Incremental reindex failures:** Retry once; if still failing, mark as stale and retry on next warmup. See `internal/indexer/indexer.go:IncrementalReindex()`.
- **Query timeouts:** Responder tools (find_usages, get_dependencies) have deadline gates; if exceeded, return partial results with `truncated: true`.

## Cross-Cutting Concerns

<!-- GSD:architecture-end -->

<!-- GSD:skills-start source:skills/ -->

## Project Skills

No project skills found. Add skills to any of: `.claude/skills/`, `.agents/skills/`, `.cursor/skills/`, `.github/skills/`, or `.codex/skills/` with a `SKILL.md` index file.
<!-- GSD:skills-end -->

<!-- GSD:workflow-start source:GSD defaults -->

## GSD Workflow Enforcement

Before using Edit, Write, or other file-changing tools, start work through a GSD command so planning artifacts and execution context stay in sync.

Use these entry points:

- `/gsd-quick` for small fixes, doc updates, and ad-hoc tasks
- `/gsd-debug` for investigation and bug fixing
- `/gsd-execute-phase` for planned phase work

Do not make direct repo edits outside a GSD workflow unless the user explicitly asks to bypass it.
<!-- GSD:workflow-end -->

<!-- GSD:profile-start -->

## Developer Profile

> Profile not yet configured. Run `/gsd-profile-user` to generate your developer profile.
> This section is managed by `generate-claude-profile` -- do not edit manually.
<!-- GSD:profile-end -->
