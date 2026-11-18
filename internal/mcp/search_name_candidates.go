package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// The selected reader must certify selection and retained payloads as one
// coherent view. Process cache tokens do not certify a paged SQL snapshot.
func nameOnlySymbolCandidates(ctx context.Context, reader graph.Reader, needle string, horizon int, scope query.QueryOptions) ([]*graph.Node, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	coherent, ok := reader.(graph.FoldedNameCandidateReader)
	if !ok || horizon <= 0 {
		return nil, false, projectionSuperseded
	}
	return coherent.FindFoldedNameCandidatesContext(ctx, needle, horizon, graph.NameSearchFilter{RepoAllow: scope.RepoAllow, Accept: func(node *graph.Node) bool {
		return scope.ScopeAllows(node) && (scope.SearchNodeFilter == nil || scope.SearchNodeFilter(node))
	}})
}
