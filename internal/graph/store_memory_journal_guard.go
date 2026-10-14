package graph

import "reflect"

// Journal guard for the memory backend (issue davalillo/gortex#22): a warm
// reconcile re-adds the resident graph (watcher metadata replacement,
// enrichment catch-up — measured ~595 MB of journal per warm boot in #22).
// Every mutation was journaled even when it changed nothing.
//
// The guard computes EFFECTIVE changes at the journal boundary: inert
// mutations (the stored payload already equals the incoming one) are applied
// exactly as before but not journaled. Journal/replay consistency survives
// by induction — an op is inert only because the state already holds its
// effect, and that state was itself produced by an earlier journaled op or
// the snapshot, so a replay from any older journal/snapshot base reaches the
// same state without it.
//
// Concurrency: the lookups below read Graph shard maps without taking shard
// locks. That is safe because every MemoryStore mutation funnels through
// applyMu (the journaled/applyOnly critical sections), so the guard's reads
// are mutually exclusive with every writer; concurrent READERS only read the
// same maps. A caller that writes to the embedded *Graph directly, bypassing
// the MemoryStore write methods, breaks this invariant — the embedded Graph
// is backend-internal.

// existingNode returns the currently stored node for id, or nil.
func (m *MemoryStore) existingNode(id string) *Node {
	if id == "" {
		return nil
	}
	return m.Graph.GetNode(id)
}

// existingEdge returns the currently stored edge with e's logical identity,
// or nil. The lookup mirrors Graph.AddEdge's own index read (same key hash,
// same bucket) so inertness is decided against exactly the slot a re-add
// would replace.
func (m *MemoryStore) existingEdge(e *Edge) *Edge {
	if e == nil || e.From == "" || e.To == "" {
		return nil
	}
	sFrom := m.Graph.shardFor(e.From)
	if positions := sFrom.outEdgeIdx[e.From]; positions != nil {
		h := hashEdgeKey(keyOf(e))
		if pos, ok := positions[h]; ok && pos < len(sFrom.outEdges[e.From]) {
			return sFrom.outEdges[e.From][pos]
		}
	}
	return nil
}

// nodeInert reports whether AddNode over the stored node would change no
// observable state. reflect.DeepEqual on the full structs deliberately —
// a hand-picked field list would silently drift when Node grows a field,
// and a missed difference means a REAL change goes unjournaled (replay
// divergence). DeepEqual can only err in the safe direction: journaling an
// op that happens to be inert costs bytes, never correctness.
func nodeInert(old, n *Node) bool {
	if old == nil || n == nil {
		return false
	}
	if old == n {
		return true
	}
	return reflect.DeepEqual(old, n)
}

// edgeInert is nodeInert's edge counterpart (covers Meta and Confidence,
// both json:"-" and therefore invisible to the journal's own projection —
// a field-level comparison here would be doubly wrong).
func edgeInert(old, e *Edge) bool {
	if old == nil || e == nil {
		return false
	}
	if old == e {
		return true
	}
	return reflect.DeepEqual(old, e)
}

