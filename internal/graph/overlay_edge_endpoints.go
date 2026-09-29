package graph

import "sort"

// Endpoint-only projections through a composed view.
//
// A single-generation store serves EdgeEndpointReader directly. A composed view
// (an OverlaidView over a base and a layer, possibly nested over further
// views) cannot promise it unconditionally: it serves the projections only
// when every reader below it does. EdgeEndpointsOf is the one entry point a
// caller uses; it answers for a direct reader, for an OverlaidView whose whole
// stack can serve them, and for any wrapper that implements
// EdgeEndpointProvider.
//
// Every answer is the full-row readers' answer through the same view reduced
// to endpoints: a base row survives exactly when baseEdgeVisible would keep
// the full row (the recording file is not claimed, the source's adjacency is
// not replaced, both endpoints are still visible), and the layer's own rows
// are the rows its full-row readers return. So a masked or deleted path, a
// tombstoned identity or a replaced source leaks no base row.

// EdgeEndpointProvider is implemented by readers that can serve the endpoint
// projections only conditionally (a composed view, a wrapper around one).
type EdgeEndpointProvider interface {
	// EdgeEndpoints returns the projections for this reader, or false when
	// some reader beneath it cannot serve them.
	EdgeEndpoints() (EdgeEndpointReader, bool)
}

// OverlayLayerEdgeEndpointReader is the optional layer-side capability a
// composed view uses to project the layer's own rows. A layer without it is
// projected from its full-row readers (OutEdges, Edges).
type OverlayLayerEdgeEndpointReader interface {
	// LayerEdgeEndpointsRecordedAt returns the endpoints of the layer's own
	// edges recorded in one of paths: Edges() filtered by FilePath.
	LayerEdgeEndpointsRecordedAt(paths []string) []EdgeEndpointRow
	// LayerEdgeEndpointsFrom returns the endpoints of OutEdges(id) for each id,
	// restricted to kinds when kinds is non-empty.
	LayerEdgeEndpointsFrom(ids []string, kinds []EdgeKind) []EdgeEndpointRow
}

// EdgeEndpointsOf returns r's endpoint projections, or false when r (or a
// reader it composes) cannot serve them; the caller then uses the full-row
// readers.
func EdgeEndpointsOf(r any) (EdgeEndpointReader, bool) {
	if r == nil {
		return nil, false
	}
	if provider, ok := r.(EdgeEndpointProvider); ok {
		return provider.EdgeEndpoints()
	}
	if direct, ok := r.(EdgeEndpointReader); ok {
		return direct, true
	}
	return nil, false
}

// EdgeEndpoints serves the projections through the view when its base can
// (recursively) serve them. A view with no layer is its base's projection.
func (v *OverlaidView) EdgeEndpoints() (EdgeEndpointReader, bool) {
	if v == nil {
		return nil, false
	}
	var base EdgeEndpointReader
	if v.base != nil {
		projected, ok := EdgeEndpointsOf(v.base)
		if !ok {
			return nil, false
		}
		if v.layer == nil {
			return projected, true
		}
		base = projected
	}
	return overlaidEdgeEndpoints{view: v, base: base}, true
}

// overlaidEdgeEndpoints is one view's projection. base is nil when the view
// has no base reader.
type overlaidEdgeEndpoints struct {
	view *OverlaidView
	base EdgeEndpointReader
}

// rowVisible is baseEdgeVisible over an endpoint row.
func (p overlaidEdgeEndpoints) rowVisible(row EdgeEndpointRow) bool {
	v := p.view
	if v.layer == nil {
		return true
	}
	if v.overlayOwnsBaseEdge(row.From, row.FilePath) {
		return false
	}
	return v.overlayIdentityVisible(row.From) && v.overlayIdentityVisible(row.To)
}

func (p overlaidEdgeEndpoints) visibleBaseRows(rows []EdgeEndpointRow) []EdgeEndpointRow {
	out := make([]EdgeEndpointRow, 0, len(rows))
	for _, row := range rows {
		if p.rowVisible(row) {
			out = append(out, row)
		}
	}
	return out
}

// EdgeEndpointsRecordedAt is AllEdges filtered to FilePath ∈ paths.
func (p overlaidEdgeEndpoints) EdgeEndpointsRecordedAt(paths []string) []EdgeEndpointRow {
	uniq := uniqueNonEmpty(paths)
	if len(uniq) == 0 {
		return nil
	}
	var out []EdgeEndpointRow
	if p.base != nil {
		out = p.visibleBaseRows(p.base.EdgeEndpointsRecordedAt(uniq))
	}
	layer := p.view.layer
	if layer == nil {
		return out
	}
	if projected, ok := layer.(OverlayLayerEdgeEndpointReader); ok {
		return append(out, projected.LayerEdgeEndpointsRecordedAt(uniq)...)
	}
	want := make(map[string]struct{}, len(uniq))
	for _, path := range uniq {
		want[path] = struct{}{}
	}
	for e := range layer.Edges() {
		if e == nil {
			continue
		}
		if _, here := want[e.FilePath]; here {
			out = append(out, endpointRowOf(e))
		}
	}
	return out
}

// EdgeEndpointsFrom is GetOutEdgesByNodeIDs reduced to endpoints and
// restricted to kinds when kinds is non-empty.
func (p overlaidEdgeEndpoints) EdgeEndpointsFrom(ids []string, kinds []EdgeKind) []EdgeEndpointRow {
	uniq := uniqueNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	kindSet, none := endpointKindSet(kinds)
	if none {
		return nil
	}
	var out []EdgeEndpointRow
	if p.base != nil {
		out = p.visibleBaseRows(p.base.EdgeEndpointsFrom(uniq, kinds))
	}
	layer := p.view.layer
	if layer == nil {
		return out
	}
	if projected, ok := layer.(OverlayLayerEdgeEndpointReader); ok {
		return append(out, projected.LayerEdgeEndpointsFrom(uniq, kinds)...)
	}
	for _, id := range uniq {
		for _, e := range layer.OutEdges(id) {
			if e == nil {
				continue
			}
			if kindSet != nil {
				if _, keep := kindSet[e.Kind]; !keep {
					continue
				}
			}
			out = append(out, endpointRowOf(e))
		}
	}
	return out
}

// NodeNamesByIDs is GetNodesByIDs reduced to names: an identity the layer
// speaks for is answered by the layer (absent when it hid the node), every
// other identity by the base.
func (p overlaidEdgeEndpoints) NodeNamesByIDs(ids []string) map[string]string {
	uniq := uniqueNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	v := p.view
	out := make(map[string]string, len(uniq))
	baseIDs := make([]string, 0, len(uniq))
	for _, id := range uniq {
		if v.layer != nil && (v.nodeBelongsToOverlay(id) || v.layer.OwnsNodeIdentity(id)) {
			if node := v.layer.NodeByID(id); node != nil {
				out[id] = node.Name
			}
			continue
		}
		baseIDs = append(baseIDs, id)
	}
	if len(baseIDs) > 0 && p.base != nil {
		for id, name := range p.base.NodeNamesByIDs(baseIDs) {
			out[id] = name
		}
	}
	return out
}

// OutEdgePathsFrom is EdgeEndpointsFrom reduced to each source's sorted
// distinct recording files. The base's own path projection cannot be used:
// visibility depends on each row's target, which it does not carry.
func (p overlaidEdgeEndpoints) OutEdgePathsFrom(ids []string) map[string][]string {
	rows := p.EdgeEndpointsFrom(ids, nil)
	if len(rows) == 0 {
		if len(uniqueNonEmpty(ids)) == 0 {
			return nil
		}
		return map[string][]string{}
	}
	seen := make(map[string]map[string]struct{})
	for _, row := range rows {
		paths := seen[row.From]
		if paths == nil {
			paths = make(map[string]struct{})
			seen[row.From] = paths
		}
		paths[row.FilePath] = struct{}{}
	}
	out := make(map[string][]string, len(seen))
	for from, paths := range seen {
		list := make([]string, 0, len(paths))
		for path := range paths {
			list = append(list, path)
		}
		sort.Strings(list)
		out[from] = list
	}
	return out
}

func endpointRowOf(e *Edge) EdgeEndpointRow {
	return EdgeEndpointRow{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath}
}

// endpointKindSet is the kind filter the store applies: nil for "every kind",
// none=true when kinds names only empty kinds (which matches nothing).
func endpointKindSet(kinds []EdgeKind) (set map[EdgeKind]struct{}, none bool) {
	if len(kinds) == 0 {
		return nil, false
	}
	set = make(map[EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if kind != "" {
			set[kind] = struct{}{}
		}
	}
	return set, len(set) == 0
}

func uniqueNonEmpty(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
