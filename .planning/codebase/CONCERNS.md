# Codebase Concerns

**Analysis Date:** 2026-07-24

## Tech Debt

### Monolithic Indexer Core
- Issue: `./internal/indexer/indexer.go` is 8,404 lines (the largest file in the codebase), aggregating parsing, resolution, graph construction, and orchestration of multiple sub-systems (parser crash handling, semantic enrichment, embedding, dependency tracking, test analysis, licensing scanning, contract extraction). Changes to any aspect require coordinating edits through this single file.
- Files: `./internal/indexer/indexer.go`
- Impact: Difficult to isolate bugs, increases regression risk on large refactors, high cognitive load for understanding the full indexing lifecycle. Difficult to test parts in isolation.
- Fix approach: Refactor along natural seams (parsing → resolution → enrichment → output as separate coordinators). Extract major sub-phases into dedicated handler structs while keeping a thin orchestrator.

### Hook-Local MCP Performance
- Issue: Each file indexed via PreToolUse hook opens a fresh MCP connection to the daemon, even though the hook batches files. Per-file connection overhead dominates on wide file globs.
- Files: `./internal/hooks/pretooluse.go:788`
- Impact: File indexing checks (used by Claude Code during edit validation) incur one dial per file, blocking editor responsiveness on large change sets.
- Fix approach: Reuse a single persistent connection across the file batch, or implement a count-only probe variant that skips the full `get_file_summary` re-indexing on the hot path. Requires updating the test seam (`fileIndexedFn`) to accept batch operations.

### Hook-Local Path Resolution Security Gap
- Issue: `repoRootForFile()` in `./internal/hooks/pretooluse.go:895` hand-rolls path resolution for file-lookup against the graph's by-file index instead of routing through the server's `resolveFilePath` (which enforces SECURITY.md repo-confinement guards). The local logic is also symlink-naive, yielding a bare path that can diverge from the canonical graph key in multi-repo mode.
- Files: `./internal/hooks/pretooluse.go:895-920`
- Impact: A symlink → external-repo escape in a hook invocation could theoretically surface code outside the intended workspace boundary, undermining the repo-confinement guarantee.
- Fix approach: Route `get_file_summary` path arguments through `resolveFilePath` server-side. Have the hook forward `{cwd, file_path}` verbatim and let the server resolve authority.

## Known Bugs

### Upstream Data Race in go-huggingface/hub
- Symptoms: The `DownloadFilesCtx` function in `github.com/go-huggingface/hub` contains a data race on a shared map guard. Manifests as unpredictable failures during model download in tests when running under `-race`.
- Files: `./internal/embedding/provider_test.go:153` (marked `skip`), test skipped under `-race` flag
- Trigger: Running `go test -race ./internal/embedding/...` or full suite with `-race`
- Workaround: Test is skipped under `-race`; users are not affected at runtime because the embedding system initializes models once per daemon lifecycle, not concurrently. Race condition is benign in production (single goroutine owns the download).
- Upstream: `github.com/go-huggingface/hub` has not merged a fix as of 2026-07. Consider pinning a known-good commit if the project updates.

## Security Considerations

### Panic Surface in Framework Dispatch
- Risk: Several `panic()` calls exist in framework-dispatch code (`./internal/resolver/framework_*.go`) that validate invariants about partial-scope resolution. If an invariant is violated (e.g., a scoped resolver is called with an operation it doesn't support), the daemon panics instead of returning an error. An attacker or malformed repository could trigger these if scoped resolution logic is misused.
- Files: `./internal/resolver/framework_scoped_store.go` (2 panics), `./internal/resolver/framework_synth.go`, `./internal/resolver/framework_edge_batch.go` (3 panics)
- Current mitigation: Framework dispatch is internal; only the resolver orchestrator calls it. Invariant checks at call sites mitigate triggering the panics in normal operation.
- Recommendations: Replace panics with sentinel errors (`ErrUnsupportedOperation`, `ErrScopeMismatch`) and propagate them upward. Document the invariant assumptions in framework handler godoc.

### Tree-Sitter ABI Assumptions
- Risk: `./internal/parser/tsitter/node_cnav.go` uses unsafe pointer arithmetic to map between Go wrapper types and tree-sitter C structs. If the tree-sitter C library's struct layout changes (e.g. a new field is added), the size checks will panic at runtime instead of gracefully degrading.
- Files: `./internal/parser/tsitter/node_cnav.go:24-34`
- Current mitigation: The code includes compile-time size assertions and panics if sizes diverge, which will catch ABI breaks in CI. Tree-sitter's releases are stable; the C ABI has not changed since v0.20.
- Recommendations: Consider adding a fallback marshalling path (slower but safer) in case ABI divergence is detected. Add a build tag (e.g. `+build gortex_no_unsafe`) to fall back to safe allocation on request.

## Performance Bottlenecks

### N+1 Graph Traversal in Semantic Enrichment
- Problem: During the semantic-enrichment pass, the daemon performs a pathologically slow `COUNT ... GROUP BY` scan on the SQLite graph store. This scan touches every edge/node and causes the store to thrash disk access patterns.
- Files: `./cmd/gortex/daemon_controller.go` (comment at ~line 96), `./cmd/gortex/daemon_state.go` (mentions "ahead of the slow enrichment pass")
- Cause: The graph store's query planner does not have indices optimized for the enrichment query shape. The pass is blocking — clients cannot query the graph until enrichment finishes.
- Improvement path: Pre-compute edge/node cardinality during graph writes and store as metadata. Avoid full table scans during enrichment. Or, defer enrichment to a background thread after the graph is queryable.

### Memory Exhaustion on Large Repositories
- Problem: The indexer loads the full repository graph into memory before writing to the store. On very large monorepos (200k+ symbols), this can exceed daemon memory limits even with GOMEMLIMIT tuning.
- Files: `./cmd/gortex/daemon_memlimit.go`, `./internal/indexer/indexer.go` (graph construction phase)
- Cause: No streaming write path; all nodes/edges are buffered before the store batch write.
- Improvement path: Implement incremental graph flushes (e.g., write 10k node batches as parsing progresses instead of buffering to the end). Requires refactoring the indexer's batching coordination.

### Parser Crash Pool Throughput
- Problem: The parser runs tree-sitter through a `crashpool` worker pool that serializes parse requests. A slow or hanging parser on one file can block other workers.
- Files: `./internal/parser/crashpool/pool.go`
- Cause: Tree-sitter is not thread-safe per-language; the pool uses a semaphore to serialize access.
- Improvement path: Profile the crash rate and timeout on real repositories. If crash rate is low, consider allowing parallel parsing per-language-pair (different languages don't contend). Or, spawn fresh processes for each language to avoid shared state.

## Fragile Areas

### Global Search Backend Invariant
- Files: `./internal/indexer/indexer.go:8000+` (contains `panic("indexer: search backend is not *search.Swappable...")`)
- Why fragile: The indexer assumes the search backend is a `*search.Swappable`. If initialization is reordered or a different backend is swapped in, this will panic at runtime. There is no type-safe way to enforce this invariant.
- Safe modification: Always initialize the search backend before calling `Index()`. Add a setup test that verifies the type invariant at startup. Consider using a builder pattern to enforce initialization order.
- Test coverage: `./cmd/gortex/daemon.go` initializes the swappable backend; no test explicitly validates this coupling.

### CGO Requirement
- Files: `./internal/parser/treesitter.go` (imports C bindings)
- Why fragile: The entire codebase requires CGO and C compiler toolchain. This breaks on systems without CGO support or C toolchain (e.g., WASM, embedded platforms, some CI runners). Vendored tree-sitter libraries must be kept in sync with language grammar bindings.
- Safe modification: Isolate tree-sitter calls behind a facade and provide a fallback regex-based parser for environments without CGO. Add CI matrix that tests both CGO and non-CGO builds (if fallback is added).
- Test coverage: Build tests are platform-specific (Darwin, Linux, Windows); no explicit non-CGO build test.

### Concurrent Goroutine Spawning Without Structured Concurrency
- Files: `./internal/indexer/watcher.go` (4 goroutines spawned in loop), `./internal/resolver/resolver.go` (multiple goroutine pools for cross-repo resolution), `./cmd/gortex/daemon.go` (4 background goroutines for lifecycle management)
- Why fragile: Goroutines are launched with `go func()` and their lifetimes are managed via channels and sync primitives. If a channel is not drained or a WaitGroup is forgotten, goroutines leak. No structured concurrency framework (e.g., `errgroup.Group`) is used consistently.
- Safe modification: Audit all `go func()` calls to ensure they have explicit done/error channels. Use `sync/errgroup` or context cancellation for coordinated shutdown. Add a goroutine leak detector to tests (e.g., `goleak` in test fixtures).
- Test coverage: No explicit goroutine leak tests. Some lifecycle tests exist (`daemon_snapshot_test.go`) but do not verify all spawned goroutines exit cleanly.

## Scaling Limits

### Single Daemon Per Workspace
- Current capacity: The daemon is a single long-running process that holds the graph in memory and coordinates all indexing/query operations. Multi-repo support exists, but all repositories share one daemon.
- Limit: If the workspace contains >10 large repositories, or a single repository >500k symbols, the daemon can exhaust available memory (even with GOMEMLIMIT). No clustering / sharding support exists.
- Scaling path: Add a federation mode where multiple daemon instances coordinate over gRPC, each owning a subset of repositories. Route queries to the appropriate daemon. This is a major architectural change; `./internal/daemon/federation.go` contains scaffolding but is incomplete.

### SQLite as the Sole Graph Store Backend
- Current capacity: SQLite is single-writer, which limits how fast the graph can be updated. Pragmas and busy-timeouts help, but under high concurrency (e.g., 50+ concurrent daemon instances in a CI farm indexing the same repo), the store will contend.
- Limit: Sustained indexing (updates > 1000 edges/sec) on a shared store degrades significantly.
- Scaling path: Add a pluggable store interface and implement a sharded store backend (e.g., one SQLite file per language/module) or migrate to a true multi-writer store (e.g., RocksDB). Existing code assumes a single `graph.Store`; plugging in a new backend requires interface changes.

### Tree-Sitter Language Support Maintenance Burden
- Current capacity: The `go.mod` includes 200+ vendored tree-sitter language grammars (`github.com/alexaandru/go-sitter-forest/*`). Each language binding is a separate module and can be updated independently, but version misalignment across the forest can cause parse errors.
- Limit: Updating all tree-sitter bindings is a monorepo operation. Missing updates to a language cause skew (newer grammars parse differently).
- Scaling path: Move to a plugin model where languages are fetched at runtime from a registry, rather than vendored at build time. Or, adopt a single unified tree-sitter build (e.g., WasmTree-Sitter) instead of separate Go bindings per language.

## Dependencies at Risk

### go-huggingface/hub Data Race
- Risk: The embedding system uses `github.com/go-huggingface/hub` to download model files. The library has a known, unfixed data race in `DownloadFilesCtx`.
- Impact: If embedding models are downloaded concurrently from multiple goroutines, unpredictable crashes or silent data corruption can occur (though the single-goroutine daemon initializes once, so risk is low in production).
- Migration plan: Monitor upstream for a fix. If unfixed, fork the library or replace with a custom downloader. The embedding system is optional (can be disabled via config), so disabling it is a fallback.

### Upstream Elixir Tree-Sitter Grammar
- Risk: The Elixir grammar is forked from the official `tree-sitter-elixir` repo by the `go-sitter-forest` maintainer. If the official grammar evolves faster, the fork falls behind and parsing diverges.
- Impact: Elixir repositories indexed by this daemon may diverge from indexing done by the upstream Elixir toolchain.
- Migration plan: Pin the Elixir binding to a release and update explicitly every 6 months. Or, contribute the Go binding back upstream.

### mattn/go-pointer Mutex Contention
- Risk: A custom in-tree sharded replacement for `github.com/mattn/go-pointer` was added due to mutex contention in the original. The original guards a single map with one global RWMutex, which becomes a bottleneck when tree-sitter's `Parser.ParseWithOptions` is called on every parse (thousands of times per indexing run).
- Impact: Replaced with `./internal/thirdparty/go-pointer` which uses sharded locks. Mitigation is in place, but the approach is fragile: if the upstream library API changes, the shim must be updated manually.
- Migration plan: Monitor the upstream repo for performance improvements. If accepted, revert to the upstream. Otherwise, document the shim as a long-term fork.

## Test Coverage Gaps

### Skipped Tests Due to Missing Optional Dependencies
- Untested area: Embedding model integration, token counting with tiktoken, semantic type inference for TypeScript, live OpenAI API integration, Potion model-based embeddings, git-based release tracking (on systems without git).
- Files: `./internal/embedding/provider_test.go:63`, `./internal/tokens/models_test.go` (5 skips), `./internal/semantic/tstypes/provider_stream_test.go`, `./internal/releases/releases_test.go`, `./internal/embedding/potion_test.go`, `./internal/embedding/truncate_test.go`
- Risk: Features are not tested in CI unless the optional dependencies (OpenAI API key, Potion models, git binary, tiktoken) are available. Regressions in these paths go unnoticed until user reports.
- Priority: Medium — these are optional integrations, but critical for some users. Add a nightly CI job that installs optional dependencies and runs the full suite.

### Concurrency Testing Gaps
- Untested area: Goroutine leak detection, race conditions in concurrent graph mutations, multi-goroutine indexing under contention.
- Files: No explicit goroutine leak detector tests; `-race` is run but may not catch all data races.
- Risk: A subtle race condition in the graph store or indexer could be introduced and not caught by CI.
- Priority: Medium — add `goleak` to test fixtures on all daemon tests and add a dedicated concurrency stress-test suite (e.g., `TestIndexer_ConcurrentGraphMutations`).

### Hook Integration Test Coverage
- Untested area: PreToolUse hook integration with the daemon, multi-repo path resolution, symlink handling in hook file checks, hook timeout behavior.
- Files: `./internal/hooks/pretooluse.go` has unit tests (`./internal/hooks/pretooluse_enforcement_test.go`) but no integration tests that exercise the hook → daemon → graph flow.
- Risk: Bugs in the hook→daemon integration (e.g., a hook deadlock or malformed MCP message) are caught only when users run Claude Code on the repo.
- Priority: High for security (path resolution) — add integration tests that mock the MCP daemon and verify hook behavior under various failure modes (daemon unavailable, symlink escape attempts, malformed input).

### Parser Crash Recovery Testing
- Untested area: Crash recovery from a hanging tree-sitter parser, timeout behavior under high parser load, graceful degradation when a language grammar is missing.
- Files: `./internal/parser/crashpool/pool.go`, `./internal/parser/treesitter.go`
- Risk: If a parser hangs, the pool timeout may not trigger correctly, blocking the entire indexing operation.
- Priority: Medium — add tests that deliberately timeout a parser and verify the pool recovers.

---

*Concerns audit: 2026-07-24*
