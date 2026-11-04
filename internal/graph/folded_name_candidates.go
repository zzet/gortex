package graph

import (
	"container/heap"
	"context"
	"fmt"
	"sort"
	"strings"
)

// FoldedNameCandidateReader selects and hydrates a bounded literal-name head
// from one coherent node view. Accept has NameSearchFilter's existing pure
// compact-key contract; payload predicates run only on the returned copies.
// The bool proves more matching, scope-admitted nodes than the requested head.
type FoldedNameCandidateReader interface {
	FindFoldedNameCandidatesContext(context.Context, string, int, NameSearchFilter) ([]*Node, bool, error)
}

// FoldedNameCandidateSet bounds selection memory without substituting SQL LIKE
// folding for the field grammar's Unicode-lowercase literal containment.
type FoldedNameCandidateSet struct {
	needle string
	limit  int
	keys   foldedNameHeap
	more   bool
}

type foldedNameKey struct {
	NodeSearchKey
	rank int
}

func foldedNameBetter(a, b foldedNameKey) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	if len(a.Name) != len(b.Name) {
		return len(a.Name) < len(b.Name)
	}
	return a.ID < b.ID
}

type foldedNameHeap []foldedNameKey

func (h foldedNameHeap) Len() int           { return len(h) }
func (h foldedNameHeap) Less(i, j int) bool { return foldedNameBetter(h[j], h[i]) }
func (h foldedNameHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *foldedNameHeap) Push(value any)    { *h = append(*h, value.(foldedNameKey)) }
func (h *foldedNameHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func NewFoldedNameCandidateSet(needle string, limit int) *FoldedNameCandidateSet {
	return &FoldedNameCandidateSet{needle: strings.ToLower(needle), limit: limit}
}

func (s *FoldedNameCandidateSet) Add(key NodeSearchKey) {
	if s.limit <= 0 || key.Kind == KindFile || key.Kind == KindImport {
		return
	}
	folded := strings.ToLower(key.Name)
	if !strings.Contains(folded, s.needle) {
		return
	}
	candidate := foldedNameKey{NodeSearchKey: key, rank: 2}
	if folded == s.needle {
		candidate.rank = 0
	} else if strings.HasPrefix(folded, s.needle) {
		candidate.rank = 1
	}
	if len(s.keys) == s.limit {
		s.more = true
		if !foldedNameBetter(candidate, s.keys[0]) {
			return
		}
	}
	candidate.ID, candidate.Name = strings.Clone(candidate.ID), strings.Clone(candidate.Name)
	if len(s.keys) < s.limit {
		heap.Push(&s.keys, candidate)
	} else {
		s.keys[0] = candidate
		heap.Fix(&s.keys, 0)
	}
}

func (s *FoldedNameCandidateSet) Keys() []NodeSearchKey {
	ordered := append(foldedNameHeap(nil), s.keys...)
	sort.Slice(ordered, func(i, j int) bool { return foldedNameBetter(ordered[i], ordered[j]) })
	out := make([]NodeSearchKey, len(ordered))
	for i, key := range ordered {
		out[i] = key.NodeSearchKey
	}
	return out
}

func (s *FoldedNameCandidateSet) More() bool { return s.more }

func compactNameNode(key NodeSearchKey) *Node {
	return &Node{ID: key.ID, Name: key.Name, Kind: key.Kind, FilePath: key.FilePath, RepoPrefix: key.RepoPrefix, WorkspaceID: key.WorkspaceID, ProjectID: key.ProjectID}
}

func (g *Graph) FindFoldedNameCandidatesContext(ctx context.Context, needle string, limit int, filter NameSearchFilter) ([]*Node, bool, error) {
	ctx = nodeSearchContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if limit <= 0 || needle == "" {
		return nil, false, fmt.Errorf("graph: invalid folded-name candidate head")
	}
	// One all-shard view covers keys and retained payloads. A contended
	// in-memory writer yields unknown rather than an uncancellable lock wait.
	locked := 0
	defer func() {
		for i := locked - 1; i >= 0; i-- {
			g.shards[i].mu.RUnlock()
		}
	}()
	for _, shard := range g.shards {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if !shard.mu.TryRLock() {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			return nil, false, fmt.Errorf("graph: folded-name snapshot busy")
		}
		locked++
	}
	set := NewFoldedNameCandidateSet(needle, limit)
	for _, shard := range g.shards {
		for _, node := range shard.nodes {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			if node == nil {
				continue
			}
			key := nodeSearchKey(node)
			if filter.Allows(compactNameNode(key)) {
				set.Add(key)
			}
		}
	}
	out := make([]*Node, 0, limit)
	for _, key := range set.Keys() {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		out = append(out, cloneDeltaNode(g.shardFor(key.ID).nodes[key.ID]))
	}
	return out, set.More(), ctx.Err()
}

var _ FoldedNameCandidateReader = (*Graph)(nil)
