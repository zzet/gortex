package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// builderDirtyCheckout builds a repository at tree A, indexes it into store
// while it is still clean, and then makes the working tree diverge from HEAD
// in every way git distinguishes: a staged modification, an unstaged one, an
// unstaged deletion, a staged rename, and an untracked add. The returned
// directory is the checkout the dirty layer describes.
func builderDirtyCheckout(t *testing.T, store *store_sqlite.Store) string {
	t.Helper()
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "checkout")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, builderTreeA())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")

	// The corpus is the committed content, so it is indexed before anything
	// diverges from it.
	builderIndex(t, store, repoDir)

	target := builderTreeB()
	builderWriteFile(t, repoDir, "core.go", target["core.go"])
	builderGit(t, repoDir, "add", "core.go")

	builderWriteFile(t, repoDir, "helper.go", `package fixture

func Helper() {
	// reworked in the working tree
}
`)
	if err := os.Remove(filepath.Join(repoDir, "gone.go")); err != nil {
		t.Fatalf("remove gone.go: %v", err)
	}
	builderGit(t, repoDir, "mv", "oldname.go", "newname.go")
	builderWriteFile(t, repoDir, "added.go", target["added.go"])
	return repoDir
}

func builderWriteFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func builderDirtyIdentity() GenerationIdentity {
	return GenerationIdentity{
		OwnerKind:  "dedicated_graph",
		GraphID:    "graph-fixture",
		LayerID:    "layer-worktree",
		CheckoutID: "checkout-fixture",
	}
}

// TestDirtyLayerChangeMapping pins how git's working-tree vocabulary lands in
// the layer's three-kind one. The distinctions that get collapsed are the
// point: a layer describes what a reader sees on disk, so whether a change is
// staged, and whether an addition was ever added to the index, say nothing
// about the content.
func TestDirtyLayerChangeMapping(t *testing.T) {
	snap := gitstate.DirtySnapshot{Entries: []gitstate.DirtyEntry{
		{Path: "staged.go", Kind: gitstate.DirtyModified, Staged: true},
		{Path: "unstaged.go", Kind: gitstate.DirtyModified, Unstaged: true},
		{Path: "fresh.go", Kind: gitstate.DirtyAdded, Staged: true},
		{Path: "untracked.go", Kind: gitstate.DirtyUntracked, Unstaged: true},
		{Path: "gone.go", Kind: gitstate.DirtyDeleted},
		{Path: "moved.go", Kind: gitstate.DirtyRenamedFrom, OldPath: "was.go"},
		{Path: "was.go", Kind: gitstate.DirtyDeleted},
		{Path: "exec.go", Kind: gitstate.DirtyModeChanged},
		{Path: "link.go", Kind: gitstate.DirtySymlinkChanged},
		{Path: "vendored", Kind: gitstate.DirtyModified, Submodule: true},
		// Two entries on one path: the staged half of a change plus its
		// unstaged half. The present claim has to win over the deletion, and
		// the path must appear once.
		{Path: "both.go", Kind: gitstate.DirtyDeleted, Staged: true},
		{Path: "both.go", Kind: gitstate.DirtyAdded, Unstaged: true},
		// `git add f && rm f` — porcelain "AD". gitstate emits one entry and
		// lets the staged column decide, so the mapping sees an add. Only the
		// checkout can say the path is gone, which is dirtyLayerDiskTruth's
		// job and not this function's.
		{Path: "addedthengone.go", Kind: gitstate.DirtyAdded, Staged: true, Unstaged: true},
	}}

	want := map[string]LayerChangeKind{
		"staged.go":        LayerPathModified,
		"unstaged.go":      LayerPathModified,
		"fresh.go":         LayerPathAdded,
		"untracked.go":     LayerPathAdded,
		"gone.go":          LayerPathDeleted,
		"moved.go":         LayerPathAdded,
		"was.go":           LayerPathDeleted,
		"exec.go":          LayerPathModified,
		"link.go":          LayerPathModified,
		"both.go":          LayerPathAdded,
		"addedthengone.go": LayerPathAdded,
	}
	got := make(map[string]LayerChangeKind)
	for _, change := range dirtyLayerChanges(snap) {
		if _, duplicate := got[change.Path]; duplicate {
			t.Fatalf("%s appears twice in the change set", change.Path)
		}
		got[change.Path] = change.Kind
	}
	if len(got) != len(want) {
		t.Fatalf("mapped %d paths, want %d: %v", len(got), len(want), got)
	}
	for path, kind := range want {
		if got[path] != kind {
			t.Errorf("%s mapped to %q, want %q", path, got[path], kind)
		}
	}
}

// TestDirtyLayerDiskTruthDemotesAVanishedPath pins the second half of the
// mapping: git's staged column can call a path present that the working tree
// no longer holds, and the checkout is what decides. A path that is simply
// unreadable is left alone — the build refuses it later rather than masking
// the layer below behind an I/O error.
func TestDirtyLayerDiskTruthDemotesAVanishedPath(t *testing.T) {
	dir := builderTempDir(t, "checkout")
	builderWriteFile(t, dir, "here.go", "package fixture\n")
	if err := os.Mkdir(filepath.Join(dir, "dir.go"), 0o755); err != nil {
		t.Fatalf("mkdir dir.go: %v", err)
	}
	target, err := source.NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	got := dirtyLayerDiskTruth([]LayerPathChange{
		{Path: "here.go", Kind: LayerPathModified},
		{Path: "addedthengone.go", Kind: LayerPathAdded},
		{Path: "stagedthengone.go", Kind: LayerPathModified},
		// A directory is not content either, so a path that turned into one
		// is as absent as one that vanished.
		{Path: "dir.go", Kind: LayerPathAdded},
		{Path: "already.go", Kind: LayerPathDeleted},
	}, target)

	want := map[string]LayerChangeKind{
		"here.go":           LayerPathModified,
		"addedthengone.go":  LayerPathDeleted,
		"stagedthengone.go": LayerPathDeleted,
		"dir.go":            LayerPathDeleted,
		"already.go":        LayerPathDeleted,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %v", len(got), len(want), got)
	}
	for _, change := range got {
		if want[change.Path] != change.Kind {
			t.Errorf("%s is %q, want %q", change.Path, change.Kind, want[change.Path])
		}
	}
}

// --- acceptance 2: the working-tree layer -------------------------------

func TestDirtyLayerComposesLikeAFlatIndex(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := builderDirtyCheckout(t, store)

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
	if report.ClosureTruncated {
		t.Fatalf("closure truncated at %d in a six-file fixture", report.ClosureCap)
	}
	// caller.go is dirty in no sense at all — it is the dependent the closure
	// exists to find. island.go is neither dirty nor a dependent and must stay
	// out of the generation entirely.
	if !slices.Contains(report.ClosurePaths, "caller.go") {
		t.Fatalf("closure %v does not carry caller.go", report.ClosurePaths)
	}
	if slices.Contains(report.IndexedPaths, "island.go") {
		t.Fatalf("island.go is in the generation's file set %v — the build is not sparse", report.IndexedPaths)
	}
	if report.DeleteMasks != 2 {
		t.Fatalf("delete masks = %d, want gone.go and the rename's source", report.DeleteMasks)
	}

	// The reference is a plain whole index of the working tree as it stands —
	// staged, unstaged, untracked and deleted state all included, because that
	// is exactly what is on disk.
	flat := builderOpenStore(t, "flat")
	builderIndex(t, flat, repoDir)

	composed := builderComposed(t, store, generationID)
	if corpus := builderNodeIDs(store.AtGeneration(0)); slices.Equal(corpus, builderNodeIDs(composed)) {
		t.Fatalf("the composed view carries the corpus's identities verbatim — the layer changed nothing")
	}
	builderAssertReadersAgree(t, composed, flat)
	builderAssertMasksValidate(t, store, generationID)
}

// TestDirtyLayerFollowsTheDiskWhenAStagedChangeWasDeleted is the same
// acceptance, run over the checkout state git describes with two columns that
// disagree: a file staged as an add and then removed ("AD"), and a tracked
// file staged as a modification and then removed ("MD"). git's staged column
// names both present; the disk holds neither, and the layer has to say what
// the disk says or refuse a legal checkout forever.
func TestDirtyLayerFollowsTheDiskWhenAStagedChangeWasDeleted(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := builderDirtyCheckout(t, store)

	builderWriteFile(t, repoDir, "staged.go", `package fixture

func Staged() {
}
`)
	builderGit(t, repoDir, "add", "staged.go")
	if err := os.Remove(filepath.Join(repoDir, "staged.go")); err != nil {
		t.Fatalf("remove staged.go: %v", err)
	}

	builderWriteFile(t, repoDir, "island.go", `package fixture

func Island() {
	// staged, then removed from the working tree
}
`)
	builderGit(t, repoDir, "add", "island.go")
	if err := os.Remove(filepath.Join(repoDir, "island.go")); err != nil {
		t.Fatalf("remove island.go: %v", err)
	}

	generationID, report, err := builderNewBuilder(store).BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildDirtyLayer refused a checkout with a staged-then-deleted path: %v", err)
	}
	if report.DeleteMasks != 4 {
		t.Fatalf("delete masks = %d, want gone.go, the rename's source, staged.go and island.go", report.DeleteMasks)
	}
	for _, gone := range []string{"staged.go", "island.go"} {
		if slices.Contains(report.IndexedPaths, gone) {
			t.Errorf("%s is in the generation's file set %v — the layer planned to read a path that is not there",
				gone, report.IndexedPaths)
		}
	}

	flat := builderOpenStore(t, "flat")
	builderIndex(t, flat, repoDir)

	composed := builderComposed(t, store, generationID)
	if n := composed.GetNode(builderRepoPrefix + "/island.go::Island"); n != nil {
		t.Errorf("a path the working tree no longer holds still shows through: %s", builderRenderNode(n))
	}
	builderAssertReadersAgree(t, composed, flat)
	builderAssertMasksValidate(t, store, generationID)
}

// --- acceptance 3: the torn-snapshot refusal ----------------------------

func TestDirtyLayerSupersedesAChangedCheckout(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := builderDirtyCheckout(t, store)

	generationID, _, err := builderNewBuilder(store).BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		// The window the fingerprint check exists to close: the payload is
		// written and the checkout moves before the publish.
		buildBarrier: func() {
			builderWriteFile(t, repoDir, "sneaked.go", `package fixture

func Sneaked() {
}
`)
		},
	})
	if err == nil {
		t.Fatal("BuildDirtyLayer published a generation built from a checkout that moved under it")
	}
	if !errors.Is(err, ErrDirtySnapshotChanged) {
		t.Fatalf("error is %v, want one that unwraps to ErrDirtySnapshotChanged", err)
	}
	var changed *DirtySnapshotChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("error %v does not carry the fingerprints that disagreed", err)
	}
	if !changed.Retryable() {
		t.Fatal("a checkout that moved is retryable — one more build against the state it has now may succeed")
	}
	if changed.Before == changed.After || changed.Before == "" || changed.After == "" {
		t.Fatalf("fingerprints %q and %q do not name a real disagreement", changed.Before, changed.After)
	}

	// The generation must be readable by nothing: not published, and marked so
	// a router cannot pick it up while a sweep is still allowed to collect it.
	row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if err != nil || !found {
		t.Fatalf("read generation %d: found=%v err=%v", generationID, found, err)
	}
	if row.State != store_sqlite.ViewGenerationSuperseded {
		t.Fatalf("generation %d is %s, want superseded", generationID, row.State)
	}
	if row.PublishedAt != 0 {
		t.Fatalf("generation %d carries a publish timestamp %d", generationID, row.PublishedAt)
	}
}
