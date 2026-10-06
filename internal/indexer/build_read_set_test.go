package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// readSetCheckout is a committed checkout with two packages and a root
// manifest, indexed into store, with one working-tree edit in pkg/a.go. It
// waits out the change-stamp margin so the committed files read as settled.
func readSetCheckout(t *testing.T, store *store_sqlite.Store) string {
	t.Helper()
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "checkout")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	for name, body := range map[string]string{
		"go.mod":     "module example.com/fixture\n\ngo 1.22\n",
		"pkg/a.go":   "package pkg\n\nfunc A() int { return B() }\n",
		"pkg/b.go":   "package pkg\n\nfunc B() int { return 1 }\n",
		"other/c.go": "package other\n\nfunc C() {}\n",
	} {
		if err := os.MkdirAll(filepath.Join(repoDir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		builderWriteFile(t, repoDir, name, body)
	}
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	builderIndex(t, store, repoDir)
	builderWriteFile(t, repoDir, "pkg/a.go", "package pkg\n\nfunc A() int { return B() + 1 }\n")
	time.Sleep(2 * time.Second / 20) // past readSetChangeMargin
	return repoDir
}

func buildReadSetLayer(t *testing.T, store *store_sqlite.Store, repoDir string, sampler *gitstate.DirtySampler, barrier func()) (int64, error) {
	t.Helper()
	generationID, _, err := builderNewBuilder(store).BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		Sampler:      sampler,
		buildBarrier: barrier,
	})
	return generationID, err
}

func readSetCounts() (confirmed, fallback uint64) {
	return readSetConfirmations.confirmed.Load(), readSetConfirmations.fallback.Load()
}

// A file the build read that changes between the build's sample and its
// publish refuses the publish, whether it is a clean file of the edited
// package or the edited file itself.
func TestPrepublishReadSetRefusesAReadFileThatChanged(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
	}{
		{"clean_sibling", "pkg/b.go", "package pkg\n\nfunc B() int { return 2 }\n"},
		{"edited_file", "pkg/a.go", "package pkg\n\nfunc A() int { return B() + 2 }\n"},
		{"root_manifest", "go.mod", "module example.com/fixture\n\ngo 1.23\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "base")
			repoDir := readSetCheckout(t, store)
			sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
			if err != nil {
				t.Fatal(err)
			}
			confirmedBefore, _ := readSetCounts()
			generationID, err := buildReadSetLayer(t, store, repoDir, sampler, func() {
				builderWriteFile(t, repoDir, tc.path, tc.body)
			})
			if !errors.Is(err, ErrDirtySnapshotChanged) {
				t.Fatalf("a build whose read file %s changed published: err=%v", tc.path, err)
			}
			if confirmed, _ := readSetCounts(); confirmed != confirmedBefore {
				t.Fatal("the read set confirmed a build whose read file changed")
			}
			row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
			if err != nil || !found || row.State != store_sqlite.ViewGenerationFailed {
				t.Fatalf("generation %d: found=%v state=%v err=%v, want failed", generationID, found, row.State, err)
			}
		})
	}
}

// A file the build did not read that appears mid-build does not make the
// payload wrong for the fingerprint it names: the read set confirms it
// without a full sample, and the next sample (the next cycle) sees the new
// state.
func TestPrepublishReadSetAcceptsAnUnrelatedNewFile(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := readSetCheckout(t, store)
	sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := sampler.ConfirmReadSet(context.Background(), probe, nil, nil)
	if err != nil || (!proof.Confirmed && proof.Reason != "the checkout's filesystem gives no change stamps") {
		t.Fatalf("fixture cannot establish read-set capability: %+v, %v", proof, err)
	}
	confirmedBefore, fallbackBefore := readSetCounts()
	takenBefore := sampler.SamplesTaken()
	generationID, err := buildReadSetLayer(t, store, repoDir, sampler, func() {
		builderWriteFile(t, repoDir, "other/d.go", "package other\n\nfunc D() {}\n")
	})
	wantConfirmed, wantFallback, wantSamples := confirmedBefore+1, fallbackBefore, uint64(1)
	if !proof.Confirmed {
		// Without change stamps the full sample cannot prove this file was
		// unrelated to the payload. It must refuse, not publish stale evidence.
		if !errors.Is(err, ErrDirtySnapshotChanged) {
			t.Fatalf("unsupported read-set proof did not refuse the changed sample: %v", err)
		}
		wantConfirmed, wantFallback, wantSamples = confirmedBefore, fallbackBefore+1, 2
	} else if err != nil {
		t.Fatalf("an unrelated new file refused the publish: %v", err)
	}
	if confirmed, fallback := readSetCounts(); confirmed != wantConfirmed || fallback != wantFallback {
		t.Fatalf("read-set counts = %d/%d, want %d/%d", confirmed, fallback, wantConfirmed, wantFallback)
	}
	if taken := sampler.SamplesTaken() - takenBefore; taken != wantSamples {
		t.Fatalf("the build took %d working-copy samples, want %d", taken, wantSamples)
	}
	row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if err != nil || !found {
		t.Fatalf("generation %d missing: found=%v row=%+v err=%v", generationID, found, row, err)
	}
	if proof.Confirmed && row.PublishedAt == 0 {
		t.Fatalf("confirmed generation %d not published: %+v", generationID, row)
	}
	if !proof.Confirmed && (row.PublishedAt != 0 || row.State != store_sqlite.ViewGenerationFailed) {
		t.Fatalf("unsupported confirmation published or retained a servable generation: %+v", row)
	}
	next, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if next.Fingerprint == row.LowerViewFingerprint {
		t.Fatal("the next sample does not see the unrelated file: the next cycle would not pick it up")
	}
}

// The full re-sample is still the fence when the read set is off.
func TestPrepublishFullResampleSwitch(t *testing.T) {
	t.Setenv(prepublishFullResampleEnv, "1")
	store := builderOpenStore(t, "base")
	repoDir := readSetCheckout(t, store)
	sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = buildReadSetLayer(t, store, repoDir, sampler, func() {
		builderWriteFile(t, repoDir, "other/d.go", "package other\n\nfunc D() {}\n")
	})
	if !errors.Is(err, ErrDirtySnapshotChanged) {
		t.Fatalf("with the full re-sample an unrelated new file must refuse the publish: %v", err)
	}
}

func TestWithBuildReadSetCoversPackagesAndAncestorManifests(t *testing.T) {
	set, ok := buildReadSetFrom(withBuildReadSet(context.Background(),
		[]string{"internal/config/config.go", "main.go", "internal/y/ctx.go"}, []string{"internal/y/ctx.go"}, []string{"internal/x/gone.go"}))
	if !ok {
		t.Fatal("no read set attached")
	}
	want := map[string]bool{
		"internal/config/config.go": true, "main.go": true, "internal/x/gone.go": true, "internal/y/ctx.go": true,
		"go.mod": true, "internal/go.mod": true, "internal/config/go.mod": true, "internal/x/package.json": true, "internal/y/go.mod": true,
	}
	have := map[string]bool{}
	for _, f := range set.files {
		have[f] = true
	}
	for f := range want {
		if !have[f] {
			t.Errorf("read set misses %s: %v", f, set.files)
		}
	}
	// A context file is read alone: its package directory is not listed.
	if got := set.dirs; len(got) != 3 || got[0] != "." || got[1] != "internal/config" || got[2] != "internal/x" {
		t.Fatalf("read dirs = %v, want [. internal/config internal/x]", got)
	}
	if _, ok := buildReadSetFrom(context.Background()); ok {
		t.Fatal("a context without a read set reported one")
	}
}
