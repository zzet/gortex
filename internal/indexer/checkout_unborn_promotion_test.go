package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
)

// A configured repository may legitimately be empty when an agent first asks
// Gortex to track it. This covers the complete cold-start path that used to
// roll that repository back: synthetic empty base, zero-diff commit layer,
// dirty layer containing every visible source file, and then the first real
// commit diffing from Git's canonical empty tree.
func TestConfiguredUnbornCheckoutPromotesAndTransitionsToFirstCommit(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := filepath.Join(f.dir, "unborn-primary")
	require.NoError(t, os.MkdirAll(root, 0o755))
	runGit(t, root, "init", "-q", "-b", "main")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	runGit(t, root, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(root, ".gitignore"), "ignored.go\n")
	writeFile(t, filepath.Join(root, "staged.go"), "package unborn\n\nfunc StagedOnly() {}\n")
	writeFile(t, filepath.Join(root, "loose.go"), "package unborn\n\nfunc LooseOnly() {}\n")
	writeFile(t, filepath.Join(root, "ignored.go"), "package unborn\n\nfunc IgnoredOnly() {}\n")
	runGit(t, root, "add", "staged.go")

	gc := f.cm.Global()
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: root, Name: "unborn-primary"}))
	require.NoError(t, gc.Save())

	builds := make(chan struct{}, 4)
	f.lc.indexBarrier = func() { builds <- struct{}{} }
	require.NoError(t, f.lc.Seed(ctx))
	require.Len(t, builds, 1, "cold seed builds one immutable empty corpus")

	checkout := f.checkoutOf("unborn-primary")
	graph := f.familyOf("unborn-primary")
	require.Positive(t, graph.ActiveGenerationID)
	emptyTree, err := gitstate.CanonicalHeadTreeOID(ctx, root, "", "")
	require.NoError(t, err)

	base, found, err := f.catalog.GetViewGeneration(ctx, graph.ActiveGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, emptyTree, base.TreeOID)
	assert.Empty(t, contentIdentities(f.store.AtGeneration(base.GenerationID), "unborn-primary"),
		"untracked files must live in the dirty overlay, not leak into the immutable base")

	route, routed := f.routeOf(checkout.CheckoutID)
	require.True(t, routed)
	require.Positive(t, route.CommitGenerationID)
	require.Positive(t, route.DirtyGenerationID)
	commitBefore, found, err := f.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	dirtyBefore, found, err := f.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, emptyTree, commitBefore.TreeOID)
	assert.Equal(t, emptyTree, dirtyBefore.TreeOID)

	view := f.materialize(checkout.CheckoutID)
	initial := contentIdentities(view.Reader, "unborn-primary")
	view.Close()
	assert.Contains(t, initial, "staged.go::StagedOnly")
	assert.Contains(t, initial, "loose.go::LooseOnly")
	assert.NotContains(t, initial, "ignored.go::IgnoredOnly")

	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-q", "-m", "first")
	firstTree := builderGit(t, root, "rev-parse", "HEAD^{tree}")
	cycle := f.runCoordinator(checkout.CheckoutID)
	assert.True(t, cycle.CommitBuilt, "the first commit must build empty-tree -> first-tree")
	require.Positive(t, cycle.CommitGenerationID)
	require.Positive(t, cycle.DirtyGenerationID)

	graphAfter := f.familyOf("unborn-primary")
	assert.Equal(t, graph.ActiveGenerationID, graphAfter.ActiveGenerationID,
		"the canonical empty base remains immutable after the first commit")
	routeAfter, routed := f.routeOf(checkout.CheckoutID)
	require.True(t, routed)
	commitAfter, found, err := f.catalog.GetViewGeneration(ctx, routeAfter.CommitGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	dirtyAfter, found, err := f.catalog.GetViewGeneration(ctx, routeAfter.DirtyGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, firstTree, commitAfter.TreeOID)
	assert.Equal(t, firstTree, dirtyAfter.TreeOID)

	view = f.materialize(checkout.CheckoutID)
	after := contentIdentities(view.Reader, "unborn-primary")
	view.Close()
	assert.Contains(t, after, "staged.go::StagedOnly")
	assert.Contains(t, after, "loose.go::LooseOnly")
	assert.NotContains(t, after, "ignored.go::IgnoredOnly")
}

func TestCheckoutCoordinatorCachesUnbornTreeWithoutRepositoryAccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "unborn")
	require.NoError(t, os.MkdirAll(root, 0o755))
	runGit(t, root, "init", "-q", "-b", "main")
	coordinator := &CheckoutCoordinator{root: root}
	snapshot := gitstate.DirtySnapshot{}

	first, err := coordinator.canonicalDirtySnapshot(context.Background(), snapshot)
	require.NoError(t, err)
	require.NotEmpty(t, first.HeadTree)

	// The second normalization must not consult Git. Removing its administrative
	// directory turns any subprocess fallback into a deterministic failure while
	// leaving the coordinator's immutable per-root cache usable.
	require.NoError(t, os.Rename(filepath.Join(root, ".git"), filepath.Join(root, ".git-away")))
	second, err := coordinator.canonicalDirtySnapshot(context.Background(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, first.HeadTree, second.HeadTree)
}

func BenchmarkCheckoutCoordinatorCanonicalUnbornSnapshotCached(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skipf("git not on PATH: %v", err)
	}
	root := filepath.Join(b.TempDir(), "unborn")
	cmd := exec.Command("git", "init", "-q", "-b", "main", "--", root)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("git init: %v\n%s", err, out)
	}
	coordinator := &CheckoutCoordinator{root: root}
	snapshot := gitstate.DirtySnapshot{}
	if _, err := coordinator.canonicalDirtySnapshot(context.Background(), snapshot); err != nil {
		b.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, ".git"), filepath.Join(root, ".git-away")); err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := coordinator.canonicalDirtySnapshot(ctx, snapshot); err != nil {
			b.Fatal(err)
		}
	}
}
