package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// The editor-buffer overlay used to be built entirely against the primary
// checkout and the shared corpus: the identities its removal markers hide, the
// reader its edges resolve through, the checkout its paths are spelled from,
// and the absolute path its drift gate stats all came from `s.graph` and the
// registered indexer roots. A request routed to a worktree view therefore got a
// layer describing a tree it is not reading, and the per-session cache — keyed
// by content hash alone — handed that layer to the next request whatever view
// it read.
//
// These tests pin the four halves of the re-rooting plus the cache key. The
// fixture is one tracked repository whose canonical checkout carries a symbol
// the routed worktree does not, and a routed view whose reader carries symbols
// the canonical checkout does not, so every assertion below distinguishes the
// two readers rather than merely observing that some reader answered.

const (
	viewRootRepo      = "repo-v"
	viewRootWorkspace = "acme-v"
	viewRootMainPath  = viewRootRepo + "/main.go"
	viewRootHandleID  = viewRootMainPath + "::Handle"
	// Declared in the canonical checkout's indexed copy of main.go and in no
	// view: a removal marker naming it proves the layer read base identities
	// off the shared corpus.
	viewRootBaseStaleID = viewRootMainPath + "::BaseStale"
	// Declared in the routed view's copy of main.go and in no base: a removal
	// marker naming it proves the layer read them off the request's reader.
	viewRootViewStaleID = viewRootMainPath + "::ViewStale"
)

// overlayRequestFixture is one tracked repository plus a routed worktree that
// is deliberately not a registered indexer root — the posture
// resolveOverlayGraphPathForRequest was written for.
type overlayRequestFixture struct {
	srv      *Server
	root     string
	viewRoot string
}

func newOverlayRequestFixture(t *testing.T) overlayRequestFixture {
	t.Helper()

	base := t.TempDir()
	root := filepath.Join(base, viewRootRepo)
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"),
		[]byte("workspace: "+viewRootWorkspace+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc Handle() {}\n\nfunc BaseStale() {}\n"), 0o644))

	// The routed checkout. Same repository, different working copy, never
	// registered with the MultiIndexer.
	viewRoot := filepath.Join(base, "worktrees", "feature")
	require.NoError(t, os.MkdirAll(viewRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(viewRoot, "main.go"),
		[]byte("package main\n\nfunc Handle() {}\n\nfunc ViewStale() {}\n"), 0o644))

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: root, Name: viewRootRepo}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(g, testRegistry(), bm, cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)

	eng := query.NewEngine(g)
	eng.SetSearch(bm)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:  mi,
		ConfigManager: cm,
	})
	return overlayRequestFixture{srv: srv, root: root, viewRoot: viewRoot}
}

// newViewReader returns a reader carrying the worktree's state of main.go plus
// a `Target` symbol at uniqueTargetPath, which is the only place that name is
// resolvable through this reader.
func newViewReader(uniqueTargetPath string) graph.Reader {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: viewRootMainPath, Kind: graph.KindFile, Name: "main.go",
		FilePath: viewRootMainPath, Language: "go", RepoPrefix: viewRootRepo,
		WorkspaceID: viewRootWorkspace, ProjectID: viewRootRepo,
	})
	g.AddNode(&graph.Node{
		ID: viewRootHandleID, Kind: graph.KindFunction, Name: "Handle",
		FilePath: viewRootMainPath, StartLine: 3, EndLine: 3, Language: "go",
		RepoPrefix: viewRootRepo, WorkspaceID: viewRootWorkspace, ProjectID: viewRootRepo,
	})
	g.AddNode(&graph.Node{
		ID: viewRootViewStaleID, Kind: graph.KindFunction, Name: "ViewStale",
		FilePath: viewRootMainPath, StartLine: 5, EndLine: 5, Language: "go",
		RepoPrefix: viewRootRepo, WorkspaceID: viewRootWorkspace, ProjectID: viewRootRepo,
	})
	g.AddNode(&graph.Node{
		ID: uniqueTargetPath, Kind: graph.KindFile, Name: filepath.Base(uniqueTargetPath),
		FilePath: uniqueTargetPath, Language: "go", RepoPrefix: viewRootRepo,
		WorkspaceID: viewRootWorkspace, ProjectID: viewRootRepo,
	})
	g.AddNode(&graph.Node{
		ID: uniqueTargetPath + "::Target", Kind: graph.KindFunction, Name: "Target",
		FilePath: uniqueTargetPath, StartLine: 3, EndLine: 3, Language: "go",
		RepoPrefix: viewRootRepo, WorkspaceID: viewRootWorkspace, ProjectID: viewRootRepo,
	})
	return g
}

// routedViewCtx installs a request view that reads `reader` through the
// worktree at viewRoot. generation separates two otherwise identical views.
func (f overlayRequestFixture) routedViewCtx(
	ctx context.Context, reader graph.Reader, generation int64,
) context.Context {
	return withRequestView(ctx, &requestView{
		reader:   reader,
		viewRoot: f.viewRoot,
		materialized: &graphview.RepoView{ID: graphview.RepoViewID{
			RepoPrefix:     viewRootRepo,
			BaseGraphID:    "graph-v",
			BaseGeneration: generation,
		}},
	})
}

// TestOverlayLayerBindsToTheRequestViewCheckout is the path half: a buffer the
// editor spells under the routed worktree must land on the repository's graph
// path, and the owning indexer must still be the one that parses and stamps it.
func TestOverlayLayerBindsToTheRequestViewCheckout(t *testing.T) {
	f := newOverlayRequestFixture(t)
	ctx := f.routedViewCtx(context.Background(), newViewReader(viewRootRepo+"/helper.go"), 1)

	layer, paths, err := f.srv.constructOverlayLayer(ctx, []daemon.OverlayFile{{
		Path:    filepath.Join(f.viewRoot, "main.go"),
		Content: "package main\n\nfunc Handle() { Target() }\n",
	}})
	require.NoError(t, err)
	require.NotNil(t, layer)

	require.Equal(t, []string{viewRootMainPath}, paths,
		"a buffer under the routed checkout must be spelled at the repository's graph path")
	require.True(t, layer.HasFile(viewRootMainPath))
	require.True(t, layer.HasNode(viewRootHandleID),
		"the buffer's own symbol must be minted at the repository's identity")

	// The owning indexer is still the authority for scope identity, even though
	// the path it was resolved from lives in an unregistered checkout.
	handle := layer.NodeByID(viewRootHandleID)
	require.NotNil(t, handle)
	require.Equal(t, viewRootWorkspace, handle.WorkspaceID)
	require.Equal(t, viewRootRepo, handle.RepoPrefix)
}

// TestOverlayLayerResolvesEdgesThroughTheRequestViewReader is the reader half
// for edges: a call to a symbol that exists only in the routed view's reader
// must bind, and it must bind to that reader's identity.
func TestOverlayLayerResolvesEdgesThroughTheRequestViewReader(t *testing.T) {
	f := newOverlayRequestFixture(t)
	targetID := viewRootRepo + "/helper.go::Target"
	ctx := f.routedViewCtx(context.Background(), newViewReader(viewRootRepo+"/helper.go"), 1)

	layer, _, err := f.srv.constructOverlayLayer(ctx, []daemon.OverlayFile{{
		Path:    filepath.Join(f.viewRoot, "main.go"),
		Content: "package main\n\nfunc Handle() { Target() }\n",
	}})
	require.NoError(t, err)
	require.NotNil(t, layer)

	require.Equal(t, targetID, overlayEdgeTarget(t, layer, viewRootHandleID, "Target"),
		"the buffer's call must resolve against the reader this request reads, "+
			"where Target exists; the shared corpus carries no such symbol")
}

// TestOverlayLayerRemovalMarkersComeFromTheRequestViewReader is the reader half
// for identities: the symbols a buffer hides are the ones THIS request's reader
// carries for the file, never the shared corpus's stale copy of it.
func TestOverlayLayerRemovalMarkersComeFromTheRequestViewReader(t *testing.T) {
	f := newOverlayRequestFixture(t)
	require.NotNil(t, f.srv.graph.GetNode(viewRootBaseStaleID),
		"fixture is pointless unless the shared corpus carries the symbol the view does not")

	ctx := f.routedViewCtx(context.Background(), newViewReader(viewRootRepo+"/helper.go"), 1)
	layer, _, err := f.srv.constructOverlayLayer(ctx, []daemon.OverlayFile{{
		Path:    filepath.Join(f.viewRoot, "main.go"),
		Content: "package main\n\nfunc Handle() { Target() }\n",
	}})
	require.NoError(t, err)
	require.NotNil(t, layer)

	require.True(t, layer.IsRemovedID(viewRootViewStaleID),
		"the routed view's symbol the buffer dropped must be hidden")
	require.False(t, layer.IsRemovedID(viewRootBaseStaleID),
		"a symbol that lives only in the corpus this request is not reading must not be marked removed")
}

// TestOverlayLayerCacheKeySeparatesTwoViewsOfOnePath is the cache half: one
// session pushing identical buffers under two views of the same path must not
// be served the first view's parse under the second.
func TestOverlayLayerCacheKeySeparatesTwoViewsOfOnePath(t *testing.T) {
	f := newOverlayRequestFixture(t)
	const session = "overlay-view-session"
	files := []daemon.OverlayFile{{
		Path:    filepath.Join(f.viewRoot, "main.go"),
		Content: "package main\n\nfunc Handle() { Target() }\n",
	}}

	build := func(reader graph.Reader, generation int64) (*overlayLayerCacheEntry, *graph.OverlaidView) {
		t.Helper()
		ctx := WithSessionID(context.Background(), session)
		ctx = withOverlayRequestSnapshot(ctx, &overlayRequestSnapshot{
			sessionID: session,
			files:     files,
			canonical: true,
		})
		ctx = f.routedViewCtx(ctx, reader, generation)
		view, err := f.srv.buildOverlayViewForCtx(ctx)
		require.NoError(t, err)
		require.NotNil(t, view)
		cached, ok := f.srv.overlayLayerCache.Load(session)
		require.True(t, ok)
		return cached.(*overlayLayerCacheEntry), view
	}

	firstEntry, _ := build(newViewReader(viewRootRepo+"/helper.go"), 1)
	require.Equal(t, viewRootRepo+"/helper.go::Target",
		overlayEdgeTarget(t, firstEntry.layer, viewRootHandleID, "Target"))

	// Same session, same bytes, same path — a different view. The second build
	// must not be answered from the first view's parse.
	secondEntry, _ := build(newViewReader(viewRootRepo+"/other.go"), 2)
	require.NotEqual(t, firstEntry.viewKey, secondEntry.viewKey,
		"two views of one path must key the overlay cache differently")
	require.Equal(t, viewRootRepo+"/other.go::Target",
		overlayEdgeTarget(t, secondEntry.layer, viewRootHandleID, "Target"),
		"the second view's buffer must resolve against the second view's reader")
}

// TestOverlayViewCacheKeyIsEmptyForAnUnroutedRequest pins the no-change half:
// a request that reads the base corpus caches under exactly the identity it
// always did.
func TestOverlayViewCacheKeyIsEmptyForAnUnroutedRequest(t *testing.T) {
	require.Empty(t, overlayViewCacheKey(nil))
	require.Empty(t, overlayViewCacheKey(&requestView{}))
	require.NotEmpty(t, overlayViewCacheKey(&requestView{
		reader:   graph.New(),
		viewRoot: "/tmp/wt",
		materialized: &graphview.RepoView{ID: graphview.RepoViewID{
			RepoPrefix: viewRootRepo, BaseGraphID: "g", BaseGeneration: 1,
		}},
	}))
}

// TestOverlayDriftGateStatsTheRequestCheckout pins the drift half: BaseSHA is a
// claim about the file the editor has open, which under a routed view is the
// worktree's copy and not the repository's canonical one.
func TestOverlayDriftGateStatsTheRequestCheckout(t *testing.T) {
	f := newOverlayRequestFixture(t)
	const session = "overlay-drift-session"

	viewSHA := overlayFileSHA(t, filepath.Join(f.viewRoot, "main.go"))
	rootSHA := overlayFileSHA(t, filepath.Join(f.root, "main.go"))
	require.NotEqual(t, viewSHA, rootSHA,
		"fixture is pointless unless the two checkouts differ")

	ctx := WithSessionID(context.Background(), session)
	ctx = withOverlayRequestSnapshot(ctx, &overlayRequestSnapshot{
		sessionID: session,
		files: []daemon.OverlayFile{{
			Path:    viewRootMainPath,
			Content: "package main\n\nfunc Handle() { Target() }\n",
			BaseSHA: viewSHA,
		}},
		canonical: true,
	})
	ctx = f.routedViewCtx(ctx, newViewReader(viewRootRepo+"/helper.go"), 1)

	view, err := f.srv.buildOverlayViewForCtx(ctx)
	require.NoError(t, err,
		"a buffer whose base sha matches the routed checkout must not be refused as drifted")
	require.NotNil(t, view)
}

// overlayEdgeTarget returns the single resolved target of the named call the
// buffer's `from` symbol makes. Fails when the edge is missing or still carries
// an `unresolved::` placeholder.
func overlayEdgeTarget(t *testing.T, layer *graph.OverlayLayer, from, name string) string {
	t.Helper()
	edges := layer.OutEdgesByFromAll()[from]
	require.NotEmpty(t, edges, "no outgoing edges from %s", from)
	for _, e := range edges {
		if e == nil {
			continue
		}
		if overlayUnresolvedTargetName(e.To) == name {
			t.Fatalf("edge %s -> %s is still unresolved", from, e.To)
		}
		if filepath.Base(e.To) == filepath.Base(from)+"::"+name ||
			hasOverlaySymbolSuffix(e.To, name) {
			return e.To
		}
	}
	t.Fatalf("no edge from %s targets %q; edges: %s", from, name, overlayEdgeTargets(edges))
	return ""
}

func hasOverlaySymbolSuffix(id, name string) bool {
	return len(id) > len(name)+2 && id[len(id)-len(name)-2:] == "::"+name
}

func overlayEdgeTargets(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e != nil {
			out = append(out, e.To)
		}
	}
	return out
}

func overlayFileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return gitBlobSHA(data)
}

// TestPrepareOverlayRequestReachesTheViewAwarePrimitives is the wiring proof:
// the production entrypoint — the one wrapToolHandlerMode calls at
// overlay.go's buffer-overlay step — must reach the view-aware path
// resolution and the view-aware readers, not only the helpers in isolation.
//
// The buffer is pushed the way an editor with the routed worktree open pushes
// it: an absolute path under a checkout that is deliberately not a registered
// MultiIndexer root. Before the re-rooting, canonicalization refused it
// outright ("outside the registered workspace").
func TestPrepareOverlayRequestReachesTheViewAwarePrimitives(t *testing.T) {
	f := newOverlayRequestFixture(t)
	const session = "overlay-prepare-session"

	mgr := daemon.NewOverlayManager(time.Minute)
	require.NoError(t, mgr.RegisterWithID(session, viewRootWorkspace))
	require.NoError(t, mgr.Push(session, daemon.OverlayFile{
		Path:    filepath.Join(f.viewRoot, "main.go"),
		Content: "package main\n\nfunc Handle() { Target() }\n",
	}, nil))
	f.srv.SetOverlayManager(mgr)

	ctx := WithSessionID(context.Background(), session)
	ctx = f.routedViewCtx(ctx, newViewReader(viewRootRepo+"/helper.go"), 1)

	ctx, view, err := f.srv.prepareOverlayRequest(ctx)
	require.NoError(t, err)
	require.NotNil(t, view, "the pushed buffer must produce a view")

	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, []string{viewRootMainPath}, overlayFilePaths(snapshot.files),
		"canonicalization must spell the buffer at the repository's graph path")

	cached, ok := f.srv.overlayLayerCache.Load(session)
	require.True(t, ok)
	entry := cached.(*overlayLayerCacheEntry)
	require.NotEmpty(t, entry.viewKey, "a routed request must cache under its view identity")
	require.Equal(t, viewRootRepo+"/helper.go::Target",
		overlayEdgeTarget(t, entry.layer, viewRootHandleID, "Target"))
	require.True(t, entry.layer.IsRemovedID(viewRootViewStaleID))
	require.False(t, entry.layer.IsRemovedID(viewRootBaseStaleID))
}

func overlayFilePaths(files []daemon.OverlayFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// TestOverlayLayerFallsBackToTheCorpusForANarrowedBaseReader pins the
// no-regression half of re-rooting the readers. The bounded localization
// projections the layer build needs are optional capabilities that fail closed;
// a labelled `base` selector installs a narrowed filter that does not serve
// them. That view reads the same checkout and the same corpus an unrouted
// request reads, so it must keep building buffers rather than refusing them.
func TestOverlayLayerFallsBackToTheCorpusForANarrowedBaseReader(t *testing.T) {
	f := newOverlayRequestFixture(t)
	narrowed := newBaseGraphReader(f.srv.graph, viewRootRepo)
	_, boundedFile := narrowed.(graph.BoundedFileNodeReader)
	require.False(t, boundedFile,
		"fixture is pointless unless the narrowed reader really lacks the bounded projection")

	ctx := withRequestView(context.Background(), &requestView{
		reader:       narrowed,
		baseNarrowed: true,
	})
	layer, paths, err := f.srv.constructOverlayLayer(ctx, []daemon.OverlayFile{{
		Path:    viewRootMainPath,
		Content: "package main\n\nfunc Handle() {}\n",
	}})
	require.NoError(t, err, "a narrowed base selector must still build editor buffers")
	require.NotNil(t, layer)
	require.Equal(t, []string{viewRootMainPath}, paths)
	require.True(t, layer.IsRemovedID(viewRootBaseStaleID),
		"base identities still come from the corpus this view reads")
}

// TestOverlayContentForServesTheRoutedCheckoutSpelling pins the raw-bytes half.
//
// The three source-reading handlers (get_symbol_source, get_editing_context,
// smart_context) resolve a node's path with resolveNodePath, which is already
// view-aware and re-roots onto the routed checkout, and then ask
// overlayContentFor whether an editor buffer owns those bytes. The snapshot it
// searches holds canonical graph paths, so resolving them against the
// registered root alone never matches a routed request's path — and the miss
// is not merely stale: the handler then slices the file ON DISK at line
// numbers minted from the BUFFER's parse, so the text returned is the wrong
// lines of the wrong tree while the graph already carries the buffer.
func TestOverlayContentForServesTheRoutedCheckoutSpelling(t *testing.T) {
	f := newOverlayRequestFixture(t)
	const session = "overlay-content-session"
	const buffer = "package main\n\nfunc Handle() { Target() }\n"

	mgr := daemon.NewOverlayManager(time.Minute)
	require.NoError(t, mgr.RegisterWithID(session, viewRootWorkspace))
	require.NoError(t, mgr.Push(session, daemon.OverlayFile{
		Path:    filepath.Join(f.viewRoot, "main.go"),
		Content: buffer,
	}, nil))
	f.srv.SetOverlayManager(mgr)

	ctx := WithSessionID(context.Background(), session)
	ctx = f.routedViewCtx(ctx, newViewReader(viewRootRepo+"/helper.go"), 1)
	ctx, view, err := f.srv.prepareOverlayRequest(ctx)
	require.NoError(t, err)
	require.NotNil(t, view)

	// Exactly the path the source-reading handlers hand overlayContentFor.
	absPath, err := f.srv.resolveNodePath(ctx, &graph.Node{
		ID: viewRootHandleID, Kind: graph.KindFunction, Name: "Handle",
		FilePath: viewRootMainPath, RepoPrefix: viewRootRepo,
	})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(f.viewRoot, "main.go"), absPath,
		"fixture is pointless unless node resolution really re-roots onto the routed checkout")

	content, ok := f.srv.overlayContentFor(ctx, absPath)
	require.True(t, ok,
		"the buffer must own the bytes at the very path node resolution produced")
	require.Equal(t, buffer, content)

	// The handler-level consequence: line 3 of the buffer, not line 3 of
	// either checkout's file on disk.
	lines, _, _, err := f.srv.readLinesForCtx(ctx, absPath, 3, 3, 0)
	require.NoError(t, err)
	require.Equal(t, "func Handle() { Target() }", lines,
		"a source read under a routed view must serve the buffer, not on-disk bytes "+
			"sliced at line numbers the buffer's parse minted")
}

// TestOverlayLayerRelPathUsesTheOwningCheckoutSpelling pins the extractor's
// path argument in the single-repo posture.
//
// relPath is what the buffer is parsed as, and the extractor mints every node
// id from it. In the single-repo posture there is no repo prefix to trim, so
// the path is derived by making the file relative to the registered root — and
// under a routed view the file the request reads is in a *different* checkout.
// When that checkout is nested inside the registered root (git worktree add
// ./worktrees/feature), the routed spelling is relative-within-root as well,
// so it yields a plausible-looking but wrong identity: the layer files the
// node under the repository's graph path while the node itself is named after
// the worktree's path, and nothing can find it again.
func TestOverlayLayerRelPathUsesTheOwningCheckoutSpelling(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc Handle() {}\n"), 0o644))
	nested := filepath.Join(root, "worktrees", "feature")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "main.go"),
		[]byte("package main\n\nfunc Handle() {}\n"), 0o644))

	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRootPath(root)
	idx.SetWorkspaceID(viewRootWorkspace)
	require.Empty(t, idx.RepoPrefix(),
		"fixture is pointless unless this is the single-repo posture, where the "+
			"repo-prefix trim does not apply and the root-relative branch runs")

	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	ctx := withRequestView(context.Background(), &requestView{
		reader:   graph.New(),
		viewRoot: nested,
		materialized: &graphview.RepoView{ID: graphview.RepoViewID{
			BaseGraphID: "graph-nested", BaseGeneration: 1,
		}},
	})

	layer, paths, err := srv.constructOverlayLayer(ctx, []daemon.OverlayFile{{
		Path:    filepath.Join(nested, "main.go"),
		Content: "package main\n\nfunc Handle() {}\n",
	}})
	require.NoError(t, err)
	require.NotNil(t, layer)
	require.Equal(t, []string{"main.go"}, paths)
	require.True(t, layer.HasNode("main.go::Handle"),
		"the buffer must be parsed as the repository's own spelling of the file")
	require.False(t, layer.HasNode("worktrees/feature/main.go::Handle"),
		"the routed checkout's position under the registered root must not become "+
			"part of the symbol identity")
}
