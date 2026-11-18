package graphview

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func contractRegistrySourceFixture(t *testing.T) (*store_sqlite.Store, *graph.Node, *graph.Node) {
	t.Helper()
	s := openTestStore(t)
	source := &graph.Node{ID: "repo/source.go::Wire", Name: "Wire", Kind: graph.KindFunction, FilePath: "repo/source.go", RepoPrefix: "repo", StartLine: 3, EndLine: 8, Meta: map[string]any{"body": "original full body", "return_type": "Value"}}
	target := &graph.Node{ID: "di::TOKEN", Name: "TOKEN", Kind: graph.KindContract, RepoPrefix: "repo", Meta: map[string]any{"type": "di"}}
	require.NoError(t, s.AddBatchChecked([]*graph.Node{source, target}, []*graph.Edge{{From: source.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: source.FilePath}}))
	return s, source, target
}

func TestContractRegistrySourceReadScopeReusesOnePhysicalCut(t *testing.T) {
	s, source, _ := contractRegistrySourceFixture(t)
	previous := acceptedContractInput("repo", "", "before")
	acceptPrimaryContractInput(t, s, nil, previous)
	inputs, err := newTestMaterializer(s).CaptureContractInputs(t.Context(), nil, "repo", "")
	require.NoError(t, err)
	scope := contractRegistrySourceReadScope{ids: []string{source.ID}}
	first, err := scope.read(t.Context(), s.AtGeneration(0))
	require.NoError(t, err)
	require.Equal(t, source, first[source.ID])
	next := previous
	next.InputFingerprint = "after"
	acceptPrimaryContractInput(t, s, &previous, next)
	changed := *source
	changed.Meta = map[string]any{"body": "changed full body", "return_type": "Other"}
	require.NoError(t, s.AddBatchChecked([]*graph.Node{&changed}, nil))
	// A second derived handle must reuse the first checked physical cut, not
	// silently read a different cut for another admitted repository.
	second, err := scope.read(t.Context(), s.AtGeneration(0))
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Len(t, scope.native, 1)
	require.ErrorIs(t, inputs.Validate(t.Context()), graph.ErrContractProjectionStale, "full input proof rejects the mutation after the shared physical cut")
	fresh := contractRegistrySourceReadScope{ids: []string{source.ID}}
	current, err := fresh.read(t.Context(), s.AtGeneration(0))
	require.NoError(t, err)
	require.Equal(t, &changed, current[source.ID], "a later LayerID scope must re-read, never retain the old source snapshot")
}

type contractRegistryCheckedSource struct {
	*store_sqlite.Store
	calls    int
	failure  error
	overflow bool
}

func (r *contractRegistryCheckedSource) LayerContractIDProjectionContext(context.Context, []string) (graph.ContractFileProjection, error) {
	r.calls++
	p := graph.ContractFileProjection{SourceNodes: map[string]*graph.Node{"partial": {ID: "partial", Kind: graph.KindFunction}}}
	if r.overflow {
		p.ScalarNodes = make([]*graph.Node, graph.ContractProjectionRowLimit+1)
	}
	return p, r.failure
}

func TestContractRegistrySourceReadScopeExcludesOtherViewsAndCheckedWrappers(t *testing.T) {
	s, source, _ := contractRegistrySourceFixture(t)
	scope := contractRegistrySourceReadScope{ids: []string{source.ID}}
	_, err := scope.read(t.Context(), s.AtGeneration(0))
	require.NoError(t, err)
	positive := *source
	positive.Meta = map[string]any{"body": "positive generation"}
	require.NoError(t, s.AtGeneration(41).AddBatchChecked([]*graph.Node{&positive}, nil))
	rows, err := scope.read(t.Context(), s.AtGeneration(41))
	require.NoError(t, err)
	require.Equal(t, &positive, rows[source.ID])
	require.Len(t, scope.native, 1, "positive handles do not share a gen0 source read")
	other, otherSource, _ := contractRegistrySourceFixture(t)
	otherSource.Meta = map[string]any{"body": "different open core"}
	require.NoError(t, other.AddBatchChecked([]*graph.Node{otherSource}, nil))
	rows, err = scope.read(t.Context(), other.AtGeneration(0))
	require.NoError(t, err)
	require.Equal(t, otherSource, rows[source.ID])
	require.Len(t, scope.native, 2)
	sentinel := errors.New("checked wrapper failed")
	wrapped := &contractRegistryCheckedSource{Store: s.AtGeneration(0), failure: sentinel}
	for range 2 {
		rows, err = scope.read(t.Context(), wrapped)
		require.ErrorIs(t, err, sentinel)
		require.Nil(t, rows)
	}
	require.Equal(t, 2, wrapped.calls, "promoted native-core methods never bypass a wrapper's checked authority")
	require.Len(t, scope.native, 2)
}

func TestContractRegistrySourceReadScopeKeepsErrorsLimitsAndCancellation(t *testing.T) {
	s, source, _ := contractRegistrySourceFixture(t)
	scope := contractRegistrySourceReadScope{ids: []string{source.ID}}
	raw, err := sql.Open("sqlite", s.Path())
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	_, err = raw.Exec(`UPDATE edges SET meta=x'0001FF' WHERE from_id=? AND view_gen=0`, source.ID)
	require.NoError(t, err)
	rows, err := scope.read(t.Context(), s.AtGeneration(0))
	require.Error(t, err)
	require.Nil(t, rows)
	require.Empty(t, scope.native, "failed physical reads are never retained")
	_, err = raw.Exec(`UPDATE edges SET meta=NULL WHERE from_id=? AND view_gen=0`, source.ID)
	require.NoError(t, err)
	rows, err = scope.read(t.Context(), s.AtGeneration(0))
	require.NoError(t, err)
	require.Equal(t, source, rows[source.ID])
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = scope.read(canceled, s.AtGeneration(0))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows, "cancellation also refuses an already retained physical cut")
	limited := &contractRegistryCheckedSource{Store: s.AtGeneration(0), overflow: true}
	rows, err = scope.read(t.Context(), limited)
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.Nil(t, rows)
	var absent *store_sqlite.Store
	rows, err = scope.read(t.Context(), absent)
	require.Error(t, err)
	require.Nil(t, rows)
}

func TestContractRegistrySourceHydrationKeepsSelectedMasksAndAllRepoConflicts(t *testing.T) {
	s, source, _ := contractRegistrySourceFixture(t)
	payload, _, _ := isolatedContractPayload(t, s, "/route", true)
	layer, err := NewGenerationLayer(payload)
	require.NoError(t, err)
	selected := *source
	selected.Meta = map[string]any{"body": "selected full body", "return_type": "Selected"}
	overlay := graph.NewOverlayLayer()
	overlay.AddNode(source.FilePath, &selected)
	view := graph.NewOverlaidViewWithLayer(s, overlay)
	r := &contractRegistryReader{Reader: view, layer: layer, sources: map[string]graph.Reader{"repo": view, "other": s.AtGeneration(0), "third": s.AtGeneration(0)}}
	p, err := r.LayerContractIDProjectionContext(t.Context(), []string{source.ID})
	require.NoError(t, err)
	require.Equal(t, &selected, p.SourceNodes[source.ID], "native companions must not replace selected source metadata")
	// Same ID in a second physical core with its own admitted owner remains
	// an ambiguity, rather than being discarded using the first row's repo hint.
	other := openTestStore(t)
	conflict := selected
	conflict.RepoPrefix = "foreign"
	require.NoError(t, other.AddBatchChecked([]*graph.Node{&conflict}, nil))
	r.sources["foreign"] = other.AtGeneration(0)
	p, err = r.LayerContractIDProjectionContext(t.Context(), []string{source.ID})
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.Equal(t, graph.ContractFileProjection{}, p)
	delete(r.sources, "foreign")
	removed := graph.NewOverlayLayer()
	removed.MarkFile(source.FilePath, true)
	r.sources["repo"] = graph.NewOverlaidViewWithLayer(view, removed)
	p, err = r.LayerContractIDProjectionContext(t.Context(), []string{source.ID})
	require.NoError(t, err)
	require.NotContains(t, p.SourceNodes, source.ID, "a deleted selected owner must not fall back to a companion's primary row")
	sentinel := errors.New("companion checked source failed")
	r.sources["other"] = &contractRegistryCheckedSource{Store: s.AtGeneration(0), failure: sentinel}
	p, err = r.LayerContractIDProjectionContext(t.Context(), []string{source.ID})
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, graph.ContractFileProjection{}, p)
}
