package graph

import (
	"iter"
	"sort"
	"strings"
	"sync"
)

// deltaLayer is the DeltaWriter's working graph read as an overlay layer, so
// the composition that serves a checkout's view (OverlaidView) also serves the
// delta while it is being written. It is mutable: every answer reads the
// working graph and the claim sets as they are at the moment of the call.
type deltaLayer struct {
	work *Graph

	mu sync.RWMutex
	// covered maps a covered graph path to nothing in particular (the value
	// is unused); presence is the claim. A covered path with no node left is
	// answered as a tombstone.
	covered map[string]bool
	// claimed is the set of sources whose whole outgoing edge set the working
	// graph holds.
	claimed map[string]struct{}
	// removed is the set of identities the delta removed from outside every
	// covered path.
	removed map[string]struct{}
	// importSources is every path a source of an import edge the working
	// graph holds or held lives at (DeltaWriter.ProjectImportAdjacency).
	importSources map[string]struct{}
}

var (
	_ OverlayLayerReader             = (*deltaLayer)(nil)
	_ OverlayLayerRecordedEdgeReader = (*deltaLayer)(nil)
	_ OverlayDetachedNodeReader      = (*deltaLayer)(nil)
)

func (l *deltaLayer) hasFile(p string) bool {
	l.mu.RLock()
	_, ok := l.covered[p]
	l.mu.RUnlock()
	return ok
}

// HasFile implements OverlayLayerReader.
func (l *deltaLayer) HasFile(graphPath string) bool {
	if graphPath == "" {
		return false
	}
	return l.hasFile(graphPath)
}

// IsTombstone implements OverlayLayerReader: a covered path the delta holds no
// node for is answered as empty.
func (l *deltaLayer) IsTombstone(graphPath string) bool {
	return l.HasFile(graphPath) && len(l.work.GetFileNodes(graphPath)) == 0
}

// FilePaths implements OverlayLayerReader.
func (l *deltaLayer) FilePaths() []string {
	l.mu.RLock()
	out := make([]string, 0, len(l.covered))
	for p := range l.covered {
		out = append(out, p)
	}
	l.mu.RUnlock()
	sort.Strings(out)
	return out
}

// CoversNodeID implements OverlayLayerReader.
func (l *deltaLayer) CoversNodeID(id string) bool {
	if id == "" {
		return false
	}
	return l.HasFile(deltaPathKey(id))
}

// OwnsNodeIdentity implements OverlayLayerReader.
func (l *deltaLayer) OwnsNodeIdentity(id string) bool {
	if id == "" {
		return false
	}
	l.mu.RLock()
	_, removed := l.removed[id]
	l.mu.RUnlock()
	return removed || l.work.GetNode(id) != nil
}

// OwnsOutEdges implements OverlayLayerReader: the claimed sources and the
// removed identities. A covered path's own recorded edges are settled per
// edge by HasFile, never here.
func (l *deltaLayer) OwnsOutEdges(id string) bool {
	if id == "" {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.claimed[id]; ok {
		return true
	}
	_, ok := l.removed[id]
	return ok
}

// IsRemovedID implements OverlayLayerReader.
func (l *deltaLayer) IsRemovedID(id string) bool {
	l.mu.RLock()
	_, ok := l.removed[id]
	l.mu.RUnlock()
	return ok
}

// RemovedIDs implements OverlayLayerReader.
func (l *deltaLayer) RemovedIDs() iter.Seq[string] {
	l.mu.RLock()
	ids := make([]string, 0, len(l.removed))
	for id := range l.removed {
		ids = append(ids, id)
	}
	l.mu.RUnlock()
	sort.Strings(ids)
	return func(yield func(string) bool) {
		for _, id := range ids {
			if !yield(id) {
				return
			}
		}
	}
}

// IsNameRemoved implements OverlayLayerReader. Removed identities carry no
// name index; the composition hides them through OwnsNodeIdentity.
func (l *deltaLayer) IsNameRemoved(name, id string) bool { return l.IsRemovedID(id) }

// RemovedIDsForName implements OverlayLayerReader.
func (l *deltaLayer) RemovedIDsForName(string) []string { return nil }

// NodeByID implements OverlayLayerReader.
func (l *deltaLayer) NodeByID(id string) *Node { return l.work.GetNode(id) }

// NodeByQualName implements OverlayLayerReader.
func (l *deltaLayer) NodeByQualName(qualName string) *Node {
	return l.work.GetNodeByQualName(qualName)
}

// GetNodesByQualNames is the batched qualified-name read the composition uses
// when a layer offers it.
func (l *deltaLayer) GetNodesByQualNames(qualNames []string) map[string][]*Node {
	return l.work.GetNodesByQualNames(qualNames)
}

// NodesByName implements OverlayLayerReader.
func (l *deltaLayer) NodesByName(name string) []*Node { return l.work.FindNodesByName(name) }

// NamedNodes implements OverlayLayerReader.
func (l *deltaLayer) NamedNodes() iter.Seq2[string, []*Node] {
	byName := make(map[string][]*Node)
	for _, n := range l.work.AllNodes() {
		if n != nil && n.Name != "" {
			byName[n.Name] = append(byName[n.Name], n)
		}
	}
	return func(yield func(string, []*Node) bool) {
		for name, nodes := range byName {
			if !yield(name, nodes) {
				return
			}
		}
	}
}

// Nodes implements OverlayLayerReader.
func (l *deltaLayer) Nodes() iter.Seq[*Node] {
	nodes := l.work.AllNodes()
	return func(yield func(*Node) bool) {
		for _, n := range nodes {
			if !yield(n) {
				return
			}
		}
	}
}

// NodesByKind is the kind-bounded Nodes the composition prefers.
func (l *deltaLayer) NodesByKind(kind NodeKind) iter.Seq[*Node] {
	return l.work.NodesByKind(kind)
}

// FileNodes implements OverlayLayerReader.
func (l *deltaLayer) FileNodes(graphPath string) []*Node {
	if !l.HasFile(graphPath) {
		return nil
	}
	return l.work.GetFileNodes(graphPath)
}

// OutEdges implements OverlayLayerReader.
func (l *deltaLayer) OutEdges(nodeID string) []*Edge { return l.work.GetOutEdges(nodeID) }

// GetOutEdgesByNodeIDs is the batched OutEdges the composition prefers.
func (l *deltaLayer) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	return l.work.GetOutEdgesByNodeIDs(ids)
}

// InEdges implements OverlayLayerReader.
func (l *deltaLayer) InEdges(nodeID string) []*Edge { return l.work.GetInEdges(nodeID) }

// Edges implements OverlayLayerReader.
func (l *deltaLayer) Edges() iter.Seq[*Edge] {
	edges := l.work.AllEdges()
	return func(yield func(*Edge) bool) {
		for _, e := range edges {
			if !yield(e) {
				return
			}
		}
	}
}

// LayerRecordedEdgesAt implements OverlayLayerRecordedEdgeReader.
func (l *deltaLayer) LayerRecordedEdgesAt(paths []string) []*Edge {
	want := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		want[p] = struct{}{}
	}
	var out []*Edge
	for _, e := range l.work.AllEdges() {
		if e == nil {
			continue
		}
		if _, ok := want[e.FilePath]; ok {
			out = append(out, e)
		}
	}
	return out
}

// detached reports whether a working-graph node lives outside every covered
// path — a node the delta carries as an identity of its own.
func (l *deltaLayer) detached(n *Node) bool {
	return n != nil && (n.FilePath == "" || !l.HasFile(n.FilePath))
}

// DetachedNodeSummaries implements OverlayDetachedNodeReader.
func (l *deltaLayer) DetachedNodeSummaries() iter.Seq[*Node] {
	var out []*Node
	for _, n := range l.work.AllNodes() {
		if l.detached(n) {
			out = append(out, n)
		}
	}
	return func(yield func(*Node) bool) {
		for _, n := range out {
			if !yield(n) {
				return
			}
		}
	}
}

// DetachedFileNodes implements OverlayDetachedNodeReader.
func (l *deltaLayer) DetachedFileNodes(filePath string) []*Node {
	if filePath == "" || l.HasFile(filePath) {
		return nil
	}
	return l.work.GetFileNodes(filePath)
}

// DetachedRepoNodes implements OverlayDetachedNodeReader.
func (l *deltaLayer) DetachedRepoNodes(repoPrefix string) []*Node {
	var out []*Node
	for _, n := range l.work.AllNodes() {
		if !l.detached(n) {
			continue
		}
		if n.RepoPrefix == repoPrefix || strings.HasPrefix(n.FilePath, repoPrefix+"/") {
			out = append(out, n)
		}
	}
	return out
}
