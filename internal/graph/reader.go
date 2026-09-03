package graph

import "iter"

// Reader is the read-only contract every graph consumer (query
// engine, MCP tool handlers, analyzers, resolver introspection) depends
// on. *Graph satisfies it directly; OverlaidView (overlay.go) wraps a
// base Reader plus a per-session overlay layer to deliver a non-
// mutating shadow view for editor-buffer queries.
//
// Mutation methods (AddNode, AddEdge, EvictFile, ReindexEdge, …) live
// on *Graph and are NOT part of this interface. Only the indexer and
// the resolver mutate; everyone else reads, and reads must go through
// Reader so the same call site transparently switches between base
// and overlay views.
//
// New read methods on *Graph should be added here too — keeping the
// surfaces in sync is what guarantees that a tool migrated to read
// through the Reader will keep working for both base and overlay
// queries.
type Reader interface {
	// Identity lookups.
	GetNode(id string) *Node
	GetNodeByQualName(qualName string) *Node
	FindNodesByName(name string) []*Node
	// FindNodesByNameContaining returns nodes whose Name (case-
	// insensitive) contains substr. The filter is pushed into the
	// backend so only matching rows cross the boundary on a disk backend;
	// the search hot path's substring fallback uses this instead of
	// the old AllNodes()-then-filter pattern (which materialised the
	// whole node set per call and didn't scale). limit caps the
	// result; 0 means "no limit".
	FindNodesByNameContaining(substr string, limit int) []*Node

	// GetNodesByIDs is the batched sibling of GetNode. The disk-backed
	// store collapses N individual point lookups into a
	// single bulk query — critical on the search hot path where one
	// query materialises 60+ candidate IDs. The in-memory backend
	// forwards to per-id GetNode, so the cost matches an inline loop
	// there. Missing IDs are simply absent from the map (no nil
	// values); duplicates dedupe naturally.
	GetNodesByIDs(ids []string) map[string]*Node

	// File / repo scopes.
	GetFileNodes(filePath string) []*Node
	GetRepoNodes(repoPrefix string) []*Node

	// Edge walks.
	GetOutEdges(nodeID string) []*Edge
	GetInEdges(nodeID string) []*Edge

	// GetInEdgesByNodeIDs / GetOutEdgesByNodeIDs are the batched
	// siblings of GetInEdges / GetOutEdges. The disk-backed store collapses
	// N per-id queries into one bulk query over an `id IN $ids`
	// filter; the in-memory backend forwards to per-id walks (no
	// concurrency win — same algorithmic cost as an inline loop). On
	// the rerank hot path this drops ~150 round-trips per
	// search_symbols call down to ~4 (prepare collects every
	// candidate's ids and fans them out in one inbound + one outbound
	// batch). Missing nodes get nil slices in the returned map so
	// callers can `for _, e := range m[id]` without an ok-check.
	GetInEdgesByNodeIDs(ids []string) map[string][]*Edge
	GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge

	// Bulk reads — used by analyzers (hotspots, cycles, dead code,
	// communities, …) and by the embedded query engine's whole-graph
	// passes.
	AllNodes() []*Node
	AllEdges() []*Edge

	// Kind-predicate scans — the bounded siblings of AllNodes /
	// AllEdges. Analyzers that only care about one edge or node kind
	// use these so disk backends serve an indexed SELECT instead of
	// materialising the whole table, and so an overlay view can answer
	// them without the caller falling back to base.
	//
	// Iterators stop when the consumer's yield returns false;
	// implementations MUST honour early-stop.
	EdgesByKind(kind EdgeKind) iter.Seq[*Edge]
	NodesByKind(kind NodeKind) iter.Seq[*Node]

	// Counters & stats.
	NodeCount() int
	EdgeCount() int
	// EdgeIdentityRevisions is the running count of provenance-bearing
	// edge-identity changes — see Graph.EdgeIdentityRevisions.
	EdgeIdentityRevisions() int
	Stats() GraphStats
	RepoStats() map[string]GraphStats
}

// Compile-time assertion that *Graph satisfies Reader. If a new
// Reader method is added without a corresponding *Graph method (or
// the *Graph signature drifts), the build breaks here rather than at
// a far-away callsite.
var _ Reader = (*Graph)(nil)
