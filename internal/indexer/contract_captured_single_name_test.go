package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractFollowupCapturedSingleNameKeepsSelectedNamespaces(t *testing.T) {
	store := newFTSStore(t)
	base := []*graph.Node{
		{ID: "a/base.go::Match", Name: "Match", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/base.go"},
		{ID: "b/base.go::Match", Name: "Match", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/base.go"},
		{ID: "Trellis/outside.go::Match", Name: "Match", Kind: graph.KindFunction, RepoPrefix: "Trellis", FilePath: "Trellis/outside.go"},
	}
	require.NoError(t, store.AddBatchChecked(base, nil))
	_, selected, err := store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "a-selected", LayerID: "selected", GenerationKind: "dirty", ConfigHash: "config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "test", CreatedAt: 1})
	require.NoError(t, err)
	require.NoError(t, selected.AddBatchChecked([]*graph.Node{{ID: "a/selected.go::Match", Name: "Match", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/selected.go"}}, nil))
	core := &contractCapturedCore{Reader: store, readers: map[string]graph.Reader{"a": selected, "b": store}}
	scratch := newFTSStore(t)
	e := &contractFollowupEvidence{Store: scratch, scratch: scratch, ctx: t.Context(), core: core, allowedRepos: map[string]bool{"a": true, "b": true}}
	found := e.FindNodesByNames([]string{"Match"})
	require.NoError(t, e.err, "a singleton must not consult the broad primary fallback")
	var ids []string
	for _, node := range found["Match"] {
		ids = append(ids, node.ID)
	}
	require.ElementsMatch(t, []string{"a/selected.go::Match", "b/base.go::Match"}, ids, "selected positive a replaces primary a; admitted primary b remains; broad-only Trellis stays outside the captured cohort")
	var plural []string
	require.NoError(t, graph.VisitNodesByNamesContext(t.Context(), core, []string{"Match", "missing"}, func(node *graph.Node) bool { plural = append(plural, node.ID); return true }))
	require.ElementsMatch(t, ids, plural)
	stopped := 0
	require.NoError(t, graph.VisitNodesByNamesContext(t.Context(), core, []string{"Match"}, func(*graph.Node) bool { stopped++; return false }))
	require.Equal(t, 1, stopped, "a stop must not continue into the next admitted namespace")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, graph.VisitNodesByNamesContext(ctx, core, []string{"Match"}, func(*graph.Node) bool { t.Fatal("canceled visit produced a row"); return true }), context.Canceled)
}

type contractCapturedFailingNameReader struct {
	graph.Reader
	err error
}

func (r contractCapturedFailingNameReader) VisitNodesByNameContext(context.Context, string, func(*graph.Node) bool) error {
	return r.err
}

func TestContractFollowupCapturedSingleNamePreservesReadFailure(t *testing.T) {
	cause := errors.New("selected namespace read failed")
	core := &contractCapturedCore{Reader: graph.New(), readers: map[string]graph.Reader{"a": contractCapturedFailingNameReader{Reader: graph.New(), err: cause}}}
	require.ErrorIs(t, graph.VisitNodesByNamesContext(t.Context(), core, []string{"Match"}, func(*graph.Node) bool { t.Fatal("failed selected read produced a row"); return true }), cause)
}
