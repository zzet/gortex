package indexer

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
)

// A changed file's prior adjacency, kept per stack.
//
// Before re-deriving a changed file the engine reads its prior nodes' in- and
// out-edges (the prior view): through a delta, a composed read of every layer
// for every node of the file, 0.8-1.1 s for config.go on a large store and
// up to 19 s cold. The delta has written nothing when it reads them, so they
// are the stack's rows, the same for every delta over the stack that edits
// the file: they are kept per stack and node identity. Over a dirty chain they
// are kept for the stack below the chain, and the layers above it are
// composed over them per read.

// priorViewLoads counts the identities read through the composition (tests).
var priorViewLoads atomic.Int64

type priorAdjacency struct {
	in, out []*graph.Edge
}

var editDeltaPriorViews struct {
	sync.Mutex
	keys  []string // most recent last
	byKey map[string]map[string]priorAdjacency
}

// resetEditDeltaPriorViews empties the cache (tests).
func resetEditDeltaPriorViews() {
	editDeltaPriorViews.Lock()
	editDeltaPriorViews.keys, editDeltaPriorViews.byKey = nil, nil
	editDeltaPriorViews.Unlock()
}

func stackPriorViews(key string) map[string]priorAdjacency {
	if editDeltaPriorViews.byKey == nil {
		editDeltaPriorViews.byKey = make(map[string]map[string]priorAdjacency)
	}
	m, ok := editDeltaPriorViews.byKey[key]
	if !ok {
		m = make(map[string]priorAdjacency)
		editDeltaPriorViews.byKey[key] = m
		editDeltaPriorViews.keys = append(editDeltaPriorViews.keys, key)
		for len(editDeltaPriorViews.keys) > editDeltaContractCacheEntries {
			delete(editDeltaPriorViews.byKey, editDeltaPriorViews.keys[0])
			editDeltaPriorViews.keys = editDeltaPriorViews.keys[1:]
		}
	}
	return m
}

func copyEdges(in []*graph.Edge) []*graph.Edge {
	if in == nil {
		return nil
	}
	out := make([]*graph.Edge, 0, len(in))
	for _, e := range in {
		if e == nil {
			continue
		}
		c := *e
		if e.Meta != nil {
			c.Meta = make(map[string]any, len(e.Meta))
			for k, v := range e.Meta {
				c.Meta[k] = v
			}
		}
		out = append(out, &c)
	}
	return out
}

// editDeltaPriorEdges answers a prior view's adjacency reads for a delta over
// the stack key: from the stack's kept rows while the delta has written
// nothing, reading and keeping the identities not kept yet.
func editDeltaPriorEdges(dw *graph.DeltaWriter, key string) func(ids []string) (map[string][]*graph.Edge, map[string][]*graph.Edge, bool) {
	return func(ids []string) (map[string][]*graph.Edge, map[string][]*graph.Edge, bool) {
		if dw == nil || !dw.Untouched() {
			return nil, nil, false
		}
		editDeltaPriorViews.Lock()
		kept := stackPriorViews(key)
		var missing []string
		for _, id := range ids {
			if _, ok := kept[id]; !ok {
				missing = append(missing, id)
			}
		}
		editDeltaPriorViews.Unlock()
		if len(missing) > 0 {
			priorViewLoads.Add(int64(len(missing)))
			sort.Strings(missing)
			// The kept rows are the part of the stack the caches are kept
			// for alone (below a dirty chain, if the delta stands on one);
			// the layers above it are composed per read below.
			in, inOK := dw.EdgesBelowChainByNodeIDs(missing, true)
			out, outOK := dw.EdgesBelowChainByNodeIDs(missing, false)
			if !inOK || !outOK {
				return nil, nil, false
			}
			editDeltaPriorViews.Lock()
			kept = stackPriorViews(key)
			for _, id := range missing {
				kept[id] = priorAdjacency{in: copyEdges(in[id]), out: copyEdges(out[id])}
			}
			editDeltaPriorViews.Unlock()
		}
		inBy := make(map[string][]*graph.Edge, len(ids))
		outBy := make(map[string][]*graph.Edge, len(ids))
		editDeltaPriorViews.Lock()
		kept = stackPriorViews(key)
		for _, id := range ids {
			adj := kept[id]
			if len(adj.in) > 0 {
				inBy[id] = adj.in
			}
			if len(adj.out) > 0 {
				outBy[id] = adj.out
			}
		}
		editDeltaPriorViews.Unlock()
		// The overlay copies every row it serves.
		return dw.OverlayEdgesAboveChain(ids, inBy, true), dw.OverlayEdgesAboveChain(ids, outBy, false), true
	}
}

// priorEdgesSources maps a delta's store to its prior adjacency source.
var priorEdgesSources sync.Map // graph.Store -> func([]string) (in, out, ok)

func setPriorEdgesSource(g graph.Store, source func([]string) (map[string][]*graph.Edge, map[string][]*graph.Edge, bool)) {
	if source == nil {
		priorEdgesSources.Delete(g)
		return
	}
	priorEdgesSources.Store(g, source)
}

func priorEdgesSourceOf(g graph.Store) func([]string) (map[string][]*graph.Edge, map[string][]*graph.Edge, bool) {
	if g == nil {
		return nil
	}
	source, ok := priorEdgesSources.Load(g)
	if !ok {
		return nil
	}
	return source.(func([]string) (map[string][]*graph.Edge, map[string][]*graph.Edge, bool))
}
