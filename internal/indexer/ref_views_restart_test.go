package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// restartableRefViewFixture owns an explicit database path so a test can close
// every SQLite handle and construct a fresh manager over the same catalog.
type restartableRefViewFixture struct {
	*refViewFixture
	dbPath string
}

func newRestartableRefViewFixture(t *testing.T) *restartableRefViewFixture {
	t.Helper()
	builderIsolateGit(t)

	repo := builderTempDir(t, "refview-restart")
	builderGit(t, repo, "init", "--initial-branch=main")
	builderWriteTree(t, repo, builderTreeA())
	builderGit(t, repo, "add", "-A")
	builderGit(t, repo, "commit", "-m", "A")
	treeA := builderGit(t, repo, "rev-parse", "HEAD^{tree}")

	dbPath := filepath.Join(builderTempDir(t, "refview-restart-store"), "graph.sqlite")
	store, err := store_sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open restartable store: %v", err)
	}
	builderIndex(t, store, repo)

	fixture := &restartableRefViewFixture{
		refViewFixture: &refViewFixture{
			t:          t,
			store:      store,
			catalog:    store.Catalog(),
			repo:       repo,
			familyID:   "family-refview-restart",
			graphID:    GraphIDFor(builderRepoPrefix),
			checkoutID: "checkout-refview-restart",
			treeA:      treeA,
		},
		dbPath: dbPath,
	}
	fixture.writeCatalogIdentity()
	t.Cleanup(func() {
		if fixture.store != nil {
			_ = fixture.store.Close()
		}
	})
	return fixture
}

func (f *restartableRefViewFixture) reopen() {
	f.t.Helper()
	if err := f.store.Close(); err != nil {
		f.t.Fatalf("close store before restart: %v", err)
	}
	f.store = nil
	f.catalog = nil

	store, err := store_sqlite.Open(f.dbPath)
	if err != nil {
		f.t.Fatalf("reopen store after restart: %v", err)
	}
	f.store = store
	f.catalog = store.Catalog()
}

// TestRefViewRestartReusesReadyGenerationWithoutPhysicalBuild pins the durable
// cache contract. A fresh store and manager must adopt the ready generation
// recorded before close; process-local build state is not required for reuse.
func TestRefViewRestartReusesReadyGenerationWithoutPhysicalBuild(t *testing.T) {
	f := newRestartableRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	var physicalBuilds atomic.Int64
	manager := f.manager(t, func() { physicalBuilds.Add(1) })
	first, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("build view before restart: %v", err)
	}
	if !first.Built || first.State != store_sqlite.RefViewReady || first.GenerationID == 0 {
		t.Fatalf("first selection = %+v, want one newly built ready generation", first)
	}
	if n := physicalBuilds.Load(); n != 1 {
		t.Fatalf("physical builds before restart = %d, want 1", n)
	}

	generationID := first.GenerationID
	f.reopen()
	manager = f.manager(t, func() { physicalBuilds.Add(1) })
	second, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("select view after restart: %v", err)
	}
	if second.Built || second.State != store_sqlite.RefViewReady || second.GenerationID != generationID {
		t.Fatalf("selection after restart = %+v, want ready generation %d with Built=false", second, generationID)
	}
	if n := physicalBuilds.Load(); n != 1 {
		t.Fatalf("restart ran %d total physical builds, want the original one only", n)
	}
	if builds := f.builds(first.RefViewID); len(builds) != 1 {
		t.Fatalf("restart left %d build rows, want the original one: %+v", len(builds), builds)
	}
	if view := f.view(first.RefViewID); view.State != store_sqlite.RefViewReady || view.ActiveGenerationID != generationID {
		t.Fatalf("persisted view after restart = %+v, want ready generation %d", view, generationID)
	}
}

// TestRefViewDeletedRefNeverServesItsPreviouslyReadyGeneration pins resolution
// ahead of cache reuse. The payload may remain retained for a future ref that
// resolves again, but an absent selector must answer failed with no generation
// rather than label the stale active pointer as exact.
func TestRefViewDeletedRefNeverServesItsPreviouslyReadyGeneration(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	const ref = "refs/heads/feature"
	f.setRef(ref, commitB)

	var physicalBuilds atomic.Int64
	manager := f.manager(t, func() { physicalBuilds.Add(1) })
	ready, err := manager.EnsureRefView(context.Background(), f.request(ref))
	if err != nil {
		t.Fatalf("build ready view: %v", err)
	}
	if ready.State != store_sqlite.RefViewReady || ready.GenerationID == 0 {
		t.Fatalf("selection before deletion = %+v, want a ready exact generation", ready)
	}

	builderGit(t, f.repo, "update-ref", "-d", ref)
	failed, err := manager.EnsureRefView(context.Background(), f.request(ref))
	if err == nil || !errors.Is(err, gitstate.ErrRefNotAvailableLocally) {
		t.Fatalf("selection after deletion error = %v, want local ref unavailable", err)
	}
	if failed.State != store_sqlite.RefViewFailed || failed.GenerationID != 0 || failed.Built {
		t.Fatalf("selection after deletion = %+v, want failed with no served generation", failed)
	}
	if failed.Resolved.CommitOID != "" || failed.Resolved.TreeOID != "" {
		t.Fatalf("failed selection retained exact metadata: %+v", failed.Resolved)
	}

	view := f.view(ready.RefViewID)
	if view.State != store_sqlite.RefViewFailed {
		t.Fatalf("persisted view after deletion = %+v, want failed", view)
	}
	if view.ActiveGenerationID != ready.GenerationID {
		t.Fatalf("failed resolution discarded retained generation %d: %+v", ready.GenerationID, view)
	}
	if n := physicalBuilds.Load(); n != 1 {
		t.Fatalf("deleted ref ran %d physical builds, want the original one only", n)
	}
	if generations := f.generations(); len(generations) != 1 {
		t.Fatalf("deleted ref produced %d generations, want the retained ready one: %+v", len(generations), generations)
	}
}
