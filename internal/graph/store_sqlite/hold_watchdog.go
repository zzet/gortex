package store_sqlite

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The hold watchdog names who holds the store's write gate, or a read
// connection's transaction, for longer than a threshold: an edit stalled
// behind either shows only as wall time with no CPU, and the holder is the
// one fact the stalled side cannot see. Opt-in (GORTEX_STORE_HOLD_WATCHDOG,
// a duration such as "2s"); off, the gates record nothing and no goroutine
// runs.

// holdWatchdogThreshold is the hold that is reported; zero disables the
// watchdog.
var holdWatchdogThreshold = parseHoldWatchdogThreshold(os.Getenv("GORTEX_STORE_HOLD_WATCHDOG"))

// holdWatchdogInterval is how often the watchdog looks.
var holdWatchdogInterval = 500 * time.Millisecond

func parseHoldWatchdogThreshold(raw string) time.Duration {
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// holdRecord is one hold: when it began and the program counters of the
// holder's stack at the start. Every write-gate hold and every read
// transaction takes one while the watchdog is on — a delta's reads among
// them — so only the counters are taken; they are resolved to functions and
// lines only for a hold that is reported.
type holdRecord struct {
	since    time.Time
	pcs      [holdRecordDepth]uintptr
	depth    int
	reported atomic.Bool
}

// holdRecordDepth is how many frames of the holder's stack are kept.
const holdRecordDepth = 32

func newHoldRecord() *holdRecord {
	if holdWatchdogThreshold <= 0 {
		return nil
	}
	h := &holdRecord{since: time.Now()}
	h.depth = runtime.Callers(2, h.pcs[:])
	return h
}

// stack renders the holder's frames, innermost first.
func (h *holdRecord) stack() string {
	var b strings.Builder
	frames := runtime.CallersFrames(h.pcs[:h.depth])
	for {
		f, more := frames.Next()
		fmt.Fprintf(&b, "%s\n\t%s:%d\n", f.Function, f.File, f.Line)
		if !more {
			break
		}
	}
	return b.String()
}

var holdWatchdog struct {
	sync.Mutex
	cores map[*storeCore]struct{}
	once  sync.Once
}

// watchHolds registers a store core with the watchdog (when enabled).
func watchHolds(core *storeCore) {
	if holdWatchdogThreshold <= 0 || core == nil {
		return
	}
	holdWatchdog.Lock()
	if holdWatchdog.cores == nil {
		holdWatchdog.cores = map[*storeCore]struct{}{}
	}
	holdWatchdog.cores[core] = struct{}{}
	holdWatchdog.Unlock()
	holdWatchdog.once.Do(func() {
		go func() {
			for {
				time.Sleep(holdWatchdogInterval)
				sweepHolds(time.Now())
			}
		}()
	})
}

// unwatchHolds drops a closed store core.
func unwatchHolds(core *storeCore) {
	holdWatchdog.Lock()
	delete(holdWatchdog.cores, core)
	holdWatchdog.Unlock()
}

// sweepHolds reports, once per hold, every write-gate or read-connection hold
// older than the threshold with the stack that began it.
func sweepHolds(now time.Time) {
	holdWatchdog.Lock()
	cores := make([]*storeCore, 0, len(holdWatchdog.cores))
	for core := range holdWatchdog.cores {
		cores = append(cores, core)
	}
	holdWatchdog.Unlock()
	for _, core := range cores {
		if h := core.writeMu.holder.Load(); h != nil {
			reportHold(now, h, "write gate", "")
		}
		if g := core.readGate; g != nil {
			g.mu.Lock()
			conns := make([]*gatedConn, 0, len(g.conns))
			for c := range g.conns {
				conns = append(conns, c)
			}
			g.mu.Unlock()
			for _, c := range conns {
				if h := c.hold.Load(); h != nil {
					label, _ := c.label.Load().(string)
					reportHold(now, h, "read transaction", label)
				}
			}
		}
	}
}

func reportHold(now time.Time, h *holdRecord, what, label string) {
	held := now.Sub(h.since)
	if held < holdWatchdogThreshold || !h.reported.CompareAndSwap(false, true) {
		return
	}
	log.Printf("store_sqlite: %s held %s (statement %q) by:\n%s", what, held.Round(time.Millisecond), label, h.stack())
}

// endHold logs, for a hold the watchdog reported while it lasted, its total
// when it is released: the report at the threshold says a hold is long, the
// release says how long.
func endHold(h *holdRecord, what string) {
	if h == nil || !h.reported.Load() {
		return
	}
	log.Printf("store_sqlite: %s released after %s (reported at the %s threshold)", what,
		time.Since(h.since).Round(time.Millisecond), holdWatchdogThreshold)
}
