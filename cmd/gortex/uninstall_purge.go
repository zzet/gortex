package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
)

// --purge extends `gortex uninstall` from per-repo / integration cleanup to a
// full machine-level teardown: stop the daemon, remove its OS service unit,
// delete the unified ~/.gortex data/cache/config tree, and — for the
// installer-script install — remove the binary. Package-manager installs
// (brew/scoop/go) leave the binary to the manager and only get advice, so its
// metadata stays consistent.

// purgePlan is the machine-scope footprint --purge removes, computed up front
// so the confirm wizard can preview every path before anything is deleted.
type purgePlan struct {
	dirs           []string      // existing data/cache/config dirs to delete
	binary         string        // the running executable
	binMethod      InstallMethod // how it was installed
	servicePresent bool          // an OS service unit file exists
}

// computePurgePlan collects the machine-scope removal targets that actually
// exist, de-duplicating the data/cache/config dirs (they collapse to one
// ~/.gortex tree unless an XDG override splits a category out).
func computePurgePlan() purgePlan {
	var p purgePlan

	seen := map[string]bool{}
	for _, d := range []string{platform.ConfigDir(), platform.DataDir(), platform.CacheDir(), platform.Home()} {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if _, err := os.Stat(d); err == nil {
			p.dirs = append(p.dirs, d)
		}
	}

	if exe, err := os.Executable(); err == nil {
		p.binary = exe
		p.binMethod = detectInstallMethod(exe, goBinDir(), homeDirOrEmpty(), os.Getenv("LOCALAPPDATA"))
	}

	p.servicePresent = serviceUnitPresent()
	return p
}

// binaryRemovable reports whether --purge should delete the binary itself.
// Only the installer-script method drops a plain file we own; package-manager
// installs must be removed through the manager (we advise instead — see
// purgeBinaryNote).
func (p purgePlan) binaryRemovable() bool {
	return p.binMethod == InstallScript && p.binary != ""
}

// serviceUnitPresent reports whether an OS service unit file for the daemon
// exists, so --purge can preview and remove it even when it isn't currently
// active.
func serviceUnitPresent() bool {
	var (
		path string
		err  error
	)
	switch runtime.GOOS {
	case "darwin":
		path, err = launchdPlistPath()
	case "linux":
		path, err = systemdUnitPath()
	case "windows":
		// No unit file — the logon task lives in Task Scheduler; query it.
		return exec.Command("schtasks", "/Query", "/TN", windowsTaskName).Run() == nil
	default:
		return false
	}
	if err != nil {
		return false
	}
	_, statErr := os.Stat(path)
	return statErr == nil
}

// purgeWizardItems renders the machine-scope targets for the confirm wizard so
// --purge shows the daemon / service / data-tree / binary blast radius
// alongside the per-repo files before any deletion. daemonRunning is passed in
// (not probed here) so the preview stays deterministic and testable.
func purgeWizardItems(p purgePlan, daemonRunning bool) []string {
	var items []string
	if daemonRunning {
		items = append(items, "stop the running daemon")
	}
	if p.servicePresent {
		items = append(items, "remove the daemon OS service unit")
	}
	for _, d := range p.dirs {
		items = append(items, d+"/  (data/cache/config)")
	}
	if p.binaryRemovable() {
		items = append(items, p.binary+"  (binary)")
	}
	return items
}

// executePurge performs the machine-scope teardown after confirmation: stop
// the daemon, remove its service unit, delete the data/cache/config tree, and
// (installer-script method) delete the binary. Failures are collected as
// warnings so a partial teardown still reports what it removed; the returned
// count tallies each successful action.
func executePurge(cmd *cobra.Command, p purgePlan) (removed int, failures []string) {
	// Stop a manually started daemon; a supervised one is stopped when its
	// unit is removed just below.
	if daemon.IsRunning() && !serviceActive() {
		if err := runDaemonStop(cmd, nil); err != nil {
			failures = append(failures, fmt.Sprintf("stop daemon: %v", err))
		} else {
			removed++
		}
	}

	// Remove the OS service unit (idempotent; also stops a supervised daemon).
	if p.servicePresent {
		if err := runDaemonUninstallService(cmd, nil); err != nil {
			failures = append(failures, fmt.Sprintf("remove service: %v", err))
		} else {
			removed++
		}
	}

	// Delete the data/cache/config directories.
	for _, d := range p.dirs {
		if err := os.RemoveAll(d); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", d, err))
			continue
		}
		removed++
	}

	// Remove the binary only for the installer-script method. Unlinking the
	// running executable is safe on Unix — the inode survives until this
	// process exits.
	if p.binaryRemovable() {
		if err := os.Remove(p.binary); err != nil {
			failures = append(failures, fmt.Sprintf("remove binary %s: %v", p.binary, err))
		} else {
			removed++
		}
	}

	return removed, failures
}

// purgeBinaryNote returns guidance for removing the binary when --purge can't
// (or shouldn't) do it automatically — every method except the installer
// script, which purge deletes directly.
func purgeBinaryNote(p purgePlan) string {
	if p.binary == "" || p.binMethod == InstallScript {
		return ""
	}
	switch p.binMethod {
	case InstallBrew:
		return "Binary left in place — remove it with `brew uninstall gortex`."
	case InstallScoop:
		return "Binary left in place — remove it with `scoop uninstall gortex`."
	case InstallGoInstall:
		return fmt.Sprintf("Binary left in place — delete it with `rm %s`.", p.binary)
	case InstallScriptPS:
		// Windows will not delete a running executable, so purge can't remove
		// it from here; the installer also put its directory on the user PATH.
		dir := filepath.Dir(p.binary)
		return fmt.Sprintf("Binary left in place (Windows can't delete a running executable) — delete the %s folder and remove it from your user PATH.", dir)
	default:
		return fmt.Sprintf("Binary left in place at %s — delete it manually.", p.binary)
	}
}
