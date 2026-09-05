package resolver

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

const unresolvedPrefix = "unresolved::"

const (
	// resolvePendingScanPageRows amortises the fixed SQLite lookup-cache and
	// worker-barrier cost across a larger keyset page. The independent byte cap
	// remains the hard payload bound; compute, mutation, deferred-LSP, guard,
	// and legacy-store chunks deliberately stay at resolvePendingPageRows.
	resolvePendingScanPageRows = 16 * 1024
	resolvePendingPageRows     = 2048
	resolvePendingPageBytes    = 16 << 20
)

// unresolvedEdgeStream keeps ResolveAll's raw pending corpus outside Go's
// retained heap. SQLite uses a stable rowid high-water/keyset scan; legacy
// stores use the early-stoppable iterator and retain at most one bounded page.
type unresolvedEdgeStream struct {
	ctx        context.Context
	pager      graph.UnresolvedEdgePager
	scan       graph.UnresolvedEdgeScan
	legacy     *unresolvedLegacySpool
	afterID    int64
	initErr    error
	countKnown bool
	exhausted  bool
}

func newUnresolvedEdgeStream(store graph.Store) *unresolvedEdgeStream {
	return newUnresolvedEdgeStreamContext(context.Background(), store)
}

func newUnresolvedEdgeStreamContext(ctx context.Context, store graph.Store) *unresolvedEdgeStream {
	if ctx == nil {
		ctx = context.Background()
	}
	stream := &unresolvedEdgeStream{ctx: ctx}
	if pager, ok := store.(graph.UnresolvedEdgePager); ok {
		scan, err := pager.BeginUnresolvedEdgeScan(ctx)
		if err != nil {
			// A store advertising a native pager never falls back to the legacy
			// spool on pager failure. Besides masking the real database error, that
			// fallback walks the complete unresolved corpus; on cancellation it
			// turned a prompt warmup stop into minutes of extra work.
			stream.initErr = err
			return stream
		}
		stream.pager = pager
		stream.scan = scan
		stream.countKnown = scan.PendingBefore >= 0
		stream.exhausted = scan.HighWaterID == 0
		return stream
	}
	legacy, err := newUnresolvedLegacySpoolContext(ctx, store)
	if err != nil {
		stream.initErr = err
		return stream
	}
	stream.legacy = legacy
	stream.scan.PendingBefore = legacy.count
	stream.countKnown = true
	stream.exhausted = legacy.count == 0
	return stream
}

func (s *unresolvedEdgeStream) close() {
	if s.legacy != nil {
		s.legacy.close()
	}
}

func (s *unresolvedEdgeStream) nextPage() ([]*graph.Edge, bool, error) {
	if s.exhausted {
		return nil, true, nil
	}
	if s.initErr != nil {
		return nil, false, s.initErr
	}
	if s.pager != nil {
		page, err := s.pager.ReadUnresolvedEdgePage(
			s.ctx, s.scan, s.afterID, resolvePendingScanPageRows, resolvePendingPageBytes,
		)
		if err != nil {
			return nil, false, err
		}
		if page.NextID <= s.afterID && !page.Exhausted {
			return nil, false, fmt.Errorf("unresolved edge pager did not advance after %d", s.afterID)
		}
		s.afterID = page.NextID
		s.exhausted = page.Exhausted
		return page.Edges, s.exhausted, nil
	}

	page, exhausted, err := s.legacy.nextPage(resolvePendingPageRows, resolvePendingPageBytes)
	if err != nil {
		return nil, false, err
	}
	s.exhausted = exhausted
	return page, exhausted, nil
}

// resolveProfileStarted guards the one-shot GORTEX_RESOLVE_CPUPROFILE capture
// so only the first full resolve pass is profiled.
var resolveProfileStarted atomic.Bool

// ResolveStats holds counts from a resolution pass.
type ResolveStats struct {
	Resolved   int `json:"resolved"`
	Unresolved int `json:"unresolved"`
	External   int `json:"external"`
	// LSP* exposes the deferred whole-pass circuit-breaker outcome. These are
	// diagnostic counters: skipped edges keep the heuristic result produced by
	// the normal resolve cascade and remain eligible for a later full pass.
	LSPDeferred        int  `json:"lsp_deferred,omitempty"`
	LSPAttempted       int  `json:"lsp_attempted,omitempty"`
	LSPResolved        int  `json:"lsp_resolved,omitempty"`
	LSPBudgetSkipped   int  `json:"lsp_budget_skipped,omitempty"`
	LSPBudgetExhausted bool `json:"lsp_budget_exhausted,omitempty"`
	// PendingBefore / PendingAfter record the pending-edge count before and
	// after the scope filter (see SetScope). Diagnostic only — the
	// warm-restart master-resolve log surfaces them so a scoped pass's
	// reduction is visible. Zero (omitted) on the unscoped whole-graph path.
	PendingBefore int `json:"pending_before,omitempty"`
	PendingAfter  int `json:"pending_after,omitempty"`
}

// Resolver resolves unresolved edge targets to actual graph node IDs.
//
// dirIndex / lastDirIndex are scratch maps populated for the duration
// of a single ResolveAll/ResolveFile pass so resolveImport can look up
// candidate file nodes in O(1) instead of scanning the whole graph per
// import edge. On large repos (vscode ≈ 150k nodes / 5k imports) the
// old full scan made ResolveAll the dominant cost of a cold index
// (8m of a 9m wall-clock). Maps are cleared between passes.
//
// mu serializes ResolveAll and ResolveFile because both reset and
// repopulate the scratch maps as part of their first step. Without
// it, two concurrent file-watcher debounce goroutines firing on the
// same per-repo Indexer (each calls Resolver.ResolveFile via
// Indexer.IndexFile) crash the daemon with "concurrent map writes"
// in buildDirIndexes.
type Resolver struct {
	graph        graph.Store
	logger       *zap.Logger
	dirIndex     map[string][]graph.FileNodeIdentity
	lastDirIndex map[string][]graph.FileNodeIdentity
	// OnComputeDone, when set, fires once per ResolveAll immediately after
	// the parallel compute loop has committed — BEFORE the deferred LSP
	// batch and the serial refinement tail (guard, attribution, dispatch
	// reconcile, terminal stamping). At that point every same-repo reference
	// the pass will resolve is already queryable; the LSP batch and the tail
	// only verify, override, and revert on top of a queryable graph. The
	// deferred LSP batch deliberately sits past this hook: its store-standing
	// yield measured 0.19% of the pending set while costing 450–512s of cold
	// wall, and the daemon uses this hook to mark the graph queryable minutes
	// before that verification work completes.
	OnComputeDone func()
	// receiverTypeIdxByDir memoizes, per package directory, the Go type index
	// the per-file method-receiver rebind builds. On a scoped tail that visits
	// every file of a D-file package, building it once per package (O(D)) rather
	// than once per file (O(D^2) GetFileNodes) removes the quadratic. Cleared
	// with dirIndex — it is only valid while the type nodes it indexes are held
	// stable for the duration of one ResolveAll/ResolveFile pass.
	receiverTypeIdxByDir map[string]map[pkgKey]string
	// cppIncludeDirs maps a repo-relative C/C++ source file to its ordered
	// include search path (the `-I` / `-isystem` dirs from compile_commands.json),
	// so a quoted/angle include resolves against the real compiler dir set
	// (deterministic, collision-breaking) before the suffix-unique fallback.
	// Populated by the indexer via SetCppIncludeDirs before ResolveAll.
	cppIncludeDirs map[string][]string
	// cppFallbackDirs is the heuristic include-root search path used when a
	// repo has no compile_commands.json: conventional dirs (include/src/inc/
	// api/lib) plus top-level header dirs, in priority order. The ordered
	// probe runs against it so collisions break deterministically even with
	// no compile DB. Populated by the indexer via SetCppFallbackIncludeDirs.
	cppFallbackDirs []string
	// providesForIdx maps `provides_for: AbstractName` (from @Module
	// useClass entries) → the set of concrete class names bound to it.
	// Populated once at the start of ResolveAll; consulted in O(1) by
	// resolveMethodCall's DI-binding fallback instead of re-walking
	// graph.AllEdges per call edge. Nil outside a resolution pass and
	// empty-but-non-nil when the graph has no @Module bindings, so
	// callers can short-circuit with len().
	providesForIdx map[string]map[string]struct{}
	// reachableDirsByFile maps caller-file ID → set of directories
	// reachable from that file (own dir ∪ directories of files reached
	// via EdgeImports). Populated once at the start of ResolveAll/
	// ResolveFile; consulted by resolveMethodCall to drop candidates
	// that live in packages the caller doesn't import. Without this,
	// the name-only fallback picks an arbitrary alphabetically-first
	// candidate across the whole graph, which produced bugs like
	// `RegisterAll` resolving to `OverlayManager.Register` simply
	// because "OverlayManager" sorts before "Registry".
	reachableDirsByFile map[string]map[string]struct{}
	// dirByFilePath memoises filePathDir(path) for every indexed file,
	// built once alongside reachableDirsByFile. filterByReachability runs in
	// the parallel resolver workers and otherwise recomputes filePathDir
	// per candidate per edge — ~20% of resolution CPU on a large TS monorepo
	// (filepathlite.Dir/Clean dominate). Read-only after build, so the
	// workers share it lock-free.
	dirByFilePath map[string]string
	// retargetedTestCallFiles accumulates the caller files of EdgeCalls
	// edges a resolution pass bound where the caller is test-classified
	// (test file path, or an is_test-stamped source symbol). The test
	// projection skips unresolved calls, so a call that resolves LATER
	// needs its caller reconciled — but the definition-side derived plan
	// never names the caller file. Consumers drain this frontier via
	// TakeRetargetedTestCallFiles after resolution and re-run the scoped
	// test projection over it. Guarded by retargetedMu: the parallel
	// apply loop holds r.mu, but single-file passes and the deferred LSP
	// apply note entries on their own paths.
	retargetedMu            sync.Mutex
	retargetedTestCallFiles map[string]struct{}
	// importEdgeGen counts imports-kind edge writes noted while a resolve
	// pass may hold pass-scoped import-adjacency retention. Write-site
	// verdicts live at noteImportEdgeWrite's callers.
	importEdgeGen uint64
	// importDirtyFiles names caller files whose stored import adjacency was
	// rewritten with known provenance; prepare deletes exactly these keys
	// from the pass-scoped retention instead of clearing it wholesale.
	importDirtyFiles map[string]struct{}
	// depModuleIndex bridges Go imports to dep::<module> contract
	// nodes emitted from go.mod. Keyed by RepoPrefix (the dep node's
	// owning repo) so we never link an import in repo A to a dep
	// declared by repo B's go.mod. Each entry list is sorted by
	// modulePath length descending so longest-prefix wins when
	// modules nest (e.g. aws-sdk-go-v2 vs aws-sdk-go-v2/service/s3).
	// Without this index, every dep::* contract node sits in the
	// graph with zero incoming edges — go.mod records the dependency
	// but no edge points consumers at it. Built once per Resolve*
	// pass, torn down at the end.
	depModuleIndex map[string][]depModuleEntry
	// mu serialises resolution phases against the shared graph.
	// Pointer so every Resolver built from the same graph.Store
	// locks the same mutex — necessary for MultiIndexer's per-repo
	// goroutines, each of which spawns its own Resolver instance.
	// Without the shared lock, concurrent ResolveAll passes race on
	// edge mutations (resolveImport writes e.To while another goroutine
	// iterates a shared edge projection).
	mu *sync.Mutex
	// scratchGeneration changes whenever a pass tears down shared Resolver
	// scratch. ResolveAll snapshots it before releasing mu between chunks; a
	// same-instance interactive resolve that interleaves and clears the caches
	// forces the current page's indexes and lookup cache to be rebuilt after
	// relock. Guarded by mu.
	scratchGeneration uint64
	// chunkYieldHook is a deterministic same-instance interleave seam used by
	// resolver tests. Nil in production.
	chunkYieldHook func()
	// validateLiveness turns on the concurrent-edit guard on the chunked
	// ResolveAll path: it releases mu between chunks so an interactive edit
	// can interleave and evict an edge the pass already resolved. With it on,
	// the per-chunk apply and guardCrossPackageCallEdges skip an evicted edge
	// (reindexing one half-resurrects it and can panic). Off (the default and
	// every non-chunked path) it is a no-op — nothing mutates the graph
	// mid-pass. Set only inside ResolveAll.
	validateLiveness bool

	// bulkMode is set true by ResolveAll for the duration of its parallel
	// worker fan-out and dropped around the inter-chunk mutex yield. While it
	// is on, resolveEdge skips the synchronous per-edge tryResolveViaLSP
	// round-trip: an LSP definition lookup serialises inside the helper, so a
	// TS/JS-dense chunk otherwise degenerates to serial-LSP-latency × chunk
	// size while every other worker idles at the barrier. The heuristic
	// cascade still runs; edges it leaves unresolved that the helper could
	// bind are collected and resolved once, off the barrier, in a deferred
	// batch after the loop (see resolveDeferredLSP). Dropped around the yield
	// so an interactive ResolveFile that interleaves on a shared Resolver
	// instance still gets inline LSP precision. Independent of scope /
	// validateLiveness; never set on any single-file path.
	bulkMode bool

	// lookupCache holds per-pass batched results from GetNodesByIDs /
	// FindNodesByNames. Populated by ResolveAll/ResolveFile before
	// the worker fan-out and cleared on return. Workers consult these
	// maps first; misses fall through to the underlying Store.
	//
	// Without the cache, the resolver fires ~3-10 store point lookups
	// per pending edge — across 10-30k unresolved edges that's 100k+
	// queries, each one a round trip on disk backends (~ms each).
	// With the cache the same information lands in two batched
	// queries per pass.
	nodeByID map[string]*graph.Node
	// missingNodeByID contains only IDs requested by the current page's
	// completed source hydration that the backend omitted (or returned nil).
	// It prevents dangling From IDs from falling into a point-query N+1 while
	// remaining generation-local; clearLookupCache tears it down on every page
	// and interleaving resolver mutation boundary.
	missingNodeByID map[string]struct{}

	nodesByName     map[string][]*graph.Node
	nodesByQualName map[string][]*graph.Node
	// nodesByRepoLanguageName is the authoritative per-pending-page cache for
	// normal resolution. It is keyed by exact repository plus compatible source
	// language family, so a Go call never materialises or examines Python
	// definitions merely because they share a name. nodesByRepoName is the
	// repository-only compatibility cache for tail passes that lack an edge
	// language; unlike the former nodesByName scan it never crosses repositories.
	nodesByRepoLanguageName map[resolverNameLookupScope]map[string][]*graph.Node
	nodesByRepoName         map[string]map[string][]*graph.Node
	// nodesByExternLanguageName is the authoritative global candidate cache for
	// explicit extern targets. Its language-family key prevents same-named Go,
	// Python, and other definitions from leaking into one another while retaining
	// language-neutral candidates.
	nodesByExternLanguageName map[string]map[string][]*graph.Node

	// importFilesByCaller memoises, per caller file, the set of file
	// paths that file imports (direct EdgeImports targets plus files
	// reached through transitive EdgeReExports barrel hops). Built
	// lazily inside the parallel resolve workers — importFilesMu guards
	// it — and cleared with the per-pass lookup caches. Consulted by
	// pickImportEvidenceCallee to disambiguate bare JS/TS calls; see
	// import_evidence.go for the precedence design.
	importFilesByCaller map[string]map[string]struct{}
	importFilesMu       sync.RWMutex

	// csharpNSByFile memoises each C# file's visible namespaces (usings
	// + own namespace chain). Built lazily inside the parallel resolve
	// workers — csharpNSMu guards it — and cleared with the per-pass
	// lookup caches. See csharp_ns_narrow.go.
	csharpNSByFile map[string]csharpFileNS
	csharpNSMu     sync.RWMutex

	// csharpGlobalByDir maps a compilation-unit key (nearest-ancestor
	// csproj directory, else the declaring file's directory) to the
	// namespaces its files' `global using` directives import;
	// csharpProjDirs is the csproj-owning directory set the keying
	// uses. nil = not built; derived lazily under csharpNSMu from the
	// persistent sources below — NOT rebuilt by a workspace scan per
	// scoped resolve (see scopedCSharpVisibilityInvalidate), and NOT
	// cleared by the per-page clearLookupCache.
	csharpGlobalByDir map[string][]string
	csharpProjDirs    map[string]struct{}

	// csharpGlobalDecls / csharpGlobalStaticDecls / csharpProjFiles are
	// the SOURCES the derived index above is built from: fileID → its
	// global_usings / global_using_static stamps, and the csproj file
	// IDs. Seeded by ONE workspace scan and then maintained by
	// scopedCSharpVisibilityInvalidate, so the scoped resolve entries
	// stop paying an O(all files) rebuild per save.
	// csharpGlobalStaticByDir is the static form's derived unit index —
	// same keying and lifetime as csharpGlobalByDir.
	csharpGlobalDecls       map[string][]string
	csharpGlobalStaticDecls map[string][]string
	csharpGlobalStaticByDir map[string][]string
	csharpProjFiles         map[string]struct{}
	csharpVisSeeded         bool

	// csharpTypeNodesByName memoises the repo-scoped type/interface
	// lookup the extension binder's eligibility rules repeat per call
	// edge, and csharpAncestorsByType the transitive base/interface
	// closure per type node — one hierarchy walk per unique receiver
	// type per pass instead of one per call site. Same lifetime and
	// lock as csharpGlobalByDir above.
	csharpTypeNodesByName map[string][]*graph.Node
	csharpAncestorsByType map[string]*csharpAncestors
	// csharpMemberNamesByType memoises each type's declared member-name
	// set for the instance-member claims gate. Same lifetime and lock.
	csharpMemberNamesByType map[string]map[string]struct{}

	// incrementalSkip holds the source-shapes of a single re-resolved file's
	// out-edges that were already unresolved before the edit; the forward
	// pass skips them. Set/cleared around ResolveFileAndIncoming by the
	// single-file index path. nil on every batch/whole-graph pass.
	incrementalSkip map[string]struct{}

	// incrementalNodesByFile / incrementalOutByNode are the bounded file and
	// adjacency frontier preloaded by ResolveFilesAndIncoming. The attribution
	// tail reuses them across its six passes so a package-sized partial resolve
	// never falls back to GetFileNodes/file adjacency once per pass per file.
	incrementalNodesByFile        map[string][]*graph.Node
	incrementalOutByNode          map[string][]*graph.Edge
	incrementalAttributionReindex []graph.EdgeReindex

	// lspHelper, when non-nil, is consulted before falling back to
	// AST heuristics for cross-file dispatch in languages whose
	// helper-reported extensions match (today: TS/JS/JSX/TSX via
	// tsserver). See lsp_helper.go for the contract. Set via
	// SetLSPHelper before ResolveAll runs.
	lspHelper LSPHelper
	// lspResolvePassBudget bounds when NEW helper attempts may start in the
	// deferred LSP batch of a whole-graph ResolveAll — it is not a phase
	// wall bound. Per-page spool/hydration/liveness overhead is bounded by
	// the expensive-path cutoff derived from it (4×, floor 60s): once
	// tripped, remaining spool pages drain record-only. Zero intentionally
	// means unlimited for compatibility and disables the cutoff. Individual
	// helper calls retain their own timeout, so attempts can overrun by at
	// most the one call already in flight.
	lspResolvePassBudget time.Duration

	// lspSpoolPageRows overrides the deferred-LSP spool page size; zero uses
	// resolvePendingPageRows. Test-only injection point so multi-page drain
	// behaviour is exercisable without thousand-row fixtures.
	lspSpoolPageRows int
	// lspNow overrides the clock the expensive-path cutoff reads; nil uses
	// time.Now. Test-only injection point — cutoff tests must not sleep.
	lspNow func() time.Time

	// hotCache retains node-by-ID and repository-scoped name-group lookups
	// across the pages of one resolver pass (see resolve_hot_cache.go). It is
	// mutated only from the pass's serial phases — page prepare, lookup warm,
	// and guard warm — never from parallel resolve workers.
	hotCache *resolveHotCache
	// placeholderSrcIdx caches, for one ResolveAll pass, the exact identities
	// of dataflow edges sourced from unresolved placeholders. Each compute
	// chunk refetches only its matching sites instead of decoding a shared
	// placeholder's complete adjacency again (see placeholder_sources.go).
	// Reset at pass start; touched only from the pass's serial apply phases.
	placeholderSrcIdx placeholderSourceIndex
	// lspDeferredRetry preserves only budget-skipped LSP work across
	// ResolveAll calls. This is required for heuristic-resolved edges: after
	// the heuristic rewrites To they no longer appear in
	// EdgesWithUnresolvedTarget, but still need the type-aware correction the
	// exhausted pass did not attempt. The cursor is the stable key of the first
	// skipped item, making the next bounded pass resume fairly even if the
	// candidate set changes between calls. All three fields are protected by
	// mu; retries run synchronously in the next compatible ResolveAll.
	lspDeferredRetry     map[deferredLSPWorkKey]deferredLSPEdge
	lspDeferredSpool     *deferredLSPSpool
	lspDeferredCursor    deferredLSPWorkKey
	lspDeferredCursorSet bool

	// lspIndex caches a (filePath, oneBasedLine) → *graph.Node
	// lookup table populated lazily on first LSP hit per pass so
	// matchNodeByLocation runs in O(1) instead of scanning every
	// node in the file. Cleared between passes.
	lspIndex   map[lspLocKey]*graph.Node
	lspIndexMu sync.RWMutex

	// npmAlias, when non-nil, rewrites a JS/TS import specifier that
	// matches an npm-alias dependency key in the importing file's
	// nearest-ancestor package.json. See npm_alias.go for the
	// contract. Set via SetNpmAliasResolver before ResolveAll runs.
	npmAlias NpmAliasResolver

	// npmDep, when non-nil, reports whether a bare JS/TS specifier is
	// declared by the importer as a dependency resolving outside the
	// repository. See npm_dependency.go for the contract. Set via
	// SetNpmDependencyLookup before ResolveAll runs.
	npmDep NpmDependencyLookup

	// pathAlias, when non-nil, expands a JS/TS tsconfig/jsconfig
	// `compilerOptions.paths` / `baseUrl` import specifier to the
	// repo-prefixed file stem it targets. See jsts_imports.go for the
	// contract. Set via SetPathAliasResolver before ResolveAll runs.
	pathAlias PathAliasResolver

	// workspaceMembers, when non-nil, maps a file path to the
	// package-manager workspace it belongs to. Used to break a
	// same-named import collision in favour of the candidate that
	// shares the importing file's workspace. See
	// workspace_membership.go for the contract. Set via
	// SetWorkspaceMembership before ResolveAll runs.
	workspaceMembers WorkspaceMembership

	// scope, when non-empty, restricts the next ResolveAll pass to the
	// pending edges that could resolve into one of the named repo
	// prefixes — the warm-restart optimisation that avoids a whole-graph
	// resolve when only a few of many tracked repos re-indexed. nil or
	// empty means whole-graph, exactly the pre-scoping behaviour. Set via
	// SetScope. Independent of any backend bulk-mode flag.
	scope map[string]struct{}

	// stampTerminal, when true, lets a FULL (unscoped) ResolveAll durably mark
	// the edges it concludes are permanently external / stdlib / definition-
	// less so a later SCOPED warm resolve can skip re-feeding them (see
	// terminal.go). Only the whole-graph master resolve — which has global
	// evidence — enables it; per-repo and single-file passes leave it false so
	// a partially-indexed graph never stamps a false "no definition". Set via
	// SetStampTerminal.
	stampTerminal bool
}

// lspLocKey identifies a node by (filePath, 1-based line) and is the
// key for lspIndex. Tsserver's textDocument/definition reports the
// declaration's start position, which graph.Node.StartLine matches.
type lspLocKey struct {
	filePath string
	line     int
}

// depModuleEntry pairs a Go module path (parsed from a dep:: contract
// node ID) with the compact target identity used by import resolution.
type depModuleEntry struct {
	modulePath string
	nodeID     string
}

// New creates a Resolver for the given store. The returned Resolver
// shares store.ResolveMutex() with every other Resolver built from
// the same Store, so their ResolveAll / ResolveFile calls serialise
// end-to-end across cross-repo / temporal / external passes.
func New(g graph.Store) *Resolver {
	return &Resolver{
		graph:                g,
		mu:                   g.ResolveMutex(),
		logger:               zap.NewNop(),
		lspResolvePassBudget: lspResolvePassBudgetFromEnv(),
	}
}

// SetLogger attaches a logger so ResolveAll emits pass-progress
// (pending count, periodic compute progress, compute/apply elapsed).
// A nil logger is replaced with a no-op so the resolver never panics
// when constructed without one (every direct caller of New gets Nop).
func (r *Resolver) SetLogger(l *zap.Logger) {
	if l == nil {
		l = zap.NewNop()
	}
	r.logger = l
}

// SetScope restricts the next ResolveAll pass to pending edges that could
// resolve into one of the given repo prefixes (see the scope field). A nil
// or empty map restores whole-graph resolution — byte-for-byte the
// pre-scoping behaviour. The scope persists across calls until reset,
// mirroring the other Set* configuration setters.
func (r *Resolver) SetScope(prefixes map[string]struct{}) {
	r.scope = prefixes
}

// SetStampTerminal enables durable terminal-edge stamping for the next FULL
// (unscoped) ResolveAll pass (see the stampTerminal field and terminal.go). It
// is a no-op on scoped passes, which lack the global evidence to conclude an
// edge is permanently unbindable. Only the whole-graph master resolve should
// enable it.
func (r *Resolver) SetStampTerminal(on bool) {
	r.stampTerminal = on
}

// SetGraph retargets the Resolver at a different Store. The indexer's
// in-memory shadow-swap path needs this: the Resolver is constructed
// against the disk Store at indexer-New time, but during IndexCtx the
// indexer reassigns its own graph pointer to an in-memory shadow.
// Without SetGraph the Resolver kept reading the (empty) disk Store
// and short-circuited on len(pending) == 0, silently disabling every
// resolver pass for backends that opt into the shadow swap.
//
// Holds the resolve mutex so a concurrent ResolveAll / ResolveFile
// can't observe a half-rotated graph reference, and switches mu to
// the new store's resolve mutex so subsequent passes serialise
// against any Resolver built directly on the new Store.
func (r *Resolver) SetGraph(g graph.Store) {
	if g == nil {
		return
	}
	oldMu := r.mu
	if oldMu != nil {
		oldMu.Lock()
	}
	r.graph = g
	r.mu = g.ResolveMutex()
	// Deferred entries hold edge pointers from the previous store. Carrying
	// them across a graph swap could reindex an unrelated/stale row.
	r.lspDeferredRetry = nil
	if r.lspDeferredSpool != nil {
		r.lspDeferredSpool.close()
		r.lspDeferredSpool = nil
	}
	r.lspDeferredCursor = deferredLSPWorkKey{}
	r.lspDeferredCursorSet = false
	if oldMu != nil {
		oldMu.Unlock()
	}
}

// ResolveAll resolves all unresolved edges in the graph.
//
// Edge resolution is partitioned across runtime.NumCPU() workers.
// Each worker iterates a disjoint slice and calls resolveEdge, which:
//
//   - mutates only its own e.To field (per-edge ownership, no
//     write-write races between workers),
//   - reads graph state via Find/Get methods that take per-shard
//     RLocks (concurrent-safe),
//   - calls graph.ReindexEdge which acquires write locks on three
//     specific shards (e.From, oldTo, newTo) — concurrency between
//     workers serialises only on shard collisions, not globally.
//
// Stats are aggregated per-worker and summed at the end so
// `Resolved++` etc. don't race. r.mu serialises ResolveAll calls
// against each other; nothing inside this function takes that lock.
func (r *Resolver) ResolveAll() *ResolveStats {
	stats, err := r.ResolveAllContext(context.Background())
	if err != nil {
		r.logger.Error("resolver: ResolveAll", zap.Error(err))
	}
	return stats
}

// ResolveAllContext is ResolveAll with cancellation propagated through the
// native unresolved-edge pager. On error it returns the counts accumulated up
// to the failure and does not publish compute readiness or run refinement
// tails. ResolveAll retains the historical best-effort API for callers that do
// not own a lifecycle context.
func (r *Resolver) ResolveAllContext(ctx context.Context) (*ResolveStats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return &ResolveStats{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return &ResolveStats{}, err
	}

	r.logUnresolvedFrontier("start")
	defer r.logUnresolvedFrontier("end")

	// A full pass follows wholesale graph mutation (bulk index, retrack)
	// no scoped reconcile ever named — rebuild the C# visibility state
	// from scratch. Scoped resolves maintain it incrementally instead.
	r.clearCSharpVisibilityCaches()

	// Fresh placeholder-source set per pass: dataflow edges indexed since the
	// previous pass must be visible, and a moved source must not linger.
	r.placeholderSrcIdx = placeholderSourceIndex{}

	// Keep the unresolved corpus disk-resident. SQLite captures a rowid
	// high-water mark and keyset-pages beneath it; legacy stores are consumed
	// through the early-stoppable iterator. Scope and terminal predicates are
	// applied to each page before any lookup cache is built. The initial scan
	// deliberately precedes every workspace index and backend bulk pass: a warm
	// no-op pays one indexed pending query, not several whole-graph scans.
	pendingStream := r.prepareResolveAllStream(ctx)
	defer pendingStream.close()
	// Push the scoped pass's row filters into the store: ScopeFilter drops
	// rows edgeInResolveScope provably never reconsiders, and SkipTerminal
	// additionally drops durably-stamped edges the pass would skip. Both are
	// tested on the generated from_repo / to_repo_unresolved columns (SQL
	// mirrors of the Go prefix helpers, parity-asserted), with NULL shapes
	// failing open. The Go-side filters below still run and stay the
	// semantic authority — they cover legacy pagers, the shapes only Go can
	// parse, and stamps that only exist in memory.
	if len(r.scope) > 0 {
		for prefix := range r.scope {
			pendingStream.scan.ScopeAnchors = append(pendingStream.scan.ScopeAnchors, prefix)
		}
		sort.Strings(pendingStream.scan.ScopeAnchors)
		pendingStream.scan.ScopeFilter = true
		pendingStream.scan.SkipTerminal = !warmupFullResolve()
	}
	pendingBefore := pendingStream.scan.PendingBefore
	if !pendingStream.countKnown {
		pendingBefore = 0
	}
	pendingAfter := 0
	var pendingTotal atomic.Int64
	var pendingLoaded atomic.Int64
	pendingTotal.Store(int64(pendingBefore))
	terminalSkipped := 0
	terminalLoaded := 0
	loadElapsed := time.Duration(0)
	streamDone := false
	loadPendingPage := func() ([]*graph.Edge, error) {
		loadStart := time.Now()
		defer func() { loadElapsed += time.Since(loadStart) }()
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			raw, done, err := pendingStream.nextPage()
			if err != nil {
				return nil, err
			}
			streamDone = done
			if !pendingStream.countKnown {
				pendingBefore += len(raw)
				pendingTotal.Store(int64(pendingBefore))
			}
			pending := raw
			if len(r.scope) > 0 {
				pending = filterPendingByScope(pending, r.scope)
			}
			if len(r.scope) > 0 && !warmupFullResolve() {
				var skipped int
				pending, skipped = filterTerminalSkip(pending, r.scope)
				terminalSkipped += skipped
			}
			// Durably-terminal edges that still enter compute — the volume a
			// terminal skip on this pass's shape would have saved.
			for _, e := range pending {
				if edgeTerminalFlag(e) {
					terminalLoaded++
				}
			}
			pendingAfter += len(pending)
			pendingLoaded.Store(int64(pendingAfter))
			if len(pending) > 0 || done {
				return pending, nil
			}
		}
	}
	pending, pendingErr := loadPendingPage()
	if pendingErr != nil {
		return &ResolveStats{PendingBefore: pendingBefore, PendingAfter: pendingAfter}, pendingErr
	}
	if len(pending) == 0 && streamDone && !r.hasDeferredLSPRetryForScope() {
		return &ResolveStats{PendingBefore: pendingBefore, PendingAfter: pendingAfter}, nil
	}

	passIndexes := newResolveAllPassIndexes(r)
	defer passIndexes.close()

	// Pass-scoped hot lookup cache: answers repeat node/name hydration across
	// pages (and the guard's re-reads) without re-touching the store. Torn
	// down with the pass; see resolve_hot_cache.go for the immutability and
	// flush rules that make this safe.
	if resolveHotCacheEnabled() {
		r.hotCache = newResolveHotCache(resolveHotCacheBudgetBytes(), len(pending))
		defer func() {
			if c := r.hotCache; c != nil {
				r.logger.Info("resolver: hot cache stats",
					zap.Int64("node_hits", c.nodeHits),
					zap.Int64("node_misses", c.nodeMisses),
					zap.Int64("name_hits", c.nameHits),
					zap.Int64("name_misses", c.nameMisses))
			}
			r.hotCache = nil
		}()
	}

	passStart := time.Now()
	// The first page fetched above predates the pass clock: drop it from
	// page_load so the interior buckets stay inside compute_loop — on a
	// scoped pass that first fetch dominates load time and would overrun it.
	loadElapsed = 0
	r.logger.Info("resolver: pass start",
		zap.Int("pending", pendingBefore),
		zap.Int("first_page", len(pending)),
		zap.Int("terminal_skipped", terminalSkipped),
		zap.Bool("backend_bulk", backendResolverEnabled()),
		zap.String("first_page_shapes", pendingShapeSummary(pending)))
	// Diagnostic: capture a CPU profile of the first full (unscoped) resolve
	// pass when GORTEX_RESOLVE_CPUPROFILE names a path. Env-gated and one-shot
	// so it never touches steady-state resolution.
	if p := os.Getenv("GORTEX_RESOLVE_CPUPROFILE"); p != "" && len(r.scope) == 0 &&
		resolveProfileStarted.CompareAndSwap(false, true) {
		if f, err := os.Create(p); err == nil {
			if pprof.StartCPUProfile(f) == nil {
				defer pprof.StopCPUProfile()
			}
		}
	}
	var processed atomic.Int64
	progressDone := make(chan struct{})
	var progressOnce sync.Once
	stopProgress := func() {
		progressOnce.Do(func() { close(progressDone) })
	}
	defer stopProgress()
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-t.C:
				r.logger.Info("resolver: compute progress",
					zap.Int64("processed", processed.Load()),
					zap.Int64("pending_loaded", pendingLoaded.Load()),
					zap.Int64("pending_total", pendingTotal.Load()),
					zap.Duration("elapsed", time.Since(passStart)))
			}
		}
	}()

	// Chunked compute + apply: process pending in super-chunks and release the
	// resolve mutex between chunks so an interactive single-file edit can
	// interleave instead of waiting out the whole pass. Each chunk's
	// compute+apply runs under the lock (atomic, fresh reads); only the
	// inter-chunk gap is unlocked, where the pass holds no partial graph state.
	// resolveEdge mutates a clone, so the live edge is only written at the
	// per-chunk apply, which skips an edge a yielded edit evicted (reindexing
	// it half-resurrects it and can panic). Accumulated jobs/stats feed the
	// post-resolve passes once, after the loop. GORTEX_RESOLVE_CHUNK=0 restores
	// the whole-pass-locked path.
	r.validateLiveness = resolveChunkEnabled()
	// Bulk mode: the parallel workers below skip the synchronous per-edge LSP
	// round-trip (see the bulkMode field) and instead collect the still-
	// unresolved LSP-eligible edges into deferredLSP, which one deferred batch
	// binds after the loop. Reset unconditionally on return so a leaked true
	// can never disable inline LSP on a later single-file ResolveFile.
	r.bulkMode = true
	defer func() { r.bulkMode = false }()
	if r.lspHelper != nil && r.lspDeferredSpool == nil {
		var err error
		r.lspDeferredSpool, err = newDeferredLSPSpool()
		if err != nil {
			r.logger.Error("resolver: create deferred LSP spool", zap.Error(err))
			return &ResolveStats{PendingBefore: pendingBefore, PendingAfter: pendingAfter}, err
		}
		if len(r.lspDeferredRetry) > 0 {
			carried := make([]deferredLSPEdge, 0, len(r.lspDeferredRetry))
			for _, deferred := range r.lspDeferredRetry {
				deferred.carried = true
				carried = append(carried, deferred)
			}
			if err := r.lspDeferredSpool.append(carried); err != nil {
				r.logger.Error("resolver: migrate deferred LSP retries", zap.Error(err))
				return &ResolveStats{PendingBefore: pendingBefore, PendingAfter: pendingAfter}, err
			}
			r.lspDeferredRetry = nil
		}
	}
	superChunk := resolvePendingPageRows
	if r.validateLiveness && resolveChunkSize() < superChunk {
		superChunk = resolveChunkSize()
	}
	if superChunk < 1 {
		superChunk = 1
	}
	guardSpool, guardSpoolErr := newResolveGuardSpool()
	if guardSpoolErr != nil {
		r.logger.Error("resolver: create guard spool", zap.Error(guardSpoolErr))
		return &ResolveStats{PendingBefore: pendingBefore, PendingAfter: pendingAfter}, guardSpoolErr
	}
	defer guardSpool.close()
	guardRepos := make(map[string]struct{})
	total := &ResolveStats{}
	resolveError := func(err error) (*ResolveStats, error) {
		total.PendingBefore = pendingBefore
		total.PendingAfter = pendingAfter
		return total, err
	}
	reindexTotal := 0
	// Conversion-vs-churn split of the reindex volume. A pass once applied a
	// 666k-entry reindex batch while net pending moved only 30k — without
	// this split there is no way to tell corpus conversion (an unresolved
	// target became a real node) from identity churn (kind promotions,
	// origin upgrades, re-corrections of already-resolved edges).
	reindexConversions := 0
	reindexChurn := 0
	churnRetargeted := 0
	churnKindPromoted := 0
	churnAttrsOnly := 0
	warmElapsed := time.Duration(0)
	prepareElapsed := time.Duration(0)
	workersElapsed := time.Duration(0)
	applyElapsed := time.Duration(0)
	applyLivenessElapsed := time.Duration(0)
	applyNoteElapsed := time.Duration(0)
	applyStoreElapsed := time.Duration(0)
	applyPlaceholderElapsed := time.Duration(0)
	applyGuardSpoolElapsed := time.Duration(0)
	for {
		if err := ctx.Err(); err != nil {
			return resolveError(err)
		}
		// The pending page is a stable edge snapshot taken while mu is held.
		// Production stores expose a cheap edge mutation revision. As long as
		// it stays unchanged across yields, no edge in this page can have been
		// evicted or replaced and the expensive liveness query is unnecessary.
		// Once an interleave mutates the store, retain exact validation for all
		// remaining edges from this old page; only a freshly loaded page resets
		// the fast path. Stores without the optional capability conservatively
		// keep the former per-chunk validation.
		pageRevision, pageRevisionKnown := loadEdgeMutationRevision(r.graph)
		pageMutationRevision, pageMutationRevisionKnown := loadMutationRevision(r.graph)
		pageLivenessRequired := false
		var pageLiveness resolveJobLiveness
		var pageDeferredLSP []deferredLSPEdge
		if len(pending) > 0 {
			prepareStart := time.Now()
			sources := passIndexes.prepare(pending)
			prepareElapsed += time.Since(prepareStart)
			warmStart := time.Now()
			r.warmLookupCacheWithSources(pending, sources)
			warmElapsed += time.Since(warmStart)
		}
		for base := 0; base < len(pending); base += superChunk {
			if err := ctx.Err(); err != nil {
				return resolveError(err)
			}
			hi := base + superChunk
			if hi > len(pending) {
				hi = len(pending)
			}
			scPending := pending[base:hi]

			if r.validateLiveness && pageRevisionKnown {
				currentRevision, _ := loadEdgeMutationRevision(r.graph)
				if currentRevision != pageRevision {
					// One set-oriented read covers every old-snapshot edge still
					// present in this page. Reuse it across later chunks until a
					// subsequent real interleave advances the revision again.
					pageLiveness = loadEdgeLiveness(r.graph, pending[base:])
					pageLivenessRequired = true
					pageRevision = currentRevision
				}
			}

			workersStart := time.Now()
			workers := runtime.NumCPU()
			if workers < 1 {
				workers = 1
			}
			if workers > len(scPending) {
				workers = len(scPending)
			}
			perWorkerStats := make([]ResolveStats, workers)
			perWorkerJobs := make([][]reindexJob, workers)
			perWorkerDeferred := make([][]deferredLSPEdge, workers)
			var wg sync.WaitGroup
			chunk := (len(scPending) + workers - 1) / workers
			for w := 0; w < workers; w++ {
				start := w * chunk
				end := start + chunk
				if end > len(scPending) {
					end = len(scPending)
				}
				if start >= end {
					continue
				}
				wg.Add(1)
				go func(idx int, slice []*graph.Edge) {
					defer wg.Done()
					ws := &perWorkerStats[idx]
					jobs := make([]reindexJob, 0, len(slice))
					var deferred []deferredLSPEdge
					for _, e := range slice {
						if ctx.Err() != nil {
							break
						}
						// Capture LSP eligibility + the pre-heuristic identifier
						// BEFORE resolveEdge runs: e.To is still the `unresolved::`
						// stub here (the real edge is rewritten only in the apply
						// phase below), so this sees the pre-heuristic target even
						// for an edge the heuristic then confidently (mis)binds.
						// Collecting EVERY LSP-eligible edge — not only the ones the
						// heuristic leaves unresolved — is what preserves the LSP-
						// first override the inline path applies: the post-loop batch
						// re-binds via the type-aware helper, correcting a confident-
						// but-wrong heuristic bind (see resolveDeferredLSP).
						lspTarget, lspElig := r.lspDeferTarget(e)
						oldKind := e.Kind
						clone := cloneEdgeForResolve(e)
						oldTo, changed := r.resolveEdge(clone, ws)
						processed.Add(1)
						if changed {
							// A now-resolved edge sheds any durable terminal-skip
							// flag it carried (full self-healing pass): it has left
							// the pending set, so a later scoped pass must not treat
							// the flag as live. The cleared Meta rides the reindex
							// below (To changed => the row is rewritten).
							if !graph.IsUnresolvedTarget(clone.To) {
								clearEdgeTerminal(clone)
							}
							job := reindexJob{
								edge:       e,
								oldTo:      oldTo,
								oldKind:    oldKind,
								newTo:      clone.To,
								kind:       clone.Kind,
								crossRepo:  clone.CrossRepo,
								confidence: clone.Confidence,
								origin:     clone.Origin,
								meta:       clone.Meta,
							}
							if r.validateLiveness {
								// resolveEdge mutates only clone, so e still holds the
								// exact persisted payload observed before resolution.
								job.preResolution = snapshotPersistedEdge(e)
							}
							jobs = append(jobs, job)
						}
						if lspElig {
							// Bulk mode skipped the inline LSP round-trip; collect the
							// edge for the post-loop deferred batch so the helper is
							// consulted off the parallel worker barrier. Independent of
							// `changed`: a heuristic-resolved edge is still deferred so
							// LSP retains override authority.
							deferred = append(deferred, deferredLSPEdge{edge: e, target: lspTarget})
						}
						releaseResolverClone(clone)
					}
					perWorkerJobs[idx] = jobs
					perWorkerDeferred[idx] = deferred
				}(w, scPending[start:end])
			}
			wg.Wait()
			workersElapsed += time.Since(workersStart)
			if err := ctx.Err(); err != nil {
				return resolveError(err)
			}
			applyStart := time.Now()

			// Apply this chunk's mutations under the lock. An edit during a PRIOR
			// inter-chunk yield may have evicted an edge this chunk resolved;
			// reindexing it would half-resurrect it, so drop it (filter in place
			// so the guard spool carries only applied jobs for the post-pass).
			// Resolve liveness in one set-oriented store query for the chunk;
			// SQLite must never pay one EdgeExists SELECT per successful edge.
			var liveJobs resolveJobLiveness
			validateLiveJobs := false
			if r.validateLiveness {
				livenessStart := time.Now()
				switch {
				case !pageRevisionKnown:
					// Unknown stores cannot prove that the page stayed stable.
					liveJobs = loadResolveJobLiveness(r.graph, perWorkerJobs)
					validateLiveJobs = true
				case pageLivenessRequired:
					liveJobs = pageLiveness
					validateLiveJobs = true
				}
				applyLivenessElapsed += time.Since(livenessStart)
			}
			reindexBatch := make([]graph.EdgeReindex, 0, resolveJobCount(perWorkerJobs))
			for i := range perWorkerJobs {
				kept := perWorkerJobs[i][:0]
				for _, j := range perWorkerJobs[i] {
					if validateLiveJobs && !liveJobs.contains(j) {
						continue
					}
					j.edge.To = j.newTo
					j.edge.Kind = j.kind
					j.edge.CrossRepo = j.crossRepo
					j.edge.Confidence = j.confidence
					j.edge.Origin = j.origin
					j.edge.Meta = j.meta
					reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: j.edge, OldTo: j.oldTo, OldKind: j.oldKind})
					if graph.IsUnresolvedTarget(j.oldTo) && !graph.IsUnresolvedTarget(j.newTo) {
						reindexConversions++
					} else {
						reindexChurn++
						switch churnShape(j.oldTo, j.newTo, j.oldKind, j.kind) {
						case churnShapeRetarget:
							churnRetargeted++
						case churnShapeKind:
							churnKindPromoted++
						default:
							churnAttrsOnly++
						}
					}
					kept = append(kept, j)
				}
				perWorkerJobs[i] = kept
			}
			if len(reindexBatch) > 0 {
				noteStart := time.Now()
				r.noteImportEdgeReindexes(reindexBatch)
				applyNoteElapsed += time.Since(noteStart)
				storeStart := time.Now()
				r.graph.ReindexEdges(reindexBatch)
				applyStoreElapsed += time.Since(storeStart)
				placeholderStart := time.Now()
				reconcilePlaceholderSources(r.graph, &r.placeholderSrcIdx, reindexBatch)
				applyPlaceholderElapsed += time.Since(placeholderStart)
				for _, ri := range reindexBatch {
					r.noteRetargetedCall(ri.Edge)
				}
				reindexTotal += len(reindexBatch)
				if pageRevisionKnown {
					// Ignore this pass's own committed mutations. A later delta
					// can then only come from work that ran while mu was yielded.
					pageRevision, _ = loadEdgeMutationRevision(r.graph)
				}
				if pageMutationRevisionKnown {
					pageMutationRevision, _ = loadMutationRevision(r.graph)
				}
			}
			if guardSpoolErr == nil {
				spoolStart := time.Now()
				guardSpoolErr = guardSpool.appendJobs(perWorkerJobs)
				applyGuardSpoolElapsed += time.Since(spoolStart)
				if guardSpoolErr != nil {
					r.logger.Error("resolver: append guard spool", zap.Error(guardSpoolErr))
				}
			}
			for i := range perWorkerJobs {
				for j := range perWorkerJobs[i] {
					if guardCandidateJob(&perWorkerJobs[i][j]) {
						guardRepos[graph.RepoPrefixOfID(perWorkerJobs[i][j].edge.From)] = struct{}{}
					}
				}
			}
			applyElapsed += time.Since(applyStart)
			for i := range perWorkerStats {
				total.Resolved += perWorkerStats[i].Resolved
				total.Unresolved += perWorkerStats[i].Unresolved
				total.External += perWorkerStats[i].External
			}
			for i := range perWorkerDeferred {
				pageDeferredLSP = append(pageDeferredLSP, perWorkerDeferred[i]...)
			}

			// Hand the resolve mutex to any waiting interactive edit before the
			// next chunk. Drop bulk mode across the hand-off so an interleaving
			// single-file ResolveFile on a shared instance resolves LSP-first.
			if r.validateLiveness && (hi < len(pending) || !streamDone) {
				r.bulkMode = false
				yieldHook := r.chunkYieldHook
				r.mu.Unlock()
				if yieldHook != nil {
					yieldHook()
				}
				runtime.Gosched()
				r.mu.Lock()
				if err := ctx.Err(); err != nil {
					return resolveError(err)
				}
				forceRefresh := false
				if pageMutationRevisionKnown {
					currentRevision, _ := loadMutationRevision(r.graph)
					forceRefresh = currentRevision != pageMutationRevision
					if forceRefresh {
						// The refreshed indexes/cache now represent this shared-store
						// generation. Keep the edge-only baseline untouched so the
						// next chunk still performs exact stale-job validation.
						pageMutationRevision = currentRevision
					}
				}
				if forceRefresh {
					// The shared-store generation changed under us — the
					// pass-lifetime C# global-usings index may describe
					// the old generation; rebuild it with the rest.
					r.clearCSharpVisibilityCaches()
				}
				passIndexes.refreshAfterInterleave(pending, forceRefresh)
				r.bulkMode = true
			}
		}
		if r.lspDeferredSpool != nil && len(pageDeferredLSP) > 0 {
			if err := r.lspDeferredSpool.append(pageDeferredLSP); err != nil {
				r.logger.Error("resolver: append deferred LSP spool", zap.Error(err))
			}
		}
		r.clearLookupCache()
		passIndexes.clearPage()
		if streamDone {
			break
		}
		var err error
		pending, err = loadPendingPage()
		if err != nil {
			return resolveError(err)
		}
	}
	stopProgress()
	loopElapsed := time.Since(passStart) - warmElapsed
	if err := ctx.Err(); err != nil {
		return resolveError(err)
	}

	// Publish compute readiness BEFORE the deferred LSP batch. The batch is
	// verification/override work whose store-standing yield measured 2,409
	// edges of a 1.29M pending set (0.19%) while costing 450–512s of wall on
	// a cold pass — it has no business inside the time-to-queryable window.
	// Like the guard and cross-repo tails that already run after this hook,
	// its rewrites land on an already-queryable graph.
	if r.OnComputeDone != nil {
		r.OnComputeDone()
	}

	// Deferred LSP work is replayed in the same stable source-key order as the
	// former whole-pass slice, but from a disk-backed dedup spool. Completed
	// keys are deleted in batches; budget-skipped keys remain carried in the
	// spool for the next compatible pass, so retry state never grows a Go map.
	lspDeferred := 0
	lspResult := deferredLSPBatchResult{}
	lspStart := time.Now()
	lspCtx := ctx
	lspPassBudget := &deferredLSPPassBudget{duration: r.lspResolvePassBudget}
	// The attempt budget bounds helper calls only. Every other per-page cost —
	// spool reads, edge hydration, liveness projection, bookkeeping — runs
	// outside its clock, and on a cold-cache host those pages were measured at
	// ~11s each with zero attempts. The expensive-path cutoff below is the
	// bound on that work: once it trips (or the attempt budget exhausts), the
	// remaining spool pages take a record-only drain — exclusions and carried
	// marks straight from spool keys, no hydration, no liveness. The drain
	// itself remains unbounded (it is a cheap key scan), so this is a cutoff
	// of the expensive path, NOT a whole-phase wall bound. Budget zero keeps
	// today's unlimited mode: no attempt budget, no cutoff.
	lspClock := r.lspNow
	if lspClock == nil {
		lspClock = time.Now
	}
	var lspCutoffAt time.Time
	if r.lspResolvePassBudget > 0 {
		wall := r.lspResolvePassBudget * 4
		if wall < r.lspResolvePassBudget {
			// Saturate the multiply for absurd configured budgets.
			wall = time.Duration(1<<63 - 1)
		}
		if wall < lspPhaseCutoffFloor {
			wall = lspPhaseCutoffFloor
		}
		if anchor := lspClock(); anchor.Add(wall).After(anchor) {
			lspCutoffAt = anchor.Add(wall)
		}
	}
	lspCutoffTripped := func() bool {
		return !lspCutoffAt.IsZero() && !lspClock().Before(lspCutoffAt)
	}
	lspPageRows := r.lspSpoolPageRows
	if lspPageRows <= 0 {
		lspPageRows = resolvePendingPageRows
	}
	var (
		lspPhaseCutoff    bool
		lspPagesHydrated  int
		lspPagesDrained   int
		lspRecordsDrained int
		lspSpoolReadDur   time.Duration
		lspHydrateDur     time.Duration
	)
	if r.lspDeferredSpool != nil {
		var start *deferredLSPWorkKey
		if r.lspDeferredCursorSet {
			cursor := r.lspDeferredCursor
			start = &cursor
		}
		iterator := r.lspDeferredSpool.iterator(start)
		attemptCursorSet := false
		drainCursorPinned := false
		excludeDrained := func(keys []deferredLSPWorkKey) {
			if len(keys) == 0 {
				return
			}
			if lspResult.terminalityExcluded == nil {
				lspResult.terminalityExcluded = make(map[deferredLSPWorkKey]struct{}, len(keys))
			}
			for _, key := range keys {
				lspResult.terminalityExcluded[key] = struct{}{}
			}
			lspRecordsDrained += len(keys)
			if !drainCursorPinned && !attemptCursorSet {
				// Cutoff before any attempt was skipped: pin the resume
				// cursor to the first unprocessed (drained) key so the next
				// pass starts exactly where this one stopped.
				r.lspDeferredCursor = keys[0]
				r.lspDeferredCursorSet = true
				drainCursorPinned = true
			}
		}
		// drainRecordsPage retires one already-read page without the
		// expensive path. Hydration knowledge is respected: only when the
		// page WAS hydrated may its proven-stale rows be deleted; an
		// unhydrated row can never be classified from its snapshot alone and
		// is always retained as carried.
		drainRecordsPage := func(records []lspSpoolRecord, staleKeys []deferredLSPWorkKey, hydrated bool) bool {
			keys := make([]deferredLSPWorkKey, 0, len(records))
			if hydrated && len(staleKeys) > 0 {
				staleSet := make(map[deferredLSPWorkKey]struct{}, len(staleKeys))
				for _, key := range staleKeys {
					staleSet[key] = struct{}{}
				}
				for _, record := range records {
					if _, dead := staleSet[record.key]; dead {
						continue
					}
					keys = append(keys, record.key)
				}
				if err := r.lspDeferredSpool.deleteKeys(staleKeys); err != nil {
					r.logger.Error("resolver: delete stale deferred LSP work", zap.Error(err))
					return false
				}
			} else {
				for _, record := range records {
					keys = append(keys, record.key)
				}
			}
			excludeDrained(keys)
			if err := r.lspDeferredSpool.markCarried(keys); err != nil {
				r.logger.Error("resolver: mark drained deferred LSP work", zap.Error(err))
				return false
			}
			lspPagesDrained++
			return true
		}
		drainKeySegment := func(from, to *deferredLSPWorkKey) bool {
			after := from
			for {
				readStart := time.Now()
				keys, segDone, err := r.lspDeferredSpool.keysPage(after, to, lspPageRows)
				lspSpoolReadDur += time.Since(readStart)
				if err != nil {
					r.logger.Error("resolver: drain deferred LSP spool keys", zap.Error(err))
					return false
				}
				if len(keys) == 0 {
					return true
				}
				excludeDrained(keys)
				lspPagesDrained++
				last := keys[len(keys)-1]
				after = &last
				if segDone {
					return true
				}
			}
		}
		// drainRemaining retires every not-yet-read spool row: exclusions via
		// key-only pages (no payload decode), then ONE carried range-update
		// per traversal segment. Rows stay durable in the spool for the next
		// pass — work may be retried unnecessarily, but is never lost.
		drainRemaining := func() {
			from := iterator.after
			if iterator.wrapped || iterator.start == nil {
				var to *deferredLSPWorkKey
				if iterator.wrapped {
					to = iterator.start
				}
				if drainKeySegment(from, to) {
					if err := r.lspDeferredSpool.markCarriedRange(from, to); err != nil {
						r.logger.Error("resolver: mark drained deferred LSP range", zap.Error(err))
					}
				}
				return
			}
			// Un-wrapped traversal with a resume cursor: the remaining rows
			// are the tail (after, end] plus the wrapped head [begin, start).
			if !drainKeySegment(from, nil) {
				return
			}
			if err := r.lspDeferredSpool.markCarriedRange(from, nil); err != nil {
				r.logger.Error("resolver: mark drained deferred LSP range", zap.Error(err))
				return
			}
			if drainKeySegment(nil, iterator.start) {
				if err := r.lspDeferredSpool.markCarriedRange(nil, iterator.start); err != nil {
					r.logger.Error("resolver: mark drained deferred LSP range", zap.Error(err))
				}
			}
		}
		for {
			spoolReadStart := time.Now()
			records, done, err := iterator.next(lspPageRows)
			lspSpoolReadDur += time.Since(spoolReadStart)
			if err != nil {
				r.logger.Error("resolver: read deferred LSP spool", zap.Error(err))
				break
			}
			if len(records) == 0 && done {
				break
			}
			// Cutoff site 1 — after the spool read, before hydration.
			if lspResult.budgetExhausted || lspCutoffTripped() {
				if !lspResult.budgetExhausted {
					lspPhaseCutoff = true
				}
				if drainRecordsPage(records, nil, false) && !done {
					drainRemaining()
				}
				break
			}
			hydrateStart := time.Now()
			edges, stale := lspEdgesFromRecords(r.graph, records, r.scope)
			lspHydrateDur += time.Since(hydrateStart)
			lspDeferred += len(edges)
			lspPagesHydrated++
			// Cutoff site 2 — after hydration, before the liveness projection
			// inside the batch resolver.
			if lspCutoffTripped() {
				lspPhaseCutoff = true
				if drainRecordsPage(records, stale, true) && !done {
					drainRemaining()
				}
				break
			}
			pageResult := r.resolveDeferredLSPWithPassBudget(lspCtx, edges, lspPassBudget)
			if pageResult.budgetExhausted && pageResult.skipped > 0 {
				// resolveDeferredLSPWithPassBudget pinned the resume cursor
				// to its first skipped key.
				attemptCursorSet = true
			}
			lspResult.newlyResolved += pageResult.newlyResolved
			lspResult.resolved += pageResult.resolved
			lspResult.attempted += pageResult.attempted
			lspResult.skipped += pageResult.skipped
			lspResult.budgetExhausted = lspResult.budgetExhausted || pageResult.budgetExhausted
			lspResult.livenessDur += pageResult.livenessDur
			lspResult.attemptsDur += pageResult.attemptsDur
			// Merge the skip exclusions: terminal stamping runs even on a
			// budget-exhausted pass and must see every edge whose LSP verdict
			// is still pending, across all spool pages.
			if len(pageResult.terminalityExcluded) > 0 {
				if lspResult.terminalityExcluded == nil {
					lspResult.terminalityExcluded = make(map[deferredLSPWorkKey]struct{}, len(pageResult.terminalityExcluded))
				}
				for key := range pageResult.terminalityExcluded {
					lspResult.terminalityExcluded[key] = struct{}{}
				}
			}
			// A skipped edge must not remain durably terminal: it still has
			// higher-quality LSP work queued. Clear any prior full-pass stamp in
			// one batch; successful heuristic resolutions already cleared it.
			terminalClears := make([]graph.EdgeReindex, 0, len(pageResult.retry))
			for _, edge := range edges {
				key := deferredLSPWorkKeyFor(edge)
				if _, retry := pageResult.retry[key]; !retry || edge.edge == nil || !edgeTerminalFlag(edge.edge) {
					continue
				}
				oldTo := edge.edge.To
				clearEdgeTerminal(edge.edge)
				terminalClears = append(terminalClears, graph.EdgeReindex{Edge: edge.edge, OldTo: oldTo})
			}
			if len(terminalClears) > 0 {
				r.noteImportEdgeReindexes(terminalClears)
				r.graph.ReindexEdges(terminalClears)
			}

			completed := append([]deferredLSPWorkKey(nil), stale...)
			carried := make([]deferredLSPWorkKey, 0, len(pageResult.retry))
			for _, edge := range edges {
				key := deferredLSPWorkKeyFor(edge)
				if _, retry := pageResult.retry[key]; retry {
					if edge.edge != nil && graph.IsUnresolvedTarget(edge.edge.To) {
						// The graph's normal pending set is already the durable
						// retry queue for an unresolved edge.
						completed = append(completed, key)
					} else {
						carried = append(carried, key)
					}
				} else {
					completed = append(completed, key)
				}
			}
			if err := r.lspDeferredSpool.markCarried(carried); err != nil {
				r.logger.Error("resolver: mark deferred LSP retries", zap.Error(err))
				break
			}
			if err := r.lspDeferredSpool.deleteKeys(completed); err != nil {
				r.logger.Error("resolver: delete completed deferred LSP work", zap.Error(err))
				break
			}
			if done {
				break
			}
		}
		if r.lspDeferredSpool.count() == 0 {
			r.lspDeferredSpool.close()
			r.lspDeferredSpool = nil
		}
	}
	lspElapsed := time.Since(lspStart)
	if lspResult.budgetExhausted || lspPhaseCutoff {
		r.logger.Warn("resolver: deferred LSP expensive path stopped early",
			zap.Bool("attempt_budget_exhausted", lspResult.budgetExhausted),
			zap.Bool("phase_cutoff_triggered", lspPhaseCutoff),
			zap.Duration("budget", r.lspResolvePassBudget),
			zap.Duration("elapsed", lspElapsed),
			zap.Int("attempted", lspResult.attempted),
			zap.Int("resolved", lspResult.resolved),
			zap.Int("skipped", lspResult.skipped),
			zap.Int("pages_drained", lspPagesDrained),
			zap.Int("records_drained", lspRecordsDrained),
			zap.String("bound", "attempt budget bounds helper calls; the cutoff stops per-page hydration and liveness — the record-only drain that follows is an unbounded cheap key scan"))
	}
	// Bulk mode covers only the parallel compute + the deferred LSP batch; the
	// guard and tail attribution passes below run identically to the single-
	// file path. (The deferred defer() is the panic-safety net.)
	r.bulkMode = false
	total.LSPDeferred = lspDeferred
	total.LSPAttempted = lspResult.attempted
	total.LSPResolved = lspResult.resolved
	total.LSPBudgetSkipped = lspResult.skipped
	total.LSPBudgetExhausted = lspResult.budgetExhausted
	if err := ctx.Err(); err != nil {
		return resolveError(err)
	}

	computeElapsed := time.Since(passStart)
	r.logger.Info("resolver: compute done",
		zap.Int("pending", pendingAfter),
		zap.Int("reindex_batch", reindexTotal),
		zap.Int("reindex_conversions", reindexConversions),
		zap.Int("reindex_churn", reindexChurn),
		zap.Int("churn_retargeted", churnRetargeted),
		zap.Int("churn_kind_promoted", churnKindPromoted),
		zap.Int("churn_attrs_only", churnAttrsOnly),
		zap.Int("super_chunk", superChunk),
		zap.Int("lsp_deferred", lspDeferred),
		zap.Int("lsp_attempted", lspResult.attempted),
		zap.Int("lsp_batch_resolved", lspResult.resolved),
		zap.Int("lsp_budget_skipped", lspResult.skipped),
		zap.Bool("lsp_budget_exhausted", lspResult.budgetExhausted),
		zap.Bool("lsp_phase_cutoff", lspPhaseCutoff),
		zap.Int("lsp_pages_hydrated", lspPagesHydrated),
		zap.Int("lsp_pages_drained", lspPagesDrained),
		zap.Int("lsp_records_drained", lspRecordsDrained),
		zap.Duration("lsp_spool_read", lspSpoolReadDur),
		zap.Duration("lsp_hydrate", lspHydrateDur),
		zap.Duration("lsp_liveness", lspResult.livenessDur),
		zap.Duration("lsp_attempts", lspResult.attemptsDur),
		zap.Duration("lsp_budget", r.lspResolvePassBudget),
		zap.Duration("warm_lookup", warmElapsed),
		zap.Duration("compute_loop", loopElapsed),
		zap.Duration("deferred_lsp", lspElapsed),
		zap.Duration("elapsed", computeElapsed))

	tailStart := time.Now()

	// The serial tail below (guard → attribution → lang dispatch → terminal
	// stamping) historically ran for minutes between "compute done" and its
	// retrospective breakdown lines — an unattributable silent span for
	// anyone watching the log. Each phase now announces itself up front.
	tailPhase := func(phase string) {
		r.logger.Info("resolver: tail phase starting", zap.String("phase", phase))
	}

	// Cross-package name-match guard. The heuristic fallbacks above can
	// resolve a call by name alone to a candidate in a package the
	// caller never imports. Now that every EdgeImports edge in this pass
	// is resolved, re-check each weak-tier call edge against the import
	// closure and revert the ones whose target is unreachable. The
	// closure is built once and shared; each job still carries its
	// pre-resolution target so a reverted edge is restored exactly.
	tailPhase("cross_package_guard")
	guarded := 0
	// The guard consults the import-reachability closure only for the caller
	// files of the jobs the compute loop resolved. On a scoped warm pass those
	// jobs live in the changed repos (plus any repo whose bare-name call
	// resolved in place), so build the closure for just those repos rather than
	// scanning every file + import edge in the workspace — the entries for the
	// queried callers stay byte-identical, so the guard's verdicts are
	// unchanged. An empty scope builds the whole-graph closure.
	var guardClosure map[string]map[string]struct{}
	if len(r.scope) == 0 {
		guardClosure = r.buildImportClosure()
	} else {
		guardClosure = r.buildImportClosureFiltered(guardRepos)
	}
	if guardSpoolErr == nil && len(guardClosure) > 0 {
		guardPages := 0
		guardJobs := 0
		lastGuardLog := time.Now()
		for done := false; !done; {
			if err := ctx.Err(); err != nil {
				return resolveError(err)
			}
			records, exhausted, err := guardSpool.nextPage(resolvePendingPageRows)
			if err != nil {
				r.logger.Error("resolver: read guard spool", zap.Error(err))
				break
			}
			jobs := guardJobsFromRecords(r.graph, records)
			r.warmGuardLookupCache(jobs)
			guarded += r.guardCrossPackageCallEdges(jobs, guardClosure)
			r.clearLookupCache()
			guardPages++
			guardJobs += len(jobs)
			if time.Since(lastGuardLog) > 10*time.Second {
				lastGuardLog = time.Now()
				r.logger.Info("resolver: guard progress",
					zap.Int("pages", guardPages),
					zap.Int("jobs_checked", guardJobs),
					zap.Int("reverted", guarded))
			}
			done = exhausted
		}
	}
	tAfterGuard := time.Now()
	if err := ctx.Err(); err != nil {
		return resolveError(err)
	}

	// Post-resolution Go attribution passes: method-receiver rebind, bare-name
	// and generic-param binding, builtin + external-call materialisation. Each
	// pass carries its own rationale on its definition; the order is
	// load-bearing (bare-name binding precedes builtin attribution so a local
	// named `len` shadows the builtin). On a scoped warm restart the same five
	// passes run per-file over just the changed repos: the per-file equivalents
	// reproduce the whole-graph effect without the whole-graph index builds
	// (scanning every KindLocal, every Go type) that dominate a warm restart,
	// and unchanged repos are already in their post-full-resolve steady state,
	// so their edges are no-ops here.
	// Past a per-repo file budget the per-file dispatch's O(files) store round
	// trips (plus the per-package type-index builds behind the receiver rebind)
	// cost more than a single whole-graph streaming sweep. The two produce the
	// identical edge set — the attribution passes are idempotent and re-running
	// them over an unchanged repo is a no-op — so a large changed repo is routed
	// through the streaming path instead of the per-file storm.
	// The attribution passes below materialise builtin/external nodes; the
	// hot cache's immutability argument ends here.
	r.hotCache.flush()
	passIndexes.prepareTail()
	tailPhase("go_attribution")
	if len(r.scope) == 0 || r.scopedTailExceedsFileBudget() {
		// The whole-graph arm is the one whose plans depend on the store
		// believing its own size: the receiver rebind and the bare-name /
		// builtin passes below join the node and edge corpora, and a planner
		// costing them against a fraction of the real cardinality inverts the
		// outer loop (issue #651). This is a whole-graph boundary, not a
		// per-file resolve, so the check is paid once per full pass and is a
		// handful of seeks unless the store has actually outgrown its
		// statistics.
		//
		// The store's write gate is not held here, but the resolver's own
		// ResolveMutex is, and on the daemon's paths so is the caller's batch
		// gate. A refresh that queued on the store gate — or held it across a
		// whole-store ANALYZE — would hold ResolveMutex for the same span and
		// block every other resolve pass on this store. EnsurePlannerStatsFresh
		// is cooperative for exactly that reason: it try-locks the store gate,
		// analyzes ONE index per hold under a per-index timeout, and stops
		// rather than waits, so other store writers — including the
		// bounded-gate writers that drop their batches after 15 s — keep
		// making progress.
		//
		// That bounds what this pass pays while holding ResolveMutex; it does
		// not make it zero. A stale store costs this boundary the refresh's
		// pass budget, plus the one index already in flight, plus one bounded
		// sqlite_schema reload at the end of a completed pass, and the
		// remaining indexes ride a resume cursor to the next boundary rather
		// than being re-started from the head there. Outside that bound, and
		// still under ResolveMutex: two health probes, the present-index list
		// and the stat-row set — read-pool queries taking no store lock.
		graph.MaybeEnsurePlannerStatsFresh(ctx, r.graph)
		r.runFileAttributionPassesLocked()
	} else {
		for _, fp := range r.scopedFiles() {
			if err := ctx.Err(); err != nil {
				return resolveError(err)
			}
			r.runFileAttributionPassesForFileLocked(fp)
		}
	}
	tAfterAttrib := time.Now()
	if err := ctx.Err(); err != nil {
		return resolveError(err)
	}

	// Relative-import resolution for Python and Dart files. Runs
	// before module attribution so internal-target stems never get
	// mis-mapped to a phantom pypi/pub package.
	tailPhase("lang_dispatch_reconcile")
	ldStart := time.Now()
	r.resolveRelativeImports()
	ld1 := time.Now()
	if err := ctx.Err(); err != nil {
		return resolveError(err)
	}

	// Lua / Luau `require(...)` binding. Same settle window as the relative
	// imports above; resolveRelativeImports never touches Lua, so this lands
	// the Lua module/instance requires onto their indexed file nodes.
	r.resolveLuaRequires()
	// Godot `res://` binding — scene `[ext_resource]` references and
	// GDScript `preload(...)`. Same settle window and the same path-suffix
	// net as the Lua requires above.
	r.resolveGodotResPaths()
	ld2 := time.Now()
	if err := ctx.Err(); err != nil {
		return resolveError(err)
	}

	// Razor / Blazor `@using` namespace-cascade binding. Same settle window;
	// binds simple-type references reachable only via an imported namespace.
	r.resolveRazorUsings()
	ld3 := time.Now()
	if err := ctx.Err(); err != nil {
		return resolveError(err)
	}

	// Module attribution for ecosystems without a CGO type-checker
	// path (Python, Dart, …). Runs serially on the post-resolution
	// graph so it sees the final `external::*` set after the
	// dep-module bridge has had its chance.
	r.attributeNonGoModuleImports()
	ld4 := time.Now()
	if err := ctx.Err(); err != nil {
		return resolveError(err)
	}

	// Java override-dispatch fan-out. An ambiguous member call on a
	// supertype-typed receiver (`x.toString()` with two candidate
	// overrides) stays unresolved after the cross-package guard reverts
	// the name-only guess; this pass fans it out to every override in the
	// hierarchy, the call-hierarchy semantics the language server presents.
	// Runs after the guard so its ast_inferred edges are never reverted.
	overrideDispatchCandidates := r.collectOverrideDispatchCandidates()
	r.resolveJavaOverrideDispatchCandidates(overrideDispatchCandidates)
	ld5 := time.Now()
	if err := ctx.Err(); err != nil {
		return resolveError(err)
	}

	// PHP dispatch resolution: bind ambiguous member/scoped calls the guard
	// left unresolved via the class hierarchy — parent::/self:: up the extends
	// chain, and interface/abstract/trait override families fanned out to
	// every implementation. Same post-guard placement as the Java pass.
	r.resolvePHPOverrideDispatchCandidates(overrideDispatchCandidates)
	ld6 := time.Now()
	if err := ctx.Err(); err != nil {
		return resolveError(err)
	}
	// Diagnostic sub-phase breakdown of lang_dispatch_reconcile. Several of
	// these passes independently EdgesByKind-scan the SAME kind (EdgeImports:
	// relative_imports, lua_imports, razor_using, module_attribution all scan
	// it) — this line exists to catch a future regression there, the same
	// blind spot go_attribution's breakdown covers for its own six passes.
	r.logger.Info("resolver: lang-dispatch sub-passes",
		zap.Duration("relative_imports", ld1.Sub(ldStart)),
		zap.Duration("lua_requires", ld2.Sub(ld1)),
		zap.Duration("razor_usings", ld3.Sub(ld2)),
		zap.Duration("nongo_module_imports", ld4.Sub(ld3)),
		zap.Duration("java_override_dispatch", ld5.Sub(ld4)),
		zap.Duration("php_override_dispatch", ld6.Sub(ld5)))

	// Terminal-edge reconciliation: only a FULL (unscoped) pass has the global
	// evidence to conclude an edge is permanently unbindable, so it durably
	// stamps the newly-terminal edges and un-stamps any that regained a
	// candidate. Gated to the master resolve via SetStampTerminal so a
	// partially-indexed per-repo pass never stamps a false "no definition".
	// An exhausted deferred-LSP budget does NOT disable stamping: the edges
	// whose LSP verdict is still pending are excluded individually (the
	// heuristic cascade DID run to completion for everything else), and on a
	// cold start the LSP budget is always exhausted — a whole-pass abort here
	// left the entire unresolved residual unstamped, so every subsequent
	// full pass re-paged ~300k edges that the first pass had already proven
	// unbindable.
	if r.stampTerminal && len(r.scope) == 0 {
		if err := ctx.Err(); err != nil {
			return resolveError(err)
		}
		tailPhase("terminal_stamping")
		stamped, unstamped := r.reconcileTerminalStampsExcluding(lspResult.terminalityExcluded)
		if stamped > 0 || unstamped > 0 {
			r.logger.Info("resolver: terminal stamps",
				zap.Int("stamped", stamped),
				zap.Int("unstamped", unstamped))
		}
	}
	// Unresolved skipped work is already represented by the graph's pending
	// set; retain explicit pointers only for skipped edges a heuristic or tail
	// pass resolved, since those would otherwise vanish from the next pass.
	r.compactDeferredLSPRetries()

	// Diagnostic sub-phase breakdown of the whole ResolveAll pass. The
	// compute loop is parallel; the tail passes (guard, Go/lang attribution,
	// dispatch, terminal reconcile) run serially under the resolve lock and
	// are otherwise unlogged — this is the split used to target cold-index
	// resolve optimisation. page_load / prepare_indexes / compute_workers /
	// commit_apply attribute compute_loop's interior (page_load counts
	// in-pass fetches only); their shortfall against it is chunk-yield gaps,
	// revision loads, the set-oriented liveness preload, deferred-LSP
	// spooling and post-interleave index refreshes. The apply_* fields split
	// commit_apply itself — apply_liveness times only the unknown-store
	// fallback (the set-oriented preload runs pre-workers, outside every
	// bucket), then import-invalidation notes, the store reindex write,
	// placeholder reconcile, guard-spool append — with the remainder being
	// the job-apply loop, post-reindex revision reloads and guard
	// bookkeeping.
	r.logger.Info("resolver: pass complete",
		zap.Duration("total", time.Since(passStart)),
		zap.Duration("warm_lookup", warmElapsed),
		zap.Duration("compute_loop", loopElapsed),
		zap.Duration("page_load", loadElapsed),
		zap.Duration("prepare_indexes", prepareElapsed),
		zap.Duration("compute_workers", workersElapsed),
		zap.Duration("commit_apply", applyElapsed),
		zap.Duration("apply_liveness", applyLivenessElapsed),
		zap.Duration("apply_note_reindex", applyNoteElapsed),
		zap.Duration("apply_store_reindex", applyStoreElapsed),
		zap.Duration("apply_placeholder", applyPlaceholderElapsed),
		zap.Duration("apply_guard_spool", applyGuardSpoolElapsed),
		zap.Int("terminal_loaded", terminalLoaded),
		zap.Duration("deferred_lsp", lspElapsed),
		zap.Duration("guard", tAfterGuard.Sub(tailStart)),
		zap.Duration("go_attribution", tAfterAttrib.Sub(tAfterGuard)),
		zap.Duration("lang_dispatch_reconcile", time.Since(tAfterAttrib)))

	// A guarded edge was counted as resolved by the fallback that
	// produced it; reverting it moves the tally back to unresolved.
	if guarded > 0 {
		if total.Resolved >= guarded {
			total.Resolved -= guarded
		} else {
			total.Resolved = 0
		}
		total.Unresolved += guarded
	}
	// Fold the deferred LSP batch into the pass total. Each edge was tallied
	// Unresolved by the heuristic cascade that left it pending; binding it in
	// the batch moves it back to Resolved, matching the non-bulk path where
	// the inline LSP win would have counted a Resolved and no Unresolved.
	if lspResult.newlyResolved > 0 {
		total.Resolved += lspResult.newlyResolved
		if total.Unresolved >= lspResult.newlyResolved {
			total.Unresolved -= lspResult.newlyResolved
		} else {
			total.Unresolved = 0
		}
	}
	total.LSPDeferred = lspDeferred
	total.LSPAttempted = lspResult.attempted
	total.LSPResolved = lspResult.resolved
	total.LSPBudgetSkipped = lspResult.skipped
	total.LSPBudgetExhausted = lspResult.budgetExhausted
	total.PendingBefore = pendingBefore
	total.PendingAfter = pendingAfter
	return total, nil
}

// filterPendingByScope keeps only the pending edges a scoped ResolveAll must
// reconsider for the given changed-repo set. It is a conservative superset:
// an unchanged repo's own edges that are already resolved never appear in
// EdgesWithUnresolvedTarget, and the ones that remain unresolved there stay
// unresolved whether or not this pass reconsiders them — so dropping them is
// a pure work saving with no effect on the final resolved edge set. Filters
// in place; the returned slice reuses pending's backing array.
func filterPendingByScope(pending []*graph.Edge, scope map[string]struct{}) []*graph.Edge {
	out := pending[:0]
	for _, e := range pending {
		if e == nil {
			continue
		}
		if edgeInResolveScope(e, scope) {
			out = append(out, e)
		}
	}
	return out
}

// edgeInResolveScope reports whether a scoped ResolveAll pass must reconsider
// a pending edge. An edge is in scope when any of three rules hold:
//
//	(a) it originates in a changed repo (its source could re-target),
//	(b) its unresolved target is repo-qualified to a changed repo, or
//	(c) its target is a bare, unqualified unresolved::Name — which could
//	    newly bind into any changed repo, so it is always reconsidered.
//
// Everything else — an edge from an unchanged repo whose target is
// repo-qualified to another unchanged repo — is excluded.
func edgeInResolveScope(e *graph.Edge, scope map[string]struct{}) bool {
	// (a) Source repo is in scope.
	if _, ok := scope[graph.RepoPrefixOfID(e.From)]; ok {
		return true
	}
	// Repo prefix the target is pinned to, over both the unresolved
	// (`<repo>::unresolved::Name`) and the general stub (`<repo>::kind::…`)
	// encodings. Never a literal HasPrefix check — the helpers normalise the
	// bare and repo-qualified forms.
	targetRepo := graph.UnresolvedRepoPrefix(e.To)
	if targetRepo == "" {
		targetRepo = graph.StubRepoPrefix(e.To)
	}
	if targetRepo == "" {
		// (c) Bare, unqualified unresolved::Name — could resolve anywhere.
		return true
	}
	// (b) Target repo-qualified to a changed repo.
	_, ok := scope[targetRepo]
	return ok
}

// edgeFromInScope reports whether an edge's source repo is within the active
// resolve scope. An empty scope (whole-graph resolve) returns true for every
// edge, so a scoped tail pass degenerates to today's behaviour. Backs the
// post-resolve passes that have no per-file sibling: an unchanged repo's edges
// are already in their post-full-resolve steady state, so re-running these
// passes over them is a no-op and skipping them is a pure work saving.
func (r *Resolver) edgeFromInScope(from string) bool {
	if len(r.scope) == 0 {
		return true
	}
	_, ok := r.scope[graph.RepoPrefixOfID(from)]
	return ok
}

// scopedTailFileBudget bounds how many files a single changed repo may hold
// before the scoped per-file attribution tail is abandoned for the whole-graph
// streaming passes. Above it the per-file store round trips dominate one
// streaming sweep, so a large changed repo among small siblings would otherwise
// make the "incremental" warm restart's pre-ready phase slower than a full one.
// A var (not const) so tests can drive both branches on small fixtures.
var scopedTailFileBudget = 2000

// scopedTailExceedsFileBudget reports whether any repo in the active scope holds
// more KindFile nodes than scopedTailFileBudget. It reads the already-built
// dirIndex (no extra store materialization) and early-returns as soon as one
// repo crosses the budget, so a large changed repo never pays the per-file
// dispatch's O(files) query storm just to discover it should have streamed.
// Callers gate on len(r.scope) > 0 first.
func (r *Resolver) scopedTailExceedsFileBudget() bool {
	perRepo := make(map[string]int, len(r.scope))
	for _, nodes := range r.dirIndex {
		for _, n := range nodes {
			prefix := n.RepoPrefix
			if prefix == "" {
				prefix = graph.RepoPrefixOfID(n.ID)
			}
			if _, ok := r.scope[prefix]; !ok {
				continue
			}
			perRepo[prefix]++
			if perRepo[prefix] > scopedTailFileBudget {
				return true
			}
		}
	}
	return false
}

// scopedFiles returns the file paths of every KindFile node owned by a repo in
// the active resolve scope. Callers gate on len(r.scope) > 0 first, so an empty
// scope yields nothing. Backs the per-file dispatch of the post-resolve Go
// attribution passes on a scoped warm restart.
func (r *Resolver) scopedFiles() []string {
	var files []string
	for prefix := range r.scope {
		for _, n := range r.graph.GetRepoNodes(prefix) {
			if n != nil && n.Kind == graph.KindFile && n.FilePath != "" {
				files = append(files, n.FilePath)
			}
		}
	}
	return files
}

// buildDirIndexes builds two lookup maps for resolveImport. Populated
// once per ResolveAll / ResolveFile pass and torn down after.
//
//   - dirIndex     keys on filePathDir(file.FilePath) for exact
//     importPath == dir matches.
//   - lastDirIndex keys on the last path component of that directory
//     so an import of "logger" matches any file under .../logger/.
func (r *Resolver) buildDirIndexes() {
	r.dirIndex = make(map[string][]graph.FileNodeIdentity, 128)
	r.lastDirIndex = make(map[string][]graph.FileNodeIdentity, 128)
	for file := range graph.FileNodeIdentitiesSeq(r.graph, nil) {
		dir := filePathDir(file.FilePath)
		r.dirIndex[dir] = append(r.dirIndex[dir], file)
		last := lastPathComponent(dir)
		if last != "" && last != dir {
			r.lastDirIndex[last] = append(r.lastDirIndex[last], file)
		}
	}
}

func (r *Resolver) clearDirIndexes() {
	r.dirIndex = nil
	r.lastDirIndex = nil
	r.receiverTypeIdxByDir = nil
}

// warmLookupCache batches the per-edge GetNode / FindNodesByName
// queries the worker loop would otherwise fire serially. We collect
// every From/To node ID across the pending slice and the bare
// identifier name embedded in each `unresolved::*` target, then issue
// the two batched queries the Store exposes. Workers consult the
// resulting maps via cachedGetNode / cachedFindNodesByName; misses
// fall through to the underlying store.
func (r *Resolver) warmLookupCache(pending []*graph.Edge) {
	r.warmLookupCacheWithSources(pending, nil)
}

// warmLookupCacheWithSources reuses source nodes already hydrated while
// deriving a scoped page's repository prefixes. A non-nil map is a completed
// hydration, even when empty; requested IDs absent from that result become
// authoritative negatives only for the current page/generation, preventing a
// dangling source from falling into a point-query N+1.
func (r *Resolver) warmLookupCacheWithSources(pending []*graph.Edge, sources map[string]*graph.Node) {
	if len(pending) == 0 {
		return
	}
	warmStart := time.Now()
	idSet := make(map[string]struct{}, len(pending))
	qualNameSet := make(map[string]struct{})
	for _, e := range pending {
		if e == nil {
			continue
		}
		if e.From != "" {
			idSet[e.From] = struct{}{}
		}
		// Import targets resolve by qualified name: resolveImport's first
		// lookup needs every qualified-name candidate. Seed the import path so
		// the disk backend performs one indexed batch probe and the workers hit
		// the complete candidate cache (or its authoritative negative).
		if t := graph.UnresolvedName(e.To); strings.HasPrefix(t, "import::") {
			if qn := strings.TrimPrefix(t, "import::"); qn != "" {
				qualNameSet[qn] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	// Load source nodes once, then query definitions by bounded
	// repository+compatible-language groups. Both positive and negative group
	// results are authoritative for the worker page.
	sort.Strings(ids)
	idStart := time.Now()
	if sources != nil {
		r.nodeByID = sources
	} else {
		r.nodeByID = r.cachedParallelGetNodesByIDs(ids)
	}
	// A completed batch is authoritative for exactly this page's requested
	// source IDs. Normalize nil/all-missing backends to a non-nil positive map
	// and record misses separately so a dangling From cannot trigger one point
	// query per edge. The negative set is cleared with the page/generation.
	if r.nodeByID == nil {
		r.nodeByID = make(map[string]*graph.Node)
	}
	r.missingNodeByID = make(map[string]struct{})
	for _, id := range ids {
		if node, ok := r.nodeByID[id]; !ok || node == nil {
			r.missingNodeByID[id] = struct{}{}
		}
	}
	if len(r.missingNodeByID) == 0 {
		r.missingNodeByID = nil
	}
	idElapsed := time.Since(idStart)
	nameStart := time.Now()
	nameGroups, names, nameErr := r.warmRepoLanguageNameCache(pending)
	nameElapsed := time.Since(nameStart)
	foldStart := time.Now()
	// Fold every candidate node returned by the name lookup into the
	// id cache too: when a worker picks a candidate and the
	// downstream guard (cross_pkg / cross_repo) calls GetNode on the
	// chosen target, the cache should hit instead of falling through
	// to a per-id store call.
	if r.nodeByID == nil && (len(r.nodesByRepoLanguageName) > 0 || len(r.nodesByExternLanguageName) > 0 || len(r.nodesByName) > 0) {
		r.nodeByID = make(map[string]*graph.Node)
	}
	cacheCandidate := func(n *graph.Node) {
		if n == nil || n.ID == "" {
			return
		}
		if existing, ok := r.nodeByID[n.ID]; !ok || existing == nil {
			r.nodeByID[n.ID] = n
		}
		delete(r.missingNodeByID, n.ID)
	}
	for _, byName := range r.nodesByRepoLanguageName {
		for _, hits := range byName {
			for _, n := range hits {
				cacheCandidate(n)
			}
		}
	}
	for _, hits := range r.nodesByName {
		for _, n := range hits {
			cacheCandidate(n)
		}
	}
	for _, byName := range r.nodesByExternLanguageName {
		for _, hits := range byName {
			for _, n := range hits {
				cacheCandidate(n)
			}
		}
	}
	foldElapsed := time.Since(foldStart)
	qualStart := time.Now()
	// Pre-warm the complete import qualified-name candidate cache and record
	// authoritative negatives, avoiding one indexed store probe per edge.
	if len(qualNameSet) > 0 {
		qns := make([]string, 0, len(qualNameSet))
		for q := range qualNameSet {
			qns = append(qns, q)
		}
		r.nodesByQualName = r.graph.GetNodesByQualNames(qns)
		if r.nodesByQualName == nil {
			r.nodesByQualName = make(map[string][]*graph.Node, len(qualNameSet))
		}
		for q := range qualNameSet {
			if _, ok := r.nodesByQualName[q]; !ok {
				r.nodesByQualName[q] = nil
			}
		}
	}
	qualElapsed := time.Since(qualStart)
	// Make the previously-silent warm phase observable — the batched store
	// reads over ~0.5M keys land before the first compute progress tick.
	r.logger.Info("resolver: warm lookup cache",
		zap.Int("ids", len(ids)),
		zap.Bool("ids_reused", sources != nil),
		zap.Int("id_misses", len(r.missingNodeByID)),
		zap.Int("name_groups", nameGroups),
		zap.Int("names", names),
		zap.Error(nameErr),
		zap.Int("qual_names", len(qualNameSet)),
		zap.Duration("id_lookup", idElapsed),
		zap.Duration("name_lookup", nameElapsed),
		zap.Duration("candidate_fold", foldElapsed),
		zap.Duration("qual_lookup", qualElapsed),
		zap.Duration("elapsed", time.Since(warmStart)))
}

// parallelGetNodesByIDs is the concurrent form of Store.GetNodesByIDs used to
// pre-warm the resolver's per-pass id cache: it splits ids into up to NumCPU
// batches issued on their own goroutines and merges the per-batch maps. ids is
// already deduped by warmLookupCache, so a key lands in exactly one batch and
// the merge never has to reconcile collisions. Small inputs fall through to a
// single call, where the goroutine + merge overhead would dominate.
func (r *Resolver) parallelGetNodesByIDs(ids []string) map[string]*graph.Node {
	batches := lookupWarmBatches(len(ids))
	if batches <= 1 {
		return r.graph.GetNodesByIDs(ids)
	}
	parts := make([]map[string]*graph.Node, batches)
	chunk := (len(ids) + batches - 1) / batches
	var wg sync.WaitGroup
	for b := 0; b < batches; b++ {
		start := b * chunk
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(bi int, sub []string) {
			defer wg.Done()
			parts[bi] = r.graph.GetNodesByIDs(sub)
		}(b, ids[start:end])
	}
	wg.Wait()
	out := make(map[string]*graph.Node, len(ids))
	for _, m := range parts {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// parallelFindNodesByNames is the concurrent form of Store.FindNodesByNames.
// names is deduped by warmLookupCache, so each name resolves in exactly one
// batch and the merge is a plain key copy (no per-name slice concat across
// batches). Small inputs fall through to a single call.
func (r *Resolver) parallelFindNodesByNames(names []string) map[string][]*graph.Node {
	batches := lookupWarmBatches(len(names))
	if batches <= 1 {
		return r.graph.FindNodesByNames(names)
	}
	parts := make([]map[string][]*graph.Node, batches)
	chunk := (len(names) + batches - 1) / batches
	var wg sync.WaitGroup
	for b := 0; b < batches; b++ {
		start := b * chunk
		end := start + chunk
		if end > len(names) {
			end = len(names)
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(bi int, sub []string) {
			defer wg.Done()
			parts[bi] = r.graph.FindNodesByNames(sub)
		}(b, names[start:end])
	}
	wg.Wait()
	out := make(map[string][]*graph.Node, len(names))
	for _, m := range parts {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// lookupWarmBatches picks the goroutine count for the parallel warm-cache
// helpers: one per minLookupWarmBatch keys, capped at NumCPU, and 1 (serial)
// below the threshold so tiny key sets skip the fan-out overhead entirely.
func lookupWarmBatches(n int) int {
	const minLookupWarmBatch = 4096
	if n <= minLookupWarmBatch {
		return 1
	}
	batches := (n + minLookupWarmBatch - 1) / minLookupWarmBatch
	if cpus := runtime.NumCPU(); batches > cpus {
		batches = cpus
	}
	if batches < 1 {
		batches = 1
	}
	return batches
}

func (r *Resolver) clearLookupCache() {
	r.scratchGeneration++
	r.nodeByID = nil
	r.missingNodeByID = nil
	r.nodesByName = nil
	r.nodesByQualName = nil
	r.nodesByRepoLanguageName = nil
	r.nodesByRepoName = nil
	r.nodesByExternLanguageName = nil
	r.importFilesMu.Lock()
	r.importFilesByCaller = nil
	r.importFilesMu.Unlock()
	r.csharpNSMu.Lock()
	r.csharpNSByFile = nil
	r.csharpNSMu.Unlock()
}

// clearCSharpVisibilityCaches drops the pass-lifetime C# caches — the
// global-usings index, the per-file namespace sets (they bake the
// propagated globals in, so a rebuilt index behind stale per-file sets
// would keep serving the old store generation), and the type-lookup /
// ancestor memos the extension eligibility walk uses. Separate from
// clearLookupCache: that runs per pending page, and these cost an
// O(graph) scan or one hierarchy walk per type to rebuild.
func (r *Resolver) clearCSharpVisibilityCaches() {
	r.csharpNSMu.Lock()
	r.csharpGlobalByDir = nil
	r.csharpProjDirs = nil
	r.csharpNSByFile = nil
	r.csharpTypeNodesByName = nil
	r.csharpAncestorsByType = nil
	r.csharpMemberNamesByType = nil
	r.csharpGlobalDecls = nil
	r.csharpGlobalStaticDecls = nil
	r.csharpGlobalStaticByDir = nil
	r.csharpProjFiles = nil
	r.csharpVisSeeded = false
	r.csharpNSMu.Unlock()
}

// cachedGetNode returns the node for id, consulting the per-pass
// lookup cache first and falling through to the underlying store on
// miss. The cache is a positive-only fast path — absence means "not
// pre-warmed" only when it was never requested by the completed page batch;
// requested omissions are authoritative until the page/generation is cleared.
// Outside a ResolveAll pass both caches are nil and every call goes straight to
// the store.
func (r *Resolver) cachedGetNode(id string) *graph.Node {
	if id == "" {
		return nil
	}
	if r.nodeByID != nil {
		if n, ok := r.nodeByID[id]; ok && n != nil {
			return n
		}
	}
	if _, missing := r.missingNodeByID[id]; missing {
		return nil
	}
	return r.graph.GetNode(id)
}

// cachedFindNodesByName returns the candidates for name, consulting
// the per-pass cache first and falling through to the store on miss.
// Returns the in-cache slice directly when hit — callers MUST treat
// the result as read-only.
func (r *Resolver) cachedFindNodesByName(name string) []*graph.Node {
	if name == "" {
		return nil
	}
	if r.nodesByName != nil {
		if hits, ok := r.nodesByName[name]; ok {
			return hits
		}
	}
	return r.graph.FindNodesByName(name)
}

// cachedFindNodesByQualName serves resolveImport's complete candidate lookup
// from the per-pass cache. A present nil slice is an authoritative negative;
// an absent key falls through to one indexed batch probe.
func (r *Resolver) cachedFindNodesByQualName(qualName string) []*graph.Node {
	if qualName == "" {
		return nil
	}
	if r.nodesByQualName != nil {
		if nodes, ok := r.nodesByQualName[qualName]; ok {
			return nodes
		}
	}
	return r.graph.GetNodesByQualNames([]string{qualName})[qualName]
}

// cachedFindNodesByNameInRepo is the repo-scoped twin of
// cachedFindNodesByName: name-matched candidates whose RepoPrefix == repo,
// served from the per-pass name cache (filtered in Go) so the
// method/function/type/field cascade doesn't fire one
// FindNodesByNameInRepo query per pending edge — the warmup storm that
// the multi-repo prefixed-stub population (100k+ edges) turned into a
// hang. Falls through to the store on a cache miss, preserving
// correctness; the cache is positive-only (absence means "not
// pre-warmed", not "doesn't exist").
func (r *Resolver) cachedFindNodesByNameInRepo(name, repo string) []*graph.Node {
	if name == "" {
		return nil
	}
	// Tail and guard callers that do not carry source-language context use the
	// repository-only compatibility view. It is already repository-scoped and
	// never rescans/filter-copies a global cross-language hit slice per edge.
	if r.nodesByRepoName != nil {
		if byName, warmed := r.nodesByRepoName[repo]; warmed {
			if hits, queried := byName[name]; queried {
				return hits
			}
		}
	}
	return r.graph.FindNodesByNameInRepo(name, repo)
}

// buildDepModuleIndex collects every dep::<module-path> contract node
// (one per non-indirect `require` line in a tracked go.mod) and groups
// them by the owning repo's prefix so resolveImport can bridge a Go
// import to the dep node it satisfies. Entries are sorted by
// modulePath length descending, which keeps longest-prefix-wins for
// nested modules (e.g. importing "github.com/aws/aws-sdk-go-v2/service/s3"
// must hit the s3 dep, not the parent aws-sdk-go-v2 dep).
//
// Skips dep IDs of the form `dep::<repoName>::<shortName>`, which
// GoModExtractor emits when the dependency is itself a tracked sibling
// repo — those resolve through the cross-repo file graph instead and
// have no module path embedded in the ID.
func (r *Resolver) buildDepModuleIndex() {
	by := make(map[string][]depModuleEntry)
	for n := range graph.RepoNodeIdentitiesSeq(r.graph, nil, graph.KindContract) {
		if !strings.HasPrefix(n.ID, "dep::") {
			continue
		}
		mp := strings.TrimPrefix(n.ID, "dep::")
		if mp == "" || strings.Contains(mp, "::") {
			continue
		}
		by[n.RepoPrefix] = append(by[n.RepoPrefix], depModuleEntry{
			modulePath: mp,
			nodeID:     n.ID,
		})
	}
	for k := range by {
		entries := by[k]
		sort.Slice(entries, func(i, j int) bool {
			return len(entries[i].modulePath) > len(entries[j].modulePath)
		})
	}
	r.depModuleIndex = by
}

func (r *Resolver) clearDepModuleIndex() {
	r.depModuleIndex = nil
}

// lookupDepModule returns the dep::<module> contract ID whose module path is a
// prefix of importPath, scoped to the caller's repo. Empty means no match.
func (r *Resolver) lookupDepModule(callerRepo, importPath string) string {
	for _, entry := range r.depModuleIndex[callerRepo] {
		if importPath == entry.modulePath || strings.HasPrefix(importPath, entry.modulePath+"/") {
			return entry.nodeID
		}
	}
	return ""
}

// buildPassIndexes builds the four per-pass lookup indexes every
// resolve pass needs and returns the matching teardown (which also
// drops the lazily-built LSP index). Factored so entry points that
// run several passes under one lock — the per-save ResolveFile +
// ResolveIncomingForFile pair — build them once instead of once per
// pass.
func (r *Resolver) buildPassIndexes() (clear func()) {
	r.buildDirIndexes()
	r.buildDepModuleIndex()
	r.buildProvidesForIndex()
	r.buildReachabilityIndex()
	return r.clearPassIndexes
}

func (r *Resolver) clearPassIndexes() {
	r.scratchGeneration++
	r.clearDirIndexes()
	r.clearDepModuleIndex()
	r.clearProvidesForIndex()
	r.clearReachabilityIndex()
	r.clearLSPIndex()
	// The C# visibility state is deliberately NOT pass-lifetime any
	// more: the global-using sources persist across scoped resolves
	// (each entry reconciles the files it names), and wholesale
	// invalidation lives at the ResolveAll entry — clearing here would
	// re-introduce the per-save workspace rescan.
}

// buildPassIndexesForPending bounds the interactive path to the caller files
// represented by the pending frontier. Directory/dependency scans are paid only
// when an unresolved import actually needs them; the DI provides index is lazy.
func (r *Resolver) buildPassIndexesForPending(pending []*graph.Edge) (clear func()) {
	indexes := newPendingFrontierPassIndexes(r)
	indexes.prepare(pending)
	return indexes.close
}

// ResolveFile resolves unresolved edges originating from a specific file.
func (r *Resolver) ResolveFile(filePath string) *ResolveStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scopedCSharpVisibilityInvalidate([]string{filePath})

	clear := r.buildPassIndexes()
	defer clear()

	stats := &ResolveStats{}
	r.resolveFileLocked(filePath, stats)
	return stats
}

// ResolveFileAndIncoming runs the forward (this file's outgoing
// references) and reverse (other files' references to symbols defined
// here) passes under one lock with one build of the per-pass indexes.
// The per-save hot path calls this instead of ResolveFile +
// ResolveIncomingForFile back-to-back, which built and tore down the
// same four indexes twice per save.
func (r *Resolver) ResolveFileAndIncoming(filePath string) *ResolveStats {
	started := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	// The edited file may have (re)stamped global usings — reconcile the
	// persistent index instead of paying the workspace rescan per save.
	r.scopedCSharpVisibilityInvalidate([]string{filePath})

	// Establish whether this edit left any work before building the four
	// graph-wide pass indexes. Generated assets and source saves that carry
	// only already-resolved/structural edges are common watcher events; the
	// old ordering rebuilt every index even though both scoped passes were
	// guaranteed to visit zero pending edges. On a disk-backed multi-repo
	// graph that no-op cost can be minutes and holds the shared resolver lock
	// for the whole duration.
	pendingStarted := time.Now()
	pending := r.pendingEdgesForFileAndIncoming(filePath)
	pendingDuration := time.Since(pendingStarted)
	if len(pending) == 0 {
		return &ResolveStats{}
	}

	indexStarted := time.Now()
	clear := r.buildPassIndexesForPending(pending)
	indexDuration := time.Since(indexStarted)
	defer clear()

	// Warm the per-edge lookup cache for this file's pending forward and
	// incoming edges. Without it the single-file path fires a fresh
	// FindNodesByNameInRepo — a scanNode + meta-decode of every same-name
	// candidate — once PER edge, re-materialising the same candidates for
	// every edge that shares a name. Seeding the cache once (one batched
	// FindNodesByNames, like ResolveAll) materialises each candidate once
	// and the passes read it from memory.
	warmStarted := time.Now()
	r.warmLookupCache(pending)
	warmDuration := time.Since(warmStarted)
	defer r.clearLookupCache()

	stats := &ResolveStats{}
	forwardStarted := time.Now()
	r.resolveFileEdgesLocked(filePath, stats)
	forwardDuration := time.Since(forwardStarted)
	attributionStarted := time.Now()
	r.runFileAttributionPassesForFileLocked(filePath)
	attributionDuration := time.Since(attributionStarted)
	incomingStarted := time.Now()
	r.resolveIncomingLocked(filePath, stats)
	incomingDuration := time.Since(incomingStarted)
	if elapsed := time.Since(started); elapsed >= time.Second {
		r.logger.Info("resolver: incremental file phases",
			zap.String("file", filePath),
			zap.Int("pending", len(pending)),
			zap.Duration("pending_collect", pendingDuration),
			zap.Duration("build_indexes", indexDuration),
			zap.Duration("warm_lookup", warmDuration),
			zap.Duration("forward", forwardDuration),
			zap.Duration("attribution", attributionDuration),
			zap.Duration("incoming", incomingDuration),
			zap.Duration("total", elapsed))
	}
	return stats
}

// pendingEdgesForFileAndIncoming gathers the unresolved edges the forward
// and reverse passes will visit — the file's own outgoing unresolved
// edges plus the unresolved in-edges parked on the stub ids of the
// referenceable symbols this file defines. It mirrors the edge walks
// resolveFileEdgesLocked / resolveIncomingLocked perform, but only to seed
// warmLookupCache; the result feeds caching, never resolution directly.
type incrementalFileFrontier struct {
	paths       []string
	nodesByFile map[string][]*graph.Node
	outByNode   map[string][]*graph.Edge
	stubKeys    []string
	pending     []*graph.Edge
	// Admissions may overlap between outgoing and incoming frontiers.
	outgoingPending int
	outgoingCollect time.Duration
	incomingCollect time.Duration
}

func (r *Resolver) pendingEdgesForFileAndIncoming(filePath string) []*graph.Edge {
	return r.collectIncrementalFileFrontier([]string{filePath}).pending
}

// collectIncrementalFileFrontier performs the complete read side of a
// multi-file incremental resolve with a constant number of logical store
// calls: one batched file-node read, one outgoing-adjacency read, and one
// incoming-stub read. Backends may chunk each request at their bind limit.
func (r *Resolver) collectIncrementalFileFrontier(filePaths []string) incrementalFileFrontier {
	return collectIncrementalFileFrontierForPreparation(r.graph, filePaths, r.incrementalSkipped)
}

func collectIncrementalFileFrontier(
	g graph.Store,
	filePaths []string,
	skip func(*graph.Edge) bool,
) incrementalFileFrontier {
	return collectIncrementalFileFrontierMode(g, filePaths, skip, false)
}

func collectIncrementalFileFrontierForPreparation(
	g graph.Store,
	filePaths []string,
	skip func(*graph.Edge) bool,
) incrementalFileFrontier {
	return collectIncrementalFileFrontierMode(g, filePaths, skip, true)
}

func collectIncrementalFileFrontierMode(
	g graph.Store,
	filePaths []string,
	skip func(*graph.Edge) bool,
	lightweightIncoming bool,
) incrementalFileFrontier {
	var frontier incrementalFileFrontier
	if g == nil {
		return frontier
	}
	seenPaths := make(map[string]struct{}, len(filePaths))
	for _, path := range filePaths {
		if path == "" {
			continue
		}
		if _, duplicate := seenPaths[path]; duplicate {
			continue
		}
		seenPaths[path] = struct{}{}
		frontier.paths = append(frontier.paths, path)
	}
	if len(frontier.paths) == 0 {
		return frontier
	}

	outgoingStarted := time.Now()
	frontier.nodesByFile = g.GetFileNodesByPaths(frontier.paths)
	var nodeIDs []string
	for _, path := range frontier.paths {
		for _, node := range frontier.nodesByFile[path] {
			if node != nil && node.ID != "" {
				nodeIDs = append(nodeIDs, node.ID)
			}
		}
	}
	frontier.outByNode = g.GetOutEdgesByNodeIDs(nodeIDs)

	seenStubKeys := make(map[string]struct{})
	appendStubKey := func(key string) {
		if key == "" {
			return
		}
		if _, duplicate := seenStubKeys[key]; duplicate {
			return
		}
		seenStubKeys[key] = struct{}{}
		frontier.stubKeys = append(frontier.stubKeys, key)
	}
	for _, path := range frontier.paths {
		for _, node := range frontier.nodesByFile[path] {
			if node == nil {
				continue
			}
			for _, edge := range frontier.outByNode[node.ID] {
				if graph.IsUnresolvedTarget(edge.To) && (skip == nil || !skip(edge)) {
					frontier.pending = append(frontier.pending, edge)
				}
			}
			if node.Name == "" || !graph.IsReferenceableSymbol(node.Kind) {
				continue
			}
			// The four name-owned forms are one contract. This builder is
			// the batched incremental path's incoming leg, and it was the
			// last one still hand-building the bare pair: a member
			// reference parked under a wildcard form stayed pending until
			// the next whole-graph resolve.
			for _, key := range graph.UnresolvedNameCandidateIDs(node) {
				appendStubKey(key)
			}
		}
	}
	frontier.outgoingPending = len(frontier.pending)
	frontier.outgoingCollect = time.Since(outgoingStarted)
	incomingStarted := time.Now()
	// The unresolved target string is the incoming-edge bucket key even when
	// no node with that ID exists.
	if lightweightIncoming {
		inByStub := graph.InEdgeIdentitiesByNodeIDs(g, frontier.stubKeys)
		for _, key := range frontier.stubKeys {
			for _, identity := range inByStub[key] {
				if !graph.IsUnresolvedTarget(identity.To) {
					continue
				}
				// Preparation needs only the logical identity. Resolution performs a
				// fresh full-edge read after forward reindexing has settled.
				frontier.pending = append(frontier.pending, &graph.Edge{
					From: identity.From, To: identity.To, Kind: identity.Kind,
					FilePath: identity.FilePath, Line: identity.Line,
				})
			}
		}
	} else {
		inByStub := g.GetInEdgesByNodeIDs(frontier.stubKeys)
		for _, key := range frontier.stubKeys {
			for _, edge := range inByStub[key] {
				if edge != nil && graph.IsUnresolvedTarget(edge.To) {
					frontier.pending = append(frontier.pending, edge)
				}
			}
		}
	}
	frontier.incomingCollect = time.Since(incomingStarted)
	return frontier
}

// ResolveFilesAndIncoming runs the forward and reverse passes for a
// batch of files under one lock, one build of the per-pass indexes, and
// one run of the attribution passes. The affected-by re-resolution path
// uses this: calling ResolveFileAndIncoming per file would rebuild the
// four pass indexes and re-run the whole-graph attribution sweeps once
// per file, turning a bounded fan-out into N whole-graph passes.
func (r *Resolver) ResolveFilesAndIncoming(filePaths []string) *ResolveStats {
	stats := &ResolveStats{}
	if len(filePaths) == 0 {
		return stats
	}
	started := time.Now()
	logger := r.logger.With(zap.Int("input_files", len(filePaths)))
	var frontier incrementalFileFrontier
	var lockDuration, visibilityDuration, pendingDuration, indexDuration, warmDuration time.Duration
	var outgoingDuration, incomingDuration, attributionDuration time.Duration
	outcome := "interrupted"
	defer func() {
		logger.Info("resolver: incremental files phases",
			zap.String("outcome", outcome),
			zap.Int("files", len(frontier.paths)),
			zap.Int("pending", len(frontier.pending)),
			zap.Int("outgoing_pending", frontier.outgoingPending),
			zap.Int("incoming_pending", len(frontier.pending)-frontier.outgoingPending),
			zap.Duration("wait_lock", lockDuration),
			zap.Duration("visibility", visibilityDuration),
			zap.Duration("pending_collect", pendingDuration),
			zap.Duration("outgoing_collect", frontier.outgoingCollect),
			zap.Duration("incoming_collect", frontier.incomingCollect),
			zap.Duration("build_indexes", indexDuration),
			zap.Duration("warm_lookup", warmDuration),
			zap.Duration("resolve_outgoing", outgoingDuration),
			zap.Duration("resolve_incoming", incomingDuration),
			zap.Duration("resolve", outgoingDuration+incomingDuration),
			zap.Duration("attribution", attributionDuration),
			zap.Duration("total", time.Since(started)))
	}()
	finish := startIncrementalPhase(logger, "wait_lock")
	r.mu.Lock()
	defer r.mu.Unlock()
	lockDuration = finish()
	// The edited files may have (re)stamped global usings — reconcile the
	// persistent index instead of paying the workspace rescan per save.
	finish = startIncrementalPhase(logger, "visibility")
	r.scopedCSharpVisibilityInvalidate(filePaths)
	visibilityDuration = finish()

	finish = startIncrementalPhase(logger, "pending_collect")
	frontier = r.collectIncrementalFileFrontier(filePaths)
	pendingDuration = finish(
		zap.Int("files", len(frontier.paths)),
		zap.Int("outgoing_pending", frontier.outgoingPending),
		zap.Int("incoming_pending", len(frontier.pending)-frontier.outgoingPending),
		zap.Int("stub_keys", len(frontier.stubKeys)),
		zap.Duration("outgoing_collect", frontier.outgoingCollect),
		zap.Duration("incoming_collect", frontier.incomingCollect))
	if len(frontier.pending) == 0 {
		outcome = "no_pending"
		return stats
	}
	finish = startIncrementalPhase(logger, "build_indexes")
	clear := r.buildPassIndexesForPending(frontier.pending)
	indexDuration = finish(zap.Int("pending", len(frontier.pending)))
	defer clear()
	finish = startIncrementalPhase(logger, "warm_lookup")
	r.warmLookupCache(frontier.pending)
	warmDuration = finish()
	defer r.clearLookupCache()
	repos, omittedRepos, omittedAdmissions := incrementalAdmissionSummary(frontier, r.nodeByID)
	logger.Info("resolver: incremental pending admissions",
		zap.Any("repositories", repos),
		zap.Int("repositories_omitted", omittedRepos),
		zap.Int("admissions_omitted", omittedAdmissions),
		zap.Int("outgoing_pending", frontier.outgoingPending),
		zap.Int("incoming_pending", len(frontier.pending)-frontier.outgoingPending))

	// Resolve every changed file from the preloaded frontier, flush once, then
	// read the incoming restub buckets afresh. The fresh read preserves the
	// old forward-before-reverse semantics without one query/transaction per
	// file. Phase durations include each leg's batched graph mutation.
	finish = startIncrementalPhase(logger, "resolve_outgoing")
	r.resolvePreparedFileEdgesLocked(frontier.paths, frontier.nodesByFile, frontier.outByNode, stats)
	outgoingDuration = finish(
		zap.Int("resolved", stats.Resolved), zap.Int("unresolved", stats.Unresolved), zap.Int("external", stats.External))
	beforeIncoming := *stats
	finish = startIncrementalPhase(logger, "resolve_incoming")
	r.resolveIncomingStubKeysLocked(frontier.stubKeys, stats)
	incomingDuration = finish(
		zap.Int("resolved", stats.Resolved-beforeIncoming.Resolved),
		zap.Int("unresolved", stats.Unresolved-beforeIncoming.Unresolved),
		zap.Int("external", stats.External-beforeIncoming.External))
	finish = startIncrementalPhase(logger, "attribution")
	r.prepareIncrementalAttributionCache(frontier)
	r.runFileAttributionPassesForFilesLocked(frontier)
	r.clearIncrementalAttributionCache()
	attributionDuration = finish()
	outcome = "complete"
	return stats
}

// Keep one bounded summary per pass, never one log record per admitted edge.
const incrementalRepoLogLimit = 32

type incrementalRepoAdmission struct {
	Repo     string `json:"repo"`
	Outgoing int    `json:"outgoing"`
	Incoming int    `json:"incoming"`
}

// incrementalAdmissionSummary only inspects the already hydrated frontier and
// lookup cache. Missing provenance is reported explicitly, never hydrated with
// an extra store query just for logging. Counts are admissions, not unique
// edges: one edge may legitimately occur in both the outgoing and incoming legs.
func incrementalAdmissionSummary(frontier incrementalFileFrontier, sources map[string]*graph.Node) ([]incrementalRepoAdmission, int, int) {
	byRepo := make(map[string]*incrementalRepoAdmission)
	for i, edge := range frontier.pending {
		repo := "(unknown)"
		if edge != nil {
			if source := sources[edge.From]; source != nil && source.RepoPrefix != "" {
				repo = source.RepoPrefix
			}
		}
		counts := byRepo[repo]
		if counts == nil {
			counts = &incrementalRepoAdmission{Repo: repo}
			byRepo[repo] = counts
		}
		if i < frontier.outgoingPending {
			counts.Outgoing++
		} else {
			counts.Incoming++
		}
	}
	repos := make([]incrementalRepoAdmission, 0, len(byRepo))
	for _, counts := range byRepo {
		repos = append(repos, *counts)
	}
	sort.Slice(repos, func(i, j int) bool {
		left, right := repos[i].Outgoing+repos[i].Incoming, repos[j].Outgoing+repos[j].Incoming
		if left != right {
			return left > right
		}
		return repos[i].Repo < repos[j].Repo
	})
	omittedRepos, omittedAdmissions := 0, 0
	if len(repos) > incrementalRepoLogLimit {
		omittedRepos = len(repos) - incrementalRepoLogLimit
		for _, counts := range repos[incrementalRepoLogLimit:] {
			omittedAdmissions += counts.Outgoing + counts.Incoming
		}
		repos = repos[:incrementalRepoLogLimit]
	}
	return repos, omittedRepos, omittedAdmissions
}

// A start event leaves the active operation visible even if it blocks or never
// returns. Durations use the monotonic component of time.Now, not wall-clock
// subtraction or sleeps.
func startIncrementalPhase(logger *zap.Logger, phase string) func(...zap.Field) time.Duration {
	started := time.Now()
	logger.Info("resolver: incremental phase starting", zap.String("phase", phase))
	return func(fields ...zap.Field) time.Duration {
		elapsed := time.Since(started)
		fields = append(fields, zap.String("phase", phase), zap.Duration("elapsed", elapsed))
		logger.Info("resolver: incremental phase complete", fields...)
		return elapsed
	}
}

// resolveFileLocked is the forward-pass core. Caller holds r.mu and
// has built the per-pass indexes.
func (r *Resolver) resolveFileLocked(filePath string, stats *ResolveStats) {
	r.resolveFileEdgesLocked(filePath, stats)
	r.runFileAttributionPassesForFileLocked(filePath)
}

// fileOutEdges returns every outgoing edge of every node defined in
// filePath — the scope a single-file attribution pass needs in place of
// a whole-graph EdgesByKind sweep. Builtin / external / bare-name
// attributions all act on edges whose source is inside the edited file,
// so this is the complete candidate set for those passes.
func (r *Resolver) fileOutEdges(filePath string) []*graph.Edge {
	nodes := r.incrementalFileNodes(filePath)
	var out []*graph.Edge
	if r.incrementalOutByNode != nil {
		for _, node := range nodes {
			if node != nil {
				out = append(out, r.incrementalOutByNode[node.ID]...)
			}
		}
		return out
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil {
			ids = append(ids, node.ID)
		}
	}
	byNode := graph.OutEdgesForNodes(r.graph, ids)
	for _, node := range nodes {
		if node != nil {
			out = append(out, byNode[node.ID]...)
		}
	}
	return out
}

// resolveFileEdgesLocked walks one file's outgoing unresolved edges and
// binds them, without the attribution tail — batch callers run the
// attribution passes once after the whole batch instead of once per
// file. Caller holds r.mu and has built the per-pass indexes.
func (r *Resolver) resolveFileEdgesLocked(filePath string, stats *ResolveStats) {
	nodesByFile := r.graph.GetFileNodesByPaths([]string{filePath})
	ids := make([]string, 0, len(nodesByFile[filePath]))
	for _, node := range nodesByFile[filePath] {
		if node != nil && node.ID != "" {
			ids = append(ids, node.ID)
		}
	}
	byNode := r.graph.GetOutEdgesByNodeIDs(ids)
	r.resolvePreparedFileEdgesLocked([]string{filePath}, nodesByFile, byNode, stats)
}

// resolvePreparedFileEdgesLocked resolves a preloaded multi-file frontier and
// persists every changed edge in one batched mutation. Caller holds r.mu and
// has built the per-pass indexes.
func (r *Resolver) resolvePreparedFileEdgesLocked(
	filePaths []string,
	nodesByFile map[string][]*graph.Node,
	outByNode map[string][]*graph.Edge,
	stats *ResolveStats,
) {
	var jobs []reindexJob
	var reindexBatch []graph.EdgeReindex
	for _, filePath := range filePaths {
		for _, node := range nodesByFile[filePath] {
			if node == nil {
				continue
			}
			for _, edge := range outByNode[node.ID] {
				if edge == nil || !graph.IsUnresolvedTarget(edge.To) || r.incrementalSkipped(edge) {
					continue
				}
				oldKind := edge.Kind
				oldTo, changed := r.resolveEdge(edge, stats)
				if !changed {
					continue
				}
				reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: edge, OldTo: oldTo, OldKind: oldKind})
				jobs = append(jobs, reindexJob{
					edge:       edge,
					oldTo:      oldTo,
					oldKind:    oldKind,
					newTo:      edge.To,
					kind:       edge.Kind,
					confidence: edge.Confidence,
					origin:     edge.Origin,
				})
			}
		}
	}
	r.applyIncrementalReindexesLocked(reindexBatch, jobs, stats)
}

func (r *Resolver) applyIncrementalReindexesLocked(
	reindexBatch []graph.EdgeReindex,
	jobs []reindexJob,
	stats *ResolveStats,
) {
	if len(reindexBatch) > 0 {
		r.noteImportEdgeReindexes(reindexBatch)
		r.graph.ReindexEdges(reindexBatch)
		// nil index: incremental batches are file-sized, direct probes
		// stay under the single-save latency budget.
		reconcilePlaceholderSources(r.graph, nil, reindexBatch)
		for _, ri := range reindexBatch {
			r.noteRetargetedCall(ri.Edge)
		}
	}
	// Cross-package name-match guard — same contract as in ResolveAll.
	if len(jobs) == 0 {
		return
	}
	if closure := r.buildImportClosure(); len(closure) > 0 {
		if guarded := r.guardCrossPackageCallEdges(jobs, closure); guarded > 0 {
			if stats.Resolved >= guarded {
				stats.Resolved -= guarded
			} else {
				stats.Resolved = 0
			}
			stats.Unresolved += guarded
		}
	}
}

// runFileAttributionPassesLocked re-runs the attribution passes that
// ResolveAll runs. The per-file resolve paths handle incremental
// updates — a re-parse of one file emits fresh `unresolved::<name>`
// edges that haven't been seen by these passes yet, so without
// re-running them the incremental graph diverges from a cold re-index
// (caught by TestIncrementalReindex_ConvergesToFullIndex). Each pass is
// idempotent on already-rewritten edges (the `unresolved::` prefix
// check makes a second sweep a no-op). Caller holds r.mu.
func (r *Resolver) runFileAttributionPassesLocked() {
	// Announce each sub-pass up front: several run 30-90s on a large cold
	// graph, and the retrospective breakdown below only lands after ALL passes —
	// until then the log was silent for the whole sweep.
	sub := func(pass string) {
		r.logger.Info("resolver: attribution sub-pass starting", zap.String("pass", pass))
	}
	t0 := time.Now()
	sub("rebind_go_method_receivers")
	r.rebindGoMethodReceivers()
	t1 := time.Now()
	sub("bind_bare_name_scope_refs")
	r.bindBareNameScopeRefs()
	t2 := time.Now()
	sub("shared_dataflow_generic_census")
	census, censusStats := r.buildPostBareAttributionCensus(defaultAttributionCensusLimits)
	censusDone := time.Now()
	censusUsed := census != nil
	if census != nil {
		defer census.close()
	}
	r.logger.Info("resolver: shared attribution census",
		zap.Bool("used", censusUsed),
		zap.String("fallback_reason", censusStats.fallbackReason),
		zap.Int("rows_scanned", censusStats.rowsScanned),
		zap.Int("dataflow_candidates", censusStats.dataflowCandidates),
		zap.Int("generic_candidates", censusStats.genericCandidates),
		zap.Int64("retained_bytes_estimate", censusStats.retainedBytes),
		zap.Int("candidate_cap", defaultAttributionCensusLimits.maxCandidates),
		zap.Int64("retained_bytes_cap", defaultAttributionCensusLimits.maxRetainedBytes),
		zap.Duration("elapsed", censusDone.Sub(t2)))
	sub("bind_dataflow_callee_refs")
	if census != nil {
		r.bindDataflowCalleeRefsFromCensus(census)
		// The callee index and dataflow identities are the largest retained
		// census state. Drop both before generic-owner indexing starts.
		census.releaseDataflow()
	} else {
		r.bindDataflowCalleeRefs()
	}
	t3 := time.Now()
	sub("bind_generic_param_refs")
	if census != nil {
		r.bindGenericParamRefsFromCensus(census)
		census.releaseGeneric()
	} else {
		r.bindGenericParamRefs()
	}
	t4 := time.Now()
	sub("attribute_go_builtins")
	r.attributeGoBuiltins()
	t5 := time.Now()
	sub("attribute_go_external_calls")
	r.attributeGoExternalCalls()
	t6 := time.Now()
	// Last, so refs the passes above just bound to a partial fragment are
	// folded onto the canonical node in the same sweep.
	sub("merge_csharp_partial_types")
	r.mergeCSharpPartialTypes()
	t7 := time.Now()
	// Diagnostic sub-phase breakdown of the whole-graph attribution sweep,
	// mirroring the framework-synthesizer per-pass timing — go_attribution
	// was previously one opaque duration in "resolver: pass complete", so a
	// future single-pass regression here had no per-pass breadcrumb.
	r.logger.Info("resolver: attribution sub-passes",
		zap.Duration("rebind_go_method_receivers", t1.Sub(t0)),
		zap.Duration("bind_bare_name_scope_refs", t2.Sub(t1)),
		zap.Duration("shared_dataflow_generic_census", censusDone.Sub(t2)),
		zap.Bool("shared_dataflow_generic_census_used", censusUsed),
		zap.Duration("bind_dataflow_callee_refs", t3.Sub(censusDone)),
		zap.Duration("bind_generic_param_refs", t4.Sub(t3)),
		zap.Duration("attribute_go_builtins", t5.Sub(t4)),
		zap.Duration("attribute_go_external_calls", t6.Sub(t5)),
		zap.Duration("merge_csharp_partial_types", t7.Sub(t6)))
}

// runFileAttributionPassesForFileLocked is the single-file equivalent of
// runFileAttributionPassesLocked. Builtin / external-call / bare-name
// attribution only ever rewrite edges originating in the edited file, so
// they run over that file's outgoing edges instead of sweeping the whole
// graph once per save — the dominant per-edit resolver cost on a large
// graph. The two passes that genuinely need cross-file context (method-
// receiver rebind reads the package's type index; generic-param binding)
// stay whole-graph; both are already batched and cheap. The pass ORDER
// matches runFileAttributionPassesLocked: bare-name binding runs before
// builtin attribution so a local named `len` shadows the builtin.
func (r *Resolver) runFileAttributionPassesForFileLocked(filePath string) {
	r.rebindGoMethodReceiversForFile(filePath)
	r.bindBareNameScopeRefsForFile(filePath)
	r.bindDataflowCalleeRefsForFile(filePath)
	r.bindGenericParamRefsForFile(filePath)
	r.attributeGoBuiltinsForFile(filePath)
	r.attributeGoExternalCallsForFile(filePath)
	r.mergeCSharpPartialTypesForFile(filePath)
}

// ResolveIncomingForFile is the reverse of ResolveFile: instead of
// resolving the file's own OUTGOING references, it binds pending
// `unresolved::<Name>` edges in OTHER files that reference a symbol
// (re)defined in this file. After a definition is added or re-indexed,
// callers elsewhere still point at an unresolved stub — either one
// emitted at their own extraction time, or one restubIncomingRefs
// re-created when this file's prior concrete node was evicted. This
// rebinds them, scoped to this file's symbol names, so it costs
// O(references to those names), not a whole-graph ResolveAll. It uses
// the same reachability / import gates as ResolveFile (via resolveEdge),
// so an ambiguous name binds no differently and unsafe matches stay
// pending for the periodic ResolveAll.
func (r *Resolver) ResolveIncomingForFile(filePath string) *ResolveStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scopedCSharpVisibilityInvalidate([]string{filePath})

	clear := r.buildPassIndexes()
	defer clear()

	stats := &ResolveStats{}
	r.resolveIncomingLocked(filePath, stats)
	return stats
}

// ResolveIncomingForNames rebinds the pending references parked under the
// given symbol names' unresolved stubs, probing the bare and the
// `<repoPrefix>::` multi-repo stub forms for every supplied prefix. The
// receipt-exact eviction path records the names whose definitions were
// removed; their pending references live OUTSIDE any file frontier (the
// definition's file no longer declares the name, so a file-scoped incoming
// pass never enumerates its stub) and were previously retried only by the
// whole-graph fallback the exact receipt now avoids. The gates are unchanged:
// resolveEdge refuses ambiguity identically here, so a name whose surviving
// candidates are still ambiguous binds no differently and stays pending.
func (r *Resolver) ResolveIncomingForNames(names, repoPrefixes []string) *ResolveStats {
	stats := &ResolveStats{}
	if len(names) == 0 {
		return stats
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	seen := make(map[string]struct{}, len(names))
	var stubKeys []string
	appendKeys := func(keys []string) {
		for _, key := range keys {
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			stubKeys = append(stubKeys, key)
		}
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		appendKeys(graph.UnresolvedNameCandidateIDsForName(name, ""))
		for _, prefix := range repoPrefixes {
			if prefix != "" {
				appendKeys(graph.UnresolvedNameCandidateIDsForName(name, prefix))
			}
		}
	}
	if len(stubKeys) == 0 {
		return stats
	}
	// Probe the pending buckets before paying for the pass indexes: the
	// receipt consumers call this on every apply, and the common case is
	// that no edge is parked under any requested name. buildPassIndexes is
	// graph-wide (it scales with the store, not with the request), so a
	// no-work call must cost one bounded read instead. The probe's
	// materialized read is retained and handed to the resolution helper:
	// it is exactly the batch that helper needs, and re-reading it doubled
	// the hit path's store time and allocations at scale.
	inByStub := r.graph.GetInEdgesByNodeIDs(stubKeys)
	pending := false
	for _, edges := range inByStub {
		for _, edge := range edges {
			if edge != nil && graph.IsUnresolvedTarget(edge.To) {
				pending = true
				break
			}
		}
		if pending {
			break
		}
	}
	if !pending {
		return stats
	}
	clear := r.buildPassIndexes()
	defer clear()
	r.resolveIncomingStubEdgesLocked(stubKeys, inByStub, stats)
	return stats
}

// resolveIncomingLocked is the core of the reverse pass. Caller holds
// r.mu and has built the per-pass indexes. For each distinct
// referenceable symbol name defined in filePath it looks up the pending
// edges parked under that name's unresolved-stub id — GetInEdges keyed
// by the `unresolved::<Name>` target, so no new index is needed: the
// stub id IS the in-edge bucket key — and runs the normal per-edge
// resolution against them. Both the bare and the `<repoPrefix>::`
// multi-repo stub forms are probed.
func (r *Resolver) resolveIncomingLocked(filePath string, stats *ResolveStats) {
	defNodes := r.graph.GetFileNodes(filePath)
	if len(defNodes) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(defNodes))
	var stubKeys []string
	for _, n := range defNodes {
		if n == nil || n.Name == "" || !graph.IsReferenceableSymbol(n.Kind) {
			continue
		}
		if _, dup := seen[n.Name]; dup {
			continue
		}
		seen[n.Name] = struct{}{}
		stubKeys = append(stubKeys, graph.UnresolvedNameCandidateIDs(n)...)
	}
	if len(stubKeys) == 0 {
		return
	}

	r.resolveIncomingStubKeysLocked(stubKeys, stats)
}

// resolveIncomingStubKeysLocked binds a deduped unresolved-target frontier in
// one incoming-edge read and one batched mutation. Caller holds r.mu.
func (r *Resolver) resolveIncomingStubKeysLocked(stubKeys []string, stats *ResolveStats) {
	if len(stubKeys) == 0 {
		return
	}
	r.resolveIncomingStubEdgesLocked(stubKeys, r.graph.GetInEdgesByNodeIDs(stubKeys), stats)
}

// resolveIncomingStubEdgesLocked is resolveIncomingStubKeysLocked over a
// caller-supplied incoming-edge batch, for callers that already materialized
// the frontier (the names-pass probe). Caller holds r.mu.
func (r *Resolver) resolveIncomingStubEdgesLocked(stubKeys []string, inByStub map[string][]*graph.Edge, stats *ResolveStats) {
	if len(stubKeys) == 0 {
		return
	}
	var reindexBatch []graph.EdgeReindex
	var jobs []reindexJob
	for _, key := range stubKeys {
		for _, edge := range inByStub[key] {
			if edge == nil || !graph.IsUnresolvedTarget(edge.To) {
				continue
			}
			oldKind := edge.Kind
			oldTo, changed := r.resolveEdge(edge, stats)
			// Restore the provenance the restub stashed when the stub rebound
			// to the same target it had before the re-parse.
			restored := graph.RestoreRestubProvenance(edge)
			switch {
			case changed:
				reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: edge, OldTo: oldTo, OldKind: oldKind})
				jobs = append(jobs, reindexJob{
					edge:       edge,
					oldTo:      oldTo,
					oldKind:    oldKind,
					newTo:      edge.To,
					kind:       edge.Kind,
					confidence: edge.Confidence,
					origin:     edge.Origin,
				})
			case restored:
				// Persist an in-place provenance restore even when To is unchanged.
				reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: edge, OldTo: edge.To})
			}
		}
	}
	r.applyIncrementalReindexesLocked(reindexBatch, jobs, stats)
}

// reindexJob captures the resolved state for an edge whose target
// changed during a parallel resolution pass.
//
// Workers operate on shallow clones of each edge (cloneEdgeForResolve
// below) so mutating helpers can write to the clone freely without
// racing with: (a) other workers reading neighbouring edges' fields
// during bucket maintenance, or (b) the serial post-pass that reads
// each edge's To via keyOf. Once the worker phase completes, the
// resolved fields (To, Kind, CrossRepo, Confidence, Origin, Meta) are
// copied onto the real edge and graph.ReindexEdge is called — both
// serially.
//
// Kind is propagated because resolveEdge may promote it after
// resolution (e.g. `*.foo` with EdgeReads that lands on a method gets
// promoted to EdgeReferences so get_callers / find_usages surface the
// method-value reference).
type reindexJob struct {
	edge          *graph.Edge
	preResolution persistedEdgeSnapshot
	oldTo         string
	oldKind       graph.EdgeKind
	newTo         string
	kind          graph.EdgeKind
	crossRepo     bool
	confidence    float64
	origin        string
	meta          map[string]any
}

// resolverClonePool recycles the *graph.Edge shells handed out by
// cloneEdgeForResolve. The clone is per-iteration garbage in the
// ResolveAll worker — Get / Put across the inner loop turns the per-
// edge alloc into pool churn. Profile #4 (post lineBytesUpTo) showed
// cloneEdgeForResolve still pulling its share of the resolver's flat
// CPU; pooling removes it. The cloned Edge's Meta map is intentionally
// NOT pooled — when a resolution succeeds, the map travels onto the
// real edge via reindexJob.meta and is owned there afterwards.
var resolverClonePool = sync.Pool{
	New: func() any { return &graph.Edge{} },
}

// cloneEdgeForResolve returns a deep-enough copy of e for safe
// worker-local mutation by resolveEdge: every scalar / string field
// is value-copied; Meta is duplicated when present so a helper
// writing `clone.Meta["resolution"] = ...` doesn't mutate a map
// shared with the original (and therefore with other goroutines
// inspecting that map). Meta is the only reference-typed field on
// Edge that resolveEdge may write to today; any future Edge field
// of map / slice type will need handling here too.
//
// The returned *Edge must be released with releaseResolverClone once
// the worker is done with it (after any reindexJob has captured the
// Meta pointer). Forgetting to release just means the clone falls
// back to GC, not a leak.
func cloneEdgeForResolve(e *graph.Edge) *graph.Edge {
	clone := resolverClonePool.Get().(*graph.Edge)
	*clone = *e
	if clone.Meta != nil {
		dup := make(map[string]any, len(clone.Meta))
		for k, v := range clone.Meta {
			dup[k] = v
		}
		clone.Meta = dup
	}
	return clone
}

// releaseResolverClone returns a clone produced by cloneEdgeForResolve
// to the pool. Safe to call after the worker has copied any needed
// fields (To, Kind, Origin, Meta, …) into a reindexJob — the job
// retains its own references to those values, and the Edge shell is
// no longer needed. Zeroing prevents the next Get from seeing stale
// pointer fields the GC would otherwise be unable to reclaim.
func releaseResolverClone(clone *graph.Edge) {
	if clone == nil {
		return
	}
	*clone = graph.Edge{}
	resolverClonePool.Put(clone)
}

// resolveEdge mutates e.To in place and returns the prior value
// when a resolution actually happened (i.e. e.To != oldTo). The
// caller decides whether to call graph.ReindexEdge immediately
// (single-threaded ResolveFile) or to defer the reindex (parallel
// ResolveAll). When nothing changed the returned bool is false.
// resolutionExempt reports whether the resolver must never bind this edge
// independently, on ANY path — the heuristic cascade, the inline LSP
// hot-path, the deferred bulk LSP batch, and the cross-repository pass all
// consult it. A tests edge is DERIVED: the test-linkage pass clones a test
// caller's calls edges, meta-free. Re-running the bind WITHOUT the
// original's receiver evidence bypasses every receiver-gated guard (a
// List<int> site's clone bound a `this List<string>` extension the guarded
// calls edge itself refuses). The tests layer follows its calls edge; the
// resolver never binds it.
func resolutionExempt(e *graph.Edge) bool {
	return e != nil && e.Kind == graph.EdgeTests
}

func (r *Resolver) resolveEdge(e *graph.Edge, stats *ResolveStats) (oldTo string, changed bool) {
	oldTo = e.To
	if resolutionExempt(e) {
		return oldTo, false
	}
	// graph.UnresolvedName handles both `unresolved::Name` (legacy)
	// and `<repoPrefix>::unresolved::Name` (multi-repo COPY rewrite).
	// strings.TrimPrefix only stripped the bare form, leaving every
	// multi-repo edge with target=full-id and no downstream pattern
	// match — that was the root cause of find_usages returning zero
	// callers across the whole gortex repo.
	target := graph.UnresolvedName(e.To)
	if target == "" {
		// Not an unresolved stub at all — fall through with the raw
		// id so the pattern dispatch below sees the original value.
		target = strings.TrimPrefix(e.To, unresolvedPrefix)
	}

	// Resolve-time LSP hot-path. Consulted for TS/JS/JSX/TSX files
	// (and any other languages a future helper claims via
	// SupportsPath). When the LSP wins, the edge is stamped with
	// OriginLSPResolved and resolved_by=lsp; the heuristic path is
	// skipped. When it loses (no helper, no answer, no match), we
	// fall through to the existing heuristic cascade unchanged so
	// the edge still gets the best best-effort target.
	//
	// In bulk mode (a whole-graph ResolveAll warmup pass) the inline
	// round-trip is skipped: the definition lookup serialises inside the
	// helper, so paying it here parks the parallel workers on the helper
	// lock. ResolveAll instead collects EVERY LSP-eligible edge and binds
	// them in one deferred batch after the loop (see resolveDeferredLSP +
	// lspDeferTarget). Deferring only the heuristic-unresolved edges would
	// let a confident heuristic mis-bind escape LSP correction, so the batch
	// re-binds heuristic-resolved edges too — retaining the LSP-first
	// override this inline branch gives single-file paths, where bulkMode is
	// false and an interactive edit still resolves LSP-first.
	if !r.bulkMode && r.tryResolveViaLSP(e, target, stats) {
		return oldTo, e.To != oldTo
	}

	switch {
	case strings.HasPrefix(target, "grpc::"):
		// gRPC client-stub call placeholder
		// (`unresolved::grpc::<Service>::<Method>`). Landed on the
		// server-side handler by the graph-wide ResolveGRPCStubCalls
		// pass, which needs the whole graph plus InferImplements — the
		// per-edge resolver can't see that. Leave the edge untouched.
		return oldTo, false
	case strings.HasPrefix(target, "pyrel::"):
		// Python relative-import placeholder
		// (`unresolved::pyrel::<projectRootedStem>`). The graph-wide
		// resolveRelativeImports pass lands these on the matching
		// KindFile node once the whole index is built; the per-edge
		// resolver can't see project-layout context. Leave untouched
		// so the post-pass owns rewriting.
		return oldTo, false
	case strings.HasPrefix(target, "import::"):
		r.resolveImport(e, strings.TrimPrefix(target, "import::"), stats)
	case strings.HasPrefix(target, "extern::"):
		// Package-qualified call (json.NewEncoder): the parser attached
		// the full import path + symbol so we don't have to guess a
		// receiver type. resolveExtern accepts type candidates too, so a
		// package-qualified embedded type (`extern::pkg::Base`) keeps
		// its precise import-path evidence here rather than falling to
		// the same-repo-only resolveTypeRef below.
		r.resolveExtern(e, strings.TrimPrefix(target, "extern::"), stats)
	case e.Kind == graph.EdgeExtends || e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeComposes ||
		e.Kind == graph.EdgeReturns || e.Kind == graph.EdgeTypedAs:
		// Type-hierarchy and type-position edges must land on a type
		// or interface — never a function or method. Without this
		// gate the default case routes them through resolveFunctionCall
		// which happily matches any same-named function (e.g.
		// `*tsitter.Language` as a return type landed on a method
		// named `Language` instead of the `Language` type alias,
		// hiding every cross-package type reference from the graph
		// and making aliased types look completely unused). The four
		// kinds covered here:
		//   - EdgeExtends/EdgeImplements/EdgeComposes: type hierarchy
		//   - EdgeReturns: function/method return types
		//   - EdgeTypedAs: parameter / variable / field declared types
		// resolveTypeRef accepts only KindType / KindInterface
		// candidates and is placed ahead of the `*.` cases so a
		// selector-shaped supertype target can't slip into method
		// resolution. extern:: targets are handled above — their
		// import path is real cross-repo evidence.
		r.resolveTypeRef(e, target, stats)
	case strings.HasPrefix(target, "*.") && (e.Kind == graph.EdgeWrites || e.Kind == graph.EdgeReads):
		// Field write/read: prefer a KindField candidate whose
		// receiver matches the edge's receiver_type hint. Falls back
		// to the method-resolution path when no field candidate
		// lands — gives degraded-but-useful behaviour for graphs
		// where the field-node pass hasn't caught up yet.
		//
		// When the fallback resolves to a method, the extractor's
		// EdgeReads label was a placeholder for "selector used as a
		// value" (e.g. `mux.HandleFunc("/p", h.foo)` — h.foo passed,
		// not called). Promote to EdgeReferences so find_usages and
		// get_callers surface the method-value reference. Writes stay
		// as EdgeWrites: assigning a func value to a method-typed
		// field slot is still a write semantically.
		fieldName := strings.TrimPrefix(target, "*.")
		if !r.resolveFieldRef(e, fieldName, stats) {
			before := e.To
			r.resolveMethodCall(e, fieldName, stats)
			if e.Kind == graph.EdgeReads && e.To != before {
				e.Kind = graph.EdgeReferences
			}
		}
	case strings.HasPrefix(target, "*."):
		// Method call or method-value reference (e.g. h.handleHealth)
		r.resolveMethodCall(e, strings.TrimPrefix(target, "*."), stats)
	case e.Kind == graph.EdgeProvides || e.Kind == graph.EdgeConsumes:
		// DI-token reference — the target is a named value (injection
		// token), usually an `export const`, that the resolver's
		// function/method passes would miss because they only accept
		// method/function candidates.
		r.resolveTokenRef(e, target, stats)
	case e.Kind == graph.EdgeRendersChild:
		// A rendered child component (`<Button/>`) binds FIRST against the
		// caller file's import bindings — ground truth that pins the exact
		// component even when the name is ambiguous repo-wide — and only
		// falls through to the name / dir-proximity cascade when the
		// component is locally defined (no matching import).
		r.resolveRendersChild(e, target, stats)
	default:
		// For instantiates/references edges, try to resolve as a type first;
		// for calls edges, resolve as a function (original behavior).
		if e.Kind == graph.EdgeInstantiates || e.Kind == graph.EdgeReferences {
			r.resolveTypeOrFunc(e, target, stats)
		} else if rp, _ := e.Meta["rust_path"].(string); strings.Contains(rp, "::") {
			// A Rust qualified path call (`MatchStrategy::new`,
			// `crate::mod::baz`) keeps its full path in Meta["rust_path"].
			// The generic function-call cascade only sees the trailing
			// segment ("new"), so it would mis-bind the call to the first
			// same-named symbol. Leave it unresolved for the SynthRustScope
			// pass, which reads the qualifier and binds the right owner.
			break
		} else {
			before := e.To
			r.resolveFunctionCall(e, target, stats)
			// Promote EdgeReads → EdgeReferences when the resolved
			// target is a function or method. The extractor emits
			// EdgeReads for "bare identifier as value" (e.g. a cobra
			// command's `RunE: runClean` or `&Command{RunE: runFoo}`),
			// because at parse time it can't tell a function pointer
			// from a variable read. Now that we know the target is a
			// function, treat it as a reference so get_callers /
			// find_usages surface the wire-up site. Without this,
			// every CLI-wired command and command-table entry looks
			// like dead code.
			if e.Kind == graph.EdgeReads && e.To != before {
				if n := r.cachedGetNode(e.To); n != nil && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
					e.Kind = graph.EdgeReferences
				}
			}
		}
	}

	return oldTo, e.To != oldTo
}

// resolveExtern handles "extern::<importPath>::<symbol>" targets produced
// by the parser when a selector call's receiver matches an import alias.
//
// Resolution order:
//  1. Look for <symbol> defined in a file whose dir matches the import
//     path — this catches cross-repo calls into another indexed tree
//     (e.g. service A calls service B's exported function).
//  2. Otherwise, keep the package-qualified target so the UI can render
//     "crosses web → encoding/json" instead of a bare em-dash. The
//     prefix chosen encodes whether the path looks stdlib-like (no dot
//     in first segment, for Go) vs a module path (dotted or vendored).
//
// Nothing is created as a graph node — these are bookkeeping strings,
// same as the existing "external::<path>" stubs for unresolved imports.
func (r *Resolver) resolveExtern(e *graph.Edge, spec string, stats *ResolveStats) {
	sep := strings.LastIndex(spec, "::")
	if sep < 0 {
		// Malformed — treat as unresolved so we don't leak the
		// "unresolved::extern::" prefix into the graph.
		e.To = "external::" + spec
		stats.External++
		return
	}
	importPath := spec[:sep]
	symbol := spec[sep+2:]

	// Pass 1: does the symbol live in a file under this import path?
	// Reuse dirIndex populated by buildDirIndexes — no extra scan.
	// cachedFindNodesByName lands in the per-pass batch cache for
	// the common worker hot path; falls through to the store when
	// called outside ResolveAll.
	callerRepo := r.callerRepoPrefix(e)
	candidates, lookupErr := r.cachedFindExternNodesByName(symbol, e)
	if lookupErr != nil {
		r.logger.Warn("resolver: extern candidate lookup failed",
			zap.String("symbol", symbol),
			zap.Error(lookupErr))
		stats.Unresolved++
		return
	}
	for _, c := range candidates {
		if c.Kind != graph.KindFunction && c.Kind != graph.KindMethod && c.Kind != graph.KindType && c.Kind != graph.KindInterface {
			continue
		}
		dir := r.dirFor(c.FilePath)
		crossRepo := callerRepo != "" && c.RepoPrefix != "" && c.RepoPrefix != callerRepo
		var matches bool
		if crossRepo {
			// Cross-repo extern call: require a precise import-path
			// suffix match. The old loose last-component test
			// (`*/go`) resolved every tree-sitter binding's
			// `Language` to whichever repo sorted first.
			matches = dirMatchesImport(dir, importPath)
		} else {
			matches = strings.HasSuffix(dir, "/"+lastPathComponent(importPath)) || dir == importPath || strings.HasSuffix(dir, importPath)
		}
		if matches {
			e.To = c.ID
			if crossRepo {
				e.CrossRepo = true
			}
			stats.Resolved++
			return
		}
	}

	// Pass 2: classify the import path. "stdlib::" when the path looks
	// like a Go stdlib package (no dot in the first segment and not a
	// known module vendor prefix). "dep::" otherwise. Callers can treat
	// both as external for edge-walk purposes. The stdlib stub carries
	// the caller's repo prefix (see internal/graph/stub.go) so two repos
	// pinned to different Go SDK versions get distinct fmt::Errorf nodes
	// instead of one shared, version-conflated terminal.
	if isStdlibLike(importPath) {
		e.To = graph.StubID(callerRepo, graph.StubKindStdlib, importPath, symbol)
	} else {
		e.To = "dep::" + importPath + "::" + symbol
	}
	stats.External++
}

// isStdlibLike reports whether the import path looks like a Go stdlib
// package. Heuristic: the first path segment must have no dot (module
// paths like github.com/foo, golang.org/x, etc. always dot the first
// segment). Vetted against the list of real stdlib roots used by
// go/build — any new single-word non-stdlib package (very rare) is
// mis-classified as stdlib, which is cosmetic only.
func isStdlibLike(importPath string) bool {
	first := importPath
	if i := strings.Index(importPath, "/"); i >= 0 {
		first = importPath[:i]
	}
	return first != "" && !strings.Contains(first, ".")
}

// pendingShapeSummary buckets a pending edge set by unresolved-target shape so
// the resolver's per-pass log shows where the work concentrates (extern stdlib
// vs external module, receiver-unknown method calls, bare names, imports).
// Diagnostic only — never influences resolution. Note extern_stdlib here uses
// the same isStdlibLike heuristic the resolver applies as a post-scan fallback,
// so it over-counts dot-less local modules; it is a coarse population estimate,
// not a resolution decision.
func pendingShapeSummary(pending []*graph.Edge) string {
	var externStdlib, externDep, starMethod, importStub, qualified, bareName, other int
	for _, e := range pending {
		if e == nil {
			continue
		}
		if !graph.IsUnresolvedTarget(e.To) {
			other++
			continue
		}
		name := graph.UnresolvedName(e.To)
		switch {
		case strings.HasPrefix(name, "extern::"):
			rest := name[len("extern::"):]
			if sep := strings.LastIndex(rest, "::"); sep >= 0 && isStdlibLike(rest[:sep]) {
				externStdlib++
			} else {
				externDep++
			}
		case strings.HasPrefix(name, "*."):
			starMethod++
		case strings.HasPrefix(name, "import::"):
			importStub++
		case strings.Contains(name, "::"):
			qualified++
		default:
			bareName++
		}
	}
	return fmt.Sprintf("extern_stdlib=%d extern_dep=%d star_method=%d import=%d qualified=%d bare_name=%d other=%d",
		externStdlib, externDep, starMethod, importStub, qualified, bareName, other)
}

func (r *Resolver) resolveImport(e *graph.Edge, importPath string, stats *ResolveStats) {
	if isGodotResPlaceholder(e.To) {
		// resolveGodotResPaths owns this one; see the note on the helper.
		stats.Unresolved++
		return
	}
	callerRepo := r.callerRepoPrefix(e)
	callerWorkspace := r.callerWorkspaceID(e)
	ambiguousQualName := false

	// JS/TS relative + tsconfig-path-alias / baseUrl import: resolve the
	// specifier onto the in-repo file (or exported symbol) it names. The
	// dirIndex cascade below is package-directory-oriented and never
	// matches a JS/TS file stem, so without this every cross-directory
	// JS/TS import would fall through to an `external::*` stub — starving
	// buildImportClosure of reachability and letting the cross-package
	// guard revert the callers (issue #136). A no-op for non-JS/TS callers
	// and for genuine third-party specifiers.
	//
	// This runs BEFORE the npm-alias rewrite, mirroring tsserver's
	// precedence: `compilerOptions.paths` beats node_modules. A library
	// whose test suite imports its own published name (zustand's tests
	// import 'zustand', mapped by tsconfig paths onto ./src) must land on
	// the in-repo source, not on its own installed dist inside
	// node_modules.
	if to := resolveJSTSImportTarget(r.cachedGetNode, r.pathAlias, jsTSImportCallerFile(e), importPath); to != "" {
		e.To = to
		if callerRepo != "" {
			if n := r.cachedGetNode(to); n != nil && n.RepoPrefix != "" && n.RepoPrefix != callerRepo {
				e.CrossRepo = true
			}
		}
		stats.Resolved++
		return
	}

	// npm-alias rewrite: a JS/TS import of a package.json alias key
	// (`"shared": "npm:@acme/shared-lib@1.4.0"`) actually targets the
	// real package. Rewrite the specifier before any further lookup so a
	// locally-vendored `@acme/shared-lib` resolves to its real node
	// instead of falling through to an external stub. A no-op for
	// non-aliased specifiers and non-JS/TS callers.
	importPath, npmAliased := rewriteNpmAliasImport(r.npmAlias, e.FilePath, importPath)
	if npmAliased {
		// The rewritten specifier may itself be tsconfig-paths/relative
		// resolvable (an alias onto a workspace member).
		if to := resolveJSTSImportTarget(r.cachedGetNode, r.pathAlias, jsTSImportCallerFile(e), importPath); to != "" {
			e.To = to
			if callerRepo != "" {
				if n := r.cachedGetNode(to); n != nil && n.RepoPrefix != "" && n.RepoPrefix != callerRepo {
					e.CrossRepo = true
				}
			}
			stats.Resolved++
			return
		}
	}

	// PHP: a `use App\Log\Handler;` names a CLASS, not a directory. The
	// cascade below is package-directory-oriented, so every PHP import fell
	// through to an external stub — which starved buildImportClosure and left
	// the cross-package guard reverting cross-directory PHP calls as
	// unreachable. The extractor records the fully-qualified name on the edge;
	// binding it to the class node makes that class's directory reachable.
	if fqn := phpEdgeMetaString(e, "fqn"); fqn != "" {
		if matches := r.phpFindByFQN(fqn, callerRepo); len(matches) == 1 {
			node := matches[0]
			e.To = node.ID
			if callerRepo != "" && node.RepoPrefix != "" && node.RepoPrefix != callerRepo {
				e.CrossRepo = true
			}
			stats.Resolved++
			return
		}
	}

	// Look for every package node with this qualified name. The same import
	// path may legitimately exist in several tracked repositories/workspaces;
	// bind the caller-local instance instead of whichever row sorted first.
	if candidates := r.cachedFindNodesByQualName(importPath); len(candidates) > 0 {
		node, ambiguous := pickResolverQualNameCandidate(candidates, callerRepo, callerWorkspace)
		if node != nil {
			e.To = node.ID
			if callerRepo != "" && node.RepoPrefix != "" && node.RepoPrefix != callerRepo {
				e.CrossRepo = true
			}
			stats.Resolved++
			return
		}
		ambiguousQualName = ambiguous
	}

	// Inverted-index lookup instead of a per-edge AllNodes() scan —
	// the old scan was O(N) per import and the dominant cost of
	// ResolveAll on large repos (e.g. vscode: 5k imports × 150k nodes
	// = 750M comparisons per cold index). Falls back to a scan only
	// when the indexes aren't populated (ResolveEdge invoked outside
	// of ResolveAll/ResolveFile).
	//
	// When a package-manager workspace lookup is installed, all
	// same-repo candidates are collected (not just the first) so a
	// same-named collision across two workspace members can be broken
	// in favour of the importer's own workspace. Without the lookup
	// the first same-repo hit short-circuits the scan, preserving the
	// pre-feature cost.
	collectAll := r.workspaceMembers != nil
	// A JS/TS bare specifier names a node_modules package or a workspace
	// member. Node and tsc load such a directory through its entry point, so
	// only an `index.*` candidate is evidence — without this the cascade
	// binds `import … from 'graphql'` to an arbitrary file under any
	// same-named local directory (issue #450). See isJSTSDirEntryPoint.
	bareJSTS := isJSTSBareSpecifier(jsTSImportCallerFile(e), importPath)
	// Stronger evidence when the manifest is readable: a bare specifier the
	// importer declares as a registry / git / tarball dependency comes from
	// node_modules by definition, so NO in-repo directory is a candidate —
	// not even one holding an entry point. This is what closes the residual
	// case the entry-point rule alone cannot: a repo that both depends on
	// npm `graphql` and contains `src/feature/graphql/index.ts`.
	externalNpmDep := bareJSTS && declaresExternalNpmDep(r.npmDep, jsTSImportCallerFile(e), importPath)
	var sameRepo, crossRepoFile graph.FileNodeIdentity
	var sameRepoFound, crossRepoFound bool
	var sameRepoAll []graph.FileNodeIdentity
	consider := func(file graph.FileNodeIdentity) {
		if bareJSTS && !isJSTSDirEntryPoint(file.FilePath) {
			return
		}
		if callerRepo == "" || file.RepoPrefix == callerRepo {
			if !sameRepoFound {
				sameRepo, sameRepoFound = file, true
			}
			if collectAll {
				sameRepoAll = append(sameRepoAll, file)
			}
			return
		}
		// Cross-repo file candidate: require a precise import-path
		// suffix match. The lastDirIndex / full-scan fallbacks key on
		// the last path component only, so without this gate an import
		// of `.../tree-sitter-c/bindings/go` would resolve to whichever
		// `*/bindings/go` directory sorts first.
		if !crossRepoFound && dirMatchesImport(filePathDir(file.FilePath), importPath) {
			crossRepoFile, crossRepoFound = file, true
		}
	}
	// stop reports whether the candidate scan can short-circuit: once a
	// same-repo hit is found and we are not collecting every candidate
	// for workspace disambiguation.
	stop := func() bool { return sameRepoFound && !collectAll }
	if externalNpmDep {
		// Declared node_modules package — skip the cascade outright.
	} else if r.dirIndex != nil {
		for _, file := range r.dirIndex[importPath] {
			consider(file)
			if stop() {
				break
			}
		}
		if !sameRepoFound || collectAll {
			for _, file := range r.lastDirIndex[lastPathComponent(importPath)] {
				consider(file)
				if stop() {
					break
				}
			}
		}
	} else {
		for file := range graph.FileNodeIdentitiesSeq(r.graph, nil) {
			dir := filePathDir(file.FilePath)
			if strings.HasSuffix(dir, lastPathComponent(importPath)) || dir == importPath {
				consider(file)
				if stop() {
					break
				}
			}
		}
	}

	if sameRepoFound {
		// Name-collision tie-break: when several same-repo files match
		// a bare import name, prefer the one in the importing file's
		// own package-manager workspace.
		if workspaceFile, ok := r.preferSameWorkspaceFile(e.FilePath, sameRepoAll); ok {
			sameRepo = workspaceFile
		}
		e.To = sameRepo.ID
		stats.Resolved++
		return
	}
	if crossRepoFound {
		e.To = crossRepoFile.ID
		if callerRepo != "" && crossRepoFile.RepoPrefix != "" && crossRepoFile.RepoPrefix != callerRepo {
			e.CrossRepo = true
		}
		stats.Resolved++
		return
	}

	// No same- or cross-repo file matched. Before falling back to an
	// `external::` stub, try the dep::<module> contract nodes from the
	// caller's go.mod — that bridge is what gives third-party imports
	// like "github.com/foo/bar/sub/pkg" an incoming edge on the
	// dep::github.com/foo/bar node.
	if depNodeID := r.lookupDepModule(callerRepo, importPath); depNodeID != "" {
		e.To = depNodeID
		stats.Resolved++
		return
	}

	// npm-alias sub-path: a rewritten import like `@acme/shared-lib/util`
	// addresses a path inside the real package. Nothing matched the
	// full path, so fall back to the package node itself — the
	// cross-package edge belongs on the package regardless of which
	// sub-module the importer reached for.
	if npmAliased {
		if pkg := npmPackagePrefix(importPath); pkg != "" {
			if candidates := r.cachedFindNodesByQualName(pkg); len(candidates) > 0 {
				node, ambiguous := pickResolverQualNameCandidate(candidates, callerRepo, callerWorkspace)
				if node != nil {
					e.To = node.ID
					if callerRepo != "" && node.RepoPrefix != "" && node.RepoPrefix != callerRepo {
						e.CrossRepo = true
					}
					stats.Resolved++
					return
				}
				ambiguousQualName = ambiguousQualName || ambiguous
			}
		}
	}

	// Several policy-equivalent qualified-name candidates are not evidence for
	// an arbitrary edge. Leave the import pending for the cross-repository pass.
	if ambiguousQualName {
		stats.Unresolved++
		return
	}

	// External/unresolvable import — create a stub target ID.
	e.To = "external::" + importPath
	stats.External++
}

func (r *Resolver) resolveFunctionCall(e *graph.Edge, funcName string, stats *ResolveStats) {
	callerRepo := r.callerRepoPrefix(e)
	candidates := withoutReExportForwarders(r.cachedFindNodesByNameInRepoForEdge(funcName, callerRepo, e))
	if len(candidates) == 0 {
		// No same-repo candidate. A genuine cross-repo callee is left
		// unresolved here for CrossRepoResolver — which alone carries the
		// import-reachability + workspace-boundary evidence — to lift.
		// Guessing "first function named X anywhere in the graph" is the
		// exact name-collision bug this gate removes.
		stats.Unresolved++
		return
	}

	// Per-language scope-based static resolver. Consulted before the
	// generic locality cascade so C file-static / C++ namespace +
	// ADL / Java enclosing-class / PHP namespace + parent::/self::
	// rules can land a precise binding when their evidence is strong.
	// Returns nil when no language-specific rule applies; the cascade
	// below then runs unchanged.
	if pick := r.preferScopeCandidate(e, funcName, candidates); pick != nil {
		e.To = pick.ID
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.92
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["resolution"] = "scope"
		stats.Resolved++
		return
	}

	// A restubbed C# member call (`x.Pack()` evicted to `unresolved::Pack`)
	// loses its member shape but keeps its evidence in Meta: the receiver
	// stamps, the member_call marker, or the extension tag of the bind it
	// lost. Route it back through the type-directed extension rule — and
	// once the edge is known to be a member call, never let a locality
	// tier below pick an extension: that would bypass the eligibility
	// veto, and the restub provenance restore re-applies the original
	// confidence verbatim, freezing a stale bind nothing re-validates.
	csharpMember := false
	if e.Meta != nil {
		recvEvidence, _ := e.Meta["receiver_type"].(string)
		if recvEvidence == "" {
			recvEvidence, _ = e.Meta["receiver_builtin"].(string)
		}
		mc, _ := e.Meta["member_call"].(bool)
		res, _ := e.Meta["resolution"].(string)
		if recvEvidence != "" || mc || res == "extension_method" {
			csharpMember = true
			if r.tryBindCSharpExtension(e, funcName, recvEvidence, candidates, stats) {
				return
			}
			if res == "extension_method" {
				// The rule refused what the tag says it once bound — a
				// stale tag must not exempt whatever binds next from
				// the receiver gate.
				delete(e.Meta, "resolution")
			}
		}
	}

	// #559: receiverless C# calls carry arg_count too — narrow the
	// ordinary-overload set before the same-file pick and the locality
	// cascade below. Runs after the extension routing above on purpose:
	// extension candidates are adjudicated by their own binder and are
	// exempt from the ordinary window.
	candidates = csharpNarrowMethodByApplicability(e, candidates)

	// File-local candidates outrank everything below: a symbol defined in
	// the caller's own file is strictly more local than a same-directory
	// neighbour in every language (in Go both are package scope, so the
	// same-file pick is equally valid; in module-scoped languages only the
	// same-file symbol is in scope at all). Without this tier a same-named
	// nested helper in a NEIGHBOURING test file captures the calls the
	// caller's own helper should receive — zustand's persistSync tests
	// bound to persistAsync's `createStore` helper purely by candidate
	// iteration order.
	var sameFile *graph.Node
	sameFileCount := 0
	for _, c := range candidates {
		if csharpMember && isCSharpExtension(c) {
			continue
		}
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			c.FilePath != "" && c.FilePath == e.FilePath {
			if sameFile == nil {
				sameFile = c
			}
			sameFileCount++
		}
	}
	if sameFile != nil {
		e.To = sameFile.ID
		// A bare-name call to the sole same-file definition is structurally
		// unambiguous — grammar-grounded, no type system needed — so stamp it
		// ast_resolved rather than leaving it to backfill to the name-only
		// tier that find_usages suppresses by default. This is what keeps a
		// freshly-added same-file call site (the common edit) visible instead
		// of silently hidden. Two same-name same-file definitions (a func and
		// a method sharing a name) stay untagged so suppression can still drop
		// the ambiguous pick. Only genuine call edges are promoted; bare-value
		// reads (cobra RunE wire-ups) keep the heuristic tier.
		if sameFileCount == 1 && e.Kind == graph.EdgeCalls && e.Origin == "" {
			e.Origin = graph.OriginASTResolved
			e.Confidence = 0.9
		}
		stats.Resolved++
		return
	}

	// Import-evidence disambiguation (JS/TS only; see import_evidence.go
	// for the full precedence design). The ES module system has no ambient
	// directory scope, so before the locality cascade below can bind a
	// same-dir shadow — and before cross-dir ambiguity guesses or refuses —
	// ask the caller file's import closure. When the caller imports exactly
	// one candidate's file (directly or through re-export/barrel hops) that
	// import statement is structural, AST-grade evidence of the binding:
	// resolve to it at OriginASTResolved, the tier resolveRendersChild's
	// import-binding path already uses. A module-local candidate blocks the
	// pick; no import or several imported candidates fall through to the
	// existing cascade unchanged.
	if pick := r.pickImportEvidenceCallee(e.FilePath, funcName, candidates); pick != nil {
		e.To = pick.ID
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.9
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["resolution"] = "import_closure"
		stats.Resolved++
		return
	}

	// Prefer same-package (same directory) match. When exactly one
	// same-package function/method carries this name, a bare-name call to the
	// sole same-package definition is structurally unambiguous, so stamp it
	// ast_resolved (same rationale as the same-file pick above). Multiple
	// same-name same-package candidates stay untagged so redundant-text
	// suppression can still drop them. Method-name fan-out (x.Get()) never
	// reaches here — it resolves in resolveMethodCall and stays text_matched,
	// preserving that precision guard.
	callerDir := r.dirFor(e.FilePath)
	var samePkg *graph.Node
	samePkgCount := 0
	for _, c := range candidates {
		if csharpMember && isCSharpExtension(c) {
			continue
		}
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			r.dirFor(c.FilePath) == callerDir {
			if samePkg == nil {
				samePkg = c
			}
			samePkgCount++
		}
	}
	if samePkg != nil {
		e.To = samePkg.ID
		if samePkgCount == 1 && e.Kind == graph.EdgeCalls && e.Origin == "" {
			e.Origin = graph.OriginASTResolved
			e.Confidence = 0.9
		}
		stats.Resolved++
		return
	}

	// Fall back to the first same-repo function/method match. This is a
	// name-only guess (no directory/import evidence), so tag it text_matched
	// — the weakest tier — so the redundant-text suppression drops it when a
	// language server later confirms the real target, and the cross-package
	// guard can revert it when unreachable. Same-file / same-directory picks
	// above stay untagged (structural locality evidence) and survive.
	for _, c := range candidates {
		if csharpMember && isCSharpExtension(c) {
			continue
		}
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			e.To = c.ID
			if e.Origin == "" {
				e.Origin = graph.OriginTextMatched
			}
			stats.Resolved++
			return
		}
	}

	// JS/TS last resort: an exported const initialised with a callable the
	// extractor could not classify as a function (alias-cast exports like
	// `export const persist = persistImpl as unknown as Persist`) lands as
	// a KindVariable/KindConstant node, which the function/method loops
	// above can never bind. Accept a TOP-LEVEL variable/constant callee —
	// same-directory first, then a unique same-repo match; any ambiguity
	// refuses so a local binding cannot capture an unrelated call.
	if isJSTSPath(e.FilePath) {
		if pick := pickTopLevelValueCallee(candidates, funcName, callerDir, r.dirFor); pick != nil {
			e.To = pick.ID
			e.Origin = graph.OriginASTInferred
			e.Confidence = 0.7
			if e.Meta == nil {
				e.Meta = map[string]any{}
			}
			e.Meta["resolution"] = "value_callee"
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

// withoutReExportForwarders drops barrel re-export binding nodes from a
// call-resolution candidate set. A re-export node (`export { X } from './mod'`)
// is a transparent forwarder, never a callee — the call binds to the
// declaration it forwards, which is a separate same-named candidate. Leaving
// the forwarder in only adds a phantom candidate that turns an otherwise-clean
// import-evidence / value-callee pick into a refused ambiguity. Returns the
// input unchanged when it holds no forwarders (the common case), so non-JS/TS
// and non-barrel resolution pays nothing.
func withoutReExportForwarders(candidates []*graph.Node) []*graph.Node {
	has := false
	for _, c := range candidates {
		if graph.IsReExportNode(c) {
			has = true
			break
		}
	}
	if !has {
		return candidates
	}
	out := make([]*graph.Node, 0, len(candidates))
	for _, c := range candidates {
		if !graph.IsReExportNode(c) {
			out = append(out, c)
		}
	}
	return out
}

// pickTopLevelValueCallee returns the variable/constant candidate a JS/TS
// call edge may bind to when no function/method candidate matched: only a
// top-level symbol (ID == <file>::<name>, i.e. not a local or an object
// member) is eligible, same-directory candidates win, and ambiguity at
// either tier returns nil so no false edge lands.
func pickTopLevelValueCallee(candidates []*graph.Node, funcName, callerDir string, dirFor func(string) string) *graph.Node {
	var sameDir, repoWide *graph.Node
	sameDirAmbiguous, repoAmbiguous := false, false
	for _, c := range candidates {
		if c.Kind != graph.KindVariable && c.Kind != graph.KindConstant {
			continue
		}
		if c.ID != c.FilePath+"::"+funcName {
			continue // nested/local binding — not a top-level value
		}
		if dirFor(c.FilePath) == callerDir {
			if sameDir != nil {
				sameDirAmbiguous = true
			} else {
				sameDir = c
			}
		}
		if repoWide != nil {
			repoAmbiguous = true
		} else {
			repoWide = c
		}
	}
	if sameDir != nil && !sameDirAmbiguous {
		return sameDir
	}
	if repoWide != nil && !repoAmbiguous {
		return repoWide
	}
	return nil
}

// resolveTypeOrFunc resolves unresolved edges that could be either a type
// reference (composite literal, type assertion) or a function reference.
// It first tries to match a type/interface node, then falls back to functions.
// Candidates are restricted to the caller's own repo — a cross-repo
// match here would be a name-only guess; CrossRepoResolver handles the
// genuine cross-repo case with import-reachability evidence.
func (r *Resolver) resolveTypeOrFunc(e *graph.Edge, name string, stats *ResolveStats) {
	callerRepo := r.callerRepoPrefix(e)
	candidates := r.cachedFindNodesByNameInRepoForEdge(name, callerRepo, e)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerDir := r.dirFor(e.FilePath)

	// PHP: narrow to the namespace the source actually names before ranking.
	// `new User()` in a file that imports App\Models\User must not land on
	// App\Entities\User just because it sorts better. See
	// phpNarrowByTargetFQN — narrowing only, never a loss.
	if narrowed := phpNarrowByTargetFQN(e, candidates); len(narrowed) > 0 {
		candidates = narrowed
	}

	// C#: narrow to the namespaces the file imports or encloses — same
	// narrowing-only contract as the PHP pass above. See csharp_ns_narrow.go.
	if narrowed := r.csharpNarrowByNamespace(e, candidates); len(narrowed) > 0 {
		candidates = narrowed
	}

	// Land the edge on the canonical type/interface definition (real,
	// exported, top-level, non-test), preferring same-package only as a
	// tiebreak. See bestTypeCandidate / resolveTypeRef for the rationale:
	// without this an instantiate / reference edge for a widely-imported
	// or builder-pattern type lands on whichever same-named rival sorts
	// first, hiding all usage from the canonical definition node.
	if best := bestTypeCandidate(candidates, callerDir); best != nil {
		e.To = best.ID
		stats.Resolved++
		return
	}

	// If no type found, try as function (e.g., bare function name passed as value).
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			if r.dirFor(c.FilePath) == callerDir {
				e.To = c.ID
				stats.Resolved++
				return
			}
		}
	}
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

// resolveTypeRef resolves an extends / implements / composes edge to a
// type or interface node. It never accepts a function or method
// candidate — a type-hierarchy edge whose target is a function is
// always a misresolution (the bug that let `type EdgeKind string`
// "extend" a method named `string`). Candidates are restricted to the
// caller's own repo; a genuine cross-repo supertype is left unresolved
// for CrossRepoResolver.
func (r *Resolver) resolveTypeRef(e *graph.Edge, name string, stats *ResolveStats) {
	// A selector-shaped target (`*.Base`, from an embedded `pkg.Base`)
	// carries no usable package qualifier once it reaches here — strip
	// the `*.` and resolve on the bare type name.
	name = strings.TrimPrefix(name, "*.")
	callerRepo := r.callerRepoPrefix(e)
	candidates := r.cachedFindNodesByNameInRepoForEdge(name, callerRepo, e)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}
	callerDir := r.dirFor(e.FilePath)

	// PHP: the extractor recorded the fully-qualified name the source names,
	// resolved through the file's `use` imports. Narrowing to the candidates
	// declared under that namespace is the difference between `App\Models\User`
	// and `App\Entities\User`, which the bare name cannot tell apart. Narrowing
	// only — when no candidate matches, the full set is ranked as before, so
	// this can sharpen a binding but never lose one.
	if narrowed := phpNarrowByTargetFQN(e, candidates); len(narrowed) > 0 {
		candidates = narrowed
	}

	// C#: narrow to the namespaces the file imports or encloses — same
	// narrowing-only contract as the PHP pass above. See csharp_ns_narrow.go.
	if narrowed := r.csharpNarrowByNamespace(e, candidates); len(narrowed) > 0 {
		candidates = narrowed
	}

	// Land the edge on the canonical type/interface definition. The
	// ranker prefers a real, exported, top-level, non-test definition
	// over same-named rivals (external stubs, test/mock defs, private or
	// nested member types), with same-package proximity folded in only as
	// a tiebreak — so a same-directory test/stub no longer steals the
	// edge from a cross-directory canonical def (the bug that made
	// widely-imported and builder-pattern types look unused).
	if best := bestTypeCandidate(candidates, callerDir); best != nil {
		e.To = best.ID
		stats.Resolved++
		return
	}
	stats.Unresolved++
}

// resolveFieldRef lands an EdgeWrites/EdgeReads edge on a KindField
// node when the receiver type is known. Returns true when a field
// candidate was picked — caller falls back to method resolution
// otherwise (handles cases where the extractor labelled the edge as a
// write but the runtime target is actually a method/property).
func (r *Resolver) resolveFieldRef(e *graph.Edge, fieldName string, stats *ResolveStats) bool {
	receiverType := edgeReceiverType(e)
	candidates := r.cachedFindNodesByNameInRepoForEdge(fieldName, r.callerRepoPrefix(e), e)
	if len(candidates) == 0 {
		return false
	}
	callerDir := r.dirFor(e.FilePath)

	// Pass 1: same-directory + exact-receiver-type field.
	if receiverType != "" {
		for _, c := range candidates {
			if c.Kind == graph.KindField &&
				r.dirFor(c.FilePath) == callerDir &&
				nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.95
				stats.Resolved++
				return true
			}
		}
		// Pass 2: exact-receiver-type field, any directory.
		for _, c := range candidates {
			if c.Kind == graph.KindField && nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.85
				stats.Resolved++
				return true
			}
		}
	}

	// Pass 3: caller is a method on type T, prefer a same-T field.
	if callerNode := r.cachedGetNode(e.From); callerNode != nil && callerNode.Kind == graph.KindMethod {
		callerRecv := nodeReceiverType(callerNode)
		if callerRecv != "" {
			for _, c := range candidates {
				if c.Kind == graph.KindField && nodeReceiverType(c) == callerRecv {
					e.To = c.ID
					e.Confidence = 0.85
					stats.Resolved++
					return true
				}
			}
		}
	}

	// Pass 4: same-directory field of any owner type — last resort
	// before falling through to method resolution.
	for _, c := range candidates {
		if c.Kind == graph.KindField && r.dirFor(c.FilePath) == callerDir {
			e.To = c.ID
			e.Confidence = 0.6
			stats.Resolved++
			return true
		}
	}
	return false
}

func (r *Resolver) resolveMethodCall(e *graph.Edge, methodName string, stats *ResolveStats) {
	// Same-repo gate first: the per-repo Resolver never resolves a
	// method call across a repo boundary by name. A cross-repo method
	// call is left unresolved for CrossRepoResolver, which carries the
	// import-reachability + workspace-boundary evidence.
	rawCandidates := r.cachedFindNodesByNameInRepoForEdge(methodName, r.callerRepoPrefix(e), e)
	if len(rawCandidates) == 0 {
		if r.applyBuiltinIfKnown(e, methodName, stats) {
			return
		}
		stats.Unresolved++
		return
	}

	// Pass 0: import-reachability filter. Drop candidates whose package
	// the caller's file does not import (or sit in). This collapses
	// most cross-package name collisions before any later pass has to
	// guess. The filter is conservative — when the index is missing or
	// would empty the list, the original candidates pass through.
	candidates := r.filterByReachability(e.FilePath, rawCandidates)

	// #559: consult applicability before any pick. C# call edges carry
	// arg_count / type_arg_count and candidates carry parameter stamps,
	// so narrowing here lets every tier below — the exact-type passes
	// and the fallbacks alike — see only overloads the call site could
	// invoke. Edges without the stamps (every other language) pass
	// through untouched, and a filter that would empty a set keeps it.
	rawCandidates = csharpNarrowMethodByApplicability(e, rawCandidates)
	candidates = csharpNarrowMethodByApplicability(e, candidates)

	// Per-language scope rule lands binding when its evidence is
	// strong (C static / C++ namespace + ADL / Java enclosing class /
	// PHP parent::/self::/namespace). Empty return falls through to
	// the existing receiver-type cascade unchanged.
	if pick := r.preferScopeCandidate(e, methodName, candidates); pick != nil {
		e.To = pick.ID
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.92
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["resolution"] = "scope"
		stats.Resolved++
		return
	}

	callerDir := r.dirFor(e.FilePath)
	receiverType := edgeReceiverType(e)

	// If we have a type hint, try exact type match first. These passes scan
	// the UNFILTERED candidate set: a receiver typed as T binds to T's method
	// regardless of whether the caller's file imports T's package. Import-
	// reachability filtering can drop the receiver's own type when it lives in
	// a sibling Maven directory (src/main vs src/test) the caller never
	// imports, leaving a same-named method in the caller's own class as the
	// only survivor — so the exact-type match must see every candidate, not
	// just the reachable ones.
	if receiverType != "" {
		// An exact receiver-type match is structural evidence — the
		// receiver's type is known and the method belongs to it — so it
		// resolves at ast_resolved (not the name-only ast_inferred tier the
		// cross-package guard reverts) and does not need import-reachability
		// corroboration, which Rust re-exports and unsplit `use a::{b, c}`
		// import groups routinely leave unresolved.
		// Pass 1: same-directory + exact type match (highest confidence).
		for _, c := range rawCandidates {
			if c.Kind == graph.KindMethod &&
				r.dirFor(c.FilePath) == callerDir &&
				nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.95
				e.Origin = graph.OriginASTResolved
				stats.Resolved++
				return
			}
		}
		// Pass 2: exact type match, any directory, over the UNFILTERED
		// candidate set with a uniqueness guard (so a same-named type in
		// another package is never mis-picked).
		var exact *graph.Node
		for _, c := range rawCandidates {
			if c.Kind == graph.KindMethod && nodeReceiverType(c) == receiverType {
				if exact != nil && exact.ID != c.ID {
					exact = nil
					break
				}
				exact = c
			}
		}
		if exact != nil {
			e.To = exact.ID
			e.Confidence = 0.85
			e.Origin = graph.OriginASTResolved
			stats.Resolved++
			return
		}
		// Pass 2b: DI useClass binding. When receiver_type is an
		// abstract/base class that has no method of this name (Passes
		// 1 and 2 found nothing), look for a `provides_for: <type>`
		// edge in the graph — that tells us which concrete class a
		// @Module has bound this abstract to. Prefer candidate methods
		// on that concrete. Without this, the final name-only fallback
		// picks the first implementer alphabetically, which produced
		// SmsNotifier.notify instead of the module-bound EmailNotifier
		// on the NestJS DI fixture.
		if bound := r.boundImplsFor(receiverType); len(bound) > 0 {
			for _, c := range candidates {
				if c.Kind != graph.KindMethod {
					continue
				}
				recv := nodeReceiverType(c)
				if _, ok := bound[recv]; !ok {
					continue
				}
				e.To = c.ID
				e.Confidence = 0.9
				if e.Meta == nil {
					e.Meta = map[string]any{}
				}
				e.Meta["resolution"] = "useClass_binding"
				stats.Resolved++
				return
			}
		}
	}

	// PHP: a receiver typed as a class this repo DEFINES, whose type declares
	// no method of this name, is calling something it INHERITS. Neither
	// fallback below can express that, and both actively mis-bind it.
	//
	// The caller-receiver fallback would answer `$this->handler->setFormatter()`
	// inside BufferHandler::setFormatter with BufferHandler::setFormatter — the
	// caller itself — because it only asks whether the CALLER's class has a
	// method of that name, and the stated receiver (HandlerInterface) is a
	// different type entirely. The locality fallback then picks by directory
	// adjacency, and a PHP package is one directory holding every sibling
	// implementation of an interface, so `$this->getFormatter()` inside
	// AmqpHandler lands on whichever sibling handler sorts first.
	//
	// Leave the edge for resolvePHPOverrideDispatch, which walks the class
	// hierarchy from the stated receiver and binds the nearest declaration.
	// Gated on the receiver being in-repo: when it names a vendor class the
	// hierarchy is not in the graph either, so withholding the fallbacks would
	// only lose the edge without anywhere better to put it.
	if receiverType != "" &&
		r.hasInRepoType(phpBaseTypeName(receiverType), r.callerRepoPrefix(e)) {
		if caller := r.cachedGetNode(e.From); phpTypedReceiverCaller(caller) {
			stats.Unresolved++
			return
		}
	}

	// Fallback: infer receiver type from the caller node.
	// If the caller is a method on type X and there's a candidate method on
	// type X with the same name, prefer it.  This handles e.extractFunctions()
	// where the type env doesn't have a hint for parameter-bound receivers.
	//
	// C# member calls are exempt: a member_call edge still untyped here has
	// an explicit receiver that is NOT this/base (those carry receiver_type
	// from extraction) — a field, parameter, or an expression the tenv
	// couldn't type. Whether the CALLER's class declares a same-named method
	// says nothing about such a receiver, and on facade classes that wrap a
	// same-named repository method this fallback bound the call to the
	// calling method ITSELF (the PHP shield above exists for the identical
	// failure). Leave the site for the extension/locality tiers and
	// semantic enrichment.
	memberCall, _ := e.Meta["member_call"].(bool)
	callerNode := r.cachedGetNode(e.From)
	if callerNode != nil && callerNode.Kind == graph.KindMethod && !memberCall {
		callerRecv := nodeReceiverType(callerNode)
		if callerRecv != "" {
			// Same receiver type + same directory = very high confidence.
			for _, c := range candidates {
				if c.Kind == graph.KindMethod &&
					r.dirFor(c.FilePath) == callerDir &&
					nodeReceiverType(c) == callerRecv {
					e.To = c.ID
					e.Confidence = 0.9
					stats.Resolved++
					return
				}
			}
			// Same receiver type, any directory.
			for _, c := range candidates {
				if c.Kind == graph.KindMethod && nodeReceiverType(c) == callerRecv {
					e.To = c.ID
					e.Confidence = 0.8
					stats.Resolved++
					return
				}
			}
		}
	}

	// C# extension methods: an `x.Foo()` with no matching instance or
	// interface member Foo may be a call to a `static Foo(this X x)`
	// extension. Bind precisely (typed match, else unambiguous name);
	// ambiguous stays unresolved rather than misattributing to a same-name
	// method on an unrelated type.
	if r.tryBindCSharpExtension(e, methodName, receiverType, rawCandidates, stats) {
		return
	}

	// Locality fallback (replaces the previous alphabetical name-only
	// pick). At this point candidates have survived Pass 0 — they all
	// live in packages reachable from the caller. Prefer in this order:
	//
	//   1. Method, same directory  — same package, definitely reachable.
	//   2. Method, any reachable directory  — exactly one survivor: take it.
	//   3. Method, any reachable directory  — multiple survivors: see below.
	//   4. Function, same directory  — pkg.Func() calls land here too.
	//   5. Function, any reachable directory  — same logic as methods.
	//
	// When step 3 finds multiple methods, we prefer the same-package one
	// (locality bias is stronger than any cross-package signal we have
	// without LSP). If no candidate is same-package, we still take the
	// first reachable one — Pass 0 already guaranteed reachability, so
	// this is no longer an arbitrary alphabetical choice across the
	// whole graph but a choice within the caller's import closure.
	var sameDirMethod, sameDirFunc, anyMethod, anyFunc *graph.Node
	methodCount := 0
	for _, c := range candidates {
		// Extension methods are bound only by the type-directed extension
		// rule above; a locality guess must never pick one, which would
		// misattribute `x.Foo()` to an unrelated extension named Foo.
		if isCSharpExtension(c) {
			continue
		}
		// A member call never locality-binds to the calling method itself:
		// `x.Foo()` inside Foo is the facade shape wrapping a same-named
		// member on x, not recursion — self-recursion through `this`
		// carries receiver_type and resolves in the typed passes above.
		if memberCall && c.ID == e.From {
			continue
		}
		switch c.Kind {
		case graph.KindMethod:
			methodCount++
			if r.dirFor(c.FilePath) == callerDir && sameDirMethod == nil {
				sameDirMethod = c
			}
			if anyMethod == nil {
				anyMethod = c
			}
		case graph.KindFunction:
			if r.dirFor(c.FilePath) == callerDir && sameDirFunc == nil {
				sameDirFunc = c
			}
			if anyFunc == nil {
				anyFunc = c
			}
		}
	}

	// Without receiver evidence, more than one reachable method is ambiguous.
	// Preserve the unresolved placeholder instead of inventing a caller for an
	// arbitrary same-named method.
	if receiverType == "" && methodCount > 1 {
		stats.Unresolved++
		return
	}

	// Interface-dispatch annotation: when the receiver type names a
	// graph interface and multiple reachable methods of this name
	// exist, every candidate is a legal runtime target. Mark the edge
	// so downstream consumers don't treat the picked target as
	// definitive. Done before the locality picks so it applies whether
	// the chosen target lands in the same-dir or any-dir bucket.
	if methodCount > 1 && r.receiverIsInterface(e, receiverType) {
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["dispatch"] = "interface"
		// The pick below is a locality heuristic over legal runtime
		// targets — no language server verified it. Stamping the LSP
		// dispatch tier here let a guessed winner masquerade as
		// semantic-provider evidence and poisoned min_tier filtering;
		// ast_inferred is what this actually is. The LSP hierarchy
		// pass upgrades (or fans out) the truly verified sites.
		e.Origin = graph.OriginASTInferred
	}

	// A member call with exactly one method candidate for the name is a
	// grounded inference, not a text-grade guess: there is nowhere else it
	// could bind. Lift it to the ast_inferred tier so min_tier filtering and
	// the cross-package guard treat it as the resolved target it is (the guard's
	// lone-definition exception keeps it from being reverted). Limited to the
	// class-dispatch languages — see loneMemberLang.
	if methodCount == 1 && anyMethod != nil && loneMemberLang(anyMethod.Language) && e.Origin == "" {
		e.Origin = graph.OriginASTInferred
		if e.Confidence == 0 {
			e.Confidence = 0.7
		}
	}

	// Every locality-fallback pick is a name-only guess: no receiver-type
	// evidence tied the call to this method/function, only same-name +
	// reachability. Tag it text_matched (unless the interface-dispatch or
	// Java lone-definition branch above already stamped a tier) so
	// redundant-text suppression drops it once a language server confirms the
	// real target — a common method name like `Get` otherwise fans a call to
	// every same-named method in the package. (Free-function calls resolve in
	// resolveFunctionCall, whose same-directory pick stays untagged.)
	if sameDirMethod != nil {
		e.To = sameDirMethod.ID
		if e.Origin == "" {
			e.Origin = graph.OriginTextMatched
		}
		stats.Resolved++
		return
	}
	if anyMethod != nil {
		e.To = anyMethod.ID
		if e.Origin == "" {
			e.Origin = graph.OriginTextMatched
		}
		stats.Resolved++
		return
	}
	if sameDirFunc != nil {
		e.To = sameDirFunc.ID
		if e.Origin == "" {
			e.Origin = graph.OriginTextMatched
		}
		stats.Resolved++
		return
	}
	if anyFunc != nil {
		e.To = anyFunc.ID
		if e.Origin == "" {
			e.Origin = graph.OriginTextMatched
		}
		stats.Resolved++
		return
	}

	// Name matched something, but not in a way we accepted. Give the
	// built-in classifier a chance before declaring the edge dead —
	// `arr.push` on an Array may also match an unrelated `push` method
	// elsewhere in the graph, in which case we'd rather label it as a
	// built-in than silently misresolve.
	if r.applyBuiltinIfKnown(e, methodName, stats) {
		return
	}
	stats.Unresolved++
}

// receiverIsInterface returns true when the named receiver type
// resolves to a graph node of kind interface. Used by the locality
// fallback to recognise interface-dispatch ambiguity rather than
// treat it as a single-target resolution. Empty receiver type returns
// false.
func (r *Resolver) receiverIsInterface(edge *graph.Edge, receiverType string) bool {
	if receiverType == "" {
		return false
	}
	for _, n := range r.cachedFindNodesByNameInRepoForEdge(receiverType, r.callerRepoPrefix(edge), edge) {
		if n.Kind == graph.KindInterface {
			return true
		}
	}
	return false
}

// applyBuiltinIfKnown routes an unresolvable method call to the
// built-in stub (`builtin::<lang>::<category>::<method>`) when the
// caller's language and the method name are both in our lookup tables.
// Returns true when the edge was rewritten; caller should skip its
// Unresolved increment in that case.
func (r *Resolver) applyBuiltinIfKnown(e *graph.Edge, methodName string, stats *ResolveStats) bool {
	lang := langFromFilePath(e.FilePath)
	if lang == "" {
		return false
	}
	category, ok := classifyBuiltin(methodName, lang)
	if !ok {
		return false
	}
	e.To = graph.StubID(r.callerRepoPrefix(e), graph.StubKindBuiltin, lang, category, methodName)
	stats.External++
	return true
}

// resolveTokenRef resolves the target of an EdgeProvides / EdgeConsumes
// edge that refers to a DI injection token. Tokens are typically
// declared as `export const MY_TOKEN = '...'` (KindVariable) — the
// method/function passes skip them. We name-lookup and accept any kind,
// preferring same-directory matches so token names that happen to
// collide across unrelated files don't pull spurious edges.
func (r *Resolver) resolveTokenRef(e *graph.Edge, name string, stats *ResolveStats) {
	// Same-repo gate: DI token names collide readily across unrelated
	// repos ("TOKEN", "CONFIG", …); a cross-repo first-candidate pick
	// is a name-only guess. CrossRepoResolver handles genuine cross-repo
	// token references.
	candidates := r.cachedFindNodesByNameInRepoForEdge(name, r.callerRepoPrefix(e), e)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}
	callerDir := r.dirFor(e.FilePath)
	for _, c := range candidates {
		if r.dirFor(c.FilePath) == callerDir {
			e.To = c.ID
			e.Confidence = 0.9
			stats.Resolved++
			return
		}
	}
	// No same-dir hit: take the first same-repo candidate so find_usages
	// can still surface the relationship. Confidence drops to reflect
	// uncertainty.
	e.To = candidates[0].ID
	e.Confidence = 0.7
	stats.Resolved++
}

// buildProvidesForIndex walks AllEdges once and materialises a map of
// abstract type → concrete class names declared via `@Module({ providers:
// [{ provide: X, useClass: Y }] })`. boundImplsFor then consults this
// index in O(1) per call edge instead of the O(E) scan that made this
// path the dominant serial cost on large repos — e.g. a vscode index
// had ~10k call edges triggering a full 30k-edge scan each, for 300M
// comparisons that found nothing (vscode has zero NestJS modules).
func (r *Resolver) buildProvidesForIndex() {
	idx := make(map[string]map[string]struct{})
	for ed := range r.graph.EdgesByKind(graph.EdgeProvides) {
		if ed.Meta == nil {
			continue
		}
		pf, _ := ed.Meta[graph.MetaDIProvidesFor].(string)
		if pf == "" {
			continue
		}
		if b, _ := ed.Meta[graph.MetaDIBinding].(string); b != graph.DIBindingUseClass {
			continue
		}
		to := ed.To
		var name string
		if graph.IsUnresolvedTarget(to) {
			name = graph.UnresolvedName(to)
		} else if cut := strings.LastIndex(to, "::"); cut >= 0 {
			name = to[cut+2:]
		} else {
			name = to
		}
		set, ok := idx[pf]
		if !ok {
			set = make(map[string]struct{})
			idx[pf] = set
		}
		set[name] = struct{}{}
	}
	r.providesForIdx = idx
}

func (r *Resolver) clearProvidesForIndex() { r.providesForIdx = nil }

// SetCppIncludeDirs installs the per-source-file C/C++ include search path
// (repo-relative `-I` dirs from compile_commands.json) the relative-import
// resolver uses to bind quoted/angle includes against the real compiler dir
// set. The indexer calls this before ResolveAll; a nil/empty map leaves the
// resolver on its suffix-unique heuristic.
func (r *Resolver) SetCppIncludeDirs(perFile map[string][]string) {
	r.cppIncludeDirs = perFile
}

// SetCppFallbackIncludeDirs installs the heuristic include-root search path
// used for repos with no compile_commands.json (conventional dirs plus
// top-level header dirs, in priority order). Consulted by the ordered probe
// only when no per-file / compile-DB dirs exist. The indexer calls this before
// ResolveAll; a nil/empty slice leaves the resolver on its suffix-unique net.
func (r *Resolver) SetCppFallbackIncludeDirs(dirs []string) {
	r.cppFallbackDirs = dirs
}

// buildReachabilityIndex walks all EdgeImports edges once and records,
// for each caller file, the set of directories of imported (indexed)
// packages. Resolved import edges point at a file node directly;
// unresolved ones still carry `unresolved::import::<importPath>`,
// which we look up via the same dirIndex resolveImport uses, so the
// reachability index is correct even before import resolution races
// to completion in the parallel pass.
//
// Files always include their own directory in the reachable set so
// same-package calls survive the filter.
func (r *Resolver) buildReachabilityIndex() {
	idx := make(map[string]map[string]struct{})

	addDir := func(callerFileID, dir string) {
		if callerFileID == "" || dir == "" {
			return
		}
		set, ok := idx[callerFileID]
		if !ok {
			set = make(map[string]struct{})
			idx[callerFileID] = set
		}
		set[dir] = struct{}{}
	}

	// Seed with each indexed file's own directory, and memoise the per-file
	// dir so filterByReachability never recomputes filePathDir per edge.
	dirByPath := make(map[string]string)
	seedFile := func(file graph.FileNodeIdentity) {
		dir := filePathDir(file.FilePath)
		dirByPath[file.FilePath] = dir
		addDir(file.ID, dir)
	}
	if r.dirIndex != nil {
		for _, files := range r.dirIndex {
			for _, file := range files {
				seedFile(file)
			}
		}
	} else {
		for file := range graph.FileNodeIdentitiesSeq(r.graph, nil) {
			seedFile(file)
		}
	}

	// Materialise the import edges and batch-load the endpoints of the
	// resolved ones (e.To naming a concrete node) in one placement projection.
	// A per-edge GetNode here is a query round-trip per import on a disk
	// backend — the same batching buildImportClosure already applies.
	// Unresolved / external targets never name an in-repo file node, so
	// they're skipped from the batch (their directory comes from dirIndex
	// or not at all).
	var imports []*graph.Edge
	ids := make(map[string]struct{})
	for e := range graph.EdgesLightSeq(r.graph, graph.EdgeImports) {
		imports = append(imports, e)
		if e.To == "" || graph.IsUnresolvedTarget(e.To) || strings.HasPrefix(e.To, "external::") {
			continue
		}
		ids[e.To] = struct{}{}
	}
	var placements map[string]graph.NodePlacement
	if len(ids) > 0 {
		idList := make([]string, 0, len(ids))
		for id := range ids {
			idList = append(idList, id)
		}
		placements = graph.NodePlacementsByIDs(r.graph, idList)
	}

	for _, e := range imports {
		var importedDir string
		switch {
		case graph.IsUnresolvedTarget(e.To) && strings.HasPrefix(graph.UnresolvedName(e.To), "import::"):
			path := strings.TrimPrefix(graph.UnresolvedName(e.To), "import::")
			importedDir = r.importedDirForSpec(jsTSImportCallerFile(e), path)
		case strings.HasPrefix(e.To, "external::"):
			// External / unindexed package — nothing to add.
		default:
			if placement, ok := placements[e.To]; ok && placement.Kind == graph.KindFile {
				importedDir = filePathDir(placement.FilePath)
			}
		}
		if importedDir != "" {
			addDir(e.From, importedDir)
		}
	}

	r.reachableDirsByFile = idx
	r.dirByFilePath = dirByPath
}

// buildReachabilityIndexForPending materialises reachability only for caller
// files in the interactive frontier. Missing edge FilePath metadata must never
// promote an interactive edit to a whole-graph scan: recover the path from the
// concrete From node when possible, otherwise leave that edge unfiltered. The
// reachability filter is explicitly fail-open for an unknown caller path.
func (r *Resolver) buildReachabilityIndexForPending(pending []*graph.Edge, sources map[string]*graph.Node) bool {
	ok, _ := r.buildReachabilityIndexForPendingCached(pending, sources, nil, nil)
	return ok
}

// buildReachabilityIndexForPendingCached materialises direct-import
// reachability only for caller files in the current page. stableByFile is a
// pass-local cache: entries containing only resolved/external imports can be
// reused by overlapping pages, while unresolved imports are rebuilt because
// resolving such an edge may change its target during the pass.

// reachabilityStableFileCap bounds both pass-scoped retentions. Stable-
// reachability entries are one file path plus a handful of directory
// strings; adjacency-retention entries carry the file's raw imported node
// IDs and are the heavier of the two — roughly 5 MB on a production-sized
// workspace and tens of MB at the cap. The cap guards pathological inputs,
// not a working-set target.
const reachabilityStableFileCap = 1 << 16

// reachabilityPageStats reports how much of one page's reachability build was
// served from the pass-scoped stable retention and where the recompute time
// went. Purely observational — prepare logs it so a cold run can show whether
// unstable caller files dominate the per-page rebuild cost.
type reachabilityPageStats struct {
	files     int
	cached    int
	missing   int
	unstable  int
	adjCached int
	fallback  bool
	occupancy int
	project   time.Duration
	place     time.Duration
	match     time.Duration
}

func (r *Resolver) buildReachabilityIndexForPendingCached(
	pending []*graph.Edge,
	sources map[string]*graph.Node,
	stableByFile map[string]map[string]struct{},
	adjacency map[string][]string,
) (bool, reachabilityPageStats) {
	var stats reachabilityPageStats
	if len(adjacency) > reachabilityStableFileCap {
		// Same overflow contract as the stable retention below: a wholesale
		// clear beats piecemeal eviction, which would recreate per-page churn.
		clear(adjacency)
	}
	callerPaths := make(map[string]struct{})
	missingCallerSet := make(map[string]struct{})
	for _, edge := range pending {
		if edge != nil && edge.FilePath == "" && edge.From != "" {
			missingCallerSet[edge.From] = struct{}{}
		}
	}
	missingCallerIDs := make([]string, 0, len(missingCallerSet))
	for id := range missingCallerSet {
		missingCallerIDs = append(missingCallerIDs, id)
	}
	missingCallerPaths := make(map[string]string, len(missingCallerIDs))
	if sources != nil {
		for _, id := range missingCallerIDs {
			if source := sources[id]; source != nil {
				missingCallerPaths[id] = source.FilePath
			}
		}
	} else {
		for id, placement := range graph.NodePlacementsByIDs(r.graph, missingCallerIDs) {
			missingCallerPaths[id] = placement.FilePath
		}
	}
	for _, edge := range pending {
		if edge == nil {
			continue
		}
		callerPath := edge.FilePath
		if callerPath == "" {
			callerPath = missingCallerPaths[edge.From]
		}
		if callerPath != "" {
			callerPaths[callerPath] = struct{}{}
		}
	}

	filePaths := make([]string, 0, len(callerPaths))
	for filePath := range callerPaths {
		filePaths = append(filePaths, filePath)
	}
	sort.Strings(filePaths)
	if stableByFile == nil {
		stableByFile = make(map[string]map[string]struct{}, len(filePaths))
	} else if len(stableByFile) > reachabilityStableFileCap {
		// Entry-capped pass-scoped retention. A stable entry (only resolved /
		// external imports) cannot change while the pass holds the resolve
		// mutex, and caller files recur across pages — re-projecting one on
		// every reappearance is a store read that decays badly once the store
		// no longer fits the page cache. The cap only guards a pathological
		// workspace with more caller files than any real one; overflow clears
		// wholesale rather than evicting piecemeal, which would just recreate
		// the per-page churn this retention exists to avoid.
		clear(stableByFile)
	}

	reachable := make(map[string]map[string]struct{}, len(filePaths))
	dirs := make(map[string]string, len(filePaths))
	missingFiles := make([]string, 0, len(filePaths))
	for _, filePath := range filePaths {
		dir := filePathDir(filePath)
		dirs[filePath] = dir
		if cached, ok := stableByFile[filePath]; ok {
			reachable[filePath] = cached
			stats.cached++
			continue
		}
		reachable[filePath] = map[string]struct{}{dir: {}}
		missingFiles = append(missingFiles, filePath)
	}

	stats.files = len(filePaths)
	stats.missing = len(missingFiles)

	importsByFile := make(map[string][]string, len(missingFiles))
	projectFiles := missingFiles
	if adjacency != nil {
		projectFiles = make([]string, 0, len(missingFiles))
		for _, filePath := range missingFiles {
			if cached, ok := adjacency[filePath]; ok {
				importsByFile[filePath] = cached
				stats.adjCached++
				continue
			}
			projectFiles = append(projectFiles, filePath)
		}
	}
	projectStart := time.Now()
	if len(projectFiles) > 0 {
		var projected map[string][]string
		complete := false
		if projector, ok := r.graph.(graph.ImportAdjacencyProjector); ok {
			projected, complete = projector.ProjectImportAdjacency(projectFiles)
		}
		if !complete {
			// complete=false is a canonicality signal, not a cacheable
			// answer — fallback results are never retained.
			stats.fallback = true
			projected = r.legacyImportTargetsByFile(projectFiles)
		} else if adjacency != nil {
			// Retain raw projections for every projected file, including
			// ones absent from the result map — known-empty is knowledge
			// too, else importless files re-project forever.
			for _, filePath := range projectFiles {
				adjacency[filePath] = projected[filePath]
			}
		}
		for filePath, targets := range projected {
			importsByFile[filePath] = targets
		}
	}
	stats.project = time.Since(projectStart)

	targetSet := make(map[string]struct{})
	for _, filePath := range missingFiles {
		for _, targetID := range importsByFile[filePath] {
			if !graph.IsUnresolvedTarget(targetID) && !strings.HasPrefix(targetID, "external::") {
				targetSet[targetID] = struct{}{}
			}
		}
	}
	targetIDs := make([]string, 0, len(targetSet))
	for id := range targetSet {
		targetIDs = append(targetIDs, id)
	}
	placeStart := time.Now()
	targets := graph.NodePlacementsByIDs(r.graph, targetIDs)
	stats.place = time.Since(placeStart)

	matchStart := time.Now()
	for _, filePath := range missingFiles {
		stable := true
		for _, targetID := range importsByFile[filePath] {
			var importedDir string
			switch {
			case graph.IsUnresolvedTarget(targetID) && strings.HasPrefix(graph.UnresolvedName(targetID), "import::"):
				stable = false
				if r.dirIndex == nil {
					r.buildDirIndexes()
				}
				path := strings.TrimPrefix(graph.UnresolvedName(targetID), "import::")
				importedDir = r.importedDirForSpec(filePath, path)
			case graph.IsUnresolvedTarget(targetID):
				stable = false
			case strings.HasPrefix(targetID, "external::"):
			default:
				if target, ok := targets[targetID]; ok && target.FilePath != "" {
					importedDir = filePathDir(target.FilePath)
					dirs[target.FilePath] = importedDir
				}
			}
			if importedDir != "" {
				reachable[filePath][importedDir] = struct{}{}
			}
		}
		if stable {
			stableByFile[filePath] = reachable[filePath]
		} else {
			stats.unstable++
		}
	}
	stats.match = time.Since(matchStart)
	stats.occupancy = len(adjacency)
	r.reachableDirsByFile = reachable
	r.dirByFilePath = dirs
	return true, stats
}

// legacyImportTargetsByFile preserves the ordinary Store path for in-memory
// stores and for SQLite projections that report malformed provenance. Reads
// remain batched, but this intentionally retains the old materialisation cost
// only on that compatibility path.
func (r *Resolver) legacyImportTargetsByFile(filePaths []string) map[string][]string {
	nodesByFile := r.graph.GetFileNodesByPaths(filePaths)
	idsByFile := make(map[string][]string, len(filePaths))
	var allNodeIDs []string
	for _, filePath := range filePaths {
		for _, node := range nodesByFile[filePath] {
			if node == nil || node.ID == "" {
				continue
			}
			idsByFile[filePath] = append(idsByFile[filePath], node.ID)
			allNodeIDs = append(allNodeIDs, node.ID)
		}
	}
	byNode := r.graph.GetOutEdgesByNodeIDs(allNodeIDs)
	out := make(map[string][]string, len(filePaths))
	for _, filePath := range filePaths {
		for _, id := range idsByFile[filePath] {
			for _, edge := range byNode[id] {
				if edge != nil && edge.Kind == graph.EdgeImports {
					out[filePath] = append(out[filePath], edge.To)
				}
			}
		}
	}
	return out
}

func (r *Resolver) clearReachabilityIndex() {
	r.reachableDirsByFile = nil
	r.dirByFilePath = nil
}

// noteImportEdgeWrite records an imports-kind edge write with no file
// provenance. The pass-scoped import-adjacency retention compares generations
// at page start and clears wholesale on drift — the conservative fallback
// behind the per-file invalidation in noteImportEdgeReindexes, which covers
// the common provenance-carrying writes.
//
// Write-site audit (every ReindexEdges / ReindexUnresolvedEdgeTargets /
// AddBatch / RemoveEdge call in this package):
//   - precise invalidation via noteImportEdgeReindexes/noteImportTargetReindexes/
//     noteImportEdgeRemovals: main page commit + incremental commit
//     (resolver.go), deferred-LSP terminal clears and page reindexes
//     (resolver.go, lsp_resolve.go), attribution batches
//     (attribution_reindex_batch.go, go_builtins_attribution.go,
//     incremental_attribution_cache.go), import-flavored retargets
//     (relative_imports.go, lua_imports.go, godot_res_paths.go), razor
//     @using marker removals (razor_using.go — its reference retargets
//     never touch an imports row), module attribution reindexes
//     (module_attribution.go).
//   - no bump, kind-bounded: generic_param_bind.go (EdgesByKind over
//     non-import kinds), cross_pkg_guard.go (call-edge guard reverts),
//     csharp_partial_merge.go (implements/extends moves between type
//     nodes — imports edges never originate at a type).
//   - no bump, outside any live pass retention: exported framework/registry
//     synthesis passes (celery/mediatr/express/fastapi/django/rails/vapor/
//     grpc/rtk/redux/vuex/ngrx/react/swiftui/uikit/sidekiq/spring/sql/
//     store_factory/temporal/goframe/mybatis/laravel/gdscript/godot-scene/
//     rust/kmp/iface-dispatch/fn-pointer/fn-value/chain-factory/
//     speculative/external_calls/object_registry/value_refs and peers),
//     cross-repo resolver (own lifecycle), framework store wrappers
//     (their callers decide), node-only AddBatch sites, and indexer-side
//     writers (interleave refresh already clears retention wholesale).
func (r *Resolver) noteImportEdgeWrite() {
	r.importEdgeGen++
}

func (r *Resolver) markImportDirty(filePath string) {
	if r.importDirtyFiles == nil {
		r.importDirtyFiles = make(map[string]struct{})
	}
	r.importDirtyFiles[filePath] = struct{}{}
}

// noteImportEdgeReindexes records which caller files' stored import adjacency
// an edge-reindex batch rewrites. Kind migrations count in both directions:
// either the new or the persisted pre-mutation kind being imports rewrites an
// imports row. Known files invalidate precisely — import edges resolve in a
// steady stream throughout a cold pass, so clearing wholesale here would
// evict the retention before it ever serves. Entries without file provenance
// fall back to the wholesale generation.
func (r *Resolver) noteImportEdgeReindexes(batch []graph.EdgeReindex) {
	for _, reindex := range batch {
		importsRow := reindex.OldKind == graph.EdgeImports ||
			(reindex.Edge != nil && reindex.Edge.Kind == graph.EdgeImports)
		if !importsRow {
			continue
		}
		// An identity-preserving rewrite cannot change stored adjacency, so
		// it must not evict the unstable files the retention serves. The one
		// writer producing this shape is the deferred-LSP terminal clears
		// (terminality is meta-only, so OldTo equals the target); they run
		// after the pass's final prepare, making this a guard on the
		// invariant rather than a hot path.
		if reindex.Edge != nil && reindex.OldTo == reindex.Edge.To &&
			reindex.OldKind == "" && reindex.OldFilePath == "" {
			continue
		}
		marked := false
		if reindex.Edge != nil && reindex.Edge.FilePath != "" {
			r.markImportDirty(reindex.Edge.FilePath)
			marked = true
		}
		if reindex.OldFilePath != "" {
			r.markImportDirty(reindex.OldFilePath)
			marked = true
		}
		if !marked {
			r.noteImportEdgeWrite()
		}
	}
}

// noteImportEdgeRemovals is the removal-side sibling of
// noteImportEdgeReindexes: deleting an imports-kind edge rewrites the caller
// file's stored adjacency exactly like a retarget. Provenance-less removals
// fall back to the wholesale generation.
func (r *Resolver) noteImportEdgeRemovals(edges []*graph.Edge) {
	for _, e := range edges {
		if e == nil || e.Kind != graph.EdgeImports {
			continue
		}
		if e.FilePath == "" {
			r.noteImportEdgeWrite()
			continue
		}
		r.markImportDirty(e.FilePath)
	}
}

func (r *Resolver) noteImportTargetReindexes(batch []graph.UnresolvedEdgeTargetReindex) {
	for _, reindex := range batch {
		if reindex.Old.Kind != graph.EdgeImports {
			continue
		}
		if reindex.Old.FilePath == "" {
			r.noteImportEdgeWrite()
			continue
		}
		r.markImportDirty(reindex.Old.FilePath)
	}
}

// importedDirForSpec returns the directory that an unresolved
// `import::<spec>` target makes reachable from callerFile, or "" when no
// credible candidate matches.
//
// The dirIndex / lastDirIndex probe here has the same last-path-component
// looseness as the cascade in resolveImport: for a JS/TS BARE specifier,
// lastDirIndex[spec] is every indexed file under any same-named directory,
// and taking files[0] admits an arbitrary one. Unlike resolveImport this
// mints no edge, so it cannot produce the false `imports` edges of issue
// #450 — but reachableDirsByFile feeds filterByReachability, and a bogus
// reachable directory lets a same-named call candidate defined there
// survive the narrowing and win a call edge. So the same gate applies:
// only a directory entry point counts for a bare specifier, and a declared
// external npm dependency contributes no in-repo directory at all.
func (r *Resolver) importedDirForSpec(callerFile, spec string) string {
	bare := isJSTSBareSpecifier(callerFile, spec)
	if bare && declaresExternalNpmDep(r.npmDep, callerFile, spec) {
		return ""
	}
	pick := func(files []graph.FileNodeIdentity) string {
		for _, f := range files {
			if bare && !isJSTSDirEntryPoint(f.FilePath) {
				continue
			}
			return filePathDir(f.FilePath)
		}
		return ""
	}
	if d := pick(r.dirIndex[spec]); d != "" {
		return d
	}
	if last := lastPathComponent(spec); last != "" {
		return pick(r.lastDirIndex[last])
	}
	return ""
}

// dirFor returns filePathDir(path), served from the per-file memo built in
// buildReachabilityIndex (every indexed file is keyed) and falling back to a
// live computation for paths not in the index. The memo turns the per-edge
// filePathDir in filterByReachability — ~20% of resolution CPU on a large
// monorepo — into a map lookup.
func (r *Resolver) dirFor(path string) string {
	if d, ok := r.dirByFilePath[path]; ok {
		return d
	}
	return filePathDir(path)
}

// filterByReachability narrows candidates to those whose defining file
// sits in a package reachable from the caller file. "Reachable" means:
// (a) same directory as the caller (same package), or (b) directory of
// a file imported via EdgeImports. Returns the original list when the
// reachability index is unavailable (e.g. resolveEdge invoked outside
// a Resolve* pass) or when no candidate is reachable — better to keep
// candidates and let downstream passes handle them than to drop the
// edge in cases where the index is incomplete.
func (r *Resolver) filterByReachability(callerFileID string, candidates []*graph.Node) []*graph.Node {
	if r.reachableDirsByFile == nil || callerFileID == "" {
		return candidates
	}
	reachable, ok := r.reachableDirsByFile[callerFileID]
	if !ok || len(reachable) == 0 {
		return candidates
	}
	out := candidates[:0:0]
	for _, c := range candidates {
		if _, ok := reachable[r.dirFor(c.FilePath)]; ok {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return candidates
	}
	return out
}

// boundImplsFor returns the set of concrete class names bound to the
// abstract type `abstractName` via @Module({ providers: [{ provide: X,
// useClass: Y }] })` entries. Keys are class names (e.g. "EmailNotifier")
// so the caller can match against nodeReceiverType of method candidates.
// Empty when no binding exists.
func (r *Resolver) boundImplsFor(abstractName string) map[string]struct{} {
	if abstractName == "" {
		return nil
	}
	if r.providesForIdx == nil {
		r.buildProvidesForIndex()
	}
	return r.providesForIdx[abstractName]
}

// edgeReceiverType extracts the receiver_type from Edge.Meta, if present.
func edgeReceiverType(e *graph.Edge) string {
	if e.Meta == nil {
		return ""
	}
	if rt, ok := e.Meta["receiver_type"].(string); ok {
		return rt
	}
	return ""
}

// nodeReceiverType extracts the receiver type from a method Node.Meta.
func nodeReceiverType(n *graph.Node) string {
	if n.Meta == nil {
		return ""
	}
	if rt, ok := n.Meta["receiver"].(string); ok {
		return rt
	}
	return ""
}

// resolverTestPathRe-equivalent: a path-shaped test detector that the
// candidate ranker uses to demote definitions that live in test sources.
// It covers the file-suffix conventions (Go `_test.`, JS/TS `.test.` /
// `.spec.`, Python `test_` prefix) and the directory conventions that the
// JVM ecosystems use heavily and that a base-name check alone would miss
// (`src/test/`, `src/jvmTest/`, `src/androidTest/`, `__tests__/`, …).
// Kept resolver-local so the resolver does not import internal/analysis
// (which depends on graph + resolver and would form an import cycle).
func isTestSourcePath(path string) bool {
	if path == "" {
		return false
	}
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))
	switch {
	case strings.Contains(base, "_test."),
		strings.Contains(base, ".test."),
		strings.Contains(base, ".spec."),
		strings.HasPrefix(base, "test_"):
		return true
	}
	// Directory conventions. The slashes are normalised to "/" already on
	// the relative paths the indexer stores; guard the leading-segment
	// case too so a top-level "test/" or "tests/" dir is caught.
	for _, seg := range []string{
		"/test/", "/tests/", "/__tests__/", "/testing/",
		"/jvmtest/", "/androidtest/", "/commontest/", "/androidhosttest/",
		"/src/test/", "/test-utils/",
	} {
		if strings.Contains(lower, seg) {
			return true
		}
	}
	if strings.HasPrefix(lower, "test/") || strings.HasPrefix(lower, "tests/") {
		return true
	}
	return false
}

// nodeIsExportedType reports whether a type/interface candidate is part
// of the module's public surface. Two signals, in order: an explicit
// `Meta["visibility"]` stamped by the extractor (Kotlin/TS/Swift/Java/…
// emit "public" | "private" | "internal" | "protected"), and the
// Go/Rust capitalisation convention as a fallback for languages that do
// not stamp visibility. Unknown → treated as exported, so a candidate
// that simply lacks the metadata is never demoted below an explicitly
// private one.
func nodeIsExportedType(n *graph.Node) bool {
	if n.Meta != nil {
		if vis, ok := n.Meta["visibility"].(string); ok && vis != "" {
			switch strings.ToLower(vis) {
			case "private", "internal", "fileprivate":
				return false
			default:
				return true
			}
		}
	}
	// Capitalisation fallback (Go exported, Rust `pub` types are PascalCase
	// by convention but not enforced — capitalisation is a weak signal,
	// only used to break ties when no visibility metadata exists).
	if n.Name == "" {
		return true
	}
	r := rune(n.Name[0])
	return r < 'a' || r > 'z'
}

// nodeIsNestedType reports whether a type candidate is a member /
// nested type rather than a top-level definition — e.g. the inner
// `Foo.Builder` rather than the top-level `Foo`. Detected from the
// dotted qualified name (`Foo.Builder`) the extractor emits for nested
// types in languages that keep the enclosing-type prefix. Languages
// that flatten nested names to the bare leaf (so `Foo.Builder` is just
// `Builder`) carry no nesting signal here — those candidates tie and
// fall through to the deterministic id-order tiebreak.
func nodeIsNestedType(n *graph.Node) bool {
	return strings.Contains(n.Name, ".")
}

// typeCandidateRank scores a type/interface candidate for an
// `unresolved::Name` type-position / reference / instantiate edge so
// the resolver lands the edge on the *canonical* definition — the same
// node search_symbols returns — rather than whichever same-named rival
// (an external/stub node, a test-file or mock definition, a private /
// nested member type) happens to sort first. Higher rank wins. The
// fields are weighted most-significant-first; same-package proximity is
// folded in by the caller as a final tiebreak so a genuinely local type
// still wins among otherwise-equal candidates without letting a
// same-directory test/stub beat a cross-directory canonical def.
func typeCandidateRank(n *graph.Node) int {
	rank := 0
	// (1) A real, in-repo definition beats a synthetic stub / external
	// placeholder by the widest margin.
	if !graph.IsStub(n.ID) && !nodeIsSyntheticOrExternal(n) {
		rank += 1000
	}
	// (2) A non-test definition beats a test/mock definition.
	if !isTestSourcePath(n.FilePath) {
		rank += 100
	}
	// (3) A top-level type beats a nested / member type.
	if !nodeIsNestedType(n) {
		rank += 10
	}
	// (4) An exported / public type beats a private / internal one.
	if nodeIsExportedType(n) {
		rank += 1
	}
	return rank
}

// nodeIsSyntheticOrExternal reports whether a node is a synthetic
// placeholder (external-call terminal, import alias, re-export shell)
// rather than a real source definition. These carry explicit Meta flags
// stamped by the synthesis passes.
func nodeIsSyntheticOrExternal(n *graph.Node) bool {
	if n.Meta == nil {
		return false
	}
	for _, k := range []string{"external", "external_call", "synthetic", "is_stub", "reexport"} {
		if b, ok := n.Meta[k].(bool); ok && b {
			return true
		}
	}
	return false
}

// bestTypeCandidate picks the canonical type/interface definition from a
// candidate slice for a type-position / reference / instantiate edge.
// Candidates are ranked by typeCandidateRank (real-def > non-test >
// top-level > exported); ties are broken by same-package proximity to
// the caller's directory, then by lexicographically-smallest id for a
// stable, deterministic result across runs and backends. Returns nil
// when no KindType / KindInterface candidate exists.
func bestTypeCandidate(candidates []*graph.Node, callerDir string) *graph.Node {
	var best *graph.Node
	bestRank := -1
	bestSameDir := false
	for _, c := range candidates {
		if c.Kind != graph.KindType && c.Kind != graph.KindInterface {
			continue
		}
		rank := typeCandidateRank(c)
		sameDir := filePathDir(c.FilePath) == callerDir
		if best == nil || candidateBeats(rank, sameDir, c.ID, bestRank, bestSameDir, best.ID) {
			best, bestRank, bestSameDir = c, rank, sameDir
		}
	}
	return best
}

// candidateBeats reports whether a candidate (rank/sameDir/id) should
// replace the current best, applying the tiebreak order: higher rank,
// then same-package proximity, then lexicographically-smallest id (for a
// stable, deterministic result independent of candidate-iteration order
// across in-memory and disk backends).
func candidateBeats(rank int, sameDir bool, id string, bestRank int, bestSameDir bool, bestID string) bool {
	if rank != bestRank {
		return rank > bestRank
	}
	if sameDir != bestSameDir {
		return sameDir
	}
	return id < bestID
}

// memberMethodInfosByType returns the storage layer's per-type member
// method projection verbatim. Routed through MemberMethodsByType when
// the backend implements it; falls back to an EdgesByKind +
// per-edge GetNode walk that synthesises matching info rows.
func memberMethodInfosByType(g graph.Store) map[string][]graph.MemberMethodInfo {
	if cap, ok := g.(graph.MemberMethodsByType); ok {
		return cap.MemberMethodsByType()
	}
	out := map[string][]graph.MemberMethodInfo{}
	for e := range g.EdgesByKind(graph.EdgeMemberOf) {
		method := g.GetNode(e.From)
		if method == nil || method.Kind != graph.KindMethod {
			continue
		}
		out[e.To] = append(out[e.To], graph.MemberMethodInfo{
			MethodID:   method.ID,
			Name:       method.Name,
			FilePath:   method.FilePath,
			StartLine:  method.StartLine,
			RepoPrefix: method.RepoPrefix,
		})
	}
	return out
}

// edgesByKinds yields every edge whose Kind is in the given set,
// using the EdgesByKindsScanner capability when the backend
// implements it (one query — an IN-list scan) and falling back to a
// chain of per-kind EdgesByKind iterators otherwise.
func edgesByKinds(g graph.Store, kinds []graph.EdgeKind) iter.Seq[*graph.Edge] {
	if scan, ok := g.(graph.EdgesByKindsScanner); ok {
		return scan.EdgesByKinds(kinds)
	}
	return func(yield func(*graph.Edge) bool) {
		for _, k := range kinds {
			for e := range g.EdgesByKind(k) {
				if !yield(e) {
					return
				}
			}
		}
	}
}

// nodesByKindsOrAll returns every node whose Kind is in the given
// set, using the NodesByKindsScanner capability when the backend
// implements it (a single kind-IN scan). Adapter stores fall back to the
// required predicate iterator once per requested kind; they never materialise
// the graph-wide node corpus.
func nodesByKindsOrAll(g graph.Store, kinds ...graph.NodeKind) []*graph.Node {
	if scan, ok := g.(graph.NodesByKindsScanner); ok {
		return scan.NodesByKinds(kinds)
	}
	seenKinds := make(map[graph.NodeKind]struct{}, len(kinds))
	seenNodes := make(map[string]struct{})
	var out []*graph.Node
	for _, k := range kinds {
		if _, duplicate := seenKinds[k]; duplicate {
			continue
		}
		seenKinds[k] = struct{}{}
		for n := range g.NodesByKind(k) {
			if n == nil {
				continue
			}
			if _, duplicate := seenNodes[n.ID]; duplicate {
				continue
			}
			seenNodes[n.ID] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}

// structuralParentEdges returns every EdgeExtends / EdgeImplements /
// EdgeComposes edge whose endpoints are both KindType / KindInterface,
// projected as the (FromID, ToID, Origin) tuples InferOverrides
// consumes. Routed through the storage layer's StructuralParentEdges
// capability when the backend implements it (one query — a join with
// kind filters on both sides — no per-edge GetNode). Adapter stores use
// three predicate edge iterators followed by one batched endpoint lookup.
func structuralParentEdges(g graph.Store) []graph.StructuralParentEdgeRow {
	if cap, ok := g.(graph.StructuralParentEdges); ok {
		return cap.StructuralParentEdges()
	}
	parentKinds := []graph.EdgeKind{
		graph.EdgeExtends,
		graph.EdgeImplements,
		graph.EdgeComposes,
	}
	var edges []*graph.Edge
	var endpointIDs []string
	for _, kind := range parentKinds {
		for e := range g.EdgesByKind(kind) {
			if e == nil {
				continue
			}
			edges = append(edges, e)
			endpointIDs = append(endpointIDs, e.From, e.To)
		}
	}
	endpoints := g.GetNodesByIDs(endpointIDs)
	var out []graph.StructuralParentEdgeRow
	for _, e := range edges {
		from := endpoints[e.From]
		to := endpoints[e.To]
		if from == nil || to == nil {
			continue
		}
		if from.Kind != graph.KindType && from.Kind != graph.KindInterface {
			continue
		}
		if to.Kind != graph.KindType && to.Kind != graph.KindInterface {
			continue
		}
		out = append(out, graph.StructuralParentEdgeRow{
			FromID:   from.ID,
			ToID:     to.ID,
			FromKind: from.Kind,
			ToKind:   to.Kind,
			Origin:   e.Origin,
		})
	}
	return out
}

// InferImplements detects structural interface satisfaction by comparing
// method sets and adds EdgeImplements edges from types to interfaces.
// Returns the number of edges added.
func (r *Resolver) InferImplements() int { return r.inferImplements(nil, nil) }

// InferImplementsScoped re-derives EdgeImplements only for (type, interface)
// pairs where the type or the interface is in the affected set — used by
// incremental reindex to avoid the whole-graph type×interface cross product.
// It is add-parity with the full pass: an inferred edge is only ever dropped
// when one of its endpoints' file is evicted, so re-checking every pair with an
// affected endpoint re-lands exactly the dropped edges while the survivors are
// left untouched. Empty scope maps mean "nothing affected" → zero work.
func (r *Resolver) InferImplementsScoped(scopeTypes, scopeIfaces map[string]bool) int {
	return r.inferImplements(scopeTypes, scopeIfaces)
}

// InferOverrides materialises EdgeOverrides edges from method-name
// matches between a type and its supertype. Walks every type that has
// at least one EdgeExtends/EdgeImplements/EdgeComposes outgoing edge,
// then for every member of the type emits an EdgeOverrides edge to a
// matching member on the supertype (matched by name). Returns the
// number of new edges added.
//
// Origin tier is ast_resolved when the supertype edge itself was
// ast_resolved (extractor confirmed parent in the same compilation
// unit); ast_inferred when the supertype edge was inferred from name
// (e.g. InferImplements above); preserved when the parent edge was
// already lsp_resolved/lsp_dispatch (the LSP enrichment path stamps
// EdgeOverrides directly with that origin).
//
// This is the AST half of override inference — works without an LSP available.
func (r *Resolver) InferOverrides() int { return r.inferOverrides(nil) }

// InferOverridesScoped re-derives EdgeOverrides only for parent-edge rows whose
// child or parent type is in the affected set (add-parity with the full pass,
// same reasoning as InferImplementsScoped). Empty scope → zero work.
func (r *Resolver) InferOverridesScoped(scope map[string]bool) int { return r.inferOverrides(scope) }

func lastPathComponent(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return path
	}
	return parts[len(parts)-1]
}

// dirMatchesImport reports whether the (repo-relative) directory dir
// genuinely corresponds to importPath. Unlike a bare last-path-component
// match, dir must be a real *suffix* of the import path — so
// `tree-sitter-c/bindings/go` matches `github.com/x/tree-sitter-c/bindings/go`
// but `tree-sitter-dockerfile/bindings/go` does not. This is the
// precision gate that stops a loose `*/go` match from resolving every
// tree-sitter binding to whichever repo happens to sort first.
//
// Used only to authorise *cross-repo* candidates: a precise import-path
// match is real evidence the caller depends on that repo. Same-repo
// candidates don't need it — a same-repo match can't be the cross-repo
// false positive this guards against.
func dirMatchesImport(dir, importPath string) bool {
	if dir == "" || importPath == "" {
		return false
	}
	return dir == importPath || strings.HasSuffix(importPath, "/"+dir)
}

// callerRepoPrefix returns the RepoPrefix of the node that owns the edge's From field.
func (r *Resolver) callerRepoPrefix(e *graph.Edge) string {
	// cachedGetNode: the pre-warm batch-loads every pending edge's From
	// id, so this is a map hit during ResolveAll instead of one GetNode
	// query per edge.
	fromNode := r.cachedGetNode(e.From)
	if fromNode != nil {
		return fromNode.RepoPrefix
	}
	return ""
}

// callerWorkspaceID returns the source workspace, falling back to its exact
// repository prefix just like candidateWorkspaceID.
func (r *Resolver) callerWorkspaceID(e *graph.Edge) string {
	fromNode := r.cachedGetNode(e.From)
	if fromNode == nil {
		return ""
	}
	if fromNode.WorkspaceID != "" {
		return fromNode.WorkspaceID
	}
	return fromNode.RepoPrefix
}
