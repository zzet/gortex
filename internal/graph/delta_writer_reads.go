package graph

// Bounded reads of a delta.
//
// These are read capabilities whose absence would make the per-save engine
// fall back to full-row reads through the composed view: each answers the
// same question from narrower projections of the same composition.

// EdgeEndpoints serves the endpoint-only projections through the composed
// view when every reader below can serve them (EdgeEndpointProvider).
func (dw *DeltaWriter) EdgeEndpoints() (EdgeEndpointReader, bool) {
	return EdgeEndpointsOf(dw.view)
}

// ProjectImportAdjacency implements ImportAdjacencyProjector: the direct
// import targets of each caller file. It answers exactly what the resolver's
// ordinary fallback (legacyImportTargetsByFile) computes from full rows.
//
// A path no layer of the stack speaks for — none covers it, and none carries
// or removes a source recorded in it — is answered by the store at the bottom
// in one indexed projection per chunk, each target kept only when no layer
// hides it; every other path is read as endpoint rows of the file's nodes
// through the composition.
//
// With a projection cache (one per immutable stack below the delta, or below
// the dirty chain the delta stands on), a path no layer above that part
// speaks for is answered from the stack's cached answer: the part is
// immutable, so what it composes for the path is the same on every delta over
// it. Only the identity filter of the layers above it is applied per read.
func (dw *DeltaWriter) ProjectImportAdjacency(filePaths []string) (map[string][]string, bool) {
	paths := UniqueRecordingPaths(filePaths)
	if len(paths) == 0 {
		return nil, true
	}
	for _, p := range paths {
		if p == "" {
			return nil, false
		}
	}
	s := dw.stack()
	k, split := dw.cacheSplit(s)
	var view Reader
	if split {
		view = dw.viewBelowLevel(k)
	}
	if view == nil {
		out, _, ok := dw.projectImportAdjacencyOver(s, paths)
		return out, ok
	}
	below := deltaStack{base: s.base, layers: s.layers[:k]}
	upper := deltaStack{layers: s.layers[k:]}
	touched := dw.deltaLayerTouchedPaths(paths)
	for p := range dw.stackTouchedPaths(deltaStack{layers: s.layers[k : len(s.layers)-1]}, paths) {
		touched[p] = struct{}{}
	}
	var own, stable []string
	for _, p := range paths {
		if _, t := touched[p]; t || s.coveredFrom(p, k) {
			own = append(own, p)
		} else {
			stable = append(stable, p)
		}
	}
	out := make(map[string][]string, len(paths))
	if len(stable) > 0 {
		entries, complete := dw.baseCache.stackImportAdjacency(stable, func(missing []string) (map[string]stackImportEntry, bool) {
			rows, filtered, ok := dw.projectImportAdjacencyWith(below, view, false, missing)
			if !ok {
				return nil, false
			}
			loaded := make(map[string]stackImportEntry, len(missing))
			for _, p := range missing {
				loaded[p] = stackImportEntry{targets: rows[p], filtered: filtered[p]}
			}
			return loaded, true
		})
		if !complete {
			out, _, ok := dw.projectImportAdjacencyOver(s, paths)
			return out, ok
		}
		// A stable path's import rows are recorded at it and leave its file
		// node, which no layer above the kept part speaks for: of those
		// layers' rules only the target's identity check is left to apply.
		for _, p := range stable {
			for _, to := range entries[p].targets {
				if !upper.identityServed(to) {
					continue
				}
				out[p] = append(out[p], to)
			}
		}
	}
	if len(own) > 0 {
		// A path the delta's own layer covers is answered from the delta's
		// rows alone: covering masks every row recorded there below, and an
		// import edge is recorded at its source's file (the extractor writes
		// it with the file it parses), so no import row from the path's
		// nodes is left below. Reading the stack for it would query every
		// layer and the store with all of the file's node ids, per edit.
		covered, rest := dw.splitCoveredPaths(own)
		for p, targets := range dw.coveredImportAdjacency(covered) {
			out[p] = targets
		}
		if len(rest) > 0 {
			rows, _, ok := dw.projectImportAdjacencyOver(s, rest)
			if !ok {
				return nil, false
			}
			for p, targets := range rows {
				out[p] = targets
			}
		}
	}
	return out, true
}

// splitCoveredPaths splits paths into those the delta's own layer covers and
// the rest.
func (dw *DeltaWriter) splitCoveredPaths(paths []string) (covered, rest []string) {
	for _, p := range paths {
		if dw.layer.HasFile(p) {
			covered = append(covered, p)
		} else {
			rest = append(rest, p)
		}
	}
	return covered, rest
}

// coveredImportAdjacency is the import adjacency of paths the delta's own
// layer covers, from the delta's working rows: the import edges out of the
// path's nodes there, in the working graph's edge order.
func (dw *DeltaWriter) coveredImportAdjacency(paths []string) map[string][]string {
	if len(paths) == 0 {
		return nil
	}
	fileOf := make(map[string]string)
	for _, p := range paths {
		for _, n := range dw.work.GetFileNodes(p) {
			if n != nil && n.ID != "" {
				fileOf[n.ID] = p
			}
		}
	}
	out := make(map[string][]string, len(paths))
	if len(fileOf) == 0 {
		return out
	}
	for _, e := range dw.work.AllEdges() {
		if e == nil || e.Kind != EdgeImports {
			continue
		}
		if file, ok := fileOf[e.From]; ok {
			out[file] = append(out[file], e.To)
		}
	}
	return out
}

// deltaLayerTouchedPaths is the subset of paths the delta's own layer speaks
// for: it covers the path, carries or held an import source there, claims or
// removed the file node or one of its edges, or removed an identity recorded
// at it (stackTouchedPaths' test for the delta's layer, plus coverage).
func (dw *DeltaWriter) deltaLayerTouchedPaths(paths []string) map[string]struct{} {
	l := dw.layer
	touched := make(map[string]struct{})
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, p := range paths {
		_, covered := l.covered[p]
		_, imports := l.importSources[p]
		_, claimed := l.claimed[p]
		_, removed := l.removed[p]
		if covered || imports || claimed || removed {
			touched[p] = struct{}{}
		}
	}
	for id := range l.removed {
		touched[deltaPathKey(id)] = struct{}{}
	}
	return touched
}

// projectImportAdjacencyOver is ProjectImportAdjacency over stack s. filtered
// reports, per path, whether its targets were filtered by s's identity check
// (a path answered by the bottom store) or are the composition's raw rows.
func (dw *DeltaWriter) projectImportAdjacencyOver(s deltaStack, paths []string) (map[string][]string, map[string]bool, bool) {
	return dw.projectImportAdjacencyWith(s, dw.view, true, paths)
}

// projectImportAdjacencyWith is projectImportAdjacencyOver with the composed
// view of stack s given: the delta's view (top) or a view below it, whose
// covered paths are then read through that view alone.
func (dw *DeltaWriter) projectImportAdjacencyWith(s deltaStack, view Reader, top bool, paths []string) (map[string][]string, map[string]bool, bool) {
	out := make(map[string][]string, len(paths))
	filtered := make(map[string]bool, len(paths))
	rest := paths
	if base, ok := s.base.(ImportAdjacencyProjector); ok {
		touched := dw.stackTouchedPaths(s, paths)
		var clean []string
		rest = nil
		for _, p := range paths {
			if _, t := touched[p]; t || s.coveredByAny(p) {
				rest = append(rest, p)
			} else {
				clean = append(clean, p)
			}
		}
		if len(clean) > 0 {
			var projected map[string][]string
			var complete bool
			if dw.baseCache != nil {
				projected, complete = dw.baseCache.importAdjacency(clean, base.ProjectImportAdjacency)
			} else {
				projected, complete = base.ProjectImportAdjacency(clean)
			}
			if !complete {
				rest = paths
			} else {
				for _, p := range clean {
					filtered[p] = true
					for _, to := range projected[p] {
						if s.identityServed(to) {
							out[p] = append(out[p], to)
						}
					}
				}
			}
		}
	}
	if len(rest) == 0 {
		return out, filtered, true
	}
	endpoints, ok := EdgeEndpointsOf(view)
	if !ok {
		return nil, nil, false
	}
	fileOf := make(map[string]string)
	var ids []string
	var byFile map[string][]*Node
	if top {
		byFile, ok = dw.composedFileNodesByPaths(rest)
	} else if base, batched := s.base.(batchedFileNodesReader); batched {
		byFile, ok = dw.composeFileNodesOver(s, base, view, rest), true
	} else {
		ok = false
	}
	if !ok {
		byFile = make(map[string][]*Node, len(rest))
		for _, p := range rest {
			byFile[p] = view.GetFileNodes(p)
		}
	}
	for _, p := range rest {
		delete(out, p)
		delete(filtered, p)
		for _, n := range byFile[p] {
			if n == nil || n.ID == "" {
				continue
			}
			fileOf[n.ID] = p
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return out, filtered, true
	}
	for _, row := range endpoints.EdgeEndpointsFrom(ids, []EdgeKind{EdgeImports}) {
		if row.Kind != EdgeImports {
			continue
		}
		if file, ok := fileOf[row.From]; ok {
			out[file] = append(out[file], row.To)
		}
	}
	return out, filtered, true
}

// stackTouchedPaths is the subset of paths whose import rows below some
// layer of the stack may replace without covering the path: a layer carries
// an import edge out of the path's file node, or replaces or removes that
// node's adjacency (import edges leave the file node); for the delta's own
// layer, also a path an import-edge source it holds lives at. Only the
// requested paths are probed; no layer is read wholesale.
func (dw *DeltaWriter) stackTouchedPaths(s deltaStack, paths []string) map[string]struct{} {
	touched := make(map[string]struct{})
	for _, l := range s.layers {
		if l == OverlayLayerReader(dw.layer) {
			dw.layer.mu.RLock()
			for _, p := range paths {
				_, imports := dw.layer.importSources[p]
				_, claimed := dw.layer.claimed[p]
				_, removed := dw.layer.removed[p]
				if imports || claimed || removed {
					touched[p] = struct{}{}
				}
			}
			for id := range dw.layer.removed {
				touched[deltaPathKey(id)] = struct{}{}
			}
			dw.layer.mu.RUnlock()
			continue
		}
		var rest []string
		for _, p := range paths {
			if l.OwnsOutEdges(p) || l.IsRemovedID(p) {
				touched[p] = struct{}{}
				continue
			}
			rest = append(rest, p)
		}
		if len(rest) == 0 {
			continue
		}
		for from, edges := range dw.layerAdjacency(l, rest, false) {
			for _, e := range edges {
				if e != nil && e.Kind == EdgeImports {
					touched[from] = struct{}{}
					break
				}
			}
		}
	}
	return touched
}

// identityServed reports whether no layer hides an identity: none speaks for
// it without carrying a row for it.
func (s deltaStack) identityServed(id string) bool {
	for _, l := range s.layers {
		if (l.CoversNodeID(id) || l.OwnsNodeIdentity(id)) && l.NodeByID(id) == nil {
			return false
		}
	}
	return true
}

// CrossRepoCandidates implements CrossRepoCandidates. A working-tree layer
// describes one repository's checkout; it materializes no cross-repository
// edges (the sparse builder's generation-scoped pass never produced any
// either), so its candidate set is empty.
func (dw *DeltaWriter) CrossRepoCandidates([]EdgeKind) []CrossRepoCandidateRow { return nil }

// CrossRepoCandidatesForRepos implements ScopedCrossRepoCandidates.
func (dw *DeltaWriter) CrossRepoCandidatesForRepos([]EdgeKind, []string) []CrossRepoCandidateRow {
	return nil
}

// CrossRepoCandidatesForFiles implements ScopedCrossRepoCandidates.
func (dw *DeltaWriter) CrossRepoCandidatesForFiles([]EdgeKind, []string) []CrossRepoCandidateRow {
	return nil
}

// CrossRepoCandidatesForMutation implements MutationScopedCrossRepoCandidates.
func (dw *DeltaWriter) CrossRepoCandidatesForMutation([]EdgeKind, []string, []string) []CrossRepoCandidateRow {
	return nil
}

var (
	_ EdgeEndpointProvider              = (*DeltaWriter)(nil)
	_ ImportAdjacencyProjector          = (*DeltaWriter)(nil)
	_ CrossRepoCandidates               = (*DeltaWriter)(nil)
	_ ScopedCrossRepoCandidates         = (*DeltaWriter)(nil)
	_ MutationScopedCrossRepoCandidates = (*DeltaWriter)(nil)
)
