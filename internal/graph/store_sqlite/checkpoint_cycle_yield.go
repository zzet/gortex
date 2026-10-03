package store_sqlite

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Background checkpoints yield to mutation cycles.
//
// The periodic PASSIVE loop and the bounded WAL reclaim are background work:
// neither is needed for an edit to publish, and on a large log a PASSIVE pass
// is seconds of CPU (page copies plus wal-index lookups). Under a daemon
// running with few cores that CPU comes straight out of the edit cycle that
// holds the build lane. So, once the daemon installs a build-lane predicate
// (SetBuildLaneBusy):
//
//   - no background checkpoint attempt starts while the lane is held; the
//     loops retry at their next poll, which lands in an idle window;
//   - an attempt already running when a cycle takes the lane is cancelled
//     (cause errWALCheckpointYieldedToCycle) within
//     walCheckpointCycleYieldPoll: its PASSIVE or TRUNCATE is interrupted, a
//     reader wait ends, the writer is released;
//   - deferral is bounded for the PASSIVE loop only: once it has been kept
//     off the lane for walCheckpointMaxCycleDeferral and the WAL is above its
//     pressure threshold, ONE PASSIVE runs anyway, without yielding, so a lane
//     that never goes idle cannot grow the log without backfill. The reclaim
//     (whose reset takes the writer and a TRUNCATE) does not run during a
//     cycle either — unless the -wal file is over the WAL ceiling
//     (walReclaimCeilingFactor × the reclaim threshold, never below
//     walReclaimCeilingFloor): then one attempt runs despite the lane, bounded
//     by the writer-hold cap rather than the lane, and logs reason=wal_ceiling.
//
// Nothing else changes: the open-read gate, TRUNCATE-first, the writer hold
// cap and the bulk-lease coordination behave exactly as before whenever the
// lane is idle, and a store without a predicate (tests, CLI, in-memory) never
// defers.

// errWALCheckpointInFlight refuses a background attempt while another one
// (the periodic PASSIVE or the reclaim) runs.
var errWALCheckpointInFlight = errors.New("store_sqlite: wal checkpoint: another background checkpoint is in flight")

var errWALCheckpointYieldedToCycle = errors.New("store_sqlite: wal checkpoint yielded to a mutation cycle holding the build lane")

// Vars, not consts, only so the in-package cases can shorten the cadence;
// production never assigns them.
var (
	// walCheckpointCycleYieldPoll is how often a running background attempt
	// checks the build lane.
	walCheckpointCycleYieldPoll = 20 * time.Millisecond
	// defaultWALCheckpointMaxCycleDeferral bounds how long the PASSIVE loop
	// may be kept off a busy lane before one attempt runs anyway (when the
	// WAL is above its threshold). Override with
	// GORTEX_SQLITE_WAL_CHECKPOINT_CYCLE_DEFER_MS (0 disables the yield).
	defaultWALCheckpointMaxCycleDeferral = 2 * time.Minute
)

const walCheckpointMaxCycleDeferralCap = 30 * time.Minute

// checkpointCyclePolicy is how a background checkpoint attempt treats the
// build lane.
type checkpointCyclePolicy uint8

const (
	// checkpointYieldsToCycle: refused while the lane is held, cancelled when
	// a cycle takes it.
	checkpointYieldsToCycle checkpointCyclePolicy = iota
	// checkpointIgnoresCycle: ignores the lane — the bounded-deferral PASSIVE,
	// and a reclaim over the WAL ceiling (still under the writer-hold cap and
	// the open-read gate).
	checkpointIgnoresCycle
	// checkpointOverridesLease: the reclaim over the WAL ceiling while a
	// generation bulk window holds the checkpoint lease. It ignores the lease
	// and the lane (never another attempt in flight), runs under the
	// writer-hold cap, and is rate-limited (walReclaimLeaseOverrideSpacing).
	checkpointOverridesLease
	// checkpointOverridesLeaseAndCycle: the lease override over the WAL hard
	// cap, which also ignores the lane (see reclaimWALAttempt).
	checkpointOverridesLeaseAndCycle
)

// checkpointCycleYield is the build-lane predicate and its counters. It lives
// on the shared core (inside walReclaimState) so every handle sees one.
type checkpointCycleYield struct {
	busy atomic.Pointer[func() bool]

	deferrals atomic.Int64 // PASSIVE attempts not started: lane held
	yields    atomic.Int64 // attempts (PASSIVE or reclaim) cancelled by a cycle
	forced    atomic.Int64 // PASSIVE attempts run past the deferral bound
	refusals  atomic.Int64 // reclaim attempts not started: lane held
	ceiling   atomic.Int64 // reclaim attempts run despite the lane: WAL over the hard cap
	// lastHardCap is the unix nanos of the last of those.
	lastHardCap atomic.Int64

	// loopThreshold is the running reclaim loop's threshold (0 when no loop
	// runs): over it the periodic PASSIVE stands aside.
	loopThreshold atomic.Int64

	leaseOverrides    atomic.Int64 // reclaim attempts run inside a bulk window: WAL over the ceiling
	lastLeaseOverride atomic.Int64 // unix nanos of the last one

	retirementWaits    atomic.Int64 // retirement chunks that waited for the reclaim (retirement_wal_wait.go)
	retirementTimeouts atomic.Int64 // of those, waits that ran out and proceeded

	retirementEditYields        atomic.Int64 // retirement chunks that waited for an edit-path writer
	retirementEditYieldTimeouts atomic.Int64 // of those, waits that ran out and proceeded

	maxDeferralOnce sync.Once
	maxDeferral     time.Duration
}

// SetBuildLaneBusy installs the predicate the background checkpoints consult:
// it reports whether a derived build (a mutation cycle) holds the daemon's
// build lane. It must be cheap and must not block; it is called every
// walCheckpointCycleYieldPoll while an attempt runs. nil removes it.
func (s *Store) SetBuildLaneBusy(busy func() bool) {
	if s.coreless() {
		return
	}
	if busy == nil {
		s.walReclaim.cycle.busy.Store(nil)
		return
	}
	s.walReclaim.cycle.busy.Store(&busy)
}

// buildLaneBusy reports the installed predicate's answer (false without one).
func (s *Store) buildLaneBusy() bool {
	if s.coreless() {
		return false
	}
	p := s.walReclaim.cycle.busy.Load()
	// An announced mutation (edit or undo, admission to route flip) counts
	// as a held lane: background work stands down for all of it.
	busy := p != nil && ((*p)() || s.editIntentActive())
	if busy {
		s.walCopy.sawBusy(time.Now())
	}
	return busy
}

func (s *Store) hasBuildLanePredicate() bool {
	return !s.coreless() && s.walReclaim.cycle.busy.Load() != nil
}

// maxCycleDeferral resolves GORTEX_SQLITE_WAL_CHECKPOINT_CYCLE_DEFER_MS once
// per store. 0 disables the yield altogether (attempts ignore the lane).
func (s *Store) maxCycleDeferral() time.Duration {
	c := &s.walReclaim.cycle
	c.maxDeferralOnce.Do(func() {
		c.maxDeferral = defaultWALCheckpointMaxCycleDeferral
		if raw := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_WAL_CHECKPOINT_CYCLE_DEFER_MS")); raw != "" {
			if ms, err := strconv.Atoi(raw); err == nil && ms >= 0 {
				c.maxDeferral = min(time.Duration(ms)*time.Millisecond, walCheckpointMaxCycleDeferralCap)
			}
		}
	})
	return c.maxDeferral
}

// cycleYieldEnabled reports whether background attempts consult the lane.
func (s *Store) cycleYieldEnabled() bool {
	return s.hasBuildLanePredicate() && s.maxCycleDeferral() > 0
}

// watchBuildLane cancels a yielding attempt once a cycle holds the lane. It
// runs on the attempt's own watcher goroutine (beginBackgroundCheckpointAttempt)
// and returns when the attempt ends, shutdown begins, or it cancelled.
func (s *Store) watchBuildLane(attempt *backgroundCheckpointAttempt) {
	ticker := time.NewTicker(walCheckpointCycleYieldPoll)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCheckpoint:
			attempt.cancel(context.Canceled)
			return
		case <-attempt.done:
			return
		case <-ticker.C:
			if s.cycleYieldEnabled() && s.buildLaneBusy() {
				if attempt.copy != nil && attempt.copy.copying.Load() {
					// A paced pass waits in its page writes instead: an
					// interrupt would discard every page it copied.
					continue
				}
				s.walReclaim.cycle.yields.Add(1)
				attempt.cancel(errWALCheckpointYieldedToCycle)
				return
			}
		}
	}
}

// cycleDeferral is the PASSIVE loop's goroutine-local deferral state.
type cycleDeferral struct {
	since  time.Time // first deferral or yield since the last attempt that ran
	logged bool      // the episode's deferral line was written
}

// decide reports whether the loop's due attempt may run now and, if so,
// whether it is the forced one. overThreshold is consulted only once the bound
// has passed.
// While a cycle holds the lane nothing runs, however long the deferral: a
// background PASSIVE's CPU would come out of the edit. A lane that never goes
// idle is bounded by the reclaim's hard cap instead (wal_reclaim.go).
func (d *cycleDeferral) decide(now time.Time, busy bool, maxDeferral time.Duration, overThreshold func() bool) (run, forced bool) {
	if !busy {
		return true, false
	}
	if d.since.IsZero() {
		d.since = now
	}
	_, _ = maxDeferral, overThreshold
	return false, false
}

// ran resets the episode after an attempt that was not cut short by a cycle.
func (d *cycleDeferral) ran() { *d = cycleDeferral{} }

// yielded starts (or continues) an episode after an attempt a cycle cut short.
func (d *cycleDeferral) yielded(now time.Time) {
	if d.since.IsZero() {
		d.since = now
	}
}

func logCycleDeferral(d *cycleDeferral, now time.Time) {
	if d.logged {
		return
	}
	d.logged = true
	log.Printf("store_sqlite: wal checkpoint deferred mode=PASSIVE reason=build_lane_busy deferred_for=%s", now.Sub(d.since).Round(time.Millisecond))
}

func logCycleForced(d *cycleDeferral, now time.Time) {
	log.Printf("store_sqlite: wal checkpoint forced mode=PASSIVE reason=cycle_deferral_bound deferred_for=%s", now.Sub(d.since).Round(time.Millisecond))
}

// cycleLane is the PASSIVE schedule's view of the build lane. The zero value
// (no store, or the yield disabled) is never busy.
type cycleLane struct {
	store       *Store
	maxDeferral time.Duration
}

// cycleLane returns the lane view for this store's PASSIVE loop. The
// predicate is read on every poll, so one installed after Open (the daemon
// wires it once the build gate exists) takes effect at the next attempt.
func (s *Store) cycleLane() cycleLane {
	if s.coreless() {
		return cycleLane{}
	}
	return cycleLane{store: s, maxDeferral: s.maxCycleDeferral()}
}

func (l cycleLane) busy() bool {
	return l.store != nil && l.maxDeferral > 0 && l.store.buildLaneBusy()
}

func (l cycleLane) noteDeferral() {
	if l.store != nil {
		l.store.walReclaim.cycle.deferrals.Add(1)
	}
}

func (l cycleLane) noteForced() {
	if l.store != nil {
		l.store.walReclaim.cycle.forced.Add(1)
	}
}

// over reports whether the -wal file is above the pressure threshold (a stat
// failure counts as over, like due's: pressure control must not turn off).
func (g *walPressureGate) over(walPath string) bool {
	if g.thresholdBytes <= 0 {
		return false
	}
	state, found, err := readSQLiteWALFileState(walPath)
	if err != nil {
		return true
	}
	return found && state.size > g.thresholdBytes
}

// cycleCancelled reports an attempt a mutation cycle cut short.
func cycleCancelled(attempt *backgroundCheckpointAttempt) bool {
	return attempt != nil && errors.Is(context.Cause(attempt.ctx), errWALCheckpointYieldedToCycle)
}

// reclaimOwns reports a log over the running reclaim's threshold: the reclaim
// owns it and the periodic PASSIVE stands aside (wal_reclaim_converge.go).
func (l cycleLane) reclaimOwns(walPath string) bool {
	if l.store == nil {
		return false
	}
	threshold := l.store.walReclaim.cycle.loopThreshold.Load()
	return threshold > 0 && walFileSize(walPath) > threshold
}

// cancelOnEditCycle cancels an in-flight maintenance statement (through
// cancel) when an edit-driven cycle takes the build lane, polling at
// walCheckpointCycleYieldPoll. yielded reports whether it fired; stop ends the
// watch and must be called when the statement returns. Without a predicate it
// watches nothing.
func (s *Store) cancelOnEditCycle(cancel context.CancelFunc) (stop func(), yielded *atomic.Bool) {
	yielded = new(atomic.Bool)
	if !s.hasBuildLanePredicate() {
		return func() {}, yielded
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(walCheckpointCycleYieldPoll)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if s.buildLaneBusy() {
					yielded.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}, yielded
}
