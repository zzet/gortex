package graph

import (
	"iter"
	"sort"
	"strings"
	"time"
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
		if ok && repoPrefixes == nil && dw.baseCache != nil {
			// The whole directory index (a pass whose pending set holds a
			// source with no repository) is every repository's, answered
			// through the per-repository-set path and its stack entry: file
			// nodes always carry their repository.
			if repos := dw.RepoPrefixes(); len(repos) > 0 {
				for row := range dw.FileNodeIdentitiesSeq(repos) {
					if !yield(row) {
						return
					}
				}
				return
			}
		}
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
		if dw.baseCache != nil {
			rows := dw.baseCache.fileIdentities(repoPrefixes, func() []FileNodeIdentity {
				var loaded []FileNodeIdentity
				for row := range seq.FileNodeIdentitiesSeq(repoPrefixes) {
					loaded = append(loaded, row)
				}
				return loaded
			})
			baseRows = func(yield func(FileNodeIdentity) bool) {
				for _, row := range rows {
					if !yield(row) {
						return
					}
				}
			}
		}
		// Every layer below the delta's own is immutable: with a projection
		// cache the composition of the part it is kept for over the bottom
		// store is kept per stack, and the layers above that part (the dirty
		// chain's and the delta's own) are applied per read.
		layers := s.layers
		if k, split := dw.cacheSplit(s); split {
			below := deltaStack{base: s.base, layers: layers[:k]}
			rows := dw.baseCache.stackFileIdentities(repoPrefixes, func() []FileNodeIdentity {
				var composed []FileNodeIdentity
				for row := range baseRows {
					if !below.hiddenAbove(row.ID, 0) {
						composed = append(composed, row)
					}
				}
				for i, l := range below.layers {
					for _, n := range dw.layerNodesInScope(l, repoPrefixes, nil, false, KindFile) {
						if n != nil && n.Kind == KindFile && inScope(n, repos, nil) && !below.hiddenAbove(n.ID, i+1) {
							composed = append(composed, fileNodeIdentity(n))
						}
					}
				}
				return composed
			})
			for _, row := range rows {
				if !s.hiddenAbove(row.ID, k) {
					out = append(out, row)
				}
			}
			for i := k; i < len(layers); i++ {
				for _, n := range dw.layerNodesInScope(layers[i], repoPrefixes, nil, false, KindFile) {
					if n != nil && n.Kind == KindFile && inScope(n, repos, nil) && !s.hiddenAbove(n.ID, i+1) {
						out = append(out, fileNodeIdentity(n))
					}
				}
			}
		} else {
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
	defer dw.noteFileNodeRead(time.Now())
	if len(files) == 0 || len(kinds) == 0 {
		return nil
	}
	want := make(map[NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		want[k] = struct{}{}
	}
	// With a projection cache, a file no layer above the cached part covers
	// (and the delta's own layer does not otherwise speak for) is answered
	// from the stack's kept rows of the kinds, and the layers above the cached
	// part — the dirty chain's and the delta's own — are applied per read:
	// their hiding and the nodes they carry at the file. A package's type
	// index reads every file of the package, and all but the changed ones
	// are such files.
	byFile := make(map[string][]*Node, len(files))
	read := files
	s := dw.stack()
	k, split := dw.cacheSplit(s)
	var view Reader
	var base batchedFileNodesReader
	if split {
		view = dw.viewBelowLevel(k)
		base, split = s.base.(batchedFileNodesReader)
		split = split && view != nil
	}
	if split {
		kindKey := nodeKindsKey(kinds)
		touched := dw.deltaLayerTouchedPaths(files)
		below := deltaStack{base: s.base, layers: s.layers[:k]}
		read = nil
		var stable []string
		for _, file := range files {
			if _, t := touched[file]; t || s.coveredFrom(file, k) {
				read = append(read, file)
			} else {
				stable = append(stable, file)
			}
		}
		kept, uncached := dw.baseCache.stackFileNodes(stable, kindKey, func(missing []string) map[string][]*Node {
			loaded := dw.baseCache.stackFileNodesByPath(missing, func(missing []string) map[string][]*Node {
				return dw.composeFileNodesOver(below, base, view, missing)
			})
			out := make(map[string][]*Node, len(missing))
			for _, f := range missing {
				rows := []*Node{}
				for _, n := range loaded[f] {
					if n == nil {
						continue
					}
					if _, ok := want[n.Kind]; ok {
						rows = append(rows, n)
					}
				}
				out[f] = rows
			}
			return out
		})
		read = append(read, uncached...)
		for file, nodes := range kept {
			for _, n := range nodes {
				if !s.hiddenAbove(n.ID, k) {
					byFile[file] = append(byFile[file], cloneDeltaNode(n))
				}
			}
		}
		// A node a layer above holds at a file it does not cover is served
		// too.
		for _, file := range stable {
			for i := k; i < len(s.layers); i++ {
				for _, n := range dw.layerDetachedFileNodes(s.layers[i], file) {
					if n == nil || s.hiddenAbove(n.ID, i+1) {
						continue
					}
					if _, ok := want[n.Kind]; !ok {
						continue
					}
					byFile[file] = append(byFile[file], cloneDeltaNode(n))
				}
			}
		}
	}
	if len(read) > 0 {
		for file, nodes := range dw.getFileNodesByPaths(read) {
			byFile[file] = nodes
		}
	}
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

func nodeKindsKey(kinds []NodeKind) string {
	keys := make([]string, 0, len(kinds))
	for _, k := range kinds {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
