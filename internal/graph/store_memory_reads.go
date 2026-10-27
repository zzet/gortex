package graph

import (
	"iter"
	"sync"
)

// MemoryStore's reads are explicit delegations to the backing *Graph
// (PR #901 review R1). MemoryStore does not embed *Graph: composition keeps
// every unjournaled write method off its method set, so the only writes the
// live store can expose are the journaled ones in store_memory.go and
// store_memory_capabilities.go.
//
// Drift is a compile error, never a silent unjournaled write: adding a method
// to Store breaks the `var _ Store = (*MemoryStore)(nil)` assertion until it
// is forwarded here (a read) or journaled there (a write). The same holds for
// the optional capability interfaces asserted in
// store_memory_capabilities.go.

func (m *MemoryStore) GetNode(id string) *Node { return m.graph.GetNode(id) }

func (m *MemoryStore) GetNodeByQualName(qualName string) *Node {
	return m.graph.GetNodeByQualName(qualName)
}

func (m *MemoryStore) GetNodesByQualNames(qualNames []string) map[string][]*Node {
	return m.graph.GetNodesByQualNames(qualNames)
}

func (m *MemoryStore) FindNodesByName(name string) []*Node {
	return m.graph.FindNodesByName(name)
}

func (m *MemoryStore) FindNodesByNameInRepo(name, repoPrefix string) []*Node {
	return m.graph.FindNodesByNameInRepo(name, repoPrefix)
}

func (m *MemoryStore) FindNodesByNameContaining(substr string, limit int) []*Node {
	return m.graph.FindNodesByNameContaining(substr, limit)
}

func (m *MemoryStore) GetFileNodes(filePath string) []*Node {
	return m.graph.GetFileNodes(filePath)
}

func (m *MemoryStore) GetFileNodesByPaths(filePaths []string) map[string][]*Node {
	return m.graph.GetFileNodesByPaths(filePaths)
}

func (m *MemoryStore) GetRepoNodes(repoPrefix string) []*Node {
	return m.graph.GetRepoNodes(repoPrefix)
}

func (m *MemoryStore) GetRepoNodesByLanguage(repoPrefix, language string) []*Node {
	return m.graph.GetRepoNodesByLanguage(repoPrefix, language)
}

func (m *MemoryStore) GetNodesByLanguage(language string) []*Node {
	return m.graph.GetNodesByLanguage(language)
}

func (m *MemoryStore) GetRepoNonContentNodes(repoPrefix string) []*Node {
	return m.graph.GetRepoNonContentNodes(repoPrefix)
}

func (m *MemoryStore) GetOutEdges(nodeID string) []*Edge { return m.graph.GetOutEdges(nodeID) }

func (m *MemoryStore) GetInEdges(nodeID string) []*Edge { return m.graph.GetInEdges(nodeID) }

func (m *MemoryStore) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	return m.graph.GetInEdgesByNodeIDs(ids)
}

func (m *MemoryStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	return m.graph.GetOutEdgesByNodeIDs(ids)
}

func (m *MemoryStore) GetEdgeCandidates(endpoints []EdgeEndpoint, sites []EdgeSite) EdgeCandidateSet {
	return m.graph.GetEdgeCandidates(endpoints, sites)
}

func (m *MemoryStore) DistinctExternalTargets(kinds []EdgeKind) []string {
	return m.graph.DistinctExternalTargets(kinds)
}

func (m *MemoryStore) GetRepoEdges(repoPrefix string) []*Edge {
	return m.graph.GetRepoEdges(repoPrefix)
}

func (m *MemoryStore) AllNodes() []*Node { return m.graph.AllNodes() }

func (m *MemoryStore) AllEdges() []*Edge { return m.graph.AllEdges() }

func (m *MemoryStore) EdgesByKind(kind EdgeKind) iter.Seq[*Edge] {
	return m.graph.EdgesByKind(kind)
}

func (m *MemoryStore) NodesByKind(kind NodeKind) iter.Seq[*Node] {
	return m.graph.NodesByKind(kind)
}

func (m *MemoryStore) EdgesWithUnresolvedTarget() iter.Seq[*Edge] {
	return m.graph.EdgesWithUnresolvedTarget()
}

func (m *MemoryStore) GetNodesByIDs(ids []string) map[string]*Node {
	return m.graph.GetNodesByIDs(ids)
}

func (m *MemoryStore) FindNodesByNames(names []string) map[string][]*Node {
	return m.graph.FindNodesByNames(names)
}

func (m *MemoryStore) NodeCount() int { return m.graph.NodeCount() }

func (m *MemoryStore) EdgeCount() int { return m.graph.EdgeCount() }

func (m *MemoryStore) ProxyNodeCountAtLeast(limit int) bool {
	return m.graph.ProxyNodeCountAtLeast(limit)
}

func (m *MemoryStore) Stats() GraphStats { return m.graph.Stats() }

func (m *MemoryStore) RepoStats() map[string]GraphStats { return m.graph.RepoStats() }

func (m *MemoryStore) RepoPrefixes() []string { return m.graph.RepoPrefixes() }

func (m *MemoryStore) EdgeIdentityRevisions() int { return m.graph.EdgeIdentityRevisions() }

func (m *MemoryStore) VerifyEdgeIdentities() error { return m.graph.VerifyEdgeIdentities() }

func (m *MemoryStore) RepoMemoryEstimate(repoPrefix string) RepoMemoryEstimate {
	return m.graph.RepoMemoryEstimate(repoPrefix)
}

func (m *MemoryStore) AllRepoMemoryEstimates() map[string]RepoMemoryEstimate {
	return m.graph.AllRepoMemoryEstimates()
}

func (m *MemoryStore) ResolveMutex() *sync.Mutex { return m.graph.ResolveMutex() }
