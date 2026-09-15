package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

// The consumers that answered every request out of the base corpus, and
// what each of them does now: the non-tool surfaces (resources/read,
// prompts/get) read the request's view; the whole-daemon health probe stays a
// store-free liveness call and says base_scoped under a view; the speculative
// editor resolves its paths and diffs its impact in the checkout the request
// reads, and declares exactly the two parts of its answer that stay base-scoped.

// readBoundResource drives one resources/read through the production wrapper —
// the same wrapper addResource installs — and hands the leaf handler back what
// it read through.
func (v *viewStack) readBoundResource(
	t *testing.T,
	cwd, uri string,
	leaf func(ctx context.Context) ([]mcplib.ResourceContents, error),
) ([]mcplib.ResourceContents, error) {
	t.Helper()
	handler := v.srv.boundResourceHandler(uri, func(ctx context.Context, _ mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
		return leaf(ctx)
	})
	req := mcplib.ReadResourceRequest{}
	req.Params.URI = uri
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), cwd)
	return handler(ctx, req)
}

// getBoundPrompt is readBoundResource for prompts/get.
func (v *viewStack) getBoundPrompt(
	t *testing.T,
	cwd, name string,
	leaf func(ctx context.Context) (*mcplib.GetPromptResult, error),
) (*mcplib.GetPromptResult, error) {
	t.Helper()
	handler := v.srv.boundPromptHandler(name, func(ctx context.Context, _ mcplib.GetPromptRequest) (*mcplib.GetPromptResult, error) {
		return leaf(ctx)
	})
	req := mcplib.GetPromptRequest{}
	req.Params.Name = name
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), cwd)
	return handler(ctx, req)
}

// TestResourceReadBindsTheSessionView is the production-entrypoint trace for
// the resource half: nothing outside the tools/call middleware ever called
// resolveRequestView, so every resource handler's s.readerFor(ctx) fell through
// to the base graph while the session was bound to a worktree.
func TestResourceReadBindsTheSessionView(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	if _, err := stack.readBoundResource(t, stack.worktreeRoot, "gortex://stats",
		func(ctx context.Context) ([]mcplib.ResourceContents, error) {
			reader = stack.srv.readerFor(ctx)
			return nil, nil
		}); err != nil {
		t.Fatalf("resource read: %v", err)
	}
	if reader == nil {
		t.Fatal("the resource read through no reader at all")
	}
	if !hasNode(reader, "repo/keep.go::Dirty") {
		t.Error("a symbol that exists only in the routed working-tree generation is not visible to a resource read")
	}
	if !hasNode(reader, "repo/added.go::Fresh") {
		t.Error("a symbol that exists only in the routed commit generation is not visible to a resource read")
	}
	if hasNode(reader, "repo/edit.go::Old") {
		t.Error("the corpus symbol the routed generation replaced is still visible to a resource read")
	}
}

// TestResourceReadOutsideAViewStillReadsTheCorpus is the control: a session
// whose cwd binds no checkout answers exactly as it always did.
func TestResourceReadOutsideAViewStillReadsTheCorpus(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	if _, err := stack.readBoundResource(t, "", "gortex://stats",
		func(ctx context.Context) ([]mcplib.ResourceContents, error) {
			reader = stack.srv.readerFor(ctx)
			return nil, nil
		}); err != nil {
		t.Fatalf("resource read: %v", err)
	}
	if !hasNode(reader, "repo/edit.go::Old") {
		t.Error("an unbound resource read stopped seeing the indexed corpus")
	}
	if hasNode(reader, "repo/keep.go::Dirty") {
		t.Error("an unbound resource read saw a routed generation's symbol")
	}
}

// TestPromptGetBindsTheSessionView is the same trace for prompts/get, which
// travels the other half of the same wrapper.
func TestPromptGetBindsTheSessionView(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	if _, err := stack.getBoundPrompt(t, stack.worktreeRoot, "orient",
		func(ctx context.Context) (*mcplib.GetPromptResult, error) {
			reader = stack.srv.readerFor(ctx)
			return &mcplib.GetPromptResult{}, nil
		}); err != nil {
		t.Fatalf("prompt get: %v", err)
	}
	if reader == nil {
		t.Fatal("the prompt read through no reader at all")
	}
	if !hasNode(reader, "repo/keep.go::Dirty") {
		t.Error("a symbol that exists only in the routed working-tree generation is not visible to a prompt")
	}
	if hasNode(reader, "repo/edit.go::Old") {
		t.Error("the corpus symbol the routed generation replaced is still visible to a prompt")
	}
}

// TestResourceReadReleasesItsView pins the lifetime half: the view a resource
// read leases is released when the read returns, exactly as the tool
// middleware's `defer view.close()` releases it.
func TestResourceReadReleasesItsView(t *testing.T) {
	stack := newViewStack(t)
	var captured *requestView
	if _, err := stack.readBoundResource(t, stack.worktreeRoot, "gortex://stats",
		func(ctx context.Context) ([]mcplib.ResourceContents, error) {
			captured = requestViewFromContext(ctx)
			return nil, nil
		}); err != nil {
		t.Fatalf("resource read: %v", err)
	}
	if captured == nil {
		t.Fatal("the resource read installed no request view")
	}
	if captured.materialized == nil {
		t.Fatal("the resource read materialized no generation stack to release")
	}
	if stack.leases.InUse(stack.dirty) {
		t.Error("the resource read returned with its lease still held")
	}
}

// TestResourceReadHoldsItsViewForTheReadOnly is the other half: the lease is
// genuinely held while the handler runs, so the generations it reads cannot be
// retired underneath it.
func TestResourceReadHoldsItsViewForTheReadOnly(t *testing.T) {
	stack := newViewStack(t)
	var duringErr error
	if _, err := stack.readBoundResource(t, stack.worktreeRoot, "gortex://stats",
		func(ctx context.Context) ([]mcplib.ResourceContents, error) {
			// Retirement refuses a generation a route still points at, so the
			// route is dropped first: what is under test is the lease.
			if err := stack.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree); err != nil {
				return nil, err
			}
			duringErr = stack.store.RetirePayloadGeneration(context.Background(), stack.dirty, stack.leases.InUse)
			return nil, nil
		}); err != nil {
		t.Fatalf("resource read: %v", err)
	}
	if !errors.Is(duringErr, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire during the resource read = %v, want %v", duringErr, store_sqlite.ErrPayloadGenerationInUse)
	}
	if stack.leases.InUse(stack.dirty) {
		t.Fatal("the resource read ended with its lease still held")
	}
}

// TestResourceStatsEqualsTheGraphStatsTool is the equality the two surfaces are
// documented to have (tools_core.go: "Shared with the `gortex://stats` resource
// so both surfaces stay byte-for-byte equal"). It was false for any session
// with a view, because only the tool had one on its context.
func TestResourceStatsEqualsTheGraphStatsTool(t *testing.T) {
	stack := newViewStack(t)

	var fromResource map[string]any
	if _, err := stack.readBoundResource(t, stack.worktreeRoot, "gortex://stats",
		func(ctx context.Context) ([]mcplib.ResourceContents, error) {
			fromResource = stack.srv.buildGraphStatsPayload(ctx)
			return nil, nil
		}); err != nil {
		t.Fatalf("resource read: %v", err)
	}

	var fromTool map[string]any
	if _, err := stack.callWithView(t, stack.worktreeRoot, "graph_stats", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			fromTool = stack.srv.buildGraphStatsPayload(ctx)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("graph_stats call: %v", err)
	}

	if fromResource == nil || fromTool == nil {
		t.Fatal("one of the two surfaces produced no stats payload")
	}
	if got, want := viewDerivedStatsJSON(t, fromResource), viewDerivedStatsJSON(t, fromTool); got != want {
		t.Errorf("the two surfaces disagree under a view:\n  resource   = %s\n  graph_stats = %s", got, want)
	}

	// And the routed answer is not simply the corpus answer: if it were, the
	// equality above would pass for the wrong reason.
	var fromBase map[string]any
	if _, err := stack.callWithView(t, "", "graph_stats", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			fromBase = stack.srv.buildGraphStatsPayload(ctx)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("unbound graph_stats call: %v", err)
	}
	if viewDerivedStatsJSON(t, fromBase) == viewDerivedStatsJSON(t, fromTool) {
		t.Fatalf("the routed and the base stats are identical (%s), so this fixture cannot tell the two apart",
			viewDerivedStatsJSON(t, fromBase))
	}
}

// viewDerivedStatsJSON renders every field of the stats payload that the
// request's view determines, in canonical JSON, so the two surfaces are
// compared as bytes rather than on a couple of integers.
//
// The excluded keys are the ones the view does not determine and that a caller
// would be wrong to read as view-scoped: token_savings and cumulative_savings
// are per-session accounting, notifications is subscription state, ppr_cache is
// a process-wide cache counter that the act of measuring moves, and semantic is
// the language-server roster. buildGraphStatsPayload (tools_core.go) sources
// everything below from s.engineFor(ctx) / s.readerFor(ctx).
func viewDerivedStatsJSON(t *testing.T, payload map[string]any) string {
	t.Helper()
	subset := map[string]any{}
	for _, key := range []string{"total_nodes", "total_edges", "by_kind", "by_language", "edge_identity_revisions", "per_repo"} {
		if value, ok := payload[key]; ok {
			subset[key] = value
		}
	}
	if len(subset) == 0 {
		t.Fatalf("the stats payload carries no view-derived field at all: %v", payload)
	}
	encoded, err := json.Marshal(subset)
	if err != nil {
		t.Fatalf("marshal stats subset: %v", err)
	}
	return string(encoded)
}

// TestIndexHealthSaysBaseScopedUnderAView covers the consumer that cannot be
// bound: the health payload is a whole-daemon probe of s.graph and the
// indexer's own ledger, cached per server, so under a view it describes the
// base corpus and the rider has to say so.
func TestIndexHealthSaysBaseScopedUnderAView(t *testing.T) {
	stack := newViewStack(t)
	args := map[string]any{
		viewArgName: map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree},
	}
	res, err := stack.callHandler(t, "", "index_health", args, stubLeaf)
	if err != nil {
		t.Fatalf("index_health under the view: %v", err)
	}
	if res.IsError {
		t.Fatalf("index_health under the view: %s", viewResultText(t, res))
	}
	rider := resultFreshness(t, res)
	if rider == nil {
		t.Fatalf("a routed answer carries no rider: %s", viewResultText(t, res))
	}
	named := map[string]bool{}
	entries, _ := rider["base_scoped"].([]any)
	for _, entry := range entries {
		name, _ := entry.(string)
		named[name] = true
	}
	for _, want := range []graphview.CapabilityID{graphview.CapSyntaxGraph, graphview.CapSourceSnapshot} {
		if !named[string(want)] {
			t.Errorf("base_scoped = %v, want it to name %s", rider["base_scoped"], want)
		}
	}

	// The control: on a base request the whole-daemon probe IS the answer, and
	// a tool with no base-scoped engine behind it is never annotated.
	plain, err := stack.callHandler(t, "", "index_health", nil, stubLeaf)
	if err != nil {
		t.Fatalf("index_health on the base: %v", err)
	}
	if strings.Contains(viewResultText(t, plain), "base_scoped") {
		t.Errorf("a base answer was annotated as base-scoped:\n%s", viewResultText(t, plain))
	}
	other, err := stack.callHandler(t, "", "get_symbol", args, stubLeaf)
	if err != nil {
		t.Fatalf("get_symbol under the view: %v", err)
	}
	if strings.Contains(viewResultText(t, other), "base_scoped") {
		t.Errorf("a tool with no base-scoped engine was annotated:\n%s", viewResultText(t, other))
	}
}

// TestIndexHealthProbeTouchesNoStore is the other half of the index_health
// statement: the payload is annotated, and the annotation is the ONLY thing
// that happens on the probe's request path. index_health is documented the
// cheap liveness call (CLAUDE.md, in contrast to graph_stats), and a cached
// payload that is still inside its TTL must be served without asking the store
// anything — a corpus identity read here would be two whole-generation
// COUNT(*) scans per call on the SQL backend (store_sqlite/store.go,
// stmtNodeCount / stmtEdgeCount).
func TestIndexHealthProbeTouchesNoStore(t *testing.T) {
	stack := newViewStack(t)
	srv := stack.srv

	srv.indexHealth.mu.Lock()
	srv.indexHealth.payload = map[string]any{"health_score": 100.0, "node_count": 7}
	srv.indexHealth.updatedAt = time.Now()
	srv.indexHealth.mu.Unlock()

	counting := &countingReader{Store: stack.store}
	srv.graph = counting
	t.Cleanup(func() { srv.graph = stack.store })

	got, updatedAt, _ := srv.indexHealthSnapshot()
	if got == nil {
		t.Fatal("a cached payload inside the TTL is not served")
	}
	if srv.indexHealthNeedsRefresh(updatedAt) {
		t.Error("a cached payload inside the TTL was scheduled for a rebuild, so every poll would rebuild it")
	}
	if n := counting.counts(); n != 0 {
		t.Errorf("the cache accessors made %d whole-generation count scans, want 0", n)
	}

	// The same property through the production entrypoint, which reads the
	// snapshot twice per call (tools_enhancements.go, handleIndexHealth): an
	// agent polling the liveness probe must not be charged a scan per poll.
	// handleIndexHealth refuses without an indexer, so the fixture's server
	// gets the same root-only one the reviewer-suggestion tests use.
	srv.indexer = rootOnlyIndexer(stack.repoRoot)
	res, err := stack.callHandler(t, "", "index_health", nil, srv.handleIndexHealth)
	if err != nil {
		t.Fatalf("index_health: %v", err)
	}
	if res.IsError {
		t.Fatalf("index_health: %s", viewResultText(t, res))
	}
	if n := counting.counts(); n != 0 {
		t.Errorf("one index_health call made %d whole-generation count scans, want 0:\n%s",
			n, viewResultText(t, res))
	}
}

// countingReader counts the two whole-generation COUNT(*) reads a corpus
// identity would make, so the probe's cost is a pinned property and not a
// claim in a comment.
type countingReader struct {
	graph.Store
	n atomic.Int64
}

func (c *countingReader) NodeCount() int {
	c.n.Add(1)
	return c.Store.NodeCount()
}

func (c *countingReader) EdgeCount() int {
	c.n.Add(1)
	return c.Store.EdgeCount()
}

func (c *countingReader) counts() int64 { return c.n.Load() }

// TestSimulationResolvesEditPathsInTheRoutedCheckout covers the speculative
// editor's path half. Every registered indexer root is a repository's canonical
// checkout, so the legacy resolver lands a repo-relative edit in the wrong tree
// — and the pre-edit content the simulation applies the caller's TextEdits to
// is read from whatever path comes out of it.
func TestSimulationResolvesEditPathsInTheRoutedCheckout(t *testing.T) {
	stack := newViewStack(t)
	if err := os.WriteFile(filepath.Join(stack.worktreeRoot, "edit.go"),
		[]byte("package repo\n\nfunc New() {}\n"), 0o644); err != nil {
		t.Fatalf("write the worktree's copy: %v", err)
	}
	edit := lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		"repo/edit.go": {{NewText: "package repo\n"}},
	}}

	var routed, unrouted []simulationFileEdit
	if _, err := stack.callWithView(t, stack.worktreeRoot, "preview_edit", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			var err error
			routed, err = stack.srv.groupEditByFileCtx(ctx, edit)
			return mcplib.NewToolResultText(`{"ok":true}`), err
		}); err != nil {
		t.Fatalf("routed grouping: %v", err)
	}
	if _, err := stack.callWithView(t, "", "preview_edit", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			var err error
			unrouted, err = stack.srv.groupEditByFileCtx(ctx, edit)
			return mcplib.NewToolResultText(`{"ok":true}`), err
		}); err != nil {
		t.Fatalf("unrouted grouping: %v", err)
	}

	if len(routed) != 1 || len(unrouted) != 1 {
		t.Fatalf("grouping produced %d routed and %d unrouted entries, want one each", len(routed), len(unrouted))
	}
	wantRouted := filepath.Join(stack.worktreeRoot, "edit.go")
	if !samePath(routed[0].absPath, wantRouted) {
		t.Errorf("routed edit resolved to %q, want the checkout the request reads (%q)", routed[0].absPath, wantRouted)
	}
	wantBase := filepath.Join(stack.repoRoot, "edit.go")
	if !samePath(unrouted[0].absPath, wantBase) {
		t.Errorf("unrouted edit resolved to %q, want the canonical checkout (%q)", unrouted[0].absPath, wantBase)
	}
}

// samePath compares two absolute paths through the symlinked spellings a temp
// root has on darwin (/tmp versus /private/tmp).
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}

// TestSimulationDiffsAgainstTheRoutedView covers the speculative editor's graph
// half. The shadow view composed the step's layer onto s.graph regardless of
// what answered the request, so a simulation under a worktree view diffed the
// caller's edit against a different branch's symbols and reported the resulting
// phantom removals as removed symbols and broken callers.
func TestSimulationDiffsAgainstTheRoutedView(t *testing.T) {
	stack := newViewStack(t)
	if err := os.WriteFile(filepath.Join(stack.worktreeRoot, "edit.go"),
		[]byte("package repo\n\nfunc New() {}\n"), 0o644); err != nil {
		t.Fatalf("write the worktree's copy: %v", err)
	}
	edit := lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		"repo/edit.go": {{
			Range:   lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 3, Character: 0}},
			NewText: "package repo\n",
		}},
	}}

	var sim *simulation
	if _, err := stack.callWithView(t, stack.worktreeRoot, "preview_edit", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			var err error
			sim, err = stack.srv.buildSimulation(ctx, []lsp.WorkspaceEdit{edit}, false)
			return mcplib.NewToolResultText(`{"ok":true}`), err
		}); err != nil {
		t.Fatalf("routed simulation: %v", err)
	}
	if sim == nil || len(sim.steps) != 1 {
		t.Fatalf("the simulation produced %v steps, want one", sim)
	}
	removed := map[string]bool{}
	for _, id := range sim.steps[0].symbolsRemoved {
		removed[id] = true
	}
	if !removed["repo/edit.go::New"] {
		t.Errorf("symbols_removed = %v, want the routed generation's symbol", sim.steps[0].symbolsRemoved)
	}
	if removed["repo/edit.go::Old"] {
		t.Errorf("symbols_removed = %v, want no corpus symbol the routed view had already replaced",
			sim.steps[0].symbolsRemoved)
	}
}

// TestRoutedSimulationDeclaresOnlyWhatIsBaseScoped pins the simulator's rider.
//
// Binding the simulation moved its diff, its broken callers and its dependency
// walk onto the request's view, so the blanket "this whole answer is base
// scoped" it used to carry became untrue. Two parts of the answer genuinely are
// not the view's, and each arm is checked here in isolation plus the two
// controls, because a rider is wire-visible output: dropping either statement
// must fail a test, and neither may fire when the thing it describes did not
// contribute.
//
// The entrypoint is the production handler, driven through the real tool
// middleware, so what is asserted is what a client receives.
func TestRoutedSimulationDeclaresOnlyWhatIsBaseScoped(t *testing.T) {
	editJSON := func(t *testing.T) string {
		t.Helper()
		encoded, err := json.Marshal(lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
			"repo/edit.go": {{
				Range:   lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 3, Character: 0}},
				NewText: "package repo\n",
			}},
		}})
		if err != nil {
			t.Fatalf("marshal the workspace edit: %v", err)
		}
		return string(encoded)
	}

	// preview_edit under the worktree view, with the two contributors switched
	// on and off independently.
	run := func(t *testing.T, routed bool, communities bool, languageServer bool) map[string]bool {
		t.Helper()
		stack := newViewStack(t)
		cwd := ""
		if routed {
			cwd = stack.worktreeRoot
		}
		if err := os.WriteFile(filepath.Join(stack.worktreeRoot, "edit.go"),
			[]byte("package repo\n\nfunc New() {}\n"), 0o644); err != nil {
			t.Fatalf("write the worktree's copy: %v", err)
		}
		if communities {
			installCommunitiesForTest(stack.srv, &analysis.CommunityResult{
				NodeToComm: map[string]string{"repo/edit.go::New": "c1"},
			})
		}
		if languageServer {
			// A manager with no provider registered: lspProviderForPath fails
			// per file, so no subprocess is spawned, and the statement under
			// test is about where a language server is ROOTED, not whether one
			// answered (tools_simulate.go, simulateDiagnosticsAtStep).
			stack.srv.semanticMgr = &semantic.Manager{}
		}
		args := map[string]any{
			"workspace_edit": editJSON(t),
			"diagnostics":    languageServer,
		}
		res, err := stack.callHandler(t, cwd, "preview_edit", args, stack.srv.handlePreviewEdit)
		if err != nil {
			t.Fatalf("preview_edit: %v", err)
		}
		if res.IsError {
			t.Fatalf("preview_edit: %s", viewResultText(t, res))
		}
		named := map[string]bool{}
		rider := resultFreshness(t, res)
		entries, _ := rider["base_scoped"].([]any)
		for _, entry := range entries {
			name, _ := entry.(string)
			named[name] = true
		}
		return named
	}

	t.Run("the corpus-wide analysis behind the impact rollup", func(t *testing.T) {
		named := run(t, true, true, false)
		if !named[string(graphview.CapSyntaxGraph)] {
			t.Errorf("base_scoped = %v, want it to name %s: the community partition and the process discovery that graded this impact are the whole corpus's, not the view's",
				named, graphview.CapSyntaxGraph)
		}
		if named[string(graphview.CapLSPDiagnostics)] {
			t.Errorf("base_scoped = %v names the language server, which this request never consulted", named)
		}
	})

	t.Run("the language server behind the diagnostics", func(t *testing.T) {
		named := run(t, true, false, true)
		if !named[string(graphview.CapLSPDiagnostics)] {
			t.Errorf("base_scoped = %v, want it to name %s: every registered workspace root is a repository's canonical checkout",
				named, graphview.CapLSPDiagnostics)
		}
		if named[string(graphview.CapSyntaxGraph)] {
			t.Errorf("base_scoped = %v names the corpus analysis, which fed no part of this answer", named)
		}
	})

	t.Run("neither contributor: the whole answer is the view's", func(t *testing.T) {
		if named := run(t, true, false, false); len(named) != 0 {
			t.Errorf("base_scoped = %v on an answer entirely derived from the view", named)
		}
	})

	t.Run("no view: reading the corpus IS the answer", func(t *testing.T) {
		if named := run(t, false, true, true); len(named) != 0 {
			t.Errorf("base_scoped = %v on an unrouted request", named)
		}
	})
}

// TestClosureProximityReadsTheSelectedSnapshot is the standing-contract check
// for the closure consumer: the seeded-walk adjacency it ranks by is built from
// the request's own reader, not from the shared whole-graph analysis pass.
func TestClosureProximityReadsTheSelectedSnapshot(t *testing.T) {
	stack := newViewStack(t)
	if _, err := stack.callWithView(t, stack.worktreeRoot, "context_closure", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			reader := stack.srv.selectedSnapshotReader(ctx)
			if reader == nil {
				t.Error("a routed closure ranks against no selected reader, so it would use the shared base snapshot")
			} else if !hasNode(reader, "repo/keep.go::Dirty") {
				t.Error("the closure's selected reader does not carry the routed generation's symbols")
			}
			_, scope := stack.srv.requestProximityAdjacency(ctx,
				[]string{"repo/keep.go::Keeper"}, []string{"repo/keep.go::Dirty"})
			if scope.source != pprWalkSourceSelectedReader {
				t.Errorf("proximity walk scope = %q, want the selected-reader namespace", scope.source)
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("closure call: %v", err)
	}
}

// TestRoutedSimulationKeepsTheLanguageServerRepoRooted is the second half of
// re-rooting the simulator, and the one that is easy to get backwards.
//
// The BYTES a simulation opens, reads and restores must be the view's — that is
// TestSimulationResolvesEditPathsInTheRoutedCheckout. The WORKSPACE the language
// server analyses them in must not be: a routed checkout is deliberately never a
// registered MultiIndexer root, so workspaceRootFor (tools_lsp.go) matches it
// against no tracked repo and falls through to its last-resort "the file's own
// directory" arm. Keying the router's (spec, workspace) provider cache on that
// spelling spawns one language server per touched directory, each rooted below
// the module root, each answering with degraded or empty diagnostics.
//
// The control arm is what makes the assertion mean something: the worktree
// spelling really does derive a different, module-less root.
func TestRoutedSimulationKeepsTheLanguageServerRepoRooted(t *testing.T) {
	stack := newViewStack(t)
	if err := os.WriteFile(filepath.Join(stack.worktreeRoot, "edit.go"),
		[]byte("package repo\n\nfunc New() {}\n"), 0o644); err != nil {
		t.Fatalf("write the worktree's copy: %v", err)
	}

	var routedAbs, routedAnchor, unroutedAbs, unroutedAnchor string
	if _, err := stack.callWithView(t, stack.worktreeRoot, "preview_edit", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			var err error
			routedAbs, err = stack.srv.resolveOverlayRequestAbsPath(ctx, "repo/edit.go")
			routedAnchor = stack.srv.simulationLSPAnchorPath(ctx, routedAbs)
			return mcplib.NewToolResultText(`{"ok":true}`), err
		}); err != nil {
		t.Fatalf("routed resolution: %v", err)
	}
	if _, err := stack.callWithView(t, "", "preview_edit", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			var err error
			unroutedAbs, err = stack.srv.resolveOverlayRequestAbsPath(ctx, "repo/edit.go")
			unroutedAnchor = stack.srv.simulationLSPAnchorPath(ctx, unroutedAbs)
			return mcplib.NewToolResultText(`{"ok":true}`), err
		}); err != nil {
		t.Fatalf("unrouted resolution: %v", err)
	}

	// Precondition: the fixture really did re-root the bytes, or the anchoring
	// below has nothing to undo.
	if !samePath(routedAbs, filepath.Join(stack.worktreeRoot, "edit.go")) {
		t.Fatalf("the routed request resolved %q, so this fixture never left the canonical checkout", routedAbs)
	}

	// Control: the worktree spelling derives a root that is not the repository.
	worktreeDerived, err := stack.srv.workspaceRootFor(routedAbs)
	if err != nil {
		t.Fatalf("workspaceRootFor(%q): %v", routedAbs, err)
	}
	if samePath(worktreeDerived, stack.repoRoot) {
		t.Fatalf("the worktree spelling already derives the repository root %q, so this test proves nothing", worktreeDerived)
	}
	if !samePath(worktreeDerived, stack.worktreeRoot) {
		t.Errorf("the worktree spelling derived %q, want the last-resort own-directory arm (%q)",
			worktreeDerived, stack.worktreeRoot)
	}

	// The anchoring: the provider lookup is keyed on the canonical checkout,
	// so the language server stays rooted at the repository.
	if !samePath(routedAnchor, filepath.Join(stack.repoRoot, "edit.go")) {
		t.Errorf("the routed simulation anchors its language server at %q, want the canonical checkout (%q)",
			routedAnchor, filepath.Join(stack.repoRoot, "edit.go"))
	}
	anchored, err := stack.srv.workspaceRootFor(routedAnchor)
	if err != nil {
		t.Fatalf("workspaceRootFor(%q): %v", routedAnchor, err)
	}
	if !samePath(anchored, stack.repoRoot) {
		t.Errorf("a routed simulation would spawn a language server rooted at %q, want the tracked repository %q",
			anchored, stack.repoRoot)
	}

	// And an unrouted request is untouched in both spellings.
	if !samePath(unroutedAnchor, unroutedAbs) {
		t.Errorf("an unrouted simulation moved its language server from %q to %q", unroutedAbs, unroutedAnchor)
	}
	if !samePath(unroutedAnchor, filepath.Join(stack.repoRoot, "edit.go")) {
		t.Errorf("the unrouted anchor is %q, want the canonical checkout", unroutedAnchor)
	}
}

// TestBaseSelectorSimulationStaysWholeCorpusAndSaysSo covers the routed request
// that reads no checkout of its own.
//
// A labelled `base` selector is routed() — it carries a reader — but that reader
// is a filter over the shared corpus, and it serves neither bounded localization
// projection, so the overlay layer's removal markers are read off the corpus
// (overlayBaseReaderFor, overlay_view.go). If the diff, the callers and the
// impact rollup were taken against the narrowed reader instead, the two halves
// of one answer would describe different corpora, and nothing on the response
// would say which. Two things are pinned here:
//
//  1. the cross-repo broken-caller contract survives — a caller in the sibling
//     repository, which the narrowed reader filters out before GetCallers can
//     see it, is still reported;
//  2. the answer states that it is the base corpus's and not the selected
//     view's, naming both the syntax graph and the local resolution.
func TestBaseSelectorSimulationStaysWholeCorpusAndSaysSo(t *testing.T) {
	stack := newViewStack(t)

	// A cross-repo call into the symbol the edit removes. It lives in the
	// corpus (generation zero), which is exactly what a narrowed base reader
	// filters by repo prefix.
	stack.store.AddBatch(nil, []*graph.Edge{{
		From:     baseSelectorForeignNode,
		To:       baseSelectorOwnNode,
		Kind:     graph.EdgeCalls,
		FilePath: baseSelectorForeignFile,
		Line:     3,
	}})

	encoded, err := json.Marshal(lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		"repo/edit.go": {{
			Range:   lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 3, Character: 0}},
			NewText: "package repo\n",
		}},
	}})
	if err != nil {
		t.Fatalf("marshal the workspace edit: %v", err)
	}

	res, err := stack.callHandler(t, stack.repoRoot, "preview_edit",
		baseSelectorArgs(stack.graphID, map[string]any{"workspace_edit": string(encoded)}),
		stack.srv.handlePreviewEdit)
	if err != nil {
		t.Fatalf("preview_edit under the base selector: %v", err)
	}
	if res.IsError {
		t.Fatalf("preview_edit under the base selector: %s", viewResultText(t, res))
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(viewResultText(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal preview_edit: %v", err)
	}
	removed := wireStringSet(payload["symbols_removed"])
	if !removed[baseSelectorOwnNode] {
		t.Fatalf("symbols_removed = %v, so this edit removed nothing and the caller pass never ran",
			payload["symbols_removed"])
	}

	callers := map[string]bool{}
	entries, _ := payload["broken_callers"].([]any)
	for _, entry := range entries {
		row, _ := entry.(map[string]any)
		id, _ := row["caller_id"].(string)
		callers[id] = true
	}
	if !callers[baseSelectorForeignNode] {
		t.Errorf("broken_callers = %v, want the cross-repo caller %s: a base selector narrows the reader to one repository, and the advertised cross-repo contract must not narrow with it",
			payload["broken_callers"], baseSelectorForeignNode)
	}

	rider := resultFreshness(t, res)
	named := wireStringSet(rider["base_scoped"])
	for _, want := range []graphview.CapabilityID{graphview.CapSyntaxGraph, graphview.CapResolutionLocal} {
		if !named[string(want)] {
			t.Errorf("base_scoped = %v, want it to name %s: this routed request read the indexed corpus, not a checkout of its own",
				rider["base_scoped"], want)
		}
	}
}

// TestBaseSelectorSimulationDiffsWhatTheLayerMarked is the reader half of the
// same coherence rule, made observable.
//
// The overlay layer's removal markers are read through overlayBaseReaderFor
// (overlay_view.go), which falls back to the whole corpus for a labelled base
// selector's narrowed reader. Taking the diff against the narrowed reader
// instead would make the two disagree wherever they can — on a file the
// narrowing filters out. The layer marks the sibling repository's symbol
// removed; a diff read through the narrowed reader sees no such file at all and
// silently reports nothing removed and no caller broken.
func TestBaseSelectorSimulationDiffsWhatTheLayerMarked(t *testing.T) {
	stack := newViewStack(t)

	encoded, err := json.Marshal(lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		baseSelectorForeignFile: {{
			Range:   lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 3, Character: 0}},
			NewText: "package other\n",
		}},
	}})
	if err != nil {
		t.Fatalf("marshal the workspace edit: %v", err)
	}

	res, err := stack.callHandler(t, stack.repoRoot, "preview_edit",
		baseSelectorArgs(stack.graphID, map[string]any{"workspace_edit": string(encoded)}),
		stack.srv.handlePreviewEdit)
	if err != nil {
		t.Fatalf("preview_edit under the base selector: %v", err)
	}
	if res.IsError {
		t.Fatalf("preview_edit under the base selector: %s", viewResultText(t, res))
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(viewResultText(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal preview_edit: %v", err)
	}
	removed := wireStringSet(payload["symbols_removed"])
	if !removed[baseSelectorForeignNode] {
		t.Errorf("symbols_removed = %v, want %s: the layer this diff was taken against marked it removed, and a diff that cannot see the file reports the edit as a no-op",
			payload["symbols_removed"], baseSelectorForeignNode)
	}
}

// wireStringSet reads a JSON array of strings off a decoded payload. A missing
// or null field is an empty set rather than a panic, so a revert that empties
// the field fails on the assertion that names it instead of on a type switch.
func wireStringSet(v any) map[string]bool {
	out := map[string]bool{}
	entries, _ := v.([]any)
	for _, entry := range entries {
		if name, ok := entry.(string); ok {
			out[name] = true
		}
	}
	return out
}
