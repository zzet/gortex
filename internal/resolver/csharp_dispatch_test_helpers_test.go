package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// bindMemberCallAtLine mirrors the enrichment/LSP binding for a member
// call whose extraction companion carries NO receiver evidence - the
// exact state a shadow-refused site is in. bindFieldReceiverCall finds
// the companion by its receiver_name stamp, which such a site does not
// have; here the companion is found by member name alone.
func bindMemberCallAtLine(t *testing.T, g graph.Store, callerID, memberName, target string) {
	t.Helper()
	var companion *graph.Edge
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeCalls && e.To == "unresolved::*."+memberName {
			companion = e
			break
		}
	}
	require.NotNil(t, companion, "fixture: the extraction must leave an unresolved companion for the member call")
	g.AddEdge(&graph.Edge{
		From: callerID, To: target, Kind: graph.EdgeCalls,
		FilePath: companion.FilePath, Line: companion.Line,
		Origin: graph.OriginASTResolved, Confidence: 0.95,
	})
}

// dispatchTargets returns the fan-out targets minted from callerID.
func dispatchTargets(g graph.Store, callerID string) []string {
	var targets []string
	for _, e := range g.GetOutEdges(callerID) {
		if isIfaceDispatchEdge(e) {
			targets = append(targets, e.To)
		}
	}
	return targets
}

// callsFrom returns the EdgeCalls targets leaving fromID.
func callsFrom(g graph.Store, fromID string) []string {
	var out []string
	for _, e := range g.GetOutEdges(fromID) {
		if e != nil && e.Kind == graph.EdgeCalls {
			out = append(out, e.To)
		}
	}
	return out
}
