package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type observedReparseWindow struct {
	*store_sqlite.Store
	ordinary, reparses int
	afterOpen          func()
}

func (p *observedReparseWindow) BeginGenerationBulkLoad(id int64) (bool, error) {
	p.ordinary++
	return p.Store.BeginGenerationBulkLoad(id)
}

func (p *observedReparseWindow) BeginManagedDedicatedReparseBulkLoad(ctx context.Context, id int64) (bool, error) {
	p.reparses++
	opened, err := p.Store.BeginManagedDedicatedReparseBulkLoad(ctx, id)
	if opened && err == nil && p.afterOpen != nil {
		p.afterOpen()
	}
	return opened, err
}

func TestClaimedPopulatedReparseUsesManagedWindowAndClosesOnCancel(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "publish", true: "cancel_after_open"}[stop], func(t *testing.T) {
			builder, request, claim, logs := claimedCopyFixture(t)
			indexGenerationZero(t, builder, request)
			baseNodes, baseEdges := claimedBasePayload(builder.Store)
			other := builder.Store.AtGeneration(91)
			other.AddNode(&graph.Node{ID: "other::Stable", Name: "Stable", Kind: graph.KindFunction, RepoPrefix: "other"})
			otherNodes, otherEdges := claimedBasePayload(other)
			handle, err := builder.Store.AtManagedGeneration(claim.GenerationID)
			require.NoError(t, err)
			residual := &graph.Node{ID: "dep::example/dep::Call", Name: "Call", Kind: graph.KindFunction, Language: "go"}
			obsolete := &graph.Node{ID: request.RepoPrefix + "::Obsolete", Name: "Obsolete", Kind: graph.KindFunction, RepoPrefix: request.RepoPrefix, FilePath: request.RepoPrefix + "/obsolete.go"}
			require.NoError(t, handle.AddBatchChecked([]*graph.Node{residual, obsolete}, nil))
			residual = handle.GetNode(residual.ID)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			probe := &observedReparseWindow{Store: handle}
			probe.afterOpen = func() {
				cache, auto, active := handle.GenerationBulkLoadShape()
				require.True(t, active)
				require.Equal(t, int64(-262144), cache)
				require.Zero(t, auto)
				require.NotNil(t, handle.GetNode(residual.ID), "the window must accept realistic unowned residue")
				if stop {
					cancel()
				}
			}
			id, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
				Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID, BulkLoad: probe,
				PrePublish: func(context.Context, int64) error {
					_, active := handle.InGenerationBulkLoad()
					require.True(t, active)
					return nil
				},
			})
			require.Equal(t, 0, probe.ordinary)
			require.Equal(t, 1, probe.reparses)
			_, active := handle.InGenerationBulkLoad()
			require.False(t, active)
			if stop {
				require.ErrorIs(t, err, context.Canceled)
				row, found, readErr := builder.Store.Catalog().GetViewGeneration(t.Context(), claim.GenerationID)
				require.NoError(t, readErr)
				require.True(t, found)
				require.Equal(t, store_sqlite.ViewGenerationBuilding, row.State)
			} else {
				require.NoError(t, err)
				require.Equal(t, claim.GenerationID, id)
				require.Nil(t, handle.GetNode(obsolete.ID))
				require.Equal(t, residual, handle.GetNode(residual.ID))
				reference, refRequest, refClaim, _ := claimedCopyFixture(t)
				indexGenerationZero(t, reference, refRequest)
				refHandle, refErr := reference.Store.AtManagedGeneration(refClaim.GenerationID)
				require.NoError(t, refErr)
				require.NoError(t, refHandle.AddBatchChecked([]*graph.Node{residual, obsolete}, nil))
				buildClaimedBase(t, reference, ClaimedDedicatedBaseRequest{Claim: refClaim, RootPath: refRequest.RootPath, WorkspaceID: refRequest.WorkspaceID, ProjectID: refRequest.ProjectID, BulkLoad: &bulkLoadProbe{}})
				wantNodes, wantEdges := claimedBasePayload(refHandle)
				gotNodes, gotEdges := claimedBasePayload(handle)
				require.Equal(t, wantNodes, gotNodes)
				require.Equal(t, wantEdges, gotEdges)
			}
			gotNodes, gotEdges := claimedBasePayload(builder.Store)
			require.Equal(t, baseNodes, gotNodes)
			require.Equal(t, baseEdges, gotEdges)
			gotNodes, gotEdges = claimedBasePayload(other)
			require.Equal(t, otherNodes, gotNodes)
			require.Equal(t, otherEdges, gotEdges)
			route, _ := claimedRoute(t, logs)
			require.Equal(t, "reparse_git_tree", route)
		})
	}
}

func TestManagedReparseAdmissionErrorsAbortPreparation(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, store_sqlite.ErrDedicatedBaseCandidate, store_sqlite.ErrPayloadGenerationSealed, store_sqlite.ErrGenerationBulkCheckpointBusy} {
		loader := &reparseRefusalLoader{cause: cause}
		window := &generationBulkWindow{loader: loader, reparseLoader: loader, generationID: 1}
		err := window.openContext(t.Context())
		require.True(t, errors.Is(err, cause))
		require.False(t, window.opened)
		require.Zero(t, loader.ordinary)
	}
}

type reparseRefusalLoader struct {
	bulkLoadProbe
	cause    error
	ordinary int
}

func (p *reparseRefusalLoader) BeginGenerationBulkLoad(int64) (bool, error) {
	p.ordinary++
	return true, nil
}
func (p *reparseRefusalLoader) BeginManagedDedicatedReparseBulkLoad(context.Context, int64) (bool, error) {
	return false, p.cause
}
