package profiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Frozen prior producer output is independently checked against the production
// supported-release digests below. It must not be reconstructed from today's
// wording: that would change the historical ownership fixture with every edit.
func previousGeneratedBody(t *testing.T, p Profile) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "legacy-v0.64.11", p.Name+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func writePreviousProfiles(t *testing.T, dir string, stamped bool) {
	t.Helper()
	stamp := generationStamp{Version: "v0.64.11", Files: make(map[string]string)}
	for _, p := range Table() {
		body := []byte(previousGeneratedBody(t, p))
		if err := os.WriteFile(filepath.Join(dir, p.Name+".md"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		stamp.Files[p.Name+".md"] = generatedHash(body)
		if p.Name == "localization" {
			if err := os.WriteFile(filepath.Join(dir, ActiveFileName), body, 0o644); err != nil {
				t.Fatal(err)
			}
			stamp.Files[ActiveFileName] = generatedHash(body)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte("{\"name\":\"localization\",\"switched_at\":\"preserved\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stamped {
		data, err := json.Marshal(stamp)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, GenerationStampFileName), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshotProfileFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for _, name := range append(profileFileNames(), ActiveFileName, stateFileName, GenerationStampFileName) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			out[name] = string(data)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return out
}

func TestProfileUpgradePreservesStoredSelectionAndIsIdempotent(t *testing.T) {
	for _, stamped := range []bool{true, false} {
		t.Run(map[bool]string{true: "stamped", false: "known-legacy"}[stamped], func(t *testing.T) {
			dir := t.TempDir()
			writePreviousProfiles(t, dir, stamped)
			t.Setenv(ActiveEnv, "full")
			stateBefore, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			updated, err := RegenerateIfNeeded(dir, "v0.64.12+test")
			if err != nil || !updated {
				t.Fatalf("upgrade: %t %v", updated, err)
			}
			stored, err := storedActiveName(dir)
			if err != nil || stored != "localization" || ActiveName(dir) != "full" {
				t.Fatalf("disk selection or process-local override changed: %q %v", stored, err)
			}
			current, _ := ByName("localization")
			files := snapshotProfileFiles(t, dir)
			if files[ActiveFileName] != current.Body() || files[stateFileName] != string(stateBefore) {
				t.Fatal("upgrade did not preserve exact stored selection/state bytes")
			}
			stamp, err := readGenerationStamp(dir)
			if err != nil || stamp.Version != "v0.64.12+test" {
				t.Fatalf("binary stamp missing: %+v %v", stamp, err)
			}
			for name, hash := range stamp.Files {
				if hash != generatedHash([]byte(files[name])) {
					t.Fatalf("managed ownership hash mismatch for %s", name)
				}
			}
			updated, err = RegenerateIfNeeded(dir, "v0.64.12+test")
			if err != nil || updated {
				t.Fatalf("second start rewrote profiles: %t %v", updated, err)
			}
			for name, before := range files {
				if after := snapshotProfileFiles(t, dir)[name]; after != before {
					t.Fatalf("second start changed %s", name)
				}
			}
		})
	}
}

func TestInterruptedProfileUpgradeAcceptsOnlyKnownOldOrNewContent(t *testing.T) {
	for _, modified := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "preserve-user-edit"}[modified], func(t *testing.T) {
			dir := t.TempDir()
			writePreviousProfiles(t, dir, true)
			core, _ := ByName("core")
			if err := atomicWrite(filepath.Join(dir, "core.md"), []byte(core.Body())); err != nil {
				t.Fatal(err)
			}
			if modified {
				if err := os.WriteFile(filepath.Join(dir, "full.md"), []byte("my custom profile\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotProfileFiles(t, dir)
			updated, err := RegenerateIfNeeded(dir, "v0.64.12")
			if modified {
				if err == nil || updated || !strings.Contains(err.Error(), "move it aside") {
					t.Fatalf("custom edit not preserved with actionable guidance: %t %v", updated, err)
				}
				for name, value := range before {
					if got := snapshotProfileFiles(t, dir)[name]; got != value {
						t.Fatalf("conflict changed %s", name)
					}
				}
			} else if err != nil || !updated {
				t.Fatalf("interrupted generated update stuck: %t %v", updated, err)
			}
		})
	}
}

func TestProfileRefreshPreservesUnmanagedFilesAndRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("user notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if updated, err := RegenerateIfNeeded(dir, "v-next"); err != nil || updated {
		t.Fatalf("empty/uninstalled directory was populated: %t %v", updated, err)
	}
	if _, err := os.Stat(filepath.Join(dir, generationLockName)); !os.IsNotExist(err) {
		t.Fatal("uninstalled directory gained generator artifacts")
	}
	if err := Generate(dir, "v-current"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "user.md")
	if err := os.WriteFile(target, []byte("user-owned target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "core.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "core.md")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if updated, err := RegenerateIfNeeded(dir, "v-next"); err == nil || updated {
		t.Fatalf("linked user file replaced: %t %v", updated, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "user-owned target" {
		t.Fatal("symlink target changed")
	}
}

func TestUnversionedRenderKeepsBodiesWithoutInventingVersionArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{GenerationStampFileName, generationLockName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("unversioned caller invented %s", name)
		}
	}
	if err := Generate(dir, "v-actual"); err != nil {
		t.Fatal(err)
	}
	stamp, err := readGenerationStamp(dir)
	if err != nil || stamp.Version != "v-actual" {
		t.Fatalf("explicit generation failed to establish provenance: %+v %v", stamp, err)
	}
}

func TestProfileGenerationRejectsUnknownLegacyAndMalformedStamp(t *testing.T) {
	for _, kind := range []string{"custom-body", "malformed-stamp", "trailing-stamp-value"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := Generate(dir, "v-old"); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "custom-body":
				if err := os.Remove(filepath.Join(dir, GenerationStampFileName)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "core.md"), []byte("## MANDATORY: my user-authored rules"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "malformed-stamp":
				if err := os.WriteFile(filepath.Join(dir, GenerationStampFileName), []byte("not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "trailing-stamp-value":
				path := filepath.Join(dir, GenerationStampFileName)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(data, []byte("{}")...), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotProfileFiles(t, dir)
			if updated, err := RegenerateIfNeeded(dir, "v-next"); err == nil || updated {
				t.Fatalf("unrecognized ownership was accepted: %t %v", updated, err)
			}
			for name, value := range before {
				if after := snapshotProfileFiles(t, dir)[name]; after != value {
					t.Fatalf("conflict rewrote %s", name)
				}
			}
		})
	}
}

func TestLegacyProfileProvenanceAndCoreGrowth(t *testing.T) {
	// Pinned public v0.64.11 profiles have this exact core length. This is
	// independent of the raisable ceiling and must not drift with new bodies.
	const previousCoreBytes = 4316
	for _, p := range Table() {
		prior := []byte(previousGeneratedBody(t, p))
		if !containsGeneratedHash(legacyGeneratedHashes[p.Name+".md"], generatedHash(prior)) {
			t.Fatalf("legacy fixture no longer matches a pinned public release: %s", p.Name)
		}
		if p.Name == "core" {
			if len(prior) != previousCoreBytes || len(p.Body()) > previousCoreBytes+16 || bodyByteCeilings[p.Name]-len(p.Body()) < 256 {
				t.Fatalf("core body growth/headroom changed: prior%d current%d ceiling%d", len(prior), len(p.Body()), bodyByteCeilings[p.Name])
			}
		}
	}
}

func TestExplicitSwitchCanRepairInvalidSelectionWithoutWeakeningAutomaticRefresh(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir, "v-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte("broken selection"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotProfileFiles(t, dir)
	if updated, err := RegenerateIfNeeded(dir, "v-next"); err == nil || updated {
		t.Fatalf("automatic refresh guessed an invalid selection: %t %v", updated, err)
	}
	for name, value := range before {
		if snapshotProfileFiles(t, dir)[name] != value {
			t.Fatalf("automatic refresh modified %s", name)
		}
	}
	if _, err := Switch(dir, "localization", "v-next"); err != nil {
		t.Fatalf("explicit selection could not repair metadata: %v", err)
	}
	if name, err := storedActiveName(dir); err != nil || name != "localization" {
		t.Fatalf("explicit selection not committed: %q %v", name, err)
	}
}
