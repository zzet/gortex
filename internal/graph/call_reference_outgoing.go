package graph

import "context"

// CallReferenceOutgoingReader is an optional exact-reader CSR projection.
// It returns complete outgoing Calls/References rows, with all metadata and
// promoted fields, in the same per-anchor order as GetOutEdgesByNodeIDs followed
// by kind filtering. No row limit is applied before the CSR's own caps.
// Errors (including cancellation) return no usable partial rows. Selected
// wrappers must not expose a physical reader's capability through unwrapping.
type CallReferenceOutgoingReader interface {
	GetCallReferenceOutEdgesContext(context.Context, []string) (map[string][]*Edge, error)
}
