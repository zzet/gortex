package graph

import (
	"iter"
	"sort"
)

// ScopedEdgeRow keeps the already-read source node beside an edge yielded by
// a scoped projection. Resolver views use Source to satisfy the common
// edge-then-GetNode pattern without one backend lookup per edge.
type ScopedEdgeRow struct {
	Edge   *Edge
	Source *Node
	Target *Node
}

// ScopedNodeProjectionSequencer streams nodes owned by a repository or file
// frontier. When filePaths is non-empty it is the tighter predicate; repository
// prefixes remain an additional safety filter. Implementations must keep their
// cursor/page bounded and must not materialise a whole repository.
type ScopedNodeProjectionSequencer interface {
	NodesInScopeSeq(repoPrefixes, filePaths []string, kinds ...NodeKind) iter.Seq[*Node]
	NodesLightInScopeSeq(repoPrefixes, filePaths []string) iter.Seq[*Node]
}

// ScopedProjectionSequencer additionally exposes the selected edge stream.
type ScopedProjectionSequencer interface {
	ScopedNodeProjectionSequencer
	EdgesInScopeSeq(repoPrefixes, filePaths []string, kinds ...EdgeKind) iter.Seq[ScopedEdgeRow]
}

// NodesInScopeSeq selects the production streaming capability. The adapter
// fallback is set-oriented (one file batch or one repository/kind projection),
// never a repository or node-shaped query loop. Production SQLite implements
// the cursor-backed path below; the fallback primarily keeps small test stores
// and the candidate in-memory backend compatible.
func NodesInScopeSeq(s Store, repoPrefixes, filePaths []string, kinds ...NodeKind) iter.Seq[*Node] {
	if s == nil || len(kinds) == 0 || (len(repoPrefixes) == 0 && len(filePaths) == 0) {
		return emptyNodeSeq
	}
	if seq, ok := s.(ScopedProjectionSequencer); ok {
		return seq.NodesInScopeSeq(repoPrefixes, filePaths, kinds...)
	}
	wantedKinds := make(map[NodeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if kind != "" {
			wantedKinds[kind] = struct{}{}
		}
	}
	var nodes map[string]*Node
	if len(filePaths) > 0 {
		byFile := s.GetFileNodesByPaths(filePaths)
		nodes = make(map[string]*Node)
		wantedRepos := stringSet(repoPrefixes)
		for _, fileNodes := range byFile {
			for _, node := range fileNodes {
				if node == nil {
					continue
				}
				if len(wantedRepos) > 0 {
					if _, ok := wantedRepos[node.RepoPrefix]; !ok {
						continue
					}
				}
				if _, ok := wantedKinds[node.Kind]; ok {
					nodes[node.ID] = node
				}
			}
		}
	} else {
		ids := ReadRepoNodeIDsByKinds(s, repoPrefixes, kinds)
		nodes = s.GetNodesByIDs(ids)
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return func(yield func(*Node) bool) {
		for _, id := range ids {
			if node := nodes[id]; node != nil && !yield(node) {
				return
			}
		}
	}
}

// EdgesInScopeSeq is the edge counterpart of NodesInScopeSeq. Adapter stores
// perform one batched adjacency read for a file frontier or one set-oriented
// repo/kind projection; sources are resolved in one ID batch.
func EdgesInScopeSeq(s Store, repoPrefixes, filePaths []string, kinds ...EdgeKind) iter.Seq[ScopedEdgeRow] {
	if s == nil || len(kinds) == 0 || (len(repoPrefixes) == 0 && len(filePaths) == 0) {
		return emptyScopedEdgeSeq
	}
	if seq, ok := s.(ScopedProjectionSequencer); ok {
		return seq.EdgesInScopeSeq(repoPrefixes, filePaths, kinds...)
	}
	var edges []*Edge
	if len(filePaths) > 0 {
		byFile := s.GetFileNodesByPaths(filePaths)
		wantedRepos := stringSet(repoPrefixes)
		ids := make([]string, 0)
		for _, fileNodes := range byFile {
			for _, node := range fileNodes {
				if node == nil {
					continue
				}
				if len(wantedRepos) > 0 {
					if _, ok := wantedRepos[node.RepoPrefix]; !ok {
						continue
					}
				}
				ids = append(ids, node.ID)
			}
		}
		bySource := s.GetOutEdgesByNodeIDs(ids)
		wantedKinds := make(map[EdgeKind]struct{}, len(kinds))
		for _, kind := range kinds {
			wantedKinds[kind] = struct{}{}
		}
		for _, id := range ids {
			for _, edge := range bySource[id] {
				if edge != nil {
					if _, ok := wantedKinds[edge.Kind]; ok {
						edges = append(edges, edge)
					}
				}
			}
		}
	} else {
		for _, row := range ReadRepoEdgesByKinds(s, repoPrefixes, kinds) {
			if row.Edge != nil {
				edges = append(edges, row.Edge)
			}
		}
	}
	endpointIDs := make([]string, 0, len(edges)*2)
	for _, edge := range edges {
		endpointIDs = append(endpointIDs, edge.From)
		if edge.To != "" && !IsUnresolvedTarget(edge.To) {
			endpointIDs = append(endpointIDs, edge.To)
		}
	}
	endpoints := s.GetNodesByIDs(endpointIDs)
	return func(yield func(ScopedEdgeRow) bool) {
		for _, edge := range edges {
			if !yield(ScopedEdgeRow{Edge: edge, Source: endpoints[edge.From], Target: endpoints[edge.To]}) {
				return
			}
		}
	}
}

// NodesLightInScopeSeq supplies the metadata-free candidate census used to
// gate framework passes. Adapter fallbacks may materialise their already-light
// projection; production SQLite is cursor-backed.
func NodesLightInScopeSeq(s Store, repoPrefixes, filePaths []string) iter.Seq[*Node] {
	if s == nil || (len(repoPrefixes) == 0 && len(filePaths) == 0) {
		return emptyNodeSeq
	}
	if seq, ok := s.(ScopedProjectionSequencer); ok {
		return seq.NodesLightInScopeSeq(repoPrefixes, filePaths)
	}
	if len(filePaths) == 0 {
		if light, ok := s.(RepoLightNodeReader); ok {
			nodes := light.RepoNodesLight(repoPrefixes)
			return func(yield func(*Node) bool) {
				for _, node := range nodes {
					if node != nil && !yield(node) {
						return
					}
				}
			}
		}
	}
	if light, ok := s.(NodeLightScanner); ok {
		nodes := light.AllNodesLight()
		wantedRepos := stringSet(repoPrefixes)
		wantedFiles := stringSet(filePaths)
		return func(yield func(*Node) bool) {
			for _, node := range nodes {
				if node == nil {
					continue
				}
				if len(wantedRepos) > 0 {
					if _, ok := wantedRepos[node.RepoPrefix]; !ok {
						continue
					}
				}
				if len(wantedFiles) > 0 {
					if _, ok := wantedFiles[node.FilePath]; !ok {
						continue
					}
				}
				if !yield(node) {
					return
				}
			}
		}
	}
	if len(filePaths) > 0 {
		byFile := s.GetFileNodesByPaths(filePaths)
		wantedRepos := stringSet(repoPrefixes)
		return func(yield func(*Node) bool) {
			for _, filePath := range filePaths {
				for _, node := range byFile[filePath] {
					if node == nil {
						continue
					}
					if len(wantedRepos) > 0 {
						if _, ok := wantedRepos[node.RepoPrefix]; !ok {
							continue
						}
					}
					if !yield(node) {
						return
					}
				}
			}
		}
	}
	nodes := ReadRepoNodesLight(s, repoPrefixes)
	return func(yield func(*Node) bool) {
		for _, node := range nodes {
			if node != nil && !yield(node) {
				return
			}
		}
	}
}

func emptyNodeSeq(func(*Node) bool) {}

func emptyScopedEdgeSeq(func(ScopedEdgeRow) bool) {}
