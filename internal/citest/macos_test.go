package citest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func (f *fixture) planWith(head string, labels ...string) Manifest {
	f.t.Helper()
	event := f.pullRequestEvent(head)
	event.Labels = labels
	m, err := f.planner.Plan(f.t.Context(), event)
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func macOSRows(m Manifest) []string {
	var rows []string
	for _, row := range m.Matrix.Include {
		if row.OS == "macos-latest" {
			rows = append(rows, row.Partition)
		}
	}
	return rows
}

// fullRows is the full matrix size with or without the macOS partitions.
func fullRows(t *testing.T, macOS bool) int {
	t.Helper()
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	return len(p.planMatrix(p.FullMatrix(), macOS).Include)
}

func TestMacOSPartitionsOmittedFromPRWithoutSensitivePathOrLabel(t *testing.T) {
	f := newFixture(t)
	m := f.plan(f.change("leaf/value.go"))
	requireAffected(t, m)
	if m.MacOS.Selected || len(macOSRows(m)) != 0 || len(m.MacOS.Matched) != 0 || !strings.Contains(m.MacOS.Reason, "no macOS-sensitive change") {
		t.Fatalf("macOS partitions planned for an ordinary change: %+v %v", m.MacOS, m.Matrix)
	}
	if len(m.Matrix.Include) == 0 {
		t.Fatal("omitting macOS dropped the other targets")
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(data)
	if err != nil || decoded.MacOS.Reason != m.MacOS.Reason || decoded.MacOS.Selected {
		t.Fatalf("test-selection.json lost the macOS decision: %+v %v", decoded.MacOS, err)
	}
}

func TestMacOSPartitionsSelectedForSensitiveChanges(t *testing.T) {
	for _, tc := range []struct {
		name, path, text, rule string
		full                   bool
	}{
		{name: "darwin suffix", path: "leaf/value_darwin.go", text: "package leaf\nfunc darwinOnly() int { return 42 }\n", rule: "darwin file suffix"},
		{name: "darwin build constraint", path: "leaf/notify.go", text: "//go:build darwin\n\npackage leaf\nfunc notify() int { return 42 }\n", rule: "darwin build constraint"},
		{name: "platform layer", path: "internal/platform/value.go", text: "package platform\nfunc Value() int { return 42 }\n", rule: "platform layer"},
		{name: "cgo", path: "leaf/cgo.go", text: "package leaf\nimport \"C\"\n", rule: "cgo source", full: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.write(tc.path, tc.text)
			m := f.plan(f.commit())
			if tc.full != (m.Mode == "full") {
				t.Fatalf("mode %s: %s", m.Mode, m.Reason)
			}
			if !m.MacOS.Selected || len(macOSRows(m)) == 0 || !slices.Contains(m.MacOS.Matched, MacOSMatch{Path: tc.path, Rule: tc.rule}) || !strings.Contains(m.MacOS.Reason, tc.rule) {
				t.Fatalf("macOS-sensitive change did not select macOS: %+v %v", m.MacOS, m.Matrix)
			}
		})
	}
}

func TestMacOSDeletedDarwinTaggedFileIsReadFromBase(t *testing.T) {
	f := newFixture(t)
	f.write("leaf/notify.go", "//go:build darwin\n\npackage leaf\n")
	f.base = f.commit()
	if err := os.Remove(filepath.Join(f.root, "leaf", "notify.go")); err != nil {
		t.Fatal(err)
	}
	m := f.plan(f.commit())
	if !m.MacOS.Selected || !slices.Contains(m.MacOS.Matched, MacOSMatch{Path: "leaf/notify.go", Rule: "darwin build constraint"}) {
		t.Fatalf("deleted darwin file did not select macOS: %+v", m.MacOS)
	}
}

func TestMacOSLabelSelectsPartitions(t *testing.T) {
	f := newFixture(t)
	m := f.planWith(f.change("internal/indexer/value.go"), "bug", macOSLabel)
	requireAffected(t, m)
	if !m.MacOS.Selected || m.MacOS.Label != macOSLabel || len(m.MacOS.Matched) != 0 || !strings.Contains(m.MacOS.Reason, macOSLabel) {
		t.Fatalf("label did not select macOS: %+v", m.MacOS)
	}
	if !slices.Equal(macOSRows(m), []string{"indexer:1/2", "indexer:2/2", "indexer-race"}) {
		t.Fatal(macOSRows(m))
	}
}

func TestMacOSManifestDecisionIsValidated(t *testing.T) {
	f := newFixture(t)
	m := f.plan(f.change("leaf/value.go"))
	target := nativeTarget(t)
	for name, edit := range map[string]func(*Manifest){
		"selected without rows":  func(v *Manifest) { v.MacOS.Selected = true },
		"missing reason":         func(v *Manifest) { v.MacOS.Reason = "" },
		"rows without selection": func(v *Manifest) { v.Matrix.Include = append(v.Matrix.Include, MatrixRow{"macos-latest", "rest"}) },
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(m)
			var bad Manifest
			if err := json.Unmarshal(data, &bad); err != nil {
				t.Fatal(err)
			}
			edit(&bad)
			bad.SHA256 = bad.Seal()
			if _, err := f.planner.Validate(t.Context(), bad, target, bad.Census[target]); err == nil {
				t.Fatal("inconsistent macOS decision accepted")
			}
		})
	}
}

func TestMacOSSelectionCLIReadsLabelsAndEmitsGateOutput(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		labels []any
		want   string
	}{{labels: []any{}, want: "macos=false\n"}, {labels: []any{map[string]any{"name": macOSLabel}}, want: "macos=true\n"}} {
		event := f.pullRequestEvent(f.change("leaf/value.go"))
		payload := eventPayload(event)
		payload["pull_request"].(map[string]any)["labels"] = tc.labels
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		output := filepath.Join(dir, "outputs")
		manifest := filepath.Join(dir, "selection.json")
		env := withEnv(eventEnvironment(t, event, body), map[string]string{"GITHUB_OUTPUT": output})
		var stdout, stderr bytes.Buffer
		if err := RunSelect(t.Context(), f.root, []string{"--output", manifest}, env, &stdout, &stderr); err != nil {
			t.Fatalf("selection CLI: %v: %s", err, &stderr)
		}
		outputs, err := os.ReadFile(output)
		if err != nil || !strings.Contains(string(outputs), tc.want) || !strings.Contains(stdout.String(), "macOS test partitions") {
			t.Fatalf("gate output %q, summary %q: %v", outputs, &stdout, err)
		}
		data, err := os.ReadFile(manifest)
		if err != nil {
			t.Fatal(err)
		}
		m, err := DecodeManifest(data)
		if err != nil || m.MacOS.Selected != (tc.want == "macos=true\n") {
			t.Fatalf("manifest macOS decision %+v %v", m.MacOS, err)
		}
	}
}

func TestMacOSPathAndContentRules(t *testing.T) {
	for name, want := range map[string]string{
		".github/workflows/ci.yml":                      "CI workflow",
		"scripts/ci-select/main.go":                     "CI command",
		"internal/citest/policy.json":                   "CI planner and runner",
		"go.sum":                                        "Go module or checksum file",
		"bench/load/metrics_darwin_test.go":             "darwin file suffix",
		"internal/thirdparty/fswatcher/watcher.go":      "file watcher backend",
		"internal/indexer/watcher.go":                   "file watcher",
		"internal/indexer/poller.go":                    "file watcher",
		"internal/pathkey/pathkey.go":                   "path keys and case folding",
		"internal/daemon/client.go":                     "daemon start, socket or process code",
		"cmd/gortex/daemon_service.go":                  "daemon start, socket or process code",
		"cmd/gortex/ensure_daemon.go":                   "daemon start, socket or process code",
		"cmd/gortex/ensure_daemon_identity.go":          "daemon start, socket or process code",
		"cmd/gortex/ensure_daemon_stale_test.go":        "daemon start, socket or process code",
		"cmd/gortex/cli_daemon.go":                      "daemon start, socket or process code",
		"cmd/gortex/cli_daemon_probe_test.go":           "daemon start, socket or process code",
		"cmd/gortex/daemon_stop_target.go":              "daemon start, socket or process code",
		"cmd/gortex/daemon_stop_target_unix_test.go":    "daemon start, socket or process code",
		"internal/daemon/runtime_state.go":              "daemon start, socket or process code",
		"internal/daemon/runtime_state_startup_test.go": "daemon start, socket or process code",
		"internal/pathguard/pathguard.go":               "path keys and case folding",
		"internal/mcp/view_paths.go":                    "path keys and case folding",
		"cmd/gortex/daemon_stop_wait.go":                "",
		"cmd/gortex/doctor.go":                          "",
		"internal/mcp/tools_core.go":                    "",
		"internal/parser/scanner.c":                     "C, C++ or Objective-C source",
		"internal/resolver/testdata/server.h":           "",
		"internal/indexer/indexer.go":                   "",
		"internal/daemon/federation.go":                 "",
		"scripts/install.sh":                            "",
		".github/dependabot.yml":                        "",
	} {
		if got := macOSPathRule(name); got != want {
			t.Errorf("macOSPathRule(%q) = %q, want %q", name, got, want)
		}
	}
	for text, want := range map[string]string{
		"//go:build !darwin\n\npackage x\n":                                          "darwin build constraint",
		"// +build darwin\n\npackage x\n":                                            "darwin build constraint",
		"//go:build unix && !linux\n\npackage x\n":                                   "build constraint that differs on macOS",
		"//go:build linux\n\npackage x\n":                                            "",
		"//go:build windows\n\npackage x\n":                                          "",
		"package x\n\nimport (\n\t\"C\"\n\t\"os\"\n)\n":                              "cgo source",
		"package x\nimport \"runtime\"\nvar mac = runtime.GOOS == \"darwin\"\n":      "darwin runtime check",
		"package x\n// a comment\nfunc f() {}\n":                                     "",
		"package x\n//go:build darwin\n":                                             "",
		"/* licence\n   text */\n//go:build darwin\n\npackage x\n":                   "darwin build constraint",
		"/*\nlicence without star prefixes\n*/\n\n//go:build !darwin\n\npackage x\n": "darwin build constraint",
		"/*\n//go:build darwin\n*/\npackage x\n":                                     "",
	} {
		if got := macOSContentRule("x.go", []byte(text)); got != want {
			t.Errorf("macOSContentRule(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestMacOSBlockCommentHeaderConstraintIsRead(t *testing.T) {
	f := newFixture(t)
	f.write("leaf/notify.go", "/*\nLicence text\nwithout star prefixes.\n*/\n\n//go:build darwin\n\npackage leaf\nfunc notify() int { return 42 }\n")
	m := f.plan(f.commit())
	if !m.MacOS.Selected || !slices.Contains(m.MacOS.Matched, MacOSMatch{Path: "leaf/notify.go", Rule: "darwin build constraint"}) {
		t.Fatalf("darwin-only file behind a block comment did not select macOS: %+v", m.MacOS)
	}
}

const removedFromDarwinRule = "change removes a file from the darwin build"

func TestMacOSChangeThatRemovesAFileFromTheDarwinBuild(t *testing.T) {
	for _, tc := range []struct {
		name, path, old, new string
		rename               string // new path when the file is renamed
		want                 string // rule, or "" when macOS stays out
	}{
		{name: "rename to a linux file name", path: "leaf/helper.go", old: "package leaf\nfunc helper() int { return 1 }\n", rename: "leaf/helper_linux.go", want: removedFromDarwinRule},
		{name: "rename to a windows file name", path: "leaf/helper.go", old: "package leaf\nfunc helper() int { return 1 }\n", rename: "leaf/helper_windows.go", want: removedFromDarwinRule},
		{name: "rename keeping the darwin build", path: "leaf/helper.go", old: "package leaf\nfunc helper() int { return 1 }\n", rename: "leaf/other.go"},
		{name: "rename from a darwin file name", path: "leaf/helper_darwin.go", old: "package leaf\nfunc helper() int { return 1 }\n", rename: "leaf/helper_linux.go", want: "darwin file suffix"},
		{name: "constraint narrowed to linux", path: "leaf/notify.go", old: "//go:build darwin || linux\n\npackage leaf\nfunc notify() int { return 1 }\n", new: "//go:build linux\n\npackage leaf\nfunc notify() int { return 1 }\n", want: removedFromDarwinRule},
		{name: "constraint narrowed behind a block comment", path: "leaf/notify.go", old: "/*\nLicence\n*/\n\n//go:build unix\n\npackage leaf\nfunc notify() int { return 1 }\n", new: "/*\nLicence\n*/\n\n//go:build linux\n\npackage leaf\nfunc notify() int { return 1 }\n", want: removedFromDarwinRule},
		{name: "constraint widened", path: "leaf/notify.go", old: "//go:build linux\n\npackage leaf\nfunc notify() int { return 1 }\n", new: "//go:build linux || windows\n\npackage leaf\nfunc notify() int { return 1 }\n"},
		{name: "ordinary edit", path: "leaf/helper.go", old: "package leaf\nfunc helper() int { return 1 }\n", new: "package leaf\nfunc helper() int { return 2 }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.write(tc.path, tc.old)
			f.base = f.commit()
			if tc.rename != "" {
				f.git("mv", tc.path, tc.rename)
			} else {
				f.write(tc.path, tc.new)
			}
			m := f.plan(f.commit())
			if tc.want == "" {
				if m.MacOS.Selected || len(m.MacOS.Matched) != 0 {
					t.Fatalf("macOS selected without a darwin build change: %+v", m.MacOS)
				}
				return
			}
			if !m.MacOS.Selected || len(macOSRows(m)) == 0 {
				t.Fatalf("macOS not selected: %+v", m.MacOS)
			}
			found := false
			for _, match := range m.MacOS.Matched {
				found = found || match.Rule == tc.want
			}
			if !found {
				t.Fatalf("no %q match: %+v", tc.want, m.MacOS.Matched)
			}
		})
	}
}

func TestMacOSDeletedFileThatBuiltOnDarwinSelectsPartitions(t *testing.T) {
	f := newFixture(t)
	f.write("leaf/notify.go", "//go:build darwin || linux\n\npackage leaf\nfunc notify() int { return 1 }\n")
	f.base = f.commit()
	if err := os.Remove(filepath.Join(f.root, "leaf", "notify.go")); err != nil {
		t.Fatal(err)
	}
	m := f.plan(f.commit())
	if !m.MacOS.Selected || !slices.Contains(m.MacOS.Matched, MacOSMatch{Path: "leaf/notify.go", Rule: "darwin build constraint"}) {
		t.Fatalf("deleted darwin-built file did not select macOS: %+v", m.MacOS)
	}
}

// macOSOnlyTestsPlan changes a source file of a package that builds only on
// the macOS runner's architecture. No macOS path rule matches the change, yet
// the package has no Linux or Windows test job.
func macOSOnlyTestsPlan(f *fixture) Manifest {
	f.write("macpkg/value_arm64.go", "package macpkg\nfunc Value() int { return 42 }\n")
	f.write("macpkg/value_arm64_test.go", "package macpkg\nimport \"testing\"\nfunc TestMacPkg(t *testing.T) { if Value() != 42 { t.Fatal(\"bad\") } }\n")
	f.base = f.commit()
	return f.plan(f.change("macpkg/value_arm64.go"))
}

// A package whose sources and tests build only on macOS has no Linux or
// Windows job. Leaving the macOS rows out would pass its change untested.
func TestMacOSOnlyPackageKeepsMacOSPartitions(t *testing.T) {
	f := newFixture(t)
	m := macOSOnlyTestsPlan(f)
	if !m.MacOS.Selected || len(m.MacOS.Matched) != 0 || !strings.Contains(m.MacOS.Reason, "test jobs only on macOS") || len(macOSRows(m)) == 0 || len(m.Matrix.Include) == 0 {
		t.Fatalf("macOS-only package left without a test job: %+v %v", m.MacOS, m.Matrix)
	}
}

func TestMacOSManifestWithoutTestJobsForSelectedPackagesIsRefused(t *testing.T) {
	f := newFixture(t)
	m := macOSOnlyTestsPlan(f)
	policy, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	target := nativeTarget(t)
	data, _ := json.Marshal(m)
	var bad Manifest
	if err := json.Unmarshal(data, &bad); err != nil {
		t.Fatal(err)
	}
	bad.MacOS.Selected = false
	bad.Matrix = policy.planMatrix(policy.SelectedMatrix(bad.Module, bad.Packages), false)
	bad.SHA256 = bad.Seal()
	_, err = f.planner.Validate(t.Context(), bad, target, bad.Census[target])
	if err == nil || !strings.Contains(err.Error(), "no test jobs") {
		t.Fatalf("manifest whose packages have no test job accepted: %v", err)
	}
}
