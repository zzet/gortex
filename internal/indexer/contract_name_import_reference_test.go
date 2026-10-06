package indexer

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The selected production visitor is synchronous/read-only. This observer
// reads private scratch to witness page boundaries; it never reenters evidence
// or writes derived rows during a visit. Those rows arise between lookups.
type namePageObservedReader struct {
	graph.Reader
	before func(int, *graph.Node)
	finish func()
}

func (r *namePageObservedReader) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	return r.VisitNodesByNamesContext(ctx, []string{name}, yield)
}
func (r *namePageObservedReader) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	ordinal := 0
	err := graph.VisitNodesByNamesContext(ctx, r.Reader, names, func(n *graph.Node) bool {
		ordinal++
		if r.before != nil {
			r.before(ordinal, n)
		}
		return yield(n)
	})
	if r.finish != nil {
		r.finish()
	}
	return err
}
func TestContractNameImportPagesPreserveSelectedRows(t *testing.T) {
	s := newFTSStore(t)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "a/old.go::Match", Name: "Match", Kind: graph.KindFunction, FilePath: "a/old.go", RepoPrefix: "a"}, {ID: "outside/f.go::Match", Name: "Match", Kind: graph.KindFunction, FilePath: "outside/f.go", RepoPrefix: "outside"}}, nil))
	_, selected, err := s.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "name-pages", LayerID: "selected", GenerationKind: "dirty", ConfigHash: "config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "test", CreatedAt: 1})
	require.NoError(t, err)
	var nodes []*graph.Node
	var ids []string
	for i := 0; i < 257; i++ {
		id := fmt.Sprintf("a/f.go::Match%03d", i)
		nodes = append(nodes, &graph.Node{ID: id, Name: "Match", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go", Language: "go", Meta: map[string]any{"doc": "full selected fields", "ordinal": i}})
		ids = append(ids, id)
	}
	require.NoError(t, selected.AddBatchChecked(nodes, nil))
	expected, err := selected.GetNodesByIDsContext(t.Context(), ids)
	require.NoError(t, err)
	e := completedNameEvidence(t, nil)
	r := &namePageObservedReader{Reader: &contractCapturedCore{Reader: s, readers: map[string]graph.Reader{"a": selected}}}
	e.core = r
	var visited []string
	r.before = func(ordinal int, n *graph.Node) {
		if ordinal == 2 {
			got, readErr := e.scratch.GetNodesByIDsContext(t.Context(), visited)
			require.NoError(t, readErr)
			require.Empty(t, got, "first missing row stays private until its page flush")
		}
		if ordinal == 129 {
			got, readErr := e.scratch.GetNodesByIDsContext(t.Context(), visited)
			require.NoError(t, readErr)
			require.Len(t, got, 128, "bounded page is committed before the next page")
		}
		visited = append(visited, n.ID)
	}
	got := e.FindNodesByNames([]string{"Match"})
	require.NoError(t, e.err)
	require.Len(t, got["Match"], 257)
	for _, n := range got["Match"] {
		require.Equal(t, expected[n.ID], n, "all persisted selected fields and authority remain exact")
	}
	require.Equal(t, 257, e.writtenNodes)
}
