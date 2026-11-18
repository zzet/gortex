package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/testenv"
)

// TestComputePurgePlanCollectsExistingDataDirs proves --purge targets the
// unified ~/.gortex tree when it exists, de-duplicated across the
// config/data/cache/home categories that collapse to it.
func TestComputePurgePlanCollectsExistingDataDirs(t *testing.T) {
	// UnifiedHome clears the XDG overrides so the categories collapse into ~/.gortex.
	home := testenv.UnifiedHome(t)

	gortexHome := filepath.Join(home, ".gortex")
	require.NoError(t, os.MkdirAll(filepath.Join(gortexHome, "cache"), 0o755))

	p := computePurgePlan()
	assert.Contains(t, p.dirs, gortexHome, "the unified ~/.gortex tree must be a purge target")

	seen := map[string]bool{}
	for _, d := range p.dirs {
		assert.Falsef(t, seen[d], "duplicate purge dir %q", d)
		seen[d] = true
	}
}

// TestComputePurgePlanEmptyWhenNothingInstalled asserts a clean host yields no
// data dirs to remove.
func TestComputePurgePlanEmptyWhenNothingInstalled(t *testing.T) {
	testenv.UnifiedHome(t)

	p := computePurgePlan()
	assert.Empty(t, p.dirs)
}

// TestBinaryRemovableOnlyForScript proves purge auto-deletes the binary only
// for the installer-script method (a plain file we own).
func TestBinaryRemovableOnlyForScript(t *testing.T) {
	assert.True(t, purgePlan{binary: "/x/gortex", binMethod: InstallScript}.binaryRemovable())
	assert.False(t, purgePlan{binary: "/x/gortex", binMethod: InstallBrew}.binaryRemovable())
	assert.False(t, purgePlan{binary: "/x/gortex", binMethod: InstallGoInstall}.binaryRemovable())
	assert.False(t, purgePlan{binary: "", binMethod: InstallScript}.binaryRemovable())
}

// TestPurgeBinaryNoteByMethod pins the per-method binary guidance: the
// installer-script binary is deleted by purge (no note), everything else gets
// method-appropriate advice.
func TestPurgeBinaryNoteByMethod(t *testing.T) {
	assert.Equal(t, "", purgeBinaryNote(purgePlan{binary: "/x/gortex", binMethod: InstallScript}))
	assert.Contains(t, purgeBinaryNote(purgePlan{binary: "/x/gortex", binMethod: InstallBrew}), "brew uninstall")
	assert.Contains(t, purgeBinaryNote(purgePlan{binary: "/x/gortex", binMethod: InstallScoop}), "scoop uninstall")
	assert.Contains(t, purgeBinaryNote(purgePlan{binary: "/x/gortex", binMethod: InstallGoInstall}), "/x/gortex")
	assert.Contains(t, purgeBinaryNote(purgePlan{binary: "/x/gortex", binMethod: InstallUnknown}), "manually")
	// install.ps1: a running .exe can't be deleted on Windows, so purge
	// advises — naming the folder and the PATH entry the installer added.
	ps := purgePlan{binary: filepath.Join("x", "gortex", "gortex.exe"), binMethod: InstallScriptPS}
	assert.False(t, ps.binaryRemovable())
	assert.Contains(t, purgeBinaryNote(ps), filepath.Join("x", "gortex"))
	assert.Contains(t, purgeBinaryNote(ps), "user PATH")
	assert.Equal(t, "", purgeBinaryNote(purgePlan{binary: "", binMethod: InstallBrew}))
}

// TestPurgeWizardItems proves the preview lists the daemon stop, the service
// unit, each data dir, and the binary (script method only) — and omits each
// when it doesn't apply.
func TestPurgeWizardItems(t *testing.T) {
	full := purgePlan{
		dirs:           []string{"/home/u/.gortex"},
		binary:         "/home/u/.local/bin/gortex",
		binMethod:      InstallScript,
		servicePresent: true,
	}
	items := strings.Join(purgeWizardItems(full, true), "\n")
	assert.Contains(t, items, "stop the running daemon")
	assert.Contains(t, items, "service unit")
	assert.Contains(t, items, "/home/u/.gortex")
	assert.Contains(t, items, "/home/u/.local/bin/gortex")

	// Not running, no service, package-manager binary → only the data dir.
	lean := purgePlan{dirs: []string{"/home/u/.gortex"}, binary: "/opt/gortex", binMethod: InstallBrew}
	leanItems := strings.Join(purgeWizardItems(lean, false), "\n")
	assert.NotContains(t, leanItems, "stop the running daemon")
	assert.NotContains(t, leanItems, "service unit")
	assert.NotContains(t, leanItems, "/opt/gortex")
	assert.Contains(t, leanItems, "/home/u/.gortex")
}
