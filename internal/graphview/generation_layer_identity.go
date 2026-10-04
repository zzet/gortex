package graphview

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.OverlayDetachedNodeReader = (*GenerationLayer)(nil)

// DetachedNodeSummaries enumerates only explicit mask-backed carried rows at
// unclaimed paths. Legacy masks may independently own outgoing adjacency.
// Copies protect the immutable membership/count index; metadata stays absent.
func (l *GenerationLayer) DetachedNodeSummaries() iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		for _, summary := range l.detachedNodes {
			owned := summary
			if !yield(&owned) {
				return
			}
		}
	}
}

// DetachedFileNodes hydrates only selected marker IDs, never all upper rows
// at the path. FileNodes retains its independent whole-file contract.
func (l *GenerationLayer) DetachedFileNodes(filePath string) []*graph.Node {
	if _, ok := l.detachedPaths[filePath]; !ok {
		return nil
	}
	l.mu.Lock()
	cached, ok := l.fileNodes[filePath]
	l.mu.Unlock()
	if ok {
		return cached
	}
	nodes := l.detachedNodesMatching(func(n graph.Node) bool { return n.FilePath == filePath })
	l.mu.Lock()
	l.fileNodes[filePath] = nodes
	l.mu.Unlock()
	return nodes
}

// DetachedRepoNodes scans the small captured marker summary set, not the
// upper repository, and performs a batched lookup only for matching IDs.
func (l *GenerationLayer) DetachedRepoNodes(repoPrefix string) []*graph.Node {
	if _, ok := l.detachedRepos[repoPrefix]; !ok {
		return nil
	}
	return l.detachedNodesMatching(func(n graph.Node) bool { return n.RepoPrefix == repoPrefix })
}

func (l *GenerationLayer) detachedNodesMatching(match func(graph.Node) bool) []*graph.Node {
	ids := make([]string, 0)
	for _, n := range l.detachedNodes {
		if match(n) {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows := l.handle.GetNodesByIDs(ids)
	out := make([]*graph.Node, 0, len(ids))
	for _, id := range ids {
		if node := rows[id]; node != nil {
			out = append(out, node)
		}
	}
	return out
}
