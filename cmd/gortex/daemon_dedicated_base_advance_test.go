package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

// advanceGit runs one git command in a repository the test owns, isolated from
// any machine-global configuration.
func advanceGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDaemonLiveHeadChangeAdvancesTheCommittedBase is the production-entrypoint
// trace for the live half of committed publication.
//
// W4.2 proved a daemon start publishes a committed base. This proves a RUNNING
// daemon keeps it current, through the real chain and nothing simulated:
//
//	warmupDaemonState brings up the MultiWatcher (its last step, after the
//	  readiness flip and after BeginDraining)
//	  -> MultiWatcher builds one GitWatcher per repository
//	  -> fsnotify observes the ref transition of a real commit
//	  -> GitWatcher.reconcile diffs the range and reindexes generation 0
//	  -> GitWatcher.finalizeReconcile restamps freshness under the repository
//	     mutation lane, then dispatches
//	  -> DedicatedBaseAdvanceTrigger.HeadChanged queues one advance
//	  -> InitialBasePublisher.publish -> ensureCurrent (SHARED leases)
//	  -> BuildClaimedDedicatedDelta -> Catalog.AdoptDedicatedBaseGeneration
//
// A unit test of the trigger cannot catch a dispatch nobody calls, and the
// daemon builds neither the watcher nor the trigger at the same site: the
// watcher comes from MultiWatcher, which holds only a MultiIndexer, and the
// trigger comes from the committed-base publisher. This is the test that the
// two ends actually meet in a real daemon.
func TestDaemonLiveHeadChangeAdvancesTheCommittedBase(t *testing.T) {
	base := startupPublicationEnv(t)
	root := startupPublicationRepo(t, base, "live")

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
		t.Fatal("the daemon built no committed-base publisher; nothing would advance")
	}
	if state.basePublisher.AdvanceTrigger() == nil {
		t.Fatal("the daemon's publisher carries no live advancement trigger; a HEAD change " +
			"would reconcile generation 0 and leave the committed base where it was")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	registered, err := state.lifecycle.Register(ctx, config.RepoEntry{Path: root}, indexer.TrackSourceCLI)
	if err != nil {
		t.Fatalf("register the repository: %v", err)
	}
	if err := state.lifecycle.Seed(ctx); err != nil {
		t.Fatalf("seed the checkout catalog: %v", err)
	}

	mw, _ := warmupDaemonState(state, zap.NewNop(), func() {})
	if mw == nil {
		t.Fatal("warmup brought up no watcher; there is no live HEAD observer to trace")
	}
	t.Cleanup(func() { _ = mw.Stop() })
	if err := waitForScheduledPublications(t, ctx, state); err != nil {
		t.Fatalf("wait for the initial publication: %v", err)
	}

	store, ok := state.graph.(*store_sqlite.Store)
	if !ok {
		t.Fatal("the daemon's backend is not the sqlite store")
	}
	catalog := store.Catalog()
	graphID := indexer.GraphIDFor(registered.Prefix)
	graph, found, err := catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil || !found {
		t.Fatalf("read the dedicated graph: found=%v err=%v", found, err)
	}
	initial := graph.ActiveGenerationID
	if initial <= 0 {
		t.Fatalf("warmup published no committed base to advance: %+v", graph)
	}

	// A real commit on the watched branch.
	if err := os.WriteFile(filepath.Join(root, "live.go"), []byte("package a\n\nfunc Live() {}\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	advanceGit(t, root, "add", ".")
	advanceGit(t, root, "commit", "-q", "-m", "live head change")
	newSHA := advanceGit(t, root, "rev-parse", "HEAD")
	newTree := advanceGit(t, root, "rev-parse", "HEAD^{tree}")

	deadline := time.Now().Add(3 * time.Minute)
	var advanced store_sqlite.DedicatedGraph
	for {
		advanced, found, err = catalog.GetDedicatedGraph(ctx, graphID)
		if err != nil || !found {
			t.Fatalf("read the dedicated graph: found=%v err=%v", found, err)
		}
		if advanced.ActiveGenerationID != initial {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the running daemon never advanced the committed base past generation %d "+
				"after a real commit; advances=%+v",
				initial, state.basePublisher.AdvanceTrigger().Advances())
		}
		time.Sleep(50 * time.Millisecond)
	}

	row, found, err := catalog.GetViewGeneration(ctx, advanced.ActiveGenerationID)
	if err != nil || !found {
		t.Fatalf("read the advanced generation: found=%v err=%v", found, err)
	}
	if row.State != store_sqlite.ViewGenerationReady || row.GenerationKind != "dedicated" {
		t.Fatalf("the advanced generation is not a ready committed one: %+v", row)
	}
	if row.TreeOID != newTree {
		t.Fatalf("the advanced base describes tree %s, not the tree HEAD names (%s)", row.TreeOID, newTree)
	}
	if row.ProvenanceCommitOID != newSHA {
		t.Fatalf("the advanced base names commit %s, not the observed HEAD (%s)", row.ProvenanceCommitOID, newSHA)
	}
	if row.DependencyRevision == "" || row.ConfigHash == "" || row.ResolverVersion == "" || row.ExtractorVersions == "" {
		t.Fatalf("the advanced base carries an incomplete frozen identity: %+v", row)
	}

	// The replaced base is retained, not retired: an old coherent route stays
	// available with truthful freshness until its readers are gone.
	previous, found, err := catalog.GetViewGeneration(ctx, initial)
	if err != nil || !found {
		t.Fatalf("the replaced base is gone: found=%v err=%v", found, err)
	}
	if previous.State != store_sqlite.ViewGenerationReady && previous.State != store_sqlite.ViewGenerationSuperseded {
		t.Fatalf("the replaced base was retired by an advance: %s", previous.State)
	}

	// Advancement is not activation: the owning repository's own request route
	// is still untouched (W4.5 stays the declared limitation).
	if _, routed, err := catalog.GetCheckoutRoute(ctx, advanced.OwnerCheckoutID); err != nil {
		t.Fatalf("read the owner's route: %v", err)
	} else if routed {
		t.Fatal("a live advance installed a route for the dedicated owner; that is W4.5")
	}
}
