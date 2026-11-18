package graph

import "iter"

// AnalysisPageBinder provides bounded read snapshots for whole-store analysis.
// Pages can observe different revisions; callers must validate generation and
// revision before publishing. Ordinary readers keep their snapshot semantics.
type AnalysisPageBinder interface {
	BindAnalysisPages() Store
}

// BindAnalysisPages selects the optional bounded analysis projection.
func BindAnalysisPages(s Store) Store {
	if paged, ok := s.(AnalysisPageBinder); ok {
		return paged.BindAnalysisPages()
	}
	return s
}

// NodeLightSequencer streams the metadata-free node projection used by
// whole-graph analysis. Unlike NodeLightScanner it does not require a backend
// to retain one []*Node containing the complete corpus before the consumer can
// start. Disk backends should honour early stop. Revision-validated analyses can opt
// into bounded snapshots through AnalysisPageBinder.
type NodeLightSequencer interface {
	NodesLightSeq() iter.Seq[*Node]
}

// LightEdgeSequencer is the edge counterpart of NodeLightSequencer. Callers
// must provide at least one kind; making the projection explicitly scoped
// prevents a new analysis from accidentally pulling every edge kind merely to
// discard most of them in Go.
type LightEdgeSequencer interface {
	EdgesLightSeq(kinds ...EdgeKind) iter.Seq[*Edge]
}

// NodesByKindsSequencer streams full nodes for a fixed kind set. Process
// discovery uses it for function/method entry-point metadata, avoiding the old
// second AllNodes scan while preserving visibility and entry-point scoring.
type NodesByKindsSequencer interface {
	NodesByKindsSeq(kinds ...NodeKind) iter.Seq[*Node]
}

// NodeIDName is the minimal symbol-name projection used by lexical linkers.
// Repo scoping is applied by the producer, so only the two columns consumed by
// the name index cross a disk-store boundary.
type NodeIDName struct {
	ID   string
	Name string
}

// NodeIDNamesByKindsSequencer streams ID/name pairs for an exact kind set.
// Empty repoPrefix is the global cross-repository view; non-empty prefixes are
// exact. Production Graph and SQLite stores implement this capability.
type NodeIDNamesByKindsSequencer interface {
	NodeIDNamesByKindsSeq(repoPrefix string, kinds ...NodeKind) iter.Seq[NodeIDName]
}

// NodesLightSeq selects the streaming capability when available. The legacy
// fallback preserves Reader compatibility for overlay/read-only views; both
// production graph stores implement NodeLightSequencer.
func NodesLightSeq(r Reader) iter.Seq[*Node] {
	if r == nil {
		return func(func(*Node) bool) {}
	}
	if seq, ok := r.(NodeLightSequencer); ok {
		return seq.NodesLightSeq()
	}
	if scan, ok := r.(NodeLightScanner); ok {
		return func(yield func(*Node) bool) {
			for _, node := range scan.AllNodesLight() {
				if node != nil && !yield(node) {
					return
				}
			}
		}
	}
	return func(yield func(*Node) bool) {
		for _, node := range r.AllNodes() {
			if node != nil && !yield(node) {
				return
			}
		}
	}
}

// EdgesLightSeq streams a metadata-free, kind-scoped projection. The Store
// fallback uses the required EdgesByKind iterator and therefore never needs an
// AllEdges snapshot or a per-node adjacency loop.
func EdgesLightSeq(s Store, kinds ...EdgeKind) iter.Seq[*Edge] {
	if s == nil || len(kinds) == 0 {
		return func(func(*Edge) bool) {}
	}
	if seq, ok := s.(LightEdgeSequencer); ok {
		return seq.EdgesLightSeq(kinds...)
	}
	want := dedupeAnalysisEdgeKinds(kinds)
	return func(yield func(*Edge) bool) {
		for _, kind := range want {
			for edge := range s.EdgesByKind(kind) {
				if edge != nil && !yield(edge) {
					return
				}
			}
		}
	}
}

// NodesByKindsSeq streams a fixed kind set without issuing one point lookup per
// node. The fallback is one indexed iterator per distinct kind, a constant
// number of queries chosen by the caller rather than an N+1 walk.
func NodesByKindsSeq(s Store, kinds ...NodeKind) iter.Seq[*Node] {
	if s == nil || len(kinds) == 0 {
		return func(func(*Node) bool) {}
	}
	if seq, ok := s.(NodesByKindsSequencer); ok {
		return seq.NodesByKindsSeq(kinds...)
	}
	want := dedupeAnalysisNodeKinds(kinds)
	return func(yield func(*Node) bool) {
		for _, kind := range want {
			for node := range s.NodesByKind(kind) {
				if node != nil && !yield(node) {
					return
				}
			}
		}
	}
}

// NodeIDNamesByKindsSeq selects the lightweight capability when available.
// The compatibility fallback still uses a constant number of exact-kind
// iterators and never calls AllNodes or performs per-node point lookups.
func NodeIDNamesByKindsSeq(s Store, repoPrefix string, kinds ...NodeKind) iter.Seq[NodeIDName] {
	if s == nil || len(kinds) == 0 {
		return func(func(NodeIDName) bool) {}
	}
	if seq, ok := s.(NodeIDNamesByKindsSequencer); ok {
		return seq.NodeIDNamesByKindsSeq(repoPrefix, kinds...)
	}
	return func(yield func(NodeIDName) bool) {
		for node := range NodesByKindsSeq(s, kinds...) {
			if node == nil || (repoPrefix != "" && node.RepoPrefix != repoPrefix) {
				continue
			}
			if !yield(NodeIDName{ID: node.ID, Name: node.Name}) {
				return
			}
		}
	}
}

func dedupeAnalysisEdgeKinds(kinds []EdgeKind) []EdgeKind {
	seen := make(map[EdgeKind]struct{}, len(kinds))
	out := make([]EdgeKind, 0, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		out = append(out, kind)
	}
	return out
}

func dedupeAnalysisNodeKinds(kinds []NodeKind) []NodeKind {
	seen := make(map[NodeKind]struct{}, len(kinds))
	out := make([]NodeKind, 0, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		out = append(out, kind)
	}
	return out
}

// NodesLightSeq is the in-memory reference implementation. It copies at most
// one shard's pointers at a time so callbacks run without a shard lock and a
// large graph never gains a second whole-corpus node slice.
func (g *Graph) NodesLightSeq() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		for _, shard := range g.shards {
			shard.mu.RLock()
			batch := make([]*Node, 0, len(shard.nodes))
			for _, node := range shard.nodes {
				if node != nil {
					batch = append(batch, node)
				}
			}
			shard.mu.RUnlock()
			for _, node := range batch {
				if !yield(node) {
					return
				}
			}
		}
	}
}

// EdgesLightSeq delegates to the already shard-bounded multi-kind iterator.
// In-memory edges are already resident, so returning their immutable pointers
// is the light projection.
func (g *Graph) EdgesLightSeq(kinds ...EdgeKind) iter.Seq[*Edge] {
	return g.EdgesByKinds(dedupeAnalysisEdgeKinds(kinds))
}

// NodesByKindsSeq is the in-memory full-node projection used by process
// discovery. The number of kind iterators is fixed by the caller.
func (g *Graph) NodesByKindsSeq(kinds ...NodeKind) iter.Seq[*Node] {
	want := dedupeAnalysisNodeKinds(kinds)
	return func(yield func(*Node) bool) {
		for _, kind := range want {
			for node := range g.NodesByKind(kind) {
				if node != nil && !yield(node) {
					return
				}
			}
		}
	}
}

// NodeIDNamesByKindsSeq projects the already-resident in-memory nodes without
// retaining another node slice. The SQLite sibling selects only id/name.
func (g *Graph) NodeIDNamesByKindsSeq(repoPrefix string, kinds ...NodeKind) iter.Seq[NodeIDName] {
	return func(yield func(NodeIDName) bool) {
		for node := range g.NodesByKindsSeq(kinds...) {
			if node == nil || (repoPrefix != "" && node.RepoPrefix != repoPrefix) {
				continue
			}
			if !yield(NodeIDName{ID: node.ID, Name: node.Name}) {
				return
			}
		}
	}
}

var (
	_ NodeLightSequencer          = (*Graph)(nil)
	_ LightEdgeSequencer          = (*Graph)(nil)
	_ NodesByKindsSequencer       = (*Graph)(nil)
	_ NodeIDNamesByKindsSequencer = (*Graph)(nil)
)
