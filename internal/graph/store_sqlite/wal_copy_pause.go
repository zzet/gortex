package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// The reclaim's copy pauses for edits instead of being interrupted.
//
// A checkpoint copies WAL frames into the database file page by page and
// records its progress (nBackfill) only when the whole pass is done. An
// interrupted pass (sqlite3_interrupt, which cancelling the statement's
// context does) breaks out of the copy loop, skips the sync and never stores
// nBackfill, so every page it copied is copied again by the next pass. With
// the reclaim cancelled at every edit, a log bigger than one gap between edits
// was never reclaimed at all: every attempt re-copied the same pages until the
// next edit (296 s of CPU in 32 laps of one run, the log growing 16 MB/min).
//
// So while the reclaim's writer-free passes copy, every page write of the
// reclaim's own checkpoint connection passes through a pacer. While an edit
// holds the build lane or a write announcement is active, the write waits;
// when the edit ends the copy continues where it was, and the pass completes
// and moves nBackfill. The wait is inside the VFS write, below SQLite: the
// statement is not interrupted and nothing it did is lost.
//
// What a paused pass holds, and why nothing waits on it:
//   - the WAL checkpointer lock (WAL_CKPT_LOCK). Only another checkpoint
//     takes it; the store runs one background checkpoint at a time, and the
//     writer never checkpoints (wal_autocheckpoint is 0).
//   - read-mark 0, exclusively, while it copies. A reader takes mark 0 only
//     when the log is completely backfilled, which it is not while a pass
//     runs (nBackfill moves at the pass's end); readers use marks 1..4. A
//     writer needs the read marks only to restart a completely backfilled
//     log, which again cannot be the case.
//   - no write lock, no read transaction of its own, no Go lock of the
//     store: the pause applies only to PASSIVE passes run without the
//     writer (never to a TRUNCATE, never to the writer step).
//
// Bounds:
//   - a pass stops pausing after walCopyPauseMax of pausing in total (it then
//     copies under the budget below, lane or not), and a leaked announcement
//     stops counting as busy after editIntentStandDownMax anyway;
//   - between edits, while the checkout is being edited (the lane was busy
//     within walCopyEditSession), the copy is limited to walCopyBudget bytes
//     written to the database file per minute; idle, it runs at full speed;
//   - shutdown and a bulk window end any pause at once (the statement is
//     then interrupted as before; the copy loop stops at its next page).
//
// The pacer is found by the libc TLS of the connection that runs the
// statement: modernc passes the connection's TLS down to every VFS call, so
// the wrapper knows whose write it is without any SQLite-side state.

var (
	// walCopyPauseMax bounds how long one pass may pause in total.
	walCopyPauseMax = 3 * time.Minute
	// walCopyPausePoll is how often a paused write rechecks the lane.
	walCopyPausePoll = 2 * time.Millisecond
	// walCopyEditSession: the lane was busy this recently, so the checkout is
	// being edited and the budget applies.
	walCopyEditSession = time.Minute
	// walCopyBudgetBytesPerMinute is the default copy budget while editing:
	// 512 MiB written to the database file per minute. At the measured cost
	// of the copy (about 75 MB per CPU-second, nearly all of it pwrite) that
	// is about 7 s of CPU per minute, 12% of one core.
	walCopyBudgetBytesPerMinute = atomicInt64(512 << 20)
	// walCopyBudgetBurst is the bucket's depth.
	walCopyBudgetBurst = atomicInt64(64 << 20)
	// walCopyStartMinTokens: while editing, a pass starts only with this
	// much budget in the bucket (the guard for a pass that would only stall).
	walCopyStartMinTokens = atomicInt64(16 << 20)
)

// walCopyBudget resolves GORTEX_SQLITE_WAL_COPY_BUDGET_MB (MiB per minute
// while editing; 0 removes the limit).
func walCopyBudget() int64 {
	if raw := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_WAL_COPY_BUDGET_MB")); raw != "" {
		if mb, err := strconv.ParseInt(raw, 10, 64); err == nil && mb >= 0 {
			return min(mb, 1<<20) << 20
		}
	}
	return walCopyBudgetBytesPerMinute.Load()
}

func walCopyPauseEnabled() bool {
	v := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_WAL_COPY_PAUSE"))
	return v != "0" && !strings.EqualFold(v, "false")
}

// walCopyState is the store's copy budget and counters, shared by the
// reclaim's attempts.
type walCopyState struct {
	mu        sync.Mutex
	tokens    float64
	refilled  time.Time
	lastBusy  atomic.Int64 // unix nanos of the last time the lane was seen busy
	passes    atomic.Int64 // pausable passes run
	paused    atomic.Int64 // passes that paused at least once
	pauseNs   atomic.Int64
	budgetNs  atomic.Int64
	written   atomic.Int64 // bytes the paced passes wrote to the database file
	whileBusy atomic.Int64 // bytes written while the lane was busy (after the pause cap)
	overruns  atomic.Int64 // passes that hit walCopyPauseMax
	discarded atomic.Int64 // attempts that ended with a pass cut short (any pass, any cause)
	// Pressure mode (wal_reclaim_pressure.go): attempts run through a busy
	// lane, resets taken inside it, and resets given up for the hold cap.
	pressureRuns, pressureResets, pressureGiveUps atomic.Int64
	// pressureLaneResets: of pressureResets, those taken inside a busy lane
	// (the others found a gap by the time they reached the writer).
	pressureLaneResets atomic.Int64
	// pacedDiscarded: paced passes themselves cut short (shutdown, a bulk
	// window); the pause exists so that an edit never does this.
	pacedDiscarded atomic.Int64
}

func (c *walCopyState) sawBusy(now time.Time) { c.lastBusy.Store(now.UnixNano()) }

func (c *walCopyState) editing(now time.Time) bool {
	last := c.lastBusy.Load()
	return last != 0 && now.Sub(time.Unix(0, last)) < walCopyEditSession
}

// take spends n bytes of budget, returning how long the caller must wait
// first (0 when the bucket covers it). rate is bytes per second.
func (c *walCopyState) take(now time.Time, n int64, rate float64) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	burst := float64(walCopyBudgetBurst.Load())
	if c.refilled.IsZero() {
		c.tokens, c.refilled = burst, now
	}
	c.tokens = min(burst, c.tokens+now.Sub(c.refilled).Seconds()*rate)
	c.refilled = now
	c.tokens -= float64(n)
	if c.tokens >= 0 {
		return 0
	}
	return time.Duration(-c.tokens / rate * float64(time.Second))
}

// available reports the bucket's content now (bytes).
func (c *walCopyState) available(now time.Time, rate float64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	burst := float64(walCopyBudgetBurst.Load())
	if c.refilled.IsZero() {
		return int64(burst)
	}
	return int64(min(burst, c.tokens+now.Sub(c.refilled).Seconds()*rate))
}

// walCopyPacer paces one pass. It runs on the goroutine executing the
// checkpoint statement (the VFS write is called there), so its own fields
// need no locking.
type walCopyPacer struct {
	store    *Store
	ctx      context.Context
	stop     <-chan struct{}
	rate     float64 // budget, bytes per second; 0 = none
	pauseNs  time.Duration
	budgetNs time.Duration
	pauses   int
	written  int64
	overran  bool
	// hardCapBytes/walPath: see walCopyAttempt.
	hardCapBytes int64
	walPath      string
	// noPause: a pressure attempt's pass is paced by the budget, never paused.
	noPause bool
}

func (p *walCopyPacer) stopped() bool {
	if p.ctx.Err() != nil {
		return true
	}
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

// beforeWrite runs before each page write of the pass. The lane is checked
// once per page, so an edit that begins between the check and the write lets
// that one page through; every later page waits.
func (p *walCopyPacer) beforeWrite(n int64) {
	st := p.store
	busy := st.buildLaneBusy()
	if busy && p.hardCapBytes > 0 && !p.overran && walFileSize(p.walPath) >= p.hardCapBytes {
		// Already over the mark: the pass does not pause at all.
		p.overran = true
	}
	if busy && !p.noPause && !p.overran && !p.stopped() && !walCopyInterruptInsteadOfPause {
		start := time.Now()
		p.pauses++
		for polls := 0; st.buildLaneBusy() && !p.stopped(); polls++ {
			if p.pauseNs+time.Since(start) >= walCopyPauseMax {
				p.overran = true
				st.walCopy.overruns.Add(1)
				log.Printf("store_sqlite: wal reclaim copy resumed despite the lane after pausing %s (pause cap)", walCopyPauseMax)
				break
			}
			if p.hardCapBytes > 0 && polls%25 == 0 && walFileSize(p.walPath) >= p.hardCapBytes {
				p.overran = true
				st.walCopy.overruns.Add(1)
				log.Printf("store_sqlite: wal reclaim copy resumed despite the lane at the pressure mark wal_bytes=%d mark=%d", walFileSize(p.walPath), p.hardCapBytes)
				break
			}
			time.Sleep(walCopyPausePoll)
		}
		p.pauseNs += time.Since(start)
		st.walCopy.sawBusy(time.Now())
		// Still busy only if the pause ended for cause (cap, shutdown).
		busy = st.buildLaneBusy()
	}
	now := time.Now()
	if busy {
		st.walCopy.sawBusy(now)
		st.walCopy.whileBusy.Add(n)
	}
	if p.rate > 0 && st.walCopy.editing(now) && !p.stopped() {
		if wait := st.walCopy.take(now, n, p.rate); wait > 0 {
			deadline := now.Add(wait)
			for time.Now().Before(deadline) && !p.stopped() {
				time.Sleep(min(time.Until(deadline), 10*time.Millisecond))
			}
			p.budgetNs += time.Since(now)
		}
	}
	p.written += n
}

// The xWrite wrapper, installed once per process on the io_methods table the
// store's database files use.

var (
	walCopyPacers       sync.Map // uintptr(*libc.TLS) → *walCopyPacer
	walCopyPacersActive atomic.Int32
	walCopyInstallOnce  sync.Once
	walCopyMethods      atomic.Uintptr // the io_methods table wrapped (0: none)
	walCopyWriteOrig    atomic.Uintptr // its original xWrite
	// walCopyInterruptInsteadOfPause restores the old behaviour (the lane
	// watcher interrupts the pass, nothing pauses) for the mutation checks
	// that prove the pause carries the result. Never set in production.
	walCopyInterruptInsteadOfPause = false
	// walCopyPassObserver, when set by a test, is told every paced pass: its
	// span and how many times it paused. nil in production.
	walCopyPassObserver func(start, end time.Time, pauses int)
)

var walCopyWriteWrapper = func(tls *libc.TLS, pFile, pBuf uintptr, iAmt int32, iOfst int64) int32 {
	if walCopyPacersActive.Load() > 0 {
		if v, ok := walCopyPacers.Load(uintptr(unsafe.Pointer(tls))); ok {
			v.(*walCopyPacer).beforeWrite(int64(iAmt))
		}
	}
	// Only files of the wrapped table call here, so its original is the one.
	fp := walCopyWriteOrig.Load()
	return (*(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&struct{ uintptr }{fp})))(tls, pFile, pBuf, iAmt, iOfst)
}

// funcPointer is the C function pointer modernc's generated code calls for a
// Go func value: the data word of an interface holding it (the same encoding
// as the generated __ccgo_fp).
func funcPointer(f any) uintptr {
	type iface [2]uintptr
	return (*iface)(unsafe.Pointer(&f))[1]
}

func fileAt(p uintptr) *sqlite3.Tsqlite3_file {
	return *(**sqlite3.Tsqlite3_file)(unsafe.Pointer(&p))
}

func ioMethodsAt(p uintptr) *sqlite3.Tsqlite3_io_methods {
	return *(**sqlite3.Tsqlite3_io_methods)(unsafe.Pointer(&p))
}

func uintptrAt(p uintptr) uintptr {
	return **(**uintptr)(unsafe.Pointer(&p))
}

func setUintptrAt(p, v uintptr) {
	**(**uintptr)(unsafe.Pointer(&p)) = v
}

// driverConnHandles reads a modernc connection's sqlite3* and libc TLS.
func driverConnHandles(dc any) (db uintptr, tls *libc.TLS, ok bool) {
	rv := reflect.ValueOf(dc)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return 0, nil, false
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return 0, nil, false
	}
	dbf, tlsf := rv.FieldByName("db"), rv.FieldByName("tls")
	if !dbf.IsValid() || !tlsf.IsValid() || dbf.Kind() != reflect.Uintptr || tlsf.Kind() != reflect.Pointer || tlsf.IsNil() {
		return 0, nil, false
	}
	return uintptr(dbf.Uint()), (*libc.TLS)(tlsf.UnsafePointer()), true
}

// mainFileMethods returns the io_methods table of the connection's main
// database file.
func mainFileMethods(db uintptr, tls *libc.TLS) (uintptr, error) {
	name, err := libc.CString("main")
	if err != nil {
		return 0, err
	}
	defer libc.Xfree(tls, name)
	out := tls.Alloc(8)
	defer tls.Free(8)
	setUintptrAt(out, 0)
	if rc := sqlite3.Xsqlite3_file_control(tls, db, name, sqlite3.SQLITE_FCNTL_FILE_POINTER, out); rc != sqlite3.SQLITE_OK {
		return 0, fmt.Errorf("file_control(FILE_POINTER) rc=%d", rc)
	}
	pFile := uintptrAt(out)
	if pFile == 0 {
		return 0, fmt.Errorf("no main file")
	}
	return fileAt(pFile).FpMethods, nil
}

// installWALCopyPause wraps xWrite of the io_methods table a database file in
// the store's directory kind opens with, once per process, before the first
// store connection opens (a probe database in a temporary directory supplies
// the table).
func installWALCopyPause() {
	walCopyInstallOnce.Do(func() {
		if !walCopyPauseEnabled() {
			log.Printf("store_sqlite: wal copy pause off (GORTEX_SQLITE_WAL_COPY_PAUSE)")
			return
		}
		dir, err := os.MkdirTemp("", "gortex-wal-copy-probe-")
		if err != nil {
			log.Printf("store_sqlite: wal copy pause not installed: %v", err)
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		db, err := sql.Open("sqlite", filepath.Join(dir, "probe.sqlite"))
		if err != nil {
			log.Printf("store_sqlite: wal copy pause not installed: %v", err)
			return
		}
		defer func() { _ = db.Close() }()
		ctx := context.Background()
		conn, err := db.Conn(ctx)
		if err != nil {
			log.Printf("store_sqlite: wal copy pause not installed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.ExecContext(ctx, `PRAGMA user_version`); err != nil {
			log.Printf("store_sqlite: wal copy pause not installed: %v", err)
			return
		}
		var methods uintptr
		err = conn.Raw(func(dc any) error {
			h, tls, ok := driverConnHandles(dc)
			if !ok {
				return fmt.Errorf("unrecognised driver connection %T", dc)
			}
			methods, err = mainFileMethods(h, tls)
			return err
		})
		if err != nil || methods == 0 {
			log.Printf("store_sqlite: wal copy pause not installed: %v", err)
			return
		}
		m := ioMethodsAt(methods)
		wrapper := funcPointer(walCopyWriteWrapper)
		if m.FxWrite == 0 || m.FxWrite == wrapper {
			return
		}
		walCopyWriteOrig.Store(m.FxWrite)
		m.FxWrite = wrapper
		walCopyMethods.Store(methods)
		// The same table's xFetch (io_methods version 3) counts the pages
		// served from the memory map (ReaderWaitMark.MappedPages).
		if m.FiVersion >= 3 && m.FxFetch != 0 {
			sqliteFetchOrig.Store(m.FxFetch)
			m.FxFetch = funcPointer(sqliteFetchWrapper)
		}
		log.Printf("store_sqlite: wal copy pause installed")
	})
}

// pausableConn reports the TLS key of conn when its main database file writes
// through the wrapped table (so a pacer registered under it sees every page
// the checkpoint copies).
func pausableConn(conn *sql.Conn) (uintptr, bool) {
	wrapped := walCopyMethods.Load()
	if wrapped == 0 {
		return 0, false
	}
	var key uintptr
	var match bool
	_ = conn.Raw(func(dc any) error {
		h, tls, ok := driverConnHandles(dc)
		if !ok {
			return nil
		}
		methods, err := mainFileMethods(h, tls)
		if err != nil {
			return nil
		}
		key, match = uintptr(unsafe.Pointer(tls)), methods == wrapped
		return nil
	})
	return key, match
}

// walCopyAttempt carries an attempt's pacing: whether its passes may pause and
// what they did.
type walCopyAttempt struct {
	pausable atomic.Bool
	// pressure: the attempt runs through a busy lane (never pauses, and its
	// passes start inside edits); set before its first pass.
	pressure bool
	// hardCapBytes: a pause ends when the log reaches the reclaim's hard cap
	// (walPath is the log), so a long edit cannot hold the one copy that
	// would bring the log down past the bound the hard cap guarantees.
	hardCapBytes int64
	walPath      string
	copying      atomic.Bool // a paced pass is running (the lane watcher must not cancel it)
	pauses       int
	pauseNs      time.Duration
	budgetNs     time.Duration
	written      int64
	passes       int
	// discarded counts this attempt's paced passes that were cut short.
	discarded int
}

func (a *walCopyAttempt) suffix() string {
	if a == nil || a.passes == 0 {
		return ""
	}
	return fmt.Sprintf(" copy_passes=%d copy_pauses=%d copy_paused=%s copy_budget_wait=%s copy_written_bytes=%d copy_passes_cut_short=%d",
		a.passes, a.pauses, a.pauseNs.Round(time.Millisecond), a.budgetNs.Round(time.Millisecond), a.written, a.discarded)
}

// pacedPassive runs one writer-free PASSIVE pass whose page writes pause for
// edits (see the file comment). Without the wrapper, or for an attempt that
// may not pause, it is the plain statement.
func (s *Store) pacedPassive(ctx context.Context, ckptDB *sql.DB, attempt *backgroundCheckpointAttempt) (result walCheckpointResult, err error) {
	if attempt == nil || attempt.copy == nil || !attempt.copy.pausable.Load() || walCopyMethods.Load() == 0 {
		return checkpointWALOnceOn(ctx, ckptDB, "PASSIVE")
	}
	// A pass never starts inside an edit: the copy would only pause at once
	// while holding the checkpointer lock. The attempt's lane watcher ends
	// the attempt instead.
	if !attempt.copy.pressure && s.buildLaneBusy() {
		return walCheckpointResult{}, errWALCheckpointYieldedToCycle
	}
	conn, cerr := ckptDB.Conn(ctx)
	if cerr != nil {
		return walCheckpointResult{}, cerr
	}
	defer func() { _ = conn.Close() }()
	key, ok := pausableConn(conn)
	if !ok {
		return checkpointWALOnceOn(ctx, conn, "PASSIVE")
	}
	pacer := &walCopyPacer{store: s, ctx: ctx, stop: s.stopCheckpoint, hardCapBytes: attempt.copy.hardCapBytes, walPath: attempt.copy.walPath, noPause: attempt.copy.pressure}
	if b := walCopyBudget(); b > 0 {
		pacer.rate = float64(b) / 60
	}
	defer func() {
		if err == nil {
			return
		}
		// Cut short? SQLite records the attempt before copying and the
		// backfill only when the pass completes.
		if snap, ok := readWALIndexSnapshot(s.dbPath); ok && snap.NBackfillAttempted > snap.NBackfill {
			attempt.copy.discarded++
			s.walCopy.pacedDiscarded.Add(1)
		}
	}()
	if observe := walCopyPassObserver; observe != nil {
		started := time.Now()
		defer func() { observe(started, time.Now(), pacer.pauses) }()
	}
	walCopyPacers.Store(key, pacer)
	walCopyPacersActive.Add(1)
	if !walCopyInterruptInsteadOfPause {
		attempt.copy.copying.Store(true)
	}
	defer func() {
		attempt.copy.copying.Store(false)
		walCopyPacersActive.Add(-1)
		walCopyPacers.Delete(key)
		c := attempt.copy
		c.passes++
		c.pauses += pacer.pauses
		c.pauseNs += pacer.pauseNs
		c.budgetNs += pacer.budgetNs
		c.written += pacer.written
		s.walCopy.passes.Add(1)
		if pacer.pauses > 0 {
			s.walCopy.paused.Add(1)
		}
		s.walCopy.pauseNs.Add(int64(pacer.pauseNs))
		s.walCopy.budgetNs.Add(int64(pacer.budgetNs))
		s.walCopy.written.Add(pacer.written)
	}()
	result, err = checkpointWALOnceOn(ctx, conn, "PASSIVE")
	if err != nil && errors.Is(err, errSQLiteCheckpointIncomplete) {
		// A reader's mark limited the pass: it completed what it could.
		return result, err
	}
	return result, err
}

// copyStartAllowed is the guard for starting a pass while the checkout is
// being edited: the budget must hold enough for the pass to make progress
// rather than stall in the bucket. Idle, a pass always starts.
func (s *Store) copyStartAllowed(now time.Time) bool {
	if !s.walCopy.editing(now) {
		return true
	}
	b := walCopyBudget()
	if b <= 0 {
		return true
	}
	return s.walCopy.available(now, float64(b)/60) >= walCopyStartMinTokens.Load()
}

// WALCopyStats is the paced copy's counters.
type WALCopyStats struct {
	Passes, PausedPasses, PauseCapOverruns, DiscardedPasses, PacedPassesCutShort int64
	PressureRuns, PressureResets, PressureGiveUps, PressureLaneResets            int64
	Paused, BudgetWait                                                           time.Duration
	WrittenBytes, WrittenWhileBusyBytes                                          int64
}

// WALCopyStats returns the paced copy's counters.
func (s *Store) WALCopyStats() WALCopyStats {
	if s.coreless() {
		return WALCopyStats{}
	}
	c := &s.walCopy
	return WALCopyStats{
		Passes: c.passes.Load(), PausedPasses: c.paused.Load(), PauseCapOverruns: c.overruns.Load(), DiscardedPasses: c.discarded.Load(), PacedPassesCutShort: c.pacedDiscarded.Load(),
		PressureRuns: c.pressureRuns.Load(), PressureResets: c.pressureResets.Load(), PressureGiveUps: c.pressureGiveUps.Load(),
		PressureLaneResets: c.pressureLaneResets.Load(),
		Paused:             time.Duration(c.pauseNs.Load()), BudgetWait: time.Duration(c.budgetNs.Load()),
		WrittenBytes: c.written.Load(), WrittenWhileBusyBytes: c.whileBusy.Load(),
	}
}

// sqliteMappedPages counts pages SQLite took from the memory map, process-wide.
var (
	sqliteMappedPages atomic.Int64
	sqliteFetchOrig   atomic.Uintptr
)

var sqliteFetchWrapper = func(tls *libc.TLS, pFile uintptr, iOfst int64, iAmt int32, pp uintptr) int32 {
	fp := sqliteFetchOrig.Load()
	rc := (*(*func(*libc.TLS, uintptr, int64, int32, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{fp})))(tls, pFile, iOfst, iAmt, pp)
	if rc == sqlite3.SQLITE_OK && pp != 0 && uintptrAt(pp) != 0 {
		sqliteMappedPages.Add(1)
	}
	return rc
}

// atomicInt64 is an atomic.Int64 holding v: the budget settings are read by
// the reclaim loop while the in-package cases change them.
func atomicInt64(v int64) *atomic.Int64 {
	a := new(atomic.Int64)
	a.Store(v)
	return a
}
