package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func selectedSemanticGeneration(t *testing.T, store *store_sqlite.Store, kind, layer string, parent int64) *store_sqlite.Store {
	t.Helper()
	_, handle, err := store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: "semantic-fixture", CheckoutID: "fixture-checkout",
		LayerID: layer, GenerationKind: kind, BaseGenerationID: parent, CreatedAt: 1,
	})
	require.NoError(t, err)
	return handle
}

func selectedSemanticFile(t *testing.T, store *store_sqlite.Store, path, hash string) {
	t.Helper()
	require.NoError(t, store.AddBatchChecked([]*graph.Node{{ID: path, FilePath: path, RepoPrefix: "fixture", Kind: graph.KindFile}}, nil))
	require.NoError(t, store.SetFileMetas("fixture", []graph.FileMetaRow{{FilePath: path, ContentHash: hash, NodeCount: 1}}))
}

func TestContractSelectedComposedSemanticInputsExcludeMutableLatest(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	_, store := newSQLiteIndexer(t)
	changed := graph.SemanticBindingSite{RepoPrefix: "fixture", FilePath: "fixture/changed.go", Line: 3, Name: "response"}
	inherited := graph.SemanticBindingSite{RepoPrefix: "fixture", FilePath: "fixture/inherited.go", Line: 4, Name: "response"}
	missing := changed
	missing.Name = "absent"
	require.NoError(t, store.ReplaceSemanticBindingTypes("fixture", []graph.SemanticBindingType{
		{Site: changed, TypeName: "WrongMutableLatest"}, {Site: inherited, TypeName: "WrongMutableParent"}, {Site: missing, TypeName: "WrongMutableAbsence"},
	}))
	base := selectedSemanticGeneration(t, store, "dedicated", "semantic-parent", 0)
	selectedSemanticFile(t, base, changed.FilePath, "parent-changed")
	selectedSemanticFile(t, base, inherited.FilePath, "parent-inherited")
	require.NoError(t, base.ReplaceSemanticBindingTypes("fixture", []graph.SemanticBindingType{
		{Site: changed, TypeName: "OldSelectedResponse"}, {Site: inherited, TypeName: "SelectedParent"},
	}))
	require.NoError(t, store.PublishPayloadGeneration(t.Context(), base.ViewGeneration(), 2))
	upper := selectedSemanticGeneration(t, store, "dirty", "semantic-upper", base.ViewGeneration())
	selectedSemanticFile(t, upper, changed.FilePath, "accepted-current")
	require.NoError(t, upper.ReplaceSemanticBindingTypes("fixture", []graph.SemanticBindingType{{Site: changed, TypeName: "SelectedResponse"}}))
	require.NoError(t, upper.SetFileMasks([]store_sqlite.FileMask{{FilePath: changed.FilePath, RepoPrefix: "fixture", Mode: store_sqlite.OwnershipReplace}}))
	require.NoError(t, store.PublishPayloadGeneration(t.Context(), upper.ViewGeneration(), 3))
	layer, err := graphview.NewGenerationLayer(upper)
	require.NoError(t, err)
	selected := graph.NewOverlaidViewWithLayer(base, layer)
	core := &contractCapturedCore{Reader: store, readers: map[string]graph.Reader{"fixture": selected}}
	rows, err := core.SemanticBindingTypes([]graph.SemanticBindingSite{changed, inherited, missing, changed})
	require.NoError(t, err, "actual selected composition must expose its physical compiler evidence")
	require.Equal(t, map[graph.SemanticBindingSite]string{changed: "SelectedResponse", inherited: "SelectedParent"}, rows)
	worker := &Indexer{graph: graph.New(), contractAnalysisOnly: true, contractSemanticReader: core}
	require.Equal(t, rows, worker.readSemanticBindingTypes([]graph.SemanticBindingSite{changed, inherited, missing}))
	require.NoError(t, worker.contractInputError(), "missing selected bindings must not consult mutable latest or the global compiler")
}

func TestContractSelectedSemanticFileOwnershipAndAcceptedAbsence(t *testing.T) {
	for _, mode := range []string{"replace", "empty", "delete", "context", "unclaimed", "missing_inventory"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
			_, store := newSQLiteIndexer(t)
			site := graph.SemanticBindingSite{RepoPrefix: "fixture", FilePath: "fixture/handler.go", Line: 4, Name: "response"}
			require.NoError(t, store.ReplaceSemanticBindingTypes("fixture", []graph.SemanticBindingType{{Site: site, TypeName: "WrongMutableLatest"}}))
			base := selectedSemanticGeneration(t, store, "dedicated", "ownership-base", 0)
			selectedSemanticFile(t, base, site.FilePath, "parent")
			require.NoError(t, base.ReplaceSemanticBindingTypes("fixture", []graph.SemanticBindingType{{Site: site, TypeName: "SelectedParent"}}))
			require.NoError(t, store.PublishPayloadGeneration(t.Context(), base.ViewGeneration(), 2))
			upper := selectedSemanticGeneration(t, store, "dirty", "ownership-upper", base.ViewGeneration())
			mask := store_sqlite.FileMask{FilePath: site.FilePath, RepoPrefix: "fixture", Mode: store_sqlite.OwnershipReplace}
			switch mode {
			case "replace", "empty":
				selectedSemanticFile(t, upper, site.FilePath, "current")
				if mode == "replace" {
					require.NoError(t, upper.ReplaceSemanticBindingTypes("fixture", []graph.SemanticBindingType{{Site: site, TypeName: "Current"}}))
				}
			case "delete":
				mask.Mode = store_sqlite.OwnershipDelete
			case "context":
				mask.Mode = store_sqlite.OwnershipContext
			case "unclaimed":
				mask.FilePath = "fixture/other.go"
				selectedSemanticFile(t, upper, mask.FilePath, "other")
			case "missing_inventory":
				require.NoError(t, upper.AddBatchChecked([]*graph.Node{{ID: site.FilePath, FilePath: site.FilePath, RepoPrefix: "fixture", Kind: graph.KindFile}}, nil))
			}
			require.NoError(t, upper.SetFileMasks([]store_sqlite.FileMask{mask}))
			require.NoError(t, store.PublishPayloadGeneration(t.Context(), upper.ViewGeneration(), 3))
			layer, err := graphview.NewGenerationLayer(upper)
			require.NoError(t, err)
			core := &contractCapturedCore{Reader: store, readers: map[string]graph.Reader{"fixture": graph.NewOverlaidViewWithLayer(base, layer)}}
			rows, err := core.SemanticBindingTypes([]graph.SemanticBindingSite{site})
			if mode == "missing_inventory" {
				require.ErrorIs(t, err, graph.ErrSemanticBindingProjectionIncomplete)
				require.Nil(t, rows)
				return
			}
			require.NoError(t, err)
			switch mode {
			case "replace":
				require.Equal(t, "Current", rows[site])
			case "context", "unclaimed":
				require.Equal(t, "SelectedParent", rows[site])
			default:
				require.Empty(t, rows, "accepted empty/deleted file cannot inherit stale compiler bindings")
			}
		})
	}
}

func TestContractSelectedSemanticTombstoneChecksRevisionAndHandleLifetime(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	_, store := newSQLiteIndexer(t)
	site := graph.SemanticBindingSite{RepoPrefix: "fixture", FilePath: "fixture/deleted.go", Line: 2, Name: "response"}
	base := selectedSemanticGeneration(t, store, "dedicated", "revision-base", 0)
	selectedSemanticFile(t, base, site.FilePath, "parent")
	require.NoError(t, base.ReplaceSemanticBindingTypes("fixture", []graph.SemanticBindingType{{Site: site, TypeName: "StaleParent"}}))
	require.NoError(t, store.PublishPayloadGeneration(t.Context(), base.ViewGeneration(), 2))
	upper := selectedSemanticGeneration(t, store, "dirty", "revision-upper", base.ViewGeneration())
	other := "fixture/other.go"
	selectedSemanticFile(t, upper, other, "accepted-other")
	constant := &graph.Node{ID: other + "::C", Kind: graph.KindConstant, FilePath: other, RepoPrefix: "fixture", Name: "C"}
	require.NoError(t, upper.AddBatchChecked([]*graph.Node{constant}, nil))
	require.NoError(t, upper.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: site.FilePath, RepoPrefix: "fixture", Mode: store_sqlite.OwnershipDelete},
		{FilePath: other, RepoPrefix: "fixture", Mode: store_sqlite.OwnershipReplace},
	}))
	require.NoError(t, store.PublishPayloadGeneration(t.Context(), upper.ViewGeneration(), 3))
	layer, err := graphview.NewGenerationLayer(upper)
	require.NoError(t, err)
	view := graph.NewOverlaidViewWithLayer(base, layer)
	reader, ok := any(view).(graph.SemanticBindingTypeReader)
	require.True(t, ok)
	rows, err := reader.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.NoError(t, err)
	require.Empty(t, rows)
	correction, err := store.BeginDerivedCorrection(t.Context(), store_sqlite.DerivedCorrectionRequest{
		GenerationID: upper.ViewGeneration(), Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField},
	})
	require.NoError(t, err)
	updated := &graph.Node{ID: other, Kind: graph.KindFile, FilePath: other, RepoPrefix: "fixture", Meta: map[string]any{"source_derived_decl_fingerprint": "changed"}}
	require.NoError(t, correction.ReplaceSourceEdges(t.Context(), []string{constant.ID}, nil, []*graph.Node{updated}))
	require.Zero(t, store.GenerationCorrectionEpoch(upper.ViewGeneration()), "committed correction chunk precedes Finish")
	rows, err = reader.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale, "even an empty tombstone result must validate captured ownership")
	require.Nil(t, rows)
	_, err = correction.Finish(t.Context())
	require.NoError(t, err)
	fresh, err := graphview.NewGenerationLayer(upper)
	require.NoError(t, err)
	current := graph.NewOverlaidViewWithLayer(base, fresh)
	require.NoError(t, store.Close())
	rows, err = any(current).(graph.SemanticBindingTypeReader).SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.Error(t, err)
	require.Nil(t, rows)
	rows, err = any(current).(graph.SemanticBindingTypeReader).SemanticBindingTypes(nil)
	require.NoError(t, err, "zero sites do not perform a storage read")
	require.Empty(t, rows)
}
