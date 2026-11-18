package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// resolvePHPDeclaredStatic recovers a declared method hidden by the extractor's
// Eloquent shortcut. The companion static-access reference carries the receiver
// FQN resolved through PHP imports (including aliases) and the caller namespace.
// Never fall back to a short-name match when that class is absent from the graph.
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
	fqn := ""
	for _, ref := range r.graph.GetOutEdges(e.From) {
		if ref.Kind != graph.EdgeReferences || ref.Line != e.Line || ref.FilePath != e.FilePath ||
			phpEdgeMetaString(ref, "ref_context") != graph.RefContextStaticAccess {
			continue
		}
		target := phpEdgeMetaString(ref, "target_fqn")
		if target == "" {
			// A global file without imports bypasses the namespace pass;
			// qualified spellings retain its scratch raw-type metadata.
			target = strings.TrimPrefix(phpEdgeMetaString(ref, "php_raw_type"), `\`)
		}
		if target == "" {
			target = model
		}
		// Line-level references cannot distinguish multiple receivers on one
		// line. Refuse a conflicting identity rather than borrow another call's.
		if fqn != "" && fqn != target {
			return false
		}
		fqn = target
	}
	if fqn == "" {
		return false
	}
	owners := r.phpFindByFQN(fqn, caller.RepoPrefix)
	if len(owners) != 1 || owners[0].Kind != graph.KindType || graph.IsUnresolvedTarget(owners[0].ID) {
		return false
	}
	owner := owners[0]
	var exact *graph.Node
	for _, c := range r.cachedFindNodesByNameInRepoForEdge(method, caller.RepoPrefix, e) {
		if c.Language != "php" || c.Kind != graph.KindMethod ||
			nodeReceiverType(c) != owner.Name || c.FilePath != owner.FilePath || graph.IsUnresolvedTarget(c.ID) {
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
