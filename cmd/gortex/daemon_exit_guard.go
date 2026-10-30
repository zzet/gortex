package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"
)

const (
	daemonExitDumpAfter = 2 * time.Second
	daemonExitAfter     = 3 * time.Second
)

const (
	daemonExitPending uint32 = iota
	daemonExitArmed
	daemonExitDisarmed
	daemonExitFailed
	daemonExitClaimed
)

// daemonProcessLifetime exists only inside main, never for an in-process Cobra
// invocation. Main owns the final disarm, after Cobra and terminal restoration.
// This is one foreground daemon's exit guard, not a command finalizer registry.
type daemonProcessLifetime struct{ guard *daemonExitGuard }

var mainDaemonLifetime *daemonProcessLifetime

func (p *daemonProcessLifetime) disarm() {
	if p.guard != nil {
		p.guard.disarm()
	}
}

func installDaemonExitGuard(logPath string, stderr io.Writer) (*daemonExitGuard, error) {
	if mainDaemonLifetime == nil {
		return nil, nil
	}
	if mainDaemonLifetime.guard != nil {
		return nil, errors.New("foreground daemon exit guard already installed")
	}
	// Open before the graph. This independent handle survives closeLog and
	// rotation (which may rename its inode), without reopening any store or log
	// during teardown. It is intentionally owned until process exit: disarm
	// must not wait for a diagnostic writer blocked in filesystem/stderr I/O.
	log, err := openDaemonExitLog(logPath)
	if err != nil {
		err = fmt.Errorf("open shutdown diagnostic log: %w", err)
	}
	g := newDaemonExitGuard(log, stderr, err, os.Exit)
	mainDaemonLifetime.guard = g
	return g, err
}

func openDaemonExitLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Match the daemon logger's existing non-regular-path refusal: opening a
	// stale FIFO for writing must not hang foreground startup.
	info, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("shutdown diagnostic log is not a regular file: %s", path)
	}
	return openDaemonExitLogFile(path)
}

// The two receipts describe actual once-owned closes, not a controller ACK or
// a join deadline. An error is sticky; a later nil cannot certify that owner.
type daemonExitGuard struct {
	mu              sync.Mutex
	shared, runtime bool
	dump, deadline  *time.Timer
	state           atomic.Uint32
	exitCode        atomic.Int32
	diagnose        func()
	exit            func(int)
}

func newDaemonExitGuard(log *os.File, stderr io.Writer, sinkErr error, exit func(int)) *daemonExitGuard {
	g := &daemonExitGuard{exit: exit}
	g.diagnose = func() { dumpDaemonExit(log, stderr, sinkErr) }
	return g
}

func (g *daemonExitGuard) sharedClosed(err error)  { g.observe(true, err) }
func (g *daemonExitGuard) runtimeClosed(err error) { g.observe(false, err) }

func (g *daemonExitGuard) observe(shared bool, err error) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.state.Store(daemonExitFailed)
		g.stopTimersLocked()
		return
	}
	if shared {
		g.shared = true
	} else {
		g.runtime = true
	}
	if !g.shared || !g.runtime || !g.state.CompareAndSwap(daemonExitPending, daemonExitArmed) {
		return
	}
	// Arm the independent exit FIRST. Diagnostic writes/Sync can block and
	// never gate this callback; no logger, file or stderr I/O precedes exit.
	g.deadline = time.AfterFunc(daemonExitAfter, func() {
		if g.state.CompareAndSwap(daemonExitArmed, daemonExitClaimed) {
			g.exit(int(g.exitCode.Load()))
		}
	})
	g.dump = time.AfterFunc(daemonExitDumpAfter, func() {
		if g.state.Load() == daemonExitArmed {
			g.diagnose()
		}
	})
}

func (g *daemonExitGuard) setExitCode(err error) {
	if g == nil || err == nil {
		return
	}
	code := 1
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		code = coded.ExitCode()
	}
	g.exitCode.Store(int32(code))
}

func (g *daemonExitGuard) disarm() {
	if g == nil {
		return
	}
	g.state.Store(daemonExitDisarmed)
	g.mu.Lock()
	g.stopTimersLocked()
	g.mu.Unlock()
	// Never join diagnose here. A blocked best-effort sink must not prevent
	// normal main completion; the OS closes the process-owned log descriptor.
}

func (g *daemonExitGuard) stopTimersLocked() {
	if g.dump != nil {
		g.dump.Stop()
	}
	if g.deadline != nil {
		g.deadline.Stop()
	}
}

func dumpDaemonExit(log *os.File, stderr io.Writer, sinkErr error) {
	var b bytes.Buffer
	b.WriteString("gortex: shared store and owned runtime closed; outer shutdown still pending\n")
	if err := pprof.Lookup("goroutine").WriteTo(&b, 2); err != nil {
		return
	}
	// Best-effort writes are sequential: a blocked log write/Sync can prevent
	// reaching stderr. Only the EXIT TIMER is independent. A completion marker
	// means the entire snapshot reached both sinks and the retained log was
	// synced; blocked or failed output leaves no completion marker.
	var logErr error
	if sinkErr == nil && log != nil {
		_, logErr = log.Write(b.Bytes())
		logErr = errors.Join(logErr, log.Sync())
	} else {
		logErr = errors.Join(sinkErr, errors.New("shutdown diagnostic log unavailable"))
	}
	_, stderrErr := io.Copy(stderr, bytes.NewReader(b.Bytes()))
	if logErr != nil || stderrErr != nil {
		return
	}
	const complete = "gortex: shutdown goroutine dump complete\n"
	if _, err := io.WriteString(log, complete); err != nil {
		return
	}
	if err := log.Sync(); err != nil {
		return
	}
	_, _ = io.WriteString(stderr, complete)
}
