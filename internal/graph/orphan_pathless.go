package graph

import "sort"

// Orphaned pathless stubs.
//
// A whole index mints a pathless stub — a builtin sentinel
// (`repo::builtin::go::len`), a standard-library or dependency symbol — only
// while some file references it. A layer that replaces the files holding the
// last references to such a stub must also withdraw the stub: a file mask
// cannot reach it (its ID has no path to key on), so without an explicit
// removal the copy the layer below carries keeps showing through, and the
// served view holds an identity a whole index of the same tree does not.

// OrphanedPathlessStubs returns, sorted, the pathless nodes of below that a
// layer replacing the files in covered (graph paths; replaced or deleted)
// leaves without any edge, given the layer's own payload edges:
//
//   - the node has no file path and is referenced, in below, by an edge
//     recorded at a covered path;
//   - the layer's payload has no edge into or out of it and no row for it;
//   - every edge below that touches it (in or out) is recorded at a covered
//     path, so the layer's file masks hide all of them.
//
// Only such stubs are returned; a pathless node that still has an edge
// anywhere is left alone, which keeps the rule to exactly the identities a
// whole index would not mint.
func OrphanedPathlessStubs(below Reader, covered map[string]struct{}, payloadNodes []*Node, payloadEdges []*Edge) []string {
	if below == nil || len(covered) == 0 {
		return nil
	}
	paths := make([]string, 0, len(covered))
	for p := range covered {
		if p != "" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var sourceIDs []string
	for _, p := range paths {
		for _, n := range below.GetFileNodes(p) {
			if n != nil && n.ID != "" {
				sourceIDs = append(sourceIDs, n.ID)
			}
		}
	}
	if len(sourceIDs) == 0 {
		return nil
	}
	keep := make(map[string]struct{})
	for _, n := range payloadNodes {
		if n != nil {
			keep[n.ID] = struct{}{}
		}
	}
	for _, e := range payloadEdges {
		if e != nil {
			keep[e.From] = struct{}{}
			keep[e.To] = struct{}{}
		}
	}
	candidateSet := make(map[string]struct{})
	for _, edges := range below.GetOutEdgesByNodeIDs(sourceIDs) {
		for _, e := range edges {
			if e == nil || e.To == "" {
				continue
			}
			if _, atCovered := covered[e.FilePath]; !atCovered {
				continue
			}
			if _, kept := keep[e.To]; kept {
				continue
			}
			candidateSet[e.To] = struct{}{}
		}
	}
	if len(candidateSet) == 0 {
		return nil
	}
	candidates := make([]string, 0, len(candidateSet))
	for id := range candidateSet {
		candidates = append(candidates, id)
	}
	sort.Strings(candidates)
	nodes := below.GetNodesByIDs(candidates)
	var pathless []string
	for _, id := range candidates {
		if n := nodes[id]; n != nil && n.FilePath == "" {
			pathless = append(pathless, id)
		}
	}
	if len(pathless) == 0 {
		return nil
	}
	in := below.GetInEdgesByNodeIDs(pathless)
	out := below.GetOutEdgesByNodeIDs(pathless)
	allCovered := func(edges []*Edge) bool {
		for _, e := range edges {
			if e == nil {
				continue
			}
			if _, atCovered := covered[e.FilePath]; !atCovered {
				return false
			}
		}
		return true
	}
	var orphans []string
	for _, id := range pathless {
		if allCovered(in[id]) && allCovered(out[id]) {
			orphans = append(orphans, id)
		}
	}
	return orphans
}
