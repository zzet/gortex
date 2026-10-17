package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func TestContractFollowupDuplicateReportsActualWinnerAndUnusedReservation(t *testing.T) {
	f := newContractWorkerFixture(t)
	first := f.request(t, "repo-a", "atomic-winner")
	duplicate := f.request(t, "repo-a", "atomic-loser")
	report, err := RunContractFollowup(t.Context(), first)
	require.NoError(t, err)
	require.True(t, report.Published)
	report, err = RunContractFollowup(t.Context(), duplicate)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, first.Payload.ViewGeneration(), report.PayloadGeneration)
	require.Equal(t, duplicate.Payload.ViewGeneration(), report.UnusedPayloadGeneration)
	header, err := f.store.GetContractAttachmentContext(t.Context(), first.Snapshot.Key)
	require.NoError(t, err)
	require.Equal(t, first.Payload.ViewGeneration(), header.PayloadGeneration)
	require.NoError(t, f.store.RetirePayloadGeneration(t.Context(), report.UnusedPayloadGeneration, f.leases.InUse))
	_, found, err := f.store.Catalog().GetViewGeneration(t.Context(), duplicate.Payload.ViewGeneration())
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = f.store.Catalog().GetViewGeneration(t.Context(), first.Payload.ViewGeneration())
	require.NoError(t, err)
	require.True(t, found, "retirement custody never follows the losing reservation as winner")
}

func TestContractCoordinatorRetiresDuplicateLoserAndKeepsWinner(t *testing.T) {
	f := newContractWorkerFixture(t)
	a := f.request(t, "repo-a", "coordinator-race-source-a")
	b := f.request(t, "repo-b", "coordinator-race-source-b")
	winner := f.request(t, "repo-a", "coordinator-race-winner")
	snapshot := a.Snapshot
	raced := false
	snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
		if !raced {
			raced = true
			report, err := RunContractFollowup(ctx, winner)
			require.NoError(t, err)
			require.True(t, report.Published)
		}
		return ContractAcceptedSource{Bytes: append([]byte(nil), f.sources[file.Path]...), SourceFingerprint: file.SourceFingerprint, Policy: file.Policy}, nil
	}
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		panic("unused")
	}, Yield: func(ctx context.Context) error { return ctx.Err() }})
	require.NoError(t, err)
	defer func() { require.NoError(t, c.Close()) }()
	job := &contractAnalysisJob{id: "atomic-duplicate", snapshot: snapshot, targets: []ContractFollowupTarget{{Key: a.Snapshot.Key, Work: a.Snapshot.Work, Catalog: f.store}, {Key: b.Snapshot.Key, Work: b.Snapshot.Work, Catalog: f.store}}, queuedAt: time.Now()}
	outcome := c.execute(job)
	require.Nil(t, job.publication, "an older race winner has no newly certified historical receipt stamps")
	require.Equal(t, viewmetrics.ContractJobPublished, outcome)
	require.True(t, raced)
	header, err := f.store.GetContractAttachmentContext(t.Context(), a.Snapshot.Key)
	require.NoError(t, err)
	require.Equal(t, winner.Payload.ViewGeneration(), header.PayloadGeneration)
	require.NoError(t, f.store.RetirePayloadGeneration(t.Context(), a.Payload.ViewGeneration(), f.leases.InUse))
	require.NoError(t, f.store.RetirePayloadGeneration(t.Context(), b.Payload.ViewGeneration(), f.leases.InUse))
	generations, err := f.store.Catalog().ListViewGenerations(t.Context(), store_sqlite.ViewGenerationFilter{OwnerKind: "dedicated_graph"})
	require.NoError(t, err)
	for _, generation := range generations {
		require.NotEqual(t, store_sqlite.ViewGenerationBuilding, generation.State, "the duplicate reservation must retire after the worker releases its lease")
	}
}
