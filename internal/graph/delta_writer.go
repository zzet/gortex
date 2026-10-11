package graph

import (
	"context"
	"iter"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// DeltaWriter is a graph.Store whose reads are a checkout's composed view and
// whose writes collect the rows that differ from it.
//
// A worktree edit runs the primary checkout's per-save engine (parse the
// changed file, evict its rows, re-add them, rebind the incoming and outgoing
// references, re-derive the frontier's derived families) against this store.
// Every read answers from the view below with the delta applied on top, so the
// engine sees exactly the graph it would see if it had been writing into a
// store holding the checkout's whole state. Every write lands in an in-memory
// working graph (work) that holds only the rows the delta touched:
//
//   - a COVERED path: the complete current node set of the path and every edge
//     recorded at it. A path is covered the first time a write evicts it or
//     adds a node at it; its rows are materialized from the view first, so the
//     working graph describes the whole file, not the part that changed.
//   - a CLAIMED source: the complete current outgoing edge set of one node
//     whose edges are recorded outside every covered path. A source is claimed
//     the first time a write adds, removes or retargets one of those edges, or
//     an eviction deletes one (the store's eviction removes the edges into and
//     out of an evicted node wherever they were recorded).
//   - a DETACHED node: a node carried outside every covered path — a
//     resolver stub with no file. Carrying it claims its outgoing edges too,
//     which is what a published generation's tombstone does.
//
// Materialization never reaches a mutation receipt: the working graph's
// receipts observe the engine's writes only, so the resolver frontier a
// receipt describes is the one the same writes would produce in a store.
//
// Payload turns the working graph into what a generation publishes: replace
// and delete file masks for the covered paths with their rows, edge-source
// markers for the claimed sources whose final edge set differs from what the
// view below shows, and tombstones for the detached nodes that differ from the
// layer below. Claims and detached rows that ended equal to the layer below
// are dropped, so a delta whose writes restored what was there publishes
// nothing for them.
//
// Reads return copies. The view below is shared (the checkout's long-lived
// materialized ancestry memoizes rows), and the per-save engine mutates edges
// in place before handing them to ReindexEdges; a copy keeps that mutation out
// of a cache other readers see, which is the same isolation a SQLite store
// gives by decoding a fresh row per read.
type DeltaWriter struct {
	// fileNodeReads counts the delta's file-node reads
	// (delta_writer_file_node_reads.go).
	fileNodeReads fileNodeReadStats
	// baseCache memoizes the bottom store's projections across deltas over
	// one immutable stack (delta_writer_base_cache.go); nil when none.
	baseCache *BaseProjectionCache
	// chainLayers is how many layers directly below the delta's own belong
	// to the dirty chain above the stack baseCache is kept for
	// (delta_writer_chain_split.go).
	chainLayers int
	chainGens   []int64
	chainEpochs []uint64
	// chainRows keeps the chain's layers' rows across deltas
	// (delta_writer_chain_layer_rows.go); nil reads them per delta.
	chainRows *ChainLayerRows
	below     Reader
	sidecar   any
	// Constant sidecar ownership is independent of detached node enrichment.
	// Successful authoritative deletes retain their file claims in Payload.
	constantOwnedFiles map[ConstantFileKey]bool
	constantReadErr    error
	// belowFileRows answers the payload's per-path comparisons against the
	// view below when installed (SetBelowFileRows).
	belowFileRows BelowFileRows
	// coverServedPaths are the paths coverFromBelowRows served in the
	// coverPaths call in progress (writeMu held).
	coverServedPaths []string

	work  *Graph
	layer *deltaLayer
	view  *OverlaidView

	// writeMu serialises every write, so a claim's read-materialize-mark
	// sequence is never interleaved with another write to the same source.
	writeMu waitTimedMutex
	// resolveMu is ResolveMutex's answer.
	resolveMu sync.Mutex

	// nameIndexes memoizes each immutable layer's name index for batched
	// name reads (delta_writer_compose.go).
	nameIndexMu    sync.Mutex
	nameIndexes    map[OverlayLayerReader]*layerNameIndex
	layerRowCache  map[OverlayLayerReader]*layerRows
	adjacencyCache map[OverlayLayerReader]*layerAdjacencyCache

	// belowOut is the layer below's outgoing set of every claimed source, as
	// read when the source was claimed (the payload compares against it).
	belowOut map[string][]*Edge
	// claimedBelow is, per source with edge claims, the lower rows those
	// claims hide, keyed by identity (delta_writer_edge_claims.go).
	claimedBelow map[string]map[edgeHash]*Edge
	// claimOp names the write in progress, for ClaimsByWrite; set under
	// writeMu by every write entry point.
	claimOp string

	// builtinTargets is every builtin sentinel an edge the delta
	// materialized pointed at: the candidates whose last referrer the delta
	// may have removed (delta_writer_shared.go).
	builtinTargets map[string]struct{}

	statsMu sync.Mutex
	stats   DeltaWriterStats
	// unsupported records every write the delta cannot express. A delta with
	// any entry must not be published; the caller builds the state the old
	// way instead.
	unsupported []string
}

// DeltaWriterStats is what a delta read and wrote, for the edit report.
type DeltaWriterStats struct {
	CoveredPaths      int
	ClaimedSources    int
	MaterializedNodes int
	MaterializedEdges int
	// EdgeClaims counts the edge tuples a file eviction claimed instead of
	// their sources' whole outgoing sets; EdgeClaimSources is the sources
	// still held that way, EdgeClaimsPromoted the ones claimed whole later
	// (at Payload, because their rows changed).
	EdgeClaims         int
	EdgeClaimSources   int
	EdgeClaimsPromoted int
	// IdentityClaims / KindClaimRows count the single rows the later writes
	// claimed by identity and the lower rows kind evictions claimed
	// (delta_writer_row_claims.go).
	IdentityClaims int
	KindClaimRows  int
	// ClaimsByWrite attributes every whole-source claim to the write that
	// made it: "<DeltaWriter method>@<engine caller>" -> sources claimed and
	// edges materialized (delta_writer_claim_attribution.go).
	ClaimsByWrite map[string]ClaimCount
	// WholeLayerLoads / WholeLayerRows count the immutable layers below the
	// delta that had to be read wholesale (a layer with no generation-scoped
	// projection) and the rows that cost.
	WholeLayerLoads int
	WholeLayerRows  int
	// LayerRowsRead counts every row the delta read from the immutable layers
	// below it, wholesale or through their generation-scoped projections.
	LayerRowsRead int
	// LayerRowsByRead splits LayerRowsRead by the read that composed the
	// rows (and the kinds it asked for).
	LayerRowsByRead map[string]int
	// BelowRowsServed counts the rows the payload's comparisons took from an
	// installed BelowFileRows source instead of the layers.
	BelowRowsServed int
	// SlowReads counts reads answered by a scan of the whole view (a
	// repository or corpus listing) because neither the view below nor the
	// delta has a bounded form of it. Each one is a per-save cost that grows
	// with the repository.
	SlowReads map[string]int
}

// NewDeltaWriter opens a delta over below. sidecar, when non-nil, is the
// generation handle the per-file side tables (full-text rows, file metadata,
// reference facts, …) are written through; DeltaWriter forwards the side-table
// capabilities it lists in DeltaWriterCapabilities to it.
func NewDeltaWriter(below Reader, sidecar any) *DeltaWriter {
	dw := &DeltaWriter{below: below, sidecar: sidecar, work: New()}
	dw.layer = &deltaLayer{
		owner:   dw,
		work:    dw.work,
		covered: make(map[string]bool),
		claimed: make(map[string]struct{}),
		removed: make(map[string]struct{}),
	}
	dw.view = NewOverlaidViewWithLayer(below, dw.layer)
	return dw
}

// Below is the reader the delta composes over.
func (dw *DeltaWriter) Below() Reader { return dw.below }

// Sidecar is the handle side tables are written through, nil when none.
func (dw *DeltaWriter) Sidecar() any { return dw.sidecar }

// View is the composed view: below with the delta applied.
func (dw *DeltaWriter) View() Reader { return dw.view }

// Unsupported lists the writes the delta could not express, in order.
func (dw *DeltaWriter) Unsupported() []string {
	dw.statsMu.Lock()
	defer dw.statsMu.Unlock()
	return append([]string(nil), dw.unsupported...)
}

// DeltaStats is a snapshot of what the delta read and wrote so far.
func (dw *DeltaWriter) DeltaStats() DeltaWriterStats {
	dw.statsMu.Lock()
	defer dw.statsMu.Unlock()
	out := dw.stats
	out.ClaimsByWrite = make(map[string]ClaimCount, len(dw.stats.ClaimsByWrite))
	for k, v := range dw.stats.ClaimsByWrite {
		out.ClaimsByWrite[k] = v
	}
	out.LayerRowsByRead = make(map[string]int, len(dw.stats.LayerRowsByRead))
	for k, v := range dw.stats.LayerRowsByRead {
		out.LayerRowsByRead[k] = v
	}
	out.SlowReads = make(map[string]int, len(dw.stats.SlowReads))
	for k, v := range dw.stats.SlowReads {
		out.SlowReads[k] = v
	}
	dw.layer.mu.RLock()
	out.CoveredPaths = len(dw.layer.covered)
	out.ClaimedSources = len(dw.layer.claimed)
	out.EdgeClaimSources = len(dw.layer.edgeClaims)
	dw.layer.mu.RUnlock()
	return out
}

func (dw *DeltaWriter) noteUnsupported(op string) {
	dw.statsMu.Lock()
	dw.unsupported = append(dw.unsupported, op)
	dw.statsMu.Unlock()
}

func (dw *DeltaWriter) noteWholeLayerLoad(rows int) {
	dw.statsMu.Lock()
	dw.stats.WholeLayerLoads++
	dw.stats.WholeLayerRows += rows
	dw.statsMu.Unlock()
}

// noteLayerRowsFor counts rows read from the layers below by the named read.
func (dw *DeltaWriter) noteLayerRowsFor(read string, rows int) {
	if rows == 0 {
		return
	}
	dw.statsMu.Lock()
	dw.stats.LayerRowsRead += rows
	if dw.stats.LayerRowsByRead == nil {
		dw.stats.LayerRowsByRead = make(map[string]int)
	}
	dw.stats.LayerRowsByRead[read] += rows
	dw.statsMu.Unlock()
}

func (dw *DeltaWriter) noteBelowRowsServed(rows int) {
	dw.statsMu.Lock()
	dw.stats.BelowRowsServed += rows
	dw.statsMu.Unlock()
}

// layerRowsRead is the running total of rows read from the layers below.
func (dw *DeltaWriter) layerRowsRead() int {
	dw.statsMu.Lock()
	defer dw.statsMu.Unlock()
	return dw.stats.LayerRowsRead
}

func layerRowKinds(kinds []string) string {
	if len(kinds) == 0 {
		return ""
	}
	sort.Strings(kinds)
	return "[" + strings.Join(kinds, ",") + "]"
}

func nodeKindNames(kinds []NodeKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

func edgeKindNames(kinds []EdgeKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

func (dw *DeltaWriter) noteSlowRead(op string) {
	dw.statsMu.Lock()
	if dw.stats.SlowReads == nil {
		dw.stats.SlowReads = make(map[string]int)
	}
	dw.stats.SlowReads[op]++
	dw.statsMu.Unlock()
}

func (dw *DeltaWriter) noteMaterialized(nodes, edges int) {
	dw.statsMu.Lock()
	dw.stats.MaterializedNodes += nodes
	dw.stats.MaterializedEdges += edges
	dw.statsMu.Unlock()
}

// --- copies -------------------------------------------------------------------

func cloneDeltaNode(n *Node) *Node {
	if n == nil {
		return nil
	}
	c := *n
	if n.Meta != nil {
		c.Meta = make(map[string]any, len(n.Meta))
		for k, v := range n.Meta {
			c.Meta[k] = v
		}
	}
	return &c
}

func cloneDeltaEdge(e *Edge) *Edge {
	if e == nil {
		return nil
	}
	c := *e
	if e.Meta != nil {
		c.Meta = make(map[string]any, len(e.Meta))
		for k, v := range e.Meta {
			c.Meta[k] = v
		}
	}
	return &c
}

func cloneDeltaNodes(in []*Node) []*Node {
	if in == nil {
		return nil
	}
	out := make([]*Node, 0, len(in))
	for _, n := range in {
		if n != nil {
			out = append(out, cloneDeltaNode(n))
		}
	}
	return out
}

func cloneDeltaEdges(in []*Edge) []*Edge {
	if in == nil {
		return nil
	}
	out := make([]*Edge, 0, len(in))
	for _, e := range in {
		if e != nil {
			out = append(out, cloneDeltaEdge(e))
		}
	}
	return out
}

// --- materialization ----------------------------------------------------------

// materializeRows inserts rows read from the view into the working graph
// without reaching any open mutation receipt: they are state the view already
// holds, not writes. Existing rows under the same identity are kept — the
// working graph's copy is the current one.
func (g *Graph) materializeRows(nodes []*Node, edges []*Edge) {
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	nodesByShard := make([][]*Node, g.shardCount)
	outByShard := make([][]*Edge, g.shardCount)
	inByShard := make([][]*Edge, g.shardCount)
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		i := g.shardIdx(n.ID)
		nodesByShard[i] = append(nodesByShard[i], n)
	}
	for _, e := range edges {
		if e == nil || e.From == "" {
			continue
		}
		outByShard[g.shardIdx(e.From)] = append(outByShard[g.shardIdx(e.From)], e)
	}
	for i := range g.shards {
		if len(nodesByShard[i]) == 0 && len(outByShard[i]) == 0 {
			continue
		}
		s := g.shards[i]
		s.mu.Lock()
		for _, n := range nodesByShard[i] {
			if _, exists := s.nodes[n.ID]; exists {
				continue
			}
			g.addNodeLocked(s, n)
		}
		for _, e := range outByShard[i] {
			h := hashEdgeKey(keyOf(e))
			if positions := s.outEdgeIdx[e.From]; positions != nil {
				if _, exists := positions[h]; exists {
					continue
				}
			}
			inserted, _ := addEdgeToBucket(s.outEdges, s.outEdgeKeys, s.outEdgeIdx, e.From, e)
			if inserted {
				var srcRepo string
				if src, ok := s.nodes[e.From]; ok && src != nil {
					srcRepo = src.RepoPrefix
				}
				s.repoEdgeAdd(srcRepo, e)
				inByShard[g.shardIdx(e.To)] = append(inByShard[g.shardIdx(e.To)], e)
			}
		}
		s.mu.Unlock()
	}
	for i := range g.shards {
		if len(inByShard[i]) == 0 {
			continue
		}
		s := g.shards[i]
		s.mu.Lock()
		for _, e := range inByShard[i] {
			addEdgeToBucket(s.inEdges, s.inEdgeKeys, s.inEdgeIdx, e.To, e)
		}
		s.mu.Unlock()
	}
	g.nodeMutGen.Add(1)
	g.edgeMutGen.Add(1)
}

// coverPaths makes each path a covered path of the delta: its current nodes
// and every edge recorded at it are copied into the working graph, then the
// layer claims the path. A path that is already covered is left alone.
// Callers hold writeMu.
func (dw *DeltaWriter) coverPaths(paths []string) {
	var fresh []string
	dw.layer.mu.RLock()
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, done := dw.layer.covered[p]; done {
			continue
		}
		fresh = append(fresh, p)
	}
	dw.layer.mu.RUnlock()
	fresh = UniqueRecordingPaths(fresh)
	if len(fresh) == 0 {
		return
	}
	var nodes []*Node
	var edges []*Edge
	// While the delta has written and claimed nothing, the composed view at
	// a path is the view below's, which the stack's below-rows source keeps
	// (delta_writer_cover_below.go): the first eviction of a delta — the
	// changed files' own — is served from it instead of scanning the stack.
	fresh, nodes, edges = dw.coverFromBelowRows(fresh)
	if len(fresh) == 0 {
		dw.noteBuiltinTargets(edges)
		dw.layer.mu.Lock()
		dw.work.materializeRows(nodes, edges)
		for _, p := range dw.coverServedPaths {
			dw.layer.covered[p] = false
		}
		dw.coverServedPaths = nil
		dw.layer.mu.Unlock()
		dw.noteMaterialized(len(nodes), len(edges))
		return
	}
	for _, p := range fresh {
		nodes = append(nodes, cloneDeltaNodes(dw.view.GetFileNodes(p))...)
	}
	if recorded, ok := RecordedEdgesOf(dw.view); ok {
		edges = append(edges, cloneDeltaEdges(recorded.RecordedEdgesAt(fresh))...)
	} else {
		// Without a by-file reader the recorded set is approximated by the
		// edges the path's own nodes hold there; a source outside the path
		// recording an edge at it stays in the layer below.
		dw.noteSlowRead("RecordedEdgesAt")
		want := make(map[string]struct{}, len(fresh))
		for _, p := range fresh {
			want[p] = struct{}{}
		}
		ids := make([]string, 0, len(nodes))
		for _, n := range nodes {
			ids = append(ids, n.ID)
		}
		for _, out := range dw.view.GetOutEdgesByNodeIDs(ids) {
			for _, e := range out {
				if e == nil {
					continue
				}
				if _, here := want[e.FilePath]; here {
					edges = append(edges, cloneDeltaEdge(e))
				}
			}
		}
	}
	dw.noteBuiltinTargets(edges)
	dw.layer.mu.Lock()
	dw.work.materializeRows(nodes, edges)
	for _, p := range fresh {
		dw.layer.covered[p] = false
	}
	for _, p := range dw.coverServedPaths {
		dw.layer.covered[p] = false
	}
	dw.coverServedPaths = nil
	dw.layer.mu.Unlock()
	dw.noteMaterialized(len(nodes), len(edges))
}

// claimSources makes the delta the authority for each source's complete
// outgoing edge set: the edges the view shows out of it are copied into the
// working graph, then the layer claims the source. Callers hold writeMu.
func (dw *DeltaWriter) claimSources(ids []string) {
	var fresh []string
	seen := make(map[string]struct{}, len(ids))
	dw.layer.mu.RLock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if _, done := dw.layer.claimed[id]; done {
			continue
		}
		fresh = append(fresh, id)
	}
	dw.layer.mu.RUnlock()
	if len(fresh) == 0 {
		return
	}
	sort.Strings(fresh)
	// The view's outgoing set of an unclaimed source is the layer below's,
	// less what the delta already speaks for (edges recorded at a covered
	// path, which the working graph holds, and edges to an identity it
	// removed). Reading the layer below directly keeps its rows for the
	// payload's comparison, which would otherwise read them again.
	below, ok := dw.composedEdgesByNodeIDs(fresh, false, false)
	if !ok {
		below = dw.below.GetOutEdgesByNodeIDs(fresh)
	}
	own := deltaStack{layers: []OverlayLayerReader{dw.layer}}
	var edges []*Edge
	if dw.belowOut == nil {
		dw.belowOut = make(map[string][]*Edge, len(fresh))
	}
	for _, id := range fresh {
		rows := below[id]
		dw.belowOut[id] = rows
		for _, e := range rows {
			if e != nil && own.edgeVisibleFrom(e, 0) {
				edges = append(edges, cloneDeltaEdge(e))
			}
		}
	}
	dw.noteBuiltinTargets(edges)
	dw.layer.mu.Lock()
	dw.work.materializeRows(nil, edges)
	for _, id := range fresh {
		dw.layer.claimed[id] = struct{}{}
	}
	// The materialization above left the edge-claimed rows out (the working
	// graph already holds the delta's rows for them); the whole claim now
	// speaks for them.
	dw.dropEdgeClaimsLocked(fresh)
	dw.layer.mu.Unlock()
	dw.noteMaterialized(0, len(edges))
	dw.noteClaim(fresh, len(edges))
}

// edgeHome reports whether an edge's row lives in the working graph without a
// claim: it is recorded at a covered path, or its source is already claimed.
func (dw *DeltaWriter) edgeHome(from, filePath string) bool {
	dw.layer.mu.RLock()
	defer dw.layer.mu.RUnlock()
	if filePath != "" {
		if _, ok := dw.layer.covered[filePath]; ok {
			return true
		}
	}
	_, ok := dw.layer.claimed[from]
	return ok
}

// prepareEdgeWrite makes the working graph hold the row an edge write
// addresses. Callers hold writeMu.
func (dw *DeltaWriter) prepareEdgeWrite(from, filePath string) {
	if from == "" || dw.edgeHome(from, filePath) {
		return
	}
	dw.claimSources([]string{from})
}

// prepareNodeWrite makes the working graph the authority for a node row. A
// node at a path covers the path; a node with no path is a detached identity
// and claims its adjacency. Callers hold writeMu.
func (dw *DeltaWriter) prepareNodeWrite(n *Node) {
	if n == nil || n.ID == "" {
		return
	}
	if n.FilePath != "" {
		dw.coverPaths([]string{n.FilePath})
		return
	}
	dw.claimSources([]string{n.ID})
}

// --- writes -------------------------------------------------------------------

// AddNode implements Store.
func (dw *DeltaWriter) AddNode(n *Node) {
	if n == nil {
		return
	}
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("AddNode")()
	dw.prepareNodeWrite(n)
	if len(dw.keepBelowSharedCopies([]*Node{n})) == 0 {
		return
	}
	dw.work.AddNode(n)
}

// AddBatch implements Store.
func (dw *DeltaWriter) AddBatch(nodes []*Node, edges []*Edge) {
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("AddBatch")()
	var paths, sources []string
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		if n.FilePath != "" {
			paths = append(paths, n.FilePath)
		} else {
			sources = append(sources, n.ID)
		}
	}
	dw.coverPaths(paths)
	var fresh []edgeKey
	for _, e := range edges {
		if e == nil || e.From == "" {
			continue
		}
		if !dw.edgeHome(e.From, e.FilePath) && !dw.edgeWriteClaimed(e) {
			fresh = append(fresh, keyOf(e))
		}
	}
	// A row new to a source the delta does not hold is claimed by identity
	// when the view below holds no row under it (claimNewRows).
	sources = append(sources, dw.claimNewRows(fresh)...)
	dw.claimSources(sources)
	dw.suppressBelowBuiltins(edges)
	dw.noteImportSources(edges)
	dw.work.AddBatch(dw.keepBelowSharedCopies(nodes), edges)
}

// suppressBelowBuiltins keeps the working graph from lazily materializing a
// builtin sentinel the view below already holds: the lazily made row carries
// no repository stamps, and as a working-graph row it would own the identity
// and shadow the stamped one below. Callers hold writeMu.
func (dw *DeltaWriter) suppressBelowBuiltins(edges []*Edge) {
	stubs := BuiltinStubNodes(edges)
	if len(stubs) == 0 {
		return
	}
	ids := make([]string, 0, len(stubs))
	for _, stub := range stubs {
		if _, seen := dw.work.builtinSeen.Load(stub.ID); !seen {
			ids = append(ids, stub.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	for id := range dw.below.GetNodesByIDs(ids) {
		dw.work.builtinSeen.Store(id, struct{}{})
	}
}

// AddEdge implements Store.
func (dw *DeltaWriter) AddEdge(e *Edge) {
	if e == nil {
		return
	}
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("AddEdge")()
	if !dw.edgeHome(e.From, e.FilePath) && !dw.edgeWriteClaimed(e) {
		dw.claimSources(dw.claimNewRows([]edgeKey{keyOf(e)}))
	}
	dw.noteImportSources([]*Edge{e})
	dw.work.AddEdge(e)
}

// SetEdgeProvenance implements Store against the stored row: the in-memory
// store updates the pointer it is handed, and the engine hands in copies.
func (dw *DeltaWriter) SetEdgeProvenance(e *Edge, newOrigin string) bool {
	if e == nil {
		return false
	}
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("SetEdgeProvenance")()
	dw.claimSources(dw.claimRows([]edgeKey{keyOf(e)}, true))
	stored := dw.work.storedEdge(keyOf(e))
	if stored == nil {
		return false
	}
	changed := dw.work.SetEdgeProvenance(stored, newOrigin)
	if changed {
		e.Origin = stored.Origin
		e.Tier = stored.Tier
	}
	return changed
}

// ReindexEdge implements Store.
func (dw *DeltaWriter) ReindexEdge(e *Edge, oldTo string) {
	if e == nil {
		return
	}
	dw.ReindexEdges([]EdgeReindex{{Edge: e, OldTo: oldTo}})
}

// ReindexEdges implements Store. The engine hands in edges it read, which
// are copies; the working graph's in-memory reindex keeps the stored row's
// pointer and expects the caller to have mutated it, so the new content is
// copied into the stored row first. A retarget of a row the delta does not
// hold is a no-op, as it is in a store.
func (dw *DeltaWriter) ReindexEdges(batch []EdgeReindex) {
	if len(batch) == 0 {
		return
	}
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("ReindexEdges")()
	// A retarget, a refresh or a move of one row is claimed by the row's old
	// and new identities (delta_writer_row_claims.go); a row the claims
	// cannot express claims its sources whole.
	var sources []string
	var oldKeys, newKeys []edgeKey
	for _, r := range batch {
		if r.Edge == nil {
			continue
		}
		oldFrom, oldPath, oldLine := r.Edge.From, r.Edge.FilePath, r.Edge.Line
		if r.RefreshIdentity {
			if r.OldFrom != "" {
				oldFrom = r.OldFrom
			}
			oldPath, oldLine = r.OldFilePath, r.OldLine
		}
		oldKind := r.OldKind
		if oldKind == "" {
			oldKind = r.Edge.Kind
		}
		// A move between sources or places is two rows: the old identity
		// removed, the new one created; both are claimed by identity.
		if r.OldTo != "" {
			oldKeys = append(oldKeys, edgeKey{From: oldFrom, To: r.OldTo, Kind: oldKind, FilePath: oldPath, Line: oldLine})
			newKeys = append(newKeys, keyOf(r.Edge))
			continue
		}
		if !dw.edgeHome(oldFrom, oldPath) {
			sources = append(sources, oldFrom)
		}
		if !dw.edgeHome(r.Edge.From, r.Edge.FilePath) {
			sources = append(sources, r.Edge.From)
		}
	}
	sources = append(sources, dw.claimRows(oldKeys, true)...)
	sources = append(sources, dw.claimNewRows(newKeys)...)
	dw.claimSources(sources)
	for _, r := range batch {
		if r.Edge != nil && r.Edge.Kind == EdgeImports {
			dw.noteImportSources([]*Edge{r.Edge})
		}
	}
	var refresh []EdgeReindex
	for _, r := range batch {
		if r.Edge == nil {
			continue
		}
		if r.RefreshIdentity {
			refresh = append(refresh, EdgeReindex{
				Edge: cloneDeltaEdge(r.Edge), OldFrom: r.OldFrom, OldTo: r.OldTo, OldKind: r.OldKind,
				RefreshIdentity: true, OldFilePath: r.OldFilePath, OldLine: r.OldLine,
			})
			continue
		}
		oldKind := r.OldKind
		if oldKind == "" {
			oldKind = r.Edge.Kind
		}
		dw.work.reindexStoredEdge(r.Edge, r.OldTo, oldKind)
	}
	for _, r := range refresh {
		dw.work.refreshStoredEdgeIdentity(r)
	}
}

// refreshStoredEdgeIdentity applies a source-span or metadata refresh (an
// EdgeReindex with RefreshIdentity) to the working graph and describes it to
// the active receipts the way a disk store does: the rewritten row is an
// inserted row, which is work for the resolver only when it is left at an
// unresolved target. The in-memory graph's own refresh voids every active
// receipt instead, which would turn the engine's exact resolver frontier into
// a whole-graph resolution a disk store never runs.
func (g *Graph) refreshStoredEdgeIdentity(r EdgeReindex) {
	e := r.Edge
	if e == nil {
		return
	}
	oldKind := r.OldKind
	if oldKind == "" {
		oldKind = e.Kind
	}
	oldFrom := r.OldFrom
	if oldFrom == "" {
		oldFrom = e.From
	}
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	if oldFrom != e.From {
		g.moveEdgeIdentity(e, oldFrom, r.OldTo, oldKind, r.OldFilePath, r.OldLine)
	} else {
		g.refreshSameSourceEdge(e, r.OldTo, oldKind, r.OldFilePath, r.OldLine)
	}
	g.edgeMutGen.Add(1)
	g.recordReindexedEdgeForReceipts(e)
}

// refreshSameSourceEdge is refreshEdgeIdentity's same-source arm without the
// receipt voiding: the stored row takes e's content and moves between the
// identity indexes when its key changed.
func (g *Graph) refreshSameSourceEdge(e *Edge, oldTo string, oldKind EdgeKind, oldFilePath string, oldLine int) {
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
	content := cloneDeltaEdge(e)
	if oldKey != newKey || oldTo != e.To {
		delete(fromIdx, oldKey)
		fromIdx[newKey] = pos
		if keys := sFrom.outEdgeKeys[e.From]; pos < len(keys) {
			keys[pos] = newKey
		}
		sOld := g.shardFor(oldTo)
		removeEdgeFromBucket(sOld.inEdges, sOld.inEdgeKeys, sOld.inEdgeIdx, oldTo, oldKey)
		*stored = *content
		sNew := g.shardFor(e.To)
		addEdgeToBucket(sNew.inEdges, sNew.inEdgeKeys, sNew.inEdgeIdx, e.To, stored)
		g.edgeIdentityRevisions.Add(1)
		return
	}
	*stored = *content
}

// storedEdge returns the working graph's row under an exact identity, nil
// when it holds none.
func (g *Graph) storedEdge(k edgeKey) *Edge {
	s := g.shardFor(k.From)
	s.mu.RLock()
	defer s.mu.RUnlock()
	positions := s.outEdgeIdx[k.From]
	if positions == nil {
		return nil
	}
	pos, ok := positions[hashEdgeKey(k)]
	if !ok || pos >= len(s.outEdges[k.From]) {
		return nil
	}
	return s.outEdges[k.From][pos]
}

// reindexStoredEdge applies a To/Kind retarget, or a same-key payload update,
// described by a copy of the row: the stored row takes the copy's content
// and then moves in the indexes exactly as reindexEdge moves a row its caller
// mutated in place.
func (g *Graph) reindexStoredEdge(e *Edge, oldTo string, oldKind EdgeKind) {
	stored := g.storedEdge(edgeKey{From: e.From, To: oldTo, Kind: oldKind, FilePath: e.FilePath, Line: e.Line})
	if stored == nil {
		return
	}
	if stored != e {
		s := g.shardFor(e.From)
		s.mu.Lock()
		content := cloneDeltaEdge(e)
		*stored = *content
		s.mu.Unlock()
	}
	g.reindexEdge(stored, oldTo, oldKind)
	g.edgeMutGen.Add(1)
}

// SetEdgeProvenanceBatch implements Store.
func (dw *DeltaWriter) SetEdgeProvenanceBatch(batch []EdgeProvenanceUpdate) int {
	changed := 0
	for _, u := range batch {
		if u.Edge != nil && dw.SetEdgeProvenance(u.Edge, u.NewOrigin) {
			changed++
		}
	}
	return changed
}

// RemoveEdge implements Store. The first edge the view shows from `from` to
// `to` of the kind is the one removed, as in the in-memory store.
func (dw *DeltaWriter) RemoveEdge(from, to string, kind EdgeKind) bool {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("RemoveEdge")()
	var target *Edge
	for _, e := range dw.view.GetOutEdges(from) {
		if e != nil && e.To == to && e.Kind == kind {
			target = e
			break
		}
	}
	if target == nil {
		return false
	}
	dw.prepareEdgeWrite(from, target.FilePath)
	return dw.work.RemoveEdge(from, to, kind)
}

// EvictFile implements Store with the store's semantics: the path's nodes go,
// and so does every edge into or out of one of them, wherever it was
// recorded. The sources of those edges are claimed first so the removal is a
// row difference the payload can publish.
func (dw *DeltaWriter) EvictFile(filePath string) (int, int) {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("EvictFile")()
	dw.prepareEviction([]string{filePath})
	return dw.work.EvictFile(filePath)
}

// EvictFiles is the batched eviction (FileBatchEvicter).
func (dw *DeltaWriter) EvictFiles(filePaths []string) (int, int) {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("EvictFiles")()
	dw.prepareEviction(filePaths)
	nodes, edges := 0, 0
	for _, p := range filePaths {
		n, e := dw.work.EvictFile(p)
		nodes += n
		edges += e
	}
	return nodes, edges
}

// prepareEviction covers the paths and claims every edge outside them the
// eviction removes: the store's eviction removes every edge into an evicted
// node wherever it was recorded. An edge the working graph already holds (its
// source is claimed, or its path covered) is removed there; every other one is
// edge-claimed (delta_writer_edge_claims.go) — the delta speaks for that row
// and holds none for it, so the removal is a row difference — without reading
// its source's other edges. The cost is the evicted nodes' incident edges.
//
// The evicted nodes' own outgoing edges recorded at other paths are NOT
// claimed. The published composition settles an edge by the path it was
// recorded at, and a surviving identity keeps the edges other files recorded
// out of it — the edges a caller's file records for a value it takes from the
// evicted file are the caller's, and a whole index of the edited tree keeps
// them. Leaving them unclaimed keeps them in the view below exactly as the
// published generation will serve them. Callers hold writeMu.
func (dw *DeltaWriter) prepareEviction(paths []string) {
	dw.coverPaths(paths)
	evicted := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		evicted[p] = struct{}{}
	}
	var ids []string
	for _, p := range paths {
		for _, n := range dw.work.GetFileNodes(p) {
			if n != nil {
				ids = append(ids, n.ID)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	in, ok := dw.composedEdgesByNodeIDs(ids, true, true)
	if !ok {
		in = dw.view.GetInEdgesByNodeIDs(ids)
	}
	var rows []*Edge
	for _, in := range in {
		for _, e := range in {
			if e == nil {
				continue
			}
			if _, inside := evicted[deltaPathKey(e.From)]; inside {
				continue
			}
			if _, recordedInside := evicted[e.FilePath]; recordedInside {
				continue
			}
			if dw.edgeHome(e.From, e.FilePath) {
				continue
			}
			rows = append(rows, e)
		}
	}
	dw.claimEdges(rows)
}

// EvictRepo implements Store. A delta describes a file-level change; a whole
// repository eviction is not one, so it is refused and recorded.
func (dw *DeltaWriter) EvictRepo(repoPrefix string) (int, int) {
	dw.noteUnsupported("EvictRepo")
	return 0, 0
}

// --- receipts -----------------------------------------------------------------

// BeginMutationReceipt implements MutationReceiptStore over the working
// graph: only the engine's writes reach it, never materialization.
func (dw *DeltaWriter) BeginMutationReceipt() MutationReceiptToken {
	return dw.work.BeginMutationReceipt()
}

// EndMutationReceipt implements MutationReceiptStore.
func (dw *DeltaWriter) EndMutationReceipt(token MutationReceiptToken) MutationReceipt {
	return dw.work.EndMutationReceipt(token)
}

// RecordMutationFanoutTruncation implements MutationFanoutRecorder.
func (dw *DeltaWriter) RecordMutationFanoutTruncation(fact ReceiptFanoutTruncation) {
	dw.work.RecordMutationFanoutTruncation(fact)
}

// --- reads --------------------------------------------------------------------

// GetNode implements Store.
func (dw *DeltaWriter) GetNode(id string) *Node { return cloneDeltaNode(dw.view.GetNode(id)) }

// GetNodeByQualName implements Store.
func (dw *DeltaWriter) GetNodeByQualName(q string) *Node {
	return cloneDeltaNode(dw.view.GetNodeByQualName(q))
}

// GetNodesByQualNames implements Store.
func (dw *DeltaWriter) GetNodesByQualNames(qualNames []string) map[string][]*Node {
	in := dw.view.GetNodesByQualNames(qualNames)
	out := make(map[string][]*Node, len(in))
	for k, v := range in {
		out[k] = cloneDeltaNodes(v)
	}
	return out
}

// FindNodesByName implements Store.
func (dw *DeltaWriter) FindNodesByName(name string) []*Node {
	return cloneDeltaNodes(dw.view.FindNodesByName(name))
}

// FindNodesByNameInRepo implements Store.
func (dw *DeltaWriter) FindNodesByNameInRepo(name, repoPrefix string) []*Node {
	var out []*Node
	for _, n := range dw.view.FindNodesByName(name) {
		if n != nil && n.RepoPrefix == repoPrefix {
			out = append(out, cloneDeltaNode(n))
		}
	}
	return out
}

// FindNodesByNameContaining implements Store.
func (dw *DeltaWriter) FindNodesByNameContaining(substr string, limit int) []*Node {
	return cloneDeltaNodes(dw.view.FindNodesByNameContaining(substr, limit))
}

// FindNodesByNameContainingFilteredContext preserves the composed view's scope
// and mask-before-limit behavior while protecting the delta's mutable payloads.
func (dw *DeltaWriter) FindNodesByNameContainingFilteredContext(ctx context.Context, substr string, limit int, filter NameSearchFilter) ([]*Node, error) {
	nodes, err := dw.view.FindNodesByNameContainingFilteredContext(ctx, substr, limit, filter)
	return cloneDeltaNodes(nodes), err
}

// GetFileNodes implements Store.
func (dw *DeltaWriter) GetFileNodes(filePath string) []*Node {
	defer dw.noteFileNodeRead(time.Now())
	return cloneDeltaNodes(dw.view.GetFileNodes(filePath))
}

// GetFileNodesByPaths implements Store.
func (dw *DeltaWriter) GetFileNodesByPaths(filePaths []string) map[string][]*Node {
	defer dw.noteFileNodeRead(time.Now())
	return dw.getFileNodesByPaths(filePaths)
}

// getFileNodesByPaths is GetFileNodesByPaths without the read count, for the
// delta's own composite reads (each counted once, by its caller).
func (dw *DeltaWriter) getFileNodesByPaths(filePaths []string) map[string][]*Node {
	if composed, ok := dw.composedFileNodesByPaths(filePaths); ok {
		out := make(map[string][]*Node, len(composed))
		for p, nodes := range composed {
			out[p] = cloneDeltaNodes(nodes)
		}
		return out
	}
	out := make(map[string][]*Node, len(filePaths))
	for _, p := range UniqueRecordingPaths(filePaths) {
		if p == "" {
			continue
		}
		if nodes := dw.view.GetFileNodes(p); len(nodes) > 0 {
			out[p] = cloneDeltaNodes(nodes)
		}
	}
	return out
}

// GetRepoNodes implements Store.
func (dw *DeltaWriter) GetRepoNodes(repoPrefix string) []*Node {
	dw.noteSlowRead("GetRepoNodes")
	return cloneDeltaNodes(dw.view.GetRepoNodes(repoPrefix))
}

// GetRepoNodesByLanguage implements Store.
func (dw *DeltaWriter) GetRepoNodesByLanguage(repoPrefix, language string) []*Node {
	dw.noteSlowRead("GetRepoNodesByLanguage")
	var out []*Node
	for _, n := range dw.view.GetRepoNodes(repoPrefix) {
		if n != nil && n.Language == language {
			out = append(out, cloneDeltaNode(n))
		}
	}
	return out
}

// GetNodesByLanguage implements Store.
func (dw *DeltaWriter) GetNodesByLanguage(language string) []*Node {
	dw.noteSlowRead("GetNodesByLanguage")
	var out []*Node
	for _, n := range dw.view.AllNodes() {
		if n != nil && n.Language == language {
			out = append(out, cloneDeltaNode(n))
		}
	}
	return out
}

// GetRepoNonContentNodes implements Store. Empty prefix is the wildcard, as in
// the in-memory store.
func (dw *DeltaWriter) GetRepoNonContentNodes(repoPrefix string) []*Node {
	dw.noteSlowRead("GetRepoNonContentNodes")
	var src []*Node
	if repoPrefix == "" {
		src = dw.view.AllNodes()
	} else {
		src = dw.view.GetRepoNodes(repoPrefix)
	}
	var out []*Node
	for _, n := range src {
		if n != nil && !IsContentNode(n) {
			out = append(out, cloneDeltaNode(n))
		}
	}
	return out
}

// GetOutEdges implements Store.
func (dw *DeltaWriter) GetOutEdges(nodeID string) []*Edge {
	return cloneDeltaEdges(dw.view.GetOutEdges(nodeID))
}

// GetInEdges implements Store.
func (dw *DeltaWriter) GetInEdges(nodeID string) []*Edge {
	return cloneDeltaEdges(dw.view.GetInEdges(nodeID))
}

// GetInEdgesByNodeIDs implements Store.
func (dw *DeltaWriter) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	in, ok := dw.composedEdgesByNodeIDs(ids, true, true)
	if !ok {
		in = dw.view.GetInEdgesByNodeIDs(ids)
	}
	out := make(map[string][]*Edge, len(in))
	for k, v := range in {
		out[k] = cloneDeltaEdges(v)
	}
	return out
}

// GetOutEdgesByNodeIDs implements Store.
func (dw *DeltaWriter) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	in, ok := dw.composedEdgesByNodeIDs(ids, false, true)
	if !ok {
		in = dw.view.GetOutEdgesByNodeIDs(ids)
	}
	out := make(map[string][]*Edge, len(in))
	for k, v := range in {
		out[k] = cloneDeltaEdges(v)
	}
	return out
}

// getEdgeCandidatesByAdjacency answers a candidate batch from the sources'
// composed out-edges (GetEdgeCandidates' form for a bottom store with no
// candidate probe).
func (dw *DeltaWriter) getEdgeCandidatesByAdjacency(endpoints []EdgeEndpoint, sites []EdgeSite) EdgeCandidateSet {
	out := NewEdgeCandidateSet()
	endpointWanted := make(map[EdgeEndpoint]struct{}, len(endpoints))
	siteWanted := make(map[EdgeSite]struct{}, len(sites))
	var sources []string
	for _, key := range endpoints {
		if key.From == "" || key.To == "" {
			continue
		}
		endpointWanted[key] = struct{}{}
		sources = append(sources, key.From)
	}
	for _, key := range sites {
		if key.From == "" {
			continue
		}
		siteWanted[key] = struct{}{}
		sources = append(sources, key.From)
	}
	if len(sources) == 0 {
		return out
	}
	for _, edges := range dw.GetOutEdgesByNodeIDs(sources) {
		for _, edge := range edges {
			if edge == nil {
				continue
			}
			if _, ok := endpointWanted[EdgeEndpoint{From: edge.From, To: edge.To}]; ok {
				out.AddEndpoint(edge)
			}
			exactSite := EdgeSite{From: edge.From, Line: edge.Line, Kind: edge.Kind}
			anySite := EdgeSite{From: edge.From, Line: edge.Line}
			if _, ok := siteWanted[exactSite]; ok {
				out.AddSite(edge)
			} else if _, ok := siteWanted[anySite]; ok {
				out.AddSite(edge)
			}
		}
	}
	return out
}

// DistinctExternalTargets implements Store by a kind-bounded scan.
func (dw *DeltaWriter) DistinctExternalTargets(kinds []EdgeKind) []string {
	dw.noteSlowRead("DistinctExternalTargets")
	targets := make(map[string]struct{})
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		for e := range dw.view.EdgesByKind(kind) {
			if e != nil && isExternalTargetID(e.To) {
				targets[e.To] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(targets))
	for t := range targets {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// GetRepoEdges implements Store: every edge whose source node belongs to the
// repository.
func (dw *DeltaWriter) GetRepoEdges(repoPrefix string) []*Edge {
	if repoPrefix == "" {
		return nil
	}
	dw.noteSlowRead("GetRepoEdges")
	nodes := dw.view.GetRepoNodes(repoPrefix)
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			ids = append(ids, n.ID)
		}
	}
	var out []*Edge
	for _, edges := range dw.view.GetOutEdgesByNodeIDs(ids) {
		out = append(out, cloneDeltaEdges(edges)...)
	}
	return out
}

// AllNodes implements Store.
func (dw *DeltaWriter) AllNodes() []*Node {
	dw.noteSlowRead("AllNodes")
	return cloneDeltaNodes(dw.view.AllNodes())
}

// AllEdges implements Store.
func (dw *DeltaWriter) AllEdges() []*Edge {
	dw.noteSlowRead("AllEdges")
	return cloneDeltaEdges(dw.view.AllEdges())
}

// EdgesByKind implements Store.
func (dw *DeltaWriter) EdgesByKind(kind EdgeKind) iter.Seq[*Edge] {
	return func(yield func(*Edge) bool) {
		rows, ok := dw.composedEdgesByKind(kind)
		if !ok {
			dw.noteSlowRead("EdgesByKind")
			for e := range dw.view.EdgesByKind(kind) {
				if !yield(cloneDeltaEdge(e)) {
					return
				}
			}
			return
		}
		for _, e := range rows {
			if !yield(cloneDeltaEdge(e)) {
				return
			}
		}
	}
}

func (dw *DeltaWriter) NodesByKind(kind NodeKind) iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		rows, ok := dw.composedNodesByKind(kind)
		if !ok {
			dw.noteSlowRead("NodesByKind")
			for n := range dw.view.NodesByKind(kind) {
				if !yield(cloneDeltaNode(n)) {
					return
				}
			}
			return
		}
		for _, n := range rows {
			if !yield(cloneDeltaNode(n)) {
				return
			}
		}
	}
}

// EdgesWithUnresolvedTarget implements Store by a scan of the composed edges.
func (dw *DeltaWriter) EdgesWithUnresolvedTarget() iter.Seq[*Edge] {
	dw.noteSlowRead("EdgesWithUnresolvedTarget")
	return func(yield func(*Edge) bool) {
		for _, e := range dw.view.AllEdges() {
			if e == nil || !IsUnresolvedTarget(e.To) || IsFnValuePlaceholder(e.To) {
				continue
			}
			if !yield(cloneDeltaEdge(e)) {
				return
			}
		}
	}
}

// GetNodesByIDs implements Store.
func (dw *DeltaWriter) GetNodesByIDs(ids []string) map[string]*Node {
	in := dw.view.GetNodesByIDs(ids)
	out := make(map[string]*Node, len(in))
	for k, v := range in {
		if v != nil {
			out[k] = cloneDeltaNode(v)
		}
	}
	return out
}

// FindNodesByNames implements Store.
func (dw *DeltaWriter) FindNodesByNames(names []string) map[string][]*Node {
	if composed, ok := dw.composedFindNodesByNames(UniqueRecordingPaths(names)); ok {
		out := make(map[string][]*Node, len(composed))
		for name, nodes := range composed {
			out[name] = cloneDeltaNodes(nodes)
		}
		return out
	}
	out := make(map[string][]*Node, len(names))
	for _, name := range UniqueRecordingPaths(names) {
		if name == "" {
			continue
		}
		if hits := dw.view.FindNodesByName(name); len(hits) > 0 {
			out[name] = cloneDeltaNodes(hits)
		}
	}
	return out
}

// NodeCount implements Store.
func (dw *DeltaWriter) NodeCount() int { return dw.view.NodeCount() }

// EdgeCount implements Store.
func (dw *DeltaWriter) EdgeCount() int {
	dw.noteSlowRead("EdgeCount")
	return dw.view.EdgeCount()
}

// ProxyNodeCountAtLeast implements Store.
func (dw *DeltaWriter) ProxyNodeCountAtLeast(limit int) bool {
	return dw.view.NodeCount() >= limit
}

// Stats implements Store.
func (dw *DeltaWriter) Stats() GraphStats { return dw.view.Stats() }

// RepoStats implements Store.
func (dw *DeltaWriter) RepoStats() map[string]GraphStats { return dw.view.RepoStats() }

// RepoPrefixes implements Store.
func (dw *DeltaWriter) RepoPrefixes() []string {
	if dw.baseCache != nil {
		// The bottom store's listing is a distinct scan; the stack's is kept.
		return append([]string(nil), dw.baseCache.stackRepoPrefixes(dw.repoPrefixesOfBase)...)
	}
	return dw.repoPrefixesOfBase()
}

func (dw *DeltaWriter) repoPrefixesOfBase() []string {
	if lister, ok := innermostReader(dw.below).(interface{ RepoPrefixes() []string }); ok {
		return lister.RepoPrefixes()
	}
	var out []string
	for prefix := range dw.view.RepoStats() {
		if prefix != "" {
			out = append(out, prefix)
		}
	}
	sort.Strings(out)
	return out
}

// EdgeIdentityRevisions implements Store.
func (dw *DeltaWriter) EdgeIdentityRevisions() int {
	return dw.view.EdgeIdentityRevisions() + dw.work.EdgeIdentityRevisions()
}

// VerifyEdgeIdentities implements Store. The delta's own rows are the working
// graph's; the view below was verified when it was published.
func (dw *DeltaWriter) VerifyEdgeIdentities() error { return dw.work.VerifyEdgeIdentities() }

// RepoMemoryEstimate implements Store from the innermost store's counters.
func (dw *DeltaWriter) RepoMemoryEstimate(repoPrefix string) RepoMemoryEstimate {
	if est, ok := innermostReader(dw.below).(interface {
		RepoMemoryEstimate(string) RepoMemoryEstimate
	}); ok {
		return est.RepoMemoryEstimate(repoPrefix)
	}
	return RepoMemoryEstimate{}
}

// AllRepoMemoryEstimates implements Store from the innermost store's counters.
func (dw *DeltaWriter) AllRepoMemoryEstimates() map[string]RepoMemoryEstimate {
	if est, ok := innermostReader(dw.below).(interface {
		AllRepoMemoryEstimates() map[string]RepoMemoryEstimate
	}); ok {
		return est.AllRepoMemoryEstimates()
	}
	return nil
}

// ResolveMutex implements Store.
func (dw *DeltaWriter) ResolveMutex() *sync.Mutex { return &dw.resolveMu }

// RecordedEdges serves the by-file full-row reader through the composed view.
func (dw *DeltaWriter) RecordedEdges() (RecordedEdgeReader, bool) {
	if reader, ok := dw.stackRecordedEdges(); ok {
		return deltaRecordedEdges{inner: reader}, true
	}
	reader, ok := RecordedEdgesOf(dw.view)
	if !ok {
		return nil, false
	}
	return deltaRecordedEdges{inner: reader}, true
}

type deltaRecordedEdges struct{ inner RecordedEdgeReader }

func (r deltaRecordedEdges) RecordedEdgesAt(paths []string) []*Edge {
	return cloneDeltaEdges(r.inner.RecordedEdgesAt(paths))
}

// innermostReader unwraps composed views down to the reader at the bottom of
// the stack, the store every layer composes over.
func innermostReader(r Reader) Reader {
	for {
		switch v := r.(type) {
		case *OverlaidView:
			if v.base == nil {
				return v
			}
			r = v.base
		case interface{ Unwrap() Reader }:
			next := v.Unwrap()
			if next == nil {
				return r
			}
			r = next
		default:
			return r
		}
	}
}

var (
	_ Store                  = (*DeltaWriter)(nil)
	_ MutationReceiptStore   = (*DeltaWriter)(nil)
	_ MutationFanoutRecorder = (*DeltaWriter)(nil)
	_ RecordedEdgeProvider   = (*DeltaWriter)(nil)
)

// deltaPathKey is the path a node identity lives at for coverage, the rule
// the composition applies (IDFile, or the bare id of a file node).
func deltaPathKey(id string) string {
	if file := IDFile(id); file != "" {
		return file
	}
	return id
}
