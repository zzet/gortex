package daemon

import (
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// WALReclaimStatusFrom renders only the existing I/O-free counter snapshot.
// Both status and live index health use this conversion.
func WALReclaimStatusFrom(st store_sqlite.WALReclaimStats) *WALReclaimStatus {
	out := &WALReclaimStatus{
		WriterHold:            walHistogramStatus(st.WriterHold),
		WriterWait:            walHistogramStatus(st.WriterWait),
		CancelledWriterParked: st.CancelledWriterParked,
		CancelledWriteIntent:  st.CancelledWriteIntent,
		CancelledOther:        st.CancelledOther,
		WALPeakObserved:       st.WALPeakObserved,

		ThresholdBytes:   st.ThresholdBytes,
		Attempts:         st.Attempts,
		Resets:           st.Resets,
		OpenGateResets:   st.OpenGateResets,
		WriterHoldMaxMS:  walDurationMS(st.WriterHoldMax),
		WriterHoldLastMS: walDurationMS(st.WriterHoldLast),
		Deferrals:        st.Deferrals,
		Skips:            st.Skips,
		Failures:         st.Failures,
		FramesReclaimed:  st.FramesReclaimed,
		BytesReclaimed:   st.BytesReclaimed,
		PauseCount:       st.PauseCount,
		PauseMaxMS:       walDurationMS(st.PauseMax),
		PauseLastMS:      walDurationMS(st.PauseLast),
		ReaderWaits:      st.ReaderWaits,
		ReaderWaitMaxMS:  walDurationMS(st.ReaderWaitMax),
		BackoffMS:        st.Backoff.Milliseconds(),
		LastOutcome:      st.LastOutcome,
		LastReason:       st.LastReason,
		CycleDeferrals:   st.CycleDeferrals,
		CycleRefusals:    st.CycleRefusals,
		CycleYields:      st.CycleYields,
		CycleForced:      st.CycleForced,
		CycleCeilingRuns: st.CycleCeilingRuns,
		CeilingBytes:     st.CeilingBytes,

		RetirementEditYields:        st.RetirementEditYields,
		RetirementEditYieldTimeouts: st.RetirementEditYieldTimeouts,
		RetirementWaits:             st.RetirementWaits,
		RetirementWaitTimeouts:      st.RetirementWaitTimeouts,
		LeaseOverrides:              st.LeaseOverrides,
		ShrinkInPlaceResets:         st.ShrinkInPlaceResets,
		ShrinkSlices:                st.ShrinkSlices,
		ShrinkBytes:                 st.ShrinkBytes,
		ShrinkSliceHoldMaxMS:        walDurationMS(st.ShrinkSliceHoldMax),
	}
	if st.WALPeakObserved {
		peak := st.ObservedPeakWALBytes
		out.ObservedPeakWALBytes = &peak
	}
	if st.PauseCount > 0 {
		out.PauseAvgMS = walDurationMS(st.PauseTotal / time.Duration(st.PauseCount))
	}
	if st.ReaderWaits > 0 {
		out.ReaderWaitAvgMS = walDurationMS(st.ReaderWaitTotal / time.Duration(st.ReaderWaits))
	}
	return out
}

func walHistogramStatus(h store_sqlite.DurationHistogram) WALDurationHistogramStatus {
	bounds := make([]float64, len(store_sqlite.DurationHistogramBounds))
	for i, bound := range store_sqlite.DurationHistogramBounds {
		bounds[i] = walDurationMS(bound)
	}
	return WALDurationHistogramStatus{
		Count: h.Count(), TotalMS: walDurationMS(h.Total), MaxMS: walDurationMS(h.Max),
		P50MS: walDurationMS(h.Quantile(0.5)), P95MS: walDurationMS(h.Quantile(0.95)),
		BucketLeMS: bounds, BucketCounts: append([]int64(nil), h.Counts[:]...),
	}
}

func walDurationMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
