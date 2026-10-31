package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

var (
	daemonCapturedProcessRunning   = func(identity platform.ProcessIdentity) (bool, error) { return identity.Running() }
	daemonKillCapturedProcess      = func(identity platform.ProcessIdentity) error { return identity.Kill() }
	daemonTerminateCapturedProcess = func(identity platform.ProcessIdentity) error { return identity.Terminate() }
	daemonDiagnosticTerminate      = func(identity platform.ProcessIdentity) error { return identity.DiagnosticTerminate() }
)

type daemonStopResult struct {
	forced     bool
	diagnostic bool // Actual SIGQUIT sent, not proof that a dump completed.
}

func (t *daemonStopTarget) completeStop(w io.Writer) (bool, error) {
	free, err := t.storeFree()
	if err != nil || !free {
		return false, err
	}
	gone, err := t.runtimeGone()
	if err != nil {
		return false, err
	}
	if !gone {
		running, processErr := t.processRunning()
		if processErr != nil && !errors.Is(processErr, os.ErrProcessDone) {
			return false, processErr
		}
		if running {
			return false, nil // Free store is never a reason to kill a live finalizer.
		}
		if err := t.removeExitedRuntime(); err != nil {
			return false, err
		}
		gone, err = t.runtimeGone()
		if err != nil || !gone {
			return false, err
		}
	}
	// Store and runtime are the completion proof; a parent need not have
	// reaped the PID. Distinguish a real Unix zombie from a still-live tail.
	if running, processErr := t.processRunning(); processErr == nil {
		if running {
			fmt.Fprintln(w, "[gortex daemon] store free and runtime removed; daemon process is still exiting")
		} else if same, matchErr := t.process.Matches(); matchErr == nil && same && (runtime.GOOS == "darwin" || runtime.GOOS == "linux") {
			fmt.Fprintln(w, "[gortex daemon] store free; exited process has not yet been reaped by its parent")
		}
	}
	if t.noStore {
		fmt.Fprintln(w, "[gortex daemon] no store was declared; completion verified by process exit and runtime cleanup")
	}
	return true, nil
}

// waitForDaemonTarget never equates a timeout with completion. Every successful
// poll releases its probe descriptor, and every forced path verifies the
// captured process incarnation before signaling. It never removes unknown or
// replaced runtime files and never kills after an actual store-free proof.
func waitForDaemonTarget(w io.Writer, target *daemonStopTarget, grace time.Duration, draining bool) (daemonStopResult, error) {
	if w == nil {
		w = os.Stderr
	}
	start := time.Now()
	deadline := start.Add(grace)
	nextProgress := start.Add(daemonDrainProgressPeriod)
	extended := false
	for {
		complete, err := target.completeStop(w)
		if complete {
			return daemonStopResult{}, nil
		}
		now := time.Now()
		progress, reporting := daemonCloseProgressFor(target.pid())
		if now.Before(deadline) {
			if (draining || reporting) && daemonDrainProgressPeriod > 0 && !now.Before(nextProgress) {
				fmt.Fprintf(w, "[gortex daemon] still draining the WAL: %s of up to %s%s\n",
					now.Sub(start).Truncate(time.Second), grace.Truncate(time.Second), describeCloseProgress(progress, reporting, now))
				nextProgress = now.Add(daemonDrainProgressPeriod)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// A changed/missing identity is uncertainty, not evidence that a force
		// is necessary or safe. Keep the actual diagnostic for the caller.
		if err != nil {
			return daemonStopResult{}, fmt.Errorf("cannot verify daemon stop completion: %w", err)
		}
		free, err := target.storeFree()
		if err != nil {
			return daemonStopResult{}, fmt.Errorf("cannot verify daemon store ownership: %w", err)
		}
		if free {
			return daemonStopResult{}, errors.New("daemon store is free but its captured runtime cleanup has not completed; process left unsignaled")
		}
		if reporting && progress.Since(now) < daemonCloseNoProgressBound {
			if !extended || !now.Before(nextProgress) {
				fmt.Fprintf(w, "[gortex daemon] close still making progress after %s — waiting (last progress %s ago, giving up after %s without progress)%s\n",
					now.Sub(start).Truncate(time.Second), progress.Since(now).Truncate(time.Second), daemonCloseNoProgressBound, describeCloseProgress(progress, reporting, now))
				extended = true
				nextProgress = now.Add(daemonDrainProgressPeriod)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if reporting {
			fmt.Fprintf(w, "[gortex daemon] close made no progress for %s (bound %s) after %s — force-killing%s\n",
				progress.Since(now).Truncate(time.Second), daemonCloseNoProgressBound, now.Sub(start).Truncate(time.Second), describeCloseProgress(progress, reporting, now))
		}
		break
	}
	diagnostic := false
	if os.Getenv("GORTEX_STOP_DUMP") == "1" {
		if runtime.GOOS == "windows" {
			fmt.Fprintln(w, "[gortex daemon] SIGQUIT diagnostic termination is unavailable on Windows; continuing the existing force-stop path")
		} else {
			fmt.Fprintln(w, "[gortex daemon] rechecking ownership before SIGQUIT diagnostic termination (fatal with Go's default handler; dump completion is not guaranteed)")
			if complete, err := fenceCapturedDaemonSignal(w, target); complete || err != nil {
				return daemonStopResult{}, err
			}
			if err := daemonDiagnosticTerminate(target.process); err != nil {
				return daemonStopResult{}, fmt.Errorf("diagnostic SIGQUIT captured daemon: %w", err)
			}
			diagnostic = true
			// Do not immediately truncate a default Go SIGQUIT dump with KILL.
			// This is one bounded completion allowance, never a mutation retry.
			until := time.Now().Add(time.Second)
			for {
				complete, err := target.completeStop(w)
				if complete || err != nil {
					return daemonStopResult{diagnostic: true}, err
				}
				if !time.Now().Before(until) {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	fmt.Fprintln(w, "[gortex daemon] rechecking captured ownership before force-killing")
	// QUIT/output may have released the store or changed process/runtime.
	// Reapply every fence AFTER intent output; pre-QUIT proof cannot authorize
	// KILL and a blocked output writer must not stale the final lock check.
	if complete, err := fenceCapturedDaemonSignal(w, target); complete || err != nil {
		return daemonStopResult{diagnostic: diagnostic}, err
	}
	if err := daemonKillCapturedProcess(target.process); err != nil {
		return daemonStopResult{diagnostic: diagnostic}, fmt.Errorf("kill captured daemon: %w", err)
	}
	fmt.Fprintln(w, "[gortex daemon] graceful shutdown timed out — sent force-kill")
	// A successful signal is not a successful stop. Retain physical ownership
	// and cleanup errors instead of unconditionally removing PID/socket files.
	forceDeadline := time.Now().Add(3 * time.Second)
	for {
		complete, err := target.completeStop(w)
		if complete {
			return daemonStopResult{forced: true, diagnostic: diagnostic}, nil
		}
		if !time.Now().Before(forceDeadline) {
			return daemonStopResult{diagnostic: diagnostic}, errors.Join(errors.New("captured daemon did not release its store and runtime after force-kill"), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Reused for the actual QUIT and KILL steps. Store-free is checked last, after
// native/runtime reads. Unknown/reused ownership never authorizes a signal.
func fenceCapturedDaemonSignal(w io.Writer, target *daemonStopTarget) (bool, error) {
	if complete, err := target.completeStop(w); complete || err != nil {
		return complete, err
	}
	if running, err := target.processRunning(); err != nil || !running {
		return false, errors.Join(errors.New("daemon still owns its store but no signalable captured process is running"), err)
	}
	if _, err := target.runtimeGone(); err != nil {
		return false, fmt.Errorf("refusing force with changed runtime ownership: %w", err)
	}
	if err := target.requireSignalOwner(); err != nil {
		return false, err
	}
	if free, err := target.storeFree(); err != nil {
		return false, err
	} else if free {
		complete, err := target.completeStop(w)
		if complete || err != nil {
			return complete, err
		}
		return false, errors.New("daemon store became free; captured runtime cleanup is still pending, process left unsignaled")
	}
	return false, nil
}

func terminateCapturedDaemon(w io.Writer, target *daemonStopTarget) (daemonStopResult, error) {
	complete, err := target.completeStop(w)
	if complete || err != nil {
		return daemonStopResult{}, err
	}
	free, err := target.storeFree()
	if err != nil {
		return daemonStopResult{}, err
	}
	if !free {
		if _, err := target.runtimeGone(); err != nil {
			return daemonStopResult{}, err
		}
		running, err := target.processRunning()
		if err != nil {
			return daemonStopResult{}, err
		}
		if running {
			if err := target.requireSignalOwner(); err != nil {
				return daemonStopResult{}, err
			}
			// Closing may have released the lock during native/runtime
			// checks. Apply the same last observed-free guard as KILL.
			free, err := target.storeFree()
			if err != nil {
				return daemonStopResult{}, err
			}
			if free {
				return waitForDaemonTarget(w, target, 3*time.Second, false)
			}
			if err := daemonTerminateCapturedProcess(target.process); err != nil {
				return daemonStopResult{}, fmt.Errorf("terminate captured daemon: %w", err)
			}
		}
	}
	return waitForDaemonTarget(w, target, 3*time.Second, false)
}
