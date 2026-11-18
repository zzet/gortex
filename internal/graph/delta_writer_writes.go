package graph

import "context"

// Set-oriented writes of a delta.
//
// The per-save engine uses a handful of set-oriented store writes that have no
// row-by-row fallback (a store without them reports "unsupported", or — for the
// attribute persisters — is assumed to be the in-memory graph, whose reads hand
// back live pointers so an in-place mutation is already durable). A
// DeltaWriter hands back copies, like a disk store, so it must implement every
// one of them: each first makes the working graph hold the rows the write
// addresses (covering, claiming or carrying them exactly as a single-row write
// would) and then applies the in-memory graph's own implementation there.

// nodeHome makes the working graph the authority for the nodes behind ids:
// a node at a path covers the path, a node at no path is carried as a
// detached identity with its adjacency claimed. It returns the nodes the view
// showed. Callers hold writeMu.
func (dw *DeltaWriter) nodeHome(ids []string) map[string]*Node {
	current := dw.view.GetNodesByIDs(ids)
	var paths, detached []string
	var carry []*Node
	for _, id := range ids {
		n := current[id]
		if n == nil {
			continue
		}
		if n.FilePath != "" {
			paths = append(paths, n.FilePath)
			continue
		}
		detached = append(detached, id)
		if dw.work.GetNode(id) == nil {
			carry = append(carry, cloneDeltaNode(n))
		}
	}
	dw.coverPaths(paths)
	dw.claimSources(detached)
	if len(carry) > 0 {
		dw.layer.mu.Lock()
		dw.work.materializeRows(carry, nil)
		dw.layer.mu.Unlock()
		dw.noteMaterialized(len(carry), 0)
	}
	return current
}

// prepareNodeEviction makes an eviction of the nodes behind ids expressible:
// their rows, and the complete edge sets of every source with an edge into
// them, are held by the working graph. Callers hold writeMu.
func (dw *DeltaWriter) prepareNodeEviction(ids []string) map[string]*Node {
	current := dw.nodeHome(ids)
	var sources []string
	for _, in := range dw.view.GetInEdgesByNodeIDs(ids) {
		for _, e := range in {
			if e != nil && !dw.edgeHome(e.From, e.FilePath) {
				sources = append(sources, e.From)
			}
		}
	}
	dw.claimSources(sources)
	return current
}

// markEvicted records, for identities the working graph no longer holds, the
// removal the published generation must express: an identity outside every
// covered path is tombstoned.
func (dw *DeltaWriter) markEvicted(before map[string]*Node) {
	dw.layer.mu.Lock()
	defer dw.layer.mu.Unlock()
	for id, n := range before {
		if n == nil || dw.work.GetNode(id) != nil {
			continue
		}
		if n.FilePath != "" {
			if _, covered := dw.layer.covered[n.FilePath]; covered {
				continue
			}
		}
		dw.layer.removed[id] = struct{}{}
	}
}

// EvictEdgesFromSourcesByKinds implements ScopedEdgeKindEvicter.
func (dw *DeltaWriter) EvictEdgesFromSourcesByKinds(ctx context.Context, sourceIDs []string, kinds []EdgeKind) (int, error) {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("EvictEdgesFromSourcesByKinds")()
	// Every lower row of the kinds out of each source is claimed by kind; the
	// source's other rows stay below (delta_writer_row_claims.go).
	rows, ok := dw.composedEdgesByNodeIDs(sourceIDs, false, true)
	if !ok {
		dw.claimSources(sourceIDs)
	} else {
		dw.claimSources(dw.claimKinds(sourceIDs, kinds, rows))
	}
	return dw.work.EvictEdgesFromSourcesByKinds(ctx, sourceIDs, kinds)
}

// RemoveEdgesExact implements ExactEdgeBatchRemover.
func (dw *DeltaWriter) RemoveEdgesExact(edges []*Edge) int {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("RemoveEdgesExact")()
	keys := make([]edgeKey, 0, len(edges))
	for _, e := range edges {
		if e != nil {
			keys = append(keys, keyOf(e))
		}
	}
	dw.claimSources(dw.claimRows(keys, true))
	return dw.work.RemoveEdgesExact(edges)
}

// EvictContractNodesByIDs implements ContractNodeBatchEvicter.
func (dw *DeltaWriter) EvictContractNodesByIDs(ids []string) (int, int) {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("EvictContractNodesByIDs")()
	before := dw.prepareNodeEviction(ids)
	nodes, edges := dw.work.EvictContractNodesByIDs(ids)
	dw.markEvicted(before)
	return nodes, edges
}

// EvictPathlessNodesByIDs implements PathlessNodeBatchEvicter.
func (dw *DeltaWriter) EvictPathlessNodesByIDs(ids []string) (int, int) {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("EvictPathlessNodesByIDs")()
	before := dw.prepareNodeEviction(ids)
	nodes, edges := dw.work.EvictPathlessNodesByIDs(ids)
	dw.markEvicted(before)
	return nodes, edges
}

// EvictConfigNodesByIDs implements ConfigNodeBatchEvicter.
func (dw *DeltaWriter) EvictConfigNodesByIDs(ids []string) (int, int) {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("EvictConfigNodesByIDs")()
	before := dw.prepareNodeEviction(ids)
	nodes, edges := dw.work.EvictConfigNodesByIDs(ids)
	dw.markEvicted(before)
	return nodes, edges
}

// ReplaceDerivedContracts implements DerivedContractReplacer.
func (dw *DeltaWriter) ReplaceDerivedContracts(replacement DerivedContractReplacement) (DerivedContractReplaceResult, error) {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("ReplaceDerivedContracts")()
	var sources, paths []string
	for _, e := range replacement.RemoveEdges {
		if e != nil && !dw.edgeHome(e.From, e.FilePath) {
			sources = append(sources, e.From)
		}
	}
	for _, e := range replacement.Edges {
		if e != nil && !dw.edgeHome(e.From, e.FilePath) {
			sources = append(sources, e.From)
		}
	}
	for _, n := range replacement.Nodes {
		if n == nil {
			continue
		}
		if n.FilePath != "" {
			paths = append(paths, n.FilePath)
		} else {
			sources = append(sources, n.ID)
		}
	}
	dw.coverPaths(paths)
	dw.claimSources(sources)
	evictable := append(append([]string(nil), replacement.RemoveBridgeNodeIDs...), replacement.TouchedTopicNodeIDs...)
	before := dw.prepareNodeEviction(evictable)
	dw.suppressBelowBuiltins(replacement.Edges)
	result, err := dw.work.ReplaceDerivedContracts(replacement)
	dw.markEvicted(before)
	return result, err
}

// persistStored copies an edge's current content onto the working graph's row
// under the same identity. Callers hold writeMu.
func (dw *DeltaWriter) persistStored(e *Edge) {
	if e == nil {
		return
	}
	dw.claimSources(dw.claimRows([]edgeKey{keyOf(e)}, true))
	stored := dw.work.storedEdge(keyOf(e))
	if stored == nil || stored == e {
		return
	}
	s := dw.work.shardFor(e.From)
	s.mu.Lock()
	content := cloneDeltaEdge(e)
	*stored = *content
	s.mu.Unlock()
	dw.work.edgeMutGen.Add(1)
}

// PersistEdgeAttributes implements EdgePersister.
func (dw *DeltaWriter) PersistEdgeAttributes(e *Edge) {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("PersistEdgeAttributes")()
	dw.persistStored(e)
}

// PersistEdgeAttributesBatch implements EdgeMetaBatchPersister.
func (dw *DeltaWriter) PersistEdgeAttributesBatch(edges []*Edge) {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("PersistEdgeAttributesBatch")()
	for _, e := range edges {
		dw.persistStored(e)
	}
}

// PersistEdgeTerminalStamps implements EdgeTerminalStampPersister. The copy
// the engine hands in was read through the delta and differs from the stored
// row only in what the engine changed, so the whole content is carried.
func (dw *DeltaWriter) PersistEdgeTerminalStamps(edges []*Edge) {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	defer dw.claimingFor("PersistEdgeTerminalStamps")()
	for _, e := range edges {
		dw.persistStored(e)
	}
}

var (
	_ ScopedEdgeKindEvicter      = (*DeltaWriter)(nil)
	_ ExactEdgeBatchRemover      = (*DeltaWriter)(nil)
	_ ContractNodeBatchEvicter   = (*DeltaWriter)(nil)
	_ PathlessNodeBatchEvicter   = (*DeltaWriter)(nil)
	_ ConfigNodeBatchEvicter     = (*DeltaWriter)(nil)
	_ DerivedContractReplacer    = (*DeltaWriter)(nil)
	_ EdgePersister              = (*DeltaWriter)(nil)
	_ EdgeMetaBatchPersister     = (*DeltaWriter)(nil)
	_ EdgeTerminalStampPersister = (*DeltaWriter)(nil)
)

// ReplaceContractOwners implements ContractOwnerReplacer: the generic
// replacement, with its candidate read scoped to the replaced files. The
// generic form reads every contract-owner edge of the repository (a
// repository-wide projection, per edit) and keeps those recorded at the
// replaced files; the delta reads exactly the edges recorded at them.
func (dw *DeltaWriter) ReplaceContractOwners(replacement ContractOwnerReplacement) (ContractOwnerReplaceResult, error) {
	return ReplaceContractOwners(deltaContractOwnerStore{Store: dw, dw: dw, files: replacement.FilePaths}, replacement)
}

// deltaContractOwnerStore is the delta as ReplaceContractOwners reads it: its
// Store surface, the two writes the replacement needs, and a repository edge
// projection that answers from the replaced files' recorded edges.
type deltaContractOwnerStore struct {
	Store
	dw    *DeltaWriter
	files []string
}

func (s deltaContractOwnerStore) RemoveEdgesExact(edges []*Edge) int {
	return s.dw.RemoveEdgesExact(edges)
}

func (s deltaContractOwnerStore) EvictContractNodesByIDs(ids []string) (int, int) {
	return s.dw.EvictContractNodesByIDs(ids)
}

func (s deltaContractOwnerStore) RepoEdgesByKinds(repoPrefixes []string, kinds []EdgeKind) []RepoEdgeRow {
	reader, ok := s.dw.RecordedEdges()
	if !ok {
		return s.dw.RepoEdgesByKinds(repoPrefixes, kinds)
	}
	wantRepo := stringKeySet(repoPrefixes)
	wantKind := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		wantKind[k] = struct{}{}
	}
	var edges []*Edge
	var ids []string
	for _, e := range reader.RecordedEdgesAt(UniqueRecordingPaths(s.files)) {
		if e == nil {
			continue
		}
		if _, ok := wantKind[e.Kind]; ok {
			edges = append(edges, e)
			ids = append(ids, e.From)
		}
	}
	sources := s.dw.GetNodesByIDs(ids)
	var out []RepoEdgeRow
	for _, e := range edges {
		src := sources[e.From]
		if src == nil {
			continue
		}
		if _, ok := wantRepo[src.RepoPrefix]; ok {
			out = append(out, RepoEdgeRow{RepoPrefix: src.RepoPrefix, Edge: e})
		}
	}
	return out
}

var _ ContractOwnerReplacer = (*DeltaWriter)(nil)
