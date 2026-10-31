package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gofrs/flock"

	"github.com/zzet/gortex/internal/daemon"
)

// daemonDecision is the resolved auto-start outcome.
type daemonDecision int

const (
	daemonReady       daemonDecision = iota // socket was already live
	daemonAutostarted                       // we (or a peer) brought it up; socket live
	daemonUnavailable                       // autostart off, or spawn failed/timed out
)

const (
	// spawnLockInactivityTimeout is deliberately an inactivity budget, not a
	// total startup budget. The lock winner holds the spawn lock until the
	// detached child's socket is live; a schema migration can legitimately keep
	// that socket closed for longer than a minute. Fresh PID-bound startup state
	// extends this deadline in the same way spawnDetachedDaemon extends its own
	// wait, while a missing/stale heartbeat still releases callers promptly.
	spawnLockInactivityTimeout = 60 * time.Second
	spawnLockPollInterval      = 100 * time.Millisecond
	spawnProgressFreshness     = 10 * time.Second
	// spawnFailCooldown bounds serial retries of a broken spawn: within
	// the window at most one spawn attempt runs across contending callers.
	spawnFailCooldown = 5 * time.Second
)

type spawnLockTryer interface {
	TryLock() (bool, error)
}

type spawnLockWaitOptions struct {
	now             func() time.Time
	wait            func(context.Context, time.Duration) error
	daemonRunning   func() bool
	startupProgress func(time.Time) bool
	inactivity      time.Duration
	poll            time.Duration
}

// waitForSpawnLock acquires the cross-process spawn lock without turning a
// healthy long migration into an autostart failure for every losing caller.
// It has no unbounded silent wait: only a fresh, live daemon startup heartbeat
// renews the inactivity deadline. Context cancellation, a live socket, a stale
// heartbeat, and a lock-holder exit all remain terminal edges.
func waitForSpawnLock(ctx context.Context, lock spawnLockTryer, opts spawnLockWaitOptions) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.wait == nil {
		opts.wait = waitSpawnLockInterval
	}
	if opts.daemonRunning == nil {
		opts.daemonRunning = func() bool { return false }
	}
	if opts.startupProgress == nil {
		opts.startupProgress = func(time.Time) bool { return false }
	}
	if opts.inactivity <= 0 {
		opts.inactivity = spawnLockInactivityTimeout
	}
	if opts.poll <= 0 {
		opts.poll = spawnLockPollInterval
	}

	now := opts.now()
	deadline := now.Add(opts.inactivity)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		locked, err := lock.TryLock()
		if err != nil {
			return false, err
		}
		if locked {
			return true, nil
		}
		if opts.daemonRunning() {
			return false, nil
		}

		now = opts.now()
		if opts.startupProgress(now) {
			deadline = now.Add(opts.inactivity)
		}
		if !now.Before(deadline) {
			return false, context.DeadlineExceeded
		}
		pause := opts.poll
		if remaining := deadline.Sub(now); pause > remaining {
			pause = remaining
		}
		if err := opts.wait(ctx, pause); err != nil {
			return false, err
		}
	}
}

func waitSpawnLockInterval(ctx context.Context, pause time.Duration) error {
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func daemonStartupHeartbeatFresh(now time.Time) bool {
	state, ok := daemon.ReadRuntimeState()
	return ok && state.StartupProgressFresh(now, spawnProgressFreshness)
}

// Injectable seams so the race/fallback/spawn-failure branches are
// testable without a real daemon.
var (
	isDaemonRunning   = daemon.IsRunning
	spawnDaemon       = spawnBareDaemon
	stopIntentActive  = daemon.StopIntentActive
	runningDaemonInfo = dialRunningDaemonIdentity
	binaryVersion     = canonicalVersion
	stopStaleDaemon   = stopDaemonForHandoff
)

// spawnBareDaemon is the autostart default for spawnDaemon. Autostart has no
// `daemon start` flags to forward — it is `gortex mcp` / `gortex track`
// bringing up a default daemon, not a user-typed start — so the child argv
// stays bare and the daemon resolves everything from config.
func spawnBareDaemon() error { return spawnDetachedDaemon(nil) }

// resolveDaemonDecision probes the socket and, when auto-start is enabled
// and no daemon is up, single-flights a spawn. It never returns an error;
// an unrecoverable state collapses to daemonUnavailable so the caller can
// apply the machine-level embedded-mode policy.
func resolveDaemonDecision() daemonDecision {
	return ensureDaemonReady(daemon.ParseAutostart())
}

// ensureDaemonReady is the lock-protected single-flight critical section,
// shared by `gortex mcp` and `gortex track`.
func ensureDaemonReady(autostart bool) daemonDecision {
	decision, _, _ := ensureDaemonReadyWithError(autostart)
	return decision
}

// MCP must retain a failed mismatch handoff as an error, even if the old
// daemon disappeared: it must never turn that failure into embedded startup.
func ensureDaemonReadyWithError(autostart bool) (daemonDecision, daemonIdentity, error) {
	if isDaemonRunning() {
		identity := runningDaemonInfo()
		if autostart && identity.olderThan(binaryVersion()) {
			decision, observedMismatch, err := replaceStaleDaemon(identity)
			if observedMismatch.mismatch() {
				return decision, observedMismatch, err
			}
			// Compatible release handoffs retain the existing policy: a
			// failed stop can continue using the old compatible daemon, and
			// a failed absent spawn can use the configured fallback. A new
			// protocol refusal observed during replacement stays terminal.
			if !errors.Is(err, daemon.ErrProtocolVersionMismatch) {
				err = nil
			}
			return decision, daemonIdentity{}, err
		}
		if identity.mismatch() {
			return daemonReady, identity, daemonMismatchError(identity)
		}
		return daemonReady, daemonIdentity{}, nil
	}
	if !autostart {
		return daemonUnavailable, daemonIdentity{}, nil
	}
	// Respect an explicit `daemon stop`: do not resurrect a daemon the user
	// deliberately stopped. The mark is cleared by `daemon start` / `restart`.
	// A suppressed `gortex mcp` either uses an explicitly allowed embedded
	// server or exits, so this never overrides the user's stay-down intent.
	if stopIntentActive() {
		return daemonUnavailable, daemonIdentity{}, nil
	}

	lockPath := daemon.SpawnLockPath()
	_ = os.MkdirAll(filepath.Dir(lockPath), 0o700)
	lock := flock.New(lockPath)

	locked, err := waitForSpawnLock(context.Background(), lock, spawnLockWaitOptions{
		daemonRunning:   isDaemonRunning,
		startupProgress: daemonStartupHeartbeatFresh,
	})
	if err != nil || !locked {
		// Couldn't acquire the lock in time. The winner may have brought
		// the socket up while we waited — re-probe before giving up.
		if isDaemonRunning() {
			return daemonReady, daemonIdentity{}, nil
		}
		return daemonUnavailable, daemonIdentity{}, nil
	}
	defer func() { _ = lock.Unlock() }()

	// Re-probe inside the lock: a peer may have won the race while we
	// blocked on the lock.
	if isDaemonRunning() {
		return daemonReady, daemonIdentity{}, nil
	}
	// A recent failed spawn within the cooldown — skip our own attempt so
	// K callers don't serially retry a broken spawn.
	if spawnFailedRecently() {
		return daemonUnavailable, daemonIdentity{}, nil
	}
	if err := spawnDaemon(); err != nil {
		stampSpawnFailure()
		return daemonUnavailable, daemonIdentity{}, nil
	}
	return daemonAutostarted, daemonIdentity{}, nil
}

// replaceStaleDaemon swaps a live daemon that is older than this binary for
// one started from it. Upgrades used to do this from the Homebrew cask's
// postflight hook; doing it here covers every install channel and runs
// outside any package-manager sandbox, under the user's real HOME.
//
// It runs under the spawn lock so concurrent callers (several editors
// reconnecting after an upgrade) replace the daemon once: losers wait for
// the lock, then re-check and find the new daemon current. Any failure to
// stop leaves the old daemon serving — the proxy's skew warning still tells
// the user to restart it.
func replaceStaleDaemon(observed daemonIdentity) (daemonDecision, daemonIdentity, error) {
	var observedMismatch daemonIdentity
	if observed.mismatch() {
		observedMismatch = observed
	}
	lockPath := daemon.SpawnLockPath()
	_ = os.MkdirAll(filepath.Dir(lockPath), 0o700)
	lock := flock.New(lockPath)

	locked, err := waitForSpawnLock(context.Background(), lock, spawnLockWaitOptions{
		startupProgress: daemonStartupHeartbeatFresh,
	})
	if err != nil || !locked {
		if err == nil {
			err = context.DeadlineExceeded
		}
		return daemonUnavailable, observedMismatch, daemonHandoffError(observed, "wait for spawn lock", err)
	}
	defer func() { _ = lock.Unlock() }()

	identity := runningDaemonInfo()
	if identity.mismatch() {
		observedMismatch = identity
	}
	if !isDaemonRunning() || !identity.olderThan(binaryVersion()) {
		// A peer replaced it (or it went away) while we waited for the lock.
		if identity.mismatch() {
			return daemonReady, observedMismatch, daemonMismatchError(identity)
		}
		if isDaemonRunning() {
			return daemonReady, observedMismatch, nil
		}
		return daemonUnavailable, observedMismatch, daemonHandoffError(observed, "recheck live daemon", daemon.ErrDaemonUnavailable)
	}
	// A supervised daemon is bounced through its supervisor, as `daemon
	// restart` does; a manual stop+start would orphan it from the unit.
	if serviceActive() {
		// The external supervisor query can outlive a concurrent daemon swap.
		// Recheck its direction before asking the supervisor to restart it.
		current := runningDaemonInfo()
		if current.mismatch() {
			observedMismatch = current
		}
		if current.err != nil && !current.mismatch() {
			return daemonUnavailable, observedMismatch, daemonHandoffError(identity, "recheck supervised daemon", current.err)
		}
		if !current.olderThan(binaryVersion()) {
			if current.mismatch() {
				return daemonReady, observedMismatch, daemonMismatchError(current)
			}
			return daemonReady, observedMismatch, nil
		}
		identity = current
		if err := serviceRestart(io.Discard); err != nil {
			return daemonReady, observedMismatch, daemonHandoffError(identity, "supervisor restart", err)
		}
		current, duringWait, err := waitForReplacementDaemon()
		if duringWait.mismatch() {
			observedMismatch = duringWait
		}
		if err != nil {
			failureIdentity := identity
			if duringWait.mismatch() {
				failureIdentity = duringWait
			}
			return daemonReady, observedMismatch, daemonHandoffError(failureIdentity, "wait for supervised replacement", err)
		}
		logDaemonHandoff(identity, current)
		return daemonReady, observedMismatch, nil
	}
	if err := stopStaleDaemon(identity); err != nil {
		if errors.Is(err, daemon.ErrProtocolVersionMismatch) {
			observedMismatch = daemonIdentityFromMismatch(err)
			return daemonReady, observedMismatch, daemonHandoffError(observedMismatch, "stop old daemon", err)
		}
		return daemonReady, observedMismatch, daemonHandoffError(identity, "stop old daemon", err)
	}
	// The store-lock wait may have allowed an independently started daemon
	// to appear. Revalidate under the spawn lock before starting anything.
	current := runningDaemonInfo()
	if current.mismatch() {
		observedMismatch = current
	}
	if current.err == nil {
		if !current.olderThan(binaryVersion()) {
			logDaemonHandoff(identity, current)
			return daemonReady, observedMismatch, nil
		}
		return daemonReady, observedMismatch, daemonHandoffError(identity, "old daemon still running", fmt.Errorf("shutdown did not retire the old daemon"))
	}
	if !errors.Is(current.err, daemon.ErrDaemonUnavailable) {
		if current.mismatch() {
			return daemonReady, observedMismatch, daemonMismatchError(current)
		}
		return daemonUnavailable, observedMismatch, daemonHandoffError(identity, "recheck daemon after close", current.err)
	}
	if err := spawnDaemon(); err != nil {
		stampSpawnFailure()
		return daemonUnavailable, observedMismatch, daemonHandoffError(identity, "start replacement", err)
	}
	current, duringWait, err := waitForReplacementDaemon()
	if duringWait.mismatch() {
		observedMismatch = duringWait
	}
	if err != nil {
		failureIdentity := identity
		if duringWait.mismatch() {
			failureIdentity = duringWait
		}
		return daemonUnavailable, observedMismatch, daemonHandoffError(failureIdentity, "wait for replacement", err)
	}
	logDaemonHandoff(identity, current)
	return daemonAutostarted, observedMismatch, nil
}

func spawnFailedRecently() bool {
	info, err := os.Stat(daemon.SpawnFailMarkerPath())
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < spawnFailCooldown
}

func stampSpawnFailure() {
	path := daemon.SpawnFailMarkerPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600)
}

// Only the spawn-lock winner emits this, after the replacement succeeds.
func logDaemonHandoff(previous, current daemonIdentity) {
	fmt.Fprintf(os.Stderr, "[gortex mcp] replaced daemon %s (protocol %s) with %s (protocol %d)\n", daemonVersionLabel(previous.version), previous.protocolLabel(), daemonVersionLabel(current.version), current.protocol)
}
