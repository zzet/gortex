package main

import "testing"

// TestUpgradeInstallMethodDetection proves the path-math that maps a binary's
// location to its install method — so `gortex upgrade` updates the way the user
// installed (Homebrew / Scoop / go install / installer script) instead of
// clobbering a package-manager-owned binary — including the Windows Scoop path
// classified on a non-Windows host.
func TestUpgradeInstallMethodDetection(t *testing.T) {
	const goBin = "/Users/x/go/bin"
	const home = "/Users/x"

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
		{"unknown_system", "/usr/bin/gortex", InstallUnknown, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := detectInstallMethod(c.path, goBin, home)
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
}

func TestUpgradeRunCommandUsesShellOnlyForPipelines(t *testing.T) {
	cmd := upgradeExecCommand(rootCmd, "curl -fsSL https://get.gortex.dev | sh")
	if got := cmd.Path; got != "sh" {
		t.Fatalf("script installer should run through sh, got path %q args %v", got, cmd.Args)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "-c" || cmd.Args[2] != "curl -fsSL https://get.gortex.dev | sh" {
		t.Fatalf("script installer command args = %#v", cmd.Args)
	}

	cmd = upgradeExecCommand(rootCmd, "go install github.com/zzet/gortex/cmd/gortex@latest")
	if got := cmd.Path; got != "go" {
		t.Fatalf("plain argv command should exec directly, got path %q args %v", got, cmd.Args)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "install" || cmd.Args[2] != "github.com/zzet/gortex/cmd/gortex@latest" {
		t.Fatalf("go install command args = %#v", cmd.Args)
	}
}

// TestUpgradeReleaseTagParse covers the /releases/latest redirect tag parse.
func TestUpgradeReleaseTagParse(t *testing.T) {
	cases := map[string]string{
		"https://github.com/zzet/gortex/releases/tag/v0.49.0":      "v0.49.0",
		"https://github.com/zzet/gortex/releases/tag/v1.0.0-rc1":   "v1.0.0-rc1",
		"https://github.com/zzet/gortex/releases/tag/v0.49.0?x=1":  "v0.49.0",
		"https://github.com/zzet/gortex/releases":                  "",
		"":                                                         "",
	}
	for loc, want := range cases {
		if got := tagFromReleaseLocation(loc); got != want {
			t.Errorf("tagFromReleaseLocation(%q) = %q, want %q", loc, got, want)
		}
	}
}
