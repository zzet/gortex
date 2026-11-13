package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func TestContractCoordinatorLazyFillResumesToPublication(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}
	for i := 0; i < 65; i++ {
		files[fmt.Sprintf("helper%02d.go", i)] = fmt.Sprintf("package fixture\nfunc Helper%d() int { return %d }\n", i, i)
	}
	f := newContractJobFixture(t, files)
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
	var cache *contractDeltaCache
	var published []int64
	completed := false
	for segment := 0; segment < 4; segment++ {
		before := 0
		if cache != nil {
			before = len(cache.units)
		}
		job := captureContractCoordinatorJob(t, f, c, cache)
		require.Len(t, job.snapshot.Files, len(files), "each continuation captures the complete current census")
		job.otherActorWaiting = func() bool { return true }
		job.targetPublished = func(result ContractFollowupTargetResult) {
			generation := result.Report.PayloadGeneration
			require.True(t, c.options.Leases.InUse(generation))
			published = append(published, generation)
		}
		outcome := c.execute(job)
		cache = job.delta.cache
		require.NotNil(t, cache)
		require.Empty(t, retired, "partial fills must not leave unused reservations")
		if outcome == viewmetrics.ContractJobPublished {
			require.Greater(t, segment, 0, "this fixture must actually resume a partial fill")
			require.True(t, contractPublishedCohortComplete(job.publication))
			completed = true
			break
		}
		require.Equal(t, viewmetrics.ContractJobFillContinued, outcome)
		require.NotNil(t, cache.continuation)
		require.Greater(t, len(cache.units), before)
		require.LessOrEqual(t, len(cache.units)-before, contractRoundFiles)
		require.Empty(t, published, "incomplete coverage cannot publish")
	}
	require.True(t, completed, "a finite unchanged census must complete across fair slices")
	require.Len(t, published, 1)
	require.False(t, c.options.Leases.InUse(published[0]))
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}

func TestContractCoordinatorLazyCancellationDoesNotReserve(t *testing.T) {
	f := newRouteConstantFixture(t)
	retired := 0
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{
		Store: f.store, Leases: f.options.Materializer.Leases, Registry: f.options.Registry,
		Config: f.options.Config, Capture: NewContractFollowupSourceCapture(f.options), Yield: f.options.Yield,
		RetireReservation: func(ctx context.Context, catalog *store_sqlite.Store, generation int64, inUse func(int64) bool) error {
			retired++
			return catalog.RetirePayloadGeneration(ctx, generation, inUse)
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	job := captureContractCoordinatorJob(t, f, c, nil)
	job.yield = func(ctx context.Context) error {
		c.Cancel()
		return ctx.Err()
	}
	require.Equal(t, viewmetrics.ContractJobPreempted, c.execute(job))
	require.Zero(t, retired)
	require.Nil(t, job.publication)
	attachment, err := f.store.GetContractAttachmentContext(t.Context(), job.targets[0].Key)
	require.NoError(t, err)
	require.Nil(t, attachment)
	require.NoError(t, c.Close())
	require.Zero(t, c.options.Leases.Held())
}

func TestContractLazyReservationRefusalAndCancellation(t *testing.T) {
	for _, mode := range []string{"refused", "canceled_after_allocation", "nil_payload", "base_payload", "mixed_ownership", "single_ownership"} {
		t.Run(mode, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			// Finish isolated output setup before capturing selected input authority.
			_, payload, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{
				OwnerKind: "dedicated_graph", GraphID: "lazy-refusal", LayerID: mode, GenerationKind: "contract_analysis", CreatedAt: 1,
			})
			require.NoError(t, err)
			empty, err := payload.StatsContext(t.Context())
			require.NoError(t, err)
			require.Zero(t, empty.TotalNodes)
			require.Zero(t, empty.TotalEdges)
			inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
			require.NoError(t, err)
			snapshot, targets, err := NewContractFollowupCapture(f.options)(t.Context(), nil, inputs, "fixture", "")
			require.NoError(t, err)
			require.Len(t, targets, 1)
			snapshot.Key, snapshot.Work = targets[0].Key, targets[0].Work
			releaseSource := snapshot.Release
			var releaseOnce sync.Once
			released := 0
			snapshot.Release = func() {
				releaseOnce.Do(func() {
					released++
					releaseSource()
				})
			}
			t.Cleanup(snapshot.Release)
			r := ContractFollowupRequest{
				Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, RebuildReason: ContractFollowupUnknownInterval,
				Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: snapshot.RepoConfigs,
				Logger: f.options.Logger, Leases: f.options.Materializer.Leases, Yield: f.options.Yield,
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			refused := errors.New("reservation refused")
			calls := 0
			req := ContractFollowupBatchRequest{
				Snapshot: r.Snapshot, Targets: []ContractFollowupTarget{{Key: r.Snapshot.Key, Work: r.Snapshot.Work, Catalog: r.Catalog}},
				RebuildReason: r.RebuildReason, Registry: r.Registry, Config: r.Config, RepoConfigs: r.RepoConfigs,
				Logger: r.Logger, Leases: r.Leases, Yield: r.Yield,
				reservePayload: func(context.Context, ContractFollowupTarget) (*store_sqlite.Store, error) {
					calls++
					switch mode {
					case "refused":
						return nil, refused
					case "canceled_after_allocation":
						cancel()
						return r.Payload, nil
					case "nil_payload":
						return nil, nil
					case "base_payload":
						return f.store.AtGeneration(0), nil
					default:
						t.Fatal("mixed ownership must be refused before allocating")
						return nil, nil
					}
				},
			}
			if mode == "mixed_ownership" {
				req.Targets[0].Payload = r.Payload
			}
			var report ContractFollowupBatchReport
			if mode == "single_ownership" {
				r.reservePayload = func(context.Context) (*store_sqlite.Store, error) {
					calls++
					return r.Payload, nil
				}
				_, err = RunContractFollowup(ctx, r)
			} else {
				report, err = RunContractFollowupBatch(ctx, req)
			}
			require.Error(t, err)
			switch mode {
			case "refused":
				require.ErrorIs(t, err, refused)
			case "canceled_after_allocation":
				require.ErrorIs(t, err, context.Canceled)
			}
			if mode == "mixed_ownership" || mode == "single_ownership" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
				require.Positive(t, report.Cohort.SourceReads, "reservation follows real converged source reads")
			}
			require.Nil(t, report.Publication)
			stats, statsErr := r.Payload.StatsContext(t.Context())
			require.NoError(t, statsErr)
			require.Zero(t, stats.TotalNodes, "refused or canceled allocation must precede payload writes")
			require.Zero(t, stats.TotalEdges)
			require.Zero(t, f.options.Materializer.Leases.Held())
			require.Equal(t, 1, released)
			attachment, err := f.store.GetContractAttachmentContext(t.Context(), r.Snapshot.Key)
			require.NoError(t, err)
			require.Nil(t, attachment)
		})
	}
}
