package indexer

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

func dirtyManifestBuild(t *testing.T, store *store_sqlite.Store, repoDir string) (int64, BuildReport) {
	t.Helper()
	generationID, report, err := builderNewBuilder(store).BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildDirtyLayer: %v", err)
	}
	return generationID, report
}

// dirtyManifestExpected renders the manifest a build over repoDir's current
// state must record, from an independent sample and the builder's own
// admission.
func dirtyManifestExpected(t *testing.T, store *store_sqlite.Store, repoDir string, identity GenerationIdentity) (gitstate.DirtySnapshot, store_sqlite.InputManifestMeta, []store_sqlite.InputManifestEntry) {
	t.Helper()
	snap, err := gitstate.SampleDirty(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	target, err := source.NewFilesystemSource(repoDir)
	if err != nil {
		t.Fatalf("open checkout: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })
	b := builderNewBuilder(store)
	meta, entries := admittedManifest(snap, b.manifestAdmitter(repoDir, target), b.dirtyManifestPolicyDigest(StampDirtyLayerIdentity(identity, snap)))
	return snap, meta, entries
}

func dirtyManifestRead(t *testing.T, store *store_sqlite.Store, generationID int64) (store_sqlite.InputManifestMeta, []store_sqlite.InputManifestEntry) {
	t.Helper()
	meta, entries, ok, err := store.AtGeneration(generationID).InputManifest(context.Background())
	if err != nil || !ok {
		t.Fatalf("read manifest of generation %d: ok=%v err=%v", generationID, ok, err)
	}
	return meta, entries
}

func dirtyManifestHeadHolds(t *testing.T, repoDir string) func(string) bool {
	t.Helper()
	head := strings.Split(builderGit(t, repoDir, "ls-tree", "-r", "--name-only", "HEAD"), "\n")
	return func(p string) bool { return slices.Contains(head, p) }
}

func TestDirtyLayerPublishesFullInputManifest(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := builderDirtyCheckout(t, store)
	// A path nothing extracts still belongs to the sample, with its verdict.
	builderWriteFile(t, repoDir, "notes.unknownext", "not source\n")

	generationID, _ := dirtyManifestBuild(t, store, repoDir)
	meta, entries := dirtyManifestRead(t, store, generationID)
	snap, wantMeta, wantEntries := dirtyManifestExpected(t, store, repoDir, builderDirtyIdentity())

	if !meta.IsFull || meta.ManifestVersion != store_sqlite.InputManifestVersion || meta.EntryCount != len(entries) || meta.PolicyDigest == "" {
		t.Fatalf("meta=%+v", meta)
	}
	if meta != wantMeta {
		t.Fatalf("meta=%+v want %+v", meta, wantMeta)
	}
	if !reflect.DeepEqual(entries, wantEntries) {
		t.Fatalf("entries=%+v\nwant   %+v", entries, wantEntries)
	}
	// Entries equal the sample: one per reported path, same content identity.
	var samplePaths []string
	for _, c := range snap.Contents {
		samplePaths = append(samplePaths, c.Path)
	}
	var paths []string
	byPath := map[string]store_sqlite.InputManifestEntry{}
	for _, e := range entries {
		paths = append(paths, e.FilePath)
		byPath[e.FilePath] = e
	}
	if !slices.Equal(paths, samplePaths) {
		t.Fatalf("manifest paths %v, sample paths %v", paths, samplePaths)
	}
	for _, c := range snap.Contents {
		e := byPath[c.Path]
		if c.State == gitstate.DirtyContentPresent && !c.HeadEqual && (e.State != store_sqlite.InputManifestPresent || e.ContentSHA256 != c.SHA256 || e.Mode != c.Mode) {
			t.Errorf("%s: entry %+v does not carry sample %+v", c.Path, e, c)
		}
	}
	for p, want := range map[string]store_sqlite.InputManifestEntryState{
		"core.go": store_sqlite.InputManifestPresent, "helper.go": store_sqlite.InputManifestPresent,
		"added.go": store_sqlite.InputManifestPresent, "newname.go": store_sqlite.InputManifestPresent,
		"gone.go": store_sqlite.InputManifestAbsent, "oldname.go": store_sqlite.InputManifestAbsent,
	} {
		if byPath[p].State != want {
			t.Errorf("%s: state %q want %q", p, byPath[p].State, want)
		}
	}
	if got := byPath["core.go"].Admission; got != store_sqlite.InputManifestAdmitted {
		t.Errorf("core.go admission %q", got)
	}
	if got := byPath["notes.unknownext"].Admission; got != store_sqlite.InputManifestUnknownLanguage {
		t.Errorf("notes.unknownext admission %q", got)
	}
	if got := byPath["gone.go"].Admission; got != store_sqlite.InputManifestNotApplicable {
		t.Errorf("gone.go admission %q", got)
	}
	// The manifest is sealed with the payload.
	if err := store.AtGeneration(generationID).WriteInputManifest(context.Background(), meta, entries); err == nil {
		t.Fatal("manifest of a published generation accepted a rewrite")
	}
}

func TestTornDirtyBuildLeavesNoReadyManifest(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := builderDirtyCheckout(t, store)
	generationID, _, err := builderNewBuilder(store).BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		buildBarrier: func() {
			builderWriteFile(t, repoDir, "sneaked.go", "package fixture\n\nfunc Sneaked() {\n}\n")
		},
	})
	if err == nil {
		t.Fatal("a torn build published")
	}
	row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if err != nil || !found {
		t.Fatalf("read generation %d: found=%v err=%v", generationID, found, err)
	}
	if row.State == store_sqlite.ViewGenerationReady || row.PublishedAt != 0 {
		t.Fatalf("torn generation %d is %s published_at=%d", generationID, row.State, row.PublishedAt)
	}
	// Whatever the torn build wrote describes a state that never existed, and
	// its generation can never become a parent: no Ready generation carries a
	// manifest at all.
	rows, err := store.Catalog().ListViewGenerations(context.Background(), store_sqlite.ViewGenerationFilter{})
	if err != nil {
		t.Fatalf("list generations: %v", err)
	}
	for _, r := range rows {
		if r.GenerationID == 0 || r.State != store_sqlite.ViewGenerationReady {
			continue
		}
		if _, _, ok, err := store.AtGeneration(r.GenerationID).InputManifest(context.Background()); err != nil || ok {
			t.Fatalf("ready generation %d carries a manifest after a torn build: ok=%v err=%v", r.GenerationID, ok, err)
		}
	}
}

func TestInputManifestSurvivesStoreReopen(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "reopen.sqlite")
	store := builderOpenStoreAt(t, storePath)
	repoDir := builderDirtyCheckout(t, store)

	firstSample, firstMeta, firstEntries := dirtyManifestExpected(t, store, repoDir, builderDirtyIdentity())
	first, _ := dirtyManifestBuild(t, store, repoDir)

	builderWriteFile(t, repoDir, "island.go", "package fixture\n\nfunc Island() {\n\t// edited\n}\n")
	secondSample, secondMeta, secondEntries := dirtyManifestExpected(t, store, repoDir, builderDirtyIdentity())
	if secondSample.Fingerprint == firstSample.Fingerprint {
		t.Fatal("the edit did not move the fingerprint")
	}
	second, _ := dirtyManifestBuild(t, store, repoDir)
	if first == second {
		t.Fatalf("both states built generation %d", first)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := builderOpenStoreAt(t, storePath)
	t.Cleanup(func() { _ = reopened.Close() })
	gotFirstMeta, gotFirst := dirtyManifestRead(t, reopened, first)
	gotSecondMeta, gotSecond := dirtyManifestRead(t, reopened, second)
	if gotFirstMeta != firstMeta || !reflect.DeepEqual(gotFirst, firstEntries) {
		t.Fatalf("first manifest after reopen differs from its sample")
	}
	if gotSecondMeta != secondMeta || !reflect.DeepEqual(gotSecond, secondEntries) {
		t.Fatalf("second manifest after reopen differs from its sample")
	}

	heads := dirtyManifestHeadHolds(t, repoDir)
	stored, storedReason := planDelta(resolvedFromFull(gotFirstMeta, gotFirst), resolvedFromFull(gotSecondMeta, gotSecond), heads)
	sampled, sampledReason := planDelta(resolvedFromFull(firstMeta, firstEntries), resolvedFromFull(secondMeta, secondEntries), heads)
	if storedReason != "" || sampledReason != "" {
		t.Fatalf("reasons stored=%q sampled=%q", storedReason, sampledReason)
	}
	want := []LayerPathChange{{Path: "island.go", Kind: LayerPathModified}}
	if !reflect.DeepEqual(stored, want) || !reflect.DeepEqual(sampled, want) {
		t.Fatalf("delta stored=%+v sampled=%+v want %+v", stored, sampled, want)
	}
}
