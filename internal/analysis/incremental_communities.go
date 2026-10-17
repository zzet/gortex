package analysis

import (
	"hash/fnv"
	"path"
	"path/filepath"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// Incremental community detection.
//
// A plain `analyze kind=clusters` call recomputes the whole-graph
// partition from scratch every time. When only one or two packages
// changed since the last run that is wasteful: the partition of the
// untouched 95% of the graph is bit-for-bit what it was before.
//
// The incremental path keeps the last node-to-community assignments in a cache
// keyed by a per-package content fingerprint. It deliberately does not retain
// the prior weighted adjacency: the current graph is rebuilt for every run.
// On the next request it diffs
// the current fingerprints against the cached ones; packages whose
// fingerprint is unchanged keep their cached community assignment,
// and only the changed packages — plus their immediate cross-package
// boundary — are re-partitioned by a restricted local-moves pass
// seeded from the cached partition. The two halves are then merged
// and run through the same labelling pipeline as a full recompute,
// so the wire shape is identical.
//
// Scope: the incremental path covers Leiden only (the default
// algorithm). Louvain and spectral always recompute in full — their
// call sites do not consult the cache. The fallback below also
// triggers a full Leiden recompute whenever there is no usable
// cache or too much of the graph changed.

// changedFractionFullRecompute is the share of packages that must
// change before the incremental path gives up and recomputes the
// whole graph. Past this point the boundary of the changed set is
// large enough that a restricted pass saves little, and a global
// optimum is worth the full cost. A brand-new cache (no overlapping
// packages) trivially exceeds this and falls back.
const changedFractionFullRecompute = 0.5

// leidenPartition is the raw, pre-renumbering output of a Leiden
// run: the data an incremental re-run needs to re-seed from. It is
// deliberately distinct from CommunityResult, which carries only
// renumbered "community-N" ids and omits singletons.
type leidenPartition struct {
	// comm maps an original symbol-node id to its stable raw
	// community key (an arbitrary member id, not "community-N").
	comm map[string]string
	// neighbors is the weighted undirected adjacency the partition
	// was computed on (symbol nodes only, edgeWeight-weighted).
	neighbors map[string]map[string]float64
	// degree is the weighted degree per symbol node.
	degree map[string]float64
	// totalWeight is the sum of edge weights (each undirected edge
	// counted once).
	totalWeight float64
	// symbolNodes is the set of clustering-relevant node ids.
	symbolNodes map[string]bool
}

// leidenGraph is the weighted symbol graph both the full and the
// incremental Leiden paths optimize over. Extracting it keeps the
// node/edge filter and weighting identical across the two paths —
// the incremental result is only trustworthy if it is built from
// exactly the same graph the full path would have built.
type leidenGraph struct {
	symbolNodes map[string]bool
	neighbors   map[string]map[string]float64
	degree      map[string]float64
	totalWeight float64
}

// buildLeidenGraph applies the Leiden/Louvain node+edge filter
// (symbol nodes only, edgeWeight-weighted, undirected) and returns
// the resulting weighted graph. Returns nil when the graph has no
// clustering-relevant edges — the caller then yields an empty
// partition.
var leidenEdgeKinds = []graph.EdgeKind{
	graph.EdgeCalls,
	graph.EdgeSpawns,
	graph.EdgeMemberOf,
	graph.EdgeParamOf,
	graph.EdgeReferences,
	graph.EdgeReturns,
	graph.EdgeTypedAs,
	graph.EdgeImplements,
	graph.EdgeExtends,
	graph.EdgeAliases,
	graph.EdgeComposes,
	graph.EdgeImports,
	graph.EdgeDependsOnModule,
	graph.EdgeInstantiates,
}

func buildLeidenGraph(g graph.Store, pace *Pace) *leidenGraph {
	symbolNodes := make(map[string]bool, g.NodeCount())
	for n := range graph.NodesLightSeq(g) {
		pace.Tick()
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			symbolNodes[n.ID] = true
		}
	}

	type edgeKey struct{ a, b string }
	weights := make(map[edgeKey]float64)
	// The fixed kind projection mirrors edgeWeight exactly. SQLite can serve it
	// from edges_by_kind and never transfers unrelated domain edges or Meta.
	for e := range graph.EdgesLightSeq(g, leidenEdgeKinds...) {
		pace.Tick()
		if !symbolNodes[e.From] || !symbolNodes[e.To] {
			continue
		}
		w := edgeWeight(e.Kind)
		if w == 0 {
			continue
		}
		weights[edgeKey{e.From, e.To}] += w
		weights[edgeKey{e.To, e.From}] += w
	}

	neighbors := make(map[string]map[string]float64)
	for k, w := range weights {
		pace.Tick()
		if neighbors[k.a] == nil {
			neighbors[k.a] = make(map[string]float64)
		}
		neighbors[k.a][k.b] = w
	}

	var totalWeight float64
	for _, w := range weights {
		totalWeight += w
	}
	totalWeight /= 2

	if totalWeight == 0 {
		return nil
	}

	degree := make(map[string]float64)
	for id := range symbolNodes {
		pace.Tick()
		for _, w := range neighbors[id] {
			degree[id] += w
		}
	}

	return &leidenGraph{
		symbolNodes: symbolNodes,
		neighbors:   neighbors,
		degree:      degree,
		totalWeight: totalWeight,
	}
}

// LeidenPartitionCache holds the last Leiden assignment map so a later run can
// recompute only the packages that changed. It intentionally omits the prior
// adjacency/degree graph, which would duplicate the store's largest topology
// structures for the lifetime of the server. It is opaque to
// callers: hand a *LeidenPartitionCache (or nil on the first call)
// to DetectCommunitiesLeidenIncremental and store the cache it
// returns for next time. A nil cache is always safe — it simply
// forces a full recompute.
//
// The cache is not safe for concurrent use; callers serialize
// access (the MCP server holds it behind a mutex).
type LeidenPartitionCache struct {
	// pkgFingerprint maps a package key to a content hash of that
	// package's nodes and incident clustering edges. A package
	// whose fingerprint is unchanged between runs keeps its cached
	// community assignment verbatim.
	pkgFingerprint map[string]uint64
	// nodeComm is the cached raw partition: symbol-node id → raw
	// community key. Reused wholesale for unchanged packages and as
	// the seed for the restricted re-optimization of changed ones.
	nodeComm map[string]string
	// edgeIdentityRevisions snapshots the graph's monotonic
	// provenance-revision counter at cache time. A mismatch means
	// in-place edge provenance changed under the cache; per-package
	// fingerprints already detect topology changes, but a pure
	// provenance churn (edge endpoints unchanged) would otherwise
	// slip past, so a mismatch alone forces a full recompute.
	edgeIdentityRevisions int
}

// PackageFingerprints returns the cached per-package fingerprint map.
// Callers MUST treat the returned value as read-only — it is the live
// map the cache reuses on the next call. Used by the MCP server to
// report total_packages from a cache hit without re-running the
// fingerprint pass.
func (c *LeidenPartitionCache) PackageFingerprints() map[string]uint64 {
	if c == nil {
		return nil
	}
	return c.pkgFingerprint
}

// IncrementalCommunityStats reports what the incremental path did on
// a single call — useful for tests and for surfacing on the wire.
type IncrementalCommunityStats struct {
	// Incremental is true when the changed-package fast path ran;
	// false means a full recompute (no cache, stale cache, or the
	// changed fraction exceeded the fallback ratio).
	Incremental bool
	// FullRecomputeReason names why a full recompute happened. Empty
	// when Incremental is true.
	FullRecomputeReason string
	// ChangedPackages is the count of packages whose fingerprint
	// differed from the cache.
	ChangedPackages int
	// TotalPackages is the package count in the current graph.
	TotalPackages int
	// RepartitionedNodes is the number of symbol nodes that were
	// re-optimized (changed packages plus their boundary).
	RepartitionedNodes int
}

// packageKey derives a stable package identity for a symbol node
// from its file path: the directory the file lives in. Nodes in the
// same directory share a key (Go packages are one directory; the
// granularity is right for "which packages changed"). A node with
// no file path is bucketed under "" — a single catch-all package.
func packageKey(filePath string) string {
	if filePath == "" {
		return ""
	}
	// path.Dir, not filepath.Dir: these keys are compared against
	// forward-slash keys elsewhere (the bundle cache mirrors this function),
	// and filepath.Dir cleans to the OS separator, undoing the ToSlash on
	// Windows.
	dir := path.Dir(filepath.ToSlash(filePath))
	if dir == "." {
		return ""
	}
	return dir
}

// fingerprintPackages computes an order-independent content hash per
// package over the clustering-relevant graph. The hash folds in,
// for every package:
//
//   - each member node's id and kind, and
//   - each clustering edge with at least one endpoint in the
//     package (the edge is mixed into both endpoints' packages so a
//     cross-package edge change marks both as changed).
//
// Per-element hashes are XOR-combined, so the result does not
// depend on graph iteration order — two runs over the same graph
// always produce the same fingerprints. Any node added/removed,
// kind change, or edge added/removed/reweighted flips the
// fingerprint of every package it touches and leaves all others
// bit-identical.
func fingerprintPackages(g graph.Store, pace *Pace) map[string]uint64 {
	// Symbol-node filter + each node's package, mirroring
	// buildLeidenGraph so the fingerprint and the partition agree on
	// what counts.
	pkgOf := make(map[string]string, g.NodeCount())
	fp := make(map[string]uint64)
	for n := range graph.NodesLightSeq(g) {
		pace.Tick()
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		pk := packageKey(n.FilePath)
		pkgOf[n.ID] = pk
		// Mix the node's identity into its package fingerprint.
		h := fnv.New64a()
		_, _ = h.Write([]byte("n\x00"))
		_, _ = h.Write([]byte(n.ID))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(string(n.Kind)))
		fp[pk] ^= h.Sum64()
	}

	for e := range graph.EdgesLightSeq(g, leidenEdgeKinds...) {
		pace.Tick()
		fromPkg, fromOK := pkgOf[e.From]
		toPkg, toOK := pkgOf[e.To]
		if !fromOK || !toOK {
			continue
		}
		if edgeWeight(e.Kind) == 0 {
			continue
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte("e\x00"))
		_, _ = h.Write([]byte(e.From))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(e.To))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(string(e.Kind)))
		sum := h.Sum64()
		// Mix into both endpoints' packages. When from and to share
		// a package the two XORs cancel; re-fold a tagged variant so
		// an intra-package edge still moves the fingerprint.
		if fromPkg == toPkg {
			intra := fnv.New64a()
			_, _ = intra.Write([]byte("ie\x00"))
			_, _ = intra.Write([]byte(e.From))
			_, _ = intra.Write([]byte{0})
			_, _ = intra.Write([]byte(e.To))
			_, _ = intra.Write([]byte{0})
			_, _ = intra.Write([]byte(string(e.Kind)))
			fp[fromPkg] ^= intra.Sum64()
			continue
		}
		fp[fromPkg] ^= sum
		fp[toPkg] ^= sum
	}

	return fp
}

// diffPackageFingerprints returns the set of package keys whose
// fingerprint differs between old and cur — added packages, removed
// packages, and packages whose content changed. A removed package's
// key is included so its now-orphaned nodes are dropped from the
// reused assignment.
func diffPackageFingerprints(old, cur map[string]uint64) map[string]bool {
	changed := make(map[string]bool)
	for pk, h := range cur {
		if oh, ok := old[pk]; !ok || oh != h {
			changed[pk] = true
		}
	}
	for pk := range old {
		if _, ok := cur[pk]; !ok {
			changed[pk] = true
		}
	}
	return changed
}

// DetectCommunitiesLeidenIncremental detects communities with
// Leiden, recomputing only the packages that changed since the
// cached partition was built. Pass cache == nil on the first call;
// store the returned cache and pass it back next time.
//
// It returns the labelled CommunityResult, a fresh cache to carry
// forward, and stats describing whether the fast path was taken.
// The result is shape-identical to DetectCommunitiesLeiden: for
// unchanged packages the community assignment is exactly what the
// cache held; for changed packages it is a genuine re-partition.
//
// A full recompute happens (and is reflected in the stats) when:
//   - cache is nil, or
//   - the graph's edge-provenance revision moved under the cache, or
//   - the changed-package fraction exceeds changedFractionFullRecompute.
func DetectCommunitiesLeidenIncremental(
	g graph.Store,
	cache *LeidenPartitionCache,
) (*CommunityResult, *LeidenPartitionCache, IncrementalCommunityStats) {
	return DetectCommunitiesLeidenIncrementalPaced(g, cache, nil)
}

// DetectCommunitiesLeidenIncrementalPaced is DetectCommunitiesLeidenIncremental
// with a cooperative scheduling point in every hot loop (see Pace). A nil Pace
// never parks.
func DetectCommunitiesLeidenIncrementalPaced(
	g graph.Store,
	cache *LeidenPartitionCache,
	pace *Pace,
) (*CommunityResult, *LeidenPartitionCache, IncrementalCommunityStats) {
	curFP := fingerprintPackages(g, pace)
	stats := IncrementalCommunityStats{TotalPackages: len(curFP)}
	edgeRev := g.EdgeIdentityRevisions()

	fullRecompute := func(reason string) (*CommunityResult, *LeidenPartitionCache, IncrementalCommunityStats) {
		result, part := detectCommunitiesLeidenRaw(g, defaultLeidenOptions(), pace)
		stats.Incremental = false
		stats.FullRecomputeReason = reason
		newCache := &LeidenPartitionCache{
			pkgFingerprint:        curFP,
			edgeIdentityRevisions: edgeRev,
		}
		if part != nil {
			// Retain only the assignment map needed to seed later incremental
			// runs. The full partition also owns weighted adjacency, degree, and
			// symbol-node maps; keeping it here duplicates the graph for the
			// lifetime of the MCP server even though incrementalLeiden rebuilds
			// current adjacency before every run.
			newCache.nodeComm = part.comm
		}
		return result, newCache, stats
	}

	// No cache, or a cache whose partition never materialized
	// (previous graph had no clustering edges): nothing to reuse.
	if cache == nil || len(cache.nodeComm) == 0 {
		return fullRecompute("no cached partition")
	}

	// Edge provenance changed in place under the cache. Topology
	// fingerprints would miss a pure provenance churn (same
	// endpoints, new origin); recompute to stay correct.
	if cache.edgeIdentityRevisions != edgeRev {
		return fullRecompute("edge provenance changed")
	}

	changed := diffPackageFingerprints(cache.pkgFingerprint, curFP)
	stats.ChangedPackages = len(changed)

	// Too much of the graph moved — a restricted pass would re-touch
	// most of it anyway. Recompute globally for a clean optimum.
	if len(curFP) == 0 ||
		float64(len(changed)) > changedFractionFullRecompute*float64(len(curFP)) {
		return fullRecompute("changed fraction exceeded threshold")
	}

	// Nothing changed: reuse the cached partition verbatim. We still
	// rebuild the CommunityResult from the cached raw partition so
	// the caller always gets a freshly-labelled result, but no
	// re-partitioning happens.
	lg := buildLeidenGraph(g, pace)
	if lg == nil {
		// The graph lost all its clustering edges since the cache
		// was built — fall back rather than reuse a stale partition.
		return fullRecompute("graph has no clustering edges")
	}

	result, newPart := incrementalLeiden(g, lg, cache, changed, pace)
	stats.Incremental = true
	stats.RepartitionedNodes = newPart.repartitioned
	newCache := &LeidenPartitionCache{
		pkgFingerprint:        curFP,
		nodeComm:              newPart.partition.comm,
		edgeIdentityRevisions: edgeRev,
	}
	return result, newCache, stats
}

// incrementalResult bundles the relabelled result of an incremental
// run with the raw partition to cache and the size of the
// re-optimized set.
type incrementalResult struct {
	partition     *leidenPartition
	repartitioned int
}

// incrementalLeiden performs the restricted re-partition. It starts
// from the cached node→community assignment, then re-optimizes only
// the nodes that belong to a changed package or sit on its boundary
// (an unchanged-package node with an edge to a changed-package
// node). Boundary nodes are anchors: they feed their cached
// community into the gain calculation but never move themselves, so
// every unchanged package's assignment is preserved bit-for-bit.
func incrementalLeiden(
	g graph.Store,
	lg *leidenGraph,
	cache *LeidenPartitionCache,
	changedPkgs map[string]bool,
	pace *Pace,
) (*CommunityResult, incrementalResult) {
	// Package of every current symbol node.
	pkgOf := make(map[string]string, len(lg.symbolNodes))
	for n := range graph.NodesLightSeq(g) {
		pace.Tick()
		if lg.symbolNodes[n.ID] {
			pkgOf[n.ID] = packageKey(n.FilePath)
		}
	}

	// Seed: cached assignment for every node still present; a new
	// node (in a changed package, by construction) seeds into its
	// own singleton community.
	seed := make(map[string]string, len(lg.symbolNodes))
	for id := range lg.symbolNodes {
		if c, ok := cache.nodeComm[id]; ok {
			seed[id] = c
		} else {
			seed[id] = id
		}
	}

	// movable = nodes in a changed package. boundary = unchanged
	// nodes with an edge into a changed package; they participate as
	// fixed anchors. The union is the re-optimized frontier.
	movable := make(map[string]bool)
	for id := range lg.symbolNodes {
		if changedPkgs[pkgOf[id]] {
			movable[id] = true
		}
	}
	boundary := make(map[string]bool)
	for id := range movable {
		for nbr := range lg.neighbors[id] {
			if nbr == id || movable[nbr] {
				continue
			}
			boundary[nbr] = true
		}
	}

	// Restricted local moves: optimize `movable`, anchored by
	// `boundary`. Deterministic — movable nodes are visited in
	// sorted order.
	movableIDs := make([]string, 0, len(movable))
	for id := range movable {
		movableIDs = append(movableIDs, id)
	}
	sort.Strings(movableIDs)

	finalComm := make(map[string]string, len(lg.symbolNodes))
	for id, c := range seed {
		finalComm[id] = c
	}
	leidenRestrictedLocalMoves(
		movableIDs, movable, lg.neighbors, lg.degree, lg.totalWeight, finalComm, pace,
	)

	// Everything else (unchanged, non-boundary) already carries its
	// cached community via the seed copy above, untouched.
	result := buildCommunityResult(g, finalComm, lg.neighbors, lg.totalWeight, lg.degree, pace)
	return result, incrementalResult{
		partition: &leidenPartition{
			comm:        finalComm,
			neighbors:   lg.neighbors,
			degree:      lg.degree,
			totalWeight: lg.totalWeight,
			symbolNodes: lg.symbolNodes,
		},
		repartitioned: len(movable) + len(boundary),
	}
}

// leidenRestrictedLocalMoves is leidenFastLocalMoves constrained to
// a movable subset. Only nodes in `movable` are ever relocated;
// every other node keeps the community it carries in `comm` and acts
// as a fixed anchor that movable nodes can be pulled toward. It is
// the same modularity-gain rule and queue-driven wake-up as the full
// pass, so a movable node settles into the modularity-best community
// available to it given the frozen anchors.
//
// Determinism is stronger here than in leidenFastLocalMoves: the
// work queue is seeded in the caller's sorted order, candidate
// communities are evaluated in sorted-key order (so an exact gain
// tie always resolves to the same community), and woken neighbours
// are enqueued in sorted-key order. The incremental path asserts
// reproducibility, so it cannot lean on map iteration order.
//
// The community-membership / sigmaTot bookkeeping spans the whole
// graph (anchors included) because a movable node's gain depends on
// the total degree already sitting in each candidate community.
func leidenRestrictedLocalMoves(
	movableIDs []string,
	movable map[string]bool,
	neighbors map[string]map[string]float64,
	degree map[string]float64,
	totalWeight float64,
	comm map[string]string,
	pace *Pace,
) {
	if totalWeight == 0 || len(movableIDs) == 0 {
		return
	}

	// sigmaTot over every node so anchor degree counts toward the
	// communities movable nodes might join.
	sigmaTot := make(map[string]float64)
	for id, c := range comm {
		sigmaTot[c] += degree[id]
	}

	queue := make([]string, len(movableIDs))
	copy(queue, movableIDs)
	inQueue := make(map[string]bool, len(movableIDs))
	for _, id := range movableIDs {
		inQueue[id] = true
	}

	for len(queue) > 0 {
		pace.Tick()
		id := queue[0]
		queue = queue[1:]
		delete(inQueue, id)

		currentComm := comm[id]
		commWeights := make(map[string]float64)
		for nbr, w := range neighbors[id] {
			commWeights[comm[nbr]] += w
		}
		ki := degree[id]
		kiIn := commWeights[currentComm]
		if loop, ok := neighbors[id][id]; ok {
			kiIn -= loop
		}
		removeDelta := kiIn - (sigmaTot[currentComm]-ki)*ki/(2*totalWeight)

		// Evaluate candidate communities in sorted-key order so a
		// gain tie is broken identically on every run.
		candidates := make([]string, 0, len(commWeights))
		for c := range commWeights {
			candidates = append(candidates, c)
		}
		sort.Strings(candidates)

		bestComm := currentComm
		bestGain := 0.0
		for _, c := range candidates {
			if c == currentComm {
				continue
			}
			gain := commWeights[c] - sigmaTot[c]*ki/(2*totalWeight) - removeDelta
			if gain > bestGain {
				bestGain = gain
				bestComm = c
			}
		}

		if bestComm == currentComm {
			continue
		}

		sigmaTot[currentComm] -= ki
		comm[id] = bestComm
		sigmaTot[bestComm] += ki

		// Wake only movable neighbours — anchors never move, so
		// re-examining them is wasted work. Sorted order keeps the
		// queue evolution deterministic.
		woken := make([]string, 0, len(neighbors[id]))
		for nbr := range neighbors[id] {
			if nbr == id || !movable[nbr] || inQueue[nbr] {
				continue
			}
			woken = append(woken, nbr)
		}
		sort.Strings(woken)
		for _, nbr := range woken {
			queue = append(queue, nbr)
			inQueue[nbr] = true
		}
	}
}
