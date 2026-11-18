package graph

// NewOverlaidViewWithLayers retains the ordinary ordered reader chain while
// composing adjacency from immutable in-memory layers in one batch. It changes
// no revision, capability, node or file surface. Other layer implementations
// retain their existing chain semantics, including delta-specific edge claims.
// The indexes live only as long as this request reader; there is no shared cache.
func NewOverlaidViewWithLayers(base Reader, layers []OverlayLayerReader) *OverlaidView {
	reader := base
	var top *OverlaidView
	immutable := make([]*OverlayLayer, 0, len(layers))
	eligible := len(layers) > 1
	for _, layer := range layers {
		top = NewOverlaidViewWithLayer(reader, layer)
		reader = top
		concrete, ok := layer.(*OverlayLayer)
		if !ok || concrete == nil {
			eligible = false
		}
		immutable = append(immutable, concrete)
	}
	if top == nil {
		return NewOverlaidViewWithLayer(base, nil)
	}
	if eligible {
		top.adjacencyBatch = newOverlayAdjacencyBatch(base, immutable)
	}
	return top
}

type indexedOverlayFile struct {
	index int
	layer *OverlayLayer
}

type overlayAdjacencyBatch struct {
	base       Reader
	layers     []*OverlayLayer
	files      map[string][]indexedOverlayFile
	latestFile map[string]int
	latestOut  map[string]int
	removedAt  map[string]int
}

func newOverlayAdjacencyBatch(base Reader, layers []*OverlayLayer) *overlayAdjacencyBatch {
	batch := &overlayAdjacencyBatch{
		base: base, layers: layers, files: make(map[string][]indexedOverlayFile),
		latestFile: make(map[string]int), latestOut: make(map[string]int), removedAt: make(map[string]int),
	}
	for i, layer := range layers {
		for path := range layer.entries {
			batch.files[path] = append(batch.files[path], indexedOverlayFile{i, layer})
			batch.latestFile[path] = i
		}
		for id := range layer.nodeByID {
			if layer.OwnsOutEdges(id) {
				batch.latestOut[id] = i
			}
		}
		for id := range layer.removedByID {
			if layer.OwnsOutEdges(id) {
				batch.latestOut[id] = i
			}
			if layer.NodeByID(id) == nil {
				batch.removedAt[id] = i
			}
		}
	}
	return batch
}

func overlayIDPath(id string) string {
	if path := IDFile(id); path != "" {
		return path
	}
	return id
}

func indexAfter(indexes map[string]int, key string, origin int) bool {
	i, ok := indexes[key]
	return ok && i > origin
}

// An endpoint removed by an intermediate layer cannot resurrect an older
// edge when a higher layer later reintroduces that node. Preserve this ordered
// chain behavior, rather than checking only the final node's presence.
func (b *overlayAdjacencyBatch) lastHidden(id string) int {
	last := -1
	if i, ok := b.removedAt[id]; ok {
		last = i
	}
	for _, file := range b.files[overlayIDPath(id)] {
		if file.index > last && file.layer.NodeByID(id) == nil {
			last = file.index
		}
	}
	return last
}

func (b *overlayAdjacencyBatch) edges(ids []string, incoming bool) map[string][]*Edge {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string][]*Edge, len(ids))
	seen := make(map[string]bool, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
			out[id] = nil
		}
	}
	if len(unique) == 0 {
		return out
	}
	// Per-call memo only: concurrent reads share immutable indexes, never
	// mutate a request's published owner or retain graph rows beyond the call.
	hidden := make(map[string]int)
	lastHidden := func(id string) int {
		if i, ok := hidden[id]; ok {
			return i
		}
		i := b.lastHidden(id)
		hidden[id] = i
		return i
	}
	keep := func(edge *Edge, origin int) bool {
		if edge == nil {
			return false
		}
		path := edge.FilePath
		if path == "" {
			path = overlayIDPath(edge.From)
		}
		return !indexAfter(b.latestFile, path, origin) && !indexAfter(b.latestOut, edge.From, origin) &&
			lastHidden(edge.From) <= origin && lastHidden(edge.To) <= origin
	}
	if b.base != nil {
		var base map[string][]*Edge
		if incoming {
			base = b.base.GetInEdgesByNodeIDs(unique)
		} else {
			base = b.base.GetOutEdgesByNodeIDs(unique)
		}
		for _, id := range unique {
			// The legacy batched reader preserves a non-nil empty slice
			// after filtering a non-nil lower answer. Point readers still
			// return nil when nothing survives.
			out[id] = base[id][:0:0]
			for _, edge := range base[id] {
				if keep(edge, -1) {
					out[id] = append(out[id], edge)
				}
			}
		}
	}
	for i, layer := range b.layers {
		for _, id := range unique {
			edges := layer.OutEdges(id)
			if incoming {
				edges = layer.InEdges(id)
			}
			if i < len(b.layers)-1 && len(edges) != 0 && out[id] == nil {
				out[id] = []*Edge{}
			}
			for _, edge := range edges {
				if keep(edge, i) {
					out[id] = append(out[id], edge)
				}
			}
		}
	}
	return out
}
