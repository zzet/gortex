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

type capturedRepoDeclarationLayer struct {
	*graph.OverlayLayer
	counter *capturedRepoNameCounter
}

func (r *capturedRepoDeclarationLayer) VisitNodeDeclarationsByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	r.counter.broad++
	return r.counter.store.VisitNodeDeclarationsByNamesContext(ctx, names, r.counter.observe(yield))
}
func (r *capturedRepoDeclarationLayer) VisitNodeDeclarationsByNamesInRepoContext(ctx context.Context, names []string, repo string, yield func(*graph.Node) bool) error {
	r.counter.scoped++
	return r.counter.store.VisitNodeDeclarationsByNamesInRepoContext(ctx, names, repo, r.counter.observe(yield))
}

// This follows the actual captured-repository dispatcher through a chain of
// checked SQLite layers. Common names in unrelated repositories must never
// cross those physical decoders just to be discarded by the outer dispatcher.
func TestContractCapturedDeclarationsKeepRepoScopeAcrossLayers(t *testing.T) {
	s := newFTSStore(t)
	var view graph.Reader = graph.New()
	var counters []*capturedRepoNameCounter
	var want []*graph.Node
	for gen := int64(0); gen < 8; gen++ {
		h := s.AtGeneration(gen)
		var nodes []*graph.Node
		for _, repo := range []string{"selected", "foreign-a", "foreign-b", "foreign-c"} {
			n := &graph.Node{ID: fmt.Sprintf("%s/file%d.go::Shared", repo, gen), Name: "Shared", Kind: graph.KindConstant, RepoPrefix: repo, FilePath: fmt.Sprintf("%s/file%d.go", repo, gen)}
			nodes = append(nodes, n)
			if repo == "selected" {
				want = append(want, n)
			}
		}
		require.NoError(t, h.AddBatchChecked(nodes, nil))
		counter := &capturedRepoNameCounter{Reader: h, store: h}
		counters = append(counters, counter)
		view = graph.NewOverlaidViewWithLayer(view, &capturedRepoDeclarationLayer{OverlayLayer: graph.NewOverlayLayer(), counter: counter})
	}
	core := &contractCapturedCore{Reader: s, readers: map[string]graph.Reader{"selected": view}}
	var got []*graph.Node
	require.NoError(t, graph.VisitNodeDeclarationsByNamesContext(t.Context(), core, []string{"Shared", "Missing"}, func(n *graph.Node) bool { got = append(got, n); return true }))
	require.ElementsMatch(t, want, got, "scope propagation must not change any selected declaration fact")
	decoded, broad, scoped := 0, 0, 0
	for _, c := range counters {
		decoded += c.decoded
		broad += c.broad
		scoped += c.scoped
	}
	t.Logf("eight physical layers: selected=%d decoded=%d broad=%d scoped=%d", len(want), decoded, broad, scoped)
	require.Equal(t, len(want), decoded, "foreign same-name rows must stay behind the physical repository filter")
	require.Zero(t, broad)
	require.Equal(t, len(counters), scoped)
}
