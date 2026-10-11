package indexer

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type capturedRepoNameCounter struct {
	graph.Reader
	store                  *store_sqlite.Store
	broad, scoped, decoded int
}

func (r *capturedRepoNameCounter) observe(yield func(*graph.Node) bool) func(*graph.Node) bool {
	return func(n *graph.Node) bool { r.decoded++; return yield(n) }
}
func (r *capturedRepoNameCounter) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	r.broad++
	return r.store.VisitNodesByNameContext(ctx, name, r.observe(yield))
}
func (r *capturedRepoNameCounter) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	r.broad++
	return r.store.VisitNodesByNamesContext(ctx, names, r.observe(yield))
}
func (r *capturedRepoNameCounter) VisitNodesByNamesInRepoContext(ctx context.Context, names []string, repo string, yield func(*graph.Node) bool) error {
	r.scoped++
	return r.store.VisitNodesByNamesInRepoContext(ctx, names, repo, r.observe(yield))
}

func TestContractFollowupCapturedRepoNamesAvoidRepeatedDecoding(t *testing.T) {
	s := newFTSStore(t)
	var nodes []*graph.Node
	for _, repo := range []string{"a", "b", "c", "d", "outside"} {
		for i := range 2 {
			id := fmt.Sprintf("%s/file.go::Match%d", repo, i)
			nodes = append(nodes, &graph.Node{ID: id, Name: "Match", Kind: graph.KindFunction, RepoPrefix: repo, FilePath: repo + "/file.go", Meta: map[string]any{"signature": "func Match()", "doc": "complete selected fields", "nested": map[string]any{"owner": repo}}})
		}
	}
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	_, selected, err := s.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "repo-name-a", LayerID: "selected", GenerationKind: "dirty", ConfigHash: "config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "test", CreatedAt: 1})
	require.NoError(t, err)
	require.NoError(t, selected.AddBatchChecked([]*graph.Node{{ID: "a/current.go::Match", Name: "Match", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/current.go", Meta: map[string]any{"selected": true}}}, nil))
	readers := map[string]graph.Reader{}
	var counters []*capturedRepoNameCounter
	var want []*graph.Node
	legacyDecoded := 0
	for _, repo := range []string{"a", "b", "c", "d"} {
		h := s.AtGeneration(0)
		if repo == "a" {
			h = selected
		}
		r := &capturedRepoNameCounter{Reader: h, store: h}
		readers[repo] = r
		counters = append(counters, r)
		// This is the unchanged dispatch's actual SQLite reference, including
		// every decoded row before repository ownership was checked in Go.
		require.NoError(t, h.VisitNodesByNameContext(t.Context(), "Match", func(n *graph.Node) bool {
			legacyDecoded++
			if n.RepoPrefix == repo {
				want = append(want, n)
			}
			return true
		}))
	}
	core := &contractCapturedCore{Reader: s, readers: readers}
	var got []*graph.Node
	require.NoError(t, graph.VisitNodesByNamesContext(t.Context(), core, []string{"Match"}, func(n *graph.Node) bool { got = append(got, n); return true }))
	require.ElementsMatch(t, want, got, "all selected node fields must match the legacy owned-row reference")
	decoded, broad, scoped := 0, 0, 0
	for _, r := range counters {
		decoded += r.decoded
		broad += r.broad
		scoped += r.scoped
	}
	t.Logf("four captured repos, three selecting generation0: legacy decoded=%d; qualified decoded=%d; broad=%d; scoped=%d", legacyDecoded, decoded, broad, scoped)
	require.Equal(t, 31, legacyDecoded)
	require.Equal(t, len(want), decoded, "only selected repository rows may cross the Store decoder")
	require.Zero(t, broad)
	require.Equal(t, len(readers), scoped)
}
