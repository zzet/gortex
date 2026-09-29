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
	out := make(map[string][]string, len(paths))
	rest := paths
	s := dw.stack()
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
		return out, true
	}
	endpoints, ok := EdgeEndpointsOf(dw.view)
	if !ok {
		return nil, false
	}
	fileOf := make(map[string]string)
	var ids []string
	byFile, ok := dw.composedFileNodesByPaths(rest)
	if !ok {
		byFile = make(map[string][]*Node, len(rest))
		for _, p := range rest {
			byFile[p] = dw.view.GetFileNodes(p)
		}
	}
	for _, p := range rest {
		delete(out, p)
		for _, n := range byFile[p] {
			if n == nil || n.ID == "" {
				continue
			}
			fileOf[n.ID] = p
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return out, true
	}
	for _, row := range endpoints.EdgeEndpointsFrom(ids, []EdgeKind{EdgeImports}) {
		if row.Kind != EdgeImports {
			continue
		}
		if file, ok := fileOf[row.From]; ok {
			out[file] = append(out[file], row.To)
		}
	}
	return out, true
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
