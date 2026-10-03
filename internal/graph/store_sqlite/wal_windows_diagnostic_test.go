package store_sqlite

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Test diagnostics for checkpoint cancellation and reader lifetimes.
// No workload, deadlines, or SQL added.
// A cancellation timestamp is callback dispatch, not the exact cancel instruction.
// Epoch snapshots are sampled/post-return connections, not active WAL lock proof.
type windowsWALEvent struct {
	at                   time.Time
	kind                 string
	attempt              *backgroundCheckpointAttempt
	err, cause, reported error
	copying, pausable    bool
	epoch                uint64
	active, older        int
	reader               int64
	sleep                SQLiteSleepMark
}
type windowsWALDiagnostic struct {
	t             *testing.T
	store         atomic.Pointer[Store]
	mu            sync.Mutex
	events        []windowsWALEvent
	dropped       int
	cost          time.Duration
	stop, joined  chan struct{}
	stack         []byte
	stackAt       time.Time
	stackCost     time.Duration
	finishRestore func()
	initialSleep  SQLiteSleepMark
}

// The optional reader hook is copied once before the reader goroutines start.
var windowsWALReaderObserver func(int64, string, error)

func newWindowsWALDiagnostic(t *testing.T) *windowsWALDiagnostic {
	return &windowsWALDiagnostic{t: t, stop: make(chan struct{}), joined: make(chan struct{}), initialSleep: sqliteSleepMark()}
}
func (d *windowsWALDiagnostic) record(kind string, reader int64, err error) {
	d.recordAttempt(kind, reader, err, nil)
}
func (d *windowsWALDiagnostic) recordAttempt(kind string, reader int64, err error, known *backgroundCheckpointAttempt) {
	started := time.Now()
	e := windowsWALEvent{at: started, kind: kind, reader: reader, reported: err, sleep: sqliteSleepMark()}
	if s := d.store.Load(); s != nil {
		s.backgroundCheckpoint.mu.Lock()
		e.attempt = s.backgroundCheckpoint.active
		s.backgroundCheckpoint.mu.Unlock()
		if known != nil {
			e.attempt = known
		}
		if a := e.attempt; a != nil {
			e.err, e.cause = a.ctx.Err(), context.Cause(a.ctx)
			if a.copy != nil {
				e.copying = a.copy.copying.Load()
				e.pausable = a.copy.pausable.Load()
			}
		}
		if g := s.readGate; g != nil {
			g.mu.Lock()
			e.epoch = g.epoch
			e.active = g.active
			e.older = g.olderLocked(g.epoch)
			g.mu.Unlock()
		}
	}
	d.mu.Lock()
	if len(d.events) < 4096 {
		d.events = append(d.events, e)
	} else {
		d.dropped++
	}
	d.cost += time.Since(started)
	d.mu.Unlock()
}
func (d *windowsWALDiagnostic) install() {
	previousCheckpoint := walCheckpointCallObserver
	walCheckpointCallObserver = func(mode string, start time.Time, took time.Duration) {
		d.record("checkpoint-return:"+mode, -1, nil)
		// Exact wrapper interval is carried by a separate immutable event.
		d.mu.Lock()
		if len(d.events)+2 <= 4096 {
			d.events = append(d.events, windowsWALEvent{at: start, kind: "checkpoint-start:" + mode, reader: -1}, windowsWALEvent{at: start.Add(took), kind: "checkpoint-wrapper-end:" + mode, reader: -1})
		}
		d.mu.Unlock()
		if previousCheckpoint != nil {
			previousCheckpoint(mode, start, took)
		}
	}
	previousReset := walIdleResetResultHook
	walIdleResetResultHook = func(err error) error {
		d.record("short-reset-error-post-return", -1, err)
		if previousReset != nil {
			return previousReset(err)
		}
		return err
	}
	go func() {
		defer close(d.joined)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		type notification struct {
			stop func() bool
			done chan struct{}
		}
		seen := make(map[*backgroundCheckpointAttempt]notification)
		var canceledSince time.Time
		var last *backgroundCheckpointAttempt
		for {
			select {
			case <-d.stop:
				for _, n := range seen {
					if !n.stop() {
						<-n.done
					}
				}
				return
			case <-ticker.C:
				s := d.store.Load()
				if s == nil {
					continue
				}
				s.backgroundCheckpoint.mu.Lock()
				a := s.backgroundCheckpoint.active
				s.backgroundCheckpoint.mu.Unlock()
				if a != last {
					canceledSince = time.Time{}
					last = a
				}
				if a == nil {
					continue
				}
				if _, ok := seen[a]; !ok {
					d.record("attempt-first-observed", -1, nil)
					done := make(chan struct{})
					stop := context.AfterFunc(a.ctx, func() { defer close(done); d.recordAttempt("attempt-cancel-dispatch", -1, nil, a) })
					seen[a] = notification{stop, done}
				}
				if a.ctx.Err() != nil {
					if canceledSince.IsZero() {
						canceledSince = time.Now()
						d.recordAttempt("attempt-cancel-first-sampled", -1, nil, a)
					}
					if d.stack == nil && time.Since(canceledSince) > 300*time.Millisecond {
						began := time.Now()
						b := make([]byte, 128<<10)
						n := runtime.Stack(b, true)
						d.stack, d.stackAt, d.stackCost = b[:n], began, time.Since(began)
					}
				} else {
					canceledSince = time.Time{}
				}
			}
		}
	}()
	// Register before Store.Close's defer: close joins drivers before hook restoration.
	d.finishRestore = func() { walCheckpointCallObserver = previousCheckpoint; walIdleResetResultHook = previousReset }
}
func (d *windowsWALDiagnostic) finish() {
	close(d.stop)
	<-d.joined
	d.finishRestore()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.t.Logf("WAL diagnosis: events=%d dropped=%d event_record_cost=%s stack_cost=%s; cancel timestamps are notification dispatch; epochs are sampled/post-return, not WAL lock ownership; no before-reset cohort or SQLite result tuple available", len(d.events), d.dropped, d.cost, d.stackCost)
	d.t.Logf("WAL process-wide sleep baseline=%+v final_including_store_close=%+v; concurrent connections contribute, no per-call attribution", d.initialSleep, sqliteSleepMark())
	for _, e := range d.events {
		d.t.Logf("WAL diagnostic event at=%s kind=%s reader=%d attempt=%p context_err=%v cause=%v reported_error=%v copying=%v pausable=%v sampled_epoch=%d sampled_active=%d sampled_older=%d process_wide_sqlite_sleep=%+v", e.at.Format(time.RFC3339Nano), e.kind, e.reader, e.attempt, e.err, e.cause, e.reported, e.copying, e.pausable, e.epoch, e.active, e.older, e.sleep)
	}
	if len(d.stack) > 0 {
		d.t.Logf("WAL one-shot canceled-still-active stack at=%s cost=%s\n%s", d.stackAt.Format(time.RFC3339Nano), d.stackCost, d.stack)
	}
}
