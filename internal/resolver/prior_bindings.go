package resolver

import (
	"strconv"

	"github.com/zzet/gortex/internal/graph"
)

// Prior bindings: the forward leg's carry.
//
// A structural reparse turns every reference of the changed file back into an
// unresolved edge, and the forward leg re-binds each one (600-1,300 per save of
// a large file): pass indexes, lookup warming, the resolution itself. Most of
// those references bind exactly as before: the reference is at the same place
// in the same unchanged enclosing declaration, it names a target whose
// declaration did not change, and nothing the save changed can add or remove a
// candidate for it (no declaration of its name changed, the file imports the
// same packages).
//
// The caller (the incremental engine) decides which references qualify and
// hands the resolver their prior binding keyed by the fresh edge's identity
// (PriorBindingKey). The forward leg applies a carried binding exactly as a
// resolution would be applied — the same reindex batch, the same
// cross-package guard — and leaves the edge out of the pending set that
// drives the pass indexes and the lookup warm-up. The evidence is
// transaction-scoped like SetIncrementalSkip: installed for one catch-up
// resolve and cleared.

// PriorBinding is the resolution a reference held before the save: the target
// and the provenance the resolver stamped, plus the Meta entries resolution
// added to the edge.
type PriorBinding struct {
	To         string
	Origin     string
	Confidence float64
	Meta       map[string]any
}

// PriorBindingKey identifies a fresh unresolved edge: its source, kind, line
// and unresolved target.
func PriorBindingKey(from string, kind graph.EdgeKind, line int, to string) string {
	return from + "\x1f" + string(kind) + "\x1f" + strconv.Itoa(line) + "\x1f" + to
}

// SetPriorBindings installs the prior bindings the next incremental resolve
// may carry (nil clears).
func (r *Resolver) SetPriorBindings(bindings map[string]PriorBinding) {
	if len(bindings) == 0 {
		r.priorBindings = nil
		return
	}
	r.priorBindings = bindings
}

// carryPriorBindingsLocked applies the installed prior bindings to the
// frontier's outgoing pending edges, removes the carried ones from the
// pending set and returns them. The caller holds r.mu.
func (r *Resolver) carryPriorBindingsLocked(frontier *incrementalFileFrontier, stats *ResolveStats) []*graph.Edge {
	if len(r.priorBindings) == 0 || frontier.outgoingPending == 0 {
		return nil
	}
	var reindexBatch []graph.EdgeReindex
	var jobs []reindexJob
	kept := make([]*graph.Edge, 0, len(frontier.pending))
	var carried []*graph.Edge
	outgoing := 0
	for i, edge := range frontier.pending {
		if i >= frontier.outgoingPending {
			kept = append(kept, edge)
			continue
		}
		binding, ok := r.priorBindingFor(edge)
		if !ok {
			kept = append(kept, edge)
			outgoing++
			continue
		}
		oldTo, oldKind := edge.To, edge.Kind
		edge.To = binding.To
		edge.Origin = binding.Origin
		edge.Confidence = binding.Confidence
		if len(binding.Meta) > 0 {
			if edge.Meta == nil {
				edge.Meta = make(map[string]any, len(binding.Meta))
			}
			for k, v := range binding.Meta {
				edge.Meta[k] = v
			}
		}
		stats.Resolved++
		carried = append(carried, edge)
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: edge, OldTo: oldTo, OldKind: oldKind})
		jobs = append(jobs, reindexJob{
			edge: edge, oldTo: oldTo, oldKind: oldKind, newTo: edge.To,
			kind: edge.Kind, confidence: edge.Confidence, origin: edge.Origin,
		})
	}
	if len(reindexBatch) == 0 {
		return nil
	}
	frontier.pending = kept
	frontier.outgoingPending = outgoing
	// Applied with the outgoing leg's own batch (resolvePreparedFileEdgesLocked):
	// one reindex, one placeholder reconciliation and one cross-package guard
	// for the carried and the resolved edges together. A separate apply
	// rebuilt the guard's import closure a second time (0.4-0.5 s per delta).
	r.carriedReindex, r.carriedJobs = reindexBatch, jobs
	return carried
}

// takeCarriedReindexes hands the carried edges' reindex batch to the leg that
// applies it, once.
func (r *Resolver) takeCarriedReindexes() ([]graph.EdgeReindex, []reindexJob) {
	batch, jobs := r.carriedReindex, r.carriedJobs
	r.carriedReindex, r.carriedJobs = nil, nil
	return batch, jobs
}

func (r *Resolver) priorBindingFor(edge *graph.Edge) (PriorBinding, bool) {
	if edge == nil || !graph.IsUnresolvedTarget(edge.To) || r.incrementalSkipped(edge) {
		return PriorBinding{}, false
	}
	binding, ok := r.priorBindings[PriorBindingKey(edge.From, edge.Kind, edge.Line, edge.To)]
	if !ok || binding.To == "" {
		return PriorBinding{}, false
	}
	return binding, true
}
