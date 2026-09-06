package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	checkout, found, err := lc.ObserveCheckoutPath(t.Context(), filepath.Join(worktree, "nested"))
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
	again, found, err := lc.ObserveCheckoutPath(t.Context(), worktree)
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
	if _, found, err := lc.ObserveCheckoutPath(t.Context(), unknown); err != nil || found {
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
	if _, found, err := lc.ObserveCheckoutPath(t.Context(), worktree, func(prefix string) error {
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
