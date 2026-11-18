package graph

// Restating an inert save's rows.
//
// A per-file delta re-derives every changed file from source, because the
// generation claims each changed path. A save whose content fingerprints call
// it inert is one the primary per-save path does not re-derive at all: it
// keeps the prior rows. The per-file engine's fresh derivation of such a file
// can differ from those rows (a pass the whole index ran for them is not run
// for one file), so the delta restates the rows the view below holds at the
// path in place of what it derived: nodes by identity, and every edge recorded
// at the path. What the delta publishes for the path is then what the primary
// path holds after the same save.

// RestateBelowRows makes the delta's rows at each path the view below's: the
// delta's edges recorded at the path are replaced by the view below's, and
// the view below's nodes at the path are written over the delta's. A path
// whose derived node identities differ from the view below's is not inert
// after all and is left as derived; it is returned in skipped.
func (dw *DeltaWriter) RestateBelowRows(paths []string) (nodes, edges int, skipped []string) {
	var restate []string
	for _, p := range UniqueRecordingPaths(paths) {
		if p == "" {
			continue
		}
		below := make(map[string]struct{})
		for _, n := range dw.below.GetFileNodes(p) {
			if n != nil {
				below[n.ID] = struct{}{}
			}
		}
		same := true
		derived := dw.work.GetFileNodes(p)
		if len(derived) != len(below) {
			same = false
		}
		for _, n := range derived {
			if _, ok := below[n.ID]; !ok {
				same = false
				break
			}
		}
		if same {
			restate = append(restate, p)
		} else {
			skipped = append(skipped, p)
		}
	}
	paths = restate
	if len(paths) == 0 {
		return 0, 0, skipped
	}
	want := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		want[p] = struct{}{}
	}
	var belowNodes []*Node
	for _, p := range paths {
		for _, n := range dw.below.GetFileNodes(p) {
			if n != nil {
				belowNodes = append(belowNodes, cloneDeltaNode(n))
			}
		}
	}
	var belowEdges []*Edge
	if recorded, ok := RecordedEdgesOf(dw.below); ok {
		belowEdges = cloneDeltaEdges(recorded.RecordedEdgesAt(paths))
	}
	// A restated edge whose endpoint the same delta removed (a deleted
	// file's symbol, an evicted identity) is not restated: the primary path
	// keeps the file's rows, and its eviction of the other file takes the
	// edges into it.
	belowEdges = dw.edgesWithLiveEndpoints(belowEdges)
	var stale []*Edge
	for _, e := range dw.work.AllEdges() {
		if e == nil {
			continue
		}
		if _, here := want[e.FilePath]; here {
			stale = append(stale, e)
		}
	}
	if len(stale) > 0 {
		dw.RemoveEdgesExact(stale)
	}
	dw.AddBatch(belowNodes, belowEdges)
	return len(belowNodes), len(belowEdges), skipped
}

// edgesWithLiveEndpoints keeps the edges whose endpoints are not identities
// the view below holds and the delta's view no longer does.
func (dw *DeltaWriter) edgesWithLiveEndpoints(edges []*Edge) []*Edge {
	if len(edges) == 0 {
		return edges
	}
	var ids []string
	for _, e := range edges {
		ids = append(ids, e.From, e.To)
	}
	ids = uniqueIDs(ids)
	before := dw.below.GetNodesByIDs(ids)
	var gone []string
	for id, n := range before {
		if n != nil {
			gone = append(gone, id)
		}
	}
	now := dw.view.GetNodesByIDs(gone)
	removed := make(map[string]struct{})
	for _, id := range gone {
		if now[id] == nil {
			removed[id] = struct{}{}
		}
	}
	if len(removed) == 0 {
		return edges
	}
	out := edges[:0]
	for _, e := range edges {
		_, fromGone := removed[e.From]
		_, toGone := removed[e.To]
		if !fromGone && !toGone {
			out = append(out, e)
		}
	}
	return out
}
