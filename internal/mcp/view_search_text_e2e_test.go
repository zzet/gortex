package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
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
	familyID   string
}

func newWorktreeSearchStack(t *testing.T) *worktreeSearchStack {
	t.Helper()
	return newWorktreeSearchStackWithConfig(t, "workspace: main-ws\n")
}

// newWorktreeSearchStackWithConfig is newWorktreeSearchStack with the primary's
// .gortex.yaml of the caller's choosing.
func newWorktreeSearchStackWithConfig(t *testing.T, gortexYAML string) *worktreeSearchStack {
	t.Helper()
	stack := buildWorktreeSearchStack(t, gortexYAML)
	stack.checkoutID = stack.awaitRoutedCheckout(t)
	return stack
}

// newDormantWorktreeSearchStack leaves the worktree as the startup inventory
// does: registered and automatic, with no coordinator and no route.
func newDormantWorktreeSearchStack(t *testing.T) *worktreeSearchStack {
	t.Helper()
	stack := buildWorktreeSearchStack(t, "workspace: main-ws\n")
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for {
		checkout, found, err := graphview.CheckoutForPath(ctx, stack.store.Catalog(), stack.srv.viewFamilies(ctx), stack.worktree)
		if err != nil {
			t.Fatalf("bind the worktree to a checkout: %v", err)
		}
		if found && graphview.ServesAutomaticView(checkout) {
			stack.checkoutID, stack.familyID = checkout.CheckoutID, checkout.FamilyID
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worktree at %s was never registered", stack.worktree)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if live := stack.lifecycle.LiveCoordinators(stack.familyID); live != 0 {
		t.Fatalf("the fixture worktree is not dormant: %d live coordinators", live)
	}
	return stack
}

func buildWorktreeSearchStack(t *testing.T, gortexYAML string) *worktreeSearchStack {
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
		".gortex.yaml": gortexYAML,
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

	return &worktreeSearchStack{
		srv: srv, store: store, lifecycle: lifecycle, primary: primary, worktree: worktree,
	}
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

func (w *worktreeSearchStack) searchSymbolsRider(t *testing.T, query string) map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{"query": query}
	ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), w.worktree)
	res, err := w.srv.wrapToolHandler(w.srv.handleSearchSymbols)(ctx, req)
	if err != nil {
		t.Fatalf("search_symbols %q: %v", query, err)
	}
	if res.IsError {
		t.Fatalf("search_symbols %q was refused: %s", query, viewResultText(t, res))
	}
	return resultFreshness(t, res)
}

// TestSourceReadActivatesDormantWorktree: a worktree that only ever receives
// source-lane reads must still be selected, or every identifier search keeps
// scanning its files instead of reaching the routed graph.
func TestSourceReadActivatesDormantWorktree(t *testing.T) {
	stack := newDormantWorktreeSearchStack(t)
	ctx := context.Background()

	if rider := stack.searchSymbolsRider(t, "Keeper"); rider["capability_scope"] != "declarations" {
		t.Fatalf("the dormant worktree's identifier search was not served by source declarations: %v", rider)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		route, found, err := stack.store.Catalog().GetCheckoutRoute(ctx, stack.checkoutID)
		if err != nil {
			t.Fatalf("read the worktree route: %v", err)
		}
		if found && route.State == store_sqlite.RouteActive && route.DirtyGenerationID != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a source-lane read never activated the dormant worktree (route found=%v state=%q coordinators=%d)",
				found, route.State, stack.lifecycle.LiveCoordinators(stack.familyID))
		}
		time.Sleep(50 * time.Millisecond)
	}
	rider := stack.searchSymbolsRider(t, "Keeper")
	if _, ok := rider["capability_scope"]; ok {
		t.Fatalf("the routed worktree's identifier search stayed on the source lane: %v", rider)
	}
	if rider["exact"] != true {
		t.Fatalf("the routed worktree's identifier search was not an exact graph answer: %v", rider)
	}
}

// TestSourceReadDoesNotActivateOutOfScopeOrRetiredCheckout: a request the
// source lane does not prove, or a retired route, never wakes a coordinator.
func TestSourceReadDoesNotActivateOutOfScopeOrRetiredCheckout(t *testing.T) {
	resolve := func(stack *worktreeSearchStack, args map[string]any) (*requestView, error) {
		req := mcplib.CallToolRequest{}
		req.Params.Name = "search_symbols"
		req.Params.Arguments = args
		ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), stack.worktree)
		return stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, &req, "search_symbols", requestFreshness{}, capabilityRequest{})
	}
	// Activation installs its coordinator promptly; a quiet window this long
	// would observe one.
	assertDormant := func(t *testing.T, stack *worktreeSearchStack) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if live := stack.lifecycle.LiveCoordinators(stack.familyID); live != 0 {
				t.Fatalf("the checkout was activated: %d live coordinators", live)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Run("out_of_scope", func(t *testing.T) {
		stack := newDormantWorktreeSearchStack(t)
		// Refused or declined, the source lane proves nothing for this domain.
		if view, _ := resolve(stack, map[string]any{"query": "Keeper", "repo": "elsewhere"}); view != nil {
			t.Fatalf("an uncontained repository domain was served by the source lane")
		}
		assertDormant(t, stack)
	})
	t.Run("retired", func(t *testing.T) {
		stack := newDormantWorktreeSearchStack(t)
		ctx := context.Background()
		graphs, err := stack.store.Catalog().ListDedicatedGraphs(ctx, stack.familyID)
		if err != nil || len(graphs) == 0 {
			t.Fatalf("list the family graphs: %d %v", len(graphs), err)
		}
		if err := stack.store.Catalog().UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
			CheckoutID: stack.checkoutID, GraphID: graphs[0].GraphID, State: store_sqlite.RouteRetired,
		}); err != nil {
			t.Fatalf("retire the route: %v", err)
		}
		if view, err := resolve(stack, map[string]any{"query": "Keeper"}); err != nil || view == nil || view.sourceScope != "declarations" {
			t.Fatalf("the retired route's identifier search was not served by source declarations: %v", err)
		}
		assertDormant(t, stack)
	})
}
