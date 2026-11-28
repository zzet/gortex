package mcp

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
	"github.com/zzet/gortex/internal/testpath"
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
			if node != nil && scope.Allows(node) && !graph.NodeIsTest(reader, node) && graphpath.HasPrefix(node.FilePath, filePrefix) {
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
	var tests, unstamped []string
	stampChecked := make(map[string]bool)
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if candidateIDs[row.To] {
			fanIn[row.To]++
		}
		forward[row.From] = append(forward[row.From], row.To)
		if row.FromKind != graph.KindFunction && row.FromKind != graph.KindMethod {
			continue
		}
		if testpath.IsTestFile(row.FromFile) {
			tests = append(tests, row.From)
		} else if !candidateIDs[row.From] && !stampChecked[row.From] {
			stampChecked[row.From] = true
			unstamped = append(unstamped, row.From)
		}
	}
	// The path check above is a fast path: the indexer stamps every function
	// in a test path is_test, and graph.NodeIsTest falls back to the same path
	// predicate. A runner that discovers tests by annotation (Rust #[test], JVM
	// @Test) lets a test live in a production path, where only the stamp says
	// so. Candidates were already classified by it, so only the remaining
	// sources need it, in one batched lookup.
	if len(unstamped) > 0 {
		for id, node := range reader.GetNodesByIDs(unstamped) {
			if graph.NodeIsTest(reader, node) {
				tests = append(tests, id)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
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
