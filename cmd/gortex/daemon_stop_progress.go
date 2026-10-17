package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The stop path's view of the daemon's closing store.
//
// Close's final checkpoint copies the whole WAL backlog in one pass and
// records nothing until the pass ends; killing it discards the copy and the
// next open starts over (restart #8, 2026-09-25: killed after 211 s with a
// 22.4 GB WAL untouched). The store reports its close in a progress file
// (store_sqlite.CloseProgress). Past its grace, the stop keeps waiting while
// that report shows fresh progress and force-kills only after
// daemonCloseNoProgressBound without any.

// daemonCloseNoProgressBound is how long the stop waits past its grace with
// no sign of progress from the close before force-killing.
// GORTEX_DAEMON_STOP_NO_PROGRESS_S overrides it.
var daemonCloseNoProgressBound = resolveDaemonCloseNoProgressBound()

func resolveDaemonCloseNoProgressBound() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("GORTEX_DAEMON_STOP_NO_PROGRESS_S")); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 60 * time.Second
}

// daemonCloseProgress reads the closing daemon's report; nil when unknown.
// A seam: the stop tests install a fake close.
var daemonCloseProgress func() (store_sqlite.CloseProgress, bool)

// installDaemonCloseProgress points daemonCloseProgress at the running
// daemon's store and returns the reset.
func installDaemonCloseProgress() func() {
	if daemonCloseProgress != nil {
		return func() {}
	}
	st, ok := daemon.ReadRuntimeState()
	if !ok || strings.TrimSpace(st.BackendPath) == "" {
		return func() {}
	}
	path := st.BackendPath
	daemonCloseProgress = func() (store_sqlite.CloseProgress, bool) { return store_sqlite.ReadCloseProgress(path) }
	return func() { daemonCloseProgress = nil }
}

// daemonCloseProgressFor returns the close report of process pid while it is
// draining.
func daemonCloseProgressFor(pid int) (store_sqlite.CloseProgress, bool) {
	if daemonCloseProgress == nil {
		return store_sqlite.CloseProgress{}, false
	}
	p, ok := daemonCloseProgress()
	if !ok || p.Phase != "draining" || (p.PID != 0 && pid != 0 && p.PID != pid) {
		return store_sqlite.CloseProgress{}, false
	}
	return p, true
}

// describeCloseProgress renders a report for the stop's log lines.
func describeCloseProgress(p store_sqlite.CloseProgress, ok bool, now time.Time) string {
	if !ok {
		return ""
	}
	return fmt.Sprintf(" [close: ~%s of %s frames copied (estimated at %.0f frames/s), ~%s remaining, last progress %s ago]",
		formatPendingFrames(p.FramesCopiedEstimate), formatPendingFrames(p.PendingFrames), p.RateFramesPerSec,
		(time.Duration(p.RemainingMillis) * time.Millisecond).Truncate(time.Second), p.Since(now).Truncate(time.Second))
}
