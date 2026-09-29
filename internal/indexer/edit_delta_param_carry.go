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
