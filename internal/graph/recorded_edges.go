package graph

// Full edge rows by recording file.
//
// RecordedEdgeReader is the full-row sibling of EdgeEndpointReader's
// EdgeEndpointsRecordedAt: every edge recorded in one of the given files, with
// line, confidence, origin and Meta, so a consumer that hands the rows to
// derived passes sees exactly what the by-node readers would have given it. It
// follows the same pattern: a single-generation store serves it directly, a
// composed view only when every reader below it can (RecordedEdgesOf), and
// the composed answer is AllEdges filtered by FilePath — base rows through
// baseEdgeVisible, plus the layer's own rows recorded there.

// RecordedEdgeReader serves full edge rows by recording file.
type RecordedEdgeReader interface {
	// RecordedEdgesAt returns every edge recorded in one of paths. The empty
	// path is a valid key: it names the edges recorded at no file.
	RecordedEdgesAt(paths []string) []*Edge
}

// UniqueRecordingPaths is paths without duplicates, first-seen order, the
// empty path kept (it is a recording key of its own).
func UniqueRecordingPaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// RecordedEdgeProvider is implemented by readers that can serve
// RecordedEdgeReader only conditionally (a composed view or a wrapper).
type RecordedEdgeProvider interface {
	RecordedEdges() (RecordedEdgeReader, bool)
}

// OverlayLayerRecordedEdgeReader is the optional layer-side capability: the
// layer's own edges (Edges()) recorded in one of paths.
type OverlayLayerRecordedEdgeReader interface {
	LayerRecordedEdgesAt(paths []string) []*Edge
}

// RecordedEdgesOf returns r's by-file full-row reader, or false when r (or a
// reader it composes) cannot serve it.
func RecordedEdgesOf(r any) (RecordedEdgeReader, bool) {
	if r == nil {
		return nil, false
	}
	if provider, ok := r.(RecordedEdgeProvider); ok {
		return provider.RecordedEdges()
	}
	if direct, ok := r.(RecordedEdgeReader); ok {
		return direct, true
	}
	return nil, false
}

// RecordedEdges serves the by-file reader through the view when its base can.
func (v *OverlaidView) RecordedEdges() (RecordedEdgeReader, bool) {
	if v == nil {
		return nil, false
	}
	var base RecordedEdgeReader
	if v.base != nil {
		reader, ok := RecordedEdgesOf(v.base)
		if !ok {
			return nil, false
		}
		if v.layer == nil {
			return reader, true
		}
		base = reader
	}
	return overlaidRecordedEdges{view: v, base: base}, true
}

type overlaidRecordedEdges struct {
	view *OverlaidView
	base RecordedEdgeReader
}

// RecordedEdgesAt is AllEdges filtered to FilePath ∈ paths.
func (p overlaidRecordedEdges) RecordedEdgesAt(paths []string) []*Edge {
	uniq := UniqueRecordingPaths(paths)
	if len(uniq) == 0 {
		return nil
	}
	var out []*Edge
	if p.base != nil {
		for _, e := range p.base.RecordedEdgesAt(uniq) {
			if p.view.baseEdgeVisible(e) {
				out = append(out, e)
			}
		}
	}
	layer := p.view.layer
	if layer == nil {
		return out
	}
	if reader, ok := layer.(OverlayLayerRecordedEdgeReader); ok {
		return append(out, reader.LayerRecordedEdgesAt(uniq)...)
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
			out = append(out, e)
		}
	}
	return out
}
