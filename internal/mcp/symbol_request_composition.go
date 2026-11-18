package mcp

import (
	"context"
	"reflect"

	"github.com/zzet/gortex/internal/graph"
)

// This observer records completion of the real constructor; it never supplies
// a reader, substitutes rows or grants filesystem/freshness authority.
type requestReaderCompositionObserverKey struct{}
type preparedSymbolReaderKey struct{}

// A symbol handler prepares one immutable base composition after scope and
// pending-source selection. The value lives only in that handler's context;
// canonical reads/revisions and all existing lease/contract/freshness checks
// still run. No result, revision, validity decision or source bytes are cached.
type preparedSymbolReader struct {
	server *Server
	view   *requestView
	base   graph.Reader
	layers []*graph.OverlayLayer
	reader graph.Reader
}

func sameSymbolReaderBase(a, b graph.Reader) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.ValueOf(a).Comparable() && reflect.ValueOf(b).Comparable() && a == b
}

func (p *preparedSymbolReader) matches(s *Server, ctx context.Context, base graph.Reader, layers []graph.OverlayLayerReader) bool {
	if p.server != s || p.view != requestViewFromContext(ctx) || !sameSymbolReaderBase(p.base, base) || len(p.layers) != len(layers) {
		return false
	}
	for i, layer := range layers {
		current, ok := layer.(*graph.OverlayLayer)
		if !ok || current != p.layers[i] {
			return false
		}
	}
	return true
}

func (s *Server) prepareSymbolRequestReader(ctx context.Context) context.Context {
	layers := requestProjectionLayers(ctx)
	if len(layers) < 2 {
		return ctx
	}
	facts, _ := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if facts == nil {
		return ctx
	}
	// Only immutable, actually borrowed pending-source layers are eligible.
	// Mutable/unsupported custom layers keep their existing construction path.
	facts.mu.RLock()
	borrowed := make(map[*graph.OverlayLayer]bool, len(facts.entries))
	for _, entry := range facts.entries {
		if entry != nil {
			borrowed[entry.layer] = true
		}
	}
	facts.mu.RUnlock()
	concrete := make([]*graph.OverlayLayer, 0, len(layers))
	for _, layer := range layers {
		current, ok := layer.(*graph.OverlayLayer)
		if !ok || current == nil || !borrowed[current] {
			return ctx
		}
		concrete = append(concrete, current)
	}
	view := requestViewFromContext(ctx)
	var base graph.Reader = s.graph
	if view.routed() {
		base = view.reader
	}
	if !sameSymbolReaderBase(base, base) {
		return ctx
	}
	prepared := &preparedSymbolReader{server: s, view: view, base: base, layers: concrete, reader: s.requestBaseReader(ctx)}
	return context.WithValue(ctx, preparedSymbolReaderKey{}, prepared)
}
