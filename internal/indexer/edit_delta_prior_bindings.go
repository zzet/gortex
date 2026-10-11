package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// The forward leg's carry: which references of a structural batch keep their
// prior binding (resolver/prior_bindings.go).
//
// A fresh unresolved reference of a reparsed file carries its prior binding
// when all of these hold, and is re-bound by the resolver otherwise:
//
//   - its enclosing declaration survives under the same identity with an
//     unchanged declaration (its name is not among the batch's changed names),
//     and the prior reference sat at the same line relative to that
//     declaration's start (an edit elsewhere in the file shifts both alike);
//   - the prior reference had the same kind and named the same symbol: its
//     target's name is the fresh reference's bare name, and exactly one prior
//     target matches;
//   - the target survives: a target in a reparsed file keeps its identity and
//     its declaration (not among the changed names);
//   - nothing in the batch can change the candidates: no declaration of the
//     name changed in any reparsed file, the reparsed file imports the same
//     packages as before, and the stage keeps its out-edge reuse (a
//     visibility-stamp change or a forced re-parse drops both).
//
// The reference kinds that bind by name are carried (calls, references, field
// reads and writes, type positions, instantiations); hierarchy edges and the
// dataflow edges the attribution passes bind keep the full resolve.
var priorBindingKinds = map[graph.EdgeKind]struct{}{
	graph.EdgeCalls:        {},
	graph.EdgeReferences:   {},
	graph.EdgeReads:        {},
	graph.EdgeWrites:       {},
	graph.EdgeTypedAs:      {},
	graph.EdgeReturns:      {},
	graph.EdgeInstantiates: {},
}

// stagePriorBindings is the carry for one structural batch, read from the
// prior view before the batch evicts it.
func stagePriorBindings(g graph.Store, stages []*incrementalBatchStage, view incrementalPriorView) map[string]resolver.PriorBinding {
	if len(stages) == 0 {
		return nil
	}
	changedNames := make(map[string]struct{})
	surviving := make(map[string]struct{})
	reparsed := make(map[string]struct{}, len(stages))
	frontiers := make([]restubFrontier, len(stages))
	for i, stage := range stages {
		if stage == nil || stage.result == nil {
			return nil
		}
		frontier := restubFrontierForStage(stage, view)
		if frontier.conservative {
			return nil
		}
		frontiers[i] = frontier
		reparsed[stage.graphPath] = struct{}{}
		for name := range frontier.changedNames {
			changedNames[name] = struct{}{}
		}
		for id := range frontier.survivingIDs {
			surviving[id] = struct{}{}
		}
	}

	type priorRef struct {
		edge   *graph.Edge
		target string
	}
	type refKey struct {
		from string
		kind graph.EdgeKind
		rel  int
	}
	out := make(map[string]resolver.PriorBinding)
	for i, stage := range stages {
		if !sameImports(stage.priorNodes, stage.result.Nodes) {
			continue
		}
		// The same refusals as the out-edge reuse (commitIncrementalStages):
		// a using-stamp change re-prices every visibility-narrowed binding of
		// the file, and a forced re-parse (the importer of a deleted file)
		// must not short-cut its resolution. Both drop the stage's reuse.
		if stage.reuse == nil ||
			csharpVisibilityStampForNodes(stage.priorNodes) != csharpVisibilityStampForNodes(stage.result.Nodes) {
			continue
		}
		fresh := make(map[string]*graph.Node, len(stage.result.Nodes))
		for _, node := range stage.result.Nodes {
			if node != nil && node.ID != "" {
				fresh[node.ID] = node
			}
		}
		prior := make(map[refKey][]priorRef)
		var targetIDs []string
		for _, node := range stage.priorNodes {
			if node == nil || node.ID == "" {
				continue
			}
			now := fresh[node.ID]
			if now == nil {
				continue
			}
			if _, changed := frontiers[i].changedNames[node.Name]; changed && node.Name != "" {
				continue
			}
			for _, edge := range view.outByNode[node.ID] {
				if edge == nil || edge.FilePath != stage.graphPath {
					continue
				}
				if _, ok := priorBindingKinds[edge.Kind]; !ok {
					continue
				}
				if edge.To == "" || graph.IsUnresolvedTarget(edge.To) || graph.IsStub(edge.To) {
					continue
				}
				key := refKey{from: node.ID, kind: edge.Kind, rel: edge.Line - node.StartLine}
				prior[key] = append(prior[key], priorRef{edge: edge, target: edge.To})
				targetIDs = append(targetIDs, edge.To)
			}
		}
		if len(prior) == 0 {
			continue
		}
		targets := g.GetNodesByIDs(targetIDs)
		for _, edge := range stage.result.Edges {
			if edge == nil || !graph.IsUnresolvedTarget(edge.To) {
				continue
			}
			if _, ok := priorBindingKinds[edge.Kind]; !ok {
				continue
			}
			from := fresh[edge.From]
			if from == nil {
				continue
			}
			name := priorBindingRefName(edge.To)
			if name == "" {
				continue
			}
			if _, changed := changedNames[name]; changed {
				continue
			}
			var chosen *priorRef
			ambiguous := false
			for j := range prior[refKey{from: edge.From, kind: edge.Kind, rel: edge.Line - from.StartLine}] {
				candidate := &prior[refKey{from: edge.From, kind: edge.Kind, rel: edge.Line - from.StartLine}][j]
				target := targets[candidate.target]
				if target == nil || target.Name != name {
					continue
				}
				if _, inReparsed := reparsed[target.FilePath]; inReparsed {
					if _, survives := surviving[target.ID]; !survives {
						continue
					}
				}
				if chosen != nil && chosen.target != candidate.target {
					ambiguous = true
					break
				}
				chosen = candidate
			}
			if ambiguous || chosen == nil {
				continue
			}
			out[resolver.PriorBindingKey(edge.From, edge.Kind, edge.Line, edge.To)] = resolver.PriorBinding{
				To:         chosen.target,
				Origin:     chosen.edge.Origin,
				Confidence: chosen.edge.Confidence,
				Meta:       resolutionMetaAdded(chosen.edge.Meta, edge.Meta),
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// priorBindingRefName is the bare symbol name an unresolved reference target
// names: the last identifier of its unresolved form.
func priorBindingRefName(to string) string {
	name := graph.UnresolvedName(to)
	if name == "" {
		return ""
	}
	if i := strings.LastIndex(name, "::"); i >= 0 {
		name = name[i+2:]
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if strings.ContainsAny(name, "*#:/ ") {
		return ""
	}
	return name
}

// sameImports reports whether a file's import declarations are the same
// before and after its reparse (import nodes are keyed by path).
func sameImports(prior, fresh []*graph.Node) bool {
	collect := func(nodes []*graph.Node) map[string]struct{} {
		out := make(map[string]struct{})
		for _, node := range nodes {
			if node != nil && node.Kind == graph.KindImport {
				out[node.ID] = struct{}{}
			}
		}
		return out
	}
	a, b := collect(prior), collect(fresh)
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}

// resolutionMetaAdded is the Meta resolution added to a bound edge: the
// entries of the prior bound edge its fresh extraction does not carry.
func resolutionMetaAdded(prior, fresh map[string]any) map[string]any {
	var out map[string]any
	for k, v := range prior {
		if _, extracted := fresh[k]; extracted {
			continue
		}
		if out == nil {
			out = make(map[string]any)
		}
		out[k] = v
	}
	return out
}

// pruneBuiltinReuse drops the reuse entries that bind to a builtin. A
// per-file delta keeps its change set's reuse (reparseKeepingResolutions),
// and a whole index binds a builtin call through the resolver's builtin
// attribution, which also reconciles the dataflow rows keyed from the call's
// placeholder; a reused builtin binding skips that and leaves a value_flow a
// whole index does not hold. Those references are resolved in full.
func pruneBuiltinReuse(reuse map[reuseKey]*reuseVal) {
	for key, val := range reuse {
		if val != nil && graph.IsBuiltinStub(val.to) {
			delete(reuse, key)
		}
	}
}

// reuseSemanticMetaKeys are the entries a semantic confirmation stamps on an
// edge (semantic.ConfirmEdge).
var reuseSemanticMetaKeys = []string{"confirmed_from_origin", "semantic_source"}

// reuseSemanticMeta is the confirmation provenance of a captured bind, nil when
// it carries none. A confirmed bind's origin is LSP-grade, and a later
// confirmation of an edge that already carries an LSP-grade origin leaves it
// as it is, so a reuse that restored the origin without this provenance would
// leave the edge different from the one a whole index confirms.
func reuseSemanticMeta(edge *graph.Edge) map[string]any {
	if edge == nil || len(edge.Meta) == 0 {
		return nil
	}
	var out map[string]any
	for _, key := range reuseSemanticMetaKeys {
		if value, ok := edge.Meta[key]; ok {
			if out == nil {
				out = make(map[string]any, len(reuseSemanticMetaKeys))
			}
			out[key] = value
		}
	}
	return out
}

// applyReuseSemanticMeta restores a reused bind's confirmation provenance.
func applyReuseSemanticMeta(edge *graph.Edge, meta map[string]any) {
	if len(meta) == 0 {
		return
	}
	if edge.Meta == nil {
		edge.Meta = make(map[string]any, len(meta))
	}
	for key, value := range meta {
		edge.Meta[key] = value
	}
}

// stripReuseSemanticMeta keeps the established reuse (no confirmation
// provenance) for every caller but a per-file delta's change set.
func stripReuseSemanticMeta(reuse map[reuseKey]*reuseVal) {
	for _, val := range reuse {
		if val != nil {
			val.semanticMeta = nil
		}
	}
}
