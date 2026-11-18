package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A stop during the initial full parse leaves an accepted input header but no
// complete repo-index state. The next process must reparse the same reservation,
// not install an empty header over it or treat its partial receipts as a census.
func TestClaimedDedicatedBaseReparsesInterruptedContractNamespace(t *testing.T) {
	builder, request, claim, _ := claimedCopyFixture(t)
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	ctx := context.Background()
	stopped, stop := context.WithCancel(ctx)
	defer stop()
	fault := func(string) error {
		stop()
		return context.Canceled
	}
	contractCoreBeginFault.Store(&fault)
	t.Cleanup(func() { contractCoreBeginFault.Store(nil) })
	_, _, err := builder.BuildClaimedDedicatedBase(stopped, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID,
	})
	contractCoreBeginFault.Store(nil)
	require.ErrorIs(t, err, context.Canceled)
	target := builder.Store.AtGeneration(claim.GenerationID)
	old, found, err := target.ContractInputStateContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, old.Accepted)
	require.NotEqual(t, contractInputHash([]byte(contractCoreInputVersion+":empty-positive:"+request.RepoPrefix)), old.InputFingerprint)
	row, found, err := builder.Store.Catalog().GetViewGeneration(ctx, claim.GenerationID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationBuilding, row.State)
	resumable, _ := claimedBaseResumable(target, request.RepoPrefix, row)
	require.False(t, resumable, "this is a partial parse, not the complete-payload resume path")
	debt, err := target.ContractWorkContext(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, debt)

	// Interrupted bounded chunks may already have accepted receipts. Include
	// an old absence baseline and an orphaned receipt to ensure the full pass
	// rebuilds the namespace rather than merely overwriting its fingerprint.
	var partial []graph.ContractBoundaryReceipt
	for _, file := range []string{"base.go", "departed.go"} {
		stored, err := contractCoreStoredReceipt(request.RepoPrefix, request.Identity.CheckoutID,
			retryReceiptChange(request.RepoPrefix+"/"+file, "interrupted", nil))
		require.NoError(t, err)
		stored.Accepted = true
		partial = append(partial, stored)
	}
	require.NoError(t, target.SetContractBoundaryReceiptsWithSourcesContext(ctx, partial, nil))
	require.NoError(t, target.SetContractBoundaryReceiptBaselineContext(ctx, graph.ContractBoundaryReceiptBaseline{
		RepoPrefix: request.RepoPrefix, CheckoutID: request.Identity.CheckoutID,
		Version: contractBoundaryReceiptVersion, Fingerprint: "old-partial-census",
	}))
	publication, found, err := builder.Store.Catalog().DedicatedBasePublication(ctx, request.Identity.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "building", publication.AttemptState)
	require.Equal(t, claim.GenerationID, publication.Claim.GenerationID)
	id := buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: publication.Claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID,
	})
	require.Equal(t, claim.GenerationID, id)
	current, _, err := target.ContractInputStateContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, current.Accepted)
	require.NotEqual(t, old.InputFingerprint, current.InputFingerprint)
	baseline, err := target.ContractBoundaryReceiptBaselineContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.NotNil(t, baseline)
	require.Equal(t, current.InputFingerprint, baseline.Fingerprint)
	refreshed, known, err := target.ContractBoundaryReceiptContext(ctx, request.RepoPrefix, request.Identity.CheckoutID, partial[0].FilePath)
	require.NoError(t, err)
	require.True(t, known)
	require.NotNil(t, refreshed)
	require.True(t, refreshed.Accepted)
	require.NotEqual(t, partial[0].Fingerprint, refreshed.Fingerprint)
	removed, known, err := target.ContractBoundaryReceiptContext(ctx, request.RepoPrefix, request.Identity.CheckoutID, partial[1].FilePath)
	require.NoError(t, err)
	require.True(t, known, "absence is certified only by the newly completed census")
	require.Nil(t, removed)
	remaining, err := target.ContractWorkContext(ctx)
	require.NoError(t, err)
	for _, owed := range debt {
		require.Contains(t, remaining, owed, "restarting a census must not acknowledge old work")
	}

	reference, referenceRequest, referenceClaim, _ := claimedCopyFixture(t)
	reference.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	referenceID := buildClaimedBase(t, reference, ClaimedDedicatedBaseRequest{
		Claim: referenceClaim, RootPath: referenceRequest.RootPath,
		WorkspaceID: referenceRequest.WorkspaceID, ProjectID: referenceRequest.ProjectID,
	})
	gotNodes, gotEdges := claimedBasePayload(materializeClaimedBase(t, builder, request.Identity.GraphID, id).Reader)
	wantNodes, wantEdges := claimedBasePayload(materializeClaimedBase(t, reference, referenceRequest.Identity.GraphID, referenceID).Reader)
	require.NotEmpty(t, gotNodes)
	require.NotEmpty(t, gotEdges)
	require.Equal(t, wantNodes, gotNodes)
	require.Equal(t, wantEdges, gotEdges)
}

func TestContractCoreFullRootResumeRefusesChangedPhysicalInput(t *testing.T) {
	builder, request, claim, _ := claimedCopyFixture(t)
	ctx := context.Background()
	target := builder.Store.AtGeneration(claim.GenerationID)
	old := graph.ContractInputState{RepoPrefix: request.RepoPrefix, CheckoutID: request.Identity.CheckoutID,
		InputVersion: contractCoreInputVersion, InputFingerprint: "interrupted", Accepted: true}
	require.NoError(t, target.SetContractInputStateWithWorkContext(ctx, nil, old, nil))
	backend, err := selectedContractCoreStorageBackend(ctx, target, nil, old.RepoPrefix, old.CheckoutID, true)
	require.NoError(t, err)
	backend.fullNamespace = true
	changed := old
	changed.InputFingerprint = "newer-writer"
	require.NoError(t, target.SetContractInputStateWithWorkContext(ctx, &old, changed, nil))
	require.ErrorIs(t, backend.beginFullCoreNamespace(ctx), store_sqlite.ErrCatalogStaleGuard)
	actual, found, err := target.ContractInputStateContext(ctx, old.RepoPrefix, old.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, changed.InputFingerprint, actual.InputFingerprint)
}
