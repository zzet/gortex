package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

// startupPublicationEnv gives one test a private HOME / XDG tree, a private
// config file and a private store path, so nothing here reads or writes the
// developer's daemon, config or graph.
func startupPublicationEnv(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	configPath := filepath.Join(base, ".gortex.yaml")
	if err := os.WriteFile(configPath, []byte("semantic:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreCfg, restoreBackendPath := cfgFile, daemonBackendPath
	t.Cleanup(func() { cfgFile, daemonBackendPath = restoreCfg, restoreBackendPath })
	cfgFile = configPath
	daemonBackendPath = filepath.Join(base, "store.sqlite")
	return base
}

func startupPublicationRepo(t *testing.T, base, name string) string {
	t.Helper()
	root := filepath.Join(base, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package a\n\nfunc A() {}\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	git("config", "commit.gpgsign", "false")
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	return root
}

// waitForScheduledPublications joins the queue on a budget of its own rather
// than on the test's whole context. A publication that is never released — a
// warmup that forgot to call BeginDraining — must surface as a fast, named
// failure instead of consuming the enclosing four-minute deadline.
func waitForScheduledPublications(t *testing.T, parent context.Context, state *daemonState) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	return state.basePublisher.Wait(ctx)
}

// TestDaemonWarmupPublishesTheInitialCommittedBase is the production-entrypoint
// trace for W4.2.
//
// The chain it exercises end to end is:
//
//	buildDaemonState (daemon_state.go)
//	  -> serverstack.NewSharedServer installs the DedicatedBaseRuntime (W4.1)
//	  -> indexer.NewInitialBasePublisher binds to it over the shared leases
//	CheckoutLifecycle.Seed registers the repository owner
//	warmupDaemonState's cold/warm dispatch indexes the repository into
//	  generation 0 and then Schedules its committed base
//	  -> InitialBasePublisher.publish -> dedicatedBaseRuntime.install
//	  -> dedicatedBasePublisher.ensureInitial -> BuildClaimedDedicatedBase
//	  -> Catalog.AdoptDedicatedBaseGeneration
//
// Asserting only that the primitive works in a unit test would not catch a
// publisher nothing calls, which is exactly the state the branch was in.
func TestDaemonWarmupPublishesTheInitialCommittedBase(t *testing.T) {
	base := startupPublicationEnv(t)
	root := startupPublicationRepo(t, base, "tracked")

	state, err := buildDaemonState(zap.NewNop())
	if err != nil {
		t.Fatalf("buildDaemonState: %v", err)
	}
	t.Cleanup(func() {
		if state.shared != nil {
			_ = state.shared.Close()
		}
	})
	if state.basePublisher == nil {
		t.Fatal("the daemon built no committed-base publisher; warmup would publish nothing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	registered, err := state.lifecycle.Register(ctx, config.RepoEntry{Path: root}, indexer.TrackSourceCLI)
	if err != nil {
		t.Fatalf("register the repository: %v", err)
	}
	if registered.Prefix == "" {
		t.Fatal("registration returned no repo prefix")
	}
	if err := state.lifecycle.Seed(ctx); err != nil {
		t.Fatalf("seed the checkout catalog: %v", err)
	}

	mw, _ := warmupDaemonState(state, zap.NewNop(), func() {})
	if mw != nil {
		t.Cleanup(func() { _ = mw.Stop() })
	}
	if err := waitForScheduledPublications(t, ctx, state); err != nil {
		t.Fatalf("wait for the scheduled publications: %v", err)
	}

	outcomes := state.basePublisher.Outcomes()
	if len(outcomes) == 0 {
		t.Fatal("warmup scheduled no committed-base publication; the dispatch does not reach the publisher")
	}
	var published indexer.InitialBasePublication
	for _, outcome := range outcomes {
		if outcome.RepoPrefix == registered.Prefix {
			published = outcome
		}
	}
	if published.RepoPrefix == "" {
		t.Fatalf("no publication for %s; outcomes=%+v", registered.Prefix, outcomes)
	}
	if published.Err != nil {
		t.Fatalf("publication failed: %v", published.Err)
	}
	if published.Skipped != "" {
		t.Fatalf("publication skipped: %s", published.Skipped)
	}
	if published.GenerationID <= 0 {
		t.Fatalf("publication adopted no generation: %+v", published)
	}

	store, ok := state.graph.(*store_sqlite.Store)
	if !ok {
		t.Fatal("the daemon's backend is not the sqlite store")
	}
	graph, found, err := store.Catalog().GetDedicatedGraph(ctx, indexer.GraphIDFor(registered.Prefix))
	if err != nil || !found {
		t.Fatalf("read the dedicated graph: found=%v err=%v", found, err)
	}
	if graph.ActiveGenerationID != published.GenerationID {
		t.Fatalf("the committed base was not adopted: active=%d published=%d",
			graph.ActiveGenerationID, published.GenerationID)
	}
	row, found, err := store.Catalog().GetViewGeneration(ctx, published.GenerationID)
	if err != nil || !found {
		t.Fatalf("read the published generation: found=%v err=%v", found, err)
	}
	if row.State != store_sqlite.ViewGenerationReady || row.GenerationKind != "dedicated" ||
		row.BaseGenerationID != 0 || row.TreeOID == "" || row.DependencyRevision == "" ||
		row.ConfigHash == "" || row.ResolverVersion == "" {
		t.Fatalf("the published generation is not a complete committed root: %+v", row)
	}
	// Publication is not activation: the owner's own route is untouched, and
	// generation 0 is still the legacy corpus (W4.5 is the declared limit).
	if _, routed, err := store.Catalog().GetCheckoutRoute(ctx, graph.OwnerCheckoutID); err != nil {
		t.Fatalf("read the owner's route: %v", err)
	} else if routed {
		t.Fatal("publication installed a route for the dedicated owner; that is W4.5, not W4.2")
	}
}

// TestDaemonWarmupReadinessIsNotBlockedByPublication states the second
// requirement: readiness is a warmup outcome, not a publication outcome.
//
// "markReady fired by the time warmupDaemonState returns" is not a claim — it
// is true of any implementation, because markReady is called unconditionally
// inside warmupDaemonState. The claim this test makes is the ORDERING, sampled
// at the instant readiness flips:
//
//   - at least one publication is queued (so the assertion is not vacuous: the
//     dispatch really did schedule this repository before readiness), and
//   - zero publications have been attempted.
//
// A publisher that published from Schedule — inline on the warmup worker,
// upstream of markReady — records its outcome before readiness and fails the
// second assertion. So does one whose queue is drained before the flip.
func TestDaemonWarmupReadinessIsNotBlockedByPublication(t *testing.T) {
	base := startupPublicationEnv(t)
	root := startupPublicationRepo(t, base, "ready-first")

	state, err := buildDaemonState(zap.NewNop())
	if err != nil {
		t.Fatalf("buildDaemonState: %v", err)
	}
	t.Cleanup(func() {
		if state.shared != nil {
			_ = state.shared.Close()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := state.lifecycle.Register(ctx, config.RepoEntry{Path: root}, indexer.TrackSourceCLI); err != nil {
		t.Fatalf("register the repository: %v", err)
	}
	if err := state.lifecycle.Seed(ctx); err != nil {
		t.Fatalf("seed the checkout catalog: %v", err)
	}

	if state.basePublisher == nil {
		t.Fatal("the daemon built no committed-base publisher; there is no ordering to observe")
	}

	// Sampled once, inside markReady: the queue depth and the attempt count at
	// the instant the daemon declares itself queryable.
	// sampleMu guards the sample: markReady may run on a warmup goroutine and
	// is read here on the test goroutine.
	var (
		sampleMu         sync.Mutex
		readyFired       bool
		pendingAtReady   int
		attemptedAtReady int
	)
	ready := make(chan struct{}, 1)
	mw, _ := warmupDaemonState(state, zap.NewNop(), func() {
		sampleMu.Lock()
		if !readyFired {
			readyFired = true
			pendingAtReady = state.basePublisher.Pending()
			attemptedAtReady = len(state.basePublisher.Outcomes())
		}
		sampleMu.Unlock()
		select {
		case ready <- struct{}{}:
		default:
		}
	})
	if mw != nil {
		t.Cleanup(func() { _ = mw.Stop() })
	}
	select {
	case <-ready:
	default:
		t.Fatal("warmup returned without marking the graph queryable")
	}
	sampleMu.Lock()
	sampledReady, sampledPending, sampledAttempted := readyFired, pendingAtReady, attemptedAtReady
	sampleMu.Unlock()
	if !sampledReady {
		t.Fatal("readiness never fired, so nothing was sampled")
	}
	// Vacuity guard first: the dispatch must have reached the publisher for
	// this repository before the flip, whichever side of the queue it landed
	// on. Otherwise "nothing was published before ready" would be trivially
	// true of a daemon that never publishes.
	if sampledPending+sampledAttempted < 1 {
		t.Fatalf("readiness fired with no publication accounted for at all (pending=%d attempted=%d); "+
			"the dispatch did not reach the publisher, so the ordering claim is vacuous",
			sampledPending, sampledAttempted)
	}
	if sampledAttempted != 0 {
		t.Fatalf("%d committed-base publication(s) had already been attempted when the daemon flipped "+
			"ready (pending=%d); publication ran in front of readiness instead of behind it",
			sampledAttempted, sampledPending)
	}
	if sampledPending < 1 {
		t.Fatalf("readiness fired with nothing queued (pending=%d)", sampledPending)
	}

	// And the deferral still publishes: the queue is released right after the
	// flip, not abandoned.
	if err := waitForScheduledPublications(t, ctx, state); err != nil {
		t.Fatalf("wait for the scheduled publications: %v", err)
	}
	outcomes := state.basePublisher.Outcomes()
	if len(outcomes) == 0 {
		t.Fatal("nothing was published after readiness; the queue is never drained")
	}
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			t.Fatalf("deferred publication failed: %+v", outcome)
		}
	}
	if pending := state.basePublisher.Pending(); pending != 0 {
		t.Fatalf("the publisher settled with %d publications still queued", pending)
	}
}
