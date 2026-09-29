package graph

import (
	"iter"
	"sort"
)

var _ FileNodeIdentitySequencer = (*DeltaWriter)(nil)

// FileNodeIdentitiesSeq implements FileNodeIdentitySequencer through the
// stack: the base's metadata-free file projection (the resolver's directory
// index lists every file of a repository, and reading full file rows with
// their Meta for it cost 0.4-0.7 s per delta on a real repository), plus the
// file nodes the layers above serve, with the rows a layer above hides left
// out. Nil prefixes mean global; non-nil empty means no rows. The order is
// by ID, as the store's projection yields it.
func (dw *DeltaWriter) FileNodeIdentitiesSeq(repoPrefixes []string) iter.Seq[FileNodeIdentity] {
	return func(yield func(FileNodeIdentity) bool) {
		if repoPrefixes != nil && len(repoPrefixes) == 0 {
			return
		}
		s := dw.stack()
		seq, ok := s.base.(FileNodeIdentitySequencer)
		if !ok || repoPrefixes == nil {
			// The composed view answers the same question row by row.
			var nodes iter.Seq[*Node]
			if repoPrefixes == nil {
				nodes = dw.NodesByKind(KindFile)
			} else {
				nodes = dw.NodesInScopeSeq(repoPrefixes, nil, KindFile)
			}
			for n := range nodes {
				if n != nil && !yield(fileNodeIdentity(n)) {
					return
				}
			}
			return
		}
		repos := stringKeySet(repoPrefixes)
		out := make([]FileNodeIdentity, 0, 1024)
		baseRows := seq.FileNodeIdentitiesSeq(repoPrefixes)

		for row := range baseRows {
			if !s.hiddenAbove(row.ID, 0) {
				out = append(out, row)
			}
		}
		for i, l := range s.layers {
			for _, n := range dw.layerNodesInScope(l, repoPrefixes, nil, false, KindFile) {
				if n != nil && n.Kind == KindFile && inScope(n, repos, nil) && !s.hiddenAbove(n.ID, i+1) {
					out = append(out, fileNodeIdentity(n))
				}
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		seen := make(map[string]struct{}, len(out))
		for _, row := range out {
			if _, dup := seen[row.ID]; dup {
				continue
			}
			seen[row.ID] = struct{}{}
			if !yield(row) {
				return
			}
		}
	}
}

var _ NodesInFilesByKindFinder = (*DeltaWriter)(nil)

// NodesInFilesByKind implements NodesInFilesByKindFinder through the batched
// per-path read (GetFileNodesByPaths), filtered by kind. Without it the
// resolver's receiver rebind builds a package's type index by scanning every
// Go type and interface of the composed graph, once per delta. It must not go
// through the file-scoped projection with no repository scope: on a
// generation handle that read is not keyed by path and cost about a second
// per call.
func (dw *DeltaWriter) NodesInFilesByKind(files []string, kinds []NodeKind) []*Node {
	if len(files) == 0 || len(kinds) == 0 {
		return nil
	}
	want := make(map[NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		want[k] = struct{}{}
	}
	byFile := dw.GetFileNodesByPaths(files)
	var out []*Node
	seen := make(map[string]struct{})
	for _, file := range files {
		for _, n := range byFile[file] {
			if n == nil {
				continue
			}
			if _, ok := want[n.Kind]; !ok {
				continue
			}
			if _, dup := seen[n.ID]; dup {
				continue
			}
			seen[n.ID] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}
