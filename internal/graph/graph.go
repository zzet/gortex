package graph

import (
	"iter"
	"math/bits"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graphpath"
)

// GraphStats holds summary counts of the graph contents.
type GraphStats struct {
	TotalNodes int            `json:"total_nodes"`
	TotalEdges int            `json:"total_edges"`
	ByKind     map[string]int `json:"by_kind"`
	ByLanguage map[string]int `json:"by_language"`
}

// The graph's write fan-out is sharded: each shard owns a disjoint
// slice of node IDs (by FNV hash) and its own RWMutex, so parallel
// indexers writing distinct nodes don't contend for a single lock. The
// shard count is chosen once per Graph at construction (see
// defaultShardCount) rather than fixed at compile time, so many-core
// machines get more fan-out while small machines keep the historical
// count. It is always a power of two: shardIdx masks an ID's hash with
// count-1, which equals the modulo only for powers of two.
//
// Trade-off: more shards cut write contention but make cross-shard
// operations (AddEdge across shards, plus exhaustive reads that walk
// every shard) take more locks. To avoid deadlock we always acquire
// shards in ascending index order.
const (
	// shardCountEnv pins the shard fan-out for a new graph, overriding
	// the CPU-derived default. Useful for benchmarks and tests. The
	// value is coerced to a power of two within [minShards, maxShards].
	shardCountEnv = "GORTEX_GRAPH_SHARDS"

	// minShards / maxShards bound every shard count — derived or
	// explicitly overridden — to a sane, power-of-two range. minShards
	// must stay >= 1 so the shardMask (count-1) is a valid mask.
	minShards = 1
	maxShards = 512

	// derivedShardFloor / derivedShardCeiling bound the CPU-derived
	// count. The floor preserves the historical fan-out of 16 on typical
	// (<= 16 logical CPU) machines, so sharded results stay
	// byte-identical there; the ceiling caps per-graph shard overhead on
	// many-core boxes.
	derivedShardFloor   = 16
	derivedShardCeiling = 256
)

// edgeKey is the logical identity of an edge — two edges with the same
// key are considered the same edge and dedup to one entry in the
// adjacency lists. Line is part of the key because a caller can hold
// the same target reference at two different call sites and both are
// real call-graph edges (see `foo(); foo();` in the same function).
//
// Both From and To are in the key even though the adjacency lists are
// keyed by one endpoint each — the other endpoint varies within an
// inEdges[to] slice (many From → same To) and within an outEdges[from]
// slice the inverse varies. Without both, property-test graphs with
// several callers to the same callee and no FilePath/Line distinction
// would dedup down to one entry and lose cross-caller traversal.
type edgeKey struct {
	From     string
	To       string
	Kind     EdgeKind
	FilePath string
	Line     int
}

func keyOf(e *Edge) edgeKey {
	return edgeKey{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line}
}

// edgeHash is the stored form of an edge's logical identity. A full
// edgeKey carries four strings plus an int — 72 bytes as a map key or
// sidecar element. Collapsing it to a 128-bit hash shrinks the
// outEdgeIdx / inEdgeIdx maps and the outEdgeKeys / inEdgeKeys sidecars
// by ~4.5×, the single largest line item in cold-warmup resident
// memory. 128 bits keeps a collision between any two distinct edges in
// a graph of realistic size out of reach (~1e-25 at 10M edges), so the
// indexes stay unique-key maps — the dedup and swap-with-last logic is
// byte-for-byte the same as the edgeKey version, only the key type
// changes.
type edgeHash struct{ lo, hi uint64 }

const fnvOffset64 = 1469598103934665603
const fnvPrime64 = 1099511628211

// hashEdgeKey computes the 128-bit identity hash of an edge key. Each
// 64-bit half is an independent FNV-1a pass: distinct seeds plus a
// reversed field order decorrelate the halves so the pair behaves as a
// 128-bit value rather than two views of the same 64-bit hash.
func hashEdgeKey(k edgeKey) edgeHash {
	return edgeHash{
		lo: fnv1aEdge(fnvOffset64, k, false),
		hi: fnv1aEdge(0x9e3779b97f4a7c15, k, true),
	}
}

// fnv1aEdge folds every edgeKey field into one FNV-1a 64-bit hash. A
// 0xff separator after each string keeps "ab"+"c" distinct from
// "a"+"bc"; reversed==true walks the string fields back-to-front.
func fnv1aEdge(seed uint64, k edgeKey, reversed bool) uint64 {
	h := seed
	mix := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= fnvPrime64
		}
		h ^= 0xff
		h *= fnvPrime64
	}
	if reversed {
		mix(k.FilePath)
		mix(string(k.Kind))
		mix(k.To)
		mix(k.From)
	} else {
		mix(k.From)
		mix(k.To)
		mix(string(k.Kind))
		mix(k.FilePath)
	}
	h ^= uint64(k.Line)
	h *= fnvPrime64
	return h
}

// hashEdgeIdentity computes the provenance-bearing identity hash of an
// edge: the logical key folded together with Origin. It reuses the
// edgeKey FNV machinery — each 64-bit half is hashEdgeKey's
// corresponding half with the Origin string mixed in as a sixth field
// (same 0xff-separated FNV-1a fold, same reversed-order decorrelation).
//
// This is deliberately distinct from hashEdgeKey: the dedup index
// (outEdgeIdx / inEdgeIdx) stays keyed on the Origin-free logical key
// so an Origin upgrade replaces the slot in place rather than creating
// a parallel edge. The identity hash is the separate value Edge.
// IdentityHash exposes so a provenance change is observable as a
// distinct identity.
func hashEdgeIdentity(k edgeKey, origin string) edgeHash {
	return edgeHash{
		lo: mixOriginInto(fnv1aEdge(fnvOffset64, k, false), origin),
		hi: mixOriginInto(fnv1aEdge(0x9e3779b97f4a7c15, k, true), origin),
	}
}

// mixOriginInto folds the Origin string into an already-computed
// fnv1aEdge hash with the same 0xff-separated FNV-1a step the key
// fields use, so Origin is a first-class sixth field of the identity
// rather than a bolt-on.
func mixOriginInto(h uint64, origin string) uint64 {
	for i := 0; i < len(origin); i++ {
		h ^= uint64(origin[i])
		h *= fnvPrime64
	}
	h ^= 0xff
	h *= fnvPrime64
	return h
}

// shard is a fragment of the graph's data. Each shard holds the node
// metadata for the subset of IDs that hash to it, plus the outgoing
// edges whose source ID is in this shard and the incoming edges whose
// target ID is in this shard. Secondary indexes (byName, byFile, etc.)
// inside a shard only contain nodes owned by that shard; aggregate
// queries iterate every shard and concatenate.
//
// Each slice-valued secondary index has a paired "Idx" sidecar that
// maps an entry's identity to its position in the slice. Two invariants
// the sidecars enforce:
//
//  1. Idempotent writes. AddNode/AddEdge consult the sidecar; duplicate
//     inserts replace the pointer in place instead of appending.
//     Without this, restart paths that load a snapshot and then re-run
//     IndexCtx (which doesn't evict first) silently double every edge
//     and every secondary-index entry. Fixes bug B1 at the write layer,
//     regardless of which call site forgets to evict first.
//  2. O(1) removal via swap-with-last. Removing a node from a
//     byRepo slice of 50k entries with filterEdge-style scanning was
//     O(N); now it's O(1) — flip the last element into the removed
//     slot and shrink the slice by one. Iteration order changes after a
//     removal, but callers that require stable ordering (snapshot
//     encoding) sort before emitting anyway.
type shard struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	outEdges map[string][]*Edge
	inEdges  map[string][]*Edge
	byFile   map[string][]*Node
	byName   map[string][]*Node
	byQual   map[string]*Node
	byRepo   map[string][]*Node // repoPrefix → nodes owned by this shard

	// Sidecar position indexes — see comment on shard. Reads are
	// unchanged (callers still iterate the slices); only writes
	// consult these maps.
	byFileIdx  map[string]map[string]int   // filePath   → id   → position
	byNameIdx  map[string]map[string]int   // name       → id   → position
	byRepoIdx  map[string]map[string]int   // repoPrefix → id   → position
	outEdgeIdx map[string]map[edgeHash]int // fromID     → hash → position
	inEdgeIdx  map[string]map[edgeHash]int // toID       → hash → position

	// Reverse-index slices that remember each entry's insertion-time
	// edgeHash, parallel to outEdges / inEdges. Required because keyOf
	// is computed from live Edge fields (To, From, ...) — and the
	// resolver mutates Edge.To when retargeting an unresolved edge.
	// During swap-with-last in removeEdgeFromBucket, computing
	// keyOf(swapped) on a swapped element whose To was just mutated
	// produced a different key than the one it was originally indexed
	// under, leaving a stale sidecar position pointing past the slice
	// (panic: "index out of range [42] with length 41" in
	// addEdgeToBucket on the next insert that collided with the stale
	// key). Storing the original hash here makes the swap update
	// independent of live Edge state.
	outEdgeKeys map[string][]edgeHash // fromID → position → hash
	inEdgeKeys  map[string][]edgeHash // toID   → position → hash

	// Running per-repo memory totals maintained under the shard's
	// existing write lock. Reading them out of RepoMemoryEstimate is
	// O(shard count) instead of the O(repo nodes + total edges) walk
	// the function used to do — and AllRepoMemoryEstimates collapses
	// R repo-by-repo queries into one O(R · S) sum. Edges are charged
	// to the source node's RepoPrefix; the shard owning the source
	// edge bucket owns the counter, so accounting matches what the
	// old AllEdges walk attributed.
	repoNodeBytes map[string]uint64
	repoNodeCount map[string]int
	repoEdgeBytes map[string]uint64
	repoEdgeCount map[string]int
}

func newShard() *shard {
	return &shard{
		nodes:         make(map[string]*Node),
		outEdges:      make(map[string][]*Edge),
		inEdges:       make(map[string][]*Edge),
		byFile:        make(map[string][]*Node),
		byName:        make(map[string][]*Node),
		byQual:        make(map[string]*Node),
		byRepo:        make(map[string][]*Node),
		byFileIdx:     make(map[string]map[string]int),
		byNameIdx:     make(map[string]map[string]int),
		byRepoIdx:     make(map[string]map[string]int),
		outEdgeIdx:    make(map[string]map[edgeHash]int),
		inEdgeIdx:     make(map[string]map[edgeHash]int),
		outEdgeKeys:   make(map[string][]edgeHash),
		inEdgeKeys:    make(map[string][]edgeHash),
		repoNodeBytes: make(map[string]uint64),
		repoNodeCount: make(map[string]int),
		repoEdgeBytes: make(map[string]uint64),
		repoEdgeCount: make(map[string]int),
	}
}

// repoNodeAdd registers a node under its RepoPrefix bucket. Caller
// must hold s.mu.Lock. No-op for nodes without a prefix (single-repo
// mode and synthetic nodes that intentionally skip byRepo accounting).
func (s *shard) repoNodeAdd(n *Node) {
	if n == nil || n.RepoPrefix == "" {
		return
	}
	s.repoNodeBytes[n.RepoPrefix] += nodeBytes(n)
	s.repoNodeCount[n.RepoPrefix]++
}

// repoNodeRemove undoes repoNodeAdd. Clamps to zero on underflow so a
// stale counter cannot wrap a uint64.
func (s *shard) repoNodeRemove(n *Node) {
	if n == nil || n.RepoPrefix == "" {
		return
	}
	b := nodeBytes(n)
	if cur := s.repoNodeBytes[n.RepoPrefix]; cur >= b {
		s.repoNodeBytes[n.RepoPrefix] = cur - b
	} else {
		s.repoNodeBytes[n.RepoPrefix] = 0
	}
	if s.repoNodeCount[n.RepoPrefix] > 0 {
		s.repoNodeCount[n.RepoPrefix]--
	}
	if s.repoNodeBytes[n.RepoPrefix] == 0 && s.repoNodeCount[n.RepoPrefix] == 0 {
		delete(s.repoNodeBytes, n.RepoPrefix)
		delete(s.repoNodeCount, n.RepoPrefix)
	}
}

// repoEdgeAdd attributes an edge to its source repo. repoPrefix is the
// source node's RepoPrefix as resolved at the time the edge is
// inserted; storing it here means a later swap of Edge.To (ReindexEdge)
// doesn't have to walk the source-node lookup again.
func (s *shard) repoEdgeAdd(repoPrefix string, e *Edge) {
	if repoPrefix == "" || e == nil {
		return
	}
	s.repoEdgeBytes[repoPrefix] += edgeBytes(e)
	s.repoEdgeCount[repoPrefix]++
}

// repoEdgeRemove undoes repoEdgeAdd. Same clamp-to-zero discipline as
// repoNodeRemove.
func (s *shard) repoEdgeRemove(repoPrefix string, e *Edge) {
	if repoPrefix == "" || e == nil {
		return
	}
	b := edgeBytes(e)
	if cur := s.repoEdgeBytes[repoPrefix]; cur >= b {
		s.repoEdgeBytes[repoPrefix] = cur - b
	} else {
		s.repoEdgeBytes[repoPrefix] = 0
	}
	if s.repoEdgeCount[repoPrefix] > 0 {
		s.repoEdgeCount[repoPrefix]--
	}
	if s.repoEdgeBytes[repoPrefix] == 0 && s.repoEdgeCount[repoPrefix] == 0 {
		delete(s.repoEdgeBytes, repoPrefix)
		delete(s.repoEdgeCount, repoPrefix)
	}
}

// addNodeToBucket appends n to bucket[key] unless an entry with that id
// is already present, in which case the existing slot is overwritten
// with the new pointer. Returns the position of the entry.
func addNodeToBucket(bucket map[string][]*Node, idx map[string]map[string]int, key, id string, n *Node) {
	if inner, ok := idx[key]; ok {
		if pos, exists := inner[id]; exists {
			bucket[key][pos] = n
			return
		}
	}
	pos := len(bucket[key])
	bucket[key] = append(bucket[key], n)
	inner, ok := idx[key]
	if !ok {
		inner = make(map[string]int)
		idx[key] = inner
	}
	inner[id] = pos
}

// removeNodeFromBucket swap-removes the entry with id from bucket[key],
// updating the sidecar position of the swapped-in element. No-op when
// the entry is absent. Cleans up the bucket + sidecar when the last
// entry for key leaves.
func removeNodeFromBucket(bucket map[string][]*Node, idx map[string]map[string]int, key, id string) {
	inner, ok := idx[key]
	if !ok {
		return
	}
	pos, exists := inner[id]
	if !exists {
		return
	}
	slice := bucket[key]
	last := len(slice) - 1
	if pos != last {
		swapped := slice[last]
		slice[pos] = swapped
		inner[swapped.ID] = pos
	}
	slice = slice[:last]
	delete(inner, id)
	if len(inner) == 0 {
		delete(idx, key)
		delete(bucket, key)
	} else {
		bucket[key] = slice
	}
}

// addEdgeToBucket appends e to bucket[key] unless an entry with the
// same logical identity (edgeKey) is already there, in which case the
// existing slot is overwritten. Reports whether this was a new insert
// and, on an in-place replace, whether the replacement carried a
// different Origin than the edge it displaced.
//
// keys is the parallel slice that remembers each slot's insertion-time
// edgeKey so removeEdgeFromBucket can update sidecars without
// recomputing keyOf on a possibly-mutated swapped Edge.
//
// originChanged surfaces the resolver path where an edge is re-added
// (via AddEdge) with upgraded provenance rather than mutated in place:
// the logical key still matches, so the slot is replaced, but the
// edge's identity has changed. AddEdge funnels this into the
// graph-level identity-revision counter so that re-add path is as
// observable as an explicit SetEdgeProvenance call.
func addEdgeToBucket(bucket map[string][]*Edge, keys map[string][]edgeHash, idx map[string]map[edgeHash]int, key string, e *Edge) (inserted, originChanged bool) {
	h := hashEdgeKey(keyOf(e))
	if inner, ok := idx[key]; ok {
		if pos, exists := inner[h]; exists {
			old := bucket[key][pos]
			originChanged = old != nil && old != e && old.Origin != e.Origin
			bucket[key][pos] = e
			// keys[key][pos] already equals h — same logical identity.
			return false, originChanged
		}
	}
	pos := len(bucket[key])
	bucket[key] = append(bucket[key], e)
	keys[key] = append(keys[key], h)
	inner, ok := idx[key]
	if !ok {
		inner = make(map[edgeHash]int)
		idx[key] = inner
	}
	inner[h] = pos
	return true, false
}

// removeEdgeFromBucket removes the entry with key k from bucket[key]
// using swap-with-last, maintaining the sidecar. No-op when absent.
func removeEdgeFromBucket(bucket map[string][]*Edge, keys map[string][]edgeHash, idx map[string]map[edgeHash]int, key string, k edgeHash) bool {
	inner, ok := idx[key]
	if !ok {
		return false
	}
	pos, exists := inner[k]
	if !exists {
		return false
	}
	slice := bucket[key]
	keySlice := keys[key]
	last := len(slice) - 1
	if pos != last {
		slice[pos] = slice[last]
		// Use the swapped slot's STORED insertion-time hash, not
		// hashEdgeKey(keyOf(swapped)). The Edge's To may have been
		// mutated by the resolver between insertion and now, in which
		// case keyOf would yield a different hash than the sidecar
		// entry that actually points at this slot — leaking a stale
		// "last" position that later panics in addEdgeToBucket.
		swappedKey := keySlice[last]
		keySlice[pos] = swappedKey
		inner[swappedKey] = pos
	}
	slice = slice[:last]
	keySlice = keySlice[:last]
	delete(inner, k)
	if len(inner) == 0 {
		delete(idx, key)
		delete(bucket, key)
		delete(keys, key)
	} else {
		bucket[key] = slice
		keys[key] = keySlice
	}
	return true
}

// Graph is a thread-safe in-memory knowledge graph. Internally sharded
// by node-ID hash so writers touching disjoint IDs run in parallel.
//
// resolveMu is a graph-wide lock shared by every resolver.Resolver
// constructed against this Graph. Per-shard locks protect individual
// node/edge writes, but resolution phases (ResolveAll, ResolveFile)
// iterate every shard and mutate edge targets in place — two
// resolvers racing on the same edge produced the data race that
// MultiIndexer.indexMultiRepo triggered when its per-repo goroutines
// each created their own Resolver. Routing every resolver through
// this single mutex serialises those phases without blocking
// ordinary shard-scoped reads and writes.
type Graph struct {
	// shards holds the lock-sharded node/edge maps. Its length is
	// shardCount (a power of two); shardMask is shardCount-1, precomputed
	// so the hot-path shardIdx masks with a single AND instead of a
	// modulo.
	shards     []*shard
	shardCount int
	shardMask  uint32
	resolveMu  sync.Mutex
	// edgeIdentityRevisions counts how many times an in-graph edge's
	// provenance-bearing identity changed — i.e. its Origin was
	// upgraded or reverted while its logical (From,To,Kind,FilePath,
	// Line) key stayed fixed. Each such change is conceptually a
	// retirement of the old identity and creation of a new one. Both
	// sanctioned mutation paths feed this counter: SetEdgeProvenance
	// (an explicit in-place change) and addEdgeToBucket's in-place
	// replace branch (a re-add of the same logical edge carrying an
	// upgraded Origin). The count is the tamper-evidence surface:
	// provenance cannot churn without it moving.
	edgeIdentityRevisions atomic.Int64
	// builtinSeen records ::builtin:: sentinel targets already materialised
	// as KindBuiltin stub nodes (see BuiltinStubNodes), so re-indexing the
	// same files doesn't re-upsert identical stubs on every batch.
	builtinSeen sync.Map
	// edgeMutGen bumps whenever the AllEdges output would change —
	// new edge inserted, existing edge removed, or an edge's
	// canonical key changed via ReindexEdge. Origin-only updates
	// (counted by edgeIdentityRevisions) do not bump this because the
	// slice content is unaffected. AllEdges uses the counter as a
	// cache-validity tag so repeated post-resolve analysis walks
	// share one materialised slice instead of each rebuilding the
	// 4 M-edge snapshot.
	edgeMutGen atomic.Uint64
	// nodeMutGen advances once per accepted node mutation batch (including
	// same-ID replacements and scoped evictions). Resolver instances combine it
	// with edgeMutGen to detect cache-invalidating watcher work performed by a
	// different Resolver while the shared resolve mutex was yielded.
	nodeMutGen atomic.Uint64

	allEdgesCacheMu  sync.Mutex
	allEdgesCache    []*Edge
	allEdgesCacheGen uint64

	// cloneShingles is the in-memory implementation of the
	// CloneShingle* capability: per-symbol MinHash shingle sets keyed by
	// node id, alongside the repo prefix that owns each row so per-repo
	// reseeds isolate correctly. Guarded by cloneShinglesMu. Slices are
	// deep-copied on set and on read so callers can't mutate the stored
	// state. The on-disk backend persists the same shape; the in-memory
	// store keeps it live so the conformance suite exercises both.
	cloneShinglesMu sync.Mutex
	cloneShingles   map[string]cloneShingleEntry

	// churnEnrich is the in-memory churn-enrichment sidecar (change A).
	churnEnrichMu sync.Mutex
	churnEnrich   map[string]ChurnEnrichment

	// coverageEnrich is the in-memory coverage-enrichment sidecar.
	coverageEnrichMu sync.Mutex
	coverageEnrich   map[string]CoverageEnrichment

	// releaseEnrich is the in-memory release-enrichment sidecar.
	releaseEnrichMu sync.Mutex
	releaseEnrich   map[string]ReleaseEnrichment

	// blameEnrich is the in-memory blame-enrichment sidecar.
	blameEnrichMu sync.Mutex
	blameEnrich   map[string]BlameEnrichment

	// constValues is the in-memory implementation of the ConstantValue*
	// capability: a KindConstant node's literal value keyed by node id,
	// alongside its owning file (for file-scoped eviction) and repo
	// prefix. Guarded by constValuesMu.
	constValuesMu sync.Mutex
	constValues   map[string]constValueEntry

	// fileMetas holds per-file metadata rows keyed by repoPrefix -> filePath
	// (the files sidecar feeding index_health). Guarded by fileMetasMu.
	fileMetasMu sync.Mutex
	fileMetas   map[string]map[string]FileMetaRow

	// mutationReceipts backs the optional, in-memory-only mutation receipt
	// capability used to bound post-enrichment resolution. It is intentionally
	// absent from disk stores until they can provide the same completeness
	// guarantee.
	mutationReceipts mutationReceiptState

	// structuralIntegrity is the store-scoped since-open recorder. Shadows may
	// forward through structuralIntegritySink so rejected attempts survive the
	// throwaway graph; the explicit repo and fixed path prevent attribution
	// from guessing ownership from unprefixed node IDs.
	structuralIntegrity     StructuralIntegrityMeter
	structuralIntegritySink StructuralIntegrityEventRecorder
	structuralIntegrityRepo string
	structuralIntegrityPath StructuralDropPath
}

// cloneShingleEntry is one in-memory clone_shingles row: the owning
// repo prefix plus the (already deep-copied) shingle set.
type cloneShingleEntry struct {
	repoPrefix string
	shingles   []uint64
	signature  string
	tokenCount int
	finalized  bool
}

// constValueEntry is one in-memory constant_values row: the owning repo
// prefix and file (for file-scoped eviction) plus the literal value.
type constValueEntry struct {
	repoPrefix string
	filePath   string
	value      string
}

// Compile-time assertions that the in-memory *Graph satisfies the
// optional per-symbol clone-shingle persistence capabilities, so the
// conformance suite exercises the same code path against both backends.
var (
	_ CloneShingleWriter       = (*Graph)(nil)
	_ CloneShingleReader       = (*Graph)(nil)
	_ CloneCorpusWriter        = (*Graph)(nil)
	_ CloneCorpusPager         = (*Graph)(nil)
	_ ChurnEnrichmentWriter    = (*Graph)(nil)
	_ ChurnEnrichmentReader    = (*Graph)(nil)
	_ CoverageEnrichmentWriter = (*Graph)(nil)
	_ CoverageEnrichmentReader = (*Graph)(nil)
	_ ReleaseEnrichmentWriter  = (*Graph)(nil)
	_ ReleaseEnrichmentReader  = (*Graph)(nil)
	_ BlameEnrichmentWriter    = (*Graph)(nil)
	_ BlameEnrichmentReader    = (*Graph)(nil)
	_ ReleaseEnrichmentWriter  = (*Graph)(nil)
	_ ReleaseEnrichmentReader  = (*Graph)(nil)
	_ ConstantValueWriter      = (*Graph)(nil)
	_ ConstantValueReader      = (*Graph)(nil)
)

// New creates an empty in-memory Store with a shard fan-out derived from the
// host (see defaultShardCount).
//
// This is not a selectable persistence backend: nothing it holds outlives the
// process. It has exactly two roles. In production it is the indexer's staging
// buffer — a cold index parses into it and then drains the result into the
// durable store, which is many times faster than writing every node and edge
// straight through. In tests it is the cheap Store fixture.
//
// Anything that must survive a restart needs a real store; a fence test in this
// package keeps the production call sites down to the indexer's staging ones.
func New() *Graph {
	return newWithShardCount(defaultShardCount())
}

// newWithShardCount creates an empty graph with an explicit shard
// fan-out. The count is coerced to a power of two within
// [minShards, maxShards] so the shardMask (count-1) modulo trick stays
// exact regardless of what the caller asks for. Used by New and by
// benchmarks/tests that pin a specific fan-out.
func newWithShardCount(count int) *Graph {
	count = coerceShardCount(count)
	g := &Graph{
		shards:     make([]*shard, count),
		shardCount: count,
		shardMask:  uint32(count - 1),
	}
	for i := range g.shards {
		g.shards[i] = newShard()
	}
	return g
}

// defaultShardCount picks the shard fan-out for a new graph. An explicit
// GORTEX_GRAPH_SHARDS override wins (coerced to a power of two within
// [minShards, maxShards]); otherwise the count derives from
// runtime.NumCPU(), rounded up to the next power of two and clamped to
// [derivedShardFloor, derivedShardCeiling].
func defaultShardCount() int {
	if v := strings.TrimSpace(os.Getenv(shardCountEnv)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return coerceShardCount(n)
		}
	}
	n := nextPow2(runtime.NumCPU())
	if n < derivedShardFloor {
		n = derivedShardFloor
	}
	if n > derivedShardCeiling {
		n = derivedShardCeiling
	}
	return n
}

// coerceShardCount rounds n up to the nearest power of two and clamps it
// to [minShards, maxShards]. Both bounds are powers of two, so the
// result is always a valid power-of-two shard count — the invariant the
// shardMask modulo trick depends on.
func coerceShardCount(n int) int {
	n = nextPow2(n)
	if n < minShards {
		n = minShards
	}
	if n > maxShards {
		n = maxShards
	}
	return n
}

// nextPow2 returns the smallest power of two >= n, and at least 1.
func nextPow2(n int) int {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}

// ResolveMutex returns the graph-wide mutex resolvers use to
// serialise resolution phases against this graph. Exposed for the
// resolver package; every Resolver built from the same Graph shares
// the same lock.
func (g *Graph) ResolveMutex() *sync.Mutex {
	return &g.resolveMu
}

// ReindexEdges is the batched sibling of ReindexEdge. The in-memory
// store has no per-call commit overhead so the implementation is a
// straight loop; the value of the batch API lives in the disk
// backends, where it collapses N transaction commits into one.
func (g *Graph) ReindexEdges(batch []EdgeReindex) {
	mutated := false
	for _, r := range batch {
		if r.Edge == nil {
			continue
		}
		mutated = true
		oldKind := r.OldKind
		if oldKind == "" {
			oldKind = r.Edge.Kind
		}
		if !r.RefreshIdentity {
			g.reindexEdge(r.Edge, r.OldTo, oldKind)
			continue
		}
		oldFrom := r.OldFrom
		if oldFrom == "" {
			oldFrom = r.Edge.From
		}
		g.refreshEdgeIdentity(r.Edge, oldFrom, r.OldTo, oldKind, r.OldFilePath, r.OldLine)
	}
	if mutated {
		// ReindexEdges also persists non-key payload changes. The lower-level
		// helpers already advance edgeMutGen for key moves; this coarse bump
		// covers same-key metadata/quality updates as a safe invalidation.
		g.edgeMutGen.Add(1)
	}
}

// refreshEdgeIdentity replaces an existing edge in place while repairing the
// out/in identity sidecars. Keeping the stored pointer preserves AllEdges cache
// coherence; the watcher uses this for source-span/meta refreshes that must not
// discard a resolver-selected target.
func (g *Graph) refreshEdgeIdentity(e *Edge, oldFrom, oldTo string, oldKind EdgeKind, oldFilePath string, oldLine int) {
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	g.markMutationReceiptsIncomplete()
	if oldFrom != e.From {
		g.moveEdgeIdentity(e, oldFrom, oldTo, oldKind, oldFilePath, oldLine)
		return
	}
	unlock := g.lockThreeWrite(e.From, oldTo, e.To)
	defer unlock()

	oldKey := hashEdgeKey(edgeKey{From: e.From, To: oldTo, Kind: oldKind, FilePath: oldFilePath, Line: oldLine})
	newKey := hashEdgeKey(keyOf(e))
	sFrom := g.shardFor(e.From)
	fromIdx := sFrom.outEdgeIdx[e.From]
	pos, ok := fromIdx[oldKey]
	if !ok || pos >= len(sFrom.outEdges[e.From]) {
		return
	}
	stored := sFrom.outEdges[e.From][pos]

	if oldKey != newKey || oldTo != e.To {
		delete(fromIdx, oldKey)
		fromIdx[newKey] = pos
		if keys := sFrom.outEdgeKeys[e.From]; pos < len(keys) {
			keys[pos] = newKey
		}
		sOld := g.shardFor(oldTo)
		removeEdgeFromBucket(sOld.inEdges, sOld.inEdgeKeys, sOld.inEdgeIdx, oldTo, oldKey)
		*stored = *e
		sNew := g.shardFor(e.To)
		addEdgeToBucket(sNew.inEdges, sNew.inEdgeKeys, sNew.inEdgeIdx, e.To, stored)
		g.edgeIdentityRevisions.Add(1)
		return
	}
	*stored = *e
}

// moveEdgeIdentity is the source-moving sibling of refreshEdgeIdentity. A
// returns_to materialization changes From from the enclosing caller to the
// resolved callee, so both adjacency buckets and the source-repository count
// must move together. The old identity is removed exactly (including source
// location) and the new identity keeps AddEdge's collision semantics.
func (g *Graph) moveEdgeIdentity(e *Edge, oldFrom, oldTo string, oldKind EdgeKind, oldFilePath string, oldLine int) {
	unlock := g.lockFourWrite(oldFrom, e.From, oldTo, e.To)
	defer unlock()

	oldKey := hashEdgeKey(edgeKey{
		From: oldFrom, To: oldTo, Kind: oldKind,
		FilePath: oldFilePath, Line: oldLine,
	})
	sOldFrom := g.shardFor(oldFrom)
	fromIdx := sOldFrom.outEdgeIdx[oldFrom]
	pos, ok := fromIdx[oldKey]
	if !ok || pos >= len(sOldFrom.outEdges[oldFrom]) {
		return
	}
	stored := sOldFrom.outEdges[oldFrom][pos]
	if stored == nil {
		return
	}
	oldStored := *stored
	oldStored.From = oldFrom
	oldStored.To = oldTo
	oldStored.Kind = oldKind
	oldStored.FilePath = oldFilePath
	oldStored.Line = oldLine
	var oldRepo string
	if src := sOldFrom.nodes[oldFrom]; src != nil {
		oldRepo = src.RepoPrefix
	}

	removeEdgeFromBucket(sOldFrom.outEdges, sOldFrom.outEdgeKeys, sOldFrom.outEdgeIdx, oldFrom, oldKey)
	sOldTo := g.shardFor(oldTo)
	removeEdgeFromBucket(sOldTo.inEdges, sOldTo.inEdgeKeys, sOldTo.inEdgeIdx, oldTo, oldKey)
	sOldFrom.repoEdgeRemove(oldRepo, &oldStored)

	*stored = *e
	sNewFrom := g.shardFor(e.From)
	inserted, originChanged := addEdgeToBucket(
		sNewFrom.outEdges, sNewFrom.outEdgeKeys, sNewFrom.outEdgeIdx, e.From, stored,
	)
	sNewTo := g.shardFor(e.To)
	addEdgeToBucket(sNewTo.inEdges, sNewTo.inEdgeKeys, sNewTo.inEdgeIdx, e.To, stored)
	if inserted {
		var newRepo string
		if src := sNewFrom.nodes[e.From]; src != nil {
			newRepo = src.RepoPrefix
		}
		sNewFrom.repoEdgeAdd(newRepo, stored)
	}
	if originChanged {
		g.edgeIdentityRevisions.Add(1)
	}
	// Moving source buckets (or collapsing onto an existing identity) changes
	// the materialized edge set even though the stored pointer is reused.
	g.edgeMutGen.Add(1)
	g.edgeIdentityRevisions.Add(1)
}

// BulkSetCloneShingles is the in-memory implementation of
// CloneShingleWriter. It records every (nodeID -> shingles) entry for
// one repo prefix, replacing any prior value in place. Slices are
// deep-copied on the way in so a later mutation of the caller's slice
// can't corrupt the stored state. Empty input is a no-op.
func (g *Graph) BulkSetCloneShingles(repoPrefix string, rows map[string][]uint64) error {
	if len(rows) == 0 {
		return nil
	}
	g.cloneShinglesMu.Lock()
	defer g.cloneShinglesMu.Unlock()
	if g.cloneShingles == nil {
		g.cloneShingles = make(map[string]cloneShingleEntry, len(rows))
	}
	for id, sh := range rows {
		cp := make([]uint64, len(sh))
		copy(cp, sh)
		prior := g.cloneShingles[id]
		entry := cloneShingleEntry{repoPrefix: repoPrefix, shingles: cp}
		if prior.repoPrefix == repoPrefix && slices.Equal(prior.shingles, sh) {
			entry.signature = prior.signature
			entry.tokenCount = prior.tokenCount
			entry.finalized = prior.finalized
		}
		g.cloneShingles[id] = entry
	}
	return nil
}

// BulkSetCloneCorpus is the in-memory compact clone projection writer. Rows
// are copied on ingress and replace the matching node-id entries atomically.
func (g *Graph) BulkSetCloneCorpus(repoPrefix string, rows []CloneCorpusRow) error {
	if len(rows) == 0 {
		return nil
	}
	g.cloneShinglesMu.Lock()
	defer g.cloneShinglesMu.Unlock()
	if g.cloneShingles == nil {
		g.cloneShingles = make(map[string]cloneShingleEntry, len(rows))
	}
	for _, row := range rows {
		if row.NodeID == "" {
			continue
		}
		cp := append([]uint64(nil), row.Shingles...)
		g.cloneShingles[row.NodeID] = cloneShingleEntry{
			repoPrefix: repoPrefix,
			shingles:   cp,
			signature:  row.Signature,
			tokenCount: row.TokenCount,
			finalized:  row.Finalized,
		}
	}
	return nil
}

// BulkSetCloneSignatures finalizes existing in-memory corpus entries without
// copying their unchanged shingle slices.
func (g *Graph) BulkSetCloneSignatures(repoPrefix string, updates []CloneCorpusSignatureUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	g.cloneShinglesMu.Lock()
	defer g.cloneShinglesMu.Unlock()
	for _, update := range updates {
		entry, ok := g.cloneShingles[update.NodeID]
		if !ok || entry.repoPrefix != repoPrefix {
			continue
		}
		entry.signature = update.Signature
		entry.finalized = true
		g.cloneShingles[update.NodeID] = entry
	}
	return nil
}

// CloneCorpusPage is the in-memory CloneCorpusPager: one repo's projection
// in stable node-id order, afterNodeID exclusive. Serving the paged corpus
// here flips the clone finalise / warm-rebuild paths onto sidecar TokenCount
// on this backend too, so the durable Meta["clone_tokens"] copy is only ever
// read on pre-projection stores.
func (g *Graph) CloneCorpusPage(repoPrefix, afterNodeID string, limit int) ([]CloneCorpusRow, error) {
	if limit <= 0 {
		limit = 1024
	}
	g.cloneShinglesMu.Lock()
	ids := make([]string, 0, len(g.cloneShingles))
	for id, entry := range g.cloneShingles {
		if entry.repoPrefix == repoPrefix && id > afterNodeID {
			ids = append(ids, id)
		}
	}
	g.cloneShinglesMu.Unlock()
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	rows := make([]CloneCorpusRow, 0, len(ids))
	g.cloneShinglesMu.Lock()
	defer g.cloneShinglesMu.Unlock()
	for _, id := range ids {
		entry, ok := g.cloneShingles[id]
		if !ok {
			continue
		}
		rows = append(rows, CloneCorpusRow{
			NodeID:     id,
			RepoPrefix: entry.repoPrefix,
			Shingles:   append([]uint64(nil), entry.shingles...),
			Signature:  entry.signature,
			TokenCount: entry.tokenCount,
			Finalized:  entry.finalized,
		})
	}
	return rows, nil
}

// DeleteCloneShingles is the in-memory implementation of the
// CloneShingleWriter delete side. It drops the rows for the supplied
// node ids. Empty input is a no-op; missing ids are ignored.
func (g *Graph) DeleteCloneShingles(nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	g.cloneShinglesMu.Lock()
	defer g.cloneShinglesMu.Unlock()
	for _, id := range nodeIDs {
		if id == "" {
			continue
		}
		delete(g.cloneShingles, id)
	}
	return nil
}

// LoadCloneShingles is the in-memory implementation of
// CloneShingleReader. It returns a fresh map of the shingle sets owned
// by one repo prefix, deep-copying each slice so callers can't mutate
// the stored state. Always returns a non-nil (possibly empty) map and
// never an error.
func (g *Graph) LoadCloneShingles(repoPrefix string) (map[string][]uint64, error) {
	g.cloneShinglesMu.Lock()
	defer g.cloneShinglesMu.Unlock()
	out := make(map[string][]uint64)
	for id, entry := range g.cloneShingles {
		if entry.repoPrefix != repoPrefix {
			continue
		}
		cp := make([]uint64, len(entry.shingles))
		copy(cp, entry.shingles)
		out[id] = cp
	}
	return out, nil
}

// DrainCloneCorpusBatches removes one repository's compact clone projection
// and yields bounded node-id-sorted batches. It streams directly from the
// existing sidecar map, so a shadow drain never builds a second repo-sized
// clone corpus in memory.
func (g *Graph) DrainCloneCorpusBatches(repoPrefix string, maxRows int, maxBytes uint64) iter.Seq[[]CloneCorpusRow] {
	if maxRows <= 0 {
		maxRows = 1
	}
	return func(yield func([]CloneCorpusRow) bool) {
		g.cloneShinglesMu.Lock()
		defer g.cloneShinglesMu.Unlock()

		batch := make([]CloneCorpusRow, 0, maxRows)
		var batchBytes uint64
		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			slices.SortFunc(batch, func(a, b CloneCorpusRow) int {
				return strings.Compare(a.NodeID, b.NodeID)
			})
			if !yield(batch) {
				return false
			}
			batch = make([]CloneCorpusRow, 0, maxRows)
			batchBytes = 0
			return true
		}
		for nodeID, entry := range g.cloneShingles {
			if entry.repoPrefix != repoPrefix {
				continue
			}
			delete(g.cloneShingles, nodeID)
			row := CloneCorpusRow{
				NodeID: nodeID, RepoPrefix: entry.repoPrefix,
				Shingles:  append([]uint64(nil), entry.shingles...),
				Signature: entry.signature, TokenCount: entry.tokenCount,
				Finalized: entry.finalized,
			}
			nextBytes := uint64(len(row.NodeID) + len(row.Signature) + len(row.Shingles)*8 + 48)
			if len(batch) > 0 && (len(batch) >= maxRows || maxBytes > 0 && batchBytes+nextBytes > maxBytes) {
				if !flush() {
					return
				}
			}
			batch = append(batch, row)
			batchBytes += nextBytes
			if len(batch) >= maxRows || maxBytes > 0 && batchBytes >= maxBytes {
				if !flush() {
					return
				}
			}
		}
		flush()
	}
}

// BulkSetConstantValues is the in-memory ConstantValueWriter. It records
// every (nodeID -> value) row for one repo prefix, replacing any prior
// value in place. Empty input is a no-op.
func (g *Graph) BulkSetConstantValues(repoPrefix string, rows []ConstantValueRow) error {
	if len(rows) == 0 {
		return nil
	}
	g.constValuesMu.Lock()
	defer g.constValuesMu.Unlock()
	if g.constValues == nil {
		g.constValues = make(map[string]constValueEntry, len(rows))
	}
	for _, r := range rows {
		if r.NodeID == "" {
			continue
		}
		g.constValues[r.NodeID] = constValueEntry{repoPrefix: repoPrefix, filePath: r.FilePath, value: r.Value}
	}
	return nil
}

// ReplaceConstantValues atomically swaps the in-memory repository projection.
// Empty rows deliberately clear the prior projection.
func (g *Graph) ReplaceConstantValues(repoPrefix string, rows []ConstantValueRow) error {
	g.constValuesMu.Lock()
	defer g.constValuesMu.Unlock()
	if g.constValues == nil {
		g.constValues = make(map[string]constValueEntry, len(rows))
	}
	for id, entry := range g.constValues {
		if entry.repoPrefix == repoPrefix {
			delete(g.constValues, id)
		}
	}
	for _, row := range rows {
		if row.NodeID == "" {
			continue
		}
		g.constValues[row.NodeID] = constValueEntry{
			repoPrefix: repoPrefix,
			filePath:   row.FilePath,
			value:      row.Value,
		}
	}
	return nil
}

// DeleteConstantValuesByFiles is the in-memory ConstantValueWriter delete
// side: it drops every row whose file is in the supplied set for the given
// repo prefix, so a reindex of those files replaces their values cleanly.
func (g *Graph) DeleteConstantValuesByFiles(repoPrefix string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	fileSet := make(map[string]struct{}, len(files))
	for _, f := range files {
		fileSet[f] = struct{}{}
	}
	g.constValuesMu.Lock()
	defer g.constValuesMu.Unlock()
	for id, entry := range g.constValues {
		if entry.repoPrefix != repoPrefix {
			continue
		}
		if _, ok := fileSet[entry.filePath]; ok {
			delete(g.constValues, id)
		}
	}
	return nil
}

// ConstantValuesByNodeIDs is the in-memory ConstantValueReader. It returns
// the recorded values for the supplied node ids (omitting ids with no
// recorded value). Always returns a non-nil map and never an error.
func (g *Graph) ConstantValuesByNodeIDs(nodeIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	g.constValuesMu.Lock()
	defer g.constValuesMu.Unlock()
	for _, id := range nodeIDs {
		if entry, ok := g.constValues[id]; ok {
			out[id] = entry.value
		}
	}
	return out, nil
}

// SetFileMetas is the in-memory FileMetaWriter. It records each per-file row
// for one repo prefix, replacing any prior row in place. Empty input is a
// no-op.
func (g *Graph) SetFileMetas(repoPrefix string, rows []FileMetaRow) error {
	if len(rows) == 0 {
		return nil
	}
	g.fileMetasMu.Lock()
	defer g.fileMetasMu.Unlock()
	if g.fileMetas == nil {
		g.fileMetas = make(map[string]map[string]FileMetaRow)
	}
	byFile := g.fileMetas[repoPrefix]
	if byFile == nil {
		byFile = make(map[string]FileMetaRow, len(rows))
		g.fileMetas[repoPrefix] = byFile
	}
	for _, r := range rows {
		if r.FilePath == "" {
			continue
		}
		byFile[r.FilePath] = r
	}
	return nil
}

// ReplaceFileMetas atomically swaps the in-memory repository projection.
// Empty rows deliberately clear the prior projection.
func (g *Graph) ReplaceFileMetas(repoPrefix string, rows []FileMetaRow) error {
	byFile := make(map[string]FileMetaRow, len(rows))
	for _, row := range rows {
		if row.FilePath != "" {
			byFile[row.FilePath] = row
		}
	}
	g.fileMetasMu.Lock()
	defer g.fileMetasMu.Unlock()
	if g.fileMetas == nil {
		g.fileMetas = make(map[string]map[string]FileMetaRow)
	}
	g.fileMetas[repoPrefix] = byFile
	return nil
}

// DeleteFileMetasByFiles drops the rows for the supplied files in one repo
// prefix so a reindex replaces them cleanly.
func (g *Graph) DeleteFileMetasByFiles(repoPrefix string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	g.fileMetasMu.Lock()
	defer g.fileMetasMu.Unlock()
	byFile := g.fileMetas[repoPrefix]
	if byFile == nil {
		return nil
	}
	for _, f := range files {
		delete(byFile, f)
	}
	return nil
}

// FileMetasForRepo is the in-memory FileMetaReader: every recorded file row
// for the repo prefix. Always non-nil.
func (g *Graph) FileMetasForRepo(repoPrefix string) ([]FileMetaRow, error) {
	g.fileMetasMu.Lock()
	defer g.fileMetasMu.Unlock()
	byFile := g.fileMetas[repoPrefix]
	out := make([]FileMetaRow, 0, len(byFile))
	for _, r := range byFile {
		out = append(out, r)
	}
	return out, nil
}

// FileMetasByPaths returns only the requested metadata rows. The result is
// keyed by the canonical stored file path; missing paths are omitted.
func (g *Graph) FileMetasByPaths(repoPrefix string, filePaths []string) (map[string]FileMetaRow, error) {
	out := make(map[string]FileMetaRow, len(filePaths))
	if len(filePaths) == 0 {
		return out, nil
	}
	g.fileMetasMu.Lock()
	defer g.fileMetasMu.Unlock()
	byFile := g.fileMetas[repoPrefix]
	for _, filePath := range filePaths {
		if row, ok := byFile[filePath]; ok {
			out[filePath] = row
		}
	}
	return out, nil
}

// BulkSetChurn is the in-memory ChurnEnrichmentWriter. ChurnEnrichment
// is a flat value type, so a map store needs no deep copy.
func (g *Graph) BulkSetChurn(repoPrefix string, rows []ChurnEnrichment) error {
	if len(rows) == 0 {
		return nil
	}
	g.churnEnrichMu.Lock()
	defer g.churnEnrichMu.Unlock()
	if g.churnEnrich == nil {
		g.churnEnrich = make(map[string]ChurnEnrichment, len(rows))
	}
	for _, r := range rows {
		r.RepoPrefix = repoPrefix
		g.churnEnrich[r.NodeID] = r
	}
	return nil
}

// DeleteChurn is the in-memory ChurnEnrichmentWriter delete side.
func (g *Graph) DeleteChurn(nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	g.churnEnrichMu.Lock()
	defer g.churnEnrichMu.Unlock()
	for _, id := range nodeIDs {
		if id != "" {
			delete(g.churnEnrich, id)
		}
	}
	return nil
}

// ChurnRows is the in-memory ChurnEnrichmentReader. An empty repoPrefix
// returns all rows across repos.
func (g *Graph) ChurnRows(repoPrefix string) []ChurnEnrichment {
	g.churnEnrichMu.Lock()
	defer g.churnEnrichMu.Unlock()
	out := make([]ChurnEnrichment, 0, len(g.churnEnrich))
	for _, r := range g.churnEnrich {
		if repoPrefix != "" && r.RepoPrefix != repoPrefix {
			continue
		}
		out = append(out, r)
	}
	return out
}

// BulkSetCoverage is the in-memory CoverageEnrichmentWriter.
func (g *Graph) BulkSetCoverage(repoPrefix string, rows []CoverageEnrichment) error {
	if len(rows) == 0 {
		return nil
	}
	g.coverageEnrichMu.Lock()
	defer g.coverageEnrichMu.Unlock()
	if g.coverageEnrich == nil {
		g.coverageEnrich = make(map[string]CoverageEnrichment, len(rows))
	}
	for _, r := range rows {
		r.RepoPrefix = repoPrefix
		g.coverageEnrich[r.NodeID] = r
	}
	return nil
}

// DeleteCoverage is the in-memory CoverageEnrichmentWriter delete side.
func (g *Graph) DeleteCoverage(nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	g.coverageEnrichMu.Lock()
	defer g.coverageEnrichMu.Unlock()
	for _, id := range nodeIDs {
		if id != "" {
			delete(g.coverageEnrich, id)
		}
	}
	return nil
}

// ChurnRows-style reader for coverage; empty repoPrefix returns all.
func (g *Graph) CoverageRows(repoPrefix string) []CoverageEnrichment {
	g.coverageEnrichMu.Lock()
	defer g.coverageEnrichMu.Unlock()
	out := make([]CoverageEnrichment, 0, len(g.coverageEnrich))
	for _, r := range g.coverageEnrich {
		if repoPrefix != "" && r.RepoPrefix != repoPrefix {
			continue
		}
		out = append(out, r)
	}
	return out
}

// BulkSetReleases is the in-memory ReleaseEnrichmentWriter.
func (g *Graph) BulkSetReleases(repoPrefix string, rows []ReleaseEnrichment) error {
	if len(rows) == 0 {
		return nil
	}
	g.releaseEnrichMu.Lock()
	defer g.releaseEnrichMu.Unlock()
	if g.releaseEnrich == nil {
		g.releaseEnrich = make(map[string]ReleaseEnrichment, len(rows))
	}
	for _, r := range rows {
		r.RepoPrefix = repoPrefix
		g.releaseEnrich[r.NodeID] = r
	}
	return nil
}

// DeleteReleases is the in-memory ReleaseEnrichmentWriter delete side.
func (g *Graph) DeleteReleases(nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	g.releaseEnrichMu.Lock()
	defer g.releaseEnrichMu.Unlock()
	for _, id := range nodeIDs {
		if id != "" {
			delete(g.releaseEnrich, id)
		}
	}
	return nil
}

// ReleaseRows reads release rows; empty repoPrefix returns all.
func (g *Graph) ReleaseRows(repoPrefix string) []ReleaseEnrichment {
	g.releaseEnrichMu.Lock()
	defer g.releaseEnrichMu.Unlock()
	out := make([]ReleaseEnrichment, 0, len(g.releaseEnrich))
	for _, r := range g.releaseEnrich {
		if repoPrefix != "" && r.RepoPrefix != repoPrefix {
			continue
		}
		out = append(out, r)
	}
	return out
}

// BulkSetBlame is the in-memory BlameEnrichmentWriter.
func (g *Graph) BulkSetBlame(repoPrefix string, rows []BlameEnrichment) error {
	if len(rows) == 0 {
		return nil
	}
	g.blameEnrichMu.Lock()
	defer g.blameEnrichMu.Unlock()
	if g.blameEnrich == nil {
		g.blameEnrich = make(map[string]BlameEnrichment, len(rows))
	}
	for _, r := range rows {
		r.RepoPrefix = repoPrefix
		g.blameEnrich[r.NodeID] = r
	}
	return nil
}

// DeleteBlame is the in-memory BlameEnrichmentWriter delete side.
func (g *Graph) DeleteBlame(nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	g.blameEnrichMu.Lock()
	defer g.blameEnrichMu.Unlock()
	for _, id := range nodeIDs {
		if id != "" {
			delete(g.blameEnrich, id)
		}
	}
	return nil
}

// BlameRows reads blame rows; empty repoPrefix returns all.
func (g *Graph) BlameRows(repoPrefix string) []BlameEnrichment {
	g.blameEnrichMu.Lock()
	defer g.blameEnrichMu.Unlock()
	out := make([]BlameEnrichment, 0, len(g.blameEnrich))
	for _, r := range g.blameEnrich {
		if repoPrefix != "" && r.RepoPrefix != repoPrefix {
			continue
		}
		out = append(out, r)
	}
	return out
}

// EdgesByKind yields every edge whose Kind matches. The in-memory
// implementation snapshots only matching edges one shard at a time;
// callbacks run after releasing the shard lock so they may safely mutate
// the graph. Disk backends override this with an index-backed scan.
func (g *Graph) EdgesByKind(kind EdgeKind) iter.Seq[*Edge] {
	return func(yield func(*Edge) bool) {
		for _, s := range g.shards {
			s.mu.RLock()
			var matches []*Edge
			for _, edges := range s.outEdges {
				for _, e := range edges {
					if e != nil && e.Kind == kind {
						matches = append(matches, e)
					}
				}
			}
			s.mu.RUnlock()

			for _, e := range matches {
				if !yield(e) {
					return
				}
			}
		}
	}
}

// EdgesByKinds is the in-memory reference implementation of
// EdgesByKindsScanner. It snapshots only matching edges one shard at a
// time. Disk backends override with one indexed WHERE kind IN query.
//
// Empty kinds yields nothing — matches the disk contract.
func (g *Graph) EdgesByKinds(kinds []EdgeKind) iter.Seq[*Edge] {
	if len(kinds) == 0 {
		return func(yield func(*Edge) bool) {}
	}
	set := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		set[k] = struct{}{}
	}
	if len(set) == 0 {
		return func(yield func(*Edge) bool) {}
	}
	return func(yield func(*Edge) bool) {
		for _, s := range g.shards {
			s.mu.RLock()
			var matches []*Edge
			for _, edges := range s.outEdges {
				for _, e := range edges {
					if e == nil {
						continue
					}
					if _, ok := set[e.Kind]; ok {
						matches = append(matches, e)
					}
				}
			}
			s.mu.RUnlock()

			for _, e := range matches {
				if !yield(e) {
					return
				}
			}
		}
	}
}

// DistinctExternalTargets returns the unique external-shaped destinations for
// kinds in one shard walk. It keeps only target strings in memory and never
// materialises the graph's full edge set.
func (g *Graph) DistinctExternalTargets(kinds []EdgeKind) []string {
	if len(kinds) == 0 {
		return nil
	}
	kindSet := make(map[EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if kind != "" {
			kindSet[kind] = struct{}{}
		}
	}
	if len(kindSet) == 0 {
		return nil
	}

	targetSet := make(map[string]struct{})
	for _, s := range g.shards {
		s.mu.RLock()
		for _, edges := range s.outEdges {
			for _, e := range edges {
				if e == nil || !isExternalTargetID(e.To) {
					continue
				}
				if _, ok := kindSet[e.Kind]; ok {
					targetSet[e.To] = struct{}{}
				}
			}
		}
		s.mu.RUnlock()
	}

	targets := make([]string, 0, len(targetSet))
	for target := range targetSet {
		targets = append(targets, target)
	}
	slices.Sort(targets)
	return targets
}

func isExternalTargetID(id string) bool {
	return strings.HasPrefix(id, "dep::") ||
		strings.HasPrefix(id, "external::") ||
		strings.HasPrefix(id, "stdlib::") ||
		strings.Contains(id, "::stdlib::") ||
		strings.HasPrefix(id, "external-call::")
}

// NodesByKind yields every node whose Kind matches. It snapshots only
// matching nodes one shard at a time and never materialises the full graph.
func (g *Graph) NodesByKind(kind NodeKind) iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		for _, s := range g.shards {
			s.mu.RLock()
			matches := make([]*Node, 0)
			for _, n := range s.nodes {
				if n != nil && n.Kind == kind {
					matches = append(matches, n)
				}
			}
			s.mu.RUnlock()

			for _, n := range matches {
				if !yield(n) {
					return
				}
			}
		}
	}
}

// GetNodesByIDs returns a map id→*Node for every input ID that
// exists in the store. The in-memory implementation loops the
// existing GetNode — algorithmic cost identical to a hand-written
// loop in the caller, no concurrency win here. The value of the
// batched API lives in the disk backends, where it collapses N
// per-id SQL/bolt queries into one.
func (g *Graph) GetNodesByIDs(ids []string) map[string]*Node {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]*Node, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := out[id]; ok {
			continue
		}
		if n := g.GetNode(id); n != nil {
			out[id] = n
		}
	}
	return out
}

// FindNodesByNames is the batched sibling of FindNodesByName.
func (g *Graph) FindNodesByNames(names []string) map[string][]*Node {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string][]*Node, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := out[name]; ok {
			continue
		}
		matches := g.FindNodesByName(name)
		if len(matches) > 0 {
			out[name] = matches
		}
	}
	return out
}

// EdgesWithUnresolvedTarget yields every edge whose To has the
// "unresolved::" prefix — the resolver's main pending-edge filter.
// In-memory iterates all edges and prefix-checks; disk backends back
// it with a range scan on a to-keyed index. Gate-owned fn-value
// placeholders (`unresolved::fnvalue::<name>`) are excluded: the master
// resolver can never bind them, so surfacing them here only bloats the
// pending set it reconciles every pass — the fn-value gate scans them
// itself via EdgesByKind(references).
func (g *Graph) EdgesWithUnresolvedTarget() iter.Seq[*Edge] {
	return func(yield func(*Edge) bool) {
		const snapshotRows = 2048
		for _, shard := range g.shards {
			// Snapshot only source IDs, never the shard's edge corpus. Each
			// adjacency bucket is then copied in bounded pieces so callbacks may
			// safely reindex without running under the shard read lock.
			shard.mu.RLock()
			sources := make([]string, 0, len(shard.outEdges))
			for from := range shard.outEdges {
				sources = append(sources, from)
			}
			shard.mu.RUnlock()
			sort.Strings(sources)

			for _, from := range sources {
				for offset := 0; ; {
					shard.mu.RLock()
					edges := shard.outEdges[from]
					if offset >= len(edges) {
						shard.mu.RUnlock()
						break
					}
					end := offset + snapshotRows
					if end > len(edges) {
						end = len(edges)
					}
					window := append([]*Edge(nil), edges[offset:end]...)
					offset = end
					shard.mu.RUnlock()

					for _, e := range window {
						// IsUnresolvedTarget matches both bare and repo-qualified
						// stubs; fn-value placeholders belong to their own gate.
						if e == nil || !IsUnresolvedTarget(e.To) || IsFnValuePlaceholder(e.To) {
							continue
						}
						if !yield(e) {
							return
						}
					}
				}
			}
		}
	}
}

// FnValuePlaceholderEdges implements graph.FnValuePlaceholderScanner: it yields
// every edge whose To is a fn-value gate placeholder (both the bare and the
// multi-repo COPY-rewrite forms) — the exact inverse of the exclusion
// EdgesWithUnresolvedTarget applies. Full edges with Meta intact, since the gate
// reads Meta["via"] and the captured fn_value_name off each one.
func (g *Graph) FnValuePlaceholderEdges() iter.Seq[*Edge] {
	return func(yield func(*Edge) bool) {
		for _, e := range g.AllEdges() {
			if e == nil || !IsFnValuePlaceholder(e.To) {
				continue
			}
			if !yield(e) {
				return
			}
		}
	}
}

// AllEdgesLight implements graph.LightEdgeScanner for the in-memory backend.
// There is no separate meta blob to skip here — the edges are live structs — so
// it returns the matching edges as-is, Meta present. Honouring the "Meta
// absent" half of the contract would mean copying every edge to strip Meta,
// doubling heap for zero benefit; the contract only promises Meta MAY be absent
// and correct callers read only the promoted fields either way. An empty kinds
// list returns every edge.
func (g *Graph) AllEdgesLight(kinds ...EdgeKind) []*Edge {
	all := g.AllEdges()
	if len(kinds) == 0 {
		return all
	}
	want := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k != "" {
			want[k] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil
	}
	out := make([]*Edge, 0, len(all))
	for _, e := range all {
		if e != nil {
			if _, ok := want[e.Kind]; ok {
				out = append(out, e)
			}
		}
	}
	return out
}

// DeadCodeCandidates is the in-memory reference implementation of
// DeadCodeCandidator. Iterates the requested node kinds and filters
// out anything whose incoming-edge bucket contains an allowlist match
// — same algorithm the analysis.FindDeadCode loop runs, just exposed
// as a single capability the disk backend can short-circuit with
// one query per kind. Pure map / slice walks here; the win lives
// in the disk backend where the equivalent path materialises the full
// in-edge map.
func (g *Graph) DeadCodeCandidates(allowedNodeKinds []NodeKind, allowedInEdgeKinds map[NodeKind][]EdgeKind) []*Node {
	if len(allowedNodeKinds) == 0 {
		return nil
	}
	// Build a per-kind set so the inner loop can match against a map
	// instead of re-scanning the allowlist slice for every edge.
	allowedSet := make(map[NodeKind]map[EdgeKind]struct{}, len(allowedNodeKinds))
	for _, k := range allowedNodeKinds {
		set := make(map[EdgeKind]struct{}, len(allowedInEdgeKinds[k]))
		for _, ek := range allowedInEdgeKinds[k] {
			set[ek] = struct{}{}
		}
		allowedSet[k] = set
	}

	var out []*Node
	for _, k := range allowedNodeKinds {
		allowed, hasAllow := allowedSet[k]
		anyKindCounts := !hasAllow || len(allowed) == 0
		for n := range g.NodesByKind(k) {
			if n == nil {
				continue
			}
			incoming := g.GetInEdges(n.ID)
			dead := true
			for _, e := range incoming {
				if e == nil {
					continue
				}
				if anyKindCounts {
					dead = false
					break
				}
				if _, ok := allowed[e.Kind]; ok {
					dead = false
					break
				}
			}
			if dead {
				out = append(out, n)
			}
		}
	}
	return out
}

// IfaceImplementsRows is the in-memory reference implementation of
// IfaceImplementsScanner. Joins KindInterface nodes carrying
// Meta["methods"] with their EdgeImplements predecessors and returns
// one row per (typeID, ifaceID, ifaceMeta) tuple.
func (g *Graph) IfaceImplementsRows() []IfaceImplementsRow {
	// Index interfaces with methods by ID so the edge walk is O(edges)
	// rather than O(edges × interfaces).
	ifaceMeta := make(map[string]map[string]any)
	for n := range g.NodesByKind(KindInterface) {
		if n == nil || n.Meta == nil {
			continue
		}
		if _, ok := n.Meta["methods"]; !ok {
			continue
		}
		ifaceMeta[n.ID] = n.Meta
	}
	if len(ifaceMeta) == 0 {
		return nil
	}
	var out []IfaceImplementsRow
	for e := range g.EdgesByKind(EdgeImplements) {
		if e == nil {
			continue
		}
		meta, ok := ifaceMeta[e.To]
		if !ok {
			continue
		}
		out = append(out, IfaceImplementsRow{
			TypeID:    e.From,
			IfaceID:   e.To,
			IfaceMeta: meta,
		})
	}
	return out
}

// NodeDegreeCounts is the in-memory reference implementation of
// NodeDegreeAggregator. Walks the per-node in/out edge buckets the
// in-memory backend already maintains — same cost as the per-node
// loop GraphConnectivity ran before this capability landed, just
// folded into one method call so the analyzer can pick the disk
// backend's bulk implementation transparently. Missing ids are
// elided from the result (matching the disk contract).
func (g *Graph) NodeDegreeCounts(ids []string, usageKinds []EdgeKind) []NodeDegreeRow {
	if len(ids) == 0 {
		return nil
	}
	usage := make(map[EdgeKind]struct{}, len(usageKinds))
	for _, k := range usageKinds {
		usage[k] = struct{}{}
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]NodeDegreeRow, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		// Skip unknown ids — the disk backend's WHERE n.id IN $ids
		// clause naturally drops them; mirror that here so both
		// backends return the same row count.
		if g.GetNode(id) == nil {
			continue
		}
		in := g.GetInEdges(id)
		row := NodeDegreeRow{
			NodeID:   id,
			InCount:  len(in),
			OutCount: len(g.GetOutEdges(id)),
		}
		if len(usage) > 0 {
			for _, e := range in {
				if e == nil {
					continue
				}
				if _, ok := usage[e.Kind]; ok {
					row.UsageInCount++
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// FileImporters is the in-memory reference implementation of the
// FileImporters capability. Iterates EdgeImports via the byKind
// bucket — same cost as the legacy AllEdges()+filter loop in
// handleCheckReferences, but exposes the predicate as a single call
// the disk backend can short-circuit with one query.
//
// Matches edges whose To node satisfies filePath == n.FilePath OR
// filePath == n.ID. The dual match keeps parity with the indexer's
// two import shapes: file-targeted imports point at the file node
// (n.ID == filePath), while symbol-targeted imports land on a symbol
// whose FilePath equals filePath.
func (g *Graph) FileImporters(filePath string) []FileImporterRow {
	if filePath == "" {
		return nil
	}
	var out []FileImporterRow
	for e := range g.EdgesByKind(EdgeImports) {
		if e == nil {
			continue
		}
		to := g.GetNode(e.To)
		if to == nil {
			continue
		}
		if to.FilePath != filePath && to.ID != filePath {
			continue
		}
		from := g.GetNode(e.From)
		if from == nil {
			continue
		}
		out = append(out, FileImporterRow{
			FromFile: from.FilePath,
			FromID:   from.ID,
			FromName: from.Name,
			FromKind: from.Kind,
		})
	}
	return out
}

// NodeFanCounts is the in-memory reference implementation of
// NodeFanAggregator. Two passes over the per-node in/out edge buckets
// the in-memory backend already maintains, filtered by the caller's
// kind sets. The disk backend overrides with one query per direction
// to drop the AllEdges() materialisation FindHotspots / health_score
// were running every call.
func (g *Graph) NodeFanCounts(ids []string, fanInKinds []EdgeKind, fanOutKinds []EdgeKind) []NodeFanRow {
	if len(ids) == 0 {
		return nil
	}
	inSet := make(map[EdgeKind]struct{}, len(fanInKinds))
	for _, k := range fanInKinds {
		inSet[k] = struct{}{}
	}
	outSet := make(map[EdgeKind]struct{}, len(fanOutKinds))
	for _, k := range fanOutKinds {
		outSet[k] = struct{}{}
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]NodeFanRow, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if g.GetNode(id) == nil {
			continue
		}
		row := NodeFanRow{NodeID: id}
		if len(inSet) > 0 {
			for _, e := range g.GetInEdges(id) {
				if e == nil {
					continue
				}
				if _, ok := inSet[e.Kind]; ok {
					row.FanIn++
				}
			}
		}
		if len(outSet) > 0 {
			for _, e := range g.GetOutEdges(id) {
				if e == nil {
					continue
				}
				if _, ok := outSet[e.Kind]; ok {
					row.FanOut++
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// InEdgeCountsByKind is the in-memory reference implementation of
// the InEdgeCounter capability. Walks each requested EdgeKind via
// the byKind bucket and increments a per-To counter. Same algorithm
// the AllEdges-bucketing fallback in handleGetUntestedSymbols runs;
// the win lives in the disk backend where AllEdges() materialises every
// edge just to bucket by target.
//
// Dedupes the kind set up front so a sloppy caller passing the same
// kind twice doesn't double-count — matches the disk backend's
// IN-list dedup.
func (g *Graph) InEdgeCountsByKind(kinds []EdgeKind) map[string]int {
	if len(kinds) == 0 {
		return nil
	}
	seen := make(map[EdgeKind]struct{}, len(kinds))
	out := make(map[string]int)
	for _, k := range kinds {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		for e := range g.EdgesByKind(k) {
			if e == nil {
				continue
			}
			out[e.To]++
		}
	}
	return out
}

// NodesInFilesByKind is the in-memory reference implementation of
// the NodesInFilesByKindFinder capability. Filters NodesByKind for
// each requested kind down to the file set. Same algorithm as the
// Go-side loop in find_declaration's buildDeclFileIndex; the win
// lives in disk backends where AllNodes() over cgo dwarfs the few
// hundred surviving rows.
func (g *Graph) NodesInFilesByKind(files []string, kinds []NodeKind) []*Node {
	if len(files) == 0 || len(kinds) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(files))
	for _, f := range files {
		if f == "" {
			continue
		}
		wanted[f] = struct{}{}
	}
	if len(wanted) == 0 {
		return nil
	}
	// Dedup the kinds so a sloppy caller doesn't double-scan.
	seenKind := make(map[NodeKind]struct{}, len(kinds))
	var out []*Node
	for _, k := range kinds {
		if _, ok := seenKind[k]; ok {
			continue
		}
		seenKind[k] = struct{}{}
		for n := range g.NodesByKind(k) {
			if n == nil {
				continue
			}
			if _, ok := wanted[n.FilePath]; !ok {
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

// NodesByKinds is the in-memory reference implementation of the
// NodesByKindsScanner capability. Loops the existing NodesByKind
// iterator per requested kind — algorithmic cost identical to the
// hand-written `for _, n := range AllNodes() if n.Kind == K` pattern
// the metadata analyzers used before. The win lives in the disk
// backend, where one IN-list query replaces the AllNodes() pull.
//
// Dedupes the kind set up front so a sloppy caller passing the same
// kind twice doesn't double-yield — matches the disk backend's
// IN-list dedup. Empty kinds returns nil without touching the store.
func (g *Graph) NodesByKinds(kinds []NodeKind) []*Node {
	if len(kinds) == 0 {
		return nil
	}
	seen := make(map[NodeKind]struct{}, len(kinds))
	var out []*Node
	for _, k := range kinds {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		for n := range g.NodesByKind(k) {
			if n == nil {
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

// EdgeAdjacencyForKinds is the in-memory reference implementation of
// the EdgeAdjacencyForKinds capability. One AllEdges scan that yields
// (from, to) pairs whose Kind is in the supplied edge-kind set AND
// whose endpoints both have a Kind in the node-kind set — identical
// shape to the join the disk backend folds into a single
// query.
//
// Empty edgeKinds or empty nodeKinds yields nothing — matches the
// disk contract.
func (g *Graph) EdgeAdjacencyForKinds(edgeKinds []EdgeKind, nodeKinds []NodeKind) iter.Seq[[2]string] {
	if len(edgeKinds) == 0 || len(nodeKinds) == 0 {
		return func(yield func([2]string) bool) {}
	}
	eset := make(map[EdgeKind]struct{}, len(edgeKinds))
	for _, k := range edgeKinds {
		if k == "" {
			continue
		}
		eset[k] = struct{}{}
	}
	nset := make(map[NodeKind]struct{}, len(nodeKinds))
	for _, k := range nodeKinds {
		if k == "" {
			continue
		}
		nset[k] = struct{}{}
	}
	if len(eset) == 0 || len(nset) == 0 {
		return func(yield func([2]string) bool) {}
	}
	return func(yield func([2]string) bool) {
		for _, e := range g.AllEdges() {
			if e == nil {
				continue
			}
			if _, ok := eset[e.Kind]; !ok {
				continue
			}
			from := g.GetNode(e.From)
			to := g.GetNode(e.To)
			if from == nil || to == nil {
				continue
			}
			if _, ok := nset[from.Kind]; !ok {
				continue
			}
			if _, ok := nset[to.Kind]; !ok {
				continue
			}
			if !yield([2]string{e.From, e.To}) {
				return
			}
		}
	}
}

// CommunityCrossingsByKind is the in-memory reference implementation
// of the CommunityCrossingsByKind capability. AllEdges scan with the
// kind-set filter, then a Go-side community comparison per edge —
// the exact loop FindHotspots.countCrossings ran before this
// capability existed.
//
// Empty kinds or empty nodeToComm returns nil. Zero-count sources
// never surface (matches the disk contract — callers probe by
// existence).
func (g *Graph) CommunityCrossingsByKind(kinds []EdgeKind, nodeToComm map[string]string) map[string]int {
	if len(kinds) == 0 || len(nodeToComm) == 0 {
		return nil
	}
	set := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		set[k] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		if _, ok := set[e.Kind]; !ok {
			continue
		}
		from := nodeToComm[e.From]
		to := nodeToComm[e.To]
		if from == "" || to == "" || from == to {
			continue
		}
		out[e.From]++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NodeIDsByKinds is the in-memory reference implementation of the
// NodeIDsByKinds capability. Single AllNodes pass with a kind-set
// filter, deduped on input — same algorithm as NodesByKinds but
// returns only the ID column. The disk-backend win is the projection
// drop, not the algorithmic shape.
func (g *Graph) NodeIDsByKinds(kinds []NodeKind) []string {
	if len(kinds) == 0 {
		return nil
	}
	seen := make(map[NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		seen[k] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	var out []string
	for _, n := range g.AllNodes() {
		if n == nil {
			continue
		}
		if _, ok := seen[n.Kind]; !ok {
			continue
		}
		out = append(out, n.ID)
	}
	return out
}

// EdgeKindCounts is the in-memory reference implementation of the
// EdgeKindCounter capability. One AllEdges scan with a per-kind
// tally — the exact loop the get_surprising_connections Go fallback
// already runs today, just exposed as a single method call so the
// the disk backend can short-circuit with a server-side GROUP BY.
//
// Empty graph returns nil so callers can short-circuit a downstream
// "kindCounts != nil" gate.
func (g *Graph) EdgeKindCounts() map[EdgeKind]int {
	out := map[EdgeKind]int{}
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		out[e.Kind]++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CrossRepoEdgeCounts is the in-memory reference implementation of
// CrossRepoEdgeAggregator. Iterates the four cross_repo_* byKind
// buckets and groups by (kind, fromRepoPrefix, toRepoPrefix). Same
// algorithm as the architecture handler's AllEdges loop but exposes
// it as a single capability so the disk backend can fold the join into
// one query.
//
// Returns nil when the graph carries no cross-repo edges (single-
// repo mode) so the caller's empty-list rendering kicks in without
// allocating.
func (g *Graph) CrossRepoEdgeCounts() []CrossRepoEdgeRow {
	type key struct {
		kind     EdgeKind
		fromRepo string
		toRepo   string
	}
	counts := map[key]int{}
	for _, k := range []EdgeKind{
		EdgeCrossRepoCalls,
		EdgeCrossRepoImplements,
		EdgeCrossRepoExtends,
	} {
		for e := range g.EdgesByKind(k) {
			if e == nil {
				continue
			}
			from := g.GetNode(e.From)
			to := g.GetNode(e.To)
			if from == nil || to == nil {
				continue
			}
			counts[key{kind: e.Kind, fromRepo: from.RepoPrefix, toRepo: to.RepoPrefix}]++
		}
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]CrossRepoEdgeRow, 0, len(counts))
	for k, c := range counts {
		out = append(out, CrossRepoEdgeRow{
			Kind: k.kind, FromRepo: k.fromRepo, ToRepo: k.toRepo, Count: c,
		})
	}
	return out
}

// FileImportCounts is the in-memory reference implementation of
// FileImportAggregator. Iterates the EdgeImports byKind bucket and
// groups by the target file path — coalescing to To-node FilePath
// or, when the indexer pointed the import edge at the file node
// directly, the target ID. Same algorithm as the AllEdges loop in
// mostImportedFiles; the win lives in disk backends where AllEdges
// + per-edge GetNode round-trips over cgo dwarf the few hundred
// surviving rows.
//
// scope, when non-nil, bounds the result to edges whose target ID
// lies in the slice (session-workspace clamp). A nil scope counts
// every imports edge. An empty (non-nil) scope returns nil — never
// a whole-graph scan.
func (g *Graph) FileImportCounts(scope []string) []FileImportCountRow {
	if scope != nil && len(scope) == 0 {
		return nil
	}
	var allowed map[string]struct{}
	if scope != nil {
		allowed = make(map[string]struct{}, len(scope))
		for _, id := range scope {
			if id == "" {
				continue
			}
			allowed[id] = struct{}{}
		}
		if len(allowed) == 0 {
			return nil
		}
	}
	counts := map[string]int{}
	for e := range g.EdgesByKind(EdgeImports) {
		if e == nil {
			continue
		}
		target := g.GetNode(e.To)
		if target == nil {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[target.ID]; !ok {
				continue
			}
		}
		path := target.FilePath
		if path == "" {
			path = target.ID
		}
		if path == "" {
			continue
		}
		counts[path]++
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]FileImportCountRow, 0, len(counts))
	for p, c := range counts {
		out = append(out, FileImportCountRow{FilePath: p, Count: c})
	}
	return out
}

// SetEdgeProvenanceBatch is the batched sibling of SetEdgeProvenance.
// Same story as ReindexEdges: per-call in memory, one transaction in
// the disk backends. Returns the number of edges whose Origin
// actually changed (matches the sum of per-edge SetEdgeProvenance
// boolean returns).
func (g *Graph) SetEdgeProvenanceBatch(batch []EdgeProvenanceUpdate) int {
	changed := 0
	for _, u := range batch {
		if u.Edge == nil {
			continue
		}
		if g.SetEdgeProvenance(u.Edge, u.NewOrigin) {
			changed++
		}
	}
	return changed
}

// shardIdx picks the shard index for an ID using FNV-1a. Inlined to
// avoid the per-call hash-object allocation that the stdlib's
// fnv.New32a() incurs — shardIdx is on the hottest path in the graph
// (every AddNode / AddEdge / GetNode call), and the heap profile shows
// 690 MB/30 s of fnv state allocations during cold-start indexing.
// shardMask is shardCount-1 and shardCount is a power of two, so
// h & shardMask is an exact, branch-free modulo.
func (g *Graph) shardIdx(id string) int {
	var h uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		h ^= uint32(id[i])
		h *= 16777619
	}
	return int(h & g.shardMask)
}

// shardFor returns the shard that owns the given ID.
func (g *Graph) shardFor(id string) *shard {
	return g.shards[g.shardIdx(id)]
}

// lockTwoWrite locks two shards for write in ascending index order to
// prevent deadlock. If both IDs land in the same shard, the mutex is
// locked exactly once. Returns a closure the caller defers to unlock.
func (g *Graph) lockTwoWrite(idA, idB string) func() {
	a := g.shardIdx(idA)
	b := g.shardIdx(idB)
	if a == b {
		s := g.shards[a]
		s.mu.Lock()
		return s.mu.Unlock
	}
	lo, hi := a, b
	if lo > hi {
		lo, hi = hi, lo
	}
	sLo := g.shards[lo]
	sHi := g.shards[hi]
	sLo.mu.Lock()
	sHi.mu.Lock()
	return func() {
		sHi.mu.Unlock()
		sLo.mu.Unlock()
	}
}

// lockThreeWrite locks up to three shards for write in ascending index
// order, deduplicating any that collide. Used by ReindexEdge, which
// mutates the From shard's outEdgeIdx plus both in-edge buckets when a
// resolver step retargets an edge; missing any of the three races with
// concurrent AddEdge on that shard (bug: "concurrent map read and map
// write" in addEdgeToBucket).
func (g *Graph) lockThreeWrite(idA, idB, idC string) func() {
	a, b, c := g.shardIdx(idA), g.shardIdx(idB), g.shardIdx(idC)
	// Sort (a, b, c) ascending without allocating a slice.
	if a > b {
		a, b = b, a
	}
	if b > c {
		b, c = c, b
	}
	if a > b {
		a, b = b, a
	}
	// Dedupe: lock each distinct index once.
	idxs := [3]int{a, -1, -1}
	n := 1
	if b != a {
		idxs[n] = b
		n++
	}
	if c != b && c != a {
		idxs[n] = c
		n++
	}
	for i := 0; i < n; i++ {
		g.shards[idxs[i]].mu.Lock()
	}
	return func() {
		for i := n - 1; i >= 0; i-- {
			g.shards[idxs[i]].mu.Unlock()
		}
	}
}

// lockFourWrite locks the source and target shards for a complete edge
// identity move. ReindexEdge only changes To and needs three shards;
// returns_to materialization can also change From and therefore needs four.
func (g *Graph) lockFourWrite(idA, idB, idC, idD string) func() {
	idxs := [4]int{
		g.shardIdx(idA), g.shardIdx(idB),
		g.shardIdx(idC), g.shardIdx(idD),
	}
	// Four-element insertion sort stays allocation-free and makes lock order
	// deterministic across every source/target collision shape.
	for i := 1; i < len(idxs); i++ {
		for j := i; j > 0 && idxs[j] < idxs[j-1]; j-- {
			idxs[j], idxs[j-1] = idxs[j-1], idxs[j]
		}
	}
	n := 1
	for i := 1; i < len(idxs); i++ {
		if idxs[i] != idxs[n-1] {
			idxs[n] = idxs[i]
			n++
		}
	}
	for i := 0; i < n; i++ {
		g.shards[idxs[i]].mu.Lock()
	}
	return func() {
		for i := n - 1; i >= 0; i-- {
			g.shards[idxs[i]].mu.Unlock()
		}
	}
}

// lockAllWrite / lockAllRead take every shard's lock in order. Used by
// operations that have to touch the whole graph (AllNodes, Stats,
// EvictRepo). Callers must match with unlockAllWrite / unlockAllRead.
func (g *Graph) lockAllWrite() {
	for _, s := range g.shards {
		s.mu.Lock()
	}
}

func (g *Graph) unlockAllWrite() {
	for i := len(g.shards) - 1; i >= 0; i-- {
		g.shards[i].mu.Unlock()
	}
}

func (g *Graph) lockAllRead() {
	for _, s := range g.shards {
		s.mu.RLock()
	}
}

func (g *Graph) unlockAllRead() {
	for i := len(g.shards) - 1; i >= 0; i-- {
		g.shards[i].mu.RUnlock()
	}
}

// AddNode inserts or updates a node in the graph and all secondary
// indexes. Idempotent — a second call with the same ID replaces the
// existing Node pointer in place instead of appending duplicates to the
// byFile / byName / byRepo slices. If the new Node's FilePath, Name, or
// RepoPrefix differs from the stored one, the secondary-index entries
// are migrated from the old bucket to the new one atomically under the
// shard lock.
func (g *Graph) AddNode(n *Node) {
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	s := g.shardFor(n.ID)
	s.mu.Lock()
	old := s.nodes[n.ID]
	g.addNodeLocked(s, n)
	s.mu.Unlock()
	g.nodeMutGen.Add(1)

	if old == nil {
		g.recordAddedNodeForReceipts(n, IsReferenceableSymbol(n.Kind), true)
		return
	}
	// Replacing an existing definition with a different identity surface is
	// uncommon in deferred passes and cannot be represented as an add-only
	// receipt. Fail closed rather than claiming a precise frontier.
	if old.Name != n.Name || old.QualName != n.QualName || old.FilePath != n.FilePath || old.Kind != n.Kind || old.RepoPrefix != n.RepoPrefix {
		g.markMutationReceiptsIncomplete()
	}
}

// addNodeLocked is AddNode's body, expecting the caller to already hold
// the shard's write lock. Used by AddBatch to amortise lock acquisition
// across many node inserts targeting the same shard.
func (g *Graph) addNodeLocked(s *shard, n *Node) {
	prev, hadPrev := s.nodes[n.ID]
	// Subtract the previous size/count before overwriting; the new
	// node's contribution is re-added after the RepoPrefix-preservation
	// logic below has settled on the final prefix.
	if hadPrev {
		s.repoNodeRemove(prev)
	}
	s.nodes[n.ID] = n

	if hadPrev {
		if prev.FilePath != n.FilePath {
			removeNodeFromBucket(s.byFile, s.byFileIdx, prev.FilePath, n.ID)
		}
		if prev.Name != n.Name {
			removeNodeFromBucket(s.byName, s.byNameIdx, prev.Name, n.ID)
		}
		if prev.QualName != n.QualName && prev.QualName != "" {
			// byQual is a 1:1 index, not a slice — only delete when
			// the stored entry still points at this node ID (a
			// different node may have since taken the slot).
			if cur, ok := s.byQual[prev.QualName]; ok && cur.ID == n.ID {
				delete(s.byQual, prev.QualName)
			}
		}
		if prev.RepoPrefix != n.RepoPrefix {
			// Preserve a previously-set RepoPrefix rather than letting
			// an empty-prefix re-add silently strip the node out of its
			// byRepo bucket. The downgrade-to-empty case has no
			// legitimate caller (contract nodes use distinct IDs that
			// never collide with symbol IDs; the parse and IncrementalReindexPaths
			// paths route through applyRepoPrefix which always stamps
			// the active idx.repoPrefix on the new node) and previously
			// caused per-repo `byRepo[prefix]` to drain mid-warmup,
			// breaking RepoStats / RepoMemoryEstimate / GetRepoNodes.
			// Restore the old prefix on the new node so the bucket
			// stays populated. The legitimate RepoPrefix-change case
			// (snapshot prefix → new prefix because config moved) still
			// works because n.RepoPrefix is non-empty there.
			if prev.RepoPrefix != "" && n.RepoPrefix == "" {
				n.RepoPrefix = prev.RepoPrefix
			} else {
				removeNodeFromBucket(s.byRepo, s.byRepoIdx, prev.RepoPrefix, n.ID)
			}
		}
	}

	addNodeToBucket(s.byFile, s.byFileIdx, n.FilePath, n.ID, n)
	addNodeToBucket(s.byName, s.byNameIdx, n.Name, n.ID, n)
	if n.QualName != "" {
		s.byQual[n.QualName] = n
	}
	// Empty-prefix nodes are the single-repo/shadow-graph case. Keeping every
	// such node in byRepo duplicates the full node set and adds an ID→position
	// map entry per node, which is substantial during SQLite cold indexing.
	// The only empty-prefix compound query scans the shard's native node map
	// directly; byRepo remains a compact index for real non-empty prefixes.
	if n.RepoPrefix != "" {
		addNodeToBucket(s.byRepo, s.byRepoIdx, n.RepoPrefix, n.ID, n)
	}
	s.repoNodeAdd(n)
}

// AddBatch inserts a set of nodes and edges in shard-grouped passes,
// acquiring each involved shard's write lock at most once across the
// whole batch. Replaces the O(N + 2E) per-item lock acquisitions of
// AddNode / AddEdge with O(distinct_shards) — typically ~16 instead of
// ~450 per per-file worker batch. The contention profile measured 69
// of 102 goroutines blocked on lockTwoWrite during cold-start parsing;
// batching is the throughput fix.
//
// Observable semantics differ slightly from a sequence of AddNode /
// AddEdge calls: for cross-shard edges, the From shard's outEdges
// receives the edge before the To shard's inEdges does. Readers that
// query one side only see the change atomically per shard; readers
// that join both sides may briefly see an outgoing edge whose
// reciprocal in-edge hasn't landed yet. The parser worker path that
// drives this is followed by ResolveAll / global derivation passes
// that take the resolver mutex graph-wide, so no concurrent reader is
// expected to depend on cross-side atomicity during warmup.
func (g *Graph) AddBatch(nodes []*Node, edges []*Edge) {
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	// Structural-shape backstop: the first rejecting boundary owns the event.
	var rejected []*Edge
	edges, rejected = FilterStructuralEdgeViolations(edges)
	g.recordStructuralRejections(StructuralPathGraphAddBatch, rejected, nodes)
	// Lazy builtin-sentinel materialization: see BuiltinStubNodes. The
	// per-store seen-set keeps it one upsert per stub per store lifetime.
	if stubs := BuiltinStubNodes(edges); len(stubs) > 0 {
		var fresh []*Node
		for _, stub := range stubs {
			if _, dup := g.builtinSeen.LoadOrStore(stub.ID, struct{}{}); !dup {
				fresh = append(fresh, stub)
			}
		}
		if len(fresh) > 0 {
			nodes = append(append(make([]*Node, 0, len(nodes)+len(fresh)), nodes...), fresh...)
		}
	}
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	nodesByShard := make([][]*Node, g.shardCount)
	outEdgesByShard := make([][]*Edge, g.shardCount)
	inEdgesByShard := make([][]*Edge, g.shardCount)
	nodeMutation := false
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		nodeMutation = true
		i := g.shardIdx(n.ID)
		nodesByShard[i] = append(nodesByShard[i], n)
	}
	for _, e := range edges {
		if e == nil {
			continue
		}
		outEdgesByShard[g.shardIdx(e.From)] = append(outEdgesByShard[g.shardIdx(e.From)], e)
		inEdgesByShard[g.shardIdx(e.To)] = append(inEdgesByShard[g.shardIdx(e.To)], e)
	}

	var addedNodes []*Node
	var addedEdges []*Edge
	incompleteReceipt := false
	replacedEdge := false
	for i := range g.shards {
		if len(nodesByShard[i]) == 0 && len(outEdgesByShard[i]) == 0 && len(inEdgesByShard[i]) == 0 {
			continue
		}
		s := g.shards[i]
		s.mu.Lock()
		for _, n := range nodesByShard[i] {
			old := s.nodes[n.ID]
			g.addNodeLocked(s, n)
			if old == nil {
				addedNodes = append(addedNodes, n)
			} else if old.Name != n.Name || old.QualName != n.QualName || old.FilePath != n.FilePath || old.Kind != n.Kind || old.RepoPrefix != n.RepoPrefix {
				incompleteReceipt = true
			}
		}
		// Out-side writes own the "was this a new insert?" signal that
		// drives the per-repo edge counter and the "did Origin change?"
		// signal that drives the identity-revision counter — the in-side
		// write is bookkeeping only and charges neither (mirrors
		// AddEdge's behaviour).
		for _, e := range outEdgesByShard[i] {
			var old *Edge
			h := hashEdgeKey(keyOf(e))
			if positions := s.outEdgeIdx[e.From]; positions != nil {
				if pos, ok := positions[h]; ok && pos < len(s.outEdges[e.From]) {
					old = s.outEdges[e.From][pos]
				}
			}
			inserted, originChanged := addEdgeToBucket(s.outEdges, s.outEdgeKeys, s.outEdgeIdx, e.From, e)
			if originChanged {
				g.edgeIdentityRevisions.Add(1)
			}
			if inserted {
				g.edgeMutGen.Add(1)
				addedEdges = append(addedEdges, e)
				var srcRepo string
				if src, ok := s.nodes[e.From]; ok && src != nil {
					srcRepo = src.RepoPrefix
				}
				s.repoEdgeAdd(srcRepo, e)
			} else {
				// One revision per batch is sufficient for same-key payload
				// replacements; inserted edges already advance edgeMutGen above.
				replacedEdge = true
				if old != nil && old.Alias != e.Alias {
					incompleteReceipt = true
				}
			}
		}
		for _, e := range inEdgesByShard[i] {
			addEdgeToBucket(s.inEdges, s.inEdgeKeys, s.inEdgeIdx, e.To, e)
		}
		s.mu.Unlock()
	}

	if nodeMutation {
		g.nodeMutGen.Add(1)
	}
	if replacedEdge {
		g.edgeMutGen.Add(1)
	}
	if incompleteReceipt {
		g.markMutationReceiptsIncomplete()
	}
	for _, n := range addedNodes {
		g.recordAddedNodeForReceipts(n, IsReferenceableSymbol(n.Kind), true)
	}
	for _, e := range addedEdges {
		file := e.FilePath
		if file == "" {
			if src := g.GetNode(e.From); src != nil {
				file = src.FilePath
			}
		}
		g.recordAddedEdgeForReceipts(e, file)
	}
}

// AddEdge inserts or updates a directed edge in the graph. Locks both
// the From and To shards (same shard locked once if they collide) so
// outEdges and inEdges stay consistent. Idempotent: a second call with
// the same (From, To, Kind, FilePath, Line) replaces the stored *Edge
// pointer in place — newer metadata (Confidence, Origin, etc.) wins,
// adjacency-list length is unchanged. Drops the double-edge problem
// that used to surface after daemon restarts (bug B1).
func (g *Graph) AddEdge(e *Edge) {
	if e == nil {
		return
	}
	// Structural-shape backstop: the first rejecting boundary owns the event.
	if StructuralEdgeTargetInvalid(e.Kind, e.To) {
		g.recordStructuralRejections(StructuralPathGraphAddEdge, []*Edge{e}, nil)
		return
	}
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	unlock := g.lockTwoWrite(e.From, e.To)
	sFrom := g.shardFor(e.From)
	sTo := g.shardFor(e.To)
	var old *Edge
	h := hashEdgeKey(keyOf(e))
	if positions := sFrom.outEdgeIdx[e.From]; positions != nil {
		if pos, ok := positions[h]; ok && pos < len(sFrom.outEdges[e.From]) {
			old = sFrom.outEdges[e.From][pos]
		}
	}
	// Only charge the source-repo counter on a brand-new insert.
	// Idempotent re-adds (same edgeKey) replace the slot in place and
	// would otherwise double-count. addEdgeToBucket reports inserted ==
	// true only when it actually appended. The out-side write owns both
	// signals: the new-insert flag (per-repo counter) and the origin-
	// changed flag (identity-revision counter). The in-side write is
	// bookkeeping only — charging either counter there would double it.
	inserted, originChanged := addEdgeToBucket(sFrom.outEdges, sFrom.outEdgeKeys, sFrom.outEdgeIdx, e.From, e)
	addEdgeToBucket(sTo.inEdges, sTo.inEdgeKeys, sTo.inEdgeIdx, e.To, e)
	if originChanged {
		// A re-add with the same logical key but an upgraded Origin:
		// the old identity is retired, a new one created.
		g.edgeIdentityRevisions.Add(1)
	}
	if inserted {
		g.edgeMutGen.Add(1)
		var srcRepo string
		if src, ok := sFrom.nodes[e.From]; ok && src != nil {
			srcRepo = src.RepoPrefix
		}
		sFrom.repoEdgeAdd(srcRepo, e)
	} else {
		// A same-key re-add replaces the persisted payload in place. It must
		// invalidate resolver liveness snapshots even when the endpoints and
		// kind did not change (for example watcher metadata replacement).
		g.edgeMutGen.Add(1)
	}
	unlock()

	if inserted {
		file := e.FilePath
		if file == "" {
			if src := g.GetNode(e.From); src != nil {
				file = src.FilePath
			}
		}
		g.recordAddedEdgeForReceipts(e, file)
	} else if old != nil && old.Alias != e.Alias {
		g.markMutationReceiptsIncomplete()
	}
}

// SetEdgeProvenance changes the Origin of an edge already in the graph
// and is the only sanctioned way to do so. Conceptually it is a
// delete-then-insert of the edge's identity: because Origin is part of
// IdentityHash, the old provenance-bearing identity is retired and a
// new one created — even though the logical (From,To,Kind,FilePath,
// Line) key, and therefore the adjacency-list slot, is unchanged.
//
// It computes the edge's old and new IdentityHash. When they are equal
// (newOrigin matches the current Origin) nothing changes and it returns
// false. When they differ it applies e.Origin = newOrigin, re-derives
// the Origin-derived Tier label when one was set (Confidence and
// ConfidenceLabel are score-derived, not Origin-derived, so they are
// left intact), increments the graph-level identity-revision counter,
// and returns true.
//
// Mutating Edge.Origin directly on an in-graph edge bypasses the
// counter and is a provenance-tampering bug — route every such change
// here so the churn stays observable via EdgeIdentityRevisions.
func (g *Graph) SetEdgeProvenance(e *Edge, newOrigin string) bool {
	if e == nil {
		return false
	}
	unlock := g.lockTwoWrite(e.From, e.To)
	defer unlock()
	oldIdentity := e.IdentityHash()
	newIdentity := hashEdgeIdentity(keyOf(e), newOrigin)
	if oldIdentity == newIdentity {
		return false
	}
	e.Origin = newOrigin
	// Tier is a pure projection of Origin (graph.ResolvedBy). Re-derive
	// it only when it was already populated — an empty Tier is the
	// in-memory default and re-deriving would silently start stamping
	// it. The same *Edge pointer lives in both the out- and in-edge
	// buckets, so this write is visible from every adjacency view.
	if e.Tier != "" {
		e.Tier = ResolvedBy(newOrigin)
	}
	g.edgeIdentityRevisions.Add(1)
	g.edgeMutGen.Add(1)
	return true
}

// EdgeIdentityRevisions returns how many times an in-graph edge's
// provenance-bearing identity has changed over this graph's lifetime —
// the running total fed by SetEdgeProvenance and by AddEdge's in-place
// re-add path. It is monotonic and never decremented; a reindex that
// retires and recreates edges does not roll it back. Surfaced through
// graph_stats as the tamper-evidence signal for provenance churn.
func (g *Graph) EdgeIdentityRevisions() int {
	return int(g.edgeIdentityRevisions.Load())
}

// VerifyEdgeIdentities walks every edge and confirms its provenance-
// bearing identity is internally consistent: the edge stored in a
// source node's outEdges bucket and the edge stored in the target
// node's inEdges bucket are the same *Edge pointer and therefore agree
// on IdentityHash. addEdgeToBucket stores one shared pointer in both
// buckets, so a consistent graph always passes; a divergence means
// some code mutated Origin on a copied edge (e.g. a resolver clone)
// and wrote it into only one adjacency view, leaving the two sides
// disagreeing about provenance. Returns nil when every edge is
// consistent, or an error naming the first divergent edge.
//
// This is the assertion a test uses to prove provenance cannot be
// silently changed outside SetEdgeProvenance.
func (g *Graph) VerifyEdgeIdentities() error {
	g.lockAllRead()
	defer g.unlockAllRead()
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			for _, e := range edges {
				if e == nil {
					continue
				}
				want := e.IdentityHash()
				sTo := g.shardFor(e.To)
				found := false
				for _, in := range sTo.inEdges[e.To] {
					if in == e {
						if in.IdentityHash() != want {
							return &edgeIdentityError{edge: e, reason: "inEdges pointer disagrees on identity hash"}
						}
						found = true
						break
					}
				}
				if !found {
					return &edgeIdentityError{edge: e, reason: "outEdges edge missing from target inEdges bucket"}
				}
			}
		}
	}
	return nil
}

// edgeIdentityError reports the first edge VerifyEdgeIdentities found
// to be inconsistent across the out- and in-edge adjacency views.
type edgeIdentityError struct {
	edge   *Edge
	reason string
}

func (e *edgeIdentityError) Error() string {
	return "edge identity inconsistent (" + e.reason + "): " +
		e.edge.From + " -" + string(e.edge.Kind) + "-> " + e.edge.To
}

// ReindexEdge updates the inEdges index after an edge's To field has
// been mutated (e.g., by the resolver changing "unresolved::X" to a
// real target). oldTo is the previous value of e.To before mutation.
//
// Three sidecar entries change: the outEdgeIdx key for From (since the
// edgeKey depends on To), the inEdgeIdx entry on the old target bucket
// (removed), and the inEdgeIdx entry on the new target bucket (added).
func (g *Graph) ReindexEdge(e *Edge, oldTo string) {
	g.reindexEdge(e, oldTo, e.Kind)
}

func (g *Graph) reindexEdge(e *Edge, oldTo string, oldKind EdgeKind) {
	if oldTo == e.To && oldKind == e.Kind {
		return
	}
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	if receiptActive {
		// Mirror the SQLite reindex recorder: only a write that leaves the
		// edge at an unresolved target creates resolver work; replacing a
		// stub with a resolved target creates none. The source-node lookup
		// happens before the shard write locks below.
		g.recordReindexedEdgeForReceipts(e)
	}
	// Must lock the From shard too — we mutate sFrom.outEdgeIdx below,
	// and without its lock a concurrent AddEdge on From panics the
	// runtime with "concurrent map read and map write".
	unlock := g.lockThreeWrite(e.From, oldTo, e.To)
	defer unlock()

	// Old identity uses oldTo; the current edge struct already has the
	// new To set, so we reconstruct the key before mutation.
	oldKey := hashEdgeKey(edgeKey{From: e.From, To: oldTo, Kind: oldKind, FilePath: e.FilePath, Line: e.Line})
	newKey := hashEdgeKey(keyOf(e))

	sFrom := g.shardFor(e.From)
	// outEdges slot position doesn't move — only the key under which
	// the sidecar records it changes. Avoid a churn of slice growth by
	// swapping the sidecar entry in place.
	//
	// The parallel outEdgeKeys slice MUST be updated alongside
	// outEdgeIdx. removeEdgeFromBucket reads outEdgeKeys[pos] to
	// learn the swapped slot's insertion-time key during swap-with-
	// last; leaving outEdgeKeys stale here would re-insert the old
	// key into outEdgeIdx pointing at a swapped position, and the
	// next swap on that key would compute a pos past the (now
	// shorter) slice — the exact index-out-of-range panic that
	// surfaces during evictEdgesLocked when warmup retargets a lot
	// of edges via ReindexEdge.
	if fromIdx, ok := sFrom.outEdgeIdx[e.From]; ok {
		if pos, exists := fromIdx[oldKey]; exists {
			delete(fromIdx, oldKey)
			fromIdx[newKey] = pos
			if keys, ok := sFrom.outEdgeKeys[e.From]; ok && pos < len(keys) {
				keys[pos] = newKey
			}
		}
	}

	// Move from the old target's inEdges bucket to the new one.
	sOld := g.shardFor(oldTo)
	removeEdgeFromBucket(sOld.inEdges, sOld.inEdgeKeys, sOld.inEdgeIdx, oldTo, oldKey)
	sNew := g.shardFor(e.To)
	addEdgeToBucket(sNew.inEdges, sNew.inEdgeKeys, sNew.inEdgeIdx, e.To, e)
	// No edgeMutGen bump here: outEdges retains the same *Edge slot,
	// and the cached AllEdges slice holds pointers — readers see the
	// already-mutated e.To via that pointer. The cache stays valid.
}

// GetNode returns a node by ID, or nil if not found.
func (g *Graph) GetNode(id string) *Node {
	s := g.shardFor(id)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[id]
}

// GetNodeByQualName returns a node by fully-qualified name, or nil.
// The qual name index is partitioned across shards (each shard owns the
// qual names of the nodes it stores), so we ask every shard.
func (g *Graph) GetNodeByQualName(qualName string) *Node {
	for _, s := range g.shards {
		s.mu.RLock()
		if n, ok := s.byQual[qualName]; ok {
			s.mu.RUnlock()
			return n
		}
		s.mu.RUnlock()
	}
	return nil
}

// GetNodesByQualNames is the multi-valued batch form of
// GetNodeByQualName. It scans each shard's canonical node map once because the
// legacy byQual point index is intentionally single-valued and cannot represent
// valid duplicate qualified names. Missing and empty names are absent.
func (g *Graph) GetNodesByQualNames(qualNames []string) map[string][]*Node {
	requested := make(map[string]struct{}, len(qualNames))
	for _, q := range qualNames {
		if q != "" {
			requested[q] = struct{}{}
		}
	}
	if len(requested) == 0 {
		return nil
	}

	out := make(map[string][]*Node, len(requested))
	for _, s := range g.shards {
		s.mu.RLock()
		for _, n := range s.nodes {
			if n != nil {
				if _, ok := requested[n.QualName]; ok {
					out[n.QualName] = append(out[n.QualName], n)
				}
			}
		}
		s.mu.RUnlock()
	}
	for q := range out {
		sort.Slice(out[q], func(i, j int) bool { return out[q][i].ID < out[q][j].ID })
	}
	return out
}

// FindNodesByName returns all nodes matching the short name.
//
// Implementation walks every shard's byName bucket. The two-pass shape
// (sum then allocate) trades one extra read-lock round trip per shard
// for a single right-sized allocation — the prior single-pass append
// re-grew `out` on every hot shard (1 + log2(N) reallocations), which
// the cold-index heap profile attributed 5.22 GB / 14% of total alloc
// to. Names with a long candidate list (`Visit`, `init`, `create`)
// see the biggest win.
func (g *Graph) FindNodesByName(name string) []*Node {
	total := 0
	for _, s := range g.shards {
		s.mu.RLock()
		total += len(s.byName[name])
		s.mu.RUnlock()
	}
	if total == 0 {
		return nil
	}
	out := make([]*Node, 0, total)
	for _, s := range g.shards {
		s.mu.RLock()
		if src := s.byName[name]; len(src) > 0 {
			out = append(out, src...)
		}
		s.mu.RUnlock()
	}
	return out
}

// FindNodesByNameInRepo returns nodes matching the short name that are
// either in the given repoPrefix or carry an empty RepoPrefix. Equivalent to
// filterSameRepo(repoPrefix, FindNodesByName(name)) but skips the
// intermediate cross-repo candidate slice.
//
// The `n.RepoPrefix == ""` disjunct is a CROSS-REPO VISIBILITY RULE, not a
// single-repo accommodation. Synthetic global externals — `dep::…`,
// `external::…` and non-Go `module::…` nodes — model third-party symbols
// that no repository owns, so a service's external surface can aggregate
// across repos. They are visible from EVERY repo by construction. Dropping
// the disjunct would not narrow a scope; it would make every third-party
// target unresolvable from every repo at once, with nothing reporting it.
//
// When repoPrefix is "" the caller has asked for no repo filter at all, so
// this behaves identically to FindNodesByName.
func (g *Graph) FindNodesByNameInRepo(name, repoPrefix string) []*Node {
	if repoPrefix == "" {
		return g.FindNodesByName(name)
	}
	// First pass: count matches that pass the repo filter. Counting in
	// a separate pass keeps `out` right-sized even when ~95% of the
	// byName bucket lives in unrelated repos.
	total := 0
	for _, s := range g.shards {
		s.mu.RLock()
		for _, n := range s.byName[name] {
			if n.RepoPrefix == "" || n.RepoPrefix == repoPrefix {
				total++
			}
		}
		s.mu.RUnlock()
	}
	if total == 0 {
		return nil
	}
	out := make([]*Node, 0, total)
	for _, s := range g.shards {
		s.mu.RLock()
		for _, n := range s.byName[name] {
			if n.RepoPrefix == "" || n.RepoPrefix == repoPrefix {
				out = append(out, n)
			}
		}
		s.mu.RUnlock()
	}
	return out
}

// FindNodesByNameContaining returns nodes whose Name (case-insensitive)
// contains substr. The in-memory backend has no name-substring index,
// so this is a single pass over the byName buckets (which already group
// nodes by exact name — the same allocation we'd pay for one FindNodesByName
// call per distinct name). limit caps the slice; 0 means "no limit".
//
// Stable order is the caller's responsibility — bucket iteration is
// deterministic per shard but cross-shard order isn't fixed.
func (g *Graph) FindNodesByNameContaining(substr string, limit int) []*Node {
	if substr == "" {
		return nil
	}
	needle := strings.ToLower(substr)
	var out []*Node
	for _, s := range g.shards {
		s.mu.RLock()
		for name, bucket := range s.byName {
			if !strings.Contains(strings.ToLower(name), needle) {
				continue
			}
			out = append(out, bucket...)
			if limit > 0 && len(out) >= limit {
				s.mu.RUnlock()
				return out[:limit]
			}
		}
		s.mu.RUnlock()
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// GetFileNodes returns all nodes defined in the given file.
func (g *Graph) GetFileNodes(filePath string) []*Node {
	var out []*Node
	for _, s := range g.shards {
		s.mu.RLock()
		if src := s.byFile[filePath]; len(src) > 0 {
			out = append(out, src...)
		}
		s.mu.RUnlock()
	}
	return out
}

// GetOutEdges returns outgoing edges for a node.
func (g *Graph) GetOutEdges(nodeID string) []*Edge {
	s := g.shardFor(nodeID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.outEdges[nodeID]
	out := make([]*Edge, len(src))
	copy(out, src)
	return out
}

// GetInEdges returns incoming edges for a node.
func (g *Graph) GetInEdges(nodeID string) []*Edge {
	s := g.shardFor(nodeID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.inEdges[nodeID]
	out := make([]*Edge, len(src))
	copy(out, src)
	return out
}

// GetOutEdgesByNodeIDs returns a map id→outgoing edges for every input
// id. The in-memory backend loops the existing GetOutEdges — cost
// matches a hand-written loop in the caller. The value of the batched
// API lives in the disk backend, where it collapses N point lookups into
// one bulk query. Empty input returns nil; duplicate ids are
// deduped naturally. Missing ids are absent from the returned map.
func (g *Graph) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string][]*Edge, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := out[id]; ok {
			continue
		}
		out[id] = g.GetOutEdges(id)
	}
	return out
}

// GetInEdgesByNodeIDs is the inbound sibling of GetOutEdgesByNodeIDs.
// See that doc-comment for the contract.
func (g *Graph) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string][]*Edge, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := out[id]; ok {
			continue
		}
		out[id] = g.GetInEdges(id)
	}
	return out
}

// EvictFile removes all nodes and edges belonging to the given file
// path. Nodes for one file can span many shards (different IDs hash
// differently), so we lock all shards for this multi-shard operation.
func (g *Graph) EvictFile(filePath string) (nodesRemoved, edgesRemoved int) {
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	g.lockAllWrite()
	defer g.unlockAllWrite()

	// Gather nodes across shards.
	var nodes []*Node
	for _, s := range g.shards {
		nodes = append(nodes, s.byFile[filePath]...)
	}
	if len(nodes) == 0 {
		return 0, 0
	}
	// id → source-repo captured BEFORE we delete the node from
	// s.nodes; evictEdgesLocked needs the repo to debit per-repo
	// edge counters and the live node would already be gone.
	evictedIDs := make(map[string]string, len(nodes))
	for _, n := range nodes {
		evictedIDs[n.ID] = n.RepoPrefix
	}
	// See EvictFiles: the edges this eviction destroys from surviving
	// sources are not reconstructible by any resolution pass, so failing the
	// receipt closed over them would force a fallback that reaches the same
	// graph. The RESOLUTION delta stays exactly describable.
	g.recordEvictedNodesForReceipts(nodes)

	for _, n := range nodes {
		s := g.shardFor(n.ID)
		s.repoNodeRemove(n)
		delete(s.nodes, n.ID)
		if n.QualName != "" {
			if cur, ok := s.byQual[n.QualName]; ok && cur.ID == n.ID {
				delete(s.byQual, n.QualName)
			}
		}
		removeNodeFromBucket(s.byName, s.byNameIdx, n.Name, n.ID)
		removeNodeFromBucket(s.byFile, s.byFileIdx, filePath, n.ID)
		removeNodeFromBucket(s.byRepo, s.byRepoIdx, n.RepoPrefix, n.ID)
	}
	nodesRemoved = len(nodes)
	g.nodeMutGen.Add(1)

	edgesRemoved = g.evictEdgesLocked(evictedIDs)
	return nodesRemoved, edgesRemoved
}

// evictEdgesLocked is the shared edge-removal core used by EvictFile
// and EvictRepo. Callers must hold every shard's write lock.
//
// For each evicted node we remove its outEdges and inEdges entries. To
// clean the reverse index on non-evicted endpoints we do a swap-with-
// last removal via sidecar, which is O(1) per edge instead of the
// O(slice-size) filterEdge scan the older implementation used.
func (g *Graph) evictEdgesLocked(evictedIDs map[string]string) int {
	removed := 0
	defer func() {
		if removed > 0 {
			g.edgeMutGen.Add(1)
		}
	}()

	// Phase 1: remove outgoing edges from every evicted node. Use the
	// parallel outEdgeKeys slice to look up each entry's insertion-time
	// edgeKey rather than recomputing keyOf — the latter races with
	// resolver-driven Edge.To mutations elsewhere in the graph and
	// can yield a key that doesn't match the inEdges sidecar.
	for id, srcRepo := range evictedIDs {
		s := g.shardFor(id)
		edges := s.outEdges[id]
		keys := s.outEdgeKeys[id]
		removed += len(edges)
		for i, e := range edges {
			s.repoEdgeRemove(srcRepo, e)
			if _, evicted := evictedIDs[e.To]; !evicted {
				sTo := g.shardFor(e.To)
				removeEdgeFromBucket(sTo.inEdges, sTo.inEdgeKeys, sTo.inEdgeIdx, e.To, keys[i])
			}
		}
		delete(s.outEdges, id)
		delete(s.outEdgeKeys, id)
		delete(s.outEdgeIdx, id)
	}

	// Phase 2: remove incoming edges to every evicted node (from
	// non-evicted sources — same-direction edges were already handled
	// in phase 1 and counted). Each surviving source's repo is read
	// from its live node — sFrom.nodes[e.From] is still present in
	// this phase because that source wasn't evicted.
	for id := range evictedIDs {
		s := g.shardFor(id)
		edges := s.inEdges[id]
		keys := s.inEdgeKeys[id]
		for i, e := range edges {
			if _, evicted := evictedIDs[e.From]; !evicted {
				removed++
				sFrom := g.shardFor(e.From)
				var srcRepo string
				if src, ok := sFrom.nodes[e.From]; ok && src != nil {
					srcRepo = src.RepoPrefix
				}
				removeEdgeFromBucket(sFrom.outEdges, sFrom.outEdgeKeys, sFrom.outEdgeIdx, e.From, keys[i])
				sFrom.repoEdgeRemove(srcRepo, e)
			}
		}
		delete(s.inEdges, id)
		delete(s.inEdgeKeys, id)
		delete(s.inEdgeIdx, id)
	}

	return removed
}

// RemoveEdge removes a specific edge by from, to, and kind. Returns
// true if the edge was found and removed. When multiple edges match
// (same from/to/kind but different file/line — rare but possible),
// removes the first one encountered.
func (g *Graph) RemoveEdge(from, to string, kind EdgeKind) bool {
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	g.markMutationReceiptsIncomplete()
	unlock := g.lockTwoWrite(from, to)
	defer unlock()

	sFrom := g.shardFor(from)
	outList := sFrom.outEdges[from]
	outKeys := sFrom.outEdgeKeys[from]
	targetIdx := -1
	for i, e := range outList {
		if e.To == to && e.Kind == kind {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return false
	}

	// Snapshot the edge plus its source-repo before mutating the
	// buckets — once removeEdgeFromBucket swaps the tail in, outList[i]
	// is no longer the edge we're removing.
	removed := outList[targetIdx]
	var srcRepo string
	if src, ok := sFrom.nodes[from]; ok && src != nil {
		srcRepo = src.RepoPrefix
	}

	// Use the stored insertion-time key rather than keyOf(target) so
	// removal is robust against in-flight Edge.To mutations.
	k := outKeys[targetIdx]
	removeEdgeFromBucket(sFrom.outEdges, sFrom.outEdgeKeys, sFrom.outEdgeIdx, from, k)
	sTo := g.shardFor(to)
	removeEdgeFromBucket(sTo.inEdges, sTo.inEdgeKeys, sTo.inEdgeIdx, to, k)
	sFrom.repoEdgeRemove(srcRepo, removed)
	g.edgeMutGen.Add(1)
	return true
}

// NodeCount returns the total number of nodes.
func (g *Graph) NodeCount() int {
	total := 0
	for _, s := range g.shards {
		s.mu.RLock()
		total += len(s.nodes)
		s.mu.RUnlock()
	}
	return total
}

// EdgeCount returns the total number of edges.
func (g *Graph) EdgeCount() int {
	total := 0
	for _, s := range g.shards {
		s.mu.RLock()
		for _, edges := range s.outEdges {
			total += len(edges)
		}
		s.mu.RUnlock()
	}
	return total
}

// AllNodes returns a snapshot of all nodes. Locks every shard for read
// to produce a coherent view — callers use this for snapshots,
// contracts extraction, etc. where a consistent crop matters.
func (g *Graph) AllNodes() []*Node {
	g.lockAllRead()
	defer g.unlockAllRead()
	total := 0
	for _, s := range g.shards {
		total += len(s.nodes)
	}
	out := make([]*Node, 0, total)
	for _, s := range g.shards {
		for _, n := range s.nodes {
			out = append(out, n)
		}
	}
	return out
}

// AllEdges returns a snapshot of all outgoing edges across every shard.
//
// Cached by the edgeMutGen counter: once built, subsequent calls
// return the same slice pointer as long as no edge has been
// inserted, removed, or had its slot pointer replaced. Mutations
// bump edgeMutGen, the next AllEdges sees the mismatch and rebuilds.
//
// On a 4 M-edge graph (k8s) one snapshot is ~32 MB of pointer slice;
// the post-resolve analysis fan-out (cycles, communities, deadcode,
// hierarchy, pagerank, hits, betweenness, several MCP analyzers) used
// to call this dozens of times per cold-index, all allocating fresh
// — 2.72 GB / 8 % of total in the heap profile. Caching collapses
// that to a single allocation per generation.
//
// Callers MUST treat the returned slice as read-only. Mutating its
// pointers is a data race against any concurrent reader holding the
// same cached reference. The underlying *Edge structs are themselves
// shared with the graph and may be mutated by ReindexEdge /
// SetEdgeProvenance — that's intentional, and readers see those
// mutations through the pointer.
func (g *Graph) AllEdges() []*Edge {
	curGen := g.edgeMutGen.Load()
	g.allEdgesCacheMu.Lock()
	defer g.allEdgesCacheMu.Unlock()
	if g.allEdgesCache != nil && g.allEdgesCacheGen == curGen {
		return g.allEdgesCache
	}

	g.lockAllRead()
	// Pre-size from the per-shard outEdges entry counts. EdgeCount
	// would re-lock; just sum inline while holding the locks.
	total := 0
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			total += len(edges)
		}
	}
	out := make([]*Edge, 0, total)
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			out = append(out, edges...)
		}
	}
	g.unlockAllRead()

	g.allEdgesCache = out
	g.allEdgesCacheGen = curGen
	return out
}

// DrainNodes yields every node and FREES the graph's internal node
// storage shard-by-shard as it goes. After Drain finishes the graph
// holds zero nodes. Intended for the one-shot persist path where the
// shadow is about to be discarded: AllNodes would pin the full 11 GB
// graph for the entire persist phase; Drain releases each shard's
// node map (and the per-name / per-file / per-repo indexes) as soon
// as that shard's iteration completes, so GC can reclaim ~700 MB at
// a time on a Linux-scale graph instead of waiting for the indexer's
// defer to return.
//
// The graph remains structurally consistent during Drain — edges and
// other indexes are untouched, only the node maps are emptied. If
// you also need DrainEdges, call them in either order; both are
// destructive and idempotent (a second call yields nothing).
func (g *Graph) DrainNodes() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		for _, s := range g.shards {
			s.mu.Lock()
			nodes := s.nodes
			// Replace with an empty map so the shard's read methods
			// keep working (return zero) instead of nil-panicking.
			s.nodes = map[string]*Node{}
			s.byFile = map[string][]*Node{}
			s.byName = map[string][]*Node{}
			s.byQual = map[string]*Node{}
			s.byRepo = map[string][]*Node{}
			s.byFileIdx = map[string]map[string]int{}
			s.byNameIdx = map[string]map[string]int{}
			s.byRepoIdx = map[string]map[string]int{}
			s.mu.Unlock()
			for _, n := range nodes {
				if !yield(n) {
					return
				}
			}
			// nodes goes out of scope here — the shard's old map plus
			// every *Node it referenced is now GC-eligible (assuming
			// the caller has dropped any remaining reference).
		}
	}
}

// DrainEdges yields every edge and FREES the graph's internal edge
// storage shard-by-shard. Same semantics as DrainNodes — meant for
// the persist hand-off, not for general queries.
func (g *Graph) DrainEdges() iter.Seq[*Edge] {
	// Invalidate the AllEdges cache so any subsequent caller doesn't
	// see drained-shard zombies. The cache holds direct *Edge slice
	// references that DrainEdges is about to start freeing.
	g.allEdgesCacheMu.Lock()
	g.allEdgesCache = nil
	g.allEdgesCacheGen = 0
	g.allEdgesCacheMu.Unlock()
	return func(yield func(*Edge) bool) {
		for _, s := range g.shards {
			s.mu.Lock()
			outEdges := s.outEdges
			s.outEdges = map[string][]*Edge{}
			s.inEdges = map[string][]*Edge{}
			s.outEdgeIdx = map[string]map[edgeHash]int{}
			s.inEdgeIdx = map[string]map[edgeHash]int{}
			s.outEdgeKeys = map[string][]edgeHash{}
			s.inEdgeKeys = map[string][]edgeHash{}
			s.mu.Unlock()
			for _, edges := range outEdges {
				for _, e := range edges {
					if !yield(e) {
						return
					}
				}
			}
		}
	}
}

// DrainNodeBatches destructively transfers nodes out of the graph in bounded
// batches. Each yielded batch is sorted by ID so SQLite's WITHOUT ROWID primary
// key sees locally ordered inserts. The backing node map is detached one shard
// at a time and entries are deleted as they enter a batch: the graph never
// allocates a whole-corpus snapshot, and processed Node objects become
// collectible as soon as the consumer returns. maxBytes is an estimate based
// on nodeBytes; an individual oversized node is yielded alone. A zero byte cap
// disables only the byte limit. Callers must consume the sequence fully.
func (g *Graph) DrainNodeBatches(maxRows int, maxBytes uint64) iter.Seq[[]*Node] {
	if maxRows <= 0 {
		maxRows = 1
	}
	return func(yield func([]*Node) bool) {
		batch := make([]*Node, 0, maxRows)
		var batchBytes uint64
		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			slices.SortFunc(batch, func(a, b *Node) int {
				return strings.Compare(a.ID, b.ID)
			})
			if !yield(batch) {
				return false
			}
			batch = make([]*Node, 0, maxRows)
			batchBytes = 0
			return true
		}

		for _, s := range g.shards {
			s.mu.Lock()
			nodes := s.nodes
			s.nodes = make(map[string]*Node)
			s.byFile = make(map[string][]*Node)
			s.byName = make(map[string][]*Node)
			s.byQual = make(map[string]*Node)
			s.byRepo = make(map[string][]*Node)
			s.byFileIdx = make(map[string]map[string]int)
			s.byNameIdx = make(map[string]map[string]int)
			s.byRepoIdx = make(map[string]map[string]int)
			s.repoNodeBytes = make(map[string]uint64)
			s.repoNodeCount = make(map[string]int)
			s.mu.Unlock()

			for id, n := range nodes {
				delete(nodes, id)
				nextBytes := nodeBytes(n)
				if len(batch) > 0 && (len(batch) >= maxRows || maxBytes > 0 && batchBytes+nextBytes > maxBytes) {
					if !flush() {
						return
					}
				}
				batch = append(batch, n)
				batchBytes += nextBytes
				if len(batch) >= maxRows || maxBytes > 0 && batchBytes >= maxBytes {
					if !flush() {
						return
					}
				}
			}
		}
		flush()
	}
}

// DrainEdgeBatches is the edge sibling of DrainNodeBatches. It drains only
// each edge's canonical out-adjacency entry (in-adjacency holds the same
// pointers), orders every bounded batch by the SQLite logical key, and clears
// the duplicate adjacency/index structures before yielding. An oversized edge
// is yielded alone; maxBytes == 0 disables the byte limit.
func (g *Graph) DrainEdgeBatches(maxRows int, maxBytes uint64) iter.Seq[[]*Edge] {
	if maxRows <= 0 {
		maxRows = 1
	}
	g.allEdgesCacheMu.Lock()
	g.allEdgesCache = nil
	g.allEdgesCacheGen = 0
	g.allEdgesCacheMu.Unlock()
	g.edgeMutGen.Add(1)

	return func(yield func([]*Edge) bool) {
		batch := make([]*Edge, 0, maxRows)
		var batchBytes uint64
		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			slices.SortFunc(batch, compareDrainEdges)
			if !yield(batch) {
				return false
			}
			batch = make([]*Edge, 0, maxRows)
			batchBytes = 0
			return true
		}

		for _, s := range g.shards {
			s.mu.Lock()
			outEdges := s.outEdges
			s.outEdges = make(map[string][]*Edge)
			s.inEdges = make(map[string][]*Edge)
			s.outEdgeIdx = make(map[string]map[edgeHash]int)
			s.inEdgeIdx = make(map[string]map[edgeHash]int)
			s.outEdgeKeys = make(map[string][]edgeHash)
			s.inEdgeKeys = make(map[string][]edgeHash)
			s.repoEdgeBytes = make(map[string]uint64)
			s.repoEdgeCount = make(map[string]int)
			s.mu.Unlock()

			for fromID, edges := range outEdges {
				delete(outEdges, fromID)
				for i, e := range edges {
					edges[i] = nil
					nextBytes := edgeBytes(e)
					if len(batch) > 0 && (len(batch) >= maxRows || maxBytes > 0 && batchBytes+nextBytes > maxBytes) {
						if !flush() {
							return
						}
					}
					batch = append(batch, e)
					batchBytes += nextBytes
					if len(batch) >= maxRows || maxBytes > 0 && batchBytes >= maxBytes {
						if !flush() {
							return
						}
					}
				}
			}
		}
		flush()
	}
}

func compareDrainEdges(a, b *Edge) int {
	if c := strings.Compare(a.From, b.From); c != 0 {
		return c
	}
	if c := strings.Compare(a.To, b.To); c != 0 {
		return c
	}
	if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
		return c
	}
	if c := strings.Compare(a.FilePath, b.FilePath); c != 0 {
		return c
	}
	switch {
	case a.Line < b.Line:
		return -1
	case a.Line > b.Line:
		return 1
	default:
		return 0
	}
}

// DrainFileMetaBatches removes one repository's compact file projection and
// yields bounded, path-sorted batches without materialising FileMetasForRepo.
func (g *Graph) DrainFileMetaBatches(repoPrefix string, maxRows int, maxBytes uint64) iter.Seq[[]FileMetaRow] {
	if maxRows <= 0 {
		maxRows = 1
	}
	return func(yield func([]FileMetaRow) bool) {
		g.fileMetasMu.Lock()
		rows := g.fileMetas[repoPrefix]
		delete(g.fileMetas, repoPrefix)
		g.fileMetasMu.Unlock()

		batch := make([]FileMetaRow, 0, maxRows)
		var batchBytes uint64
		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			slices.SortFunc(batch, func(a, b FileMetaRow) int {
				return strings.Compare(a.FilePath, b.FilePath)
			})
			if !yield(batch) {
				return false
			}
			batch = make([]FileMetaRow, 0, maxRows)
			batchBytes = 0
			return true
		}
		for filePath, row := range rows {
			delete(rows, filePath)
			nextBytes := uint64(len(row.FilePath) + len(row.ContentHash) + len(row.Errors) + 64)
			if len(batch) > 0 && (len(batch) >= maxRows || maxBytes > 0 && batchBytes+nextBytes > maxBytes) {
				if !flush() {
					return
				}
			}
			batch = append(batch, row)
			batchBytes += nextBytes
			if len(batch) >= maxRows || maxBytes > 0 && batchBytes >= maxBytes {
				if !flush() {
					return
				}
			}
		}
		flush()
	}
}

// DrainConstantValueBatches removes one repository's compact constant-value
// projection and yields bounded node-ID-sorted batches.
func (g *Graph) DrainConstantValueBatches(repoPrefix string, maxRows int, maxBytes uint64) iter.Seq[[]ConstantValueRow] {
	if maxRows <= 0 {
		maxRows = 1
	}
	return func(yield func([]ConstantValueRow) bool) {
		g.constValuesMu.Lock()
		owned := g.constValues
		remaining := make(map[string]constValueEntry)
		for nodeID, entry := range owned {
			if entry.repoPrefix != repoPrefix {
				remaining[nodeID] = entry
				delete(owned, nodeID)
			}
		}
		g.constValues = remaining
		g.constValuesMu.Unlock()

		batch := make([]ConstantValueRow, 0, maxRows)
		var batchBytes uint64
		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			slices.SortFunc(batch, func(a, b ConstantValueRow) int {
				return strings.Compare(a.NodeID, b.NodeID)
			})
			if !yield(batch) {
				return false
			}
			batch = make([]ConstantValueRow, 0, maxRows)
			batchBytes = 0
			return true
		}
		for nodeID, entry := range owned {
			delete(owned, nodeID)
			row := ConstantValueRow{NodeID: nodeID, FilePath: entry.filePath, Value: entry.value}
			nextBytes := uint64(len(row.NodeID) + len(row.FilePath) + len(row.Value) + 48)
			if len(batch) > 0 && (len(batch) >= maxRows || maxBytes > 0 && batchBytes+nextBytes > maxBytes) {
				if !flush() {
					return
				}
			}
			batch = append(batch, row)
			batchBytes += nextBytes
			if len(batch) >= maxRows || maxBytes > 0 && batchBytes >= maxBytes {
				if !flush() {
					return
				}
			}
		}
		flush()
	}
}

// Stats returns summary counts by kind and language.
func (g *Graph) Stats() GraphStats {
	g.lockAllRead()
	defer g.unlockAllRead()

	byKind := make(map[string]int)
	byLang := make(map[string]int)
	totalNodes := 0
	for _, s := range g.shards {
		for _, n := range s.nodes {
			// Cross-daemon proxy-edge nodes stand in for symbols a
			// remote daemon owns; they are never counted in local
			// stats. Inert until edge-minting is enabled.
			if IsProxyNode(n) {
				continue
			}
			totalNodes++
			byKind[string(n.Kind)]++
			if n.Language != "" {
				byLang[n.Language]++
			}
		}
	}

	edgeCount := 0
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			edgeCount += len(edges)
		}
	}

	return GraphStats{
		TotalNodes: totalNodes,
		TotalEdges: edgeCount,
		ByKind:     byKind,
		ByLanguage: byLang,
	}
}

// GetRepoNodes returns all nodes belonging to the exact repository prefix.
// Non-empty prefixes use each shard's compact byRepo bucket.
//
// THE EMPTY-PREFIX BRANCH IS LIVE — do not delete it as single-repo-mode
// residue. The daemon always prefixes now, but the STANDALONE Indexer
// (indexer.New with no SetRepoPrefix) does not, and it backs `gortex init`,
// the eval harnesses, bench, pkg/gortex's public API and serverstack's
// embedded indexer. In that shape the whole graph is one unprefixed repo, so
// this scans shard maps directly rather than duplicating every node into a
// byRepo bucket. See TestStandaloneIndexerKeepsTheEmptyPrefix.
func (g *Graph) GetRepoNodes(repoPrefix string) []*Node {
	var out []*Node
	for _, s := range g.shards {
		s.mu.RLock()
		if repoPrefix == "" {
			for _, n := range s.nodes {
				if n != nil && n.RepoPrefix == "" {
					out = append(out, n)
				}
			}
			s.mu.RUnlock()
			continue
		}
		if src := s.byRepo[repoPrefix]; len(src) > 0 {
			out = append(out, src...)
		}
		s.mu.RUnlock()
	}
	return out
}

// GetRepoNodesByLanguage is the exact compound-predicate sibling of
// GetRepoNodes, and its empty-prefix branch is live for the same reason —
// the standalone Indexer. See GetRepoNodes.
func (g *Graph) GetRepoNodesByLanguage(repoPrefix, language string) []*Node {
	if language == "" {
		return nil
	}
	var out []*Node
	for _, s := range g.shards {
		s.mu.RLock()
		candidates := s.byRepo[repoPrefix]
		if repoPrefix == "" {
			for _, n := range s.nodes {
				if n != nil && n.RepoPrefix == "" && n.Language == language {
					out = append(out, n)
				}
			}
			s.mu.RUnlock()
			continue
		}
		for _, n := range candidates {
			if n != nil && n.Language == language {
				out = append(out, n)
			}
		}
		s.mu.RUnlock()
	}
	return out
}

// GetRepoEdges returns every edge whose source node has the given
// RepoPrefix — the in-memory reference implementation of the
// Store-interface method. Walks each shard's byRepo bucket and
// concatenates that node's outEdges in place (no per-node
// GetOutEdges call, so no per-call slice copy). Equivalent in
// observable behaviour to the GetRepoNodes(r) × GetOutEdges loop
// callers used before this method existed; meant to give disk
// backends a single-query hook without changing in-memory cost.
// Empty repoPrefix returns nil (callers use AllEdges() instead).
func (g *Graph) GetRepoEdges(repoPrefix string) []*Edge {
	if repoPrefix == "" {
		return nil
	}
	var out []*Edge
	for _, s := range g.shards {
		s.mu.RLock()
		for _, n := range s.byRepo[repoPrefix] {
			if src := s.outEdges[n.ID]; len(src) > 0 {
				out = append(out, src...)
			}
		}
		s.mu.RUnlock()
	}
	return out
}

// EvictRepo removes all nodes with matching RepoPrefix and all edges
// referencing those nodes. Returns counts of removed nodes and edges.
func (g *Graph) EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	// Empty prefix has never been a public repo bucket (GetRepoNodes("") and
	// RepoPrefixes intentionally exclude it). Preserve the historical no-op
	// contract now that empty-prefix nodes are no longer duplicated in byRepo.
	if repoPrefix == "" {
		return 0, 0
	}
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	g.markMutationReceiptsIncomplete()
	g.lockAllWrite()
	defer g.unlockAllWrite()

	var nodes []*Node
	for _, s := range g.shards {
		nodes = append(nodes, s.byRepo[repoPrefix]...)
	}
	if len(nodes) == 0 {
		return 0, 0
	}
	evictedIDs := make(map[string]string, len(nodes))
	for _, n := range nodes {
		evictedIDs[n.ID] = repoPrefix
	}

	for _, n := range nodes {
		s := g.shardFor(n.ID)
		s.repoNodeRemove(n)
		delete(s.nodes, n.ID)
		if n.QualName != "" {
			if cur, ok := s.byQual[n.QualName]; ok && cur.ID == n.ID {
				delete(s.byQual, n.QualName)
			}
		}
		removeNodeFromBucket(s.byName, s.byNameIdx, n.Name, n.ID)
		removeNodeFromBucket(s.byFile, s.byFileIdx, n.FilePath, n.ID)
		removeNodeFromBucket(s.byRepo, s.byRepoIdx, repoPrefix, n.ID)
	}
	nodesRemoved = len(nodes)
	g.nodeMutGen.Add(1)

	edgesRemoved = g.evictEdgesLocked(evictedIDs)
	return nodesRemoved, edgesRemoved
}

// EvictRepoCurrentGeneration declares the in-memory store's one-generation
// semantics to generation-aware callers.
func (g *Graph) EvictRepoCurrentGeneration(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	return g.EvictRepo(repoPrefix)
}

// EvictRepoAllGenerations declares the destructive capability explicitly. An
// in-memory Graph has exactly one logical generation, so the implementation is
// identical to EvictRepo; its empty-prefix guard is inherited.
func (g *Graph) EvictRepoAllGenerations(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	return g.EvictRepo(repoPrefix)
}

// RepoStats returns per-repository node and edge counts.
func (g *Graph) RepoStats() map[string]GraphStats {
	g.lockAllRead()
	defer g.unlockAllRead()

	// Aggregate byRepo across shards first.
	repoNodes := make(map[string][]*Node)
	for _, s := range g.shards {
		for prefix, nodes := range s.byRepo {
			if prefix == "" {
				continue
			}
			repoNodes[prefix] = append(repoNodes[prefix], nodes...)
		}
	}

	stats := make(map[string]GraphStats, len(repoNodes))
	repoByKind := make(map[string]map[string]int)
	repoByLang := make(map[string]map[string]int)
	repoNodeCount := make(map[string]int)
	for prefix, nodes := range repoNodes {
		repoNodeCount[prefix] = len(nodes)
		byKind := make(map[string]int)
		byLang := make(map[string]int)
		for _, n := range nodes {
			byKind[string(n.Kind)]++
			if n.Language != "" {
				byLang[n.Language]++
			}
		}
		repoByKind[prefix] = byKind
		repoByLang[prefix] = byLang
	}

	// Count edges per repo by the From node's repo. Need to look up the
	// From node in whichever shard owns it.
	repoEdgeCount := make(map[string]int)
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			for _, e := range edges {
				fromShard := g.shardFor(e.From)
				if fromNode, ok := fromShard.nodes[e.From]; ok && fromNode.RepoPrefix != "" {
					repoEdgeCount[fromNode.RepoPrefix]++
				}
			}
		}
	}

	for prefix := range repoNodes {
		stats[prefix] = GraphStats{
			TotalNodes: repoNodeCount[prefix],
			TotalEdges: repoEdgeCount[prefix],
			ByKind:     repoByKind[prefix],
			ByLanguage: repoByLang[prefix],
		}
	}

	return stats
}

// RepoMemoryEstimate is an approximate breakdown of how many bytes a
// single repository's graph contribution occupies. It covers the
// sharded node/edge maps only — search and vector indexes are
// orthogonal and computed elsewhere.
type RepoMemoryEstimate struct {
	NodeBytes uint64 `json:"node_bytes"`
	EdgeBytes uint64 `json:"edge_bytes"`
	NodeCount int    `json:"node_count"`
	EdgeCount int    `json:"edge_count"`
}

// Total returns the saturating sum of NodeBytes and EdgeBytes. Saturation keeps
// an extreme or synthetic estimate conservative instead of wrapping back to a
// small value and bypassing memory-pressure admission.
func (e RepoMemoryEstimate) Total() uint64 {
	return saturatingAddUint64(e.NodeBytes, e.EdgeBytes)
}

func saturatingAddUint64(total, next uint64) uint64 {
	const maxUint64 = ^uint64(0)
	if total > maxUint64-next {
		return maxUint64
	}
	return total + next
}

// per-node fixed overhead: the struct header plus the amortised cost
// of the pointers held by byRepo/byFile/byName/byQual secondary
// indexes inside each shard (4 maps × ~24 bytes for map bucket + slice
// element ≈ 100 bytes). Tuned against runtime.ReadMemStats deltas on a
// 50k-node repo; within ~10% of actual.
const nodeStructOverhead = 240

// per-edge fixed overhead: two string pointers, kind, filepath, line,
// plus slice-header and adjacency-map amortisation for outEdges AND
// inEdges (every edge is stored once as a struct but is referenced from
// both the source's out-adjacency list and the target's in-adjacency
// list, so the amortised overhead is ~2× slice-element + map-bucket).
const edgeStructOverhead = 144

// RepoMemoryEstimate sums the running per-shard counters maintained
// by AddNode / AddEdge / RemoveEdge / EvictFile / EvictRepo. O(shard
// count) instead of the O(repo nodes + total edges) walk this used to
// do — relevant because daemon-status queries call this once per
// tracked repo, and on a 488-repo / 1.9M-edge graph the old
// implementation was the single biggest source of writer contention
// during warmup.
func (g *Graph) RepoMemoryEstimate(repoPrefix string) RepoMemoryEstimate {
	g.lockAllRead()
	defer g.unlockAllRead()

	var est RepoMemoryEstimate
	for _, s := range g.shards {
		est.NodeBytes = saturatingAddUint64(est.NodeBytes, s.repoNodeBytes[repoPrefix])
		est.NodeCount += s.repoNodeCount[repoPrefix]
		est.EdgeBytes = saturatingAddUint64(est.EdgeBytes, s.repoEdgeBytes[repoPrefix])
		est.EdgeCount += s.repoEdgeCount[repoPrefix]
	}
	return est
}

// AllRepoMemoryEstimates returns the per-repo estimate for every
// repo with a tracked counter — one pass across shards, one read lock
// acquisition. Callers driving a per-repo loop (daemon status) should
// prefer this over the single-repo variant.
func (g *Graph) AllRepoMemoryEstimates() map[string]RepoMemoryEstimate {
	g.lockAllRead()
	defer g.unlockAllRead()

	out := make(map[string]RepoMemoryEstimate)
	for _, s := range g.shards {
		for prefix, bytes := range s.repoNodeBytes {
			est := out[prefix]
			est.NodeBytes = saturatingAddUint64(est.NodeBytes, bytes)
			est.NodeCount += s.repoNodeCount[prefix]
			out[prefix] = est
		}
		for prefix, bytes := range s.repoEdgeBytes {
			est := out[prefix]
			est.EdgeBytes = saturatingAddUint64(est.EdgeBytes, bytes)
			est.EdgeCount += s.repoEdgeCount[prefix]
			out[prefix] = est
		}
	}
	return out
}

// nodeBytes estimates the memory footprint of a single graph.Node.
func nodeBytes(n *Node) uint64 {
	if n == nil {
		return 0
	}
	b := uint64(nodeStructOverhead)
	b += uint64(len(n.ID) + len(n.Name) + len(n.QualName) + len(n.FilePath) + len(n.Language) + len(n.RepoPrefix))
	b += metaBytes(n.Meta)
	return b
}

// edgeBytes estimates the memory footprint of a single graph.Edge.
func edgeBytes(e *Edge) uint64 {
	if e == nil {
		return 0
	}
	b := uint64(edgeStructOverhead)
	b += uint64(len(e.From) + len(e.To) + len(e.Kind) + len(e.FilePath))
	return b
}

// metaBytes approximates the size of a Node.Meta map. Only handles the
// kinds of values we actually produce (string, bool, numeric, nested
// map, []string) — more exotic types fall back to a conservative
// constant rather than reflecting recursively.
func metaBytes(m map[string]any) uint64 {
	if m == nil {
		return 0
	}
	// map header + bucket amortisation for small maps.
	b := uint64(48 + 8*len(m))
	for k, v := range m {
		b += uint64(len(k)) + 16 // key entry overhead
		switch val := v.(type) {
		case string:
			b += uint64(len(val)) + 16
		case bool:
			b += 1 + 16
		case int, int32, int64, uint, uint32, uint64, float32, float64:
			b += 8 + 16
		case []string:
			b += 24 // slice header
			for _, s := range val {
				b += uint64(len(s)) + 16
			}
		case map[string]any:
			b += metaBytes(val)
		default:
			b += 32 // unknown — leave a sensible estimate
		}
	}
	return b
}

// RepoPrefixes returns a list of unique repository prefixes in the
// graph.
func (g *Graph) RepoPrefixes() []string {
	seen := make(map[string]struct{})
	for _, s := range g.shards {
		s.mu.RLock()
		for prefix := range s.byRepo {
			if prefix == "" {
				continue
			}
			seen[prefix] = struct{}{}
		}
		s.mu.RUnlock()
	}
	prefixes := make([]string, 0, len(seen))
	for prefix := range seen {
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// InDegreeForNodes is the in-memory reference implementation of the
// InDegreeForNodes capability. Walks the per-target in-edge buckets
// directly — the same arithmetic the disk backend pushes into a single
// server-side COUNT.
func (g *Graph) InDegreeForNodes(ids []string) map[string]int {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]int, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		c := len(g.GetInEdges(id))
		if c == 0 {
			continue
		}
		out[id] = c
	}
	return out
}

// ReachableForwardByKinds is the in-memory reference implementation
// of the ReachableForwardByKinds capability. Layer-by-layer BFS from
// the seed frontier, following only edges whose Kind is in the
// supplied set. Pure map / slice walks here — the win is the disk
// backend folds the BFS into one variable-length match.
func (g *Graph) ReachableForwardByKinds(seeds []string, kinds []EdgeKind) map[string]bool {
	if len(seeds) == 0 {
		return nil
	}
	covered := make(map[string]bool, len(seeds))
	frontier := make([]string, 0, len(seeds))
	for _, id := range seeds {
		if id == "" || covered[id] {
			continue
		}
		covered[id] = true
		frontier = append(frontier, id)
	}
	if len(kinds) == 0 {
		return covered
	}
	allowed := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		allowed[k] = struct{}{}
	}
	for len(frontier) > 0 {
		next := frontier[:0:0]
		for _, id := range frontier {
			for _, e := range g.GetOutEdges(id) {
				if e == nil {
					continue
				}
				if _, ok := allowed[e.Kind]; !ok {
					continue
				}
				if !covered[e.To] {
					covered[e.To] = true
					next = append(next, e.To)
				}
			}
		}
		frontier = next
	}
	return covered
}

// ThrowerErrorSurface is the in-memory reference implementation of
// the ThrowerErrorSurfacer capability. Walks EdgeThrows once for the
// per-thrower target dedup, then walks each thrower's out-edges for
// the EdgeEmits → KindString(context=error_msg) attachment. The disk
// backend collapses both passes into two server-side GROUP BYs.
func (g *Graph) ThrowerErrorSurface(pathPrefix string) []ThrowerErrorRow {
	byThrower := map[string]*ThrowerErrorRow{}
	addUnique := func(set []string, v string) []string {
		if slices.Contains(set, v) {
			return set
		}
		return append(set, v)
	}
	for e := range g.EdgesByKind(EdgeThrows) {
		if e == nil {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(graphpath.Norm(e.FilePath), graphpath.Norm(pathPrefix)) {
			continue
		}
		row, ok := byThrower[e.From]
		if !ok {
			file := e.FilePath
			line := e.Line
			n := g.GetNode(e.From)
			if n != nil {
				if file == "" {
					file = n.FilePath
				}
				if line == 0 {
					line = n.StartLine
				}
			}
			row = &ThrowerErrorRow{ThrowerID: e.From, FilePath: file, Line: line}
			byThrower[e.From] = row
		}
		row.Throws++
		row.ErrorTargets = addUnique(row.ErrorTargets, e.To)
	}
	for thrower, row := range byThrower {
		for _, e := range g.GetOutEdges(thrower) {
			if e == nil || e.Kind != EdgeEmits {
				continue
			}
			n := g.GetNode(e.To)
			if n == nil || n.Kind != KindString {
				continue
			}
			ctxLabel, _ := n.Meta["context"].(string)
			if ctxLabel != "error_msg" {
				continue
			}
			row.ErrorMsgs = addUnique(row.ErrorMsgs, n.Name)
		}
	}
	out := make([]ThrowerErrorRow, 0, len(byThrower))
	for _, r := range byThrower {
		out = append(out, *r)
	}
	return out
}

// MemberMethodsByType is the in-memory reference implementation of the
// MemberMethodsByType capability. One EdgesByKind(EdgeMemberOf) walk
// joined with the in-memory node table to filter Kind == KindMethod
// and project the four columns the resolver consumes — the exact
// loop the resolver runs today, just exposed as a single method call
// so the disk backend can fold the join into one query.
//
// Empty graph returns nil. Per-type method lists are deduplicated by
// MethodID so a method that appears twice in the EdgeMemberOf bucket
// (defensive against double-insertion) yields a single row.
func (g *Graph) MemberMethodsByType() map[string][]MemberMethodInfo {
	out := map[string][]MemberMethodInfo{}
	seen := map[string]map[string]struct{}{}
	var edges []*Edge
	var methodIDs []string
	for e := range g.EdgesByKind(EdgeMemberOf) {
		if e == nil {
			continue
		}
		edges = append(edges, e)
		methodIDs = append(methodIDs, e.From)
	}
	methods := g.GetNodesByIDs(methodIDs)
	for _, e := range edges {
		m := methods[e.From]
		if m == nil || m.Kind != KindMethod {
			continue
		}
		typeID := e.To
		dedup := seen[typeID]
		if dedup == nil {
			dedup = make(map[string]struct{})
			seen[typeID] = dedup
		}
		if _, ok := dedup[m.ID]; ok {
			continue
		}
		dedup[m.ID] = struct{}{}
		out[typeID] = append(out[typeID], MemberMethodInfo{
			MethodID:   m.ID,
			Name:       m.Name,
			FilePath:   m.FilePath,
			StartLine:  m.StartLine,
			RepoPrefix: m.RepoPrefix,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// StructuralParentEdges is the in-memory reference implementation of
// the StructuralParentEdges capability. It snapshots only
// (Extends | Implements | Composes) edges, resolves their endpoints in one
// batch, and then applies the (Type | Interface) endpoint-kind gate.
//
// Empty graph or no matching edges returns nil.
func (g *Graph) StructuralParentEdges() []StructuralParentEdgeRow {
	var edges []*Edge
	var endpointIDs []string
	for e := range g.EdgesByKinds([]EdgeKind{EdgeExtends, EdgeImplements, EdgeComposes}) {
		if e == nil {
			continue
		}
		edges = append(edges, e)
		endpointIDs = append(endpointIDs, e.From, e.To)
	}
	endpoints := g.GetNodesByIDs(endpointIDs)
	var out []StructuralParentEdgeRow
	for _, e := range edges {
		from := endpoints[e.From]
		to := endpoints[e.To]
		if from == nil || to == nil {
			continue
		}
		if from.Kind != KindType && from.Kind != KindInterface {
			continue
		}
		if to.Kind != KindType && to.Kind != KindInterface {
			continue
		}
		out = append(out, StructuralParentEdgeRow{
			FromID:   from.ID,
			ToID:     to.ID,
			FromKind: from.Kind,
			ToKind:   to.Kind,
			Origin:   e.Origin,
		})
	}
	return out
}

// CrossRepoCandidates is the in-memory reference implementation of the
// CrossRepoCandidates capability. Single AllEdges scan with the
// edge-kind gate + the (non-empty, distinct) repo-prefix gate. Returns
// one row per surviving edge carrying the underlying Edge pointer plus
// the two RepoPrefix values projected from the endpoints.
//
// Empty baseKinds returns nil — matches the disk-backend contract.
// Single-repo graphs (or graphs whose nodes carry no RepoPrefix)
// return no rows because the prefix gate filters them out.
func (g *Graph) CrossRepoCandidates(baseKinds []EdgeKind) []CrossRepoCandidateRow {
	return g.crossRepoCandidatesScoped(baseKinds, nil, nil)
}

// CrossRepoCandidatesForRepos returns relationships incident to one of the
// supplied repositories. The endpoint lookup is one batched projection; it
// never performs a node lookup per edge.
func (g *Graph) CrossRepoCandidatesForRepos(baseKinds []EdgeKind, repoPrefixes []string) []CrossRepoCandidateRow {
	repos := nonEmptyStringSet(repoPrefixes)
	if len(repos) == 0 {
		return nil
	}
	return g.crossRepoCandidatesScoped(baseKinds, repos, nil)
}

// CrossRepoCandidatesForFiles is the exact changed-file frontier used by the
// watcher path. An edge is included when its source site or either endpoint is
// owned by a changed file.
func (g *Graph) CrossRepoCandidatesForFiles(baseKinds []EdgeKind, filePaths []string) []CrossRepoCandidateRow {
	files := nonEmptyStringSet(filePaths)
	if len(files) == 0 {
		return nil
	}
	return g.crossRepoCandidatesScoped(baseKinds, nil, files)
}

func nonEmptyStringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func (g *Graph) crossRepoCandidatesScoped(baseKinds []EdgeKind, repos, files map[string]struct{}) []CrossRepoCandidateRow {
	if len(baseKinds) == 0 {
		return nil
	}

	var edges []*Edge
	for edge := range g.EdgesByKinds(baseKinds) {
		if edge != nil {
			edges = append(edges, edge)
		}
	}
	if len(edges) == 0 {
		return nil
	}

	endpointIDs := make([]string, 0, len(edges)*2)
	for _, edge := range edges {
		endpointIDs = append(endpointIDs, edge.From, edge.To)
	}
	nodes := g.GetNodesByIDs(endpointIDs)
	out := make([]CrossRepoCandidateRow, 0, len(edges))
	for _, edge := range edges {
		from, to := nodes[edge.From], nodes[edge.To]
		if from == nil || to == nil || from.RepoPrefix == "" || to.RepoPrefix == "" || from.RepoPrefix == to.RepoPrefix {
			continue
		}
		if repos != nil {
			_, fromScoped := repos[from.RepoPrefix]
			_, toScoped := repos[to.RepoPrefix]
			if !fromScoped && !toScoped {
				continue
			}
		}
		if files != nil {
			_, edgeScoped := files[edge.FilePath]
			_, fromScoped := files[from.FilePath]
			_, toScoped := files[to.FilePath]
			if !edgeScoped && !fromScoped && !toScoped {
				continue
			}
		}
		out = append(out, CrossRepoCandidateRow{Edge: edge, FromRepo: from.RepoPrefix, ToRepo: to.RepoPrefix})
	}
	return out
}

// bfsHopLess orders two discovery candidates for the SAME node at the
// SAME depth: smaller ParentID first, then smaller EdgeKind. This is the
// tie-break the SQLite implementation applies via
// ROW_NUMBER() OVER (PARTITION BY node_id ORDER BY depth, parent_id,
// edge_kind), so both backends settle on the identical discovery edge.
func bfsHopLess(a, b BFSHop) bool {
	if a.ParentID != b.ParentID {
		return a.ParentID < b.ParentID
	}
	return a.EdgeKind < b.EdgeKind
}

// BFS is the in-memory reference implementation of the BFSCapable
// capability — the oracle the SQLite recursive-CTE walk is shadow-tested
// against. Layer-by-layer BFS over GetOutEdges / GetInEdges; see the
// BFSCapable doc for the exact reachability / determinism / bound
// semantics. Always returns a nil error (the error in the signature is
// for the disk implementation's query failures).
func (g *Graph) BFS(seeds []string, dir Direction, kinds []EdgeKind, maxDepth, limit int) ([]BFSHop, error) {
	if len(seeds) == 0 {
		return nil, nil
	}
	kset := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k != "" {
			kset[k] = struct{}{}
		}
	}
	forward := dir != DirectionBackward

	// chosen[node] = the winning hop for that node (min depth, then the
	// (parent, kind)-smallest discovery edge). Once a node is in chosen it
	// is settled at its minimum depth and never revisited — that, plus the
	// maxDepth bound, is what terminates a cyclic graph.
	chosen := make(map[string]BFSHop, len(seeds))
	frontier := make([]string, 0, len(seeds))
	for _, s := range seeds {
		if s == "" {
			continue
		}
		if _, ok := chosen[s]; ok {
			continue
		}
		chosen[s] = BFSHop{NodeID: s, Depth: 0}
		frontier = append(frontier, s)
	}

	if len(kset) > 0 && maxDepth > 0 {
		for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
			// Collect every depth+1 candidate across the WHOLE frontier
			// before settling, so a node reached by several same-layer
			// parents converges on the deterministic (parent, kind)-smallest
			// — matching the CTE's ROW_NUMBER pick.
			cand := make(map[string]BFSHop)
			for _, cur := range frontier {
				var edges []*Edge
				if forward {
					edges = g.GetOutEdges(cur)
				} else {
					edges = g.GetInEdges(cur)
				}
				for _, e := range edges {
					if e == nil {
						continue
					}
					if _, ok := kset[e.Kind]; !ok {
						continue
					}
					nb := e.To
					if !forward {
						nb = e.From
					}
					if nb == "" {
						continue
					}
					if _, ok := chosen[nb]; ok {
						continue // already settled at a shallower-or-equal depth
					}
					// Node-backed targets only: an edge to an unresolved /
					// external stub (no node row) is not a reachable hop.
					if g.GetNode(nb) == nil {
						continue
					}
					h := BFSHop{NodeID: nb, Depth: depth + 1, ParentID: cur, EdgeKind: e.Kind}
					if existing, ok := cand[nb]; !ok || bfsHopLess(h, existing) {
						cand[nb] = h
					}
				}
			}
			next := make([]string, 0, len(cand))
			for nb, h := range cand {
				chosen[nb] = h
				next = append(next, nb)
			}
			frontier = next
		}
	}

	out := make([]BFSHop, 0, len(chosen))
	for _, h := range chosen {
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b BFSHop) int {
		if a.Depth != b.Depth {
			return a.Depth - b.Depth
		}
		if a.NodeID < b.NodeID {
			return -1
		}
		if a.NodeID > b.NodeID {
			return 1
		}
		return 0
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ExtractCandidates is the in-memory reference implementation of
// ExtractCandidatesScanner. Walks NodesByKind for function + method,
// applies the threshold gates locally, and counts distinct in-edge
// From / out-edge To values restricted to the requested edge kinds.
func (g *Graph) ExtractCandidates(
	kinds []EdgeKind,
	minLines, minCallers, minFanOut int,
	pathPrefix string,
) []ExtractCandidateRow {
	if len(kinds) == 0 {
		return nil
	}
	kset := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		kset[k] = struct{}{}
	}
	if len(kset) == 0 {
		return nil
	}
	var out []ExtractCandidateRow
	for _, n := range g.NodesByKinds([]NodeKind{KindFunction, KindMethod}) {
		if n == nil {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(graphpath.Norm(n.FilePath), graphpath.Norm(pathPrefix)) {
			continue
		}
		if n.StartLine == 0 || n.EndLine == 0 {
			continue
		}
		lineCount := n.EndLine - n.StartLine + 1
		if lineCount < minLines {
			continue
		}
		callerSet := make(map[string]struct{})
		for _, e := range g.GetInEdges(n.ID) {
			if e == nil {
				continue
			}
			if _, ok := kset[e.Kind]; !ok {
				continue
			}
			callerSet[e.From] = struct{}{}
		}
		if len(callerSet) < minCallers {
			continue
		}
		calleeSet := make(map[string]struct{})
		for _, e := range g.GetOutEdges(n.ID) {
			if e == nil {
				continue
			}
			if _, ok := kset[e.Kind]; !ok {
				continue
			}
			calleeSet[e.To] = struct{}{}
		}
		if len(calleeSet) < minFanOut {
			continue
		}
		out = append(out, ExtractCandidateRow{
			NodeID:      n.ID,
			Name:        n.Name,
			FilePath:    n.FilePath,
			StartLine:   n.StartLine,
			EndLine:     n.EndLine,
			LineCount:   lineCount,
			CallerCount: len(callerSet),
			FanOut:      len(calleeSet),
		})
	}
	return out
}

// FileSymbolNamesByPaths is the in-memory reference implementation of
// the FileSymbolNamesByPaths capability. Walks GetFileNodes for every
// input path, keeps the requested kinds, and emits one row per
// (path, name) pair. Duplicates within a file collapse to a single
// row (a method declared once per file emits once regardless of how
// many times the indexer touched it).
func (g *Graph) FileSymbolNamesByPaths(paths []string, kinds []NodeKind) []FileSymbolNameRow {
	if len(paths) == 0 {
		return nil
	}
	kset := make(map[NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		kset[k] = struct{}{}
	}
	seen := make(map[string]struct{})
	dedupKey := func(p, name string) string { return p + "\x00" + name }
	var out []FileSymbolNameRow
	for _, p := range paths {
		if p == "" {
			continue
		}
		for _, n := range g.GetFileNodes(p) {
			if n == nil || n.Name == "" {
				continue
			}
			if len(kset) > 0 {
				if _, ok := kset[n.Kind]; !ok {
					continue
				}
			}
			k := dedupKey(p, n.Name)
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, FileSymbolNameRow{FilePath: p, Name: n.Name})
		}
	}
	return out
}

// ClassHierarchyTraverse is the in-memory reference implementation of
// ClassHierarchyTraverser. Performs the same BFS as
// query.ClassHierarchy, but stops at the kind/depth gates and returns
// the full Path + EdgeKinds for each terminal node reached so the
// disk backend's variable-length match can be a drop-in
// replacement. Direction "up" follows out-edges; "down" follows
// in-edges.
func (g *Graph) ClassHierarchyTraverse(
	seedID string,
	direction string,
	kinds []EdgeKind,
	depth int,
) []ClassHierarchyRow {
	if seedID == "" || depth <= 0 || len(kinds) == 0 {
		return nil
	}
	kset := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		kset[k] = struct{}{}
	}
	if len(kset) == 0 {
		return nil
	}
	if g.GetNode(seedID) == nil {
		return nil
	}
	walkUp := direction == "up"
	walkDown := direction == "down"
	if !walkUp && !walkDown {
		return nil
	}
	type queued struct {
		id        string
		path      []string
		edgeKinds []EdgeKind
		hops      int
	}
	visited := map[string]struct{}{seedID: {}}
	queue := []queued{{id: seedID, path: nil, edgeKinds: nil, hops: 0}}
	var out []ClassHierarchyRow
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.hops >= depth {
			continue
		}
		var edges []*Edge
		if walkUp {
			edges = g.GetOutEdges(cur.id)
		} else {
			edges = g.GetInEdges(cur.id)
		}
		for _, e := range edges {
			if e == nil {
				continue
			}
			if _, ok := kset[e.Kind]; !ok {
				continue
			}
			var nb string
			if walkUp {
				nb = e.To
			} else {
				nb = e.From
			}
			if nb == "" {
				continue
			}
			if _, ok := visited[nb]; ok {
				continue
			}
			visited[nb] = struct{}{}
			newPath := append([]string(nil), cur.path...)
			newPath = append(newPath, nb)
			newKinds := append([]EdgeKind(nil), cur.edgeKinds...)
			newKinds = append(newKinds, e.Kind)
			out = append(out, ClassHierarchyRow{
				Path:      newPath,
				EdgeKinds: newKinds,
			})
			queue = append(queue, queued{id: nb, path: newPath, edgeKinds: newKinds, hops: cur.hops + 1})
		}
	}
	return out
}

// FileEditingContext is the in-memory reference implementation of the
// FileEditingContext capability. Performs the equivalent of
// GetFileSymbols + per-function GetCallers/GetCallChain but bounded
// to the call/method node set, so the disk backend's batched query
// returns the same projection. The kinds parameter is the set of
// kinds treated as call targets (function + method).
func (g *Graph) FileEditingContext(filePath string, kinds []NodeKind) *FileEditingContextResult {
	if filePath == "" {
		return nil
	}
	nodes := g.GetFileNodes(filePath)
	if len(nodes) == 0 {
		return nil
	}
	kset := make(map[NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		kset[k] = struct{}{}
	}
	res := &FileEditingContextResult{}
	var fileNodeID string
	var defNodeIDs []string
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind == KindFile {
			res.FileNode = n
			fileNodeID = n.ID
			continue
		}
		res.Defines = append(res.Defines, n)
		if _, ok := kset[n.Kind]; ok {
			defNodeIDs = append(defNodeIDs, n.ID)
		}
	}
	if fileNodeID != "" {
		for _, e := range g.GetOutEdges(fileNodeID) {
			if e == nil {
				continue
			}
			if e.Kind == EdgeImports {
				res.Imports = append(res.Imports, e)
			}
		}
	}
	if len(defNodeIDs) == 0 {
		return res
	}
	inEdges := g.GetInEdgesByNodeIDs(defNodeIDs)
	outEdges := g.GetOutEdgesByNodeIDs(defNodeIDs)
	callerIDSet := make(map[string]struct{})
	calleeIDSet := make(map[string]struct{})
	for _, id := range defNodeIDs {
		for _, e := range inEdges[id] {
			if e == nil || e.Kind != EdgeCalls {
				continue
			}
			if e.From == "" {
				continue
			}
			callerIDSet[e.From] = struct{}{}
		}
		for _, e := range outEdges[id] {
			if e == nil || e.Kind != EdgeCalls {
				continue
			}
			if e.To == "" {
				continue
			}
			calleeIDSet[e.To] = struct{}{}
		}
	}
	callerIDs := make([]string, 0, len(callerIDSet))
	for id := range callerIDSet {
		callerIDs = append(callerIDs, id)
	}
	calleeIDs := make([]string, 0, len(calleeIDSet))
	for id := range calleeIDSet {
		calleeIDs = append(calleeIDs, id)
	}
	callerNodes := g.GetNodesByIDs(callerIDs)
	calleeNodes := g.GetNodesByIDs(calleeIDs)
	for _, id := range callerIDs {
		n := callerNodes[id]
		if n == nil || n.FilePath == filePath {
			continue
		}
		res.CalledBy = append(res.CalledBy, n)
	}
	for _, id := range calleeIDs {
		n := calleeNodes[id]
		if n == nil || n.FilePath == filePath {
			continue
		}
		res.Calls = append(res.Calls, n)
	}
	return res
}

// GetFileSubGraph is the in-memory reference implementation of the
// FileSubGraphReader capability. Iterates the existing per-file
// byFile bucket and the per-node outEdges / inEdges shards — the
// same lookups Engine.GetFileSymbols' fallback path already runs,
// just collapsed behind one method so the disk backend can push the
// whole walk into a single query.
func (g *Graph) GetFileSubGraph(filePath string) ([]*Node, []*Edge) {
	if filePath == "" {
		return nil, nil
	}
	nodes := g.GetFileNodes(filePath)
	if len(nodes) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil && n.ID != "" {
			ids = append(ids, n.ID)
		}
	}
	outByID := g.GetOutEdgesByNodeIDs(ids)
	inByID := g.GetInEdgesByNodeIDs(ids)
	type edgeKey struct {
		from string
		to   string
		kind EdgeKind
	}
	seen := make(map[edgeKey]struct{}, 2*len(ids))
	edges := make([]*Edge, 0, 2*len(ids))
	add := func(e *Edge) {
		if e == nil {
			return
		}
		k := edgeKey{from: e.From, to: e.To, kind: e.Kind}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		edges = append(edges, e)
	}
	for _, id := range ids {
		for _, e := range outByID[id] {
			add(e)
		}
		for _, e := range inByID[id] {
			add(e)
		}
	}
	return nodes, edges
}

// GetFileSubGraphCounts is the in-memory reference implementation of
// FileSubGraphCountReader. The per-node bucket reads are already
// O(1) so it just walks GetFileSubGraph and reports len(edges); the
// row-materialisation win belongs to disk backends.
func (g *Graph) GetFileSubGraphCounts(filePath string) ([]*Node, int) {
	nodes, edges := g.GetFileSubGraph(filePath)
	return nodes, len(edges)
}

// NodeDegreeByKinds is the in-memory reference implementation of the
// NodeDegreeByKinds capability. Walks NodesByKinds and reads each
// node's in/out edge buckets — the disk backend overrides with one
// kind-filtered aggregation per direction so the IN-list of node IDs
// the legacy NodeDegreeCounts path needed is avoided altogether.
func (g *Graph) NodeDegreeByKinds(kinds []NodeKind, pathPrefix string) []NodeDegreeRow {
	if len(kinds) == 0 {
		return nil
	}
	pool := g.NodesByKinds(kinds)
	out := make([]NodeDegreeRow, 0, len(pool))
	for _, n := range pool {
		if n == nil {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(graphpath.Norm(n.FilePath), graphpath.Norm(pathPrefix)) {
			continue
		}
		out = append(out, NodeDegreeRow{
			NodeID:   n.ID,
			InCount:  len(g.GetInEdges(n.ID)),
			OutCount: len(g.GetOutEdges(n.ID)),
		})
	}
	return out
}
