package resolver

import "github.com/zzet/gortex/internal/graph"

// resolvePHPDeclaredStatic recovers a declared method hidden by the extractor's
// Eloquent shortcut. Short class names can collide across namespaces, so require
// a unique class as well as a unique method; locality is not evidence here.
func (r *Resolver) resolvePHPDeclaredStatic(e *graph.Edge, stats *ResolveStats) bool {
	method := phpEdgeMetaString(e, "eloquent_method")
	if method == "" {
		return false
	}
	caller := r.cachedGetNode(e.From)
	if caller == nil || caller.Language != "php" {
		return false
	}
	model := phpEdgeMetaString(e, "eloquent_model")
	var owner *graph.Node
	for _, c := range r.cachedFindNodesByNameInRepoForEdge(model, caller.RepoPrefix, e) {
		if c.Language != "php" || c.Kind != graph.KindType || graph.IsUnresolvedTarget(c.ID) {
			continue
		}
		if owner != nil && owner.ID != c.ID {
			return false
		}
		owner = c
	}
	if owner == nil {
		return false
	}
	var exact *graph.Node
	for _, c := range r.cachedFindNodesByNameInRepoForEdge(method, caller.RepoPrefix, e) {
		if c.Language != "php" || c.Kind != graph.KindMethod ||
			nodeReceiverType(c) != model || c.FilePath != owner.FilePath || graph.IsUnresolvedTarget(c.ID) {
			continue
		}
		if exact != nil && exact.ID != c.ID {
			return false
		}
		exact = c
	}
	if exact == nil {
		return false
	}
	e.To = exact.ID
	e.Origin = graph.OriginASTResolved
	e.Confidence = 0.85 // Same evidence as resolveMethodCall's exact-type pass.
	stats.Resolved++
	return true
}
