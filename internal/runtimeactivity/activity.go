// Package runtimeactivity coordinates process-wide work with expensive runtime
// maintenance such as debug.FreeOSMemory. The daemon, MCP handlers, and
// background indexers share one tracker because a process-wide GC cannot be
// made safe by guarding only one subsystem.
package runtimeactivity

import (
	"sync"
	"sync/atomic"
	"time"
)

// Snapshot is a point-in-time view of process work. ByKind is a defensive copy
// and can be inspected without holding the tracker lock.
type Snapshot struct {
	Active         int64
	Epoch          uint64
	LastTransition time.Time
	ByKind         map[string]int64
}

// QuietFor reports how long no tracked work transition has occurred. It is
// zero while work is active or when the snapshot predates tracker startup.
func (s Snapshot) QuietFor(now time.Time) time.Duration {
	if s.Active != 0 || s.LastTransition.IsZero() || now.Before(s.LastTransition) {
		return 0
	}
	return now.Sub(s.LastTransition)
}

// Tracker closes the check-to-maintenance race with gate: Begin and End take a
// read lock while a maintenance callback takes the write lock. Once RunIfQuiet
// admits a callback, no new tracked work can start until it returns.
type Tracker struct {
	gate sync.RWMutex

	active   atomic.Int64
	epoch    atomic.Uint64
	lastNano atomic.Int64

	kindsMu sync.Mutex
	byKind  map[string]int64
	// kindEpoch counts each kind's Begin and End calls (KindEpoch).
	kindEpoch map[string]uint64
}

// NewTracker returns an independent tracker. Most production code uses the
// process-wide package functions below; the constructor exists for isolated
// policy and race tests.
func NewTracker() *Tracker {
	t := &Tracker{byKind: make(map[string]int64), kindEpoch: make(map[string]uint64)}
	t.lastNano.Store(time.Now().UnixNano())
	return t
}

// Begin marks one unit of work active. kind should be a bounded subsystem name,
// not a path or user-provided value.
func (t *Tracker) Begin(kind string) {
	if t == nil {
		return
	}
	kind = normalizeKind(kind)
	t.gate.RLock()
	t.active.Add(1)
	t.epoch.Add(1)
	t.lastNano.Store(time.Now().UnixNano())
	t.kindsMu.Lock()
	if t.byKind == nil {
		t.byKind = make(map[string]int64)
	}
	t.byKind[kind]++
	t.noteKindLocked(kind)
	t.kindsMu.Unlock()
	t.gate.RUnlock()
}

// End balances Begin. A defensive underflow repair prevents a caller bug from
// permanently disabling reclamation; per-kind counts are repaired likewise.
func (t *Tracker) End(kind string) {
	if t == nil {
		return
	}
	kind = normalizeKind(kind)
	t.gate.RLock()
	if t.active.Add(-1) < 0 {
		t.active.Store(0)
	}
	t.epoch.Add(1)
	t.lastNano.Store(time.Now().UnixNano())
	t.kindsMu.Lock()
	if n := t.byKind[kind]; n > 1 {
		t.byKind[kind] = n - 1
	} else {
		delete(t.byKind, kind)
	}
	t.noteKindLocked(kind)
	t.kindsMu.Unlock()
	t.gate.RUnlock()
}

func (t *Tracker) noteKindLocked(kind string) {
	if t.kindEpoch == nil {
		t.kindEpoch = make(map[string]uint64)
	}
	t.kindEpoch[kind]++
}

// KindEpoch counts kind's Begin and End calls. It moves whenever work of that
// kind starts or ends, so a caller that reads it now and then still sees work
// that started and ended between two reads, which ByKind cannot show.
func (t *Tracker) KindEpoch(kind string) uint64 {
	if t == nil {
		return 0
	}
	kind = normalizeKind(kind)
	t.kindsMu.Lock()
	defer t.kindsMu.Unlock()
	return t.kindEpoch[kind]
}

// Snapshot returns process activity without blocking a running maintenance
// callback for longer than the small map copy.
func (t *Tracker) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{}
	}
	s := Snapshot{
		Active: t.active.Load(),
		Epoch:  t.epoch.Load(),
	}
	if n := t.lastNano.Load(); n > 0 {
		s.LastTransition = time.Unix(0, n)
	}
	t.kindsMu.Lock()
	if len(t.byKind) > 0 {
		s.ByKind = make(map[string]int64, len(t.byKind))
		for kind, count := range t.byKind {
			s.ByKind[kind] = count
		}
	}
	t.kindsMu.Unlock()
	return s
}

// RunIfQuiet runs fn only when there is no active work and the last activity
// transition is at least quiet ago. retryAfter is the earliest useful retry.
// The callback runs behind the exclusive gate, so Begin cannot race between
// the idle check and a stop-the-world runtime operation.
func (t *Tracker) RunIfQuiet(quiet time.Duration, fn func()) (ran bool, retryAfter time.Duration) {
	if t == nil {
		return false, quiet
	}
	if quiet < 0 {
		quiet = 0
	}
	t.gate.Lock()
	defer t.gate.Unlock()

	if t.active.Load() != 0 {
		return false, quiet
	}
	last := time.Unix(0, t.lastNano.Load())
	if remaining := quiet - time.Since(last); remaining > 0 {
		return false, remaining
	}
	if fn != nil {
		fn()
	}
	return true, 0
}

func normalizeKind(kind string) string {
	if kind == "" {
		return "unspecified"
	}
	return kind
}

var process = NewTracker()

// Begin marks process-wide work active.
func Begin(kind string) { process.Begin(kind) }

// End balances a process-wide Begin.
func End(kind string) { process.End(kind) }

// Current returns a process-wide activity snapshot.
func Current() Snapshot { return process.Snapshot() }

// KindEpoch returns the process-wide KindEpoch of kind.
func KindEpoch(kind string) uint64 { return process.KindEpoch(kind) }

// RunIfQuiet executes fn behind the process-wide exclusive gate.
func RunIfQuiet(quiet time.Duration, fn func()) (bool, time.Duration) {
	return process.RunIfQuiet(quiet, fn)
}
