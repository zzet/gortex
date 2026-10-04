package graph

// EdgeEndpointRow is one edge row reduced to what a traversal needs to follow it:
// its two endpoints, its kind and the file the edge is recorded in. It carries
// no line, confidence, origin or Meta, so a reader serving it never transfers
// or decodes the meta blob.
type EdgeEndpointRow struct {
	From     string
	To       string
	Kind     EdgeKind
	FilePath string
}

// EdgeEndpointReader is an optional capability of a single-generation reader:
// endpoint-only projections of the rows GetOutEdgesByNodeIDs,
// GetInEdgesByNodeIDs and GetNodesByIDs return in full.
//
// Every method answers from exactly the rows the matching full-row reader
// would return on the same handle (the same generation, the same structural
// read filter), so a caller may swap one for the other without changing what
// it computes. A composed view (an overlay over several layers) does not
// implement it; callers type-assert and fall back to the full-row readers.
type EdgeEndpointReader interface {
	// EdgeEndpointsRecordedAt returns every edge recorded in one of paths,
	// whichever side of it the file's symbols stand on.
	EdgeEndpointsRecordedAt(paths []string) []EdgeEndpointRow
	// EdgeEndpointsFrom returns the out-edges of ids, restricted to kinds
	// when kinds is non-empty.
	EdgeEndpointsFrom(ids []string, kinds []EdgeKind) []EdgeEndpointRow
	// NodeNamesByIDs returns id -> name for every id the handle carries.
	NodeNamesByIDs(ids []string) map[string]string
	// OutEdgePathsFrom returns, per source id, the distinct sorted files its
	// out-edges are recorded in. Sources with no out-edge are absent.
	OutEdgePathsFrom(ids []string) map[string][]string
}
