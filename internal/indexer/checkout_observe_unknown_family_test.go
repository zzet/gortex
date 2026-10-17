package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestObserveCheckoutPathTrackedNonGitUnknownAncestorSkipsInventory(t *testing.T) {
	lc, catalog, primary, familyID, _, _ := newCheckoutObservationFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ancestor := filepath.Join(filepath.Dir(primary), "unknown-ancestor")
	require.NoError(t, os.Mkdir(ancestor, 0o755))
	builderGit(t, ancestor, "init", "--initial-branch=main")
	builderWriteTree(t, ancestor, builderTreeA())
	builderGit(t, ancestor, "add", "-A")
	builderGit(t, ancestor, "commit", "-m", "unknown ancestor")
	root := filepath.Join(ancestor, "competitor")
	require.NoError(t, os.Mkdir(root, 0o755))
	writeFile(t, filepath.Join(root, "competitor.go"), "package competitor\nfunc CompetitorHandler() {}\n")
	_, err := os.Lstat(filepath.Join(root, ".git"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, commonDir, err := gitstate.ResolveFamilyDirs(ctx, root)
	require.NoError(t, err)
	unknownFamily := FamilyIDFor(commonDir)
	require.NotEqual(t, familyID, unknownFamily)
	_, known, err := catalog.GetRepositoryFamily(ctx, unknownFamily)
	require.NoError(t, err)
	require.False(t, known)
	tracked, err := lc.Register(ctx, config.RepoEntry{Path: root, Name: "competitor"}, TrackSourceCLI)
	require.NoError(t, err)
	require.Error(t, tracked.CatalogErr, "the ancestor does not list the independently tracked child as a worktree")
	require.Equal(t, "competitor", tracked.Prefix)
	require.Empty(t, tracked.CheckoutID)
	_, _, prefix, scoped := lc.mi.ScopeForCWD(root)
	require.True(t, scoped)
	require.Equal(t, "competitor", prefix)
	nodes := lc.store.FindNodesByNameInRepo("CompetitorHandler", prefix)
	require.Len(t, nodes, 1, "the supported non-Git root has its own indexed core data")
	require.Equal(t, "competitor/competitor.go", nodes[0].FilePath)
	families, err := catalog.ListRepositoryFamilies(ctx)
	require.NoError(t, err)
	var inventories atomic.Int32
	lc.observationInventory = func(ctx context.Context, path string) (*gitstate.FamilyInventory, error) {
		inventories.Add(1)
		return gitstate.Inventory(ctx, path)
	}
	for range 3 {
		job, err := lc.checkoutObservation(pathkey.CanonicalExistingRoot(root))
		require.NoError(t, err)
		select {
		case <-job.done:
		case <-ctx.Done():
			t.Fatal("negative observation did not finish", ctx.Err())
		}
		require.NoError(t, job.proofErr)
		require.NoError(t, job.err)
		require.Nil(t, job.proof)
		require.False(t, job.found)
		_, _, selected, ok := lc.mi.ScopeForCWD(root)
		require.True(t, ok)
		require.Equal(t, prefix, selected)
		require.Len(t, lc.store.FindNodesByNameInRepo("CompetitorHandler", selected), 1)
		// The worker forgets the completed negative before canceling its context.
		select {
		case <-job.ctx.Done():
		case <-ctx.Done():
			t.Fatal("negative observation did not release its job", ctx.Err())
		}
	}
	require.Zero(t, inventories.Load(), "unknown ancestor families need no worktree inventory")
	after, err := catalog.ListRepositoryFamilies(ctx)
	require.NoError(t, err)
	require.Equal(t, families, after, "ordinary discovery must not adopt the unknown ancestor")
	rows, err := catalog.ListCheckouts(ctx, familyID)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// Preserve the old fixture, then make the exact selected spelling a real
	// worktree of the known family. No remembered negative may hide its identity.
	require.NoError(t, os.Rename(root, root+"-before-worktree"))
	builderGit(t, primary, "worktree", "add", "-b", "competitor-worktree", root)
	job, err := lc.checkoutObservation(pathkey.CanonicalExistingRoot(root))
	require.NoError(t, err)
	select {
	case <-job.proofReady:
	case <-ctx.Done():
		t.Fatal("new known-family proof did not finish", ctx.Err())
	}
	require.NoError(t, job.proofErr)
	require.NotNil(t, job.proof)
	require.Equal(t, familyID, job.proof.familyID)
	require.Equal(t, "primary", job.proof.primary.RepoPrefix)
	job.authorizeOnce.Do(func() { close(job.authorized) })
	select {
	case <-job.done:
	case <-ctx.Done():
		t.Fatal("new known-family observation did not finish", ctx.Err())
	}
	require.NoError(t, job.err)
	require.True(t, job.found)
	checkout, found, err := lc.ObserveCheckoutPath(ctx, root)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, checkout.CheckoutID)
	require.Equal(t, familyID, checkout.FamilyID)
	require.True(t, pathkey.EqualPaths(root, checkout.RootPath))
	require.Equal(t, store_sqlite.CheckoutModeAutomatic, checkout.EffectiveMode)
	require.EqualValues(t, 1, inventories.Load())
	_, known, err = catalog.GetRepositoryFamily(ctx, unknownFamily)
	require.NoError(t, err)
	require.False(t, known)
}

func TestObserveCheckoutPathRejectsInventoryFromAnotherPreflightFamily(t *testing.T) {
	lc, catalog, primary, familyID, _, _ := newCheckoutObservationFixture(t)
	other := filepath.Join(filepath.Dir(primary), "other-family")
	require.NoError(t, os.Mkdir(other, 0o755))
	builderGit(t, other, "init", "--initial-branch=main")
	builderWriteTree(t, other, builderTreeA())
	builderGit(t, other, "add", "-A")
	builderGit(t, other, "commit", "-m", "other family")
	var inventories atomic.Int32
	lc.observationInventory = func(ctx context.Context, _ string) (*gitstate.FamilyInventory, error) {
		inventories.Add(1)
		return gitstate.Inventory(ctx, other)
	}
	proof, err := lc.prepareCheckoutObservation(t.Context(), primary)
	require.ErrorIs(t, err, ErrCheckoutMutationStale)
	require.Nil(t, proof)
	require.EqualValues(t, 1, inventories.Load())
	rows, err := catalog.ListCheckouts(t.Context(), familyID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "a different inventory cannot allocate under the preflight family")
}
