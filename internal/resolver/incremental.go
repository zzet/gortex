package resolver

import "github.com/zzet/gortex/internal/graph"

// Incremental single-file resolve: skip re-resolving a file's references that
// were already unresolved before the edit.
//
// On a save the whole file is evicted, re-parsed (every edge unresolved), and
// re-resolved. For a reference-heavy file most of those edges are stdlib /
// external calls (strings.HasPrefix, fmt.Sprintf, …) that never bind to an
// in-repo symbol — yet the resolver re-runs its full candidate fetch + cascade
// on each of them every save. That re-work dominates edit latency.
//
// An edge that was unresolved before the edit and is unchanged will not bind
// now either: it still points at the same stub, and the incoming pass already
// rebinds it if a matching symbol later appears (when that symbol's file is
// indexed). So the single-file path captures those prior-unresolved shapes and
// the forward pass skips them, touching only references the edit actually
// added or changed.

// SetIncrementalSkip installs the prior-unresolved out-edge shapes for the
// file about to be re-resolved (nil to clear afterwards). Only the forward
// pass honours it; the incoming pass still rebinds other files' references to
// this file's symbols. The per-file resolve runs single-goroutine under r.mu,
// so this field needs no extra synchronisation.
func (r *Resolver) SetIncrementalSkip(priorUnresolved []*graph.Edge) {
	r.incrementalTerminals = nil
	if len(priorUnresolved) == 0 {
		r.incrementalSkip = nil
		return
	}
	skip := make(map[string]struct{}, len(priorUnresolved))
	terminalOverflow := false
	for _, e := range priorUnresolved {
		if e != nil {
			skip[r.edgeShape(e)] = struct{}{}
			if !terminalOverflow && edgeTerminalFlag(e) && e.FilePath != "" && e.Line > 0 {
				if r.incrementalTerminals == nil {
					r.incrementalTerminals = make(map[graph.EdgeIdentity]*graph.Edge)
				}
				prior := *e
				prior.Meta = make(map[string]any, len(e.Meta))
				for key, value := range e.Meta {
					prior.Meta[key] = value
				}
				r.incrementalTerminals[graph.EdgeIdentityFor(e)] = &prior
				if len(r.incrementalTerminals) > attributionReindexBatchSize {
					r.incrementalTerminals = nil
					terminalOverflow = true
				}
			}
		}
	}
	r.incrementalSkip = skip
}

// edgeShape canonicalises an edge by its source-side identity — origin symbol,
// kind, receiver type, referenced name — independent of line number and of
// whether the edge is currently resolved. The captured prior edges and the
// freshly re-parsed ones run through the same function, so an unchanged
// reference produces the same key and is recognised as a carry-over.
func (r *Resolver) edgeShape(e *graph.Edge) string {
	name := identifierFromTarget(graph.UnresolvedName(e.To))
	return e.From + "\x1f" + string(e.Kind) + "\x1f" + edgeReceiverType(e) + "\x1f" + name
}

// incrementalSkipped reports whether an unresolved edge should be left for the
// incoming pass instead of re-running the forward cascade on it.
func (r *Resolver) incrementalSkipped(e *graph.Edge) bool {
	if r.incrementalSkip == nil {
		return false
	}
	_, ok := r.incrementalSkip[r.edgeShape(e)]
	return ok
}
