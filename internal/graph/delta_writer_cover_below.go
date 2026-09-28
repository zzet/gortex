package graph

// Covering a path from the stack's below rows.
//
// coverPaths copies a path's current rows into the working graph before the
// delta claims it: its nodes and every edge recorded at it, read through the
// composed view. For the delta's first eviction — the changed files, before
// anything is written — that read is a scan of every layer and the store at
// the bottom (recorded edges above all), repeated on every save of the file
// though the stack below does not change. A delta that has written and
// claimed nothing composes, at any path, exactly the view below's rows, and
// the installed below-rows source (SetBelowFileRows) keeps those per stack.

// coverFromBelowRows serves the paths the below-rows source holds while the
// delta is pristine, returning the paths left to read through the view and
// the rows served (copies). The served paths are recorded for coverPaths to
// claim.
func (dw *DeltaWriter) coverFromBelowRows(paths []string) (rest []string, nodes []*Node, edges []*Edge) {
	if dw.belowFileRows == nil || !dw.pristine() {
		return paths, nil, nil
	}
	for _, p := range paths {
		n, e, ok := dw.belowFileRows.BelowFileRows(p)
		if !ok {
			rest = append(rest, p)
			continue
		}
		nodes = append(nodes, cloneDeltaNodes(n)...)
		edges = append(edges, cloneDeltaEdges(e)...)
		dw.coverServedPaths = append(dw.coverServedPaths, p)
		dw.noteBelowRowsServed(len(n) + len(e))
	}
	return rest, nodes, edges
}

// pristine reports that the delta has written and claimed nothing: its view
// is the view below's everywhere.
func (dw *DeltaWriter) pristine() bool {
	if dw.work.NodeCount() != 0 || dw.work.EdgeCount() != 0 {
		return false
	}
	l := dw.layer
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.covered) == 0 && len(l.claimed) == 0 && len(l.removed) == 0
}
