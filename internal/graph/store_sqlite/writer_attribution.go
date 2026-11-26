package store_sqlite

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"modernc.org/libc"
)

// SQLite writer time owned by one caller.
//
// The VFS counters (vfs_io_counters.go) are process-wide: every concurrent
// writer adds to any interval of them, so they cannot say how much of the
// writer connection's xWrite time belongs to one edit. A WriterAttribution
// collects only the xWrite calls that a store's writer connection makes under
// the write-gate holds acquired by the goroutine running inside
// AttributeWriterWrites.
//
// Ownership is decided once per hold, when a store's writer gate is acquired,
// from the acquiring goroutine's own stack. AttributeWriterWrites calls its
// function below marker frames whose number is the attribution's slot, so a
// stack that carries the marker belongs to exactly one open attribution and
// any other stack to none. Holds taken by other goroutines (another editor's
// attempt or acknowledgement, maintenance, a goroutine the owner starts) are
// never counted. An owned hold is keyed by the writer connection's libc TLS
// until its release, and the xWrite wrappers add every write of that
// connection to the owner: only the gate holder writes through the store's
// single writer connection, and SQLite runs on the calling goroutine. The gate
// is binary, so owned holds, and the writes under them, never overlap.
//
// With no attribution open, a gate acquisition pays one atomic load, a
// release one and an xWrite one. While one is open, each acquisition of a
// store's writer gate walks the acquiring stack once, before it waits for the
// gate rather than inside the hold, and each xWrite under an owned hold adds a
// map lookup (BenchmarkWriteGateLockUnlock,
// BenchmarkWriterAttributionStoreWrite).

// WriterAttribution accumulates one owner's writer-connection xWrite time.
// The zero value is ready to use; it is opened at most once.
type WriterAttribution struct {
	mu     sync.Mutex
	reason string
	// gate is the writer gate of the first store the owner bound or held;
	// bound says BindStore ran. gateAtBind and ownedAtBind are the gate's
	// and this attribution's completed holds when it was bound.
	gate                    *sqliteWriteGate
	bound                   bool
	gateAtBind, ownedAtBind int64
	holds, open             int64
	holdTime                time.Duration
	firstHold, lastHold     time.Time
	// xWrite tallies, added on the holder's goroutine.
	main, wal writeTally
}

type writeTally struct{ calls, nanos, bytes atomic.Int64 }

func (t *writeTally) add(n int32, took time.Duration) {
	t.calls.Add(1)
	t.nanos.Add(int64(took))
	t.bytes.Add(int64(n))
}

// WriterAttributionSnapshot is one reading of a WriterAttribution. When
// Available is false, Reason says why and no counter is a measured value.
type WriterAttributionSnapshot struct {
	Available bool
	Reason    string
	// Holds are the completed owned holds and HoldTime their summed
	// acquisition-to-release wall; OpenHolds are still held (their writes so
	// far are counted, their wall is not).
	Holds, OpenHolds              int64
	HoldTime                      time.Duration
	FirstHoldStart, LastHoldEnd   time.Time
	MainWrites, WALWrites         int64
	MainWriteTime, WALWriteTime   time.Duration
	MainWriteBytes, WALWriteBytes int64
	// UnownedStoreHolds counts the holds of the bound store's writer gate
	// completed since BindStore that this attribution did not own: other
	// writers that used the store meanwhile. -1 when the store is not bound.
	UnownedStoreHolds int64
}

// WriteTime is the owned xWrite time on the main database file and the log.
func (s WriterAttributionSnapshot) WriteTime() time.Duration {
	return s.MainWriteTime + s.WALWriteTime
}

// MarkUnavailable records why the attribution cannot be complete. The first
// reason is kept.
func (a *WriterAttribution) MarkUnavailable(reason string) {
	if a == nil || reason == "" {
		return
	}
	a.mu.Lock()
	a.setReasonLocked(reason)
	a.mu.Unlock()
}

func (a *WriterAttribution) setReasonLocked(reason string) {
	if a.reason == "" {
		a.reason = reason
	}
}

// BindStore names the store whose writes the owner makes; nil when the
// owner's graph is not a SQLite store. Writes then become attributable only
// when that store has a dedicated writer connection whose VFS writes are
// counted, and the store's other holds from now on are counted as unowned.
func (a *WriterAttribution) BindStore(s *Store) {
	if a == nil {
		return
	}
	reason := s.writerAttributionUnavailable()
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.bound:
		if reason != "" || a.gate != &s.writeMu {
			a.setReasonLocked("writes_to_multiple_stores")
		}
		return
	case reason != "":
		a.bound = true
		a.setReasonLocked(reason)
		return
	case a.gate != nil && a.gate != &s.writeMu:
		a.setReasonLocked("writes_to_multiple_stores")
	}
	a.bound, a.gate = true, &s.writeMu
	a.gateAtBind, a.ownedAtBind = s.writeMu.holds.Load(), a.holds
}

// writerAttributionUnavailable says why writes to s cannot be attributed to
// an owner, or "" when they can.
func (s *Store) writerAttributionUnavailable() string {
	switch {
	case s == nil:
		return "not_a_sqlite_store"
	case s.coreless() || s.writerDB == nil || s.writerDB == s.db:
		return "no_dedicated_writer_connection"
	case !vfsIOInstalled.Load():
		return "vfs_write_hooks_unavailable"
	case s.writeMu.writerTLS.Load() == 0:
		return "writer_connection_unidentified"
	}
	return ""
}

// Snapshot reads the attribution.
func (a *WriterAttribution) Snapshot() WriterAttributionSnapshot {
	if a == nil {
		return WriterAttributionSnapshot{Reason: "attribution_not_opened", UnownedStoreHolds: -1}
	}
	a.mu.Lock()
	out := WriterAttributionSnapshot{
		Reason: a.reason, Holds: a.holds, OpenHolds: a.open, HoldTime: a.holdTime,
		FirstHoldStart: a.firstHold, LastHoldEnd: a.lastHold, UnownedStoreHolds: -1,
	}
	if !a.bound && out.Reason == "" {
		out.Reason = "store_not_bound"
	}
	if a.bound && a.gate != nil {
		out.UnownedStoreHolds = (a.gate.holds.Load() - a.gateAtBind) - (a.holds - a.ownedAtBind)
	}
	a.mu.Unlock()
	out.Available = out.Reason == ""
	out.MainWrites, out.MainWriteTime, out.MainWriteBytes = a.main.calls.Load(), time.Duration(a.main.nanos.Load()), a.main.bytes.Load()
	out.WALWrites, out.WALWriteTime, out.WALWriteBytes = a.wal.calls.Load(), time.Duration(a.wal.nanos.Load()), a.wal.bytes.Load()
	return out
}

func (a *WriterAttribution) beginHold(g *sqliteWriteGate) {
	a.mu.Lock()
	switch {
	case a.gate == nil:
		a.gate = g
	case a.gate != g:
		a.setReasonLocked("writes_to_multiple_stores")
	}
	a.open++
	a.mu.Unlock()
}

func (a *WriterAttribution) finishHold(start, end time.Time) {
	a.mu.Lock()
	a.open--
	a.holds++
	a.holdTime += end.Sub(start)
	if a.firstHold.IsZero() {
		a.firstHold = start
	}
	a.lastHold = end
	a.mu.Unlock()
}

func (a *WriterAttribution) openHolds() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.open
}

// writerAttributionSlots bounds how many attributions can be open at once; a
// slot's index is the number of its marker frames.
const writerAttributionSlots = 64

var writerAttributions struct {
	open  atomic.Int32
	slots [writerAttributionSlots]atomic.Pointer[WriterAttribution]
}

// AttributeWriterWrites runs fn with a open: the writer-gate holds that fn's
// own goroutine acquires, and the writer-connection writes made under them,
// are added to a. A nil a runs fn unobserved. A hold still open when fn
// returns makes a unavailable.
func AttributeWriterWrites(a *WriterAttribution, fn func()) {
	if a == nil {
		fn()
		return
	}
	if entry, _ := writerMarkers(); entry == 0 {
		a.MarkUnavailable("attribution_marker_unavailable")
		fn()
		return
	}
	slot := -1
	for i := range writerAttributions.slots {
		if writerAttributions.slots[i].CompareAndSwap(nil, a) {
			slot = i
			break
		}
	}
	if slot < 0 {
		a.MarkUnavailable("attribution_slots_exhausted")
		fn()
		return
	}
	writerAttributions.open.Add(1)
	defer func() {
		writerAttributions.open.Add(-1)
		writerAttributions.slots[slot].Store(nil)
		if a.openHolds() > 0 {
			a.MarkUnavailable("owned_hold_open_at_return")
		}
	}()
	writerAttributionFrame(slot, fn)
}

// writerAttributionFrame calls itself depth times, then fn through
// writerAttributionEntry. Neither is inlined and both calls are direct, so
// their return addresses mark an attributed stack: the entry call's once and
// the recursive call's once per level.
//
//go:noinline
func writerAttributionFrame(depth int, fn func()) {
	if depth > 0 {
		writerAttributionFrame(depth-1, fn)
		return
	}
	writerAttributionEntry(fn)
}

//go:noinline
func writerAttributionEntry(fn func()) { fn() }

var writerMarkerPCs struct {
	once sync.Once
	// The return addresses of the entry call and of the recursive call.
	entry, level uintptr
}

// writerMarkers learns the marker return addresses once; zero when they
// cannot be told apart (AttributeWriterWrites then refuses to attribute).
func writerMarkers() (entry, level uintptr) {
	writerMarkerPCs.once.Do(func() {
		var pcs [2]uintptr
		writerAttributionFrame(1, func() {
			// Skip Callers, this closure and writerAttributionEntry.
			runtime.Callers(3, pcs[:])
		})
		if pcs[0] != 0 && pcs[1] != 0 && pcs[0] != pcs[1] {
			writerMarkerPCs.entry, writerMarkerPCs.level = pcs[0], pcs[1]
		}
	})
	return writerMarkerPCs.entry, writerMarkerPCs.level
}

// An acquiring stack is walked into a pooled buffer first and, when deeper,
// once more up to writerDeepStackDepth frames. A deeper stack without the
// marker in that range cannot be classified.
const (
	writerStackDepth     = 256
	writerDeepStackDepth = 1 << 14
)

var writerStackPCs = sync.Pool{New: func() any { return new([writerStackDepth]uintptr) }}

// callerWriterAttribution is the open attribution the calling goroutine runs
// inside, nil when none; decided is false when its stack was too deep to
// tell.
func callerWriterAttribution() (a *WriterAttribution, decided bool) {
	entry, level := writerMarkers()
	if entry == 0 {
		return nil, true
	}
	buf := writerStackPCs.Get().(*[writerStackDepth]uintptr)
	n := runtime.Callers(3, buf[:])
	slot, decided := writerMarkerSlot(buf[:n], n == len(buf), entry, level)
	writerStackPCs.Put(buf)
	if !decided {
		deep := make([]uintptr, writerDeepStackDepth)
		n = runtime.Callers(3, deep)
		slot, decided = writerMarkerSlot(deep[:n], n == len(deep), entry, level)
	}
	if slot < 0 {
		return nil, decided
	}
	return writerAttributions.slots[slot].Load(), true
}

// writerMarkerSlot finds the innermost marker in pcs (innermost frame first)
// and returns its slot, or -1. truncated says the stack goes on past pcs.
func writerMarkerSlot(pcs []uintptr, truncated bool, entry, level uintptr) (slot int, decided bool) {
	for i, pc := range pcs {
		if pc != entry {
			continue
		}
		depth := 0
		for i+1+depth < len(pcs) && pcs[i+1+depth] == level {
			depth++
		}
		if truncated && i+1+depth == len(pcs) || depth >= writerAttributionSlots {
			return -1, false
		}
		return depth, true
	}
	return -1, !truncated
}

func markOpenWriterAttributions(reason string) {
	for i := range writerAttributions.slots {
		writerAttributions.slots[i].Load().MarkUnavailable(reason)
	}
}

// ownedWriterHold is one writer-gate hold that belongs to an attribution.
type ownedWriterHold struct {
	owner *WriterAttribution
	tls   uintptr
	start time.Time
}

var (
	ownedWriterHolds     sync.Map // writer connection TLS (uintptr) → *ownedWriterHold
	ownedWriterHoldCount atomic.Int32
)

// holdOwner is the attribution an acquiring goroutine runs inside, decided
// before it takes the gate.
type holdOwner struct {
	attribution *WriterAttribution
	undecided   bool
}

// acquiringOwner runs on the acquiring goroutine before it waits for g. With
// no attribution open, the goroutine cannot be inside one.
func (g *sqliteWriteGate) acquiringOwner() holdOwner {
	if writerAttributions.open.Load() == 0 || g.writerTLS.Load() == 0 {
		return holdOwner{} // none open, or not a store's writer gate
	}
	a, decided := callerWriterAttribution()
	return holdOwner{attribution: a, undecided: !decided}
}

// attributeHold runs on the acquiring goroutine right after it took g.
func (g *sqliteWriteGate) attributeHold(owner holdOwner) {
	if owner.undecided {
		markOpenWriterAttributions("attribution_stack_undecidable")
		return
	}
	a := owner.attribution
	if a == nil {
		return
	}
	tls := g.writerTLS.Load()
	if tls == 0 {
		a.MarkUnavailable("writer_connection_unidentified")
		return
	}
	if !vfsIOInstalled.Load() {
		a.MarkUnavailable("vfs_write_hooks_unavailable")
		return
	}
	h := &ownedWriterHold{owner: a, tls: tls, start: time.Now()}
	a.beginHold(g)
	g.owned.Store(h)
	ownedWriterHolds.Store(tls, h)
	ownedWriterHoldCount.Add(1)
}

// finishOwnedHold runs in Unlock after the release hook sampled the writer
// connection again: a different connection means writes may have gone to one
// the hold was not keyed under.
func (g *sqliteWriteGate) finishOwnedHold(h *ownedWriterHold, released time.Time) {
	g.owned.Store(nil)
	ownedWriterHolds.CompareAndDelete(h.tls, h)
	ownedWriterHoldCount.Add(-1)
	if g.writerTLS.Load() != h.tls {
		h.owner.MarkUnavailable("writer_connection_changed")
	}
	h.owner.finishHold(h.start, released)
}

// noteOwnedWriterWrite adds one xWrite of an owned hold's connection.
func noteOwnedWriterWrite(tls *libc.TLS, kind int, n int32, took time.Duration) {
	v, ok := ownedWriterHolds.Load(uintptr(unsafe.Pointer(tls)))
	if !ok {
		return
	}
	a := v.(*ownedWriterHold).owner
	switch kind {
	case vfsFileMain:
		a.main.add(n, took)
	case vfsFileWAL:
		a.wal.add(n, took)
	}
}
