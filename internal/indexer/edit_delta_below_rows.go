package indexer

import (
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// The view below's rows at a changed path, kept per stack.
//
// Publishing a delta compares each replaced path's rows with the rows the
// view below holds there (graph.DeltaWriter.Payload: the restated count and
// the drop of a covered path that ended as it was). Through a stack of
// generation layers that is one composed read of the path's nodes and of the
// edges recorded at it per delta. The stack is immutable, so the rows at a
// path are the same for every delta over it that edits the path: they are
// kept per stack key (editDeltaBaseCacheKey), like the prior view
// (edit_delta_prior_view.go), and read once.

var editDeltaBelowRows struct {
	sync.Mutex
	keys  []string // most recent last
	byKey map[string]map[string]editDeltaPathRows
}

type editDeltaPathRows struct {
	nodes    []*graph.Node
	recorded []*graph.Edge
}

// editDeltaBelowRowsSource answers graph.BelowFileRows for a delta over the
// stack key from the kept rows, reading and keeping a path on first use. Over
// a dirty chain (chain set), base is the view below the chain, the rows are
// kept for it, and the chain's layers are composed over them per read: the
// answer is the view below the delta's.
type editDeltaBelowRowsSource struct {
	key   string
	base  graph.Reader
	chain *graph.DeltaWriter
}

var _ graph.BelowFileRows = editDeltaBelowRowsSource{}

func (s editDeltaBelowRowsSource) BelowFileRows(path string) ([]*graph.Node, []*graph.Edge, bool) {
	nodes, recorded, ok := s.keptRows(path)
	if !ok || s.chain == nil {
		return nodes, recorded, ok
	}
	return s.chain.ChainFileNodesAt(path, nodes), s.chain.ChainRecordedEdgesAt([]string{path}, recorded), true
}

// keptRows is base's rows at path, from the kept rows or read and kept.
func (s editDeltaBelowRowsSource) keptRows(path string) ([]*graph.Node, []*graph.Edge, bool) {
	editDeltaBelowRows.Lock()
	if rows, ok := editDeltaBelowRows.byKey[s.key][path]; ok {
		editDeltaBelowRows.Unlock()
		return rows.nodes, rows.recorded, true
	}
	editDeltaBelowRows.Unlock()
	recorded, ok := graph.RecordedEdgesOf(s.base)
	if !ok {
		return nil, nil, false
	}
	rows := editDeltaPathRows{
		nodes:    s.base.GetFileNodes(path),
		recorded: recorded.RecordedEdgesAt([]string{path}),
	}
	editDeltaBelowRows.Lock()
	if editDeltaBelowRows.byKey == nil {
		editDeltaBelowRows.byKey = make(map[string]map[string]editDeltaPathRows)
	}
	m, ok := editDeltaBelowRows.byKey[s.key]
	if !ok {
		m = make(map[string]editDeltaPathRows)
		editDeltaBelowRows.byKey[s.key] = m
		editDeltaBelowRows.keys = append(editDeltaBelowRows.keys, s.key)
		for len(editDeltaBelowRows.keys) > editDeltaContractCacheEntries {
			delete(editDeltaBelowRows.byKey, editDeltaBelowRows.keys[0])
			editDeltaBelowRows.keys = editDeltaBelowRows.keys[1:]
		}
	}
	m[path] = rows
	editDeltaBelowRows.Unlock()
	return rows.nodes, rows.recorded, true
}
