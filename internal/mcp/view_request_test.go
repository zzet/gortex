package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

// The routed-view fixture. One tracked repository indexed for real, a sibling
// repository in another workspace (the scope ceiling the selector tests need),
// and a worktree of the first whose route names two published generations.
const (
	viewTestFamily        = "fam-view"
	viewTestWorktree      = "wt-view"
	viewTestPrimary       = "co-primary"
	viewTestCommitLayerID = "layer-view-commit"
	viewTestDirtyLayerID  = "layer-view-dirty"
	viewTestSession       = "view-session"
)

// viewStack is everything one routed-view test addresses.
type viewStack struct {
	srv          *Server
	store        *store_sqlite.Store
	leases       *graphview.LeaseManager
	dbPath       string
	repoRoot     string
	otherRoot    string
	worktreeRoot string
	graphID      string
	commit       int64
	dirty        int64
}

func viewRepoNode(id, name string, kind graph.NodeKind, file string, startLine int) *graph.Node {
	return &graph.Node{
		ID:         id,
		Kind:       kind,
		Name:       name,
		QualName:   "repo." + name,
		FilePath:   file,
		RepoPrefix: "repo",
		Language:   "go",
		StartLine:  startLine,
		EndLine:    startLine + 2,
	}
}

func viewFileNode(path string, endLine int) *graph.Node {
	return &graph.Node{
		ID:         path,
		Kind:       graph.KindFile,
		Name:       path[strings.LastIndexByte(path, '/')+1:],
		FilePath:   path,
		RepoPrefix: "repo",
		Language:   "go",
		EndLine:    endLine,
	}
}

func writeViewRepo(t *testing.T, dir, workspace string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte("workspace: "+workspace+"\n"), 0o644); err != nil {
		t.Fatalf("write .gortex.yaml: %v", err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// newViewStack indexes the fixture repositories, publishes the two
// generations the worktree's route names, and writes the catalog rows that
// make the worktree an automatic checkout of the indexed family.
func newViewStack(t *testing.T) *viewStack {
	t.Helper()
	base := t.TempDir()
	repoRoot := writeViewRepo(t, filepath.Join(base, "repo"), "main-ws", map[string]string{
		"edit.go": "package repo\n\nfunc Old() {}\n",
		"keep.go": "package repo\n\nfunc Keeper() {}\n",
	})
	otherRoot := writeViewRepo(t, filepath.Join(base, "other"), "other-ws", map[string]string{
		"other.go": "package other\n\nfunc Other() {}\n",
	})
	worktreeRoot := filepath.Join(base, "wt")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	dbPath := filepath.Join(base, "store.sqlite")
	store, err := store_sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfgPath := filepath.Join(base, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: repoRoot, Name: "repo"},
		{Path: otherRoot, Name: "other"},
	}}
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
	if _, err := mi.IndexScoped("", ""); err != nil {
		t.Fatalf("index fixture repos: %v", err)
	}

	graphID := indexer.GraphIDFor("repo")
	stack := &viewStack{
		store:        store,
		leases:       graphview.NewLeaseManager(),
		dbPath:       dbPath,
		repoRoot:     repoRoot,
		otherRoot:    otherRoot,
		worktreeRoot: worktreeRoot,
		graphID:      graphID,
	}
	stack.commit = writeViewCommitGeneration(t, store, graphID)
	stack.dirty = writeViewDirtyGeneration(t, store, graphID, stack.commit)
	seedViewCatalog(t, store, graphID, repoRoot, worktreeRoot)
	routeViewCheckout(t, store, graphID, stack.commit, stack.dirty, store_sqlite.RouteActive)

	eng := query.NewEngine(store)
	eng.SetSearch(bm)
	srv := NewServer(eng, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:  mi,
		ConfigManager: cm,
	})
	srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: stack.leases})
	stack.srv = srv
	return stack
}

// writeViewCommitGeneration renames edit.go's symbol and adds a file that
// exists in no other layer, so a reader can be asked both "did the layer
// replace the corpus?" and "does the layer's own content show up at all?".
func writeViewCommitGeneration(t *testing.T, store *store_sqlite.Store, graphID string) int64 {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        graphID,
		LayerID:        viewTestCommitLayerID,
		CheckoutID:     viewTestWorktree,
		GenerationKind: "commit",
		TreeOID:        "tree-view-commit",
		CreatedAt:      1000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(commit): %v", err)
	}
	handle.AddBatch([]*graph.Node{
		viewFileNode("repo/edit.go", 8),
		viewFileNode("repo/added.go", 6),
		viewRepoNode("repo/edit.go::New", "New", graph.KindFunction, "repo/edit.go", 3),
		viewRepoNode("repo/added.go::Fresh", "Fresh", graph.KindFunction, "repo/added.go", 3),
	}, []*graph.Edge{
		{From: "repo/edit.go", To: "repo/edit.go::New", Kind: graph.EdgeContains, FilePath: "repo/edit.go", Line: 3},
		{From: "repo/added.go", To: "repo/added.go::Fresh", Kind: graph.EdgeContains, FilePath: "repo/added.go", Line: 3},
	})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "repo", FilePath: "repo/edit.go", Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: "repo", FilePath: "repo/added.go", Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(commit): %v", err)
	}
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 2000); err != nil {
		t.Fatalf("PublishPayloadGeneration(commit): %v", err)
	}
	return generationID
}

// writeViewDirtyGeneration is the working-tree slot: keep.go re-derived with
// one extra symbol, so a route with both slots ready has something to prove.
func writeViewDirtyGeneration(t *testing.T, store *store_sqlite.Store, graphID string, baseGeneration int64) int64 {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          graphID,
		LayerID:          viewTestDirtyLayerID,
		CheckoutID:       viewTestWorktree,
		GenerationKind:   "dirty",
		BaseGenerationID: baseGeneration,
		TreeOID:          "tree-view-dirty",
		CreatedAt:        3000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(dirty): %v", err)
	}
	handle.AddBatch([]*graph.Node{
		viewFileNode("repo/keep.go", 9),
		viewRepoNode("repo/keep.go::Keeper", "Keeper", graph.KindFunction, "repo/keep.go", 3),
		viewRepoNode("repo/keep.go::Dirty", "Dirty", graph.KindFunction, "repo/keep.go", 6),
	}, []*graph.Edge{
		{From: "repo/keep.go", To: "repo/keep.go::Keeper", Kind: graph.EdgeContains, FilePath: "repo/keep.go", Line: 3},
		{From: "repo/keep.go", To: "repo/keep.go::Dirty", Kind: graph.EdgeContains, FilePath: "repo/keep.go", Line: 6},
	})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "repo", FilePath: "repo/keep.go", Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(dirty): %v", err)
	}
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 4000); err != nil {
		t.Fatalf("PublishPayloadGeneration(dirty): %v", err)
	}
	return generationID
}

func seedViewCatalog(t *testing.T, store *store_sqlite.Store, graphID, repoRoot, worktreeRoot string) {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          viewTestFamily,
		CommonDirIdentity: filepath.Join(repoRoot, ".git"),
		DisplayRemote:     "git@example.invalid:view.git",
		State:             reconcile.FamilyStateReady,
		CreatedAt:         100,
		LastSeen:          100,
	}); err != nil {
		t.Fatalf("UpsertRepositoryFamily: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    viewTestPrimary,
		Incarnation:   "inc-primary",
		FamilyID:      viewTestFamily,
		RootPath:      repoRoot,
		GitDir:        filepath.Join(repoRoot, ".git"),
		AdminName:     "primary",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      101,
	}); err != nil {
		t.Fatalf("UpsertCheckout(primary): %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    viewTestWorktree,
		Incarnation:   "inc-worktree",
		FamilyID:      viewTestFamily,
		RootPath:      worktreeRoot,
		GitDir:        filepath.Join(repoRoot, ".git", "worktrees", "wt"),
		AdminName:     "wt",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		LastSeen:      101,
	}); err != nil {
		t.Fatalf("UpsertCheckout(worktree): %v", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         graphID,
		OwnerCheckoutID: viewTestPrimary,
		RepoPrefix:      "repo",
		FamilyID:        viewTestFamily,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
}

func routeViewCheckout(t *testing.T, store *store_sqlite.Store, graphID string, commit, dirty int64, state store_sqlite.RouteState) {
	t.Helper()
	if err := store.Catalog().UpsertCheckoutRoute(context.Background(), store_sqlite.CheckoutRoute{
		CheckoutID:         viewTestWorktree,
		GraphID:            graphID,
		CommitGenerationID: commit,
		DirtyGenerationID:  dirty,
		State:              state,
	}); err != nil {
		t.Fatalf("UpsertCheckoutRoute: %v", err)
	}
}

// setWorktreeState rewrites the worktree checkout's lifecycle state.
func (v *viewStack) setWorktreeState(t *testing.T, state store_sqlite.CheckoutState) {
	t.Helper()
	if err := v.store.Catalog().UpdateCheckoutState(context.Background(), store_sqlite.UpdateCheckoutStateRequest{
		CheckoutID:    viewTestWorktree,
		Incarnation:   "inc-worktree",
		State:         state,
		DesiredMode:   store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
	}); err != nil {
		t.Fatalf("UpdateCheckoutState: %v", err)
	}
}

// seedRetiredWorktreeGraph leaves the disappearing checkout's former
// dedicated ownership row beside the surviving family primary. Its prefix is
// intentionally absent from the live workspace: grace must scope the primary
// corpus it will serve, not this stale owner.
func (v *viewStack) seedRetiredWorktreeGraph(t *testing.T) {
	t.Helper()
	if err := v.store.Catalog().UpsertDedicatedGraph(context.Background(), store_sqlite.DedicatedGraph{
		GraphID:         "graph-retired-worktree",
		OwnerCheckoutID: viewTestWorktree,
		RepoPrefix:      "repo@worktree",
		FamilyID:        viewTestFamily,
		State:           reconcile.GraphStateReady,
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph(retired worktree): %v", err)
	}
}

// setPrimaryGraphState rewrites the primary base graph's readiness state.
func (v *viewStack) setPrimaryGraphState(t *testing.T, state string) {
	t.Helper()
	if err := v.store.Catalog().UpsertDedicatedGraph(context.Background(), store_sqlite.DedicatedGraph{
		GraphID:         v.graphID,
		OwnerCheckoutID: viewTestPrimary,
		RepoPrefix:      "repo",
		FamilyID:        viewTestFamily,
		IsPrimaryBase:   true,
		State:           state,
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
}

// breakCheckoutReads drops the checkouts table from a second connection, so
// every catalog read of a checkout fails while the rest of the store — the
// indexed corpus and the graph rows the binding needs first — keeps working.
// It is the only way to reach the binding's read-failure path from the
// outside: the catalog is a concrete type with no fault injection.
func breakCheckoutReads(t *testing.T, dbPath string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open the store for DDL: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`DROP TABLE checkouts`); err != nil {
		t.Fatalf("drop checkouts: %v", err)
	}
}

// demotePrimaryGraph leaves the family with no primary base graph.
func (v *viewStack) demotePrimaryGraph(t *testing.T) {
	t.Helper()
	if err := v.store.Catalog().UpsertDedicatedGraph(context.Background(), store_sqlite.DedicatedGraph{
		GraphID:         v.graphID,
		OwnerCheckoutID: viewTestPrimary,
		RepoPrefix:      "repo",
		FamilyID:        viewTestFamily,
		State:           reconcile.GraphStateReady,
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
}

// callWithView drives one request through the whole tool middleware and hands
// the leaf handler back what it read through, plus the decorated response.
func (v *viewStack) callWithView(
	t *testing.T,
	cwd, tool string,
	args map[string]any,
	leaf func(ctx context.Context) (*mcplib.CallToolResult, error),
) (*mcplib.CallToolResult, error) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), cwd)
	handler := v.srv.wrapToolHandler(func(hctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return leaf(hctx)
	})
	return handler(ctx, req)
}

// captureReader records the reader a request read through and returns an
// empty JSON object so the riders have somewhere to land.
func captureReader(srv *Server, out *graph.Reader) func(ctx context.Context) (*mcplib.CallToolResult, error) {
	return func(ctx context.Context) (*mcplib.CallToolResult, error) {
		*out = srv.readerFor(ctx)
		return mcplib.NewToolResultText(`{"ok":true}`), nil
	}
}

func hasNode(reader graph.Reader, id string) bool {
	return reader != nil && reader.GetNode(id) != nil
}

func viewResultText(t *testing.T, res *mcplib.CallToolResult) string {
	t.Helper()
	text, ok := singleTextContent(res)
	if !ok {
		t.Fatalf("result carries no text content: %+v", res)
	}
	return text
}

func resultFreshness(t *testing.T, res *mcplib.CallToolResult) map[string]any {
	t.Helper()
	var asObj map[string]any
	if err := json.Unmarshal([]byte(viewResultText(t, res)), &asObj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	rider, _ := asObj["freshness"].(map[string]any)
	return rider
}

func TestRoutedViewServesTheSessionCheckout(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, &reader)); err != nil {
		t.Fatalf("call: %v", err)
	}
	if reader == nil {
		t.Fatal("the request read through no reader at all")
	}
	if !hasNode(reader, "repo/added.go::Fresh") {
		t.Error("a symbol that exists only in the routed generation is not visible")
	}
	if hasNode(reader, "repo/edit.go::Old") {
		t.Error("the corpus symbol the commit generation replaced is still visible")
	}
	if !hasNode(reader, "repo/edit.go::New") {
		t.Error("the replacement the commit generation published is not visible")
	}
	if !hasNode(reader, "repo/keep.go::Dirty") {
		t.Error("the working-tree generation's symbol is not visible")
	}
}

func TestRoutedViewCarriesTheExactRider(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	rider := resultFreshness(t, res)
	if rider == nil {
		t.Fatal("a routed answer carries no view rider")
	}
	want := "worktree:" + viewTestWorktree
	if rider["requested_view"] != want || rider["actual_view"] != want {
		t.Errorf("rider = %v, want requested and actual %q", rider, want)
	}
	if rider["exact"] != true {
		t.Errorf("exact = %v, want true", rider["exact"])
	}
	if _, present := rider["fallback_reason"]; present {
		t.Errorf("an exact answer carries a fallback reason: %v", rider["fallback_reason"])
	}
}

func TestBufferOverlayComposesOverTheRoutedView(t *testing.T) {
	stack := newViewStack(t)
	manager := daemon.NewOverlayManager(time.Hour)
	stack.srv.SetOverlayManager(manager)
	if err := manager.RegisterWithID(viewTestSession, ""); err != nil {
		t.Fatalf("register overlay session: %v", err)
	}
	if err := manager.Push(viewTestSession, daemon.OverlayFile{
		Path:    "repo/edit.go",
		Content: "package repo\n\nfunc Overlaid() {}\n",
	}, nil); err != nil {
		t.Fatalf("push overlay: %v", err)
	}

	var reader graph.Reader
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, &reader)); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !hasNode(reader, "repo/edit.go::Overlaid") {
		t.Error("the editor buffer's symbol is not visible over the routed view")
	}
	if hasNode(reader, "repo/edit.go::New") {
		t.Error("the routed generation's symbol survived a buffer that replaced its file")
	}
	if hasNode(reader, "repo/edit.go::Old") {
		t.Error("the corpus symbol survived both layers")
	}
	if !hasNode(reader, "repo/added.go::Fresh") {
		t.Error("the buffer overlay hid the routed view it should have composed over")
	}
}

func TestWorktreeSelectorOverridesTheSessionCWD(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	args := map[string]any{"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree}}
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", args, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !hasNode(reader, "repo/added.go::Fresh") {
		t.Error("the explicit selector did not override the primary the cwd binds to")
	}
	if rider := resultFreshness(t, res); rider["exact"] != true {
		t.Errorf("rider = %v, want an exact answer", rider)
	}
}

func TestPrimaryCWDReadsTheBaseCorpus(t *testing.T) {
	stack := newViewStack(t)
	var routed graph.Reader
	if _, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil, captureReader(stack.srv, &routed)); err != nil {
		t.Fatalf("call with a materializer: %v", err)
	}
	stack.srv.SetMaterializer(nil)
	var plain graph.Reader
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil, captureReader(stack.srv, &plain))
	if err != nil {
		t.Fatalf("call without a materializer: %v", err)
	}
	if routed != plain {
		t.Errorf("a dedicated checkout's cwd read through %T with a materializer and %T without one", routed, plain)
	}
	if !hasNode(routed, "repo/edit.go::Old") {
		t.Error("the base corpus answer lost the symbol only the corpus carries")
	}
	if rider := resultFreshness(t, res); rider != nil {
		t.Errorf("a base answer carries a view rider: %v", rider)
	}
}

func TestBuildingRouteFallsBackToTheBase(t *testing.T) {
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, 0, store_sqlite.RouteActive)

	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if hasNode(reader, "repo/added.go::Fresh") {
		t.Error("a half-routed checkout was served from its generations anyway")
	}
	if !hasNode(reader, "repo/edit.go::Old") {
		t.Error("the fallback did not land on the base corpus")
	}
	rider := resultFreshness(t, res)
	if rider == nil {
		t.Fatal("a fallback answered without saying so")
	}
	if rider["exact"] != false {
		t.Errorf("exact = %v, want false", rider["exact"])
	}
	if rider["fallback_reason"] != graphview.CodeViewBuilding {
		t.Errorf("fallback_reason = %v, want %q", rider["fallback_reason"], graphview.CodeViewBuilding)
	}
	if rider["actual_view"] != string(graphview.SelectorBase) {
		t.Errorf("actual_view = %v, want %q", rider["actual_view"], string(graphview.SelectorBase))
	}
}

func worktreeViewArgs() map[string]any {
	return map[string]any{"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree}}
}

func TestRemovalGraceFallsBackForEligibleGraphSearchWithoutBuffers(t *testing.T) {
	stack := newViewStack(t)
	stack.setWorktreeState(t, store_sqlite.CheckoutStateRemovalGrace)
	stack.seedRetiredWorktreeGraph(t)

	manager := daemon.NewOverlayManager(time.Hour)
	stack.srv.SetOverlayManager(manager)
	if err := manager.RegisterWithID(viewTestSession, ""); err != nil {
		t.Fatalf("register overlay session: %v", err)
	}
	if err := manager.Push(viewTestSession, daemon.OverlayFile{
		Path:    "repo/edit.go",
		Content: "package repo\n\nfunc GraceBufferMustNotLeak() {}\n",
	}, nil); err != nil {
		t.Fatalf("push overlay: %v", err)
	}

	var reader graph.Reader
	res, err := stack.callWithView(t, stack.repoRoot, "search_symbols", worktreeViewArgs(),
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			prepared, overlay, err := stack.srv.prepareOverlayRequest(ctx)
			if err != nil {
				return nil, err
			}
			if overlay != nil {
				return nil, errors.New("nested facade preparation composed an editor overlay over grace fallback")
			}
			reader = stack.srv.readerFor(prepared)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("eligible grace search was refused: %s", viewResultText(t, res))
	}
	if !hasNode(reader, "repo/edit.go::Old") {
		t.Error("the grace fallback did not read the primary base corpus")
	}
	if hasNode(reader, "repo/added.go::Fresh") {
		t.Error("the unavailable checkout's generation leaked into the grace fallback")
	}
	if hasNode(reader, "repo/edit.go::GraceBufferMustNotLeak") {
		t.Error("a session buffer composed over the read-only grace fallback")
	}

	rider := resultFreshness(t, res)
	wantActual := "base:" + stack.graphID
	if rider["requested_view"] != "worktree:"+viewTestWorktree || rider["actual_view"] != wantActual {
		t.Errorf("rider = %v, want worktree request and %q answer", rider, wantActual)
	}
	if rider["exact"] != false || rider["fallback_reason"] != string(store_sqlite.CheckoutStateRemovalGrace) {
		t.Errorf("rider = %v, want labeled removal-grace fallback", rider)
	}
	if rider["graph_id"] != stack.graphID || rider["checkout_id"] != viewTestWorktree {
		t.Errorf("rider identity = %v, want graph %q checkout %q", rider, stack.graphID, viewTestWorktree)
	}
	if rider["requested_state"] != string(store_sqlite.CheckoutStateReady) ||
		rider["actual_state"] != string(store_sqlite.CheckoutStateRemovalGrace) {
		t.Errorf("rider state = %v, want ready -> removal_grace", rider)
	}
}

func TestGraceFallbackRequiresAReadyPrimary(t *testing.T) {
	stack := newViewStack(t)
	stack.setWorktreeState(t, store_sqlite.CheckoutStateRemovalGrace)
	stack.setPrimaryGraphState(t, "graph_building")

	// Driven from the primary's own root, not the worktree root: a checkout in
	// removal grace no longer serves an automatic view, so a session sitting
	// inside it binds unresolved (see
	// TestAutomaticCheckoutScope_OnlyLiveAutomaticCheckoutsBind) and its
	// selector would refuse as selector_out_of_scope before readiness is ever
	// consulted. The primary-not-ready contract is exercised from the in-scope
	// primary root, where the grace selector resolves and reaches the readiness
	// gate this test pins.
	res, err := stack.callWithView(t, stack.repoRoot, "search_symbols", worktreeViewArgs(),
		captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertToolError(t, res, graphview.CodePrimaryNotReady)
}

func TestGraceWorktreeSelectorKeepsExactFileAndWriteRequestsStrict(t *testing.T) {
	stack := newViewStack(t)
	stack.setWorktreeState(t, store_sqlite.CheckoutStateRemovalGrace)
	stack.seedRetiredWorktreeGraph(t)

	for _, tool := range []string{"get_symbol", "read_file", "search_ast", "get_diagnostics", "edit_file", "change_contract"} {
		t.Run(tool, func(t *testing.T) {
			ran := false
			res, err := stack.callWithView(t, stack.repoRoot, tool, worktreeViewArgs(),
				func(context.Context) (*mcplib.CallToolResult, error) {
					ran = true
					return mcplib.NewToolResultText(`{"ok":true}`), nil
				})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertToolError(t, res, graphview.CodeCheckoutInaccessible)
			if ran {
				t.Errorf("%s reached its handler during removal grace", tool)
			}
		})
	}
}

func TestGraceWorktreeSelectorScopesPrimaryBeforeReadiness(t *testing.T) {
	for _, primaryState := range []string{reconcile.GraphStateReady, "graph_building"} {
		t.Run(primaryState, func(t *testing.T) {
			stack := newViewStack(t)
			stack.setWorktreeState(t, store_sqlite.CheckoutStateRemovalGrace)
			stack.seedRetiredWorktreeGraph(t)
			stack.setPrimaryGraphState(t, primaryState)

			ran := false
			res, err := stack.callWithView(t, stack.otherRoot, "search_symbols", worktreeViewArgs(),
				func(context.Context) (*mcplib.CallToolResult, error) {
					ran = true
					return mcplib.NewToolResultText(`{"ok":true}`), nil
				})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertToolError(t, res, graphview.CodeSelectorOutOfScope)
			if ran {
				t.Error("foreign-scope grace selector reached its handler")
			}
		})
	}
}

func BenchmarkGraceWorktreeSelectorPrimaryFallback(b *testing.B) {
	store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "grace-selector.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	catalog := store.Catalog()
	const (
		familyID         = "bench-grace-family"
		primaryCheckout  = "bench-grace-primary"
		worktreeCheckout = "bench-grace-worktree"
		primaryGraph     = "bench-grace-graph"
	)
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID: familyID, CommonDirIdentity: "bench-grace-common", State: "family_ready",
		CreatedAt: 1, LastSeen: 1,
	}); err != nil {
		b.Fatal(err)
	}
	for _, checkout := range []store_sqlite.Checkout{
		{
			CheckoutID: primaryCheckout, Incarnation: "bench-primary-inc", FamilyID: familyID,
			RootPath: "/bench/primary", GitDir: "/bench/.git", AdminName: "main",
			State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeDedicated,
			EffectiveMode: store_sqlite.CheckoutModeDedicated, LastSeen: 1,
		},
		{
			CheckoutID: worktreeCheckout, Incarnation: "bench-worktree-inc", FamilyID: familyID,
			RootPath: "/bench/worktree", GitDir: "/bench/.git/worktrees/worktree", AdminName: "worktree",
			State: store_sqlite.CheckoutStateRemovalGrace, DesiredMode: store_sqlite.CheckoutModeAutomatic,
			EffectiveMode: store_sqlite.CheckoutModeAutomatic, LastSeen: 1,
		},
	} {
		if err := catalog.UpsertCheckout(ctx, checkout); err != nil {
			b.Fatal(err)
		}
	}
	for _, dedicated := range []store_sqlite.DedicatedGraph{
		{
			GraphID: primaryGraph, OwnerCheckoutID: primaryCheckout, RepoPrefix: "bench-primary",
			FamilyID: familyID, IsPrimaryBase: true, State: reconcile.GraphStateReady,
		},
		{
			GraphID: "bench-retired-graph", OwnerCheckoutID: worktreeCheckout,
			RepoPrefix: "bench-retired", FamilyID: familyID, State: reconcile.GraphStateReady,
		},
	} {
		if err := catalog.UpsertDedicatedGraph(ctx, dedicated); err != nil {
			b.Fatal(err)
		}
	}

	engine := query.NewEngine(store)
	srv := NewServer(engine, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{})
	srv.SetMaterializer(&graphview.Materializer{
		Store: store, Catalog: catalog, Leases: graphview.NewLeaseManager(),
	})
	selector := graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: worktreeCheckout}
	policy := requestViewPolicy{allowGraceBaseFallback: true}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		view, err := srv.viewForWorktreeSelector(ctx, selector, policy)
		if err != nil {
			b.Fatal(err)
		}
		if view == nil || view.rider == nil || !view.suppressBufferOverlay {
			b.Fatalf("grace selector returned %+v", view)
		}
		view.close()
	}
}

func TestGraceFallbackPolicyUsesResolvedOperationEffects(t *testing.T) {
	stack := newViewStack(t)
	request := func(name string, args map[string]any) *mcplib.CallToolRequest {
		req := &mcplib.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = args
		return req
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		want bool
	}{
		{name: "search", args: map[string]any{"operation": "symbols"}, want: true},
		{name: "search_symbols", want: true},
		{name: "relations", args: map[string]any{"operation": "callers"}, want: true},
		{name: "analyze", args: map[string]any{"kind": "architecture"}, want: true},
		{name: "read", args: map[string]any{"operation": "source"}, want: false},
		{name: "search", args: map[string]any{"operation": "ast"}, want: false},
		{name: "analyze", args: map[string]any{"kind": "lint", "target": map[string]any{"file": "repo/edit.go"}}, want: false},
		{name: "analyze", args: map[string]any{"kind": "temporal_verify"}, want: false},
		{name: "edit", args: map[string]any{"operation": "file"}, want: false},
		{name: "change_contract", want: false},
	} {
		label := "legacy"
		if operation, _ := tc.args["operation"].(string); operation != "" {
			label = operation
		} else if kind, _ := tc.args["kind"].(string); kind != "" {
			label = kind
		}
		t.Run(tc.name+"/"+label, func(t *testing.T) {
			if got := stack.srv.requestAllowsGraceBaseFallback(request(tc.name, tc.args)); got != tc.want {
				t.Errorf("requestAllowsGraceBaseFallback(%s, %v) = %v, want %v", tc.name, tc.args, got, tc.want)
			}
		})
	}
}

func TestWorktreeSelectorRefusalsCarryTheirOwnCode(t *testing.T) {
	// A fresh map per call: the middleware strips the view argument, so a
	// shared one would arrive empty on the second request.
	worktreeArgs := func() map[string]any {
		return map[string]any{"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree}}
	}

	t.Run("outside the session scope", func(t *testing.T) {
		stack := newViewStack(t)
		res, err := stack.callWithView(t, stack.otherRoot, "get_symbol", worktreeArgs(), captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		assertToolError(t, res, graphview.CodeSelectorOutOfScope)
	})

	t.Run("unregistered checkout", func(t *testing.T) {
		stack := newViewStack(t)
		args := map[string]any{"view": map[string]any{"kind": "worktree", "checkout_id": "wt-nobody"}}
		res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args, captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		assertToolError(t, res, graphview.CodeCheckoutInaccessible)
	})

	t.Run("checkout that stopped answering", func(t *testing.T) {
		stack := newViewStack(t)
		stack.setWorktreeState(t, store_sqlite.CheckoutStateUnavailable)
		// From the in-scope primary root: an unavailable worktree no longer
		// serves an automatic view, so a session inside it binds unresolved and
		// the selector would refuse as selector_out_of_scope on the scope
		// ceiling. Named from the primary's own root, the selector resolves and
		// carries the checkout_inaccessible code its unavailable state earns.
		res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", worktreeArgs(), captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		assertToolError(t, res, graphview.CodeCheckoutInaccessible)
	})

	t.Run("family without a primary", func(t *testing.T) {
		stack := newViewStack(t)
		stack.demotePrimaryGraph(t)
		res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", worktreeArgs(), captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		assertToolError(t, res, graphview.CodeNoPrimary)
	})
}

func TestBaseSelectorNamesAReadyGraph(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	args := map[string]any{"view": map[string]any{"kind": "base", "graph_id": stack.graphID}}
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !hasNode(reader, "repo/edit.go::Old") {
		t.Error("a base selector did not read the indexed corpus")
	}
	rider := resultFreshness(t, res)
	if rider["actual_view"] != "base:"+stack.graphID || rider["exact"] != true {
		t.Errorf("rider = %v, want an exact base:%s answer", rider, stack.graphID)
	}

	unknown := map[string]any{"view": map[string]any{"kind": "base", "graph_id": "graph-nobody"}}
	res, err = stack.callWithView(t, stack.worktreeRoot, "get_symbol", unknown, captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertToolError(t, res, graphview.CodeInvalidViewSelector)
}

func TestBaseSelectorChecksScopeBeforeReadiness(t *testing.T) {
	args := func(graphID string) map[string]any {
		return map[string]any{"view": map[string]any{"kind": "base", "graph_id": graphID}}
	}

	t.Run("a session in scope is told the graph is building", func(t *testing.T) {
		stack := newViewStack(t)
		stack.setPrimaryGraphState(t, "graph_building")
		res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", args(stack.graphID), captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		assertToolError(t, res, graphview.CodeViewBuilding)
	})

	// The ceiling is checked first, so a session in another workspace gets the
	// same answer whatever state the graph is in: a building graph must not be
	// distinguishable from a ready one across the workspace boundary.
	for _, state := range []string{reconcile.GraphStateReady, "graph_building"} {
		t.Run("a session out of scope learns nothing from state "+state, func(t *testing.T) {
			stack := newViewStack(t)
			stack.setPrimaryGraphState(t, state)
			res, err := stack.callWithView(t, stack.otherRoot, "get_symbol", args(stack.graphID), captureReader(stack.srv, new(graph.Reader)))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertToolError(t, res, graphview.CodeSelectorOutOfScope)
		})
	}
}

func TestUnreadableCWDBindingSaysItFellBack(t *testing.T) {
	stack := newViewStack(t)
	breakCheckoutReads(t, stack.dbPath)

	var reader graph.Reader
	// Tracked-root metadata proves this canonical CWD independently of the
	// unreadable checkout table. The automatic sibling has no such proof.
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("canonical fallback was refused: %s", viewResultText(t, res))
	}
	if hasNode(reader, "repo/added.go::Fresh") {
		t.Error("an unreadable binding served the routed generations anyway")
	}
	if !hasNode(reader, "repo/edit.go::Old") {
		t.Error("the fallback did not land on the base corpus")
	}
	rider := resultFreshness(t, res)
	if rider == nil {
		t.Fatal("an unreadable binding degraded to the base without saying so")
	}
	if rider["requested_view"] != string(graphview.SelectorAuto) {
		t.Errorf("requested_view = %v, want %q", rider["requested_view"], string(graphview.SelectorAuto))
	}
	if rider["actual_view"] != string(graphview.SelectorBase) {
		t.Errorf("actual_view = %v, want %q", rider["actual_view"], string(graphview.SelectorBase))
	}
	if rider["exact"] != false {
		t.Errorf("exact = %v, want false", rider["exact"])
	}
	if rider["fallback_reason"] != graphview.CodeCheckoutInaccessible {
		t.Errorf("fallback_reason = %v, want %q", rider["fallback_reason"], graphview.CodeCheckoutInaccessible)
	}
}

func TestUnreadableAutomaticBindingDoesNotReturnBaseData(t *testing.T) {
	stack := newViewStack(t)
	breakCheckoutReads(t, stack.dbPath)

	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertToolError(t, res, graphview.CodeCheckoutInaccessible)
	if reader != nil {
		t.Fatal("unreadable automatic binding reached a graph reader without checkout authority")
	}
}

// TestRefSelectorsFailLoudlyOnAGraphWithNothingToBuildOver pins that a
// selector naming committed state the server cannot produce is refused rather
// than quietly answered from the base corpus. This fixture's graph records no
// committed tree, so there is nothing for a commit layer to diff against.
func TestRefSelectorsFailLoudlyOnAGraphWithNothingToBuildOver(t *testing.T) {
	stack := newViewStack(t)
	for _, sel := range []map[string]any{
		{"kind": "git_ref", "value": "refs/heads/main"},
		{"kind": "commit", "value": strings.Repeat("a", 40)},
	} {
		res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
			map[string]any{"view": sel}, captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call %v: %v", sel, err)
		}
		assertToolError(t, res, graphview.CodeCheckoutInaccessible)
	}
}

// TestRefSelectorRefusesAnAmbiguousGraph pins that a session reaching several
// repositories must name the one it means: the same branch name in two of them
// is two different answers.
func TestRefSelectorRefusesAnAmbiguousGraph(t *testing.T) {
	stack := newViewStack(t)
	res, err := stack.callWithView(t, "", "get_symbol",
		map[string]any{"view": map[string]any{"kind": "git_ref", "value": "refs/heads/main"}},
		captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertToolError(t, res, graphview.CodeInvalidViewSelector)
}

func TestMalformedSelectorReportsInvalidViewSelector(t *testing.T) {
	stack := newViewStack(t)
	for _, sel := range []any{
		map[string]any{"kind": "branch", "value": "main"},
		map[string]any{"kind": "git_ref", "value": "main"},
		map[string]any{"kind": "worktree"},
		map[string]any{"kind": "auto", "nonsense": "x"},
		"worktree",
	} {
		res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
			map[string]any{"view": sel}, captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call %v: %v", sel, err)
		}
		assertToolError(t, res, graphview.CodeInvalidViewSelector)
	}
}

func TestMutationAgainstARoutedViewIsRefused(t *testing.T) {
	stack := newViewStack(t)
	// The facade names (edit / refactor) are refused earlier by the session's
	// surface gate on this legacy-surface server, so the tools under test here
	// are the implementations both facades dispatch to.
	for _, tool := range []string{"edit_file", "write_file", "batch_edit", "rename_symbol", "safe_delete_symbol"} {
		ran := false
		res, err := stack.callWithView(t, stack.worktreeRoot, tool, nil,
			func(context.Context) (*mcplib.CallToolResult, error) {
				ran = true
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			})
		if err != nil {
			t.Fatalf("call %s: %v", tool, err)
		}
		assertToolError(t, res, graphview.CodeViewReadOnly)
		if ran {
			t.Errorf("%s reached its handler against a routed view", tool)
		}
	}
	// A read tool on the same request is untouched.
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("call get_symbol: %v", err)
	}
	if res.IsError {
		t.Errorf("a read tool was refused as a mutation: %s", viewResultText(t, res))
	}
}

func TestRoutedViewLeaseIsHeldForTheRequestOnly(t *testing.T) {
	stack := newViewStack(t)
	ctx := context.Background()

	var duringErr error
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			if !hasNode(stack.srv.readerFor(hctx), "repo/added.go::Fresh") {
				return nil, errors.New("the request did not read through the routed view")
			}
			// Retirement refuses a generation a route still points at, so the
			// route is dropped first: what is under test is the lease.
			if err := stack.store.Catalog().DeleteCheckoutRoute(ctx, viewTestWorktree); err != nil {
				return nil, err
			}
			duringErr = stack.store.RetirePayloadGeneration(ctx, stack.dirty, stack.leases.InUse)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !errors.Is(duringErr, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire during the request = %v, want %v", duringErr, store_sqlite.ErrPayloadGenerationInUse)
	}
	if stack.leases.InUse(stack.dirty) {
		t.Fatal("the request ended with its lease still held")
	}
	if err := stack.store.RetirePayloadGeneration(ctx, stack.dirty, stack.leases.InUse); err != nil {
		t.Fatalf("retire after the request: %v", err)
	}
}

// assertToolError checks that res is a structured tool error carrying code.
func assertToolError(t *testing.T, res *mcplib.CallToolResult, code string) {
	t.Helper()
	if res == nil || !res.IsError {
		t.Fatalf("expected a %s error, got %+v", code, res)
	}
	text := viewResultText(t, res)
	if !strings.Contains(text, code) {
		t.Fatalf("error %q does not carry %q", text, code)
	}
}
