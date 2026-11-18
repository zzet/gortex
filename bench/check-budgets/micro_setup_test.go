package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestFixtureSetupPinsOneOwnedRepositoryAndRetainsFailures(t *testing.T) {
	for _, mutation := range []string{"", "wrong-head", "dirty", "fetch-failure"} {
		t.Run(map[string]string{"": "complete", "wrong-head": "wrong-head", "dirty": "dirty", "fetch-failure": "fetch-failure"}[mutation], func(t *testing.T) {
			revision := strings.Repeat("a", 40)
			out := filepath.Join(t.TempDir(), "owned-fixture")
			calls := 0
			process := func(_ context.Context, binary string, args []string, cwd string, _ []string) ([]byte, []byte, error) {
				calls++
				if binary != "git" || cwd != filepath.Join(out, "repo") {
					t.Fatal("fixture command escaped owned repository")
				}
				switch strings.Join(args, " ") {
				case "remote add origin " + fixtureRepository:
				case "fetch --quiet --depth=1 --no-tags origin " + revision:
					if mutation == "fetch-failure" {
						return nil, []byte("synthetic fetch failure\n"), errors.New("synthetic fetch refused")
					}
				case "rev-parse HEAD":
					if mutation == "wrong-head" {
						return []byte(strings.Repeat("b", 40)), nil, nil
					}
					return []byte(revision), nil, nil
				case "rev-parse HEAD^{tree}":
					return []byte(strings.Repeat("c", 40)), nil, nil
				case "status --porcelain --untracked-files=normal":
					if mutation == "dirty" {
						return []byte(" M fixture.go\n"), nil, nil
					}
				case "--version":
					return []byte("git version synthetic\n"), nil, nil
				}
				return nil, nil, nil
			}
			err := prepareFixture(context.Background(), revision, out, process)
			if mutation == "" && err != nil || mutation != "" && err == nil {
				t.Fatalf("wrong setup result: %v", err)
			}
			var receipt fixtureSetup
			if err := readStrictJSON(filepath.Join(out, "fixture.json"), &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Repository != fixtureRepository || receipt.Revision != revision || receipt.Kind != "fixture_setup_only" || receipt.Complete != (mutation == "") {
				t.Fatal("fixture provenance/failure disappeared")
			}
			before := calls
			if err := prepareFixture(context.Background(), revision, out, process); err == nil || calls != before {
				t.Fatal("existing fixture was overwritten or reused")
			}
		})
	}
}

func TestPinnedToolSetupUsesExistingPinAndRetainsInstallFailure(t *testing.T) {
	out := filepath.Join(t.TempDir(), "owned-tool")
	process := func(_ context.Context, binary string, args []string, cwd string, env []string) ([]byte, []byte, error) {
		if binary != "go" || !slices.Equal(args, []string{"install", "golang.org/x/perf/cmd/benchstat@" + benchstatVersion}) || cwd != out || !slices.Contains(env, "GOBIN="+out) || !slices.Contains(env, "GOTOOLCHAIN=local") {
			t.Fatal("tool version or install destination was not pinned")
		}
		return nil, []byte("synthetic install refusal\n"), errors.New("synthetic install failure")
	}
	if err := prepareBenchstat(context.Background(), out, process); err == nil {
		t.Fatal("failed tool install became success")
	}
	var receipt pinnedToolSetup
	if err := readStrictJSON(filepath.Join(out, "tool.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Complete || receipt.Error == "" || receipt.ModulePin != "golang.org/x/perf/cmd/benchstat@"+benchstatVersion {
		t.Fatal("tool failure/version evidence missing")
	}
}

func TestExecutableObservationDoesNotInventEmbeddedVCS(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	observed, err := observeExecutable(binary)
	if err != nil {
		t.Fatal(err)
	}
	if observed.GoVersion != runtime.Version() || !planPattern.MatchString(observed.SHA256) || observed.Bytes <= 0 {
		t.Fatal("actual executable identity missing")
	}
	revision, present := observed.Settings["vcs.revision"]
	if present && (observed.EmbeddedRevision == nil || *observed.EmbeddedRevision != revision) || !present && observed.EmbeddedRevision != nil {
		t.Fatal("embedded VCS observation replaced by an inferred revision")
	}
	data, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "embedded_vcs_revision") {
		t.Fatal("unavailable embedded provenance was silently omitted")
	}
}
