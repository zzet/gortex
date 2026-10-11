package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
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

func buildReadSetLayer(t *testing.T, store *store_sqlite.Store, repoDir string, sampler *gitstate.DirtySampler, barrier func(), enrich ...*semantic.Manager) (int64, error) {
	t.Helper()
	builder := builderNewBuilder(store)
	if len(enrich) > 0 {
		builder.Semantic = enrich[0]
	}
	generationID, _, err := builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
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

// A file the build read but did not parse that changes between the build's
// sample and its publish refuses the publish, whether it is a clean file of
// the edited package (which the enrichment stage reads whole) or a manifest.
// (The edited file itself, saved again after the build parsed it, is
// confirmed by its parsed bytes:
// TestPrepublishConfirmsAParsedFileSavedAgainByItsParsedBytes; and without an
// enrichment stage nothing reads the sibling:
// TestPrepublishWithoutAPackageReaderLeavesASiblingSaveToTheNextBuild.)
func TestPrepublishReadSetRefusesAReadFileThatChanged(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	for _, tc := range []struct {
		name, path, body string
	}{
		{"clean_sibling", "pkg/b.go", "package pkg\n\nfunc B() int { return 2 }\n"},
		{"root_manifest", "go.mod", "module example.com/fixture\n\ngo 1.23\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "base")
			repoDir := readSetCheckout(t, store)
			sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
			if err != nil {
				t.Fatal(err)
			}
			spy := &checkoutEnrichSpy{}
			confirmedBefore, _ := readSetCounts()
			generationID, err := buildReadSetLayer(t, store, repoDir, sampler, func() {
				builderWriteFile(t, repoDir, tc.path, tc.body)
			}, enrichmentManager(t, 4, spy))
			if len(spy.enriched()) == 0 {
				t.Fatal("the enrichment stage never ran: the package was not read whole")
			}
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
		// The walk gate reads every ancestor's ignore files to admit a file.
		".gortexignore": true, "internal/.ignore": true, "internal/y/.rgignore": true,
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
	// A directory a reader listed or probed is read whole, the root among
	// them, with the manifests above it.
	claimed, _ := buildReadSetFrom(withBuildReadSetDirs(context.Background(), []string{"main.go"}, []string{"main.go"}, nil, []string{".", "build/x"}))
	if got := claimed.dirs; len(got) != 2 || got[0] != "." || got[1] != "build/x" {
		t.Fatalf("claimed dirs = %v, want [. build/x]", got)
	}
	if !slices.Contains(claimed.files, "build/go.mod") || !slices.Contains(claimed.files, "build/x/.gortexignore") {
		t.Fatalf("claimed directories' manifests and ignore files missing: %v", claimed.files)
	}
}

// The fence confirms a build by the bytes it parsed, not by what the working
// copy holds at the fence. The edited file saved again after the build parsed
// it — with other bytes, or with the very bytes it held — does not refuse the
// publish: the parsed bytes are the sample's, so the fence confirms the
// payload by them (buildContentProof) without a full sample, publishes it
// under the sample's fingerprint and reports that the working copy has moved
// past it. Bytes saved between the sample and the parse are not the sample's,
// and the build is torn although the working copy holds them at the fence.
func TestPrepublishConfirmsAParsedFileSavedAgainByItsParsedBytes(t *testing.T) {
	const edited = "package pkg\n\nfunc A() int { return B() + 1 }\n"
	for _, tc := range []struct {
		name        string
		beforeBuild string
		afterParse  string
		published   bool
	}{
		{name: "saved again after the parse with other bytes", afterParse: "package pkg\n\nfunc A() int { return B() + 2 }\n", published: true},
		{name: "saved again after the parse with the same bytes", afterParse: edited, published: true},
		{name: "saved between the sample and the parse", beforeBuild: "package pkg\n\nfunc A() int { return B() + 3 }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			capability, err := sampler.ConfirmReadSet(context.Background(), probe, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.beforeBuild != "" {
				builderWriteFile(t, repoDir, "pkg/a.go", tc.beforeBuild)
			}
			confirmedBefore, _ := readSetCounts()
			provenBefore := readSetConfirmations.contentProven.Load()
			takenBefore := sampler.SamplesTaken()
			// The daemon's builder: without the contract-core runtime the proof
			// only refutes (TestPrepublishWithoutAPackageReaderLeavesASiblingSaveToTheNextBuild).
			builder := builderNewBuilder(store)
			builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
			generationID, report, err := builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
				Identity:     builderDirtyIdentity(),
				Base:         store,
				CheckoutRoot: repoDir,
				RepoPrefix:   builderRepoPrefix,
				WorkspaceID:  builderRepoPrefix,
				ProjectID:    builderRepoPrefix,
				Sampler:      sampler,
				before:       &probe,
				buildBarrier: func() {
					if tc.afterParse != "" {
						builderWriteFile(t, repoDir, "pkg/a.go", tc.afterParse)
					}
				},
			})
			if !tc.published {
				if !errors.Is(err, ErrDirtySnapshotChanged) {
					t.Fatalf("a build that parsed bytes other than its sample's = generation %d, %v; want torn", generationID, err)
				}
				if row, found, _ := store.Catalog().GetViewGeneration(context.Background(), generationID); found && row.PublishedAt != 0 {
					t.Fatalf("generation %d of bytes other than the sample's was published", generationID)
				}
				return
			}
			if !capability.Confirmed {
				// Without change stamps nothing but a full sample decides, and
				// it sees the save.
				if tc.afterParse != edited && !errors.Is(err, ErrDirtySnapshotChanged) {
					t.Fatalf("without change stamps the moved file must refuse the publish: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a save of a file the build had parsed refused the publish: %v", err)
			}
			if !report.outpacedSample {
				t.Fatal("the build did not report that the working copy moved past its sample")
			}
			row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
			if err != nil || !found || row.PublishedAt == 0 || row.LowerViewFingerprint != probe.Fingerprint {
				t.Fatalf("generation %d: found=%v row=%+v err=%v, want published under the sample %q", generationID, found, row, err, probe.Fingerprint)
			}
			if confirmed, _ := readSetCounts(); confirmed != confirmedBefore+1 || readSetConfirmations.contentProven.Load() != provenBefore+1 {
				t.Fatal("the read set did not confirm the build by its parsed bytes")
			}
			if taken := sampler.SamplesTaken() - takenBefore; taken != 0 {
				t.Fatalf("the build took %d working-copy samples, want none beyond its own", taken)
			}
		})
	}
}

// The build holds its own sample for its lifetime. Twenty freshness proofs
// of other states taken while it runs — more than the sampler keeps in its
// recent-sample cache — then a save of the file it parsed: the fence still
// confirms the build by its parsed bytes against its own sample, and
// publishes it under that sample instead of tearing it.
func TestTwentyProofsDuringABuildDoNotTearIt(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := readSetCheckout(t, store)
	sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capability, err := sampler.ConfirmReadSet(context.Background(), before, nil, nil); err != nil || !capability.Confirmed {
		t.Skipf("the checkout's filesystem gives no change stamps: %+v %v", capability, err)
	}
	proofs := 0
	builder := builderNewBuilder(store)
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	generationID, report, err := builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		Sampler:      sampler,
		before:       &before,
		buildBarrier: func() {
			// An untracked file created after the build's sample beside a
			// file the build never reads: each proof finds it changed.
			for ; proofs < 20; proofs++ {
				builderWriteFile(t, repoDir, "other/probe.txt", fmt.Sprintf("%d\n", proofs+1))
				if _, err := sampler.Sample(context.Background()); err != nil {
					t.Error(err)
					return
				}
			}
			builderWriteFile(t, repoDir, "pkg/a.go", "package pkg\n\nfunc A() int { return B() + 2 }\n")
		},
	})
	if proofs != 20 {
		t.Fatalf("%d proofs ran during the build, want 20", proofs)
	}
	if _, _, cached := sampler.LatestSampleOf(before.Fingerprint, time.Time{}); cached {
		t.Fatal("the build's sample is still in the sampler's recent-sample cache: the test does not exercise eviction")
	}
	if err != nil {
		t.Fatalf("twenty proofs during the build tore it: %v", err)
	}
	row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if err != nil || !found || row.PublishedAt == 0 || row.LowerViewFingerprint != before.Fingerprint || !report.outpacedSample {
		t.Fatalf("generation %d: found=%v row=%+v outpaced=%v err=%v, want published under the build's sample %q",
			generationID, found, row, report.outpacedSample, err, before.Fingerprint)
	}
}

// Without an enrichment stage the build reads the files it parses and
// nothing else of their packages: a clean sibling saved mid-build was never
// read, so the build publishes its sample and the next sample sees the save.
// That needs the contract-core runtime the daemon installs: without it the
// contract refresh reads handler files the proof does not record, so the
// fence claims the parsed file's directory whole and the sibling's save tears
// the build, as before the proof.
func TestPrepublishWithoutAPackageReaderLeavesASiblingSaveToTheNextBuild(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime bool
	}{{"contract-core runtime", true}, {"legacy contract path", false}} {
		t.Run(tc.name, func(t *testing.T) {
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
			capability, err := sampler.ConfirmReadSet(context.Background(), probe, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			builder := builderNewBuilder(store)
			if tc.runtime {
				builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
			}
			generationID, _, err := builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
				Identity:     builderDirtyIdentity(),
				Base:         store,
				CheckoutRoot: repoDir,
				RepoPrefix:   builderRepoPrefix,
				WorkspaceID:  builderRepoPrefix,
				ProjectID:    builderRepoPrefix,
				Sampler:      sampler,
				buildBarrier: func() {
					builderWriteFile(t, repoDir, "pkg/b.go", "package pkg\n\nfunc B() int { return 2 }\n")
				},
			})
			if !capability.Confirmed || !tc.runtime {
				if !errors.Is(err, ErrDirtySnapshotChanged) {
					t.Fatalf("the sibling's save must refuse the publish here: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a save of a file the build never read refused the publish: %v", err)
			}
			row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
			if err != nil || !found || row.LowerViewFingerprint != probe.Fingerprint {
				t.Fatalf("generation %d: found=%v row=%+v err=%v, want the sample %q", generationID, found, row, err, probe.Fingerprint)
			}
			next, err := sampler.Sample(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if next.Fingerprint == row.LowerViewFingerprint {
				t.Fatal("the next sample does not see the sibling's save")
			}
		})
	}
}

// A build that read bytes other than its sample's for a file is torn at the
// fence even when the working copy is back at the sample by then (an undo):
// the payload is another state's, so neither the read set nor a full sample
// that equals the sample may publish it under the sample. With the sample's
// own bytes recorded the same fence publishes.
func TestPrepublishRefusesReadsThatContradictTheSampleAfterAnUndo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ticket bool
	}{{"read set", false}, {"a refresh ticket's full sample", true}} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "base")
			repoDir := readSetCheckout(t, store)
			sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
			if err != nil {
				t.Fatal(err)
			}
			before, err := sampler.Sample(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			fence := func(parsed []byte) (bool, error) {
				proof := newBuildContentProof(repoDir, before)
				proof.record(filepath.Join(repoDir, "pkg", "a.go"), gitstate.BlobSHA256(parsed))
				ctx := proof.readSet(context.Background(), []string{"pkg/a.go"}, nil)
				if tc.ticket {
					ctx = withPrepublishSampleDemand(ctx, func() bool { return true })
				}
				return builderNewBuilder(store).confirmDirtyBuildInputs(ctx, sampler, repoDir, 0, before, proof)
			}
			refuted := readSetConfirmations.refuted.Load()
			// The working copy holds the sample's bytes; the build parsed a
			// save that was undone before the fence.
			outpaced, err := fence([]byte("package pkg\n\nfunc A() int { return B() + 2 }\n"))
			if !errors.Is(err, ErrDirtySnapshotChanged) || outpaced {
				t.Fatalf("fence over contradicting reads = (outpaced %v, %v), want torn", outpaced, err)
			}
			if readSetConfirmations.refuted.Load() != refuted+1 {
				t.Fatal("the fence did not refute the build by its reads")
			}
			current, err := os.ReadFile(filepath.Join(repoDir, "pkg", "a.go"))
			if err != nil {
				t.Fatal(err)
			}
			if outpaced, err := fence(current); err != nil || outpaced {
				t.Fatalf("fence over the sample's bytes = (outpaced %v, %v), want published as the sample", outpaced, err)
			}
		})
	}
}

// The shared-row emitter scan reads unchanged files from disk to decide
// which one keeps a row; a build that proves its reads records those reads
// too, so the fence confirms the scanned files (the read set names them) and
// refutes a scan of bytes other than the sample's.
func TestTheSharedEmitterScanRecordsItsReads(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := readSetCheckout(t, store)
	before, err := gitstate.SampleDirty(context.Background(), repoDir)
	if err != nil {
		t.Fatal(err)
	}
	proof := newBuildContentProof(repoDir, before)
	req := BuildRequest{RootPath: repoDir, contentProof: proof}
	if _, ok := editDeltaReadSource(req, "other/c.go"); !ok {
		t.Fatal("read other/c.go")
	}
	set, _ := buildReadSetFrom(proof.readSet(context.Background(), nil, nil))
	if !slices.Contains(set.files, "other/c.go") {
		t.Fatalf("read set files %v, want the scanned other/c.go", set.files)
	}
	if refuted := proof.contradiction(); refuted != "" {
		t.Fatalf("a scan of the sample's bytes is refuted at %s", refuted)
	}
	builderWriteFile(t, repoDir, "pkg/a.go", "package pkg\n\nfunc A() int { return B() + 3 }\n")
	if _, ok := editDeltaReadSource(req, "pkg/a.go"); !ok {
		t.Fatal("read pkg/a.go")
	}
	if refuted := proof.contradiction(); refuted != "pkg/a.go" {
		t.Fatalf("a scan of bytes saved after the sample is refuted at %q, want pkg/a.go", refuted)
	}
}

// A fence sample the working tree moves under — gitstate refuses it as
// changed while sampling — proves the tree has left the build's sample: the
// fence tears the build with the retryable DirtySnapshotChangedError, as a
// sample that differs would, instead of failing the cycle.
func TestAFenceSampleTheTreeMovesUnderTearsTheBuild(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := readSetCheckout(t, store)
	sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	written := make(chan struct{})
	go func() {
		defer close(written)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(repoDir, "pkg", "a.go"), fmt.Appendf(nil, "package pkg\n\nfunc A() int { return B() + %d }\n", i), 0o644)
		}
	}()
	defer func() { close(stop); <-written }()
	b := builderNewBuilder(store)
	raced, tries := 0, 0
	for deadline := time.Now().Add(20 * time.Second); raced == 0 && time.Now().Before(deadline); tries++ {
		err := b.confirmDirtySnapshotWith(context.Background(), sampler, repoDir, 0, before.Fingerprint)
		switch {
		case errors.Is(err, ErrDirtySnapshotChanged):
			if strings.Contains(err.Error(), heldBySample) {
				raced++
			}
		case err != nil:
			t.Fatalf("a fence sample of a moving tree failed instead of tearing the build: %v", err)
		}
	}
	if raced == 0 {
		t.Skipf("the sampler never caught the tree moving under it in %d samples", tries)
	}
	t.Logf("the tree moved under %d of %d fence samples", raced, tries)
}
