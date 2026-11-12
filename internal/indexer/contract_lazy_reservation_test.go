package indexer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// Exercise the installed capture and coordinator body, including real catalog
// cleanup. A continued fill has no payload to publish and must not create a
// generation merely to retire it when yielding the slot.
func TestContractCoordinatorFillContinuationDoesNotReservePayload(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}
	for i := 0; i < 33; i++ {
		files[fmt.Sprintf("helper%02d.go", i)] = fmt.Sprintf("package fixture\nfunc Helper%d() int { return %d }\n", i, i)
	}
	f := newContractJobFixture(t, files)
	var retired []store_sqlite.ViewGeneration
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{
		Store: f.store, Leases: f.options.Materializer.Leases, Registry: f.options.Registry,
		Config: f.options.Config, Capture: NewContractFollowupSourceCapture(f.options), Yield: f.options.Yield,
		RetireReservation: func(ctx context.Context, catalog *store_sqlite.Store, generation int64, inUse func(int64) bool) error {
			row, found, err := catalog.Catalog().GetViewGeneration(ctx, generation)
			require.NoError(t, err)
			require.True(t, found, "retirement must refer to an actual catalog generation")
			require.Equal(t, "contract_analysis", row.GenerationKind)
			require.False(t, inUse(generation), "batch leases drain before failed reservation cleanup")
			retired = append(retired, row)
			return catalog.RetirePayloadGeneration(ctx, generation, inUse)
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	job := captureContractCoordinatorJob(t, f, c, nil)
	job.otherActorWaiting = func() bool { return true }
	require.Len(t, job.snapshot.Files, len(files))
	require.Equal(t, viewmetrics.ContractJobFillContinued, c.execute(job))
	require.NotNil(t, job.delta.cache)
	require.NotNil(t, job.delta.cache.continuation, "yield must retain a resumable frontier")
	require.NotEmpty(t, job.delta.cache.units, "yield must advance the extraction frontier")
	require.LessOrEqual(t, len(job.delta.cache.units), 16, "a contended fill keeps its fair quantum")
	attachment, err := f.store.GetContractAttachmentContext(t.Context(), job.targets[0].Key)
	require.NoError(t, err)
	require.Nil(t, attachment, "incomplete coverage cannot publish")
	require.Empty(t, retired, "continued fill must not create a payload reservation")
}

func TestContractCoordinatorCompletedFillKeepsPublishedPayload(t *testing.T) {
	f := newRouteConstantFixture(t)
	var retired []int64
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{
		Store: f.store, Leases: f.options.Materializer.Leases, Registry: f.options.Registry,
		Config: f.options.Config, Capture: NewContractFollowupSourceCapture(f.options), Yield: f.options.Yield,
		RetireReservation: func(ctx context.Context, catalog *store_sqlite.Store, generation int64, inUse func(int64) bool) error {
			retired = append(retired, generation)
			return catalog.RetirePayloadGeneration(ctx, generation, inUse)
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	job := captureContractCoordinatorJob(t, f, c, nil)
	var published []int64
	job.targetPublished = func(result ContractFollowupTargetResult) {
		generation := result.Report.PayloadGeneration
		require.Positive(t, generation)
		require.True(t, c.options.Leases.InUse(generation), "payload lease covers publication callbacks")
		published = append(published, generation)
	}
	require.Equal(t, viewmetrics.ContractJobPublished, c.execute(job))
	require.True(t, contractPublishedCohortComplete(job.publication))
	require.Len(t, published, 1, "one target publishes once")
	require.False(t, c.options.Leases.InUse(published[0]), "successful batch releases its payload lease")
	attachment, err := f.store.GetContractAttachmentContext(t.Context(), job.targets[0].Key)
	require.NoError(t, err)
	require.NotNil(t, attachment)
	require.Empty(t, retired, "a committed winner keeps its payload")
	// The public fixture lookup independently captures and opens current inputs.
	// It also verifies the winner's payload after coordinator cleanup.
	require.NotEmpty(t, f.contracts())
}

// Source capture owns its checked readers until execute consumes the snapshot.
// The fixture's ordinary eager worker helper is deliberately not used here.
func captureContractCoordinatorJob(t *testing.T, f *contractJobFixture, c *ContractAnalysisCoordinator, cache *contractDeltaCache) *contractAnalysisJob {
	t.Helper()
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	snapshot, targets, err := c.options.Capture(t.Context(), nil, inputs, "fixture", "")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	t.Cleanup(snapshot.Release)
	snapshot.Key, snapshot.Work = targets[0].Key, targets[0].Work
	delta, err := newContractDeltaSession(cache, snapshot, f.options.Config)
	require.NoError(t, err)
	id, err := contractAnalysisCohortID(snapshot, f.options.Config)
	require.NoError(t, err)
	return &contractAnalysisJob{id: id, snapshot: snapshot, targets: targets, delta: delta, queuedAt: time.Now()}
}
