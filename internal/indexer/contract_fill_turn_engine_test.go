package indexer

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The demand signal and competitor predicate are controlled scheduling inputs;
// linked capture, convergence, coordinator execution and source custody are real.
// In particular, this does not replace the engine with a callback-only fake job.
func TestContractFillTurnEngineReleasesOnSecondChunkDemandOrCancel(t *testing.T) {
	for _, mode := range []string{"demand", "demand_and_competitor", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}
			for i := 0; i < 65; i++ {
				files[fmt.Sprintf("helper%03d.go", i)] = fmt.Sprintf("package fixture\nfunc Helper%d() int { return %d }\n", i, i)
			}
			f := newContractJobFixture(t, files)
			linked := make(map[string]string, len(files))
			for name, text := range files {
				linked[name] = text
			}
			linked["consts.go"] = "package fixture\nconst RoutePath = \"/linked-turn\"\n"
			w := newContractLinkedWorktree(t, f, linked, "consts.go")
			actor := NewContractActor(contractLinkedCheckout, []string{"fixture"})
			factory := NewContractLinkedInputCapture(f.options.Materializer)
			capture := NewContractFollowupSourceCapture(f.options)
			var acquisitions, sourceReleases, captures, snapshotReleases, retired atomic.Int64
			var clock atomic.Int64
			var calls atomic.Uint64
			var triggered, thirdChunk atomic.Bool
			var c *ContractAnalysisCoordinator
			now := func() time.Time { return time.Unix(1_000_000, 0).Add(time.Duration(clock.Load())) }
			ownedInputs := func(ctx context.Context, actor ContractActor, repo string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
				inputs, source, err := factory(ctx, actor, repo)
				if source != nil {
					acquisitions.Add(1)
					previous := source.onRelease
					source.onRelease = func() {
						if previous != nil {
							previous()
						}
						sourceReleases.Add(1)
					}
				}
				return inputs, source, err
			}
			options := ContractAnalysisCoordinatorOptions{
				Store: f.store, Leases: f.options.Materializer.Leases, Registry: f.options.Registry,
				Config: f.options.Config, Yield: f.options.Yield, LinkedInputs: ownedInputs,
				LinkedPolicy: ContractLinkedIdle, Now: now, ScratchParent: t.TempDir(),
				Demand: func() (bool, uint64) { return false, calls.Load() },
				RetireReservation: func(ctx context.Context, catalog *store_sqlite.Store, generation int64, inUse func(int64) bool) error {
					retired.Add(1)
					return catalog.RetirePayloadGeneration(ctx, generation, inUse)
				},
			}
			options.Capture = func(ctx context.Context, source *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
				snapshot, targets, err := capture(ctx, source, inputs, repo, checkout)
				if err != nil {
					return snapshot, targets, err
				}
				n := captures.Add(1)
				release := snapshot.Release
				snapshot.Release = func() { release(); snapshotReleases.Add(1) }
				if n == 1 {
					read := snapshot.ReadPinned
					if read == nil {
						t.Error("production linked capture must install the pinned-source reader")
						return snapshot, targets, fmt.Errorf("missing production pinned-source reader")
					}
					// Install only after capture preparation; the engine reads
					// these independent helpers during actual extraction.
					snapshot.ReadPinned = func(ctx context.Context, file ContractFollowupFile, fingerprint string) ([]byte, error) {
						data, err := read(ctx, file, fingerprint)
						if file.Path == "fixture/helper031.go" {
							thirdChunk.Store(true)
						}
						// consts.go followed by helper000..014 is the first
						// admitted16-file chunk; helper015 enters chunk two.
						if err == nil && file.Path == "fixture/helper015.go" && triggered.CompareAndSwap(false, true) {
							if mode == "cancel" {
								c.Cancel()
							} else {
								calls.Add(2)
							}
						}
						if err == nil {
							err = ctx.Err()
						}
						return data, err
					}
				}
				return snapshot, targets, nil
			}
			var err error
			c, err = NewContractAnalysisCoordinator(options)
			require.NoError(t, err)
			clock.Add(int64(c.options.LinkedIdleWindow))
			t.Cleanup(func() { require.NoError(t, c.Close()) })
			type result struct {
				outcome string
				units   int
				demand  bool
			}
			first := make(chan result, 1)
			var runs atomic.Int64
			c.run = func(job *contractAnalysisJob) string {
				n := runs.Add(1)
				if n == 1 {
					// Admit the existing16-file split without inventing
					// preemption inside an ordinary noncontended64-file round.
					job.otherActorWaiting = func() bool { return mode == "demand_and_competitor" || !triggered.Load() }
					c.mu.Lock()
					a := c.actors[actor]
					c.mu.Unlock()
					c.linkJob(a, job)
				}
				outcome := c.execute(job)
				if n == 1 {
					units := 0
					if job.delta.cache != nil {
						units = len(job.delta.cache.units)
					}
					first <- result{outcome: outcome, units: units, demand: job.demandStop.Load()}
				}
				return outcome
			}
			beforeHeld := f.options.Materializer.Leases.Held()
			inputs, source, err := ownedInputs(t.Context(), actor, "fixture")
			require.NoError(t, err)
			t.Cleanup(source.Release)
			scheduled, err := c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor,
				Key: graph.ContractAttachmentKey{RepoPrefix: "fixture", CheckoutID: contractLinkedCheckout}, Inputs: inputs, Source: source, Epoch: c.InputEpoch()})
			require.NoError(t, err)
			var observed result
			select {
			case observed = <-first:
			case <-time.After(10 * time.Second):
				t.Fatal("first real engine turn did not return")
			}
			require.True(t, triggered.Load(), "the actual engine entered the second16-file chunk: outcome=%s units=%d captures=%d", observed.outcome, observed.units, captures.Load())
			require.False(t, thirdChunk.Load(), "the first turn never starts the third16-file extraction chunk")
			if mode == "cancel" {
				require.Equal(t, viewmetrics.ContractJobPreempted, observed.outcome)
				require.NoError(t, c.Close())
				c.mu.Lock()
				cache := c.deltaCaches.get(actor)
				c.mu.Unlock()
				if cache != nil {
					require.Nil(t, cache.continuation, "cancellation cannot mint a continuation anchor")
				}
			} else {
				require.Equal(t, viewmetrics.ContractJobFillContinued, observed.outcome)
				require.Equal(t, 32, observed.units, "demand stops at chunk two instead of the64-path competitor threshold")
				require.Equal(t, mode == "demand", observed.demand, "preserve competitor-first stop accounting")
				require.Eventually(t, func() bool {
					c.mu.Lock()
					defer c.mu.Unlock()
					return c.actors[actor].held && sourceReleases.Load() == 1
				}, 10*time.Second, time.Millisecond)
			}
			require.EqualValues(t, 1, captures.Load())
			require.EqualValues(t, 1, snapshotReleases.Load())
			require.EqualValues(t, 1, sourceReleases.Load(), "the turn returns its consumed source before waiting")
			require.Equal(t, beforeHeld, f.options.Materializer.Leases.Held())
			entries, err := os.ReadDir(options.ScratchParent)
			require.NoError(t, err)
			require.Empty(t, entries)
			require.Zero(t, retired.Load(), "partial or canceled work creates no reservation to retire")
			if mode == "cancel" {
				return
			}
			clock.Add(int64(6 * time.Second))
			c.mu.Lock()
			ring := c.actors[actor].ring
			c.mu.Unlock()
			ring.signal()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			status, err := c.WaitActor(ctx, actor, scheduled.ArrivalSeq)
			require.NoError(t, err)
			require.Equal(t, ContractActorPublished, status.Result)
			require.NoError(t, c.Close())
			require.EqualValues(t, 2, captures.Load(), "resume takes a fresh production capture")
			require.Equal(t, acquisitions.Load(), sourceReleases.Load())
			require.Equal(t, captures.Load(), snapshotReleases.Load())
			view := w.view(t)
			require.Equal(t, contractMeta(coldContracts(t, linked)), contractMeta(w.contracts(t, view)))
			view.Close()
		})
	}
}
