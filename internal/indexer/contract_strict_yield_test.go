package indexer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// TestContractStrictJobBoundsItsYield: while a strict request waits on a
// job, primary or linked, the job's yield takes at most
// contractStrictYieldShare of the time since its first strict call, plus the
// slice in progress, and it still yields; a skipped yield reports
// cancellation without waiting. A job no strict request waits on yields at
// every unit, and so does a strict job once the request's budget ended. The
// shared yield here is one that always meets foreground work: each call
// takes its whole slice on the probe's clock.
func TestContractStrictJobBoundsItsYield(t *testing.T) {
	const slice, work, units = 25 * time.Millisecond, time.Millisecond, 1000
	for _, tc := range []struct {
		name     string
		checkout string
		strict   bool
	}{
		{name: "strict_primary", strict: true},
		{name: "strict_linked", checkout: "wt", strict: true},
		{name: "unwaited_primary"},
		{name: "unwaited_linked", checkout: "wt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *linkedHoldProbe
			var yields atomic.Int64
			h = newLinkedHoldProbe(t, ContractLinkedEager, func(options *ContractAnalysisCoordinatorOptions) {
				options.Yield = func(ctx context.Context) error {
					yields.Add(1)
					h.advance(slice)
					return ctx.Err()
				}
			})
			// The strict budget is the request's deadline: past the units
			// below on the probe's clock, short of the interest TTL.
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			type observed struct {
				yields, after   int64
				elapsed         time.Duration
				err, canceled   error
				skippedCanceled bool
			}
			done := make(chan observed, 1)
			run := h.c.run
			h.c.run = func(job *contractAnalysisJob) string {
				var o observed
				yield := h.c.jobYield(job)
				start := h.now()
				for range units {
					if err := yield(t.Context()); err != nil {
						o.err = err
					}
					h.advance(work)
				}
				o.yields, o.elapsed = yields.Load(), h.now().Sub(start)
				if tc.strict {
					// Back-to-back yields reach the bound, and the yield
					// after one it skipped is skipped too.
					for range units {
						before := yields.Load()
						if err := yield(t.Context()); err != nil {
							o.err = err
						}
						if yields.Load() == before {
							break
						}
					}
				}
				canceled, stop := context.WithCancel(t.Context())
				stop()
				before := yields.Load()
				o.canceled = yield(canceled)
				o.skippedCanceled = yields.Load() == before
				// Past the strict budget every call yields again.
				h.advance(10 * time.Second)
				before = yields.Load()
				for range 10 {
					if err := yield(t.Context()); err != nil {
						o.err = err
					}
				}
				o.after = yields.Load() - before
				done <- o
				return run(job)
			}
			request := ContractScheduleRequest{Actor: NewContractActor(tc.checkout, []string{"r"}), Key: graph.ContractAttachmentKey{RepoPrefix: "r", CheckoutID: tc.checkout}, Inputs: linkedProbeInputs(tc.checkout, []string{"r"}, "k"), Epoch: h.c.InputEpoch(), Strict: tc.strict}
			if tc.checkout != "" {
				request.Source = &ContractCaptureSource{}
			}
			result, err := h.c.Schedule(ctx, request)
			require.NoError(t, err)
			require.Equal(t, ContractActorPublished, h.wait(result).Result)
			o := <-done
			require.NoError(t, o.err)
			require.ErrorIs(t, o.canceled, context.Canceled)
			require.EqualValues(t, 10, o.after, "a job yields at every unit once no strict request waits on it")
			if !tc.strict {
				require.EqualValues(t, units, o.yields, "a job no strict request waits on yields at every unit")
				require.False(t, o.skippedCanceled)
				return
			}
			require.Positive(t, o.yields, "a strict job still yields")
			require.Less(t, o.yields, int64(units), "but not at every unit")
			require.LessOrEqual(t, time.Duration(o.yields)*slice, o.elapsed*contractStrictYieldShare/100+slice, "the yield takes a bounded share of the job's time")
			require.True(t, o.skippedCanceled, "a skipped yield does not wait")
		})
	}
}
