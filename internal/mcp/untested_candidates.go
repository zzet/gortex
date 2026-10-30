package mcp

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

func untestedCandidates(ctx context.Context, reader graph.Reader, scope graph.NodeScope, filePrefix string) ([]*graph.Node, error) {
	var out []*graph.Node
	for _, kind := range []graph.NodeKind{graph.KindFunction, graph.KindMethod} {
		var rows iter.Seq[*graph.Node]
		if scoped, ok := reader.(graph.NodeScopedDeadCodeReader); ok {
			rows = scoped.NodesByKindInScope(kind, nil, scope)
		} else {
			rows = reader.NodesByKind(kind)
		}
		for node := range rows {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if node != nil && scope.Allows(node) && !isTestFile(node.FilePath) && graphpath.HasPrefix(node.FilePath, filePrefix) {
				out = append(out, node)
			}
		}
	}
	return out, ctx.Err()
}

// candidateTestCoverage walks callers of the candidate identities, retaining
// every reader-visible calls/references edge in that ancestor closure. Tests
// outside the candidate scope remain evidence: a real foreign caller is not an
// unrelated repository. Every test-to-candidate path lies in this closure, so
// a final forward walk over it agrees with seeding every test in the corpus
// without enumerating tests or edges disconnected from the candidates.
func candidateTestCoverage(ctx context.Context, reader graph.Reader, candidates []*graph.Node) (covered map[string]bool, fanIn map[string]int, err error) {
	seen := make(map[string]bool, len(candidates))
	candidateIDs := make(map[string]bool, len(candidates))
	frontier := make([]string, 0, len(candidates))
	for _, node := range candidates {
		if !seen[node.ID] {
			seen[node.ID] = true
			candidateIDs[node.ID] = true
			frontier = append(frontier, node.ID)
		}
	}
	fanIn = make(map[string]int, len(candidates))
	forward := make(map[string][]string)
	var tests []string
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}
	for len(frontier) > 0 {
		var next []string
		for _, id := range frontier {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			rows, err := graph.ReadKindAdjacency(ctx, reader, id, graph.DirectionBackward, kinds)
			if err != nil {
				return nil, nil, err
			}
			if candidateIDs[id] {
				fanIn[id] = len(rows.Edges)
			}
			for _, edge := range rows.Edges {
				forward[edge.From] = append(forward[edge.From], edge.To)
				if edge.From != "" && !seen[edge.From] {
					seen[edge.From] = true
					next = append(next, edge.From)
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		for _, node := range reader.GetNodesByIDs(next) {
			if node != nil && (node.Kind == graph.KindFunction || node.Kind == graph.KindMethod) && isTestFile(node.FilePath) {
				tests = append(tests, node.ID)
			}
		}
		frontier = next
	}
	covered = make(map[string]bool, len(tests))
	for _, id := range tests {
		covered[id] = true
	}
	for len(tests) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		var next []string
		for _, id := range tests {
			for _, target := range forward[id] {
				if !covered[target] {
					covered[target] = true
					next = append(next, target)
				}
			}
		}
		tests = next
	}
	return covered, fanIn, ctx.Err()
}
