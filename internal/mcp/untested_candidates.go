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
	ids := make([]string, 0, len(candidates))
	candidateIDs := make(map[string]bool, len(candidates))
	for _, node := range candidates {
		if !candidateIDs[node.ID] {
			candidateIDs[node.ID] = true
			ids = append(ids, node.ID)
		}
	}
	rows, err := graph.CallAncestorsContext(ctx, reader, ids)
	if err != nil {
		return nil, nil, err
	}
	fanIn = make(map[string]int, len(candidates))
	forward := make(map[string][]string)
	var tests []string
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if candidateIDs[row.To] {
			fanIn[row.To]++
		}
		forward[row.From] = append(forward[row.From], row.To)
		if (row.FromKind == graph.KindFunction || row.FromKind == graph.KindMethod) && isTestFile(row.FromFile) {
			tests = append(tests, row.From)
		}
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
