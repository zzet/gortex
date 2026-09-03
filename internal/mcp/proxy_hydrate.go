package mcp

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// annotateProxyFreshness sets sg.LastSynced to the stalest fetched-at among
// any federation proxy nodes in the result, surfacing how fresh the
// remote-derived part of the answer is. A no-op when the result holds no
// proxy node, so a purely-local result is unchanged.
func annotateProxyFreshness(sg *query.SubGraph) {
	if sg == nil {
		return
	}
	var oldest time.Time
	for _, n := range sg.Nodes {
		if !graph.IsProxyNode(n) || n.FetchedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || n.FetchedAt.Before(oldest) {
			oldest = n.FetchedAt
		}
	}
	if !oldest.IsZero() {
		sg.LastSynced = &oldest
	}
}

// SetProxyHydrator installs the cross-daemon proxy-edge lazy hydration hook.
// The daemon wires it to ProxyHydrator.Hydrate when federation.edges is
// enabled; nil (the default) makes hydrateProxyTargets a no-op, so
// pure-local and read-only-fan-out-only installs pay nothing.
func (s *Server) SetProxyHydrator(h func(ctx context.Context, proxyID string) (int, error)) {
	s.proxyHydrate = h
}

// hydrateProxyTargets pulls one neighbour ring for any federation proxy
// node directly reachable from id (or id itself, when id is already a
// proxy), so a traversal that crosses into a proxy node sees a ring of the
// remote's neighbours rather than a dead end. A no-op when no hydrator is
// installed or there are no proxy targets. Errors are swallowed — a failed
// hydration degrades to the un-hydrated single-hop view, never a tool
// failure.
func (s *Server) hydrateProxyTargets(ctx context.Context, id string) {
	if s.proxyHydrate == nil || s.graph == nil || id == "" {
		return
	}
	if graph.IsProxyID(id) {
		_, _ = s.proxyHydrate(ctx, id)
		return
	}
	// Base read on purpose: hydration writes the fetched ring into the base
	// graph, so the edges it looks for must be read from there too.
	for _, e := range s.graph.GetOutEdges(id) {
		if graph.IsProxyID(e.To) {
			_, _ = s.proxyHydrate(ctx, e.To)
		}
	}
}
