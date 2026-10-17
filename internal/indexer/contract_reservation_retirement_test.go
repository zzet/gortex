package indexer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
)

func TestContractFailedNonemptyReservationHandsOffDurableCleanup(t *testing.T) {
	f := newContractWorkerFixture(t)
	ctx := t.Context()
	req := f.request(t, "repo-a", "unused-handoff-fixture")
	require.NoError(t, f.store.RetirePayloadGeneration(ctx, req.Payload.ViewGeneration(), nil))
	snapshot := req.Snapshot
	var releaseOnce sync.Once
	snapshot.Release = func() { releaseOnce.Do(req.Snapshot.Release) }
	snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
		return ContractAcceptedSource{Bytes: append([]byte(nil), f.sources[file.Path]...), SourceFingerprint: file.SourceFingerprint, Policy: file.Policy}, ctx.Err()
	}
	lifecycle := newGenerationRetirementLifecycle(f.store, time.Now())
	lifecycle.leases = f.leases
	const jobID = "nonempty-reservation-handoff"
	var reserved int64
	moved := false
	yield := func(ctx context.Context) error {
		if reserved == 0 {
			rows, err := f.store.Catalog().ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{States: []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationBuilding}, Limit: 100})
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.LowerViewFingerprint == jobID {
					reserved = row.GenerationID
				}
			}
		}
		if reserved != 0 && !moved {
			rows, err := f.store.AtGeneration(reserved).LayerContractFileProjectionContext(ctx, "repo-a", []string{"repo-a/server.go"})
			if err != nil {
				return err
			}
			if len(rows.FileNodes["repo-a/server.go"]) != 0 {
				before := f.inputs[0].State
				after := before
				after.Accepted, after.InputFingerprint = false, "moved-after-payload-write"
				if err := f.store.BeginContractInputMutationContext(ctx, &before, after, nil); err != nil {
					return err
				}
				if err := f.store.AcceptContractInputMutationContext(ctx, after); err != nil {
					return err
				}
				moved = true
			}
		}
		return ctx.Err()
	}
	handed := 0
	wake := DeferredRetirementWake()
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Logger: zap.NewNop(), Yield: yield,
		Capture: func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
			t.Fatal("execute already owns its captured snapshot")
			return ContractFollowupSnapshot{}, nil, nil
		},
		RetireReservation: func(ctx context.Context, catalog *store_sqlite.Store, id int64, inUse func(int64) bool) error {
			handed++
			require.Equal(t, reserved, id)
			require.False(t, inUse(id), "the failed batch released its payload lease before cleanup")
			return lifecycle.DeferContractReservationRetirement(ctx, catalog, id, inUse)
		}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	outcome := c.execute(&contractAnalysisJob{id: jobID, snapshot: snapshot, targets: []ContractFollowupTarget{{Key: snapshot.Key, Work: snapshot.Work, Catalog: f.store}}, queuedAt: time.Now()})
	require.True(t, moved, "the real publication CAS must fail after actual payload rows were written")
	require.Equal(t, "superseded_publish_refused", outcome)
	require.Equal(t, 1, handed)
	require.Equal(t, 1, f.releases)
	row, found, err := f.store.Catalog().GetViewGeneration(ctx, reserved)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationRetiring, row.State)
	projection, err := f.store.AtGeneration(reserved).LayerContractFileProjectionContext(ctx, "repo-a", []string{"repo-a/server.go"})
	require.NoError(t, err)
	require.NotEmpty(t, projection.FileNodes["repo-a/server.go"], "handoff leaves physical rows for the existing worker")
	attached, err := f.store.GetContractAttachmentContext(ctx, snapshot.Key)
	require.NoError(t, err)
	require.Nil(t, attached)
	select {
	case <-wake:
	default:
		t.Fatal("durable debt did not wake the existing worker")
	}
	require.Contains(t, lifecycle.owed, reserved)
	// Lose every in-memory owed entry, as on reconstruction. Existing catalog
	// discovery must still collect this contract_analysis retiring row.
	rebuilt := newGenerationRetirementLifecycle(f.store, time.Now())
	rebuilt.leases = f.leases
	for attempt := 0; attempt < 64; attempt++ {
		_, _, err := rebuilt.SweepDeferredRetirements(ctx)
		require.NoError(t, err)
		_, found, err := f.store.Catalog().GetViewGeneration(ctx, reserved)
		require.NoError(t, err)
		if !found {
			require.NoError(t, c.Close())
			require.Zero(t, f.leases.Held())
			return
		}
	}
	t.Fatal("the existing reconstructed worker did not collect durable reservation debt")
}

func TestContractReservationHandoffRefusesForeignStoreAndLiveLease(t *testing.T) {
	f := newContractWorkerFixture(t)
	other := newContractWorkerFixture(t)
	lifecycle := newGenerationRetirementLifecycle(f.store, time.Now())
	req := f.request(t, "repo-a", "leased-reservation")
	id := req.Payload.ViewGeneration()
	foreign := other.request(t, "repo-a", "foreign-reservation")
	require.Equal(t, id, foreign.Payload.ViewGeneration(), "the wrong store can contain the same numeric generation")
	require.ErrorIs(t, lifecycle.DeferContractReservationRetirement(t.Context(), other.store, id, nil), store_sqlite.ErrCatalogInvalidValue)
	require.Empty(t, lifecycle.owed)
	foreignRow, foreignFound, foreignErr := other.store.Catalog().GetViewGeneration(t.Context(), id)
	require.NoError(t, foreignErr)
	require.True(t, foreignFound)
	require.Equal(t, store_sqlite.ViewGenerationBuilding, foreignRow.State)
	lease := lifecycle.leases.Acquire(id)
	require.ErrorIs(t, lifecycle.DeferContractReservationRetirement(t.Context(), f.store, id, nil), store_sqlite.ErrPayloadGenerationInUse)
	row, found, err := f.store.Catalog().GetViewGeneration(t.Context(), id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationBuilding, row.State)
	require.Empty(t, lifecycle.owed)
	lease.Release()
	require.NoError(t, lifecycle.DeferContractReservationRetirement(t.Context(), f.store, id, nil))
	require.Contains(t, lifecycle.owed, id)
}
