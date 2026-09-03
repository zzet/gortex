package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// The end-to-end worktree fixture: a real git family whose primary checkout is
// the indexed corpus and whose linked worktree the daemon serves automatically.
//
// Nothing between the request and the bytes is stubbed. The checkout is
// registered through the lifecycle, so its coordinator is the one production
// starts, the generations are the production builder's, and the searcher the
// search reaches is the one that coordinator owns.

const wtSearchSession = "wt-search-session"

type worktreeSearchStack struct {
	srv        *Server
	store      *store_sqlite.Store
	lifecycle  *indexer.CheckoutLifecycle
	primary    string
	worktree   string
	checkoutID string
}

func newWorktreeSearchStack(t *testing.T) *worktreeSearchStack {
	t.Helper()
	refIsolateGit(t)

	base := t.TempDir()
	primary := filepath.Join(base, "primary")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatalf("mkdir primary: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(primary); err == nil {
		primary = resolved
	}
	refWriteFiles(t, primary, map[string]string{
		".gortex.yaml": "workspace: main-ws\n",
		"keep.go":      "package repo\n\nfunc Keeper() {}\n",
		"gone.go":      "package repo\n\nfunc Gone() {}\n",
	})
	refGit(t, primary, "init", "--initial-branch=main")
	refGit(t, primary, "add", "-A")
	refGit(t, primary, "commit", "-m", "main")

	worktree := filepath.Join(base, "wt")
	refGit(t, primary, "worktree", "add", "-b", "feature", worktree)
	if resolved, err := filepath.EvalSymlinks(worktree); err == nil {
		worktree = resolved
	}

	store, err := store_sqlite.Open(filepath.Join(base, "store.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfgPath := filepath.Join(base, "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(cfgPath)
	if err := gc.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	cm, err := config.NewConfigManager(cfgPath)
	if err != nil {
		t.Fatalf("config manager: %v", err)
	}

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(store, reg, bm, cm, zap.NewNop())
	leases := graphview.NewLeaseManager()
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer:  mi,
		ConfigManager: cm,
		Graph:         store,
		Logger:        zap.NewNop(),
		ViewLeases:    leases,
	})
	if err != nil {
		t.Fatalf("build the lifecycle: %v", err)
	}
	t.Cleanup(func() { _ = lifecycle.Close() })

	watcher, err := indexer.NewMultiWatcher(mi, map[string]config.WatchConfig{}, zap.NewNop())
	if err != nil {
		t.Fatalf("build the repository watcher: %v", err)
	}
	if err := watcher.Start(); err != nil {
		t.Fatalf("start the repository watcher: %v", err)
	}
	lifecycle.SetWatcherSource(func() indexer.RepoWatcher { return watcher })
	t.Cleanup(func() {
		lifecycle.SetWatcherSource(nil)
		if err := watcher.Stop(); err != nil {
			t.Errorf("stop the repository watcher: %v", err)
		}
	})

	// Registering the primary is what gives the worktree beside it an identity;
	// the coordinator that composes its view is started on demand, when the
	// worktree is first selected (see awaitRoutedCheckout below).
	if _, err := lifecycle.Register(context.Background(),
		config.RepoEntry{Path: primary, Name: refTestPrefix}, store_sqlite.IntentSourceCLITrack); err != nil {
		t.Fatalf("register the primary checkout: %v", err)
	}

	eng := query.NewEngine(store)
	eng.SetSearch(bm)
	srv := NewServer(eng, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:      mi,
		ConfigManager:     cm,
		CheckoutLifecycle: lifecycle,
	})
	srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases})

	stack := &worktreeSearchStack{
		srv: srv, store: store, lifecycle: lifecycle, primary: primary, worktree: worktree,
	}
	stack.checkoutID = stack.awaitRoutedCheckout(t)
	return stack
}

// awaitRoutedCheckout waits for the worktree to become a checkout with a route
// a view can be composed from, and returns its id.
func (w *worktreeSearchStack) awaitRoutedCheckout(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	catalog := w.store.Catalog()
	activated := false
	deadline := time.Now().Add(60 * time.Second)
	for {
		checkout, found, err := graphview.CheckoutForPath(ctx, catalog, w.srv.viewFamilies(ctx), w.worktree)
		if err != nil {
			t.Fatalf("bind the worktree to a checkout: %v", err)
		}
		if found && graphview.ServesAutomaticView(checkout) {
			// The worktree is dormant until selected; activating it once is what
			// a session selecting the view does, and what starts the coordinator
			// whose route this fixture waits for. Only once: re-signalling every
			// poll would keep resetting the coordinator's quiet window and it
			// would never settle on a generation to route.
			if !activated {
				w.lifecycle.ActivateCheckout(checkout.CheckoutID, "test select")
				activated = true
			}
			route, routed, err := catalog.GetCheckoutRoute(ctx, checkout.CheckoutID)
			if err != nil {
				t.Fatalf("read the worktree route: %v", err)
			}
			if routed && graphview.RouteReady(route) {
				return checkout.CheckoutID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worktree at %s never got a routed view", w.worktree)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dirtyGeneration is the working-tree generation the route names right now.
func (w *worktreeSearchStack) dirtyGeneration(t *testing.T) int64 {
	t.Helper()
	route, found, err := w.store.Catalog().GetCheckoutRoute(context.Background(), w.checkoutID)
	if err != nil || !found {
		t.Fatalf("read the worktree route: found=%v err=%v", found, err)
	}
	return route.DirtyGenerationID
}

// awaitDirtyGenerationAfter waits for a cycle to route a working-tree
// generation other than previous, which is how the fixture knows the edits it
// just made have reached the view.
func (w *worktreeSearchStack) awaitDirtyGenerationAfter(t *testing.T, previous int64) {
	t.Helper()
	w.lifecycle.SignalCheckout(w.checkoutID, "test edit")
	deadline := time.Now().Add(60 * time.Second)
	for {
		if current := w.dirtyGeneration(t); current > 0 && current != previous {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worktree's edits never reached a routed generation")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (w *worktreeSearchStack) search(t *testing.T, cwd, query string) []string {
	t.Helper()
	req := newSearchTextRequest(query)
	ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), cwd)
	res, err := w.srv.wrapToolHandler(w.srv.handleSearchText)(ctx, req)
	if err != nil {
		t.Fatalf("search %q from %s: %v", query, cwd, err)
	}
	if res.IsError {
		t.Fatalf("search %q from %s was refused: %s", query, cwd, viewResultText(t, res))
	}
	return searchTextMatchPaths(t, res)
}

// TestSearchTextRoutedViewSearchesTheCheckoutWorkingCopy is the end-to-end
// claim: a request routed to a worktree searches that worktree's bytes in both
// directions — it finds what only the worktree holds, and it misses what only
// the canonical checkout still holds.
func TestSearchTextRoutedViewSearchesTheCheckoutWorkingCopy(t *testing.T) {
	stack := newWorktreeSearchStack(t)

	previous := stack.dirtyGeneration(t)
	refWriteFiles(t, stack.worktree, map[string]string{
		"keep.go": "package repo\n\nfunc Keeper() {\n\t// zephyr-worktree-marker\n}\n",
	})
	if err := os.Remove(filepath.Join(stack.worktree, "gone.go")); err != nil {
		t.Fatalf("remove gone.go from the worktree: %v", err)
	}
	stack.awaitDirtyGenerationAfter(t, previous)

	if got := stack.search(t, stack.worktree, "zephyr-worktree-marker"); len(got) != 1 || got[0] != "repo/keep.go" {
		t.Errorf("the worktree's own edit answered %v, want repo/keep.go", got)
	}
	if got := stack.search(t, stack.primary, "zephyr-worktree-marker"); len(got) != 0 {
		t.Errorf("the canonical checkout holds the marker, so the hit above proves nothing: %v", got)
	}

	if got := stack.search(t, stack.worktree, "func Gone"); len(got) != 0 {
		t.Errorf("a file deleted in the worktree still answered: %v", got)
	}
	if got := stack.search(t, stack.primary, "func Gone"); len(got) != 1 || got[0] != "repo/gone.go" {
		t.Errorf("the canonical checkout answered %v for the deleted file, want repo/gone.go", got)
	}
}
