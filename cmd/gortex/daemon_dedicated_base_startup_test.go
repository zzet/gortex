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
		// The repository-local user.name/user.email below are what this fixture
		// commits under, but they only exist once `git config` has run. Naming
		// the identity in the environment as well means no git subcommand here
		// can fall back to auto-detection, which a CI runner's unqualified
		// hostname turns into "Author identity unknown".
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1",
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
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

// startupPublicationConsumer adds a linked worktree to a repository, so the
// family the daemon registers has a CONSUMER for its committed base.
//
// A committed base is published only where something can read one. The owning
// repository's own request route stays on legacy generation 0, so an
// owner-only family is declined by InitialBasePublisher.publish's "no
// dependent checkout" skip and nothing is built. Every test here that asserts
// a base WAS published therefore has to name the reader it was published for;
// TestDaemonWarmupPublishesNothingWithoutAConsumer is the deliberately
// owner-only case.
//
// The worktree is added before the daemon registers the repository, because the
// checkout rows are allocated by the family reconciliation Register runs.
func startupPublicationConsumer(t *testing.T, base, root, name string) string {
	t.Helper()
	path := filepath.Join(base, name)
	cmd := exec.Command("git", "worktree", "add", "-q", "-b", name, path)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add %s: %v: %s", path, err, out)
	}
	return path
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
// trace for the initial committed publication on cold and warm startup.
//
// The chain it exercises end to end is:
//
//	buildDaemonState (daemon_state.go)
//	  -> serverstack.NewSharedServer installs the DedicatedBaseRuntime, which
//	     must precede every owner registration
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
	// The reader the base is published FOR; without it the family is declined.
	startupPublicationConsumer(t, base, root, "tracked-dependent")

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
	// generation 0 is still the legacy corpus — the declared limit on the
	// primary's own working route.
	if _, routed, err := store.Catalog().GetCheckoutRoute(ctx, graph.OwnerCheckoutID); err != nil {
		t.Fatalf("read the owner's route: %v", err)
	} else if routed {
		t.Fatal("publication installed a route for the dedicated owner; publication must not activate the owner's own route")
	}
}

// TestDaemonWarmupPublishesNothingWithoutAConsumer is the production-entrypoint
// trace for the committed-base consumer gate, and for the cold-index regression
// it removes.
//
// The measured shape is this one: a single tracked repository, one checkout,
// zero routes. The warmup dispatch still schedules the repository — the
// scheduling is what makes the deferral observable and what a later consumer's
// demand is measured against — but the publication is declined, so the daemon
// does NOT pay for a second whole-repository index. On the 1,500-file phase
// that one decision is +847 MB of logical writes and a store of 61 -> 122 MB.
//
// A unit test of publish's skip would not catch a daemon that reached the
// publisher through some other door; this asserts on the state the real warmup
// leaves the catalog in.
//
// Revert-red: delete the "no dependent checkout" skip from
// InitialBasePublisher.publish and this test fails on all three assertions.
func TestDaemonWarmupPublishesNothingWithoutAConsumer(t *testing.T) {
	base := startupPublicationEnv(t)
	// No worktree: one checkout, no ref view, nothing that can read a base.
	root := startupPublicationRepo(t, base, "owner-only")

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
		t.Fatal("the daemon built no committed-base publisher; the gate would be untested")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	registered, err := state.lifecycle.Register(ctx, config.RepoEntry{Path: root}, indexer.TrackSourceCLI)
	if err != nil {
		t.Fatalf("register the repository: %v", err)
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

	var attempted indexer.InitialBasePublication
	for _, outcome := range state.basePublisher.Outcomes() {
		if outcome.RepoPrefix == registered.Prefix {
			attempted = outcome
		}
	}
	if attempted.RepoPrefix == "" {
		t.Fatalf("warmup never reached the publisher for %s, so the deferral is vacuous: outcomes=%+v",
			registered.Prefix, state.basePublisher.Outcomes())
	}
	if attempted.Err != nil {
		t.Fatalf("the declined publication reported an error: %v", attempted.Err)
	}
	if attempted.Skipped != "no dependent checkout" {
		t.Fatalf("an owner-only family was not declined: %+v", attempted)
	}

	store, ok := state.graph.(*store_sqlite.Store)
	if !ok {
		t.Fatal("the daemon's backend is not the sqlite store")
	}
	graph, found, err := store.Catalog().GetDedicatedGraph(ctx, indexer.GraphIDFor(registered.Prefix))
	if err != nil || !found {
		t.Fatalf("read the dedicated graph: found=%v err=%v", found, err)
	}
	if graph.ActiveGenerationID != 0 {
		t.Fatalf("an owner-only family adopted committed generation %d", graph.ActiveGenerationID)
	}
	rows, err := store.Catalog().ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{})
	if err != nil {
		t.Fatalf("list the view generations: %v", err)
	}
	for _, row := range rows {
		if row.GraphID == graph.GraphID && row.GenerationKind == "dedicated" {
			t.Fatalf("an owner-only family allocated a committed generation: %+v", row)
		}
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

// TestDaemonShutdownStopsTheCommittedBasePublisher is the production-entrypoint
// trace for the publisher's teardown, and the reason the live advancement
// trigger can bind its registry entry to the publisher's context instead of to
// InitialBasePublisher.Close.
//
// The daemon never calls Close. Its exit paths converge on the teardown that
// runs `state.shared.Close()` (daemon.go), which runs the shared stack's
// cleanup chain, which calls CheckoutLifecycle.Close, whose
// stopRepositoryPublishers closes publisher admission on the runtime
// (repository_admission.go) and cancels every registered publication driver.
//
// Previously nothing on that path removed the trigger from the process-wide
// advancement registry, so a stopped daemon's whole stack —
// trigger -> publisher -> lifecycle -> *MultiIndexer — stayed reachable for the
// life of the process, leaking it. The registry half is asserted in the
// indexer package, where the map is visible; what is asserted here is
// that the daemon's own shutdown really does reach the cancellation the
// unregistration now hangs off.
func TestDaemonShutdownStopsTheCommittedBasePublisher(t *testing.T) {
	base := startupPublicationEnv(t)
	root := startupPublicationRepo(t, base, "shutdown")

	state, err := buildDaemonState(zap.NewNop())
	if err != nil {
		t.Fatalf("buildDaemonState: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if state.shared != nil && !closed {
			_ = state.shared.Close()
		}
	})
	if state.basePublisher == nil {
		t.Fatal("the daemon built no committed-base publisher; there is no teardown to trace")
	}
	if state.basePublisher.AdvanceTrigger() == nil {
		t.Fatal("the daemon's publisher carries no live advancement trigger")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := state.lifecycle.Register(ctx, config.RepoEntry{Path: root}, indexer.TrackSourceCLI); err != nil {
		t.Fatalf("register the repository: %v", err)
	}
	if err := state.lifecycle.Seed(ctx); err != nil {
		t.Fatalf("seed the checkout catalog: %v", err)
	}

	// Vacuity guard: while the daemon is up, the publisher admits work. Without
	// this the post-shutdown assertion would also pass for a publisher that
	// never admitted anything.
	state.basePublisher.Schedule("pre-shutdown-probe")
	if pending := state.basePublisher.Pending(); pending != 1 {
		t.Fatalf("a running daemon's publisher queued %d publications for one schedule; "+
			"the shutdown assertion below would be vacuous", pending)
	}

	// The daemon's own teardown, exactly as installDaemonTeardown runs it.
	closed = true
	if err := state.shared.Close(); err != nil {
		t.Fatalf("shut the daemon stack down: %v", err)
	}

	before := state.basePublisher.Pending()
	state.basePublisher.Schedule("post-shutdown-probe")
	if after := state.basePublisher.Pending(); after != before {
		t.Fatalf("the publisher admitted a publication after the daemon shut down "+
			"(pending %d -> %d); shutdown did not reach the publication driver's cancellation",
			before, after)
	}
	if err := state.basePublisher.Wait(context.Background()); err == nil {
		t.Fatal("the publisher reported its abandoned queue as settled after shutdown; " +
			"its driver was never cancelled")
	}
}
