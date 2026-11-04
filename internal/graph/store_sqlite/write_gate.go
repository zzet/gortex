package store_sqlite

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// sqliteWriteGate is a zero-value, context-aware binary semaphore. It keeps
// the Lock/Unlock surface used by existing mutation paths while allowing
// maintenance and bounded resolver work to stop waiting when their context
// expires. A channel is used instead of racing a helper goroutine against a
// sync.Mutex; the latter leaks a waiter that can acquire the mutex after the
// caller has already returned.
type sqliteWriteGate struct {
	once  sync.Once
	token chan struct{}
	// waiters counts callers parked in LockContext because the gate was held.
	// A holder that can give the gate up early (the WAL reclaim's open-gate
	// stage) watches it and yields as soon as a writer queues.
	waiters        atomic.Int32
	reclaimWaiters atomic.Int32 // maintenance queues are not foreground writer demand
	// holder is the current hold, recorded only while the hold watchdog runs.
	holder atomic.Pointer[holdRecord]
	// onRelease runs at every Unlock while the gate is still held (the
	// writer connection's page-cache counters; writer_cache_counters.go).
	onRelease atomic.Pointer[func()]
	// holds counts completed holds (measurement: how often a path takes the
	// writer).
	holds atomic.Int64
	// A WAL reclaim takes the gate through lockReclaim. reclaimSince is when
	// its hold began and reclaimQueued when another writer first queued
	// behind it (gateClock readings, zero when unset); Unlock turns them into
	// how long the reclaim held the writer and how long writers waited for it.
	reclaimSince                                 atomic.Int64
	reclaimQueued                                atomic.Int64
	reclaimHold                                  durationHistogram
	reclaimBurstHold                             durationHistogram
	reclaimBurst                                 atomic.Bool
	reclaimBurstCheck                            atomic.Pointer[func() bool]
	reclaimBulkExceptions, reclaimHardExceptions atomic.Int64
	reclaimWait                                  durationHistogram
}

// gateEpoch anchors gateClock to the monotonic clock, so a wall-clock step
// cannot bend a recorded hold or wait.
var gateEpoch = time.Now()

// gateClock is nanoseconds since gateEpoch, never zero.
func gateClock() int64 { return int64(time.Since(gateEpoch)) + 1 }

// held reports whether someone holds the gate right now (a racy snapshot).
func (g *sqliteWriteGate) held() bool {
	g.init()
	return len(g.token) == 0
}

// waiting reports how many callers are parked waiting for the gate.
func (g *sqliteWriteGate) waiting() int32 { return g.waiters.Load() }

func (g *sqliteWriteGate) init() {
	g.once.Do(func() {
		g.token = make(chan struct{}, 1)
		g.token <- struct{}{}
	})
}

func (g *sqliteWriteGate) Lock() {
	_ = g.LockContext(context.Background())
}

func (g *sqliteWriteGate) LockContext(ctx context.Context) error {
	return g.lockContext(ctx, true)
}

func (g *sqliteWriteGate) lockContext(ctx context.Context, foreground bool) error {
	g.init()
	// A single select may choose the token nondeterministically when ctx is
	// already canceled and the gate is available. Check before waiting, then
	// re-check after acquisition and restore the token on cancellation so a
	// maintenance deadline can never start work after it has expired.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-g.token:
		g.holder.Store(newHoldRecord())
		return nil
	default:
	}
	waiters := &g.waiters
	if !foreground {
		waiters = &g.reclaimWaiters
	}
	waiters.Add(1)
	defer waiters.Add(-1)
	if foreground && g.reclaimSince.Load() != 0 {
		g.reclaimQueued.CompareAndSwap(0, gateClock())
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.token:
		if err := ctx.Err(); err != nil {
			g.token <- struct{}{}
			return err
		}
		g.holder.Store(newHoldRecord())
		return nil
	}
}

// lockReclaim takes the gate for a WAL reclaim. Its Unlock records how long
// the reclaim held the writer and, when other writers queued meanwhile, how
// long the first of them waited for it. Other holders record neither.
func (g *sqliteWriteGate) lockReclaim(ctx context.Context) error {
	if err := g.lockContext(ctx, false); err != nil {
		return err
	}
	g.beginReclaim(false)
	return nil
}

func (g *sqliteWriteGate) beginReclaim(burst bool) {
	g.reclaimBurst.Store(burst)
	now := gateClock()
	g.reclaimQueued.Store(0)
	g.reclaimSince.Store(now)
	// Writers parked before the reclaim got the gate wait on it from now.
	// A writer that parks after the store above marks reclaimQueued itself.
	if g.waiters.Load() > 0 {
		g.reclaimQueued.CompareAndSwap(0, now)
	}
}

func (g *sqliteWriteGate) TryLock() bool {
	g.init()
	select {
	case <-g.token:
		g.holder.Store(newHoldRecord())
		return true
	default:
		return false
	}
}

func (g *sqliteWriteGate) Unlock() {
	g.init()
	if f := g.onRelease.Load(); f != nil {
		(*f)()
	}
	// Only the reclaim holding the gate sets reclaimSince, so an ordinary
	// release pays one load here.
	if since := g.reclaimSince.Load(); since != 0 {
		g.reclaimSince.Store(0)
		now := gateClock()
		g.reclaimHold.observe(time.Duration(now - since))
		burst := g.reclaimBurst.Swap(false)
		if check := g.reclaimBurstCheck.Swap(nil); check != nil {
			burst = burst || (*check)()
		}
		if burst {
			g.reclaimBurstHold.observe(time.Duration(now - since))
		}
		if queued := g.reclaimQueued.Swap(0); queued != 0 {
			g.reclaimWait.observe(time.Duration(now - max(since, queued)))
		}
	}
	endHold(g.holder.Swap(nil), "write gate")
	g.holds.Add(1)
	select {
	case g.token <- struct{}{}:
	default:
		panic("store_sqlite: unlock of unlocked write gate")
	}
}

// HoldWriteGate takes the store's mutation gate and returns the release.
//
// It is the exclusive-writer window in its own right, the way ResolveMutex is
// the resolver's: a caller that needs every other write to queue behind it
// holds this, and the read pool keeps answering throughout. It is also the
// only way a package that cannot see the gate can reproduce a saturated
// writer — the state a long build leaves the store in — and prove that a read
// path never reaches for it.
//
// Nothing bounds how long the gate may be held, so the release belongs on a
// defer. It is idempotent; releasing twice does not free somebody else's turn.
func (s *Store) HoldWriteGate(ctx context.Context) (func(), error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(s.writeMu.Unlock) }, nil
}
