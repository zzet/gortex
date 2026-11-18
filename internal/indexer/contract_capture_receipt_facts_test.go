package indexer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestContractCaptureReceiptFactsKeepOpaqueAndFailureDistinct(t *testing.T) {
	f := newRouteConstantFixture(t)
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	options := f.options
	options.selectedInputs = inputs
	row := opaqueTestReceipt(t, jobRoutes, map[string]string{strings.Repeat("k", contractCoreReceiptKeyBytes+1): "value"})
	files := map[string]ContractFollowupFile{row.FilePath: {RepoPrefix: "fixture", Path: row.FilePath, Policy: "current-policy"}}
	handles := map[string][]*store_sqlite.Store{"fixture": {f.store}}
	persist := func(row graph.ContractBoundaryReceipt) {
		row.Accepted = false
		require.NoError(t, f.store.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
		require.NoError(t, f.store.AcceptContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
	}
	persist(row)
	facts, err := contractCaptureReceiptFacts(t.Context(), options, handles, files)
	require.NoError(t, err)
	require.Equal(t, contractCaptureReceiptFact{source: row.SourceFingerprint, opaque: true}, facts[row.FilePath])
	row.Scope.Causes = []string{contractFileIndexFailed}
	row.Fingerprint = "failed-envelope"
	persist(row)
	facts, err = contractCaptureReceiptFacts(t.Context(), options, handles, files)
	require.NoError(t, err)
	require.Equal(t, contractCaptureReceiptFact{excluded: true}, facts[row.FilePath])
}

func TestContractCaptureReceiptFactsKeepStableRowsAndPendingFallback(t *testing.T) {
	f := newRouteConstantFixture(t)
	ctx := t.Context()
	inputs, err := f.options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	snapshot, _, err := NewContractFollowupCapture(f.options)(ctx, nil, inputs, "fixture", "")
	require.NoError(t, err)
	defer snapshot.Release()
	files := map[string]ContractFollowupFile{}
	for _, file := range snapshot.Files {
		files[file.Path] = file
	}
	options := f.options
	options.selectedInputs = inputs
	handles := map[string][]*store_sqlite.Store{"fixture": {f.store}}
	facts, err := contractCaptureReceiptFacts(ctx, options, handles, files)
	require.NoError(t, err)
	require.Len(t, facts, len(files))
	for path, file := range files {
		require.Equal(t, file.SourceFingerprint, facts[path].source)
	}
	const pending = "fixture/consts.go"
	f.stageInFlight(pending)
	facts, err = contractCaptureReceiptFacts(ctx, options, handles, files)
	require.NoError(t, err)
	require.NotContains(t, facts, pending, "pending rows must run the original write-window admission")
	for path := range files {
		if path != pending {
			require.Contains(t, facts, path)
		}
	}
	file := files["fixture/routes.go"]
	file.Policy = "different-policy"
	files[file.Path] = file
	facts, err = contractCaptureReceiptFacts(ctx, options, handles, files)
	require.NoError(t, err)
	require.NotContains(t, facts, file.Path, "policy conversion still needs original selected-file proof")
}

func TestContractCaptureReceiptFactsDoNotReplaceANewerPendingOwner(t *testing.T) {
	f := newRouteConstantFixture(t)
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	snapshot, _, err := NewContractFollowupCapture(f.options)(t.Context(), nil, inputs, "fixture", "")
	require.NoError(t, err)
	defer snapshot.Release()
	files := map[string]ContractFollowupFile{}
	for _, file := range snapshot.Files {
		files[file.Path] = file
	}
	generation, upper, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "capture-pending", CheckoutID: "linked", LayerID: "pending-upper", GenerationKind: "dirty", BaseGenerationID: 0, TreeOID: "tree-linked", CreatedAt: 1})
	require.NoError(t, err)
	row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/consts.go")
	require.NoError(t, err)
	require.NotNil(t, row)
	staged := *row
	staged.Accepted = false
	staged.CheckoutID = "linked"
	staged.Fingerprint = "newer-pending"
	staged.Previous = nil
	require.NoError(t, upper.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{staged}))
	options := f.options
	// This lower-level facts check uses real physical building rows. Outer capture
	// still independently requires accepted selected authority before using it.
	options.selectedInputs = &graphview.SelectedContractInputs{Witnesses: append(append([]graph.ContractInputWitness(nil), inputs.Witnesses...), graph.ContractInputWitness{GenerationID: generation, State: graph.ContractInputState{RepoPrefix: "fixture", CheckoutID: "linked"}})}
	facts, err := contractCaptureReceiptFacts(t.Context(), options, map[string][]*store_sqlite.Store{"fixture": {f.store, upper}}, files)
	require.NoError(t, err)
	require.NotContains(t, facts, staged.FilePath, "an older accepted base must not replace the selected pending owner")
	require.Contains(t, facts, "fixture/routes.go", "unmodified inherited owner remains captured")
}
