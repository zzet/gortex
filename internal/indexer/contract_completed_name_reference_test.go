package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func completedNameSelectedFixture(t *testing.T) (*contractFollowupEvidence, []*capturedRepoNameCounter) {
	t.Helper()
	s := newFTSStore(t)
	var nodes []*graph.Node
	for _, repo := range []string{"a", "b", "outside"} {
		nodes = append(nodes, &graph.Node{ID: repo + "/base.go::Match", Name: "Match", Kind: graph.KindFunction, RepoPrefix: repo, FilePath: repo + "/base.go", Meta: map[string]any{"doc": "all persisted fields", "owner": repo}})
	}
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	_, selected, err := s.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "completed-name-a", LayerID: "selected", GenerationKind: "dirty", ConfigHash: "config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "test", CreatedAt: 1})
	require.NoError(t, err)
	require.NoError(t, selected.AddBatchChecked([]*graph.Node{{ID: "a/current.go::Match", Name: "Match", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/current.go", Meta: map[string]any{"current": true}}}, nil))
	readers := map[string]graph.Reader{}
	var counts []*capturedRepoNameCounter
	for _, repo := range []string{"a", "b"} {
		h := s
		if repo == "a" {
			h = selected
		}
		r := &capturedRepoNameCounter{Reader: h, store: h}
		readers[repo] = r
		counts = append(counts, r)
	}
	scratch := newFTSStore(t)
	e := &contractFollowupEvidence{Store: scratch, scratch: scratch, ctx: t.Context(), core: &contractCapturedCore{Reader: s, readers: readers}, allowedRepos: map[string]bool{"a": true, "b": true}}
	return e, counts
}

func TestContractCompletedNamesAvoidRepeatedCoreAndKeepLiveScratch(t *testing.T) {
	e, counts := completedNameSelectedFixture(t)
	first := e.FindNodesByNames([]string{"Match", "Missing"})
	require.NoError(t, e.err)
	var ids []string
	for _, n := range first["Match"] {
		ids = append(ids, n.ID)
	}
	require.ElementsMatch(t, []string{"a/current.go::Match", "b/base.go::Match"}, ids)
	require.Empty(t, first["Missing"])
	// Derived scratch evidence can appear after a completed negative lookup.
	e.AddNode(&graph.Node{ID: "a/derived.go::Missing", Name: "Missing", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/derived.go", Meta: map[string]any{"derived": true}})
	second := e.FindNodesByNames([]string{"Match", "Missing"})
	require.NoError(t, e.err)
	require.Equal(t, first["Match"], second["Match"], "full selected node fields remain exact")
	require.Len(t, second["Missing"], 1, "negative core completion cannot freeze scratch results")
	require.Equal(t, "a/derived.go::Missing", second["Missing"][0].ID)
	third := e.FindNodesByName("Match")
	require.Equal(t, first["Match"], third)
	for _, r := range counts {
		require.Equal(t, 1, r.scoped, "each selected repository should hydrate these names exactly once")
		require.Equal(t, 1, r.decoded)
		require.Zero(t, r.broad)
	}
}
