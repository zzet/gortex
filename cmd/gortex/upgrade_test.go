package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpgradeInstallMethodDetection proves the path-math that maps a binary's
// location to its install method — so `gortex upgrade` updates the way the user
// installed (Homebrew / Scoop / go install / installer script) instead of
// clobbering a package-manager-owned binary — including the Windows Scoop path
// classified on a non-Windows host.
func TestUpgradeInstallMethodDetection(t *testing.T) {
	const goBin = "/Users/x/go/bin"
	const home = "/Users/x"
	const localAppData = `C:\Users\x\AppData\Local`

	cases := []struct {
		name    string
		path    string
		want    InstallMethod
		wantCmd string
	}{
		{"brew_cellar", "/opt/homebrew/Cellar/gortex/0.48.0/bin/gortex", InstallBrew, "brew upgrade gortex"},
		{"brew_intel", "/usr/local/Cellar/gortex/0.48.0/bin/gortex", InstallBrew, "brew upgrade gortex"},
		{"scoop_windows", `C:\Users\x\scoop\apps\gortex\current\gortex.exe`, InstallScoop, "scoop update gortex"},
		{"go_install", "/Users/x/go/bin/gortex", InstallGoInstall, "go install github.com/zzet/gortex/cmd/gortex@latest"},
		{"installer_script", "/Users/x/.local/bin/gortex", InstallScript, "curl -fsSL https://get.gortex.dev | sh"},
		{"installer_ps1", `C:\Users\x\AppData\Local\Programs\gortex\gortex.exe`, InstallScriptPS, "irm https://get.gortex.dev/install.ps1 | iex"},
		// os.Executable and %LOCALAPPDATA% can disagree on casing; Windows
		// paths are case-insensitive, so the anchor must be too (issue #872).
		{"installer_ps1_case", `c:\users\x\appdata\local\programs\Gortex\gortex.exe`, InstallScriptPS, "irm https://get.gortex.dev/install.ps1 | iex"},
		{"localappdata_other_program", `C:\Users\x\AppData\Local\Programs\gortex-tools\gortex.exe`, InstallUnknown, ""},
		{"unknown_system", "/usr/bin/gortex", InstallUnknown, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := detectInstallMethod(c.path, goBin, home, localAppData)
			if got != c.want {
				t.Errorf("detectInstallMethod(%q) = %q, want %q", c.path, got, c.want)
			}
			cmd, manual := upgradeInstructions(got, "")
			if cmd != c.wantCmd {
				t.Errorf("upgradeInstructions(%q) cmd = %q, want %q", got, cmd, c.wantCmd)
			}
			if (cmd == "") != manual {
				t.Errorf("manual flag inconsistent with command for %q", got)
			}
		})
	}

	// A version pin flows into the go-install target.
	if cmd, _ := upgradeInstructions(InstallGoInstall, "v0.49.0"); cmd != "go install github.com/zzet/gortex/cmd/gortex@v0.49.0" {
		t.Errorf("pinned go install cmd = %q", cmd)
	}
	// …and into the PowerShell installer through GORTEX_VERSION, normalised to
	// the tag form install.ps1 downloads.
	if cmd, _ := upgradeInstructions(InstallScriptPS, "0.49.0"); cmd != "$env:GORTEX_VERSION='v0.49.0'; irm https://get.gortex.dev/install.ps1 | iex" {
		t.Errorf("pinned install.ps1 cmd = %q", cmd)
	}
	if cmd, _ := upgradeInstructions(InstallScriptPS, "latest"); cmd != installPSScript {
		t.Errorf("latest install.ps1 cmd = %q", cmd)
	}

	// Without LOCALAPPDATA (any non-Windows host) the anchor never matches.
	if got := detectInstallMethod(`C:\Users\x\AppData\Local\Programs\gortex\gortex.exe`, goBin, home, ""); got != InstallUnknown {
		t.Errorf("empty LOCALAPPDATA classified as %q", got)
	}
}

// TestValidateUpgradePin proves only a release tag reaches the PowerShell
// installer's command string — anything else could break out of the quoted
// literal — while go install keeps accepting branch / commit pins.
func TestValidateUpgradePin(t *testing.T) {
	assert.NoError(t, validateUpgradePin(InstallScriptPS, ""))
	assert.NoError(t, validateUpgradePin(InstallScriptPS, "v0.64.0"))
	assert.NoError(t, validateUpgradePin(InstallScriptPS, "0.64.0-rc.1"))
	assert.Error(t, validateUpgradePin(InstallScriptPS, "v1'; Remove-Item C:\\x; '"))
	assert.NoError(t, validateUpgradePin(InstallScriptPS, "latest"))
	assert.Error(t, validateUpgradePin(InstallScriptPS, "main"))
	assert.NoError(t, validateUpgradePin(InstallGoInstall, "main"))
}

// TestUpgradeRunCommandUsesShellOnlyForPipelines proves --run routes the
// installer-script pipeline through `sh -c` (so the pipe is interpreted rather
// than split into raw curl args — issue #281) while plain-argv methods still
// exec directly with no shell in the way.
func TestUpgradeRunCommandUsesShellOnlyForPipelines(t *testing.T) {
	ctx := context.Background()
	cmd := upgradeExecCommand(ctx, InstallScript, "curl -fsSL https://get.gortex.dev | sh")
	// Assert on Args[0], not Path: exec.Command resolves a bare name through
	// LookPath, so Path becomes /bin/sh once sh is on $PATH — Args[0] stays "sh".
	if got := cmd.Args[0]; got != "sh" {
		t.Fatalf("script installer should run through sh, got %q args %v", got, cmd.Args)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "-c" || cmd.Args[2] != "curl -fsSL https://get.gortex.dev | sh" {
		t.Fatalf("script installer command args = %#v", cmd.Args)
	}

	cmd = upgradeExecCommand(ctx, InstallGoInstall, "go install github.com/zzet/gortex/cmd/gortex@latest")
	if got := cmd.Args[0]; got != "go" {
		t.Fatalf("plain argv command should exec directly, got %q args %v", got, cmd.Args)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "install" || cmd.Args[2] != "github.com/zzet/gortex/cmd/gortex@latest" {
		t.Fatalf("go install command args = %#v", cmd.Args)
	}

	// The PowerShell installer is a PowerShell pipeline: it must reach
	// powershell.exe as one -Command argument, never sh (absent on Windows).
	cmd = upgradeExecCommand(ctx, InstallScriptPS, installPSScript)
	want := []string{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", installPSScript}
	if !assert.ObjectsAreEqual(want, cmd.Args) {
		t.Fatalf("install.ps1 command args = %#v, want %#v", cmd.Args, want)
	}
}

// TestRunUpgradeCommandDaemonBounce proves --run bounces the daemon around the
// binary swap: a manually started daemon is stopped before and restarted after
// (in that order), a supervised daemon is bounced through its supervisor
// without a manual stop/start, an idle host just runs the command, and a failed
// install still brings the daemon back so the host isn't left with it down.
func TestRunUpgradeCommandDaemonBounce(t *testing.T) {
	newOps := func(log *[]string, running, supervised bool, stopErr error) upgradeDaemonOps {
		return upgradeDaemonOps{
			isRunning:    func() bool { return running },
			supervised:   func() bool { return supervised },
			stop:         func() error { *log = append(*log, "stop"); return stopErr },
			start:        func() error { *log = append(*log, "start"); return nil },
			superRestart: func(io.Writer) error { *log = append(*log, "superRestart"); return nil },
		}
	}
	runFn := func(log *[]string, err error) func() error {
		return func() error { *log = append(*log, "run"); return err }
	}

	t.Run("idle host runs the command only", func(t *testing.T) {
		var log []string
		err := runUpgradeCommand(io.Discard, io.Discard, newOps(&log, false, false, nil), runFn(&log, nil))
		require.NoError(t, err)
		assert.Equal(t, []string{"run"}, log)
	})

	t.Run("manual daemon: stop then run then start", func(t *testing.T) {
		var log []string
		err := runUpgradeCommand(io.Discard, io.Discard, newOps(&log, true, false, nil), runFn(&log, nil))
		require.NoError(t, err)
		assert.Equal(t, []string{"stop", "run", "start"}, log)
	})

	t.Run("supervised daemon: run then supervisor restart", func(t *testing.T) {
		var log []string
		err := runUpgradeCommand(io.Discard, io.Discard, newOps(&log, true, true, nil), runFn(&log, nil))
		require.NoError(t, err)
		assert.Equal(t, []string{"run", "superRestart"}, log)
	})

	t.Run("failed install still restarts the manual daemon", func(t *testing.T) {
		var log []string
		boom := errors.New("boom")
		err := runUpgradeCommand(io.Discard, io.Discard, newOps(&log, true, false, nil), runFn(&log, boom))
		require.ErrorIs(t, err, boom)
		assert.Equal(t, []string{"stop", "run", "start"}, log)
	})

	t.Run("stop failure aborts before the install runs", func(t *testing.T) {
		var log []string
		err := runUpgradeCommand(io.Discard, io.Discard, newOps(&log, true, false, errors.New("stopfail")), runFn(&log, nil))
		require.Error(t, err)
		assert.Equal(t, []string{"stop"}, log)
	})
}

// TestUpgradeReleaseTagParse covers the /releases/latest redirect tag parse.
func TestUpgradeReleaseTagParse(t *testing.T) {
	cases := map[string]string{
		"https://github.com/zzet/gortex/releases/tag/v0.49.0":     "v0.49.0",
		"https://github.com/zzet/gortex/releases/tag/v1.0.0-rc1":  "v1.0.0-rc1",
		"https://github.com/zzet/gortex/releases/tag/v0.49.0?x=1": "v0.49.0",
		"https://github.com/zzet/gortex/releases":                 "",
		"": "",
	}
	for loc, want := range cases {
		if got := tagFromReleaseLocation(loc); got != want {
			t.Errorf("tagFromReleaseLocation(%q) = %q, want %q", loc, got, want)
		}
	}
}

// TestUpgradeMigratesWithTheNewBinary pins the sequencing that makes the
// post-upgrade refresh correct at all: this process is the OLD binary, so
// re-applying adapters in-process would faithfully write the shapes the
// upgrade is meant to migrate away from. The refresh must re-exec.
func TestUpgradeMigratesWithTheNewBinary(t *testing.T) {
	var gotBin string
	var gotArgs []string
	orig := upgradeMigrateCommand
	upgradeMigrateCommand = func(ctx context.Context, bin string) *exec.Cmd {
		gotBin = bin
		c := exec.CommandContext(ctx, "true")
		gotArgs = []string{"install", "--yes"}
		return c
	}
	t.Cleanup(func() { upgradeMigrateCommand = orig })

	var out, errw bytes.Buffer
	migrateAgentConfigs(context.Background(), &out, &errw)

	if gotBin == "" {
		t.Fatal("migration did not resolve a binary to re-exec")
	}
	if !filepath.IsAbs(gotBin) {
		t.Errorf("re-exec target should be absolute, got %q", gotBin)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "install" || gotArgs[1] != "--yes" {
		t.Errorf("migration should run a non-interactive install, got %v", gotArgs)
	}
	if !strings.Contains(out.String(), "Refreshing agent configuration") {
		t.Errorf("the refresh should announce itself:\n%s", out.String())
	}
}

// TestUpgradeMigrationFailureDoesNotFailTheUpgrade: the binary has already
// been replaced by the time the refresh runs, so a refresh that cannot run is
// a warning with a manual command — never a failed upgrade.
func TestUpgradeMigrationFailureDoesNotFailTheUpgrade(t *testing.T) {
	orig := upgradeMigrateCommand
	upgradeMigrateCommand = func(ctx context.Context, bin string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}
	t.Cleanup(func() { upgradeMigrateCommand = orig })

	var out, errw bytes.Buffer
	migrateAgentConfigs(context.Background(), &out, &errw)

	if !strings.Contains(errw.String(), "run `gortex install` by hand") {
		t.Errorf("a failed refresh must hand back a manual command:\n%s", errw.String())
	}
}

// TestUpgradeFollowUpsListConfigOnlyWhenNotDone keeps the closing advice
// honest: it must not tell the user to run the step just performed.
func TestUpgradeFollowUpsListConfigOnlyWhenNotDone(t *testing.T) {
	var done bytes.Buffer
	upgradeFollowUps(&done, true, true)
	if strings.Contains(done.String(), "gortex install") {
		t.Errorf("config was refreshed; do not ask for it again:\n%s", done.String())
	}
	if !strings.Contains(done.String(), "gortex index .") {
		t.Errorf("reindex advice is unconditional:\n%s", done.String())
	}

	var skipped bytes.Buffer
	upgradeFollowUps(&skipped, false, false)
	if !strings.Contains(skipped.String(), "gortex install") {
		t.Errorf("a dry run still owes the config step:\n%s", skipped.String())
	}
}

// TestUpgradeAcceptsUpdate: "update" is what people type, and the command's
// own summary line has always said "Update gortex…". It used to be an
// unknown-command error, which meant the post-upgrade config refresh would
// never run for anyone whose habit is `gortex update`.
func TestUpgradeAcceptsUpdate(t *testing.T) {
	found, _, err := rootCmd.Find([]string{"update"})
	if err != nil {
		t.Fatalf("`gortex update` does not resolve: %v", err)
	}
	if found != upgradeCmd {
		t.Fatalf("`gortex update` resolved to %q, want the upgrade command", found.Name())
	}
}

// TestPostUpgradeStepsDecision pins the wiring the whole feature hangs on:
// after a real upgrade the refresh runs, --no-migrate skips it visibly, and
// the return value is what the closing advice keys off.
func TestPostUpgradeStepsDecision(t *testing.T) {
	var ran bool
	orig := upgradeMigrateCommand
	upgradeMigrateCommand = func(ctx context.Context, bin string) *exec.Cmd {
		ran = true
		return exec.CommandContext(ctx, "true")
	}
	t.Cleanup(func() { upgradeMigrateCommand = orig })

	t.Run("refreshes by default", func(t *testing.T) {
		ran = false
		var out, errw bytes.Buffer
		if migrated := postUpgradeSteps(context.Background(), &out, &errw, false); !migrated {
			t.Error("a completed upgrade should report the refresh as done")
		}
		if !ran {
			t.Error("the refresh did not run")
		}
	})

	t.Run("--no-migrate skips visibly", func(t *testing.T) {
		ran = false
		var out, errw bytes.Buffer
		if migrated := postUpgradeSteps(context.Background(), &out, &errw, true); migrated {
			t.Error("--no-migrate must not report the refresh as done")
		}
		if ran {
			t.Error("the refresh ran despite --no-migrate")
		}
		if !strings.Contains(out.String(), "--no-migrate") {
			t.Errorf("skipping must be stated, not silent:\n%s", out.String())
		}
	})
}
