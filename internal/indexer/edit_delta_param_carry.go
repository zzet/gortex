package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// carryIntoParam reports whether a parameter's incoming caller-owned dataflow
// (an argument another file passes into it) survives its file's structural
// reparse: its owning definition survives with an unchanged contract, so the
// dataflow pass re-derives the parameter under the same identity (parameters
// are not extracted; they are derived from the owner's signature) and the row
// the eviction deletes is the one a whole index of the edited tree holds.
//
// It is used by the restub step (restubIncomingRefsFromView). It lives here
// rather than beside it so a concurrent edit of that file cannot drop it.
func carryIntoParam(frontier restubFrontier, stage *incrementalBatchStage, param *graph.Node) bool {
	if frontier.conservative || param == nil {
		return false
	}
	ownerID, _, found := strings.Cut(param.ID, "#param:")
	if !found {
		return false
	}
	for _, node := range stage.priorNodes {
		if node != nil && node.ID == ownerID {
			return !frontier.requiresRestub(node)
		}
	}
	return false
}

// argOfIntoSurvivingOwner is the arg_of edge another file records into a
// parameter whose owning definition survives its file's reparse under the
// same identity but with a changed contract (a renamed, retyped or reordered
// parameter): carryIntoParam refuses it, and the eviction would delete it.
// No pass of the save re-derives it either — the caller is not reparsed —
// while a whole index of the edited tree binds the caller's argument to the
// owner's new parameter at the same position.
//
// It returns a copy of the edge pointed back at the owner, keeping its
// position Meta: the form the resolver leaves an arg_of edge in before
// dataflow materialization maps (callee, position) to a parameter. The
// affected-by pass re-materializes the caller's dataflow (the changed
// contract puts the caller in its frontier), which rewrites it to the new
// parameter, or leaves it on the owner where a whole index would too (no
// parameter at that position). nil when the edge is not an arg_of edge, the
// owner does not survive, or the stage is conservative.
func argOfIntoSurvivingOwner(frontier restubFrontier, param *graph.Node, edge *graph.Edge) *graph.Edge {
	if frontier.conservative || param == nil || edge == nil || edge.Kind != graph.EdgeArgOf {
		return nil
	}
	ownerID, _, found := strings.Cut(param.ID, "#param:")
	if !found || ownerID == "" {
		return nil
	}
	if _, survives := frontier.survivingIDs[ownerID]; !survives {
		return nil
	}
	if _, ok := argPositionFromMeta(edge.Meta); !ok {
		return nil
	}
	moved := *edge
	moved.To = ownerID
	if edge.Meta != nil {
		moved.Meta = make(map[string]any, len(edge.Meta))
		for k, v := range edge.Meta {
			moved.Meta[k] = v
		}
	}
	return &moved
}
