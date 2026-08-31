package graph

import (
	"context"
	"iter"
	"sync"
)

// EdgeReindex is the per-edge payload for ReindexEdges. Edge points
// at the (already mutated) Edge value the caller wants the store to
// re-bind; OldFrom / OldTo are the endpoints the edge had BEFORE the
// mutation, so the store can drop both stale adjacency entries while writing
// the new identity. OldFrom may be empty when the source did not move; stores
// then use Edge.From for backward compatibility with resolver callers.
type EdgeReindex struct {
	Edge    *Edge
	OldFrom string
	OldTo   string
	// OldKind is the persisted pre-mutation kind. It is required when a
	// resolver changes the edge identity's Kind before handing the edge to
	// ReindexEdges (for example Reads -> References). Empty preserves backward
	// compatibility and means Edge.Kind did not change.
	OldKind EdgeKind
	// RefreshIdentity replaces the complete stored edge payload while moving
	// its identity from (OldFrom, OldTo, OldFilePath, OldLine) to Edge's
	// current key.
	// Resolver callers leave it false and retain the historical To-only path.
	RefreshIdentity bool
	OldFilePath     string
	OldLine         int
}

// EdgeProvenanceUpdate is the per-edge payload for
// SetEdgeProvenanceBatch. Edge points at the stored Edge whose
// origin should be promoted; NewOrigin is the target tier. The store
// only persists the change (and bumps EdgeIdentityRevisions) when
// NewOrigin differs from the currently stored Origin.
type EdgeProvenanceUpdate struct {
	Edge      *Edge
	NewOrigin string
}

// UnresolvedEdgeScan is the immutable boundary of one unresolved-edge pass.
// HighWaterID is backend-owned and opaque to callers; SQLite uses the edge
// rowid so rows inserted or reinserted while a chunked resolver yields cannot
// leak into the pass. PendingBefore is the exact number of eligible rows at
// that boundary when non-negative; -1 means the backend intentionally skipped
// a pre-count and the consumer must derive the diagnostic count while paging.
type UnresolvedEdgeScan struct {
	HighWaterID int64
	// LowWaterID is the first row the pass may consider, 0 when the whole
	// frontier is the pass's own. A backend that keeps several payload
	// generations in one table sets it to the pinned generation's own first
	// row, so a sparse generation's pass is bounded by what it carries
	// instead of by how much unresolved work the corpus beneath it holds.
	LowWaterID    int64
	PendingBefore int
	// SkipTerminal, when set by the consumer between Begin and the first
	// Read, asks the pager to exclude edges carrying a live durable
	// resolve_terminal stamp AT THE STORE (the promoted column), so a pass
	// that would drop them anyway (a scoped resolve outside
	// GORTEX_WARMUP_FULL_RESOLVE) never loads, decodes, or allocates them.
	// Pagers without column-level support may ignore it — every consumer
	// keeps its own in-memory terminal filter as the semantic authority.
	SkipTerminal bool
	// ScopeAnchors carries the scoped pass's repo prefixes. Consumed by
	// SkipTerminal (a stamped edge anchored to a scope repo in either
	// endpoint is still returned) and by ScopeFilter below. Membership is
	// tested against the from_repo / to_repo_unresolved generated columns —
	// SQLite-side mirrors of graph.RepoPrefixOfID / UnresolvedRepoPrefix —
	// with NULL always passing through (fail-open), so the consumer's exact
	// in-memory filters remain the semantic authority.
	ScopeAnchors []string
	// ScopeFilter, when set with ScopeAnchors, asks the pager to drop rows a
	// scoped resolve provably never reconsiders: source repo not in scope
	// AND target repo-qualified (unresolved form) to a repo not in scope.
	// Bare targets ('') and unrecognized shapes (NULL) always load — the
	// same keep rule as the consumer's edgeInResolveScope, minus the shapes
	// only Go can parse, which fail open.
	ScopeFilter bool
}

// UnresolvedEdgePage is one bounded keyset page from an UnresolvedEdgeScan.
// NextID advances monotonically even across gaps. Exhausted means no row at or
// below the scan's high-water mark remains after this page.
type UnresolvedEdgePage struct {
	Edges     []*Edge
	NextID    int64
	Exhausted bool
}

// UnresolvedEdgePager is an optional disk-backend capability used by the
// resolver's cold pass. Implementations must keyset-page a stable high-water
// snapshot and honour both bounds (an individually oversized row may exceed
// maxBytes so the cursor can still make progress). PendingBefore may be -1
// when the backend deliberately avoids an exact pre-count; consumers then
// derive the diagnostic count while paging. A zero high-water mark still
// means the frontier is known to be empty.
type UnresolvedEdgePager interface {
	BeginUnresolvedEdgeScan(ctx context.Context) (UnresolvedEdgeScan, error)
	ReadUnresolvedEdgePage(ctx context.Context, scan UnresolvedEdgeScan, afterID int64, maxRows, maxBytes int) (UnresolvedEdgePage, error)
}

// Store is the persistence-and-query backend the rest of gortex sees
// behind the *Graph type. The only implementation today is the
// in-memory *Graph; future implementations will include an on-disk
// embedded-DB backend (local single-binary) and a remote network
// client. The interface is the seam that lets the rest of the
// codebase be backend-agnostic.
//
// The method set deliberately mirrors *Graph's current public API so
// the codebase compiles unchanged the day this interface lands. A few
// notes on shape:
//
//   - Slice-shaped reads (AllNodes / AllEdges / FindNodesByName / …)
//     materialise their result in memory — fine for the in-memory
//     store, but disk / remote backends will want iterator-shaped
//     variants added alongside as those implementations come online.
//
//   - Memory-estimate methods (RepoMemoryEstimate /
//     AllRepoMemoryEstimates) are inherently in-memory specific; disk
//     and remote backends return whatever they can compute and callers
//     treat the result as advisory.
//
//   - ResolveMutex() returns a backend-owned mutex that resolver
//     instances (cross-repo, temporal, external) share to serialise
//     their edge-mutation passes against each other and against the
//     indexer's incremental rewrites. A resolve rewrites To / Kind /
//     Origin on the store's live *Edge values before handing them to
//     ReindexEdges, so a whole-graph pass that only SCANS edges has to
//     hold this lock too — the pointers it walks are the same memory.
//     Every backend needs equivalent
//     coordination; the in-memory store uses its existing
//     graph-wide resolveMu, disk backends keep a dedicated mutex
//     alongside their own write serialisation. The returned pointer
//     is owned by the store and must not be Unlocked when not held.
//
// CurrentGenerationRepoEvicter makes replacement scope explicit for stores
// that multiplex multiple immutable graph generations in one backend.
type CurrentGenerationRepoEvicter interface {
	EvictRepoCurrentGeneration(repoPrefix string) (nodesRemoved, edgesRemoved int)
}

// AllGenerationsRepoEvicter is the destructive repository-administration
// surface. Callers must use it only when the repository is being forgotten.
type AllGenerationsRepoEvicter interface {
	EvictRepoAllGenerations(repoPrefix string) (nodesRemoved, edgesRemoved int)
}

// CheckedAllGenerationsRepoEvicter is the retryable form used by
// authoritative repository cleanup. Implementations return storage failures
// instead of converting them to empty counts or a process panic.
type CheckedAllGenerationsRepoEvicter interface {
	EvictRepoAllGenerationsChecked(repoPrefix string) (nodesRemoved, edgesRemoved int, err error)
}

// CheckedBatchAdder is the retryable durable-write form of Store.AddBatch.
//
// Store predates persistent backends and deliberately has a no-error mutation
// surface. Durable implementations opt into this companion capability so
// long-running index builds can abort cleanly on disk-full, I/O, or WAL
// pressure instead of converting the storage failure into a process panic.
// Callers that need a durability boundary should prefer AddBatchChecked below.
type CheckedBatchAdder interface {
	AddBatchChecked(nodes []*Node, edges []*Edge) error
}

// AddBatchChecked writes one batch and returns durable-backend failures when
// the store supports them. In-memory and compatibility stores retain their
// established AddBatch contract.
func AddBatchChecked(store Store, nodes []*Node, edges []*Edge) error {
	if checked, ok := store.(CheckedBatchAdder); ok {
		return checked.AddBatchChecked(nodes, edges)
	}
	store.AddBatch(nodes, edges)
	return nil
}

type Store interface {
	// --- Writes -----------------------------------------------------

	AddNode(n *Node)
	AddBatch(nodes []*Node, edges []*Edge)
	AddEdge(e *Edge)
	SetEdgeProvenance(e *Edge, newOrigin string) bool
	// ReindexEdge is the legacy To-only mutation. Callers that change Kind
	// must use ReindexEdges and provide EdgeReindex.OldKind.
	ReindexEdge(e *Edge, oldTo string)
	// Batched siblings of the per-edge mutators. Same semantics, but
	// disk backends amortise the per-call transaction overhead by
	// committing in implementation-chosen chunks (the in-memory
	// backend just loops). The resolver fans out per-edge mutations
	// across thousands of edges in a single ResolveAll pass, so the
	// per-call form was unusable on disk backends without these.
	// Callers MUST first mutate the *Edge fields they want persisted
	// (To / Kind / Origin / …) before handing the entry over — these
	// methods read the post-mutation Edge state and update the
	// backend's indexes accordingly. When Kind changes, EdgeReindex.OldKind
	// must carry the persisted pre-mutation kind so the old exact identity is
	// removed; zero means Kind did not change.
	ReindexEdges(batch []EdgeReindex)
	SetEdgeProvenanceBatch(batch []EdgeProvenanceUpdate) (changed int)
	RemoveEdge(from, to string, kind EdgeKind) bool
	EvictFile(filePath string) (nodesRemoved, edgesRemoved int)
	// EvictRepo removes the repository only from this handle's logical
	// generation. Backends without generations have one logical generation.
	// Administrative deletion must opt in through AllGenerationsRepoEvicter.
	EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int)

	// --- Point lookups ---------------------------------------------

	GetNode(id string) *Node
	GetNodeByQualName(qualName string) *Node

	// GetNodesByQualNames returns every node for each requested qualified
	// name. Qualified names are lookup labels rather than global identities,
	// so callers that need one target must disambiguate the candidate slice by
	// repository/workspace context. Candidate order is deterministic by node ID.
	// The batch pre-warms import resolution with one indexed store probe.
	GetNodesByQualNames(qualNames []string) map[string][]*Node

	// --- Name + scope queries --------------------------------------

	FindNodesByName(name string) []*Node
	FindNodesByNameInRepo(name, repoPrefix string) []*Node
	// FindNodesByNameContaining returns nodes whose Name (case-
	// insensitive) contains the given substring. The implementation
	// pushes the filter into the backend so only matching rows cross
	// the cgo boundary — the old search-substring fallback's
	// AllNodes()-then-filter pattern materialised the whole node set
	// per query and breaks at Linux-kernel scale (10M+ symbols).
	// limit caps the result set so a very common substring can't blow
	// up memory; pass 0 for "no limit" (caller's responsibility to
	// handle). The order is implementation-defined — callers that
	// need deterministic output sort the result.
	FindNodesByNameContaining(substr string, limit int) []*Node
	GetFileNodes(filePath string) []*Node
	// GetFileNodesByPaths is the batched sibling of GetFileNodes. Backends
	// dedupe empty/repeated paths and execute in implementation-sized chunks;
	// missing paths are absent from the result map. Incremental resolution uses
	// it to avoid one store round trip for every file in a changed-package
	// frontier.
	GetFileNodesByPaths(filePaths []string) map[string][]*Node
	// GetRepoNodes returns nodes owned by the exact prefix. Empty prefix is
	// the single-repository/shadow graph namespace, not a global wildcard.
	GetRepoNodes(repoPrefix string) []*Node
	// GetRepoNodesByLanguage returns only nodes owned by repoPrefix whose
	// promoted Language column matches language. Empty repoPrefix is an exact
	// query for single-repository graphs, not a request for every repository.
	// Backends must implement the compound predicate natively so callers never
	// materialise all repo nodes merely to discard other languages.
	GetRepoNodesByLanguage(repoPrefix, language string) []*Node
	// GetNodesByLanguage is the global language predicate. Backends must
	// evaluate it natively; semantic helpers use it to avoid a repository loop
	// and the edge helper performs one batched source adjacency lookup.
	GetNodesByLanguage(language string) []*Node
	// GetRepoNonContentNodes returns the code/search projection. Empty prefix is
	// the global view; backends must exclude data_class=content natively.
	GetRepoNonContentNodes(repoPrefix string) []*Node

	// --- Edge adjacency --------------------------------------------

	GetOutEdges(nodeID string) []*Edge
	GetInEdges(nodeID string) []*Edge

	// GetInEdgesByNodeIDs / GetOutEdgesByNodeIDs batch the per-node
	// edge fan-out into a single backend round-trip. The rerank
	// pipeline calls these once per Rerank() to materialise every
	// candidate's incoming + outgoing edges in two cgo round-trips
	// instead of 6N per-candidate calls. Missing IDs are absent from
	// the returned map (callers can index without an ok-check via the
	// nil-slice semantics of map[k][]*Edge — range over nil is a no-op).
	GetInEdgesByNodeIDs(ids []string) map[string][]*Edge
	GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge

	// GetEdgeCandidates resolves a batch of exact endpoint and source-site
	// predicates without issuing one adjacency lookup per key. Backends must
	// implement this natively: SQLite performs indexed joins and Graph scans
	// its sharded adjacency buckets under one lock per shard.
	GetEdgeCandidates(endpoints []EdgeEndpoint, sites []EdgeSite) EdgeCandidateSet

	// DistinctExternalTargets returns the unique external-shaped destination
	// IDs used by the supplied edge kinds. Backends must push both the kind and
	// target predicates down and project only the destination ID; no per-kind
	// queries or full Edge materialisation are permitted.
	DistinctExternalTargets(kinds []EdgeKind) []string

	// GetRepoEdges returns every edge whose source node has the given
	// RepoPrefix. Equivalent to GetRepoNodes(r) followed by
	// GetOutEdges(n.ID) for every n, but executes as a single backend
	// query — critical on the disk backend (SQLite)
	// where the per-node loop is O(repo_nodes) round-trips. The
	// in-memory backend forwards to that same nested walk; the disk
	// backends push the join into one server-side query.
	//
	// Empty repoPrefix returns nothing — use AllEdges() for the
	// global view. Nodes with an empty RepoPrefix are unreachable
	// through this method by design (they don't belong to any repo).
	GetRepoEdges(repoPrefix string) []*Edge

	// --- Bulk reads ------------------------------------------------

	AllNodes() []*Node
	AllEdges() []*Edge

	// --- Predicate-shaped reads (push filters into the store) ------
	//
	// These methods replace the pre-Store idiom of `for _, e := range
	// AllEdges() { if cond { ... } }`. On the in-memory backend they
	// iterate the existing internal byKind / byPrefix buckets — same
	// algorithmic cost as the inline filter. On disk backends they
	// fan out to dedicated indexes (idx_edge_kind / idx_node_kind /
	// the to_id LIKE prefix scan, etc.) so the row count actually
	// materialised is proportional to the predicate match, not the
	// whole table.
	//
	// The resolver alone calls AllEdges/AllNodes 34× per pass and
	// throws away >99% of each scan; using these predicate methods
	// instead cut a 503-second disk-backed resolver pass on a 122k-node
	// graph down to seconds.
	//
	// Iterators stop when the consumer's yield returns false.
	// Implementations MUST honour early-stop so callers can break
	// out of a search.

	// EdgesByKind yields every edge whose Kind matches.
	EdgesByKind(kind EdgeKind) iter.Seq[*Edge]

	// NodesByKind yields every node whose Kind matches.
	NodesByKind(kind NodeKind) iter.Seq[*Node]

	// EdgesWithUnresolvedTarget yields every edge whose To has the
	// "unresolved::" prefix. The resolver's main loop calls this
	// once per pass; on disk backends it should range-scan a
	// to-keyed index over the single contiguous "unresolved::" slice
	// rather than materialise the whole edges table. Gate-owned fn-value
	// placeholders (FnValuePlaceholderMarker) are excluded — the master
	// resolver never binds them; the fn-value gate scans them itself by kind.
	EdgesWithUnresolvedTarget() iter.Seq[*Edge]

	// --- Batched point lookups -------------------------------------
	//
	// The resolver fires ~3-10 GetNode / FindNodesByName calls per
	// unresolved edge across its workers. With 10-30k pending edges
	// that's 100k-300k individual queries. On in-memory that's
	// fine (map lookups, nanoseconds). On a disk backend each point
	// lookup is ~ms — at 100k+ calls the per-pass cost is hundreds
	// of seconds, dominating the resolver. The batched variants
	// collapse those into one (or chunked) bulk query.

	// GetNodesByIDs returns a map id→*Node for every input ID present
	// in the store. IDs not in the store are simply absent from the
	// returned map (no nil values). Callers may pass duplicates; the
	// returned map dedupes naturally.
	GetNodesByIDs(ids []string) map[string]*Node

	// FindNodesByNames returns a map name→[]*Node where each slot
	// holds every node whose Name field matches. Names that match no
	// node are absent. Used by the resolver to pre-warm its name-only
	// fallback lookup across the whole pending-edge slice in one
	// batched call instead of one query per edge.
	FindNodesByNames(names []string) map[string][]*Node

	// --- Counts and stats ------------------------------------------

	NodeCount() int
	EdgeCount() int
	// ProxyNodeCountAtLeast answers the federation heap-budget gate without
	// materialising nodes. Implementations may stop as soon as limit matches.
	ProxyNodeCountAtLeast(limit int) bool
	Stats() GraphStats
	RepoStats() map[string]GraphStats
	RepoPrefixes() []string

	// --- Provenance verification -----------------------------------

	EdgeIdentityRevisions() int
	VerifyEdgeIdentities() error

	// --- Memory estimation (advisory; in-memory-specific) ----------

	RepoMemoryEstimate(repoPrefix string) RepoMemoryEstimate

	// AllRepoMemoryEstimates reports one entry per repo the backend has
	// counters for. Backends serve it from maintained counters, not by
	// walking the corpus — the in-memory graph from its shard counters, the
	// SQLite store from the row the indexer persists per repo. Callers must
	// not read a missing or short result as evidence that a repo is empty:
	// a repo that has never finished an index simply has no counter yet.
	AllRepoMemoryEstimates() map[string]RepoMemoryEstimate

	// --- Coordination ----------------------------------------------

	// ResolveMutex returns a backend-owned mutex resolver instances
	// share to serialise edge-mutation passes. See the package doc
	// above for the full contract.
	ResolveMutex() *sync.Mutex
}

// Compile-time assertion: *Graph satisfies the Store interface. If a
// *Graph method's signature ever drifts from the interface, the build
// fails fast here instead of at runtime when a different Store
// implementation gets swapped in.
var _ Store = (*Graph)(nil)

// GoMethodReceiverRebinder is an optional capability for backends that can
// repair Go member_of edges in one set-oriented operation. The Go extractor
// initially targets a method at <method-file>::<receiver-name>; when the
// receiver type is declared in another file of the package that target is a
// phantom ID. The capability rewrites it to the unique canonical Go
// type/interface in the same package directory.
//
// filePath scopes the source method nodes for an incremental pass. An empty
// filePath means the whole graph. Implementations must leave ambiguous names,
// non-Go methods, and already-canonical targets unchanged. The return value is
// the number of old edge rows rewritten or removed by logical-key deduplication.
//
// This remains optional because the in-memory Graph is already efficient with
// the predicate/batch fallback in resolver; disk backends should implement it
// with indexed joins so no edge or node corpus crosses into Go memory.
type GoMethodReceiverRebinder interface {
	RebindGoMethodReceivers(filePath string) (changed int, err error)
}

// GoMethodReceiverBatchRebinder is the partial-index sibling: it limits the
// same set-oriented receiver repair to a deduped changed-file frontier in one
// backend transaction.
type GoMethodReceiverBatchRebinder interface {
	RebindGoMethodReceiversForFiles(filePaths []string) (changed int, err error)
}

// BackendResolver is an optional interface backends MAY implement to
// drain the bulk-tractable subset of the resolver's work entirely
// inside the backend engine (a single server-side bulk UPDATE on the
// disk backend) instead of round-tripping every
// resolution decision back to Go.
//
// Sequencing matters: earlier rules are higher-precision than later
// ones. The orchestrator (ResolveAllBulk) runs them in the order
// listed below so that, e.g., an intra-file call binds to its same-
// file declaration before the unique-name pass would have bound it
// to a same-named symbol elsewhere in the repo.
//
// Each method returns the number of pending edges it drained.
// Unimplemented methods return (0, nil) and the orchestrator skips
// to the next. Errors surface as non-fatal — the orchestrator logs
// and continues with subsequent rules; the Go-side Resolver then
// picks up whatever the bulk pass didn't drain.
type BackendResolver interface {
	// ResolveSameFile: unresolved::Name where target is in the
	// caller's same source file. Strongest precision — a same-file
	// declaration is almost never ambiguous.
	ResolveSameFile() (resolved int, err error)

	// ResolveSamePackage: unresolved::Name where target is in the
	// caller's same directory (Go package). Repo_prefix must match
	// to keep the rule within one source tree.
	ResolveSamePackage() (resolved int, err error)

	// ResolveImportAware: caller's file imports F, target is a
	// symbol in F. Joins against the EdgeImports adjacency.
	ResolveImportAware() (resolved int, err error)

	// ResolveRelativeImports: unresolved::pyrel::<stem> / Dart
	// relative-URI stubs rewritten to the matching KindFile node
	// (e.g. <stem>.py or <stem>/__init__.py for Python).
	// `lang` selects the dialect; empty string runs all supported
	// dialects in turn.
	ResolveRelativeImports(lang string) (resolved int, err error)

	// ResolveCrossRepo: unresolved::Name where exactly one
	// cross-repo Node carries that name. Lower precision than the
	// same-repo rules; sets cross_repo = true on the resulting edge.
	ResolveCrossRepo() (resolved int, err error)

	// ResolveUniqueNames: unresolved::Name where exactly one Node
	// in the entire graph carries that name. Lowest-precision
	// "fallback" — runs after the same-file / same-package /
	// import-aware passes have drained anything they could resolve
	// more precisely.
	ResolveUniqueNames() (resolved int, err error)

	// ResolveExternalCallStubs: ensures every external::* edge
	// target has a corresponding Node row (the existing
	// SynthesizeExternalCalls pass on the Go side). Promotes
	// origin to ast_resolved for edges that now point at a real
	// stub.
	ResolveExternalCallStubs() (resolved int, err error)

	// ResolveAllBulk runs the bulk-tractable methods in
	// precision-descending order and returns the cumulative count
	// of edges resolved across all rules. The default backend
	// implementation should chain the methods above; callers use
	// ResolveAllBulk as the single Resolver-side hook.
	ResolveAllBulk() (totalResolved int, err error)
}

// BulkLoader is an optional interface backends MAY implement to expose
// a high-throughput cold-load fast path that bypasses per-call query
// overhead. The cold-start indexer fires ~2000 small AddBatch calls
// during its parse phase; on backends where every AddBatch round-trips
// through a query parser that per-call cost
// dominates wall time. BulkLoader lets the indexer bracket the parse
// loop with BeginBulkLoad / FlushBulk: AddBatch calls inside the
// bracket buffer rows in memory, and FlushBulk commits them through
// the backend's native bulk primitive.
//
// Contract:
//
//   - BeginBulkLoad may be called on a non-empty store. The cold-start
//     parse phase calls it on an empty store, but later passes (notably
//     the contracts pass, which appends a few hundred contract nodes /
//     edges after resolve) re-enter the bracket against a populated
//     backend. FlushBulk commits via the backend's native bulk
//     primitive in MERGE-on-primary-key mode, so re-appending rows
//     that share an ID with existing data does not duplicate them.
//
//   - Between BeginBulkLoad and FlushBulk, AddBatch is the only mutator
//     the caller may invoke. Reads (GetNode, AllEdges, EdgesByKind, …)
//     return whatever the backend can see — typically nothing buffered.
//     The resolver MUST NOT run until after FlushBulk.
//
//   - FlushBulk commits everything buffered since BeginBulkLoad and
//     returns the backend to normal per-call write mode. An error
//     leaves the store in an implementation-defined state.
//
//   - Calling BeginBulkLoad twice without an intervening FlushBulk,
//     or calling FlushBulk without a prior BeginBulkLoad, is a
//     programmer error; backends are free to panic.
//
// The in-memory *Graph deliberately does NOT implement BulkLoader —
// it's already optimal at the per-call path. bbolt and SQLite likewise
// skip it: their per-call overhead is already amortised by their own
// internal batching (chunked transactions, prepared statements). The
// interface is intentionally opt-in so the indexer can probe with a
// type assertion and fall through to today's per-batch path uniformly.
type BulkLoader interface {
	BeginBulkLoad()
	FlushBulk() error
}

// CoordinatedBulkLoader extends BulkLoader with an outer window for a
// multi-repository cold load. Implementations engage only on an empty store;
// nested per-repository BulkLoader calls must leave the outer window open so
// secondary indexes are rebuilt once after every repository drains.
type CoordinatedBulkLoader interface {
	BeginCoordinatedBulkLoad() bool
	EndCoordinatedBulkLoad() error
}

// CoordinatedBulkAborter is the terminal cleanup companion for a coordinated
// load whose final index seal could not complete. End remains retryable; a
// production owner that will not retry must abort so connection-local unsafe
// pragmas and the pinned writer cannot leak into the running daemon.
type CoordinatedBulkAborter interface {
	AbortCoordinatedBulkLoad() error
}

// SymbolHit is a single full-text-search result: the matched node ID
// plus its relevance score from the backend's scorer (BM25 in
// the disk backend's FTS). Higher score = more relevant.
type SymbolHit struct {
	NodeID string
	Score  float64
}

// SymbolFTSItem is the payload BulkUpsertSymbolFTS takes per node:
// the node's ID and its pre-tokenised text. Reused so the indexer
// can preallocate one slice and the backend can iterate without
// per-element wrapper allocs.
type SymbolFTSItem struct {
	NodeID string
	Tokens string
}

// SymbolSearcher is an optional interface backends MAY implement to
// expose engine-native full-text search over the graph's symbol
// names. When the backing store implements it, the daemon's
// search_symbols path routes through the backend FTS; a store that
// does not gets search.NullBackend and the engine's index-free
// substring scan instead. Serving ranked search from the store's own
// index is what keeps ~100MB of heap off a vscode-scale repo and puts
// the search latency in the same address space as the rest of the
// graph.
//
// Contract:
//
//   - UpsertSymbolFTS is the compatibility single-item write path.
//     Backends used for incremental indexing SHOULD also implement the
//     separate SymbolFTSBatchUpserter capability: it replaces only supplied
//     node IDs without wiping the rest of a repo, and bounds statement size
//     instead of issuing one transaction or lookup per symbol. The store
//     decides how to persist the pre-tokenised text (a sidecar table, an FTS
//     column, an in-engine index — backend choice). Tokens are produced by
//     internal/search.Tokenize so camelCase / snake_case / path-separator
//     semantics match the existing BM25 corpus contract.
//
//   - BulkUpsertSymbolFTS is the cold-start fast path used by the
//     indexer's shadow-swap drain. Implementations SHOULD use the
//     backend's native bulk primitive
//     so a 600k-node repo doesn't pay per-row query parse cost.
//     Idempotent on NodeID like UpsertSymbolFTS — re-running with
//     an overlapping set replaces in place.
//
//     repoPrefix is the per-repo namespace; the store wipes only
//     rows owned by that prefix before COPYing the new items, so
//     multiple repos sharing one store don't clobber each other's
//     FTS corpus. An empty prefix is the standalone Indexer, which
//     owns the whole store, so it wipes everything.
//
//   - BuildSymbolIndex finalises the index after the bulk parse
//     phase. For backends whose FTS index updates automatically on
//     row writes, this is a one-shot cold-start call;
//     for backends that need an explicit build pass, it's where
//     the work happens. Idempotent — safe to call multiple times.
//
//   - SearchSymbols runs a query and returns hits ordered by score
//     descending. The query string is the user's raw input; the
//     backend is expected to tokenise it the same way it tokenised
//     the indexed text (typically by passing it through
//     internal/search.Tokenize before invoking the FTS).
//
//   - Close is implied by graph.Store.Close — no separate
//     teardown method here.
type SymbolSearcher interface {
	UpsertSymbolFTS(nodeID, tokens string) error
	BulkUpsertSymbolFTS(repoPrefix string, items []SymbolFTSItem) error
	BuildSymbolIndex() error
	SearchSymbols(query string, limit int) ([]SymbolHit, error)
}

// SymbolFTSCounter is the optional authoritative document count. Search
// adapters use it to seed readiness lazily, while user-facing reporting reads
// it directly because the adapter's cached snapshot plus incremental deltas can
// drift from the current corpus size.
type SymbolFTSCounter interface {
	SymbolFTSCount() (int, error)
}

// SymbolFTSNormalizationState persists the normalization mode used to produce
// each repository's native symbol FTS corpus. An indexer must advance the
// marker only after an authoritative replacement succeeds; a crash between
// replacement and marker update is safe because the next warm reconcile
// repeats the idempotent rebuild.
type SymbolFTSNormalizationState interface {
	GetSymbolFTSNormalization(repoPrefix string) (mode string, ok bool, err error)
	SetSymbolFTSNormalization(repoPrefix, mode string) error
}

// SymbolFTSBatchUpserter is the optional incremental fast path. It is kept
// separate from SymbolSearcher so alternate search implementations and test
// doubles retain source compatibility. Indexing paths use this capability as
// one batch and never synthesize an N+1 loop from single-item upserts.
type SymbolFTSBatchUpserter interface {
	BatchUpsertSymbolFTS(items []SymbolFTSItem) error
}

// SymbolFTSRepoResetter starts an authoritative, bounded full-repository FTS
// replacement by deleting only the selected repository's owned documents.
// Cold shadow drains call it once, then append SymbolFTSBatchUpserter chunks;
// separating reset from append avoids retaining a whole-repository token slice.
type SymbolFTSRepoResetter interface {
	ResetSymbolFTS(repoPrefix string) error
}

// SymbolFTSBatchDeleter removes a bounded set of symbol documents in one
// backend transaction. Partial re-indexing uses it before the matching batch
// upsert so renamed and line-keyed symbols cannot leave stale FTS rows, and a
// multi-file reconcile never expands into one delete transaction per symbol.
type SymbolFTSBatchDeleter interface {
	BatchDeleteSymbolFTS(nodeIDs []string) error
}

// SymbolFTSRepoReplacer replaces one repository's whole symbol corpus as a
// single all-or-nothing unit. It is what a rebuild against an already-indexed
// repository must use: composing SymbolFTSRepoResetter with a series of
// SymbolFTSBatchUpserter appends commits the wipe first, so a failure part-way
// through leaves the repository with a truncated corpus and durable false
// negatives that only a later full re-index repairs.
//
// Contract:
//
//   - The implementation removes repoPrefix's documents and applies everything
//     produce emits inside one backend transaction. If produce returns an
//     error, or any emit fails, or the commit fails, the corpus that existed
//     before the call is left exactly as it was.
//
//   - produce is called once and streams the replacement corpus through emit
//     in bounded chunks, so neither side retains a whole-repository slice.
//     Each emit either applies the chunk or returns the error that ends the
//     replacement.
//
//   - produce runs while the backend holds its writer for the transaction, so
//     it MUST NOT issue writes against the same store. Reads are fine on a
//     backend whose readers do not contend with its writer; an implementation
//     that cannot offer that MUST refuse the call rather than deadlock.
type SymbolFTSRepoReplacer interface {
	ReplaceSymbolFTS(repoPrefix string, produce func(emit func([]SymbolFTSItem) error) error) error
}

// FileBatchEvicter is the set-oriented sibling of Store.EvictFile. The
// partial/warm reconcile path parses a bounded file chunk first, then asks the
// backend to remove every successfully parsed replacement in one atomic
// mutation. Backends without this optional capability retain the per-file
// fallback; SQLite implements it natively.
type FileBatchEvicter interface {
	EvictFiles(filePaths []string) (nodesRemoved, edgesRemoved int)
}

// ContentHit is one result from a ContentSearcher query: the content
// section node's ID plus locating metadata (file + ordinal), its BM25
// relevance score (higher = more relevant), and a short snippet excerpt
// from the matched body for display.
type ContentHit struct {
	NodeID   string
	FilePath string
	Ordinal  int
	Score    float64
	Snippet  string
}

// ContentFTSItem is the payload AppendContent takes per content section:
// the node's ID, its locating metadata, and the full section body. The
// body is indexed in the content store and is deliberately NOT retained on
// the graph node (the node keeps only a short snippet), so the symbol
// index and the code-oriented passes stay free of bulk content text.
type ContentFTSItem struct {
	NodeID   string
	FilePath string
	Ordinal  int
	Body     string
}

// ContentFTSFileReplacement is one authoritative file-level content-index
// replacement. Items is the complete fresh section set for FilePath; an empty
// Items slice deliberately removes every prior section for that file. The
// replacement owns item paths, so backends persist FilePath even when an item
// carries an empty or stale FilePath.
type ContentFTSFileReplacement struct {
	FilePath string
	Items    []ContentFTSItem
}

// ContentFTSBatchReplacer is the set-oriented cold/partial content writer.
// Implementations replace all supplied files in bounded, atomic groups so a
// crash exposes either the old or new rows for each group, never a wipe-only
// intermediate state. Duplicate FilePath entries are last-write-wins.
//
// It is separate from ContentSearcher so in-memory stores and compatibility
// backends retain their existing per-file API. Indexers probe this optional
// capability and fall back to WipeContentFile + AppendContent when absent.
type ContentFTSBatchReplacer interface {
	ReplaceContentFiles(repoPrefix string, files []ContentFTSFileReplacement) error
}

// ContentSearcher is an optional interface backends MAY implement to
// expose a full-text index over CONTENT (data_class="content") section
// text, kept physically separate from the symbol search (SymbolSearcher).
// Content text never enters the symbol FTS or the code-oriented analysis
// passes; it streams into this dedicated index per file during parsing, so
// a content-heavy repo (a few hundred large PDFs / text dumps that explode
// into hundreds of thousands of sections) cannot flood the symbol index or
// pin every section body in graph nodes.
//
// Contract:
//
//   - WipeContent clears a repo's content rows before a full rebuild
//     (empty prefix = whole table, single-repo / conformance behaviour).
//
//   - WipeContentFile clears one file's content rows — the incremental
//     reindex path when a single content file changes.
//
//   - AppendContent inserts content rows for repoPrefix without wiping —
//     the streamed per-file build path. Callers WipeContent (full) or
//     WipeContentFile (incremental) first, then AppendContent each file's
//     sections. Idempotent only in combination with a preceding wipe.
//
//   - SearchContent runs a query scoped to repoPrefix (empty = all repos)
//     and returns hits ordered by score descending, each with a snippet.
//
//   - BuildContentIndex finalises the index after the bulk parse phase
//     (segment merge / optimize). Idempotent — safe to call repeatedly.
//
//   - ScanContent streams every stored content row (scoped to repoPrefix;
//     empty = all repos) to fn with its FULL body — so a consumer that
//     needs the whole section text (e.g. the content->code linker) reads it
//     from here rather than the graph node, which keeps only a snippet.
//     fn returns false to stop the scan early.
type ContentSearcher interface {
	WipeContent(repoPrefix string) error
	WipeContentFile(filePath string) error
	AppendContent(repoPrefix string, items []ContentFTSItem) error
	SearchContent(query, repoPrefix string, limit int) ([]ContentHit, error)
	BuildContentIndex() error
	ScanContent(repoPrefix string, fn func(nodeID, filePath, body string) bool) error
}

// SymbolBundle is the rerank-shaped result of one search call: the
// matched node, its BM25 score, AND the in/out edges the rerank
// pipeline reads from. Backends that can compose this in a single
// engine round-trip implement SymbolBundleSearcher; callers can fall
// through to SymbolSearcher + GetNodesByIDs + GetIn/OutEdgesByNodeIDs
// when the backend doesn't.
//
// The same node may appear in successive bundles when a multi-call
// retrieval path (primary + expansion) returns it more than once; the
// caller's dedup-by-ID step keeps the per-call shape simple and the
// engine can merge across calls into a single rerank candidate set
// without paying for the duplicate edge fetch — the second occurrence
// already carries the same edges.
type SymbolBundle struct {
	Node     *Node
	Score    float64
	InEdges  []*Edge
	OutEdges []*Edge
}

// SymbolBundleSearcher is an optional capability backends MAY
// implement to fold the symbol-search hot path's three
// per-BM25-call cgo round-trips (FTS + GetNodesByIDs + the rerank
// prepare's batched in/out edge fetch) into one bundled
// engine-side call:
//
//   - FTS yields (id, score)
//   - One batched node materialise + one in-edge fan-in + one
//     out-edge fan-out, all keyed on the same id list, return the
//     bundle.
//
// Backends that do NOT implement this interface still serve the
// search path through SymbolSearcher; callers fall back to
// SymbolSearcher.SearchSymbols + GetNodesByIDs +
// GetIn/OutEdgesByNodeIDs and pay the per-call cgo cost the
// bundled form avoids. The contract is intentionally read-only —
// writes still go through UpsertSymbolFTS / BulkUpsertSymbolFTS on
// the SymbolSearcher.
type SymbolBundleSearcher interface {
	SearchSymbolBundles(query string, limit int) ([]SymbolBundle, error)
}

// ScopedSymbolBundleSearcher is the repo-narrowed variant of
// SymbolBundleSearcher: repoAllow filters inside the FTS query, not
// over a bounded fetched head — a post-fetch filter starves when
// out-of-scope rows outrank every in-scope one deeper than any
// over-fetch. Unowned rows (empty repo prefix) always pass, the
// ScopeAllows carve-out.
type ScopedSymbolBundleSearcher interface {
	SearchSymbolBundlesRepoScoped(query string, repoAllow []string, limit int) ([]SymbolBundle, error)
}

// VectorItem is the payload BulkUpsertEmbeddings takes per node:
// the node's ID and its embedding vector. Length of Vec must
// match the dim the corresponding BuildVectorIndex call declared
// — backends with fixed-width vector columns reject inserts that
// don't match.
type VectorItem struct {
	NodeID string
	Vec    []float32
}

// VectorHit is a single ANN search result: the matched node ID
// plus its distance to the query vector under the backend's
// metric (cosine by default). LOWER distance = more
// similar. Callers that need a similarity score in [0,1] should
// translate via `1 - distance` for cosine.
type VectorHit struct {
	NodeID   string
	ParentID string
	Distance float64
}

// VectorSearcher is an optional interface backends MAY implement to
// expose engine-native HNSW vector indexing over per-symbol
// embedding vectors. When the backing store implements it, the
// daemon's semantic-search path routes through the backend's
// native ANN index instead of holding a parallel in-process
// HNSW — saving roughly `dim × 4 × N` bytes of heap (≈ 1 GB for
// 384-dim × 663k symbols on a Vscode-scale repo).
//
// The bigger win is that vector neighbours and graph traversal can
// be combined in a single server-side round-trip: an ANN seed
// lookup feeding straight into an adjacency match (e.g. "callers
// of the nearest symbols, scoped to one repo and excluding tests").
//
// Today this is three round-trips on the in-process HNSW
// path (ANN → IDs → graph fetch → Go-side filter); with
// VectorSearcher it's one engine-side pipeline.
//
// Contract:
//
//   - UpsertEmbedding is the per-call write path used by
//     incremental reindex when one file's embeddings change.
//
//   - BulkUpsertEmbeddings is the cold-start fast path used by
//     the indexer's embedding pass. Implementations SHOULD use
//     the backend's native bulk primitive so a 600k-node corpus
//     doesn't pay per-row query parse cost. Idempotent on NodeID
//     — re-running with an overlapping set replaces in place.
//
//   - BuildVectorIndex finalises the HNSW index after the bulk
//     populate. The dim parameter declares the embedding
//     width; backends with fixed-width columns lazily create
//     the storage schema on the first BuildVectorIndex call.
//     Idempotent — safe to call multiple times with the same dim.
//
//   - SimilarTo runs an ANN query: given a vector, return the k
//     closest stored vectors ordered by ascending distance.
//
//   - GetEmbeddings reads back the stored vectors for an explicit
//     set of node IDs in one batch. Unlike SimilarTo it does not
//     score or rank — it hands the raw vectors to the caller so a
//     post-rerank refinement stage can recompute exact cosine
//     against the query embedding. IDs with no stored vector are
//     simply absent from the returned map (never an error); an
//     empty input yields an empty map.
//
//   - Close is implied by graph.Store.Close — no separate
//     teardown method here.
type VectorSearcher interface {
	UpsertEmbedding(nodeID string, vec []float32) error
	BulkUpsertEmbeddings(items []VectorItem) error
	BuildVectorIndex(dims int) error
	SimilarTo(vec []float32, limit int) ([]VectorHit, error)
	GetEmbeddings(ids []string) map[string][]float32
}

// VectorCorpusItem is one row in an atomically replaceable repository vector
// corpus. ParentID is empty for an ordinary symbol vector and identifies the
// owning symbol for a synthetic chunk vector.
type VectorCorpusItem struct {
	NodeID   string
	ParentID string
	Vec      []float32
}

// VectorCorpusStats describes the complete committed durable corpus for one
// embedding dimension. ChunkCount counts rows with a non-empty ParentID. The
// Repository fields are populated by replacement and repo-scoped discovery so
// warm startup can distinguish "another repo has vectors" from "this repo's
// migration-cleared corpus still needs rebuilding".
type VectorCorpusStats struct {
	VectorCount           int
	ChunkCount            int
	RepositoryVectorCount int
	RepositoryChunkCount  int
	Dims                  int
}

// AtomicVectorCorpusInstaller is an optional durable-store capability. A
// replacement validates the complete input before mutation, swaps exactly one
// repository's rows in one transaction, and returns complete post-commit stats
// for dims across every repository in the store. An empty item slice is an
// authoritative empty corpus and therefore removes stale rows for repoPrefix.
//
// VectorCorpusStats filters by dims when dims is positive. When dims is zero or
// negative, an empty store returns zero stats, a single-dimension store returns
// that resolved dimension, and a mixed-dimension store returns an error rather
// than guessing which corpus a warm-start delegate should publish.
type AtomicVectorCorpusInstaller interface {
	ReplaceVectorCorpus(ctx context.Context, repoPrefix string, dims int, items []VectorCorpusItem) (VectorCorpusStats, error)
	VectorCorpusStats(ctx context.Context, dims int) (VectorCorpusStats, error)
	VectorCorpusStatsForRepo(ctx context.Context, repoPrefix string, dims int) (VectorCorpusStats, error)
}

// PageRankOpts tunes the PageRank computation. Zero values request
// the backend default — only set fields you genuinely want to
// override so backends can pick their own parallel-tuned defaults
// without the caller second-guessing the constants.
//
// NodeKinds / EdgeKinds restrict the projected subgraph the
// algorithm runs over. Empty means "all kinds" — the algo sees the
// full graph. A non-empty filter is rewritten into a projected-
// graph predicate (e.g. n.kind = "function").
type PageRankOpts struct {
	NodeKinds     []NodeKind
	EdgeKinds     []EdgeKind
	DampingFactor float64
	MaxIterations int
	Tolerance     float64
	Limit         int // 0 = return every ranked node
}

// PageRankHit is one row of the PageRank output: the node ID plus
// its rank score. Hits come back sorted by rank descending.
type PageRankHit struct {
	NodeID string
	Rank   float64
}

// PageRanker is an optional interface backends MAY implement to
// expose engine-native PageRank centrality. When the store
// implements it, the daemon's hotspot / authority-ranking path
// routes through the backend's parallel implementation instead of
// computing degree-centrality in-process.
//
// Engine-native PageRank is qualitatively different from the
// degree-based hotspot analyzer: random-walk authority weights
// rare-but-influential nodes the degree count would miss
// (a low-fan-in API that's called from every domain layer ranks
// higher than a high-fan-in test helper).
//
// Contract:
//
//   - PageRank runs the algorithm against a projected subgraph and
//     returns hits sorted by rank descending. The projection is
//     declared and torn down per call — callers don't manage
//     PROJECT_GRAPH lifecycle directly.
//
//   - The score is normalized so the full corpus sums to 1.
//     Relative ordering — not the absolute value — is what callers
//     should consume.
//
//   - Close is implied by graph.Store.Close.
type PageRanker interface {
	PageRank(opts PageRankOpts) ([]PageRankHit, error)
}

// BundleFingerprintSink is an optional capability a backend MAY
// implement to accept an authoritative per-package content-fingerprint
// map for its SearchSymbolBundles cache. The daemon calls
// SetBundleFingerprints after every analysis pass with the fresh
// fingerprints derived from the live graph; the backend retires any
// cached bundle whose package fingerprint changed and serves the rest.
// Backends without a bundle cache simply do not implement this — the
// daemon's type assertion no-ops.
//
// fps is keyed by package key (the directory the package's files live
// in, repo-prefixed in multi-repo because the stored node file paths
// are). The fingerprints must be edge-aware — fold in the package's
// nodes AND the edges touching them — so a cross-file edge change
// invalidates the bundles whose in/out edges it altered.
type BundleFingerprintSink interface {
	SetBundleFingerprints(fps map[string]uint64)
}

// CommunityOpts tunes Louvain community detection over a projected
// subgraph. Zero values request the backend default
// (maxPhases=20, maxIterations=20). NodeKinds / EdgeKinds
// restrict the projection; an empty filter runs over the full graph.
type CommunityOpts struct {
	NodeKinds     []NodeKind
	EdgeKinds     []EdgeKind
	MaxPhases     int
	MaxIterations int
}

// CommunityHit is one row of the Louvain output: the node ID plus
// the integer community label the algorithm assigned. Two nodes
// with the same CommunityID are in the same community; the actual
// integer is opaque and promises no stability across runs.
type CommunityHit struct {
	NodeID      string
	CommunityID int64
}

// CommunityDetector is an optional interface backends MAY
// implement to expose engine-native Louvain community detection.
// When the store implements it, the daemon's
// analysis.DetectCommunitiesLouvain
// path can delegate the partitioning step and keep the existing
// post-processing (label disambiguation, hub detection, cohesion,
// parent assignment).
//
// Contract:
//
//   - Louvain runs the algorithm against a projected subgraph and
//     returns one hit per node assigning it to a community. The
//     projection is declared and torn down per call.
//
//   - The engine-native implementation treats edges as undirected (the
//     modularity score is computed on the undirected graph even
//     though the projected Edge table is directed). Callers that
//     care about directed modularity should consult the in-process
//     fallback.
//
//   - Close is implied by graph.Store.Close.
type CommunityDetector interface {
	Louvain(opts CommunityOpts) ([]CommunityHit, error)
}

// ComponentOpts tunes connected-component computation over a
// projected subgraph. Zero values request the backend default
// (maxIterations=100). NodeKinds / EdgeKinds restrict
// the projection.
type ComponentOpts struct {
	NodeKinds     []NodeKind
	EdgeKinds     []EdgeKind
	MaxIterations int
}

// ComponentHit is one row of a connected-component output: the
// node ID plus the integer component label the algorithm assigned.
// Two nodes with the same ComponentID are in the same component.
// The integer is opaque.
type ComponentHit struct {
	NodeID      string
	ComponentID int64
}

// ComponentFinder is an optional interface backends MAY implement
// to expose engine-native weakly- and strongly-connected-component
// algorithms. Two methods because the algorithms answer different
// questions:
//
//   - WeaklyConnectedComponents treats edges as undirected — every
//     pair of nodes reachable from each other (ignoring direction)
//     lands in one component. Useful for "is this symbol part of
//     the connected core?" diagnostics.
//
//   - StronglyConnectedComponents respects edge direction — only
//     nodes mutually reachable end up in the same component. The
//     SCC of a call graph is the cycle structure: every non-
//     trivial SCC (size > 1) is a mutual-recursion ring.
//
// When the store implements ComponentFinder, the daemon's
// connectivity diagnostics and circular-dependency detection
// (`analyze kind=wcc` / `analyze kind=scc`) route through it;
// otherwise the in-process analysis.ComputeWCC / analysis.ComputeSCC
// fallbacks run.
type ComponentFinder interface {
	WeaklyConnectedComponents(opts ComponentOpts) ([]ComponentHit, error)
	StronglyConnectedComponents(opts ComponentOpts) ([]ComponentHit, error)
}

// KCoreOpts tunes k-core decomposition. NodeKinds / EdgeKinds
// restrict the projection. The algorithm itself takes no
// per-call parameters — it always computes the full
// decomposition (every node gets its k-degree).
type KCoreOpts struct {
	NodeKinds []NodeKind
	EdgeKinds []EdgeKind
}

// KCoreHit is one row of the k-core output: the node ID plus the
// largest k for which the node remains in the k-core after
// iteratively pruning nodes with degree < k. A node's KDegree is
// its position in the core hierarchy — high values mean the node
// sits inside a densely connected centre.
type KCoreHit struct {
	NodeID  string
	KDegree int64
}

// KCorer is an optional interface backends MAY implement to
// expose engine-native k-core decomposition. When the store
// implements it, the daemon's `analyze kind=kcore` path delegates
// to the engine-native implementation; otherwise
// analysis.ComputeKCore runs in-process.
//
// k-core finds the densest subgraph: the k-core of a graph is
// the largest subgraph where every node has at least k
// neighbours. The k-degree of a node is the largest k for which
// it stays in the k-core — useful for "find the hub-of-hubs", or
// "what's the core infrastructure code that everything depends
// on".
type KCorer interface {
	KCoreDecomposition(opts KCoreOpts) ([]KCoreHit, error)
}

// DeadCodeCandidator is an optional capability backends MAY implement
// to compute the dead-code candidate set server-side. The default Go
// path in analysis.FindDeadCode pulls every node + a batched in-edge
// map and filters in Go; on a disk backend that's
// ~1.3M edge rows per call. A backend that implements
// DeadCodeCandidator runs the equivalent WHERE-NOT-EXISTS filter
// inside the query engine and returns ~hundreds of true candidates,
// skipping the materialise-then-filter loop entirely.
//
// The opts mirror analysis.FindDeadCodeOptions to keep the surface
// in sync — only the fields the backend can act on (kinds + the
// per-kind in-edge allowlist) are honoured. File-path / build-tag
// / well-known-name exclusions stay in Go because they need
// string parsing the backend can't do efficiently.
type DeadCodeCandidator interface {
	// DeadCodeCandidates returns nodes matching the allowed node
	// kinds that have NO incoming edges of the corresponding
	// allowed in-edge kinds. The map keys the in-edge allowlist by
	// node kind — backends evaluate the right allowlist per row.
	// Empty allowedInEdgeKinds for a kind means "any incoming edge
	// counts as usage".
	DeadCodeCandidates(allowedNodeKinds []NodeKind, allowedInEdgeKinds map[NodeKind][]EdgeKind) []*Node
}

// IfaceImplementsRow is the per-row payload returned by
// IfaceImplementsScanner — one tuple per EdgeImplements edge whose
// target is a KindInterface node carrying Meta["methods"]. TypeID
// is the implementing type (the edge's source); IfaceID is the
// interface (the edge's target); IfaceMeta is the interface
// node's decoded Meta map, from which the caller pulls the
// "methods" field. Rows where the interface had no Meta are
// elided server-side.
type IfaceImplementsRow struct {
	TypeID    string
	IfaceID   string
	IfaceMeta map[string]any
}

// IfaceImplementsScanner returns the set of (typeID, interfaceID,
// interfaceMeta) tuples for every EdgeImplements edge where the
// target is a KindInterface node carrying Meta["methods"]. Used by
// analysis.FindDeadCode to compute "type implements interface, so
// these methods are alive even if never called directly". The
// server-side join is one query; the Go-side equivalent fetched
// every interface node then every implements edge separately.
//
// Optional capability — analysis.FindDeadCode falls back to the
// Go-side scan when the backend doesn't implement it.
type IfaceImplementsScanner interface {
	IfaceImplementsRows() []IfaceImplementsRow
}

// NodeDegreeRow is one tuple returned by NodeDegreeAggregator. InCount
// counts EVERY incoming edge (any kind); OutCount counts EVERY outgoing
// edge; UsageInCount counts only the subset whose kind is in the
// "usage" set (Calls, References, Instantiates, Implements, Extends,
// Reads, Writes, Tests). The split exists because connectivity_health
// needs the totals (for isolated / leaf classification) AND the
// usage-edge presence; pulling them in one row saves a second cgo trip
// per node.
//
// UsageInCount is a kind-only count and is NOT a substitute for
// ClassifyZeroEdge, which also weighs each usage edge's provenance —
// a name-only match does not prove use. A caller that needs the
// classification must ask ClassifyZeroEdge for it.
type NodeDegreeRow struct {
	NodeID       string
	InCount      int
	OutCount     int
	UsageInCount int
}

// NodeDegreeAggregator is an optional capability backends MAY
// implement to return per-node in/out edge counts plus a usage-edge
// count, server-side. Used by analysis.GraphConnectivity to replace
// the per-node g.GetInEdges(id) + g.GetOutEdges(id) +
// graph.ClassifyZeroEdge(id) trio — three full edge materialisations
// per node on a disk backend.
// One round-trip returns all three counts and lets the analyzer
// classify isolated / leaf / source-only / sink-only / extraction-gap
// without ever materialising the underlying edge structs.
//
// The usageKinds slice MUST mirror graph.usageEdgeKinds (the kind set
// ClassifyZeroEdge consults; it applies a provenance test on top that
// this count cannot express). Empty usageKinds means UsageInCount is
// always 0; an empty input ids slice returns nil.
//
// Optional capability — GraphConnectivity falls back to the per-node
// GetInEdges/GetOutEdges path when the backend doesn't implement it.
type NodeDegreeAggregator interface {
	NodeDegreeCounts(ids []string, usageKinds []EdgeKind) []NodeDegreeRow
}

// NodeFanRow is one tuple returned by NodeFanAggregator. FanIn counts
// incoming edges whose kind is in the fanInKinds set; FanOut counts
// outgoing edges whose kind is in the fanOutKinds set. The two kind
// sets are passed by the caller so the same capability serves both
// FindHotspots (fanIn = Calls+References, fanOut = Calls) and any
// future analyzer with a different kind split.
type NodeFanRow struct {
	NodeID string
	FanIn  int
	FanOut int
}

// NodeFanAggregator is an optional capability backends MAY implement
// to compute per-node fan-in / fan-out counts filtered by edge kind,
// server-side. Used by analysis.FindHotspots and
// handleAnalyzeHealthScore to replace the AllEdges() materialisation
// they both ran every call (~500k edges on the gortex
// workspace, the bulk of the wall-clock cost on a disk backend). The Go-side
// crossing computation still needs per-edge (from, to) for the
// Calls/References kinds — that runs through EdgesByKind, which
// streams without materialising the full edge set.
//
// Empty ids => nil; empty fanInKinds / fanOutKinds means that side
// is always 0. Output order is unspecified.
//
// Optional capability — both analyzers fall back to the AllEdges scan
// when the backend doesn't implement it.
type NodeFanAggregator interface {
	NodeFanCounts(ids []string, fanInKinds []EdgeKind, fanOutKinds []EdgeKind) []NodeFanRow
}

// FileImporterRow is the per-row payload returned by FileImporters.
// FromFile is the importing file's path (the result the caller cares
// about); FromID / FromName / FromKind describe the node that owns
// the EdgeImports edge, in case the caller needs more than just the
// file list.
type FileImporterRow struct {
	FromFile string
	FromID   string
	FromName string
	FromKind NodeKind
}

// FileImporters is an optional capability backends MAY implement to
// answer "which files import filePath?" with a single backend round-
// trip instead of a Go-side AllEdges() scan. The MCP check_references
// tool's importing-files block hammered AllEdges() per call: ~286k
// edges materialised on the gortex workspace, then a per-
// edge GetNode(e.To) + GetNode(e.From) — multiple thousand backend
// round-trips for a single check_references call. A backend that implements
// FileImporters runs the equivalent join inside the query engine and
// only surfaces the rows that match.
//
// Match semantics mirror the original handler: an EdgeImports edge
// counts when its To node's FilePath equals filePath OR when the To
// node's ID equals filePath (the file's own node id, used by the
// indexer for file-level import bindings). The same-file dedup the
// caller applies stays in Go — backends just stream the candidate
// rows.
//
// Optional capability — handleCheckReferences falls back to the
// AllEdges-driven loop when the backend doesn't implement it.
type FileImporters interface {
	FileImporters(filePath string) []FileImporterRow
}

// InEdgeCounter is an optional capability backends MAY implement to
// compute incoming-edge fan-in counts per target node for a fixed
// set of edge kinds in one backend round-trip. The fallback iterates
// AllEdges() Go-side; on a disk backend that materialises every edge
// (~286k rows on the gortex workspace) just to bucket by To.
// The capability instead runs a single server-side GROUP BY filtered
// by edge kind and ships back only the per-target
// counts — a fraction of the rows and zero per-row Go object alloc.
//
// Used by handleGetUntestedSymbols to compute the calls+references
// fan-in ranking. The map keys are node IDs; values are the integer
// count of matching incoming edges. Targets with zero matching in-
// edges are absent from the map (callers index with `m[id]` and rely
// on the zero-value default).
//
// Optional capability — the handler falls back to AllEdges-driven
// bucketing when the backend doesn't implement it.
type InEdgeCounter interface {
	InEdgeCountsByKind(kinds []EdgeKind) map[string]int
}

// NodesInFilesByKindFinder is an optional capability backends MAY
// implement to answer "which nodes of kinds K live in files F?"
// with a single backend round-trip. The fallback iterates AllNodes()
// Go-side; on a disk backend that materialises the full node table
// per call. The capability instead runs a single server-side query
// filtering by file path and kind, and ships only the matching rows.
//
// Used by handleFindDeclaration to build the per-file enclosing-
// symbol index off the small set of trigram-match file paths. The
// Go fallback's AllNodes pull was ~70k rows on the gortex workspace
// to land at ~hundreds of relevant rows.
//
// Empty files / empty kinds returns nil — never a whole-graph scan.
//
// Optional capability — the handler falls back to AllNodes when the
// backend doesn't implement it.
type NodesInFilesByKindFinder interface {
	NodesInFilesByKind(files []string, kinds []NodeKind) []*Node
}

// NodesInFilesByKindStreamer is the streaming face of
// NodesInFilesByKindFinder: the same projection grouped per file, yielded
// in the caller's (deduped) file order, without materialising the combined
// slice. Only files that contain at least one matching node are yielded, so
// consumers that cache negative results diff the yielded set against their
// request. Optional capability — callers fall back to the Finder form.
type NodesInFilesByKindStreamer interface {
	NodesInFilesByKindSeq(files []string, kinds []NodeKind) iter.Seq2[string, []*Node]
}

// FileMtimeWriter is an optional capability backends MAY implement to
// persist the per-file modification time the indexer uses for its
// incremental-reindex decisions. Lifting this state off the daemon's
// gob+gzip snapshot makes warm restarts read it through the same
// backend the graph already lives in (no second persistence surface
// to keep coherent).
//
// repoPrefix is the indexer's own prefix tag; mtimes is keyed on the
// repo-relative file path (the same key the in-memory Indexer's
// fileMtimes map uses). Empty input is a no-op; empty repoPrefix is
// allowed for single-repo daemons.
type FileMtimeWriter interface {
	BulkSetFileMtimes(repoPrefix string, mtimes map[string]int64) error
}

// FileMtimeReader is the read side of FileMtimeWriter. Returns the
// recorded mtimes for one repo prefix as a fresh map (nil for "no
// data"). Used by warmup to seed ReconcileRepoCtx with the per-file
// mtimes it would otherwise have read from the gob snapshot.
type FileMtimeReader interface {
	LoadFileMtimes(repoPrefix string) map[string]int64
}

// FileMtimeReplacer is an optional capability: persist the AUTHORITATIVE
// full mtime set for a repo prefix, dropping any previously-stored rows for
// files no longer present. The full-index persist path calls this so files
// deleted since the last index are pruned. A backend that only implements
// the upsert-only FileMtimeWriter leaves deleted-file rows behind, and
// warm-restart reconcile then detects them as phantom deletions on every
// restart — forcing a full re-track that never converges. Empty input is a
// no-op (it must never wipe a repo's mtimes from an empty snapshot).
type FileMtimeReplacer interface {
	ReplaceFileMtimes(repoPrefix string, mtimes map[string]int64) error
}

// FileMtimeDeleter is an optional capability: drop the persisted mtime rows
// for a set of repo-relative file paths. The incremental-reindex / watcher
// path calls it when a file is deleted so the persisted set stays in step
// with the live graph (the per-file sibling of FileMtimeReplacer). Empty
// input is a no-op.
type FileMtimeDeleter interface {
	DeleteFileMtimes(repoPrefix string, paths []string) error
}

// EnrichmentState is the per-(repo, provider) completion marker for
// semantic enrichment: the git revision the provider last enriched the repo
// at, when it finished, and the coverage it reached. Enrichment completion
// otherwise lives only in an in-memory map, so a restart forgets it and
// re-runs full LSP hover passes even though the persisted graph already
// carries the edges. Persisting this row lets the deferred-enrichment gate
// skip a provider whose IndexedSHA still matches HEAD on a clean tree.
type EnrichmentState struct {
	RepoPrefix  string
	Provider    string
	IndexedSHA  string
	CompletedAt int64 // unix seconds
	Coverage    float64
}

// EnrichmentStateStore is an optional capability backends MAY implement to
// persist and read back the per-(repo, provider) enrichment completion
// marker. Backends without durable state (the in-memory graph) simply do
// not implement it — the manager type-asserts and, when the assertion
// fails, gates nothing (it always enriches), which is the safe default.
//
// GetEnrichmentState's bool is false when no row has been recorded yet (a
// never-enriched or pre-feature repo/provider), which the gate treats as
// "freshness unknown" and never blocks on.
type EnrichmentStateStore interface {
	GetEnrichmentState(repoPrefix, provider string) (EnrichmentState, bool, error)
	SetEnrichmentState(state EnrichmentState) error
}

// ContractState is the per-repo completion marker for the contract tier: the
// git revision the whole-repo contract pass last committed at, when it
// finished, and how many contracts it wrote.
//
// Contracts are committed all-at-once by the tail of a full index (the
// deferred contract pass, or its inline equivalent), while re-index admission
// is per-file mtime. Those two policies disagree: a run whose tail never
// completes leaves the contract tier EMPTY — not degraded, empty — and every
// later warm restart sees unchanged mtimes, re-extracts nothing, and keeps it
// empty for the life of the index. Nothing on disk recorded that the pass was
// owed, so nothing could tell an unbuilt tier from a repo that genuinely
// declares no contracts.
//
// This row is that record. A repo with no ContractState row has never had a
// whole-repo contract pass commit against this store, so a zero-row answer
// from a contract / route query is unbuilt rather than empty.
type ContractState struct {
	RepoPrefix    string
	IndexedSHA    string
	CompletedAt   int64 // unix seconds
	ContractCount int
}

// ContractStateStore is an optional capability backends MAY implement to
// persist and read back the per-repo contract-tier completion marker.
// Backends without durable state (the in-memory graph) simply do not
// implement it — callers type-assert and, when the assertion fails, report no
// signal at all rather than a false "unbuilt" (an in-memory graph rebuilds
// its contract tier from scratch on every start, so the failure mode this
// marker exists to catch cannot occur there).
//
// GetContractState's bool is false when no row has been recorded yet: either
// the repo's contract pass never completed, or the store predates the marker.
// Both are "not known to be built", which is what the query path reports.
type ContractStateStore interface {
	GetContractState(repoPrefix string) (ContractState, bool, error)
	SetContractState(state ContractState) error
}

// LightNodeReader is an optional store capability: a repo-scoped node scan
// that skips decoding each row's opaque meta blob, populating only struct
// columns and the already-promoted meta keys (signature/visibility/doc/
// external/return_type/.../semantic_type/semantic_source) into Meta. Safe
// for read-only structural use (file path, kind, position, the promoted
// stamp check) — a node fetched this way must NEVER be round-tripped back
// through AddNode/AddBatch, since any non-promoted meta content still
// living in the row's blob would be silently discarded on write. Backends
// with no separate blob (nothing to skip) need not implement it — callers
// fall back to the full scan.
type LightNodeReader interface {
	GetRepoNodesLight(repoPrefix string) []*Node
}

// NodeLightScanner is an optional reader capability for whole-graph algorithms
// that need only the Node struct's identity and location fields. Implementations
// must not materialize Meta, including promoted metadata such as docs and
// signatures. Callers must treat returned nodes as read-only snapshots and must
// not depend on Meta being present. A backend that persists federation proxy
// nodes must preserve Origin, Stub, and FetchedAt so centrality filters remain
// sound (SQLite never persists proxy nodes).
type NodeLightScanner interface {
	AllNodesLight() []*Node
}

// AllNodesLight uses the metadata-free summary projection when the reader
// supports it. In-memory and overlay readers fall back to AllNodes because their
// nodes are already materialized and no disk blob decoding is involved.
func AllNodesLight(s Reader) []*Node {
	if scanner, ok := s.(NodeLightScanner); ok {
		return scanner.AllNodesLight()
	}
	return s.AllNodes()
}

// LightEdgeScanner is an optional store capability: a kind-scoped edge scan
// that skips decoding each row's opaque meta blob. AllEdgesLight returns the
// edges whose Kind is in kinds (an empty kinds list means every kind), Meta
// left nil, with only the struct columns plus the promoted edge fields
// (Origin/Tier/Confidence/ConfidenceLabel/CrossRepo/Line) populated.
//
// The warm-restart centrality/analysis passes (PageRank, HITS, adjacency CSR,
// process discovery, Leiden) each scan the whole call/reference edge set on
// every run. On a large multi-repo graph AllEdges() materialises millions of
// Meta maps those passes never read — the per-edge JSON decode + map
// allocation dominates the scan and inflates warm-restart heap. This capability
// serves the same rows without that cost.
//
// Contract drift, documented once: the ONE Meta key any of these passes still
// consults sits inside graph.ProvenanceWeight, and only for legacy rows whose
// Origin column is empty (it falls back to Meta["semantic_source"] to
// reconstruct the origin). Rows written since the origin-column promotion carry
// Origin directly, so ProvenanceWeight never touches Meta for them; on a
// meta-less legacy row it degrades to the default AST-inferred weight. That
// drift is accepted — the promotion is long-shipped and these passes produce
// approximate rankings, not exact values. Callers MUST NOT rely on any other
// Meta content from a light edge. The in-memory backend returns its live edges
// as-is (Meta present) rather than copy every edge just to strip Meta — the
// contract only promises Meta MAY be absent, so a correct caller reads only the
// promoted fields regardless of backend.
type LightEdgeScanner interface {
	AllEdgesLight(kinds ...EdgeKind) []*Edge
}

// FnValuePlaceholderScanner is an optional store capability: a scan restricted
// to the fn-value gate's placeholder namespace (FnValuePlaceholderMarker, both
// the bare `unresolved::fnvalue::<name>` and the multi-repo
// `<repoPrefix>::unresolved::fnvalue::<name>` COPY-rewrite forms). The gate
// (resolver.ResolveFnValueCallbacks) is the sole consumer and needs only these
// placeholders, but its generic path scans the entire EdgeReferences kind —
// placeholders plus every real reference edge — and Go-filters by Meta["via"].
// On a large multi-repo graph that materialises millions of reference edges on
// every whole-graph synthesizer pass just to keep a handful of placeholders.
// Backends that can range-scan the namespace on a to-keyed index implement
// this; others are served by the gate's EdgesByKind(references) fallback. Meta
// IS populated — the gate reads Meta["via"] and the captured fn_value_name off
// each placeholder.
type FnValuePlaceholderScanner interface {
	FnValuePlaceholderEdges() iter.Seq[*Edge]
}

// ValueRefPlaceholderScanner is an optional disk-backend capability for the
// value-reference gate. It yields EdgeReads whose targets occupy the extractor's
// bare `unresolved::valueref::` namespace, with Meta populated for the gate's
// exact `via == value_ref_candidate` revalidation. Value-ref extraction was
// introduced after repository prefixing already preserved unresolved targets,
// so every current or restorable value-ref placeholder uses the bare form.
type ValueRefPlaceholderScanner interface {
	ValueRefPlaceholderEdges() iter.Seq[*Edge]
}

// EdgePersister is an optional capability backends MAY implement to
// durably rewrite the mutable attribute columns (Confidence,
// ConfidenceLabel, Origin, Tier, Meta) of an edge already present in the
// graph, identified by its full logical key (From, To, Kind, FilePath,
// Line). The in-memory backend never needs it — GetOutEdges hands back
// the live *Edge pointer, so an in-place field mutation is already
// durable. A disk backend, by contrast, returns a detached row copy:
// mutating Confidence / Meta on that copy and calling SetEdgeProvenance
// (which only writes Origin + Tier) silently drops the rest. A pass that
// confirms an edge's full provenance bundle calls PersistEdgeAttributes
// so every backend keeps it. A no matching row is a no-op.
type EdgePersister interface {
	PersistEdgeAttributes(e *Edge)
}

// EdgeMetaBatchPersister is the batched sibling of EdgePersister for passes
// that durably rewrite the attribute columns (Meta in particular) of many
// edges in one sweep — the resolver's terminal-skip stamping walks the whole
// unresolved population and flips a durable flag on the permanently external /
// stdlib / definition-less edges. A disk backend amortises the per-edge
// transaction overhead across the batch. The in-memory backend never
// implements it: a read hands back the live *Edge pointer, so an in-place Meta
// mutation is already durable and the caller's type assertion simply fails.
type EdgeMetaBatchPersister interface {
	PersistEdgeAttributesBatch(edges []*Edge)
}

// NodeNameClassCount is one candidate name's classification tally from
// NodeNameClassCounter: how many same-named nodes are Real (non-stub,
// definition-kind) candidates vs Stub placeholders. A name with neither
// (Real == 0 && Stub == 0) has no matching node at all.
type NodeNameClassCount struct {
	Real int
	Stub int
}

// NodeNameClassCounter is an optional Store capability for classifying a
// batch of candidate identifier names in one round trip instead of one
// FindNodesByName call per name. The resolver's terminal classification
// (reconcileTerminalStamps / classifyTerminal in resolver/terminal.go) asks,
// for each still-unresolved edge's target identifier, whether ANY real
// definition or stub placeholder shares that name anywhere in the graph —
// language family turns out not to affect that decision, so a same-language
// and a cross-language real match both simply count as Real. definitionKinds
// is the set of NodeKind values that count as a "real" candidate (mirrors
// nodeIsDefinitionKind); a node whose kind isn't in that set and isn't a
// stub counts as neither. Backends without this capability fall back to the
// existing per-edge cachedFindNodesByName + IsStub loop.
type NodeNameClassCounter interface {
	CountNodesByNameClass(names []string, definitionKinds []NodeKind) map[string]NodeNameClassCount
}

// CloneShingleWriter is an optional capability backends MAY implement
// to persist each function/method node's MinHash shingle set (a
// []uint64) keyed by node id. Lifting this state into the same backend
// the graph already lives in lets the maintained clone-detection
// count-min sketch (CMS) be rebuilt after a warm restart from the
// persisted snapshot — no re-parse, no second persistence surface to
// keep coherent. It is the shingle-set sibling of FileMtimeWriter.
//
// repoPrefix is the indexer's own prefix tag; rows is keyed on the
// node id whose shingle set the value carries. Empty input is a
// no-op; empty repoPrefix is allowed for single-repo daemons.
// DeleteCloneShingles drops the rows for a set of node ids (evicted
// or rebuilt symbols) so the persisted snapshot stays in step with
// the live graph; empty input is a no-op.
type CloneShingleWriter interface {
	BulkSetCloneShingles(repoPrefix string, rows map[string][]uint64) error
	DeleteCloneShingles(nodeIDs []string) error
}

// CloneShingleReader is the read side of CloneShingleWriter. Returns
// the recorded shingle sets for one repo prefix as a fresh map (nil
// for "no data"). Used by warmup to reseed the clone-detection CMS
// from the persisted snapshot instead of re-shingling every body.
type CloneShingleReader interface {
	LoadCloneShingles(repoPrefix string) (map[string][]uint64, error)
}

// CloneCorpusRow is the compact, durable clone-analysis projection for one
// function or method. Signature is meaningful only when Finalized is true;
// an empty finalized signature records a body intentionally excluded by the
// boilerplate filter, while a non-finalized row still needs the corpus pass.
type CloneCorpusRow struct {
	NodeID     string
	RepoPrefix string
	Shingles   []uint64
	Signature  string
	TokenCount int
	Finalized  bool
}

// CloneCorpusWriter persists bounded clone projection batches. Implementations
// also hydrate Node.Meta["clone_sig"] on ordinary node reads so callers outside
// the clone detector retain the historical query surface.
type CloneCorpusWriter interface {
	BulkSetCloneCorpus(repoPrefix string, rows []CloneCorpusRow) error
}

// CloneCorpusSignatureUpdate is the finalized signature projection for one
// existing clone-corpus row. An empty Signature is authoritative and means the
// body was filtered out after CMS boilerplate removal.
type CloneCorpusSignatureUpdate struct {
	NodeID    string
	Signature string
}

// CloneCorpusSignatureWriter updates finalized signatures without rewriting
// the unchanged raw-shingle BLOB. Backends may omit this optimization; callers
// then fall back to CloneCorpusWriter with complete rows.
type CloneCorpusSignatureWriter interface {
	BulkSetCloneSignatures(repoPrefix string, updates []CloneCorpusSignatureUpdate) error
}

// CloneCorpusRepoReplacer resets one repository's authoritative clone
// projection before a full shadow drain. The empty replacement must clear
// stale rows left by a prior index.
type CloneCorpusRepoReplacer interface {
	ReplaceCloneCorpus(repoPrefix string, rows []CloneCorpusRow) error
}

// UnresolvedInsertionCounter is an optional capability: a monotonic count of
// edge writes whose target is unresolved-shaped (insert or reindex-to). It
// may overcount (conflict-ignored duplicates included) but never undercounts,
// so "counter unchanged across a window" proves no new unresolved work
// appeared in that window — the deferred tail resolve uses this to skip a
// whole-graph fallback pass that would provably resolve nothing.
type UnresolvedInsertionCounter interface {
	UnresolvedEdgeInsertions() uint64
}

// CloneCorpusPager reads one repository's projection in stable node-id order.
// afterNodeID is exclusive; limit must bound the returned page. This avoids a
// whole-repository node/Meta snapshot during cold finalisation and warm LSH
// rebuilds.
type CloneCorpusPager interface {
	CloneCorpusPage(repoPrefix, afterNodeID string, limit int) ([]CloneCorpusRow, error)
}

// CloneCorpusInitialization is an optional capability for stores that can
// distinguish an authoritative empty clone projection from a store created
// before clone-corpus persistence existed. Backends without this capability
// retain the compatibility node scan.
type CloneCorpusInitialization interface {
	CloneCorpusInitialized(repoPrefix string) (bool, error)
	MarkCloneCorpusInitialized(repoPrefix string) error
}

// ConstantValueWriter is an optional capability backends MAY implement
// to persist a KindConstant node's literal value (string / numeric)
// keyed by node id, in a queryable sidecar rather than the gob-encoded
// Meta blob (which is unindexable and decoded on every node load). The
// resolver reads these to dereference a const-identifier dispatch name
// (e.g. `const ChargeCardActivity = "ChargeCard"`) to its value across
// files. It is the const-value sibling of CloneShingleWriter.
//
// rows is keyed on the const node id; the value is the literal text.
// Empty input is a no-op. DeleteConstantValuesByFiles drops the rows
// for a set of re-indexed / evicted files so the snapshot stays in step
// with the live graph.
type ConstantValueWriter interface {
	BulkSetConstantValues(repoPrefix string, rows []ConstantValueRow) error
	DeleteConstantValuesByFiles(repoPrefix string, files []string) error
}

// ConstantValueRepoReplacer atomically replaces the authoritative compact
// constant-value projection for one repository. Full shadow drains use this
// instead of replaying per-file deletes, including the empty-set case that
// must clear stale rows from a prior index.
type ConstantValueRepoReplacer interface {
	ReplaceConstantValues(repoPrefix string, rows []ConstantValueRow) error
}

// ConstantValueReader is the read side of ConstantValueWriter. Returns
// the recorded constant values for the supplied node ids as a fresh map
// (node id → value); ids with no recorded value are omitted. A nil /
// empty ids slice returns an empty map.
type ConstantValueReader interface {
	ConstantValuesByNodeIDs(nodeIDs []string) (map[string]string, error)
}

// ConstantValueRow is one persisted constant value: the const node id,
// its owning file (for file-scoped eviction), and the literal value.
type ConstantValueRow struct {
	NodeID   string
	FilePath string
	Value    string
}

// FileMetaRow is one per-file metadata record: the BLAKE3 content hash (the
// Merkle leaf), byte size, extracted node count, and a JSON array of
// parse-error locations ("" when clean). The Merkle tree stays the
// authoritative change detector; this row is queryable supplementary metadata
// that index_health surfaces per file.
type FileMetaRow struct {
	FilePath    string
	ContentHash string
	Size        int
	NodeCount   int
	Errors      string
}

// FileMetaWriter persists per-file metadata rows. Implemented by the on-disk
// and in-memory backends; the indexer writes through it after each file's
// nodes are batched.
type FileMetaWriter interface {
	SetFileMetas(repoPrefix string, rows []FileMetaRow) error
	DeleteFileMetasByFiles(repoPrefix string, files []string) error
}

// FileMetaRepoReplacer atomically replaces the authoritative compact file
// metadata projection for one repository after a full shadow drain.
type FileMetaRepoReplacer interface {
	ReplaceFileMetas(repoPrefix string, rows []FileMetaRow) error
}

// FileMetaReader is the read side: every recorded file row for a repo prefix.
type FileMetaReader interface {
	FileMetasForRepo(repoPrefix string) ([]FileMetaRow, error)
}

// FileMetaPathReader is the indexed read side for a bounded set of files.
// Watchers use it to confirm an ambiguous equal-mtime event against the
// persisted content receipt without scanning every file in the repository.
type FileMetaPathReader interface {
	FileMetasByPaths(repoPrefix string, filePaths []string) (map[string]FileMetaRow, error)
}

// FileReceipt is the compact polling projection joining one tracked path's
// durable mtime with its optional content identity. ContentHash is empty for
// tracked paths (such as contract manifests) that do not have a files row.
type FileReceipt struct {
	FilePath    string
	MtimeNS     int64
	ContentHash string
	Size        int64
}

// FileReceiptPager reads tracked-file receipts through bounded keyset pages.
// A caller freezes HighWater before a rotation, then advances AfterPath until
// the returned page is short. Implementations must close their database cursor
// before returning so filesystem work cannot pin a read transaction.
type FileReceiptPager interface {
	FileReceiptHighWater(repoPrefix string) (string, error)
	FileReceiptPage(repoPrefix, afterPath, highWaterPath string, limit int, includeContent bool) ([]FileReceipt, error)
}

// RefFact is one durable resolved-reference fact: a reference edge from
// FromID resolved TO ToID with the provenance tier that resolved it. Persisted
// per source file so a reference's resolution is an auditable, diffable record
// — gortex persists the RESOLVED target + its 5-tier provenance (unlike
// codegraph, which persists only unresolved refs behind a flat provenance
// string). The candidate set the resolver chose AMONG (when ambiguous) rides
// in Candidates for later disambiguation / audit.
type RefFact struct {
	RepoPrefix string
	FromID     string
	ToID       string
	Kind       string // edge kind (calls / references / …)
	RefName    string // the referenced symbol's bare name
	Line       int
	Origin     string // provenance tier (lsp_resolved … text_matched)
	Tier       string // coarse provenance label
	Candidates []string
	FilePath   string // source file (denormalized for indexed per-file queries)
	Lang       string
}

// RefFactsWriter is an optional capability backends MAY implement to persist
// per-file resolved-reference facts in a dedicated sidecar table. Sibling of
// CloneShingleWriter / FileMtimeWriter. Empty input is a no-op.
type RefFactsWriter interface {
	BulkSetRefFacts(repoPrefix string, facts []RefFact) error
	DeleteRefFactsByFiles(repoPrefix string, files []string) error
}

// RefFactsRebuilder is the SQLite-first persistence path for the resolved-
// reference sidecar. It derives facts inside the backend from the flat node
// and edge columns, replacing the selected scope atomically without shipping
// the graph corpus through Go. A nil repoPrefixes slice means every repository;
// a non-nil empty slice is a no-op. ReplaceRefFactsForFiles always treats an
// empty file slice as a no-op and scopes deletion by the exact repo prefix, so
// a removed file cannot retain stale facts or erase a same-named file elsewhere.
type RefFactsRebuilder interface {
	RebuildRefFactsForRepos(repoPrefixes []string) error
	ReplaceRefFactsForFiles(repoPrefix string, files []string) error
}

// RefFactsReader is the read side of RefFactsWriter: the persisted facts for a
// set of source files (all files when files is empty), as the audit/diff seed.
type RefFactsReader interface {
	LoadRefFactsByFiles(repoPrefix string, files []string) ([]RefFact, error)
	// LoadRefFactsByTargets is the reverse lookup: the persisted facts that
	// resolve TO any of the given node IDs, grouped by source file path. It
	// answers "which files reference these symbols" durably — live in-edges
	// are dropped when their target file is re-indexed, but the sidecar row
	// keyed by to_id survives, so incremental re-resolution can find the
	// referencing files after the eviction. Empty input yields an empty,
	// non-nil map.
	LoadRefFactsByTargets(repoPrefix string, targetIDs []string) (map[string][]RefFact, error)
}

// ChurnEnrichment is one node's git-churn enrichment, moved out of
// nodes.meta into a typed sidecar (change A). Maps 1:1 to the payload
// internal/churn.EnrichGraph used to stamp on Meta["churn"]/["churn_meta"].
// HeadSHA/Branch/ComputedAt are file-level only (empty for symbols).
type ChurnEnrichment struct {
	NodeID       string
	RepoPrefix   string
	CommitCount  int
	AgeDays      int
	ChurnRate    float64
	LastAuthor   string
	LastCommitAt string // RFC3339
	HeadSHA      string
	Branch       string
	ComputedAt   string // RFC3339
}

// ChurnEnrichmentWriter is an optional capability backends MAY implement
// to persist git-churn enrichment in a typed sidecar instead of the
// node meta blob. When absent the enricher falls back to stamping
// Node.Meta (legacy path).
type ChurnEnrichmentWriter interface {
	BulkSetChurn(repoPrefix string, rows []ChurnEnrichment) error
	DeleteChurn(nodeIDs []string) error
}

// ChurnEnrichmentReader is the read side. ChurnRows returns every churn
// row for repoPrefix; an EMPTY repoPrefix returns ALL rows across repos
// (the cross-repo read get_churn_rate uses, then scope-filters per node).
type ChurnEnrichmentReader interface {
	ChurnRows(repoPrefix string) []ChurnEnrichment
}

// CoverageEnrichment is one node's coverage enrichment (change A),
// moved out of nodes.meta into a typed sidecar.
type CoverageEnrichment struct {
	NodeID      string
	RepoPrefix  string
	CoveragePct float64
	NumStmt     int
	Hit         int
}

// CoverageEnrichmentWriter persists coverage enrichment in a typed
// sidecar. Optional capability; absent → enricher falls back to Meta.
type CoverageEnrichmentWriter interface {
	BulkSetCoverage(repoPrefix string, rows []CoverageEnrichment) error
	DeleteCoverage(nodeIDs []string) error
}

// CoverageEnrichmentReader reads coverage rows; empty repoPrefix returns
// ALL rows across repos.
type CoverageEnrichmentReader interface {
	CoverageRows(repoPrefix string) []CoverageEnrichment
}

// ReleaseEnrichment is one file node's "first appeared in <tag>"
// enrichment (change A), moved out of nodes.meta.
type ReleaseEnrichment struct {
	NodeID     string
	RepoPrefix string
	AddedIn    string
}

// ReleaseEnrichmentWriter persists release enrichment in a typed sidecar.
type ReleaseEnrichmentWriter interface {
	BulkSetReleases(repoPrefix string, rows []ReleaseEnrichment) error
	DeleteReleases(nodeIDs []string) error
}

// ReleaseEnrichmentReader reads release rows; empty repoPrefix → all.
type ReleaseEnrichmentReader interface {
	ReleaseRows(repoPrefix string) []ReleaseEnrichment
}

// BlameEnrichment is one node's latest-author enrichment (change A),
// moved out of nodes.meta. Timestamp is unix seconds.
type BlameEnrichment struct {
	NodeID     string
	RepoPrefix string
	Commit     string
	Email      string
	Timestamp  int64
}

// BlameEnrichmentWriter persists blame enrichment in a typed sidecar.
type BlameEnrichmentWriter interface {
	BulkSetBlame(repoPrefix string, rows []BlameEnrichment) error
	DeleteBlame(nodeIDs []string) error
}

// BlameEnrichmentReader reads blame rows; empty repoPrefix → all.
type BlameEnrichmentReader interface {
	BlameRows(repoPrefix string) []BlameEnrichment
}

// EdgesByKindsScanner is an optional capability backends MAY
// implement to stream every edge whose Kind is in the supplied set,
// in a single backend round-trip. The fallback iterates AllEdges()
// Go-side and filters in process — on a disk backend AllEdges
// materialises every edge (~286k rows on the gortex workspace) for the
// edge-driven analyzers (channel_ops, pubsub, k8s_resources,
// kustomize, error_surface, …) that only care about a handful of
// kinds. The capability runs a single server-side query filtering
// by edge kind and ships back only the matching rows.
//
// The single-kind variant EdgesByKind already exists, but the
// analyzers in question typically need 2-5 kinds in one pass; firing
// EdgesByKind once per kind would issue N independent backend queries
// when the planner can naturally batch them with an IN-list. Calling
// EdgesByKinds with one kind is equivalent to EdgesByKind for that
// kind — backends should still prefer the IN-list path so the call
// site never branches on len(kinds).
//
// Empty kinds yields nothing — never a whole-table scan. Iterators
// stop when the consumer's yield returns false; implementations MUST
// honour early-stop so callers can break out of a search.
//
// Optional capability — analyzers fall back to per-kind EdgesByKind
// iteration when the backend doesn't implement it.
type EdgesByKindsScanner interface {
	EdgesByKinds(kinds []EdgeKind) iter.Seq[*Edge]
}

// NodesByKindsScanner is an optional capability backends MAY implement
// to fetch every node whose Kind is in the supplied set in a single
// backend round-trip. Replaces the AllNodes() + Go-side `if n.Kind !=
// allowed` filter used by the metadata-oriented analyze handlers
// (todos, stale_code, stale_flags, ownership, coverage_gaps,
// coverage_summary, cgo_users, wasm_users, orphan_tables,
// unreferenced_tables). Each of those scans the entire node table just
// to keep one or two kinds — on a disk backend that's ~70k rows on
// the gortex workspace per call. The capability runs a single
// server-side query filtering by node kind and ships only the
// matching rows.
//
// Why a separate kinds-IN scanner instead of looping the existing
// NodesByKind iterator per kind: on a disk backend NodesByKind is one
// query per call. Looping it for {function, method} doubles the round-trip
// count and rebuilds the row decoder for each pass. One IN-list query
// returns the union directly. The dedup is intentional — duplicated
// kinds in the input never reach the IN-list, matching the in-memory
// reference's behaviour.
//
// Optional capability — handlers fall back to AllNodes-driven scanning
// when the backend doesn't implement it. Empty kinds returns nil
// without touching the backend.
type NodesByKindsScanner interface {
	NodesByKinds(kinds []NodeKind) []*Node
}

// EdgeAdjacencyForKinds is an optional capability backends MAY
// implement to stream (from, to) id pairs for every edge whose Kind
// is in the supplied edge-kind set AND whose endpoints both belong
// to the supplied node-kind set. The shape covers the betweenness /
// centrality adjacency build that today calls EdgesByKinds and
// filters Go-side: on a disk backend the per-edge row carries ~10 string
// columns, multiplied by ~286k edges on the gortex
// workspace, just for a build that uses only From/To. The
// capability returns a 2-column projection from a single server-side
// join — every endpoint kind is enforced by the planner, so neither
// the cross-kind edges nor the irrelevant columns ever leave the backend.
//
// Empty edgeKinds or empty nodeKinds yields nothing — never a
// whole-table scan. Iterators stop when the consumer's yield
// returns false; implementations MUST honour early-stop.
//
// Optional capability — analyzers fall back to EdgesByKinds when
// the backend doesn't implement it.
type EdgeAdjacencyForKinds interface {
	EdgeAdjacencyForKinds(edgeKinds []EdgeKind, nodeKinds []NodeKind) iter.Seq[[2]string]
}

// ExternalCallCandidates is the optional pushdown for external-call
// synthesis. ExternalCallCandidateEdges returns only the call / reference
// edges whose target is an un-indexed external-package terminal
// (dep:: / stdlib:: / external::, including the per-repo-prefixed stdlib
// form) or an already-materialised external-call:: node — the exact set
// the synthesizer might act on. The disk backend selects these rows with
// a GLOB predicate (served by a partial index) instead of marshaling
// every call edge in the graph and filtering Go-side; the marshaling /
// allocation saving dominates on large graphs even when the planner
// full-scans.
//
// Optional capability — resolver.externalCallCandidateEdges falls back to
// the EdgesByKinds scan + prefix filter when the backend doesn't
// implement it (the in-memory store, where there is no row marshaling to
// save).
type ExternalCallCandidates interface {
	ExternalCallCandidateEdges() []*Edge
}

// CommunityCrossingsByKind is an optional capability backends MAY
// implement to return per-source crossing counts for edges whose
// Kind is in the supplied set, given a node→community membership
// map. A "crossing" is an edge whose source community differs from
// its target community; the count is keyed by source id.
//
// Replaces the FindHotspots.countCrossings loop that today iterates
// EdgesByKind twice and tallies per-source Go-side: on the gortex
// workspace the two EdgesByKind passes materialised the full call /
// reference bucket (~286k rows × ~10 columns) just to
// derive a thousand-row aggregate. The capability ships only the
// (from, to) projection — the community comparison runs Go-side
// because the community map isn't a Node column today.
//
// Empty kinds or an empty community map returns nil. The map keys
// in the result MUST be source ids whose count is non-zero —
// implementations MUST drop zero-count rows so callers can probe
// existence without a >0 check.
//
// Optional capability — analyzers fall back to EdgesByKind iteration
// when the backend doesn't implement it.
type CommunityCrossingsByKind interface {
	CommunityCrossingsByKind(kinds []EdgeKind, nodeToComm map[string]string) map[string]int
}

// NodeIDsByKinds is an optional capability backends MAY implement
// to return just the IDs of nodes whose Kind is in the supplied
// set. Replaces NodesByKinds in ranking paths (betweenness,
// hotspots) that only need to iterate ids — the full *Node carries
// ~10 string columns over cgo per row, and the candidate set is
// thousands of function/method rows, so the projection drops the
// per-call cgo allocation count by an order of magnitude.
//
// Empty kinds returns nil without touching the backend. Duplicated
// input kinds must NOT duplicate the output — backends MUST dedup
// the kind set in the IN-list.
//
// Optional capability — callers fall back to NodesByKinds when the
// backend doesn't implement it.
type NodeIDsByKinds interface {
	NodeIDsByKinds(kinds []NodeKind) []string
}

// EdgeKindCounter is an optional capability backends MAY implement
// to return one row per distinct edge kind with its occurrence
// count, server-side. Used by handleGetSurprisingConnections to
// derive the "rare kinds" set (kinds whose share of all edges is at
// or below the rare_kind_pct threshold) without materialising every
// edge over cgo just to bucket by Kind. On the gortex workspace the
// AllEdges() bucket pass was ~286k edges over cgo per call; the
// aggregator returns ~30 rows.
//
// The map's key is the EdgeKind; the value is the integer occurrence
// count. Empty graph returns nil (or an empty map — callers MUST
// treat both as "no rare kinds detected").
//
// Optional capability — handleGetSurprisingConnections falls back
// to the AllEdges-driven kind bucketing when the backend doesn't
// implement it.
type EdgeKindCounter interface {
	EdgeKindCounts() map[EdgeKind]int
}

// CrossRepoEdgeRow is one tuple returned by CrossRepoEdgeAggregator.
// Kind is the cross_repo_* edge kind verbatim. FromRepo / ToRepo
// are the source / target node's RepoPrefix; Count is the number of
// underlying edges that share the triple.
type CrossRepoEdgeRow struct {
	Kind     EdgeKind
	FromRepo string
	ToRepo   string
	Count    int
}

// CrossRepoEdgeAggregator is an optional capability backends MAY
// implement to return pre-grouped cross-repo edge counts. Used by
// the get_architecture handler's cross_repo rollup, which previously
// scanned AllEdges() + per-edge GetNode(from)+GetNode(to) just to
// emit one row per (kind, from_repo, to_repo). On the gortex
// workspace that meant ~286k edge rows + ~thousands of GetNode
// round-trips for typically <100 cross-repo rows. The
// aggregator runs one server-side GROUP BY and ships only the surviving
// per-triple counts.
//
// Cross-repo edges are identified by graph.BaseKindForCrossRepo —
// the disk implementation MUST use the same kind list (so single-
// repo graphs return an empty slice, not a whole-graph scan).
//
// Optional capability — handleGetArchitecture falls back to the
// AllEdges-driven loop when the backend doesn't implement it.
type CrossRepoEdgeAggregator interface {
	CrossRepoEdgeCounts() []CrossRepoEdgeRow
}

// FileImportCountRow is one tuple returned by FileImportAggregator.
// FilePath is the imported file path (the target node's FilePath, or
// the target node's ID when the indexer pointed the import edge at
// the file node directly). Count is the number of distinct EdgeImports
// edges whose To resolves to that path.
type FileImportCountRow struct {
	FilePath string
	Count    int
}

// FileImportAggregator is an optional capability backends MAY
// implement to return per-target-file incoming-imports counts in
// one backend round-trip. Used by mostImportedFiles (shared between
// get_repo_outline and suggest_queries) which previously scanned
// AllEdges() + per-edge GetNode(to) just to bucket counts by path.
// On the gortex workspace that loop materialised ~286k edges + per-
// edge GetNode round-trips to produce a top-10 list. The
// aggregator GROUPs server-side and ships the per-file counts only.
//
// scope, when non-nil, bounds the counted edges to those whose target
// node ID lies in the slice (session-workspace clamp). An empty (but
// non-nil) scope returns nil — never a whole-graph scan. A nil scope
// means "no clamp" and counts every imports edge.
//
// Optional capability — mostImportedFiles falls back to the
// AllEdges-driven loop when the backend doesn't implement it.
type FileImportAggregator interface {
	FileImportCounts(scope []string) []FileImportCountRow
}

// InDegreeForNodes is an optional capability backends MAY implement to
// return the per-target incoming-edge count for the given node id set
// in one backend round-trip. Unlike InEdgeCounter (which filters by
// edge kind across the WHOLE graph), this counter is scoped to a
// caller-supplied id set and counts EVERY incoming edge regardless of
// kind. handleGetSurprisingConnections needs both the hub heuristic
// and the per-edge anomaly walk, but the hub check only cares about
// nodes already inside the session-scoped working set; counting every
// edge across the table just to bucket by `To` materialises the entire
// edge column (~286k rows on a disk backend).
//
// Empty ids returns nil — never a whole-table scan. Targets with zero
// matching in-edges may be absent from the returned map (callers index
// with `m[id]` and treat zero as the default).
//
// Optional capability — handleGetSurprisingConnections falls back to
// the AllEdges-driven bucketing when the backend doesn't implement it.
type InDegreeForNodes interface {
	InDegreeForNodes(ids []string) map[string]int
}

// ReachableForwardByKinds is an optional capability backends MAY
// implement to compute the set of node IDs reachable from the seed
// frontier via outgoing edges whose Kind is in the supplied set, in
// one backend round-trip. The Go fallback runs a layer-by-layer BFS
// firing GetOutEdges per node — on a disk backend that's N+1 round-trips
// where N is the transitive frontier size; on a 100k-symbol repo with
// a few thousand test functions the BFS easily issues tens of
// thousands of edge fetches.
//
// reachableFromTests in handleGetUntestedSymbols is the primary
// caller: seeds are every function/method in a test file, kinds are
// {calls, references}, and the result is the closed set of symbols
// covered transitively by the test surface. The capability runs one
// variable-length match expression and ships the closure back as a
// single id list.
//
// Empty seeds returns nil; an empty kinds set returns the seed set
// unchanged (no edges to traverse). The returned map keys are the
// reachable node IDs (including the seeds); the bool value is always
// true — the shape mirrors the in-memory implementation's covered set
// so the caller's index expression stays identical.
//
// Optional capability — reachableFromTests falls back to the
// per-layer GetOutEdges BFS when the backend doesn't implement it.
type ReachableForwardByKinds interface {
	ReachableForwardByKinds(seeds []string, kinds []EdgeKind) map[string]bool
}

// ThrowerErrorRow is one tuple returned by ThrowerErrorSurfacer. ThrowerID
// is the symbol that originates the EdgeThrows edges; ErrorTargets is the
// distinct set of error-type node IDs the thrower reaches via EdgeThrows;
// ErrorMsgs is the distinct set of literal error-message strings the
// thrower emits (KindString nodes with meta.context = "error_msg", linked
// by EdgeEmits). Throws is the count of underlying EdgeThrows edges (one
// thrower may raise the same target multiple times from different sites).
// FilePath / Line are the row metadata the legacy handler propagated from
// the first edge / falling back to the thrower node — they ride here so
// the analyzer never has to issue a follow-up GetNode lookup.
type ThrowerErrorRow struct {
	ThrowerID    string
	FilePath     string
	Line         int
	Throws       int
	ErrorTargets []string
	ErrorMsgs    []string
}

// ThrowerErrorSurfacer is an optional capability backends MAY implement
// to evaluate the analyze(error_surface) rollup entirely inside the
// storage layer. The Go fallback walks EdgeThrows once for the per-
// thrower aggregation, then issues GetOutEdges per surviving thrower
// to attach the literal error-message strings. On a disk backend that's
// two scans of the edge table plus an N+1 loop for the per-thrower
// emit walk; the capability runs two server-side GROUP BYs and ships the
// pre-shaped rows back.
//
// pathPrefix narrows the EdgeThrows rows by their stored FilePath
// prefix; an empty prefix means "every thrower". Returned rows are
// already deduplicated per (thrower, error_target) and per (thrower,
// error_msg) — callers feed them directly into the analyzer's sort /
// truncate path without further bucketing.
//
// Optional capability — handleAnalyzeErrorSurface falls back to the
// AllEdges-driven loop when the backend doesn't implement it.
type ThrowerErrorSurfacer interface {
	ThrowerErrorSurface(pathPrefix string) []ThrowerErrorRow
}

// MemberMethodInfo is one row of the MemberMethodsByType projection.
// MethodID is the method node's id; Name is its name (the key the
// InferImplements method-set check compares against); FilePath /
// StartLine are the source coordinates InferOverrides stamps on the
// EdgeOverrides edge it emits; RepoPrefix lets consumers
// (ResolveGRPCStubCalls' pickGRPCHandler) tie-break on same-repo
// without a follow-up GetNode.
type MemberMethodInfo struct {
	MethodID   string
	Name       string
	FilePath   string
	StartLine  int
	RepoPrefix string
}

// MemberMethodsByType is an optional capability backends MAY implement
// to return the typeID → []MemberMethodInfo projection of every
// EdgeMemberOf edge whose source is a KindMethod node, in one backend
// round-trip. Replaces the InferImplements / InferOverrides Pass 1
// pattern of EdgesByKind(EdgeMemberOf) followed by per-edge
// GetNode(e.From) to filter on Kind == KindMethod and read the
// method's columns. On a disk backend that loop is N+1 round-trips:
// each method GetNode pulls ~10 string columns + the Meta blob just to
// read four scalar fields. The capability runs a single server-side
// join and ships only the four method columns the resolver
// actually consumes.
//
// Empty graph returns nil; types with no method members are absent
// from the result. The returned slice's elements are unique per
// MethodID — duplicated (typeID, methodID) pairs (a method
// member-of'd twice) collapse to one row.
//
// Optional capability — InferImplements / InferOverrides fall back to
// the per-edge GetNode walk when the backend doesn't implement it.
type MemberMethodsByType interface {
	MemberMethodsByType() map[string][]MemberMethodInfo
}

// StructuralParentEdgeRow is one tuple returned by StructuralParentEdges.
// FromID / ToID are the child / parent node IDs verbatim. FromKind /
// ToKind let the consumer apply the (Type | Interface) gate without a
// follow-up GetNode. Origin is the edge's resolution-tier label, which
// drives override-edge origin selection in InferOverrides.
type StructuralParentEdgeRow struct {
	FromID   string
	ToID     string
	FromKind NodeKind
	ToKind   NodeKind
	Origin   string
}

// StructuralParentEdges is an optional capability backends MAY
// implement to return every EdgeExtends / EdgeImplements / EdgeComposes
// edge whose endpoints are both KindType / KindInterface, projected as
// (FromID, ToID, FromKind, ToKind, Origin) in one backend round-trip.
// Replaces the InferOverrides Pass 2 pattern of g.AllEdges() followed
// by per-edge GetNode(e.From) + GetNode(e.To) to apply the kind gate.
// On a disk backend the AllEdges scan materialises every edge (~286k
// on the gortex workspace) plus issues two per-edge node lookups; the
// capability runs one server-side join with kind filters on both sides
// and ships only the surviving rows back (typically a small fraction of
// the edge table).
//
// Empty graph returns nil. Rows from extends/implements/composes edges
// whose endpoints aren't both type/interface are filtered server-side
// — the consumer never has to gate them again.
//
// Optional capability — InferOverrides falls back to the AllEdges +
// per-edge GetNode walk when the backend doesn't implement it.
type StructuralParentEdges interface {
	StructuralParentEdges() []StructuralParentEdgeRow
}

// CrossRepoCandidateRow is one tuple returned by CrossRepoCandidates.
// Edge is the underlying base-kind edge verbatim — the consumer
// rewrites Edge.CrossRepo on it and emits a parallel cross_repo_* edge.
// FromRepo / ToRepo are the (already-distinct) source and target
// RepoPrefix values projected from the endpoint nodes.
type CrossRepoCandidateRow struct {
	Edge     *Edge
	FromRepo string
	ToRepo   string
}

// CrossRepoCandidates is an optional capability backends MAY implement
// to return every edge whose Kind has a parallel cross_repo_* kind AND
// whose endpoints carry two different non-empty RepoPrefix values, in
// one backend round-trip. Replaces the DetectCrossRepoEdges pattern of
// g.AllEdges() + per-edge GetNode(e.From) + GetNode(e.To) to extract
// the RepoPrefix pair. On a disk backend the AllEdges scan ships every
// edge in the graph plus issues two GetNode lookups per surviving
// row; the capability filters by edge kind + the repo-prefix mismatch
// server-side and ships only the surviving rows (typically a small
// fraction of the edge table on a multi-repo workspace).
//
// baseKinds is the set of edge kinds for which a CrossRepoKindFor
// mapping exists — the caller passes the list and the implementation
// MUST use exactly that set in the IN-list, so a single-repo graph
// (or a graph whose nodes carry no RepoPrefix) returns no rows.
//
// Optional capability — DetectCrossRepoEdges falls back to the
// AllEdges + per-edge GetNode loop when the backend doesn't implement
// it.
type CrossRepoCandidates interface {
	CrossRepoCandidates(baseKinds []EdgeKind) []CrossRepoCandidateRow
}

// ScopedCrossRepoCandidates is the incremental counterpart to
// CrossRepoCandidates. Implementations must apply the scope inside the
// backend query: callers use it specifically to avoid re-reading every
// cross-repository relationship after one repository or file changes.
// Repository scope is incident (either endpoint belongs to one of the
// prefixes), which preserves inbound relationships when a target repository
// is newly indexed. File scope is likewise incident and also includes the
// edge's source location.
type ScopedCrossRepoCandidates interface {
	CrossRepoCandidatesForRepos(baseKinds []EdgeKind, repoPrefixes []string) []CrossRepoCandidateRow
	CrossRepoCandidatesForFiles(baseKinds []EdgeKind, filePaths []string) []CrossRepoCandidateRow
}

// MutationScopedCrossRepoCandidates is an optional role-aware refinement for a
// coordinated mutation receipt. Edge-source files need only edges stamped with
// those files; changed definitions need edges incident to nodes they contain.
// Keeping the roles separate avoids three broad query arms for every file.
type MutationScopedCrossRepoCandidates interface {
	CrossRepoCandidatesForMutation(baseKinds []EdgeKind, edgeSourceFiles, incidentNodeFiles []string) []CrossRepoCandidateRow
}

// CrossRepoFlagMarker persists the CrossRepo flag on existing base edges in
// one or more set-oriented backend statements. DetectCrossRepoEdges mutates
// the returned Edge pointers for the in-memory backend; disk backends return
// projected values and implement this capability so the promoted column stays
// durable without rewriting unrelated edge metadata.
type CrossRepoFlagMarker interface {
	MarkEdgesCrossRepo(edges []*Edge) (changed int)
}

// Direction selects which way a BFSCapable.BFS walk follows edges.
type Direction int

const (
	// DirectionForward follows edges from source to target (from_id ->
	// to_id): the callee / dependency / out-edge direction.
	DirectionForward Direction = iota
	// DirectionBackward follows edges from target to source (to_id ->
	// from_id): the caller / dependent / in-edge direction.
	DirectionBackward
)

// String renders the direction for logs / debugging.
func (d Direction) String() string {
	if d == DirectionBackward {
		return "backward"
	}
	return "forward"
}

// BFSHop is one node reached by a BFSCapable.BFS walk. NodeID is the
// reached node; Depth is its minimum hop-distance from the nearest seed
// (seeds are depth 0). ParentID + EdgeKind name the edge that first
// reached the node at that minimum depth — the discovery edge — so a
// caller can rebuild the spanning tree / call chain without a second
// pass. Seeds carry ParentID == "" and EdgeKind == "".
type BFSHop struct {
	NodeID   string
	Depth    int
	ParentID string
	EdgeKind EdgeKind
}

// BFSCapable is an optional capability a backend MAY implement to run a
// whole bounded breadth-first traversal in one round-trip instead of the
// engine's layer-by-layer GetOutEdges / GetInEdges walk (one store call
// per depth). The SQLite backend lowers it to a single recursive CTE; the
// in-memory graph keeps the reference Go walk. Both MUST agree on the
// reachable hop-set for the same arguments — the in-memory walk is the
// correctness oracle the disk implementation is shadow-tested against.
//
// Semantics:
//   - seeds enter the result at depth 0 (ParentID / EdgeKind empty). Empty
//     seeds returns nil; duplicate seeds collapse.
//   - dir picks the edge direction — forward follows from_id -> to_id,
//     backward follows to_id -> from_id.
//   - kinds gates which edge kinds the walk follows. Empty kinds returns
//     the seed hops only (no edge is followed).
//   - only node-backed targets are followed and returned; an edge to a
//     target with no node row (an unresolved / external stub) is not a hop.
//     This mirrors the engine's reachable-set semantics, where such targets
//     are dropped rather than expanded.
//   - each node appears once, at its minimum depth; among the discovery
//     edges at that depth the (ParentID, EdgeKind)-smallest is chosen, so
//     the result is deterministic. A cycle terminates because depth is
//     bounded by maxDepth, not because of any visited bookkeeping.
//   - maxDepth bounds the hop distance: a node at depth d is expanded only
//     while d < maxDepth, so the deepest hop is maxDepth. maxDepth <= 0
//     returns the seed hops only.
//   - the result is ordered by (Depth, NodeID); limit > 0 caps the number
//     of hops after that ordering (limit <= 0 means no cap).
//
// Optional capability — the engine's GetCallers / GetCallChain /
// GetDependents / GetDependencies fall back to the in-memory layer walk
// when the backend does not implement it, or when the walk needs features
// the flat hop-set cannot express (workspace scope, test exclusion,
// dispatch expansion, or a bidirectional cluster walk).
type BFSCapable interface {
	BFS(seeds []string, dir Direction, kinds []EdgeKind, maxDepth int, limit int) ([]BFSHop, error)
}

// ExtractCandidateRow is one tuple returned by ExtractCandidatesScanner.
// Caller / FanOut counts are distinct-by-endpoint (one caller counted
// once per (From, kind) pair, one callee counted once per (To, kind)
// pair) restricted to the call-like edge kinds the consumer cares
// about. LineCount is EndLine - StartLine + 1; rows whose StartLine or
// EndLine is zero are filtered server-side.
type ExtractCandidateRow struct {
	NodeID      string
	Name        string
	FilePath    string
	StartLine   int
	EndLine     int
	LineCount   int
	CallerCount int
	FanOut      int
}

// ExtractCandidatesScanner is an optional capability backends MAY
// implement to compute the get_extraction_candidates ranking in two
// server-side round-trips (per-node caller-count and fan-out aggregation
// joined to the node table). Replaces the AllNodes() scan + per-node
// GetInEdges / GetOutEdges loop the handler used previously — on the
// gortex workspace that was ~30k node × 2 trips per call, where
// each trip materialised the full edge bucket just to count
// distinct endpoints. The capability instead runs the count
// (DISTINCT-by-endpoint) inside the engine and ships only the rows
// that satisfy the three threshold gates.
//
// Empty kinds yields nothing — the handler always passes a non-empty
// set (EdgeCalls + EdgeCrossRepoCalls). pathPrefix narrows the scan to
// nodes under that file-path prefix; empty matches every path. The
// returned rows mirror the result of the Go-side loop verbatim:
// thresholds applied, line_count = EndLine - StartLine + 1.
//
// Optional capability — handleGetExtractionCandidates falls back to
// the AllNodes scan when the backend doesn't implement it.
type ExtractCandidatesScanner interface {
	ExtractCandidates(
		kinds []EdgeKind,
		minLines, minCallers, minFanOut int,
		pathPrefix string,
	) []ExtractCandidateRow
}

// FileSymbolNameRow is one tuple returned by FileSymbolNamesByPaths.
// FilePath echoes the input slot; Name is one symbol name observed in
// the file (function / method / type / interface kinds only, matching
// symbolNamesInFile's Go-side filter). One file may produce many rows.
type FileSymbolNameRow struct {
	FilePath string
	Name     string
}

// FileSymbolNamesByPaths is an optional capability backends MAY
// implement to fetch the sorted distinct (file → function/method/type
// names) projection for a slice of file paths in one backend round-
// trip. Replaces the per-file GetFileNodes loop find_co_changing_symbols
// runs after a positive cochange match: 20 result rows × one
// per-file query each on a disk backend. The capability runs a single
// query filtering by file path and kind with an IN-list, and ships
// one row per (file, name).
//
// Empty paths returns nil — never a whole-table scan. Rows for paths
// with no qualifying symbols are absent from the result; callers
// always index by file path and treat missing keys as "no names".
//
// Optional capability — symbolNamesInFile and its callers fall back to
// the per-file GetFileNodes loop when the backend doesn't implement
// it.
type FileSymbolNamesByPaths interface {
	FileSymbolNamesByPaths(paths []string, kinds []NodeKind) []FileSymbolNameRow
}

// ClassHierarchyRow is one tuple returned by ClassHierarchyTraverser.
// Path carries the node IDs visited from the seed (exclusive of the
// seed) out to the terminal node, in BFS order. EdgeKinds carries the
// per-hop edge kind so the caller can reconstruct the *Edge values.
// For a single hop Path has one element and EdgeKinds has one element;
// for a depth-N walk both slices have length N.
type ClassHierarchyRow struct {
	Path      []string
	EdgeKinds []EdgeKind
}

// ClassHierarchyTraverser is an optional capability backends MAY
// implement to compute the inheritance subgraph rooted at a seed in
// one (or two — up + down) variable-length traversals, server-
// side. Replaces the BFS in query.ClassHierarchy: each frontier node
// fired GetNode + GetInEdges or GetOutEdges per visit on a disk
// backend, so a depth-5 walk over an interface with a wide implementer
// set burned hundreds of round-trips just to discover ~50 edges.
//
// kinds is the edge-kind set the walk consumes (EdgeExtends +
// EdgeImplements + EdgeComposes + EdgeOverrides). depth caps the hop
// budget. direction:
//   - "up"   — follow outgoing edges from each frontier node.
//   - "down" — follow incoming edges into each frontier node.
//
// Empty kinds / depth <= 0 / unknown seed returns nil. The returned
// rows are deduplicated by (Path[-1], last EdgeKind) — the consumer
// reconstructs the visited node set and the edge list from them.
//
// Optional capability — query.ClassHierarchy falls back to the BFS
// when the backend doesn't implement it.
type ClassHierarchyTraverser interface {
	ClassHierarchyTraverse(
		seedID string,
		direction string,
		kinds []EdgeKind,
		depth int,
	) []ClassHierarchyRow
}

// FileEditingContext is an optional capability backends MAY
// implement to return the get_editing_context payload (defines +
// imports + 1-hop callers + 1-hop callees, all for one file) in a
// small fixed number of server-side round-trips. Replaces the handler's
// per-symbol GetCallers / GetCallChain loop — for a file with 30
// functions that fired 60 query-engine entry points on a disk backend.
//
// kinds is the set of node kinds the caller treats as call-targets
// (KindFunction + KindMethod). The capability returns FileNode (the
// file row), Defines (every non-file node anchored to the path,
// signature carried through Meta), Imports (the EdgeImports out-edges
// of the file node), CalledBy (one-hop callers of any defines node,
// filtered to symbols outside the file), and Calls (one-hop callees of
// any defines node, filtered to symbols outside the file). All five
// projections are scoped to the input file in one round-trip each.
//
// Optional capability — handleGetEditingContext falls back to the
// per-symbol loop when the backend doesn't implement it.
type FileEditingContextResult struct {
	FileNode *Node
	Defines  []*Node
	Imports  []*Edge
	CalledBy []*Node
	Calls    []*Node
}

type FileEditingContext interface {
	FileEditingContext(filePath string, kinds []NodeKind) *FileEditingContextResult
}

// FileSubGraphReader is an optional capability backends MAY implement
// to return the full file neighbourhood — the file node, every node
// defined in or contained by it, and every adjacent edge — in a
// single backend round-trip.
//
// On the in-memory backend the per-id GetOutEdges / GetInEdges loop
// is already O(1) per node, so the query.Engine.GetFileSymbols
// fallback wraps it. On a disk backend the same loop is
// O(file_symbols) round-trips — ~547 symbols on a real file fanned
// out into ~5 000 round-trips just to dedup edges in Go. The
// capability lets the backend express the walk as a single server-side
// query over the node and edge indexes.
//
// Returned slices are deduplicated by the implementation. Missing
// file returns (nil, nil); empty file (file node only, no symbols)
// returns ([file], nil). Callers that need the symbols-only view
// strip KindFile + KindImport on top (see
// internal/mcp/tools_core.go::stripFileAndImportNodes).
//
// Optional capability — query.Engine.GetFileSymbols falls back to
// GetFileNodes + GetOut/InEdgesByNodeIDs when the backend doesn't
// implement it.
type FileSubGraphReader interface {
	GetFileSubGraph(filePath string) (nodes []*Node, edges []*Edge)
}

// FrontierHop is one (edge, neighbour) pair from a FrontierExpander: an
// edge adjacent to a queried source node plus the node at its far end,
// with the neighbour's columns populated and Meta left nil (traversal
// callers don't read it). It lets a BFS record the edge and
// scope-check / materialise the neighbour without a GetNode per edge.
type FrontierHop struct {
	Edge     *Edge
	Neighbor *Node
}

// FrontierExpander is an optional backend capability: given a set of
// source node IDs it returns, in a single round-trip, their adjacent
// edges of the requested kinds plus the neighbour nodes — the
// node-edge-node projection a BFS frontier needs. forward=true follows
// outgoing edges (neighbour = edge target); forward=false follows
// incoming (neighbour = edge source). kinds must be non-empty (the
// directed-traversal contract). limit derives a deterministic per-call
// row cap so a hub node's fan-out can no longer be dragged across the
// boundary in full.
//
// query.Engine.bfs uses it when the reader implements it (the disk
// store) and falls back to per-node GetOutEdges/GetInEdges + GetNode
// otherwise — the in-memory graph needs no batching (its reads are O(1)).
type FrontierExpander interface {
	ExpandFrontier(ids []string, forward bool, kinds []EdgeKind, limit int) []FrontierHop
}

// FileSubGraphCountReader is the count-only sibling of
// FileSubGraphReader: returns the file's nodes plus the number of
// distinct edges adjacent to any of them, without materialising the
// edges themselves.
//
// The disk-backend headline cost for get_file_summary on a 500-symbol
// file was the ~4 000-row crossing to ship every adjacent edge back to
// Go. The gcx and compact output paths only emit a total_edges scalar
// in their meta headers — never per-edge rows — so handleGetFileSummary
// routes gcx through this method and skips the row materialisation
// entirely. The json output path keeps the full GetFileSubGraph call
// because it serialises every edge in the body, and the compact path
// keeps it because it summarises edges per confidence label.
//
// On the in-memory backend the per-node edge bucket lookups are
// already O(1), so its implementation just counts via the same path
// GetFileSubGraph walks; the win is on disk backends.
//
// Optional capability — query.Engine.GetFileSymbolsCounts falls back
// to len(GetFileSubGraph().edges) when the backend doesn't implement
// it.
type FileSubGraphCountReader interface {
	GetFileSubGraphCounts(filePath string) (nodes []*Node, edgeCount int)
}

// NodeDegreeByKinds is an optional capability backends MAY implement
// to return per-node total in/out edge counts for every node whose
// kind is in the supplied set, server-side. Replaces the
// get_knowledge_gaps pattern of "give me all functions, then ask for
// their in/out degree" — on a disk backend that fed an IN-list of ~30k
// node IDs to the NodeDegreeCounts query, which has to compare every
// node against the list. The capability instead matches kinds at the
// source and groups by node — one query per direction with a kind
// predicate the planner can index.
//
// pathPrefix narrows the scan to nodes under that file-path prefix;
// empty matches every path. Empty kinds returns nil (never a whole-
// graph scan).
//
// The returned rows mirror NodeDegreeRow's shape but UsageInCount is
// always 0 — knowledge_gaps does not need the usage subset, only the
// total degree. Adding the usage filter back would re-tie the
// capability to ClassifyZeroEdge's notion of "alive" without buying
// any other call site.
//
// Optional capability — handleGetKnowledgeGaps falls back to the
// NodeDegreeCounts IN-list when the backend doesn't implement it.
type NodeDegreeByKinds interface {
	NodeDegreeByKinds(kinds []NodeKind, pathPrefix string) []NodeDegreeRow
}

// RepoMemoryEstimateScanner recomputes the per-repo estimates from the stored
// nodes and edges instead of reading maintained counters. It is the audit path
// for backends whose counters are written once per index pass and could drift;
// backends that maintain counters on every mutation have nothing to reconcile
// against and do not implement it.
//
// The scan is proportional to the whole corpus — tens of seconds on a large
// store — so it belongs behind an explicit user request, never on a polling
// path. A scan that cannot finish inside the caller's context returns the
// context error rather than a short map: a partial count is a wrong answer,
// not a stale one.
type RepoMemoryEstimateScanner interface {
	ScanRepoMemoryEstimates(ctx context.Context) (map[string]RepoMemoryEstimate, error)
}
