package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func syntheticFullMicroBundle() (artifactSelection, mainArtifact, map[string][]byte) {
	selection, metadata, daemon := syntheticMainArtifact()
	files := map[string][]byte{mainArtifactBinary: daemon}
	bundle := &mainMicroBundle{Version: 1, FixtureRevision: strings.Repeat("c", 40), FixtureTree: strings.Repeat("d", 40), SourceTree: strings.Repeat("e", 40), Tests: map[string]packagedExecutable{}}
	revision, modified := metadata.Revision, "false"
	for _, pkg := range microPackages {
		file := microTestName(pkg)
		data := []byte("synthetic executable for " + pkg)
		observed := executableObservation{Path: "github.com/zzet/gortex/" + pkg + ".test", Module: "github.com/zzet/gortex", Version: "(devel)", GoVersion: runtime.Version(), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Bytes: int64(len(data)), Settings: maps.Clone(metadata.Settings), EmbeddedRevision: &revision, EmbeddedModified: &modified}
		bundle.Tests[pkg] = packagedExecutable{File: file, Observed: observed}
		files[file] = data
	}
	data := []byte("synthetic benchstat executable")
	bundle.Benchstat = packagedExecutable{File: bundledBenchstatName, Observed: executableObservation{Path: "golang.org/x/perf/cmd/benchstat", Module: "golang.org/x/perf", Version: benchstatVersion, GoVersion: runtime.Version(), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Bytes: int64(len(data)), Settings: map[string]string{"GOOS": metadata.OS, "GOARCH": metadata.Arch}}}
	files[bundledBenchstatName] = data
	metadata.Microbench = bundle
	return selection, metadata, files
}

func TestFullMainMicroBundleRefusesMissingAlteredOrUnprovenExecutables(t *testing.T) {
	for _, mutation := range []string{"", "missing-package", "missing-bytes", "missing-vcs", "dirty-vcs", "wrong-package", "wrong-tool-pin", "tool-tamper", "empty-tool", "missing-fixture"} {
		t.Run(map[string]string{"": "complete"}[mutation]+mutation, func(t *testing.T) {
			_, metadata, files := syntheticFullMicroBundle()
			bundle := metadata.Microbench
			switch mutation {
			case "missing-package":
				delete(bundle.Tests, microPackages[0])
			case "missing-bytes":
				delete(files, microTestName(microPackages[0]))
			case "missing-vcs":
				record := bundle.Tests[microPackages[0]]
				record.Observed.EmbeddedRevision = nil
				bundle.Tests[microPackages[0]] = record
			case "dirty-vcs":
				record := bundle.Tests[microPackages[0]]
				value := "true"
				record.Observed.EmbeddedModified = &value
				bundle.Tests[microPackages[0]] = record
			case "wrong-package":
				record := bundle.Tests[microPackages[0]]
				record.Observed.Path = "command-line-arguments"
				bundle.Tests[microPackages[0]] = record
			case "wrong-tool-pin":
				bundle.Benchstat.Observed.Version = "v0.0.0"
			case "tool-tamper":
				files[bundledBenchstatName] = []byte("tampered")
			case "empty-tool":
				files[bundledBenchstatName] = nil
				bundle.Benchstat.Observed.Bytes = 0
				bundle.Benchstat.Observed.SHA256 = fmt.Sprintf("%x", sha256.Sum256(nil))
			case "missing-fixture":
				bundle.FixtureRevision = ""
			}
			err := validateMicroBundle(metadata, files)
			if mutation == "" && err != nil || mutation != "" && err == nil {
				t.Fatalf("wrong executable bundle result: %v", err)
			}
		})
	}
}

func TestFullMicroBundleExtractionKeepsExactExecutableInventory(t *testing.T) {
	selection, metadata, files := syntheticFullMicroBundle()
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	files["main-artifact.json"] = data
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, data := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	selection.Artifact.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(archive.Bytes()))
	out := filepath.Join(t.TempDir(), "bundle")
	if err := extractMainArtifact(selection, archive.Bytes(), out); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifiedMainBundle(out); err != nil {
		t.Fatal(err)
	}
	for _, pkg := range microPackages {
		info, err := os.Stat(filepath.Join(out, microTestName(pkg)))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Fatal("benchmark execute mode lost on download")
		}
	}
	if err := os.WriteFile(filepath.Join(out, microTestName(microPackages[0])), []byte("altered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifiedMainBundle(out); err == nil {
		t.Fatal("retained main benchmark binary changed without refusal")
	}
}

func TestPreparedTestBuildRejectsDirtySourceBeforeCompilerAndRetainsFailure(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("CGO_CFLAGS", "-O2 -g -fno-strict-aliasing")
	t.Setenv("CGO_CXXFLAGS", "-O2 -g -fno-strict-aliasing")
	for _, dirty := range []bool{true, false} {
		t.Run(fmt.Sprint(dirty), func(t *testing.T) {
			revision := strings.Repeat("a", 40)
			out := filepath.Join(t.TempDir(), "builds")
			compiles := 0
			process := func(_ context.Context, binary string, args []string, _ string, _ []string) ([]byte, []byte, error) {
				if binary == "go" {
					compiles++
					if !strings.Contains(strings.Join(args, " "), "-buildvcs=true -mod=readonly -p=2") {
						t.Fatal("test compilation flags drifted")
					}
					return nil, nil, nil
				}
				switch strings.Join(args, " ") {
				case "rev-parse HEAD":
					return []byte(revision), nil, nil
				case "rev-parse HEAD^{tree}":
					return []byte(strings.Repeat("b", 40)), nil, nil
				case "status --porcelain --untracked-files=normal":
					if dirty {
						return []byte(" M fixture.go"), nil, nil
					}
				}
				return nil, nil, nil
			}
			if err := prepareTestBinaries(context.Background(), "/synthetic/source", revision, out, process); err == nil {
				t.Fatal("dirty source/missing executable was accepted")
			}
			if dirty && compiles != 0 || !dirty && compiles != 1 {
				t.Fatal("dirty source compiled or missing binary was discarded")
			}
			var receipt testBuildReceipt
			if err := readStrictJSON(filepath.Join(out, testBuildReceiptName), &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Complete || receipt.Error == "" {
				t.Fatal("failed test compilation lost evidence")
			}
		})
	}
}
