package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

func newRepoLoadTestMulti(t *testing.T) *MultiIndexer {
	t.Helper()
	cm, err := config.NewConfigManager("")
	require.NoError(t, err)
	return NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
}

func writeRepoLoadSource(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc LoadProbe() {}\n"), 0o644))
}

// linkedWorktreeLayout lays out a main checkout and one linked worktree the
// way git does, without running git: ResolveWorktree reads only files.
func linkedWorktreeLayout(t *testing.T) (main, worktree string) {
	t.Helper()
	base := t.TempDir()
	main = filepath.Join(base, "main")
	worktree = filepath.Join(base, "wt")
	admin := filepath.Join(main, ".git", "worktrees", "wt")
	require.NoError(t, os.MkdirAll(admin, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+admin+"\n"), 0o644))
	return main, worktree
}

func TestExpectRepos_RecordsConfiguredReposAsLoading(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	present := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")

	mi.ExpectRepos([]config.RepoEntry{{Path: present}, {Path: present}, {Path: missing}})

	loads := mi.RepoLoads()
	require.Len(t, loads, 2, "a duplicate entry must not add a second record")
	rec, ok := mi.RepoLoadFor(present)
	require.True(t, ok)
	assert.Equal(t, RepoLoadQueued, rec.State)
	assert.True(t, rec.State.Loading())

	gone, ok := mi.RepoLoadFor(missing)
	require.True(t, ok)
	assert.Equal(t, RepoLoadMissing, gone.State, "a configured path that does not exist is terminal at once")
}

func TestExpectRepos_ReannouncementDropsUnconfiguredLoadingRecords(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	keep, drop := t.TempDir(), t.TempDir()
	mi.ExpectRepos([]config.RepoEntry{{Path: keep}, {Path: drop}})
	mi.StartRepoLoad(keep)

	mi.ExpectRepos([]config.RepoEntry{{Path: keep}})

	_, dropped := mi.RepoLoadFor(drop)
	assert.False(t, dropped, "an entry warmup will never load must not read as loading forever")
	rec, ok := mi.RepoLoadFor(keep)
	require.True(t, ok)
	assert.Equal(t, RepoLoadRunning, rec.State, "a re-announcement keeps an existing record's progress")
}

func TestFinishRepoLoad_EndsLoadingWithAReason(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	failed, silent, vanished := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "later-gone")
	require.NoError(t, os.MkdirAll(vanished, 0o755))
	mi.ExpectRepos([]config.RepoEntry{{Path: failed}, {Path: silent}, {Path: vanished}})
	for _, p := range []string{failed, silent, vanished} {
		mi.StartRepoLoad(p)
	}
	require.NoError(t, os.RemoveAll(vanished))

	mi.FinishRepoLoad(failed, errors.New("indexing exploded"))
	mi.FinishRepoLoad(silent, nil)
	mi.FinishRepoLoad(vanished, errors.New("path does not exist"))

	rec, _ := mi.RepoLoadFor(failed)
	assert.Equal(t, RepoLoadFailed, rec.State)
	assert.Equal(t, "indexing exploded", rec.Reason)

	rec, _ = mi.RepoLoadFor(silent)
	assert.Equal(t, RepoLoadFailed, rec.State, "a job that ends without registering is terminal, not loading")
	assert.NotEmpty(t, rec.Reason)

	rec, _ = mi.RepoLoadFor(vanished)
	assert.Equal(t, RepoLoadMissing, rec.State)
}

func TestRepoRegistration_MarksReadyAndAnnouncesOnce(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	root := filepath.Join(t.TempDir(), "probe")
	writeRepoLoadSource(t, root)
	mi.ExpectRepos([]config.RepoEntry{{Path: root}})

	var announced atomic.Int32
	got := make(chan RepoLoad, 4)
	mi.OnRepoRegistered(func(rec RepoLoad) {
		announced.Add(1)
		got <- rec
	})

	mi.StartRepoLoad(root)
	res, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: root})
	require.NoError(t, err)
	mi.FinishRepoLoad(root, nil)

	select {
	case rec := <-got:
		assert.Equal(t, RepoLoadReady, rec.State)
		assert.Equal(t, res.RepoPrefix, rec.Prefix)
	case <-time.After(5 * time.Second):
		t.Fatal("registration was never announced")
	}
	rec, _ := mi.RepoLoadFor(root)
	assert.Equal(t, RepoLoadReady, rec.State, "FinishRepoLoad must not demote a registered repository")

	// A rewrite of the existing entry is not a registration: re-indexing must
	// neither clear every session's scope nor announce ready again.
	_, err = mi.IndexRepo(res.RepoPrefix)
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, 1, announced.Load())
}

func TestPendingRepoForPath(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	main, worktree := linkedWorktreeLayout(t)
	registered := filepath.Join(t.TempDir(), "registered")
	writeRepoLoadSource(t, registered)
	mi.ExpectRepos([]config.RepoEntry{{Path: main}, {Path: registered}})
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: registered})
	require.NoError(t, err)

	for name, path := range map[string]string{
		"root":            main,
		"subdirectory":    filepath.Join(main, "internal", "pkg"),
		"parent of root":  filepath.Dir(main),
		"linked worktree": filepath.Join(worktree, "pkg"),
	} {
		t.Run(name, func(t *testing.T) {
			rec, pending := mi.PendingRepoForPath(path)
			require.True(t, pending)
			assert.Equal(t, RepoLoadQueued, rec.State)
			assert.Equal(t, absRepoRoot(main), rec.Root)
		})
	}

	t.Run("registered repository is not pending", func(t *testing.T) {
		_, pending := mi.PendingRepoForPath(filepath.Join(registered, "main.go"))
		assert.False(t, pending)
	})
	t.Run("unconfigured path is not pending", func(t *testing.T) {
		_, pending := mi.PendingRepoForPath(t.TempDir())
		assert.False(t, pending)
	})
	t.Run("worktree of a primary that finished loading is not pending", func(t *testing.T) {
		mi.StartRepoLoad(main)
		mi.FinishRepoLoad(main, errors.New("boom"))
		_, pending := mi.PendingRepoForPath(worktree)
		assert.False(t, pending, "only a loading primary claims its worktrees")
		rec, pending := mi.PendingRepoForPath(main)
		require.True(t, pending, "the failed root itself still reports its state")
		assert.Equal(t, RepoLoadFailed, rec.State)
	})
}

// The admission gate and the probe read the load state on every frame; it
// must answer while a topology mutation holds the registry lock.
func TestRepoLoadReadsDoNotTakeRegistryLock(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	root := t.TempDir()
	mi.ExpectRepos([]config.RepoEntry{{Path: root}})

	mi.mu.Lock()
	defer mi.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = mi.RepoLoadFor(root)
		_, _ = mi.PendingRepoForPath(filepath.Join(root, "x"))
		_ = mi.RepoLoads()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("load-state reads blocked behind MultiIndexer.mu")
	}
}

func TestForgetRepoLoad_DropsTheRecord(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	root := t.TempDir()
	mi.markRepoRegistered("probe", root)
	_, ok := mi.RepoLoadFor(root)
	require.True(t, ok)

	mi.forgetRepoLoad(root)
	_, ok = mi.RepoLoadFor(root)
	assert.False(t, ok)
}

// On a restart, seeding applies coordinators before warmup registers any
// primary, so an admitted coordinator meets an unregistered primary and is
// skipped. Its primary's registration must start it, instead of leaving it to
// the janitor an hour later.
func TestPrimaryRegisteredStartsTheCoordinatorsThatWaitedForIt(t *testing.T) {
	f := newFamilyFixture(t, "await-primary")
	defer f.close()
	ctx := context.Background()

	f.restart()
	require.NoError(t, f.lc.Seed(ctx))
	require.Nil(t, f.mi.GetMetadata(f.mainPrefix), "the restart window: the primary is bound in the catalog but not registered")
	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)

	f.lc.ensureCoordinator(ctx, f.primaryGraph, checkout)
	require.False(t, f.lc.hasCoordinator(checkout.CheckoutID), "no coordinator can start before its primary registers")

	f.lc.PrimaryRegistered("some-other-repo")
	require.False(t, f.lc.hasCoordinator(checkout.CheckoutID), "another repository's registration must not start it")

	_, err = f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: f.main, Name: f.mainPrefix})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	require.False(t, f.lc.hasCoordinator(checkout.CheckoutID),
		"registration alone starts nothing; the daemon's registration listener calls PrimaryRegistered")
	f.lc.PrimaryRegistered(f.mainPrefix)
	require.Eventually(t, func() bool { return f.lc.hasCoordinator(checkout.CheckoutID) },
		5*time.Second, 20*time.Millisecond, "the primary's registration must start the coordinator that waited for it")
}

// A selected restart cohort must all be retried by registration, rather than
// only the first checkout noticed before the primary's index was registered.
// These are real Git worktrees and durable catalog identities; this test is not
// the separate reference-host warm-restart workload or its latency evidence.
func TestPrimaryRegisteredStartsAll24WaitingCoordinators(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	main := f.gitRepo("await-cohort-main")
	const wanted = 24
	for i := range wanted {
		f.worktreeOf(main, fmt.Sprintf("await-cohort-%02d", i))
	}
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "await-cohort-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	f.restart()
	require.NoError(t, f.lc.Seed(ctx))
	require.Nil(t, f.mi.GetMetadata(tracked.Prefix))
	checkouts, err := f.catalog.ListCheckouts(ctx, tracked.FamilyID)
	require.NoError(t, err)
	require.Len(t, checkouts, wanted+1, "the primary and every linked checkout must survive restart")
	for _, checkout := range checkouts {
		if checkout.CheckoutID == tracked.CheckoutID {
			continue
		}
		// The same startup edge as an activation reaching ensureCoordinator:
		// the catalog identity exists, but its primary is not registered yet.
		f.lc.ensureCoordinator(ctx, tracked.GraphID, checkout)
		require.False(t, f.lc.hasCoordinator(checkout.CheckoutID))
	}
	f.lc.awaitingPrimaryMu.Lock()
	waiting := len(f.lc.awaitingPrimary[tracked.GraphID])
	f.lc.awaitingPrimaryMu.Unlock()
	require.Equal(t, wanted, waiting)

	f.lc.PrimaryRegistered("unrelated-primary")
	require.Zero(t, f.lc.LiveCoordinators(tracked.FamilyID),
		"another repository's event must not wake this cohort")
	registered := make(chan time.Time, 1)
	f.mi.OnRepoRegistered(func(rec RepoLoad) {
		if rec.Prefix == tracked.Prefix {
			registered <- time.Now()
		}
		f.lc.PrimaryRegistered(rec.Prefix)
	})
	_, err = f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: main, Name: tracked.Prefix})
	require.NoError(t, err)
	var registeredAt time.Time
	select {
	case registeredAt = <-registered:
	case <-time.After(2 * time.Second):
		t.Fatal("the real registration event did not drive the retry")
	}
	remaining := time.Until(registeredAt.Add(2 * time.Second))
	require.Positive(t, remaining, "the retry must start within the registration budget")
	require.Eventually(t, func() bool {
		return f.lc.LiveCoordinators(tracked.FamilyID) == wanted
	}, remaining, 5*time.Millisecond,
		"all waiting build loops must be live without a janitor sweep")
	t.Logf("%d/%d coordinators live %s after primary registration", wanted, wanted, time.Since(registeredAt))
	assert.Empty(t, f.lc.CoordinatorStartFailures())
	f.lc.awaitingPrimaryMu.Lock()
	waiting = len(f.lc.awaitingPrimary[tracked.GraphID])
	f.lc.awaitingPrimaryMu.Unlock()
	assert.Zero(t, waiting, "successful retries must drain the waiting cohort")
}

func TestDeclareRepos_DefersFilesystemIdentityUntilWarmup(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	missing := filepath.Join(t.TempDir(), "not-mounted")
	entries := []config.RepoEntry{{Path: missing, Name: "pending"}, {Path: missing}}
	mi.DeclareRepos(entries)
	require.Len(t, mi.RepoLoads(), 1)
	rec, ok := mi.RepoLoadFor(missing)
	require.True(t, ok)
	assert.Equal(t, RepoLoadQueued, rec.State, "declaration must not stat a configured root before Listen")
	assert.True(t, rec.identityPending)
	assert.Empty(t, rec.Reason)
	rec, pending := mi.PendingRepoForPath(filepath.Join(missing, "pkg"))
	require.True(t, pending)
	assert.Equal(t, "pending", rec.Prefix)

	mi.ExpectRepos(entries)
	rec, ok = mi.RepoLoadFor(missing)
	require.True(t, ok)
	assert.Equal(t, RepoLoadMissing, rec.State, "warmup still diagnoses missing paths")
	assert.False(t, rec.identityPending)
}

func TestDeclareRepos_WarmupEnrichmentPreservesRunningState(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	root, worktree := linkedWorktreeLayout(t)
	entries := []config.RepoEntry{{Path: root}}
	mi.DeclareRepos(entries)
	_, pending := mi.PendingRepoForPath(filepath.Join(worktree, "pkg"))
	require.False(t, pending, "an unresolved declaration cannot name the worktree's family without request filesystem work")
	require.True(t, mi.RepoIdentityPending(), "negative startup admission must remain generically retryable until the owner resolves identity")

	mi.StartRepoLoad(root)
	before, _ := mi.RepoLoadFor(root)
	mi.ExpectRepos(entries)
	after, ok := mi.RepoLoadFor(root)
	require.True(t, ok)
	assert.Equal(t, RepoLoadRunning, after.State)
	assert.Equal(t, before.Since, after.Since)
	assert.False(t, after.identityPending)
	rec, pending := mi.PendingRepoForPath(worktree)
	assert.True(t, pending)
	assert.Equal(t, root, rec.Root, "the owner now proves the linked worktree's configured family")

	mi.DeclareRepos(nil)
	assert.Empty(t, mi.RepoLoads(), "removed declarations must not remain loading forever")
}

// The daemon invalidates cached scopes and publishes workspace_readiness from
// OnRepoRegistered. A real file-save mutation must not trigger that boot event.
func TestRepoRegistration_FileSaveDoesNotAnnounceAgain(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	root := filepath.Join(t.TempDir(), "saved")
	writeRepoLoadSource(t, root)
	mi.ExpectRepos([]config.RepoEntry{{Path: root}})
	announced := make(chan RepoLoad, 4)
	mi.OnRepoRegistered(func(rec RepoLoad) { announced <- rec })
	res, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: root})
	require.NoError(t, err)
	select {
	case <-announced:
	case <-time.After(time.Second):
		t.Fatal("initial cold registration was not announced")
	}
	before, ok := mi.RepoLoadFor(root)
	require.True(t, ok)
	require.Equal(t, RepoLoadReady, before.State)
	idx := mi.GetIndexer(res.RepoPrefix)
	require.NotNil(t, idx)
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc SavedAfterRegistration() {}\n"), 0o644))
	require.NoError(t, idx.IndexFile("main.go"))
	after, ok := mi.RepoLoadFor(root)
	require.True(t, ok)
	assert.Equal(t, before.Since, after.Since, "a save must not create another registration epoch")
	assert.Equal(t, before.State, after.State)
	select {
	case rec := <-announced:
		t.Fatalf("file save emitted a boot readiness/scope-invalidation event: %+v", rec)
	case <-time.After(100 * time.Millisecond):
	}
}
