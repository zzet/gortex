package graph

import (
	"sort"
)

// Rows no single file owns.
//
// Two kinds of row are not settled by the file they are recorded at:
//
//   - a shared registry node (a string literal's registry row, a synthetic
//     annotation node): every file that uses it emits a copy under the same
//     identity, and a whole index keeps the copy of the smallest file path
//     (keepSharedNodeCopy). Its identity names no file, so a file mask never
//     speaks for it; a generation serves a copy only through an identity
//     claim, and a copy stays visible below until a claim hides it.
//   - a builtin sentinel: one pathless row per builtin, which a whole index
//     holds exactly while some edge points at it.
//
// The delta keeps both the way a whole index of the tree would have them.

// sharedCopyNode reports whether n is a registry row several files may emit
// under one identity — the rows keepSharedNodeCopy settles by file path.
func sharedCopyNode(n *Node) bool {
	if n == nil || n.FilePath == "" || deltaPathKey(n.ID) == n.FilePath {
		return false
	}
	if n.Kind == KindString {
		return true
	}
	synthetic, _ := n.Meta["synthetic"].(bool)
	return synthetic
}

// keepBelowSharedCopies drops, from rows about to be written, every shared
// registry copy that loses to the copy the view already serves from a path
// the delta does not cover: that copy's file is smaller, so a whole index
// keeps it and the written file does not list the row. A copy whose rival is
// at a covered path reaches the working graph, where the graph's own rule
// picks between them. Callers hold writeMu.
func (dw *DeltaWriter) keepBelowSharedCopies(nodes []*Node) []*Node {
	var ids []string
	for _, n := range nodes {
		if sharedCopyNode(n) {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nodes
	}
	current := dw.view.GetNodesByIDs(ids)
	out := make([]*Node, 0, len(nodes))
	for _, n := range nodes {
		if sharedCopyNode(n) {
			cur := current[n.ID]
			if cur != nil && sharedCopyNode(cur) && cur.Kind == n.Kind &&
				cur.FilePath != n.FilePath && cur.FilePath < n.FilePath && !dw.layer.hasFile(cur.FilePath) {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}

// LostSharedNodes lists the shared registry copies the view below serves from
// a path the delta covers that the working graph no longer holds anywhere: the
// covered file stopped emitting the row (or went away). A whole index keeps
// the copy of the smallest file that still emits it, or no row at all when no
// file does; the caller finds that file (it has to read other files' source)
// and re-derives it through the same writes, or leaves the row to be removed.
func (dw *DeltaWriter) LostSharedNodes() []*Node {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	var out []*Node
	for _, n := range dw.lostForeignNodes() {
		if sharedCopyNode(n) {
			out = append(out, cloneDeltaNode(n))
		}
	}
	return out
}

// lostForeignNodes is every node the view below serves at a covered path
// whose identity names no covered path (so no file mask hides it) and that
// the working graph no longer holds. Callers hold writeMu.
func (dw *DeltaWriter) lostForeignNodes() []*Node {
	dw.layer.mu.RLock()
	covered := make([]string, 0, len(dw.layer.covered))
	for p := range dw.layer.covered {
		covered = append(covered, p)
	}
	dw.layer.mu.RUnlock()
	sort.Strings(covered)
	var out []*Node
	belowNodes := dw.belowNodesAt(covered)
	for _, p := range covered {
		for _, n := range belowNodes[p] {
			if n == nil || n.ID == "" || n.ID == p {
				continue
			}
			if dw.layer.hasFile(deltaPathKey(n.ID)) || dw.work.GetNode(n.ID) != nil {
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

// CoversPath reports whether the delta covers a graph path: the working graph
// holds that file's complete current rows.
func (dw *DeltaWriter) CoversPath(graphPath string) bool {
	return graphPath != "" && dw.layer.hasFile(graphPath)
}

// noteBuiltinTargets records the builtin sentinels materialized edges point
// at. Callers hold writeMu.
func (dw *DeltaWriter) noteBuiltinTargets(edges []*Edge) {
	dw.noteImportSources(edges)
	for _, e := range edges {
		if e == nil || !IsBuiltinStub(e.To) {
			continue
		}
		if dw.builtinTargets == nil {
			dw.builtinTargets = make(map[string]struct{})
		}
		dw.builtinTargets[e.To] = struct{}{}
	}
}

// noteImportSources records the paths of import-edge sources the working
// graph takes on. Callers hold writeMu.
func (dw *DeltaWriter) noteImportSources(edges []*Edge) {
	for _, e := range edges {
		if e == nil || e.Kind != EdgeImports || e.From == "" {
			continue
		}
		dw.layer.mu.Lock()
		if dw.layer.importSources == nil {
			dw.layer.importSources = make(map[string]struct{})
		}
		dw.layer.importSources[deltaPathKey(e.From)] = struct{}{}
		dw.layer.mu.Unlock()
	}
}

// unreferencedBuiltins lists the builtin sentinels the view below holds whose
// every referrer the delta removed: no edge into one survives in the
// composition with the delta applied. A whole index holds a builtin's row only
// while something refers to it. Callers hold writeMu.
func (dw *DeltaWriter) unreferencedBuiltins() []string {
	if len(dw.builtinTargets) == 0 {
		return nil
	}
	var candidates []string
	for id := range dw.builtinTargets {
		if len(dw.work.GetInEdges(id)) == 0 {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Strings(candidates)
	present := dw.below.GetNodesByIDs(candidates)
	var ids []string
	for _, id := range candidates {
		if present[id] != nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	referenced := dw.builtinsReferenced(ids)
	var out []string
	for _, id := range ids {
		if !referenced[id] {
			out = append(out, id)
		}
	}
	return out
}

// builtinsReferenced reports, per builtin, whether some edge into it survives
// in the composition with the delta applied: a layer's own row (the working
// graph's included) that no layer above hides, or a row of the store at the
// bottom that no layer hides. A builtin a layer already answers for is not
// read from the store; the rest are read through the stack in one batch.
// (The stores' bounded identity projections take an explicit kind set of at
// most a handful of kinds, and any kind can point at a builtin.) Callers hold
// writeMu.
func (dw *DeltaWriter) builtinsReferenced(ids []string) map[string]bool {
	referenced := make(map[string]bool, len(ids))
	s := dw.stack()
	for i, l := range s.layers {
		for id, rows := range dw.layerAdjacency(l, ids, true) {
			if referenced[id] {
				continue
			}
			for _, e := range rows {
				if e != nil && s.edgeVisibleFrom(e, i+1) {
					referenced[id] = true
					break
				}
			}
		}
	}
	var rest []string
	for _, id := range ids {
		if !referenced[id] {
			rest = append(rest, id)
		}
	}
	if len(rest) == 0 {
		return referenced
	}
	dw.noteSlowRead("builtin referrers")
	in, ok := dw.composedEdgesByNodeIDs(rest, true, true)
	if !ok {
		in = dw.view.GetInEdgesByNodeIDs(rest)
	}
	for _, id := range rest {
		referenced[id] = len(in[id]) > 0
	}
	return referenced
}

// unreferencedIdentities lists the identities no edge of the composition with
// the delta applied points at or leaves from. Callers hold writeMu.
func (dw *DeltaWriter) unreferencedIdentities(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	in, okIn := dw.composedEdgesByNodeIDs(ids, true, true)
	if !okIn {
		in = dw.view.GetInEdgesByNodeIDs(ids)
	}
	out, okOut := dw.composedEdgesByNodeIDs(ids, false, true)
	if !okOut {
		out = dw.view.GetOutEdgesByNodeIDs(ids)
	}
	var orphaned []string
	for _, id := range ids {
		if len(in[id]) == 0 && len(out[id]) == 0 {
			orphaned = append(orphaned, id)
		}
	}
	sort.Strings(orphaned)
	return orphaned
}
