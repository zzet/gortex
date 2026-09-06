package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

func newCheckoutObservationFixture(t testing.TB) (*CheckoutLifecycle, *store_sqlite.Catalog, string, string, string, func()) {
	t.Helper()
	builderIsolateGit(t)
	dir := builderTempDir(t, "observe")
	primary := filepath.Join(dir, "primary")
	if err := os.Mkdir(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	builderGit(t, primary, "init", "--initial-branch=main")
	builderWriteTree(t, primary, builderTreeA())
	builderGit(t, primary, "add", "-A")
	builderGit(t, primary, "commit", "-m", "primary")
	store := builderOpenStore(t, "observe")
	configPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(configPath)
	if err := gc.Save(); err != nil {
		t.Fatal(err)
	}
	cm, err := config.NewConfigManager(configPath)
	if err != nil {
		t.Fatal(err)
	}
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	lc, err := NewCheckoutLifecycle(CheckoutLifecycleConfig{MultiIndexer: mi, ConfigManager: cm, Graph: store, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	closeFixture := func() { once.Do(func() { _ = lc.Close(); _ = mi.Close(context.Background()) }) }
	t.Cleanup(closeFixture)
	tracked, err := lc.Register(context.Background(), config.RepoEntry{Path: primary, Name: "primary"}, TrackSourceCLI)
	if err != nil || tracked.CatalogErr != nil {
		t.Fatalf("register primary: %v %v", err, tracked.CatalogErr)
	}
	// Only metadata is measured/asserted here. A closed TEST-LOCAL build gate
	// makes any accidental synchronous indexing observable as a stalled call.
	lc.SetBuildGate(NewViewBuildGate())
	return lc, store.Catalog(), primary, tracked.FamilyID, configPath, closeFixture
}

// Slow Git startup may outlive a request's wait budget, but retries must join
// the same lifecycle-owned work. Only a typed pending outcome is retryable.
func observeCheckoutUntilSettled(t testing.TB, lc *CheckoutLifecycle, path string, authorize ...func(string) error) (store_sqlite.Checkout, bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		started := time.Now()
		checkout, found, err := lc.ObserveCheckoutPath(ctx, path, authorize...)
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("discovery request exceeded bounded wait: %s", elapsed)
		}
		if !errors.Is(err, ErrCheckoutMutationBusy) || ctx.Err() != nil {
			return checkout, found, err
		}
		select {
		case <-ctx.Done():
			return store_sqlite.Checkout{}, false, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func TestObserveCheckoutPathDiscoversNewWorktreeWithoutTracking(t *testing.T) {
	lc, catalog, primary, familyID, configPath, _ := newCheckoutObservationFixture(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(filepath.Dir(primary), "late")
	builderGit(t, primary, "worktree", "add", "-b", "late", worktree)
	if err := os.Mkdir(filepath.Join(worktree, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	checkout, found, err := observeCheckoutUntilSettled(t, lc, filepath.Join(worktree, "nested"))
	if err != nil || !found || checkout.FamilyID != familyID || !pathkey.EqualPaths(checkout.RootPath, pathkey.CanonicalExistingRoot(worktree)) || checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic || checkout.DesiredMode != store_sqlite.CheckoutModeAutomatic {
		t.Fatalf("new CWD observation: %+v found=%v error=%v", checkout, found, err)
	}
	graphs, err := catalog.ListDedicatedGraphs(t.Context(), familyID)
	if err != nil || len(graphs) != 1 {
		t.Fatalf("implicit worktree got a dedicated graph: %+v %v", graphs, err)
	}
	intents, err := catalog.ListTrackingIntents(t.Context(), checkout.CheckoutID)
	if err != nil || len(intents) != 0 {
		t.Fatalf("implicit worktree got explicit intent: %+v %v", intents, err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("implicit observation changed config: %v", err)
	}
	again, found, err := observeCheckoutUntilSettled(t, lc, worktree)
	if err != nil || !found || again.CheckoutID != checkout.CheckoutID || again.Incarnation != checkout.Incarnation {
		t.Fatalf("identity changed on repeated observation: %+v %v", again, err)
	}
	lc.coordMu.Lock()
	_, activating := lc.coordinatorActivating[checkout.CheckoutID]
	live := lc.coordinators[checkout.CheckoutID] != nil
	lc.coordMu.Unlock()
	if !activating && !live {
		t.Fatal("selected worktree was not activated")
	}
}

func TestObserveCheckoutPathUnknownFamilyAndDeniedScopeHaveNoSideEffects(t *testing.T) {
	lc, catalog, primary, familyID, configPath, _ := newCheckoutObservationFixture(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(filepath.Dir(primary), "unknown")
	if err := os.Mkdir(unknown, 0o755); err != nil {
		t.Fatal(err)
	}
	builderGit(t, unknown, "init", "--initial-branch=main")
	if _, found, err := observeCheckoutUntilSettled(t, lc, unknown); err != nil || found {
		t.Fatalf("unknown family was adopted: %v %v", found, err)
	}
	inv, err := gitstate.Inventory(t.Context(), unknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := catalog.GetRepositoryFamily(t.Context(), FamilyIDFor(inv.CommonDir)); err != nil || found {
		t.Fatalf("unknown family was recorded: %v %v", found, err)
	}
	worktree := filepath.Join(filepath.Dir(primary), "denied")
	builderGit(t, primary, "worktree", "add", "-b", "denied", worktree)
	denied := errors.New("outside authorized workspace")
	called := false
	if _, found, err := observeCheckoutUntilSettled(t, lc, worktree, func(prefix string) error {
		called = true
		if prefix != "primary" {
			t.Fatalf("authorizer got wrong primary %q", prefix)
		}
		return denied
	}); !errors.Is(err, denied) || found || !called {
		t.Fatalf("unauthorized observation: found=%v error=%v called=%v", found, err, called)
	}
	rows, err := catalog.ListCheckouts(t.Context(), familyID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("authorization happened after allocation: %+v %v", rows, err)
	}
	lc.coordMu.Lock()
	active := len(lc.coordinators) + len(lc.coordinatorActivating)
	lc.coordMu.Unlock()
	if active != 0 {
		t.Fatal("denied observation started a coordinator")
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(before) != string(after) {
		t.Fatalf("refused observation changed config: %v", err)
	}
}

func TestObserveCheckoutPathCancellationAndClosingDoNotAllocate(t *testing.T) {
	lc, catalog, primary, familyID, _, closeFixture := newCheckoutObservationFixture(t)
	worktree := filepath.Join(filepath.Dir(primary), "canceled")
	builderGit(t, primary, "worktree", "add", "-b", "canceled", worktree)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, found, err := lc.ObserveCheckoutPath(ctx, worktree); !errors.Is(err, context.Canceled) || found {
		t.Fatalf("canceled observation: %v %v", found, err)
	}
	closeFixture()
	if _, found, err := lc.ObserveCheckoutPath(t.Context(), worktree); !errors.Is(err, ErrCheckoutRefreshStopped) || found {
		t.Fatalf("closed observation: %v %v", found, err)
	}
	rows, err := catalog.ListCheckouts(t.Context(), familyID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("canceled observation allocated: %+v %v", rows, err)
	}
}

func TestObserveCheckoutPathSlowMetadataReturnsRetryableBusy(t *testing.T) {
	lc, catalog, primary, familyID, _, _ := newCheckoutObservationFixture(t)
	worktree := filepath.Join(filepath.Dir(primary), "slow")
	builderGit(t, primary, "worktree", "add", "-b", "slow", worktree)
	reconcile.WithHEADSampler(func(ctx context.Context, _ string) (gitstate.HEADState, error) {
		<-ctx.Done()
		return gitstate.HEADState{}, errors.New("injected Git subprocess killed")
	})(lc.rec)
	started := time.Now()
	_, found, observeErr := lc.ObserveCheckoutPath(t.Context(), worktree)
	if found || !errors.Is(observeErr, ErrCheckoutMutationBusy) || !errors.Is(observeErr, context.DeadlineExceeded) {
		t.Fatalf("slow discovery lost bounded retry state: found=%v err=%v", found, observeErr)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("metadata admission escaped its deadline")
	}
	rows, err := catalog.ListCheckouts(t.Context(), familyID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expired metadata admission allocated a checkout: %+v %v", rows, err)
	}
}

func TestObserveCheckoutPathSlowInventoryMakesProgressAcrossRetries(t *testing.T) {
	lc, catalog, primary, familyID, _, _ := newCheckoutObservationFixture(t)
	worktree := filepath.Join(filepath.Dir(primary), "slow-inventory")
	builderGit(t, primary, "worktree", "add", "-b", "slow-inventory", worktree)
	var calls atomic.Int32
	lc.observationInventory = func(ctx context.Context, path string) (*gitstate.FamilyInventory, error) {
		calls.Add(1)
		timer := time.NewTimer(350 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return gitstate.Inventory(ctx, path)
		}
	}
	started := time.Now()
	_, found, err := lc.ObserveCheckoutPath(t.Context(), worktree)
	if found || !errors.Is(err, ErrCheckoutMutationBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow inventory did not return bounded pending: found=%v err=%v", found, err)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond || elapsed > time.Second {
		t.Fatalf("unexpected discovery wait: %s", elapsed)
	}
	checkout, found, err := observeCheckoutUntilSettled(t, lc, worktree)
	if err != nil || !found || checkout.FamilyID != familyID {
		t.Fatalf("retry did not finish continuing discovery: %+v found=%v err=%v", checkout, found, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("retry restarted slow inventory %d times", got)
	}
	rows, err := catalog.ListCheckouts(t.Context(), familyID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("continuing job did not allocate exactly one checkout: %+v %v", rows, err)
	}

	// A successfully shared result still requires each caller's authorization.
	denied := errors.New("another session cannot access this checkout")
	_, found, err = lc.ObserveCheckoutPath(t.Context(), worktree, func(string) error { return denied })
	if found || !errors.Is(err, denied) {
		t.Fatalf("cached result escaped caller authorization: found=%v err=%v", found, err)
	}
}

func TestObserveCheckoutPathSlowObservationRunsOnceAcrossRetries(t *testing.T) {
	lc, _, primary, _, _, _ := newCheckoutObservationFixture(t)
	worktree := filepath.Join(filepath.Dir(primary), "slow-observe")
	builderGit(t, primary, "worktree", "add", "-b", "slow-observe", worktree)
	var calls atomic.Int32
	reconcile.WithHEADSampler(func(ctx context.Context, root string) (gitstate.HEADState, error) {
		calls.Add(1)
		timer := time.NewTimer(350 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return gitstate.HEADState{}, ctx.Err()
		case <-timer.C:
			return gitstate.SampleHEAD(ctx, root)
		}
	})(lc.rec)
	_, found, err := lc.ObserveCheckoutPath(t.Context(), worktree)
	if found || !errors.Is(err, ErrCheckoutMutationBusy) {
		t.Fatalf("slow observation did not return pending: found=%v err=%v", found, err)
	}
	checkout, found, err := observeCheckoutUntilSettled(t, lc, worktree)
	if err != nil || !found || checkout.CheckoutID == "" {
		t.Fatalf("slow observation never progressed: %+v found=%v err=%v", checkout, found, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("retry restarted HEAD metadata %d times", got)
	}
}

func TestObserveCheckoutPathExpiredJobsStayBoundedUntilTheyDrain(t *testing.T) {
	lc, catalog, primary, familyID, _, _ := newCheckoutObservationFixture(t)
	release := make(chan struct{})
	defer close(release)
	var started atomic.Int32
	lc.observationInventory = func(context.Context, string) (*gitstate.FamilyInventory, error) {
		started.Add(1)
		<-release // Deliberately model a dependency that ignores cancellation.
		return nil, context.Canceled
	}
	jobs := make([]*checkoutObservationJob, 0, checkoutObservationCapacity)
	for i := 0; i < checkoutObservationCapacity; i++ {
		job, err := lc.checkoutObservation(filepath.Join(primary, fmt.Sprintf("request-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
	}
	deadline := time.Now().Add(time.Second)
	for started.Load() != checkoutObservationCapacity && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := started.Load(); got != checkoutObservationCapacity {
		t.Fatalf("workers did not start: %d", got)
	}
	for _, job := range jobs {
		job.cancel() // Expiry must not release capacity before the worker exits.
	}
	for i := 0; i < 2*checkoutObservationCapacity; i++ {
		if _, err := lc.checkoutObservation(filepath.Join(primary, fmt.Sprintf("extra-%d", i))); !errors.Is(err, ErrCheckoutMutationBusy) {
			t.Fatalf("canceled but running jobs released capacity: %v", err)
		}
	}
	lc.observationMu.Lock()
	active := len(lc.observationJobs)
	lc.observationMu.Unlock()
	if active != checkoutObservationCapacity || started.Load() != checkoutObservationCapacity {
		t.Fatalf("discovery bound escaped: jobs=%d workers=%d", active, started.Load())
	}
	rows, err := catalog.ListCheckouts(t.Context(), familyID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read-only jobs allocated checkout state: %+v %v", rows, err)
	}
}

func TestObserveCheckoutPathCloseCancelsAndJoinsMetadata(t *testing.T) {
	lc, _, primary, _, _, _ := newCheckoutObservationFixture(t)
	entered := make(chan struct{})
	exited := make(chan struct{})
	lc.observationInventory = func(ctx context.Context, _ string) (*gitstate.FamilyInventory, error) {
		close(entered)
		<-ctx.Done()
		close(exited)
		return nil, ctx.Err()
	}
	if _, err := lc.checkoutObservation(primary); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := lc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Close returned before metadata work stopped")
	}
	lc.observationMu.Lock()
	active := len(lc.observationJobs)
	lc.observationMu.Unlock()
	if active != 0 {
		t.Fatalf("Close retained %d discovery jobs", active)
	}
	if _, err := lc.checkoutObservation(primary); !errors.Is(err, ErrCheckoutRefreshStopped) {
		t.Fatalf("closed lifecycle admitted new work: %v", err)
	}
}

func TestObserveCheckoutPathRejectsInventoryWithReboundGitMarker(t *testing.T) {
	lc, catalog, primary, familyID, _, _ := newCheckoutObservationFixture(t)
	worktree := filepath.Join(filepath.Dir(primary), "rebound")
	sibling := filepath.Join(filepath.Dir(primary), "sibling")
	builderGit(t, primary, "worktree", "add", "-b", "rebound", worktree)
	builderGit(t, primary, "worktree", "add", "-b", "sibling", sibling)
	otherMarker, err := os.ReadFile(filepath.Join(sibling, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	lc.observationInventory = func(ctx context.Context, path string) (*gitstate.FamilyInventory, error) {
		inv, err := gitstate.Inventory(ctx, path)
		if err != nil {
			return nil, err
		}
		// Force the exact gap between Inventory and the later proof snapshots.
		if err := os.WriteFile(filepath.Join(worktree, ".git"), otherMarker, 0o644); err != nil {
			return nil, err
		}
		return inv, nil
	}
	authorized := false
	_, found, err := observeCheckoutUntilSettled(t, lc, worktree, func(string) error {
		authorized = true
		return nil
	})
	if found || !errors.Is(err, ErrCheckoutMutationStale) || authorized {
		t.Fatalf("old inventory acquired new binding authority: found=%v err=%v authorized=%v", found, err, authorized)
	}
	rows, err := catalog.ListCheckouts(t.Context(), familyID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("inconsistent proof allocated checkout: %+v %v", rows, err)
	}
}

func TestObserveCheckoutPathRejectsBindingChangesWhileProofIsPending(t *testing.T) {
	for _, change := range []string{"root", "git_marker", "commondir_marker", "common_directory"} {
		t.Run(change, func(t *testing.T) {
			lc, catalog, primary, familyID, _, _ := newCheckoutObservationFixture(t)
			worktree := filepath.Join(filepath.Dir(primary), "pending-proof")
			builderGit(t, primary, "worktree", "add", "-b", "pending-proof", worktree)
			job, err := lc.checkoutObservation(pathkey.CanonicalExistingRoot(worktree))
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-job.proofReady:
			case <-time.After(2 * time.Second):
				t.Fatal("read-only proof did not finish")
			}
			if job.proofErr != nil || job.proof == nil {
				t.Fatalf("proof failed: %v", job.proofErr)
			}
			switch change {
			case "root":
				if err := os.Rename(worktree, worktree+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(worktree, 0o755); err != nil {
					t.Fatal(err)
				}
				marker, err := os.ReadFile(filepath.Join(worktree+"-old", ".git"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(worktree, ".git"), marker, 0o644); err != nil {
					t.Fatal(err)
				}
			case "git_marker":
				marker := filepath.Join(worktree, ".git")
				content, err := os.ReadFile(marker)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(marker, append(content, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			case "commondir_marker":
				if err := os.Remove(filepath.Join(job.proof.inventory.GitDir, "commondir")); err != nil {
					t.Fatal(err)
				}
			case "common_directory":
				common := job.proof.inventory.CommonDir
				if err := os.Rename(common, common+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(common, 0o755); err != nil {
					t.Fatal(err)
				}
				// Preserve the worktree GitDir identity while replacing its family.
				if err := os.Rename(filepath.Join(common+"-old", "worktrees"), filepath.Join(common, "worktrees")); err != nil {
					t.Fatal(err)
				}
			}
			_, found, err := observeCheckoutUntilSettled(t, lc, worktree)
			if found || !errors.Is(err, ErrCheckoutMutationStale) {
				t.Fatalf("pending proof survived %s replacement: found=%v err=%v", change, found, err)
			}
			rows, err := catalog.ListCheckouts(t.Context(), familyID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("invalid proof allocated checkout: %+v %v", rows, err)
			}
		})
	}
}

func BenchmarkObserveCheckoutPath(b *testing.B) {
	lc, _, primary, _, _, _ := newCheckoutObservationFixture(b)
	var busy, successful int
	var successTime time.Duration
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		worktree := filepath.Join(filepath.Dir(primary), fmt.Sprintf("observed-%d", i))
		builderGit(b, primary, "worktree", "add", "-b", fmt.Sprintf("observed-%d", i), worktree)
		b.StartTimer()
		started := time.Now()
		checkout, found, err := lc.ObserveCheckoutPath(context.Background(), worktree)
		elapsed := time.Since(started)
		b.StopTimer()
		if errors.Is(err, ErrCheckoutMutationBusy) {
			busy++
			continue
		}
		if err != nil || !found || checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic {
			b.Fatalf("observe: %+v found=%v err=%v", checkout, found, err)
		}
		successful++
		successTime += elapsed
	}
	b.ReportMetric(float64(busy), "busy-admissions")
	b.ReportMetric(float64(successful), "successful-admissions")
	if successful != 0 {
		b.ReportMetric(float64(successTime.Nanoseconds())/float64(successful), "success-ns/admission")
	}
}

func BenchmarkObserveCheckoutPathCachedProof(b *testing.B) {
	lc, _, primary, _, _, _ := newCheckoutObservationFixture(b)
	worktree := filepath.Join(filepath.Dir(primary), "cached")
	builderGit(b, primary, "worktree", "add", "-b", "cached", worktree)
	var inventories atomic.Int32
	lc.observationInventory = func(ctx context.Context, path string) (*gitstate.FamilyInventory, error) {
		inventories.Add(1)
		return gitstate.Inventory(ctx, path)
	}
	checkout, found, err := observeCheckoutUntilSettled(b, lc, worktree)
	if err != nil || !found {
		b.Fatalf("initial observation: found=%v err=%v", found, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		again, found, err := lc.ObserveCheckoutPath(context.Background(), worktree)
		if err != nil || !found || again.CheckoutID != checkout.CheckoutID {
			b.Fatalf("cached observation: %+v found=%v err=%v", again, found, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(inventories.Load()), "inventory-calls")
}
