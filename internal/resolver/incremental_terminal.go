package resolver

import (
	"context"
	"reflect"

	"github.com/zzet/gortex/internal/graph"
)

// This is continuation of an existing full-pass proof, not a new scoped
// absence policy. Exact old terminal rows must survive unchanged, and their
// name classification must still hold after the file's resolve completed.
// Callers hold r.mu; no old row or missing/failed lookup can mint a new edge.
func (r *Resolver) restoreIncrementalTerminalStampsLocked(files []string) {
	if len(r.incrementalTerminals) == 0 {
		return
	}
	finder, ok := r.graph.(graph.EdgeIdentityBatchFinder)
	if !ok {
		return
	}
	// Unknown adapters without an error-reporting exact-name visitor cannot
	// certify absence. The in-memory graph's direct lookup is infallible.
	_, nativeNames := r.graph.(interface {
		VisitNodeDeclarationsByNamesContext(context.Context, []string, func(*graph.Node) bool) error
	})
	_, memory := r.graph.(*graph.Graph)
	if !nativeNames && !memory {
		return
	}
	allowed := make(map[string]bool, len(files))
	for _, file := range files {
		allowed[file] = true
	}
	keys := make([]graph.EdgeIdentity, 0, len(r.incrementalTerminals))
	for key, old := range r.incrementalTerminals {
		if !allowed[key.FilePath] || old.Kind == graph.EdgeImports {
			continue
		}
		// A supported LSP path can have an outstanding/failed definition lookup.
		// Keep it pending rather than reuse terminality across that uncertainty.
		if r.lspHelper != nil && r.lspHelper.SupportsPath(key.FilePath) {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return
	}
	current := finder.FindEdgesByIdentities(keys)
	var pending []*graph.Edge
	names := map[string]graph.NodeNameClassCount{}
	for _, key := range keys {
		edge := current[key]
		if edge == nil || !graph.IsUnresolvedTarget(edge.To) || !sameTerminalCarryPayload(r.incrementalTerminals[key], edge) {
			continue
		}
		name := identifierFromTarget(graph.UnresolvedName(edge.To))
		if name == "" {
			continue
		}
		names[name] = graph.NodeNameClassCount{}
		pending = append(pending, edge)
	}
	if len(pending) == 0 {
		return
	}
	requested := make([]string, 0, len(names))
	for name := range names {
		requested = append(requested, name)
	}
	visited, overflow := 0, false
	err := graph.VisitNodeDeclarationsByNamesContext(context.Background(), r.graph, requested, func(n *graph.Node) bool {
		visited++
		if visited > attributionReindexBatchSize {
			overflow = true
			return false
		}
		if n == nil {
			return true
		}
		count, asked := names[n.Name]
		if !asked {
			return true
		}
		if graph.IsStub(n.ID) {
			count.Stub++
		} else if nodeIsDefinitionKind(n.Kind) {
			count.Real++
		}
		names[n.Name] = count
		return true
	})
	if err != nil || overflow {
		return
	}
	var changed []*graph.Edge
	for _, edge := range pending {
		count := names[identifierFromTarget(graph.UnresolvedName(edge.To))]
		if count.Real > 0 {
			if edgeTerminalFlag(edge) {
				clearEdgeTerminal(edge)
				changed = append(changed, edge)
			}
			continue
		}
		reason := terminalReasonNoDefinition
		if count.Stub > 0 {
			reason = terminalReasonStubOnly
		}
		setEdgeTerminal(edge, reason)
		changed = append(changed, edge)
	}
	if p, ok := r.graph.(graph.EdgeTerminalStampPersister); ok {
		p.PersistEdgeTerminalStamps(changed)
	} else if p, ok := r.graph.(graph.EdgeMetaBatchPersister); ok {
		p.PersistEdgeAttributesBatch(changed)
	}
}

func sameTerminalCarryPayload(old, current *graph.Edge) bool {
	if old == nil || current == nil {
		return false
	}
	a, b := *old, *current
	a.Meta, b.Meta = nil, nil
	if !reflect.DeepEqual(a, b) {
		return false
	}
	count := func(m map[string]any) int {
		n := len(m)
		if _, ok := m[metaResolveTerminal]; ok {
			n--
		}
		if _, ok := m[metaResolveTerminalReason]; ok {
			n--
		}
		return n
	}
	if count(old.Meta) != count(current.Meta) {
		return false
	}
	for key, value := range old.Meta {
		if key == metaResolveTerminal || key == metaResolveTerminalReason {
			continue
		}
		other, ok := current.Meta[key]
		if !ok || !reflect.DeepEqual(value, other) {
			return false
		}
	}
	return true
}
