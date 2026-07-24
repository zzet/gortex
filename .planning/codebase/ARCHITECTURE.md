<!-- refreshed: 2026-07-24 -->
# Architecture

**Analysis Date:** 2026-07-24

## System Overview

Gortex is a code intelligence engine that indexes source code repositories into an in-memory knowledge graph and exposes queryable APIs via CLI, MCP (Model Context Protocol), and HTTP.

```text
┌─────────────────────────────────────────────────────────────────────┐
│                         CLI Interface                               │
│                      `cmd/gortex/main.go`                           │
│  (Cobra-based CLI routing to daemon or standalone commands)         │
└────────────────────────┬────────────────────────────────────────────┘
                         │
        ┌────────────────┼────────────────┐
        │                │                │
        ▼                ▼                ▼
   ┌─────────────┐ ┌──────────────┐ ┌───────────────┐
   │   Daemon    │ │ MCP Server   │ │ HTTP Server   │
   │  (IPC RPC)  │ │ (JSON-RPC)   │ │ (Dashboard)   │
   └──────┬──────┘ └──────┬───────┘ └───────┬───────┘
          │                │                │
          └────────────────┼────────────────┘
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
        ▼                  ▼                  ▼
┌──────────────┐  ┌───────────────┐  ┌──────────────┐
│ Query Engine │  │     MCP       │  │  Analysis    │
│  (traversal) │  │ Tool handlers │  │   Engines    │
│ `query/`     │  │ `mcp/tools_*` │  │ `analysis/*` │
└──────┬───────┘  └───────────────┘  └──────────────┘
       │                                      │
       └──────────────┬───────────────────────┘
                      │
        ┌─────────────▼──────────────┐
        │  In-Memory Graph (Sharded) │
        │     `graph/graph.go`       │
        │  nodes + edges + indices   │
        └────────────┬────────────────┘
                     │
        ┌────────────▼───────────────┐
        │  SQLite Persistence Layer  │
        │  `graph/store_sqlite/`     │
        └────────────────────────────┘
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

**Overall:** Microkernel with plug-in analysis engines, lazy-loaded and cached.

**Key Characteristics:**
- **Persistent graph in SQLite** — Full-index and incremental-reindex paths both populate a persisted knowledge base
- **Sharded write concurrency** — Power-of-two shard count (~16–256 depending on CPU cores) eliminates write-lock contention during indexing
- **Query-time type resolution** — Symbol binding happens at query time, not index time, to handle cross-repo and dynamic dispatch
- **Analyzer-as-a-tool pattern** — Each analysis (clones, dead code, cycles, etc.) is registered as an MCP tool and runs on-demand
- **Lazy evaluation** — Results are computed once and cached; cache invalidates on graph mutation

## Layers

**CLI Layer:**
- Purpose: Route user intent to daemon or execute standalone
- Location: `cmd/gortex/`
- Contains: Command definitions, daemon control, subcommand dispatch
- Depends on: `internal/daemon`, `internal/config`, `internal/progress`
- Used by: End users and automation scripts

**Daemon Layer:**
- Purpose: Long-lived process managing the in-memory graph and responding to queries
- Location: `internal/daemon/`, `internal/server/`
- Contains: Graph lifecycle, repository tracking, IPC/HTTP listeners
- Depends on: `internal/graph`, `internal/indexer`, `internal/mcp`
- Used by: CLI clients (via IPC), editor extensions (via MCP/HTTP), build systems

**Index Layer:**
- Purpose: Convert source code to graph nodes and edges
- Location: `internal/indexer/`, `internal/parser/`, `internal/resolver/`
- Contains: File walk, tree-sitter parsing, semantic binding
- Depends on: `internal/graph`, tree-sitter bindings
- Used by: Daemon (on track/untrack/reload)

**Graph Layer:**
- Purpose: Store and retrieve nodes/edges with concurrent write safety
- Location: `internal/graph/`
- Contains: Sharded node storage, edge adjacency lists, SQLite persistence
- Depends on: SQLite driver
- Used by: Indexer (writes), Query (reads), Analysis (walks)

**Query Layer:**
- Purpose: Traverse the graph to answer questions (dependencies, callers, types, etc.)
- Location: `internal/query/`
- Contains: BFS/DFS traversal, type resolution, class hierarchies
- Depends on: `internal/graph`, `internal/resolver`
- Used by: MCP tools, Analysis engines

**Analysis Layer:**
- Purpose: Compute properties (dead code, clones, cycles, etc.) on demand
- Location: `internal/analysis/`
- Contains: ~30 pluggable analyzers (deadcode, clones, cycles, coverage, etc.)
- Depends on: Query engine, Graph
- Used by: MCP analyzer tool

**MCP/API Layer:**
- Purpose: Expose graph queries as tool calls
- Location: `internal/mcp/`, `internal/server/`, `internal/serverstack/`
- Contains: 180+ MCP tools, HTTP handlers, session management
- Depends on: Query, Analysis
- Used by: Editor extensions, CI systems, other agents

## Data Flow

### Primary Request Path (Lookup Symbol)

1. **MCP request arrives** → `internal/mcp/session_ctx.go` (route by tool name)
2. **Tool handler runs** → e.g. `internal/mcp/tools_search.go:SearchSymbols()`
3. **Query engine walks graph** → `internal/query/engine.go:Search()`
4. **Results marshalled to JSON** → `internal/mcp/manifest.go` (schema + serialization)
5. **Response sent to client** (editor/agent)

### Index Path (Track Repository)

1. **CLI: `gortex track <repo>`** → `cmd/gortex/track.go`
2. **Daemon persists tracking** → `internal/daemon/federation.go`
3. **Indexer walks files** → `internal/indexer/indexer.go:Index()`
4. **For each file:**
   - Parser extracts syntax tree (tree-sitter) → `internal/parser/`
   - Extractor creates nodes (symbols, types, etc.) → language-specific extractors
   - Resolver binds references to definitions → `internal/resolver/`
5. **Graph mutates** → `internal/graph/graph.go:AddNode/AddEdge()`
6. **SQLite persists** → `internal/graph/store_sqlite/`
7. **Analysis caches invalidate** → `internal/analysis/`

### Query Path (Find Usages)

1. **MCP: `find_usages` request** → `internal/mcp/tools_usages.go`
2. **Lookup symbol by name** → `internal/query/engine.go:Search()`
3. **Resolve type** (if needed) → `internal/resolver/`
4. **Walk inbound edges** → `internal/graph/graph.go:InEdges()`
5. **Filter by scope/visibility** → `internal/query/`
6. **Return results** with source locations

**State Management:**
- **In-memory graph**: `internal/graph/graph.go:Graph` (sharded, thread-safe)
- **Overlay sessions**: Unsaved buffers shadow the base graph per MCP session
- **Analysis caches**: `internal/analysis/` caches reused during query session
- **SQLite backend**: Durable copy for daemon restarts

## Key Abstractions

**Node:**
- Purpose: Represents a code entity (function, class, variable, import, file, etc.)
- Examples: `internal/graph/node.go`
- Pattern: Node carries `Kind` (enum), `ID` (unique), `Name`, `Range` (file+line), `Meta` (language-specific data)

**Edge:**
- Purpose: Represents a relationship (calls, references, inherits, imports, etc.)
- Examples: `internal/graph/edge.go`
- Pattern: Edge carries `From`, `To` (node IDs), `Kind` (enum), `FilePath`, `Line` (call site), metadata

**SubGraph:**
- Purpose: Result of a query, sliced and paginated
- Examples: `internal/query/subgraph.go`
- Pattern: Contains a filtered set of nodes + edges, ranked by relevance/fan-in

**Analyzer:**
- Purpose: Computes a graph property on demand (dead code, clones, etc.)
- Examples: `internal/analysis/deadcode.go`, `internal/analysis/clones.go`
- Pattern: Takes graph + options, returns structured result, caches across queries

## Entry Points

**CLI Entry:**
- Location: `cmd/gortex/main.go`
- Triggers: `gortex <subcommand> [args]`
- Responsibilities: Build flags, route to daemon or standalone, handle telemetry

**Daemon Entry:**
- Location: `cmd/gortex/daemon.go`
- Triggers: `gortex daemon` (starts; daemonizes if needed)
- Responsibilities: Initialize graph, load persisted data, listen for IPC/HTTP

**MCP Server Entry:**
- Location: `cmd/gortex/mcp.go` (MCP server starts as daemon child)
- Triggers: `gortex mcp` (stdio MCP relay)
- Responsibilities: Dispatch JSON-RPC tool calls to handlers

**HTTP Server Entry:**
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

**What happens:** An old pattern computed all analyses at index time (clones, dead code) and stored them in the graph.
**Why it's wrong:** Index time is deterministic but analyses change as the graph evolves; pre-computing them requires re-indexing every change, which is expensive.
**Do this instead:** Compute on-demand in MCP tool handlers with caching; invalidate cache on graph mutations. See `internal/analysis/analysis.go`.

### Graph-Wide Locks for Reads

**What happens:** Early versions locked the entire graph for multi-shard queries.
**Why it's wrong:** Sharded locking exists to allow parallel writes; coarse locks defeat that parallelism and block the daemon.
**Do this instead:** Acquire shard locks in ascending order (to avoid deadlock) and release immediately after copying data. See `internal/graph/graph.go:OutEdges()`.

### Unresolved Symbol Placeholder Nodes

**What happens:** For unresolved references, old code created placeholder nodes that cluttered the graph.
**Why it's wrong:** Placeholders pollute node counts, break dedup logic, and clutter searches.
**Do this instead:** Store unresolved references as `unresolved` edges pointing to a synthetic `UnresolvedName` node. See `internal/graph/stub.go`.

## Error Handling

**Strategy:** Fail-silent indexing with telemetry; queries never error on missing data.

**Patterns:**
- **Parse errors:** Quarantine the file (crash-isolation), record as a `Meta["parse_error"]` node, continue indexing. See `internal/parser/crashpool/`.
- **Unresolved references:** Record as edges to `unresolved` nodes (e.g., `missing_type:T`), marked with origin provenance.
- **Incremental reindex failures:** Retry once; if still failing, mark as stale and retry on next warmup. See `internal/indexer/indexer.go:IncrementalReindex()`.
- **Query timeouts:** Responder tools (find_usages, get_dependencies) have deadline gates; if exceeded, return partial results with `truncated: true`.

## Cross-Cutting Concerns

**Logging:** Structured logging via `go.uber.org/zap`, initialized per daemon. Log to stderr; MCP/HTTP clients never see logs (daemon logs stay local).

**Validation:** Source locations (file path + line + column) validated on node creation. Symbol IDs validated as `repo@path::name` (with escaping for special chars). No schema validation on edges; edges are truthy or falsy by topology.

**Authentication:** IPC socket owned by daemon user (no auth). HTTP endpoints require `--http-auth-token` (bearer token in Authorization header). MCP relay (stdio) inherits parent process credentials.

**Metrics:** Telemetry opt-in (daily aggregate counts via sidecar SQLite ledger). CLI verb tracing via `GORTEX_SESSION_ID` environment variable. See `internal/telemetry/`.

---

*Architecture analysis: 2026-07-24*
