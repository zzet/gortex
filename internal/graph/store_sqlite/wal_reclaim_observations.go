package store_sqlite

import (
	"context"
	"errors"
	"strings"
)

// Observation precedence, not exclusive causality: a positively observed
// writer parked on writeMu wins over a simultaneously observed announced intent.
type walReclaimInterruption uint32

const (
	walReclaimUninterrupted walReclaimInterruption = iota
	walReclaimWriterParked
	walReclaimWriteIntent
	walReclaimOtherCancellation
)

type walReclaimDemandCause struct{ kind walReclaimInterruption }

func (c *walReclaimDemandCause) Error() string { return errWALReclaimWriterWaiting.Error() }
func (c *walReclaimDemandCause) Unwrap() error { return errWALReclaimWriterWaiting }

var (
	walParkedDemand = &walReclaimDemandCause{walReclaimWriterParked}
	walIntentDemand = &walReclaimDemandCause{walReclaimWriteIntent}
)

type walReclaimAttemptKey struct{}

func (s *Store) writeDemandCause() error {
	if s.writeMu.waiting() > 0 {
		return walParkedDemand
	}
	if s.writeIntents.Load() > 0 {
		return walIntentDemand
	}
	return nil
}

func noteWALReclaimDemand(ctx context.Context, cause error) {
	attempt, _ := ctx.Value(walReclaimAttemptKey{}).(*backgroundCheckpointAttempt)
	if attempt == nil {
		return
	}
	var demand *walReclaimDemandCause
	if errors.As(cause, &demand) {
		attempt.reclaimInterruption.CompareAndSwap(uint32(walReclaimUninterrupted), uint32(demand.kind))
	}
}

func noteWALReclaimCancellation(attempt *backgroundCheckpointAttempt, err error) {
	// Child budget expiration, cycle/bulk withdrawal and finish/stopYield
	// cleanup are not terminal caller/shutdown cancellation. Observe only a
	// consumed error while the ORIGINAL operation is cancelled by that cause.
	if errors.Is(err, context.Canceled) && errors.Is(context.Cause(attempt.ctx), context.Canceled) {
		attempt.reclaimInterruption.CompareAndSwap(uint32(walReclaimUninterrupted), uint32(walReclaimOtherCancellation))
	}
}

func (st *WALReclaimStats) noteInterruption(res walReclaimResult) {
	if res.outcome == walReclaimReset || res.attempt == nil {
		return
	}
	switch walReclaimInterruption(res.attempt.reclaimInterruption.Load()) {
	case walReclaimWriterParked:
		st.CancelledWriterParked++
	case walReclaimWriteIntent:
		st.CancelledWriteIntent++
	case walReclaimOtherCancellation:
		st.CancelledOther++
	}
}

// noteObservedWALBytes consumes an EXISTING successful size observation, never
// performs I/O, and never resets on truncate/shrink or a status read.
func (s *Store) noteObservedWALBytes(bytes int64) {
	if s.coreless() || s.dbPath == "" || strings.Contains(s.dbPath, ":memory:") || bytes < 0 {
		return
	}
	for old := s.walReclaim.observedPeak.Load(); bytes > old; old = s.walReclaim.observedPeak.Load() {
		if s.walReclaim.observedPeak.CompareAndSwap(old, bytes) {
			break
		}
	}
	s.walReclaim.peakObserved.Store(true)
}

func (s *Store) observedWALFileSize(path string) int64 {
	bytes := walFileSize(path)
	// The legacy helper collapses absence/I/O errors to0. A positive size
	// proves an observation; DBStats separately handles proven empty/absent WAL.
	if bytes > 0 {
		s.noteObservedWALBytes(bytes)
	}
	return bytes
}
