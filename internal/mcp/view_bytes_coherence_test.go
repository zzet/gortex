package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Snapshot coherence between the two lanes a view answers content through.
//
// The text lane refuses a view whose top layer is a committed tree — the
// coordinator refuses to search a root no routed layer describes
// (indexer/checkout_text_search.go:82-88) and the reader declares the
// capability unavailable (graphview/materialize.go:846-881). The byte lane had
// no matching rule: a committed-tree view whose file surface could not open
// its tree fell through to the working-copy resolvers, and resolveFilePath —
// unlike resolveNodePath and resolveGraphPath — has no committed-tree guard of
// its own, so the answer was the canonical checkout's bytes under a rider that
// claimed the ref the caller asked for.
//
// These tests drive the production read_file handler. Two of them bind the
// view to the request themselves rather than letting the middleware select
// one: the shape they pin (a routed checkout whose route withdrew its
// working-tree layer between materializeRequestView's readiness check and
// MaterializeCheckout's own route read) exists only for the width of that
// race, and there is no seam to hold it open from outside.

const coherenceWorkingCopyMarker = "zephyr-working-copy-marker"

// coherenceWrite puts bytes at one path, creating the parent directory.
func coherenceWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// coherenceReadFile runs the production read_file handler with view already
// bound to the request.
func coherenceReadFile(t *testing.T, srv *Server, view *requestView, path string) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "read_file"
	req.Params.Arguments = map[string]any{"path": path}
	ctx := withRequestView(WithSessionID(context.Background(), viewTestSession), view)
	res, err := srv.handleReadFile(ctx, req)
	if err != nil {
		t.Fatalf("read_file through the bound view: %v", err)
	}
	return res
}

// coherenceRider is the rider a routed request carries: exact, and naming the
// checkout it was routed to.
func coherenceRider(checkoutID string) *graphview.ViewRider {
	actual := "worktree:" + checkoutID
	return &graphview.ViewRider{
		RequestedView: actual,
		ActualView:    actual,
		CheckoutID:    checkoutID,
		Exact:         true,
	}
}

// withdrawnWorkingTreeView is the shape the race produces, built the way
// production builds it rather than by hand.
//
// The coordinator's clearDirtySlot (indexer/checkout_coordinator.go) clears the
// route's working-tree slot with FlipCheckoutRouteSlot(GenerationID: 0), and
// MaterializeCheckout then leases the commit generation alone: it appends the
// dirty generation only when the route it re-reads still names one, so
// assemble's layer loop runs zero times and RepoViewID.Layers comes back EMPTY.
// That empty stack — not a LayerCommit ref, which nothing on the checkout path
// mints — is what a withdrawn working tree looks like, and the assertion below
// is what keeps this fixture honest about it.
func withdrawnWorkingTreeView(t *testing.T, stack *viewStack) *requestView {
	t.Helper()
	ctx := context.Background()
	catalog := stack.store.Catalog()
	route, found, err := catalog.GetCheckoutRoute(ctx, viewTestWorktree)
	if err != nil || !found {
		t.Fatalf("read the fixture route: %v (found=%v)", err, found)
	}
	if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         viewTestWorktree,
		Slot:               store_sqlite.RouteSlotDirty,
		GenerationID:       0,
		State:              store_sqlite.RoutePending,
		ExpectedRouteEpoch: route.RouteEpoch,
	}); err != nil {
		t.Fatalf("withdraw the working-tree slot the way clearDirtySlot does: %v", err)
	}

	materialized, err := stack.srv.Materializer().MaterializeCheckout(ctx, viewTestWorktree)
	if err != nil {
		t.Fatalf("materialize the checkout whose working-tree slot was withdrawn: %v", err)
	}
	t.Cleanup(materialized.Close)
	if len(materialized.ID.Layers) != 0 {
		t.Fatalf("a route with no working-tree slot materialized %d layers, want a bare commit stack: %v",
			len(materialized.ID.Layers), materialized.ID.Layers)
	}
	return &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktreeRoot,
		rider:        coherenceRider(viewTestWorktree),
	}
}

// committedLayerRefView is the other spelling of the same classification: a
// stack whose top LayerRef is an explicit LayerCommit. Nothing on the
// materialization path mints one today — dirtyLayerRef is the sole LayerRef
// constructor there and always sets LayerDirty — so this one view IS built by
// hand, and it is used only where the point is that the enum arm is covered
// rather than that a production shape is reproduced.
func committedLayerRefView(t *testing.T, stack *viewStack) *requestView {
	t.Helper()
	id, err := graphview.NewRepoViewID("repo", stack.graphID, 1, graphview.LayerRef{
		Kind:       graphview.LayerCommit,
		LayerID:    viewTestCommitLayerID,
		Generation: stack.commit,
	})
	if err != nil {
		t.Fatalf("build the commit-layer view identity: %v", err)
	}
	return &requestView{
		kind: requestViewKindWorktree,
		// A composed Reader is what makes this a leased stack rather than an
		// identity-only value; viewStackReadsCommittedTree declines to classify
		// the latter, so a fixture without one would exercise nothing.
		materialized: &graphview.RepoView{ID: id, Reader: stack.store.AtGeneration(0)},
		viewRoot:     stack.worktreeRoot,
		rider:        coherenceRider(viewTestWorktree),
	}
}

// workingTreeLayerView is the healthy routed checkout beside it: the same
// stack with the working-tree layer the route names on top.
func workingTreeLayerView(t *testing.T, stack *viewStack) *requestView {
	t.Helper()
	id, err := graphview.NewRepoViewID("repo", stack.graphID, 1,
		graphview.LayerRef{Kind: graphview.LayerCommit, LayerID: viewTestCommitLayerID, Generation: stack.commit},
		graphview.LayerRef{Kind: graphview.LayerDirty, LayerID: viewTestDirtyLayerID, Generation: stack.dirty})
	if err != nil {
		t.Fatalf("build the working-tree view identity: %v", err)
	}
	return &requestView{
		kind:         requestViewKindWorktree,
		materialized: &graphview.RepoView{ID: id, Reader: stack.store.AtGeneration(0)},
		viewRoot:     stack.worktreeRoot,
		rider:        coherenceRider(viewTestWorktree),
	}
}

// TestCommittedViewWithNoTreeRefusesRatherThanReadingAWorkingCopy is the
// defining claim for a committed-tree view that cannot open its tree: it says
// so, rather than answering with bytes from a checkout that holds another
// state of the world.
//
// A ref view is built with a repository directory and a tree oid resolved off
// the catalog (view_ref.go:146-159); both can come back empty — repoDirForPrefix
// returns "" when the owning checkout row, the multi-indexer and the lone
// indexer all fail to name a directory (view_ref.go:202-218). The surface was
// then reported as "no view file surface at all", and read_file resolved the
// path against the canonical checkout instead.
func TestCommittedViewWithNoTreeRefusesRatherThanReadingAWorkingCopy(t *testing.T) {
	stack := newViewStack(t)
	coherenceWrite(t, filepath.Join(stack.repoRoot, "edit.go"),
		"package repo\n\n// "+coherenceWorkingCopyMarker+"\nfunc Old() {}\n")

	view := &requestView{
		kind: requestViewKindRef,
		files: &refViewFiles{
			fingerprint: "fp-committed-without-a-tree",
			repoPrefix:  "repo",
		},
		rider: &graphview.ViewRider{
			RequestedView: "git_ref:refs/heads/feature",
			ActualView:    "fp-committed-without-a-tree",
			Exact:         true,
		},
	}

	res := coherenceReadFile(t, stack.srv, view, "repo/edit.go")
	text := viewResultText(t, res)
	if strings.Contains(text, coherenceWorkingCopyMarker) {
		t.Fatalf("a view of a committed tree answered with the working copy's bytes:\n%s", text)
	}
	if !res.IsError {
		t.Fatalf("a committed-tree view with no tree served a successful read:\n%s", text)
	}
	assertToolError(t, res, string(graphview.CodeCapabilityUnavailable))
	if !strings.Contains(text, string(graphview.CapSourceSnapshot)) {
		t.Errorf("the refusal does not name the capability it cannot serve:\n%s", text)
	}
}

// TestWithdrawnWorkingTreeLayerRefusesItsRootsBytes is the same rule for the
// shape a working copy hides: the route withdrew the working-tree slot, so the
// view reads its commit generation alone while the request still carries the
// checkout's root. The text lane refuses this view; the byte lane must not
// answer it off that root.
//
// The view here is materialized by the production materializer over a route
// whose dirty slot was cleared, so what is pinned is the stack MaterializeCheckout
// really composes for that route — not a hand-assembled identity.
func TestWithdrawnWorkingTreeLayerRefusesItsRootsBytes(t *testing.T) {
	stack := newViewStack(t)
	coherenceWrite(t, filepath.Join(stack.worktreeRoot, "edit.go"),
		"package repo\n\n// "+coherenceWorkingCopyMarker+"\nfunc Old() {}\n")

	view := withdrawnWorkingTreeView(t, stack)

	// The read comes first, deliberately: what this test is about is the bytes
	// the production handler answers with, not how the predicate classifies the
	// view. A classification assertion in front of it would fail before the
	// defect it exists to catch was ever exercised.
	res := coherenceReadFile(t, stack.srv, view, "repo/edit.go")
	text := viewResultText(t, res)
	if strings.Contains(text, coherenceWorkingCopyMarker) {
		t.Fatalf("a checkout routed to its commit generation alone answered with its root's bytes:\n%s", text)
	}
	if !res.IsError {
		t.Fatalf("the withdrawn-working-tree view served a successful read:\n%s", text)
	}
	assertToolError(t, res, string(graphview.CodeCapabilityUnavailable))
	if !strings.Contains(text, string(graphview.CapSourceSnapshot)) {
		t.Errorf("the refusal does not name the capability it cannot serve:\n%s", text)
	}
	if !viewReadsCommittedTree(view) {
		t.Errorf("the view refused the read but is not classified as reading a committed tree")
	}
}

// TestRoutedWorkingTreeViewStillReadsItsOwnRoot is the keep: the routed
// checkout every request actually gets — a working-tree layer on top — reads
// the bytes on its own root exactly as before. Without this the fix above
// could be "refuse everything routed" and still look correct.
func TestRoutedWorkingTreeViewStillReadsItsOwnRoot(t *testing.T) {
	stack := newViewStack(t)
	coherenceWrite(t, filepath.Join(stack.worktreeRoot, "edit.go"),
		"package repo\n\n// "+coherenceWorkingCopyMarker+"\nfunc Old() {}\n")

	view := workingTreeLayerView(t, stack)
	if viewReadsCommittedTree(view) {
		t.Fatalf("a routed checkout with a working-tree layer was classified as a committed tree")
	}

	res := coherenceReadFile(t, stack.srv, view, "repo/edit.go")
	text := viewResultText(t, res)
	if res.IsError {
		t.Fatalf("the routed checkout's own working copy was refused:\n%s", text)
	}
	if !strings.Contains(text, coherenceWorkingCopyMarker) {
		t.Fatalf("the routed checkout did not answer with its own root's bytes:\n%s", text)
	}
}

// TestRoutedWorktreeSelectorStillReadsTheSelectedCheckout drives the same keep
// through the whole middleware, so the classification is exercised on a view
// the server selected rather than one the test bound.
func TestRoutedWorktreeSelectorStillReadsTheSelectedCheckout(t *testing.T) {
	stack := newViewStack(t)
	coherenceWrite(t, filepath.Join(stack.repoRoot, "keep.go"),
		"package repo\n\n// primary-checkout-bytes\nfunc Keeper() {}\n")
	coherenceWrite(t, filepath.Join(stack.worktreeRoot, "keep.go"),
		"package repo\n\n// selected-worktree-bytes\nfunc Keeper() {}\n")

	res, err := stack.callWithView(t, stack.repoRoot, "read_file", map[string]any{
		"path": "repo/keep.go",
		"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree},
	}, func(ctx context.Context) (*mcplib.CallToolResult, error) {
		req := mcplib.CallToolRequest{}
		req.Params.Name = "read_file"
		req.Params.Arguments = map[string]any{"path": "repo/keep.go"}
		return stack.srv.handleReadFile(ctx, req)
	})
	if err != nil {
		t.Fatalf("read_file through the selected worktree: %v", err)
	}
	text := viewResultText(t, res)
	if res.IsError {
		t.Fatalf("the selected worktree was refused: %s", text)
	}
	if !strings.Contains(text, "selected-worktree-bytes") || strings.Contains(text, "primary-checkout-bytes") {
		t.Fatalf("the selected worktree's bytes were not what answered:\n%s", text)
	}
}

// TestTextLaneKeepsUsingTheLiveRootForARoutedCheckout pins the other half of
// the shared predicate: a routed checkout with a working-tree layer still
// takes the live-root search lane. The refusal it gets here is the searcher's
// ("nothing indexes its working copy" — this fixture wires no coordinator),
// which is only reachable past the committed-tree arm.
func TestTextLaneKeepsUsingTheLiveRootForARoutedCheckout(t *testing.T) {
	stack := newViewStack(t)
	stack.declareProducer(t, stack.dirty, graphview.CapSearchText, store_sqlite.ProducerStateComplete)

	res, err := stack.callWithView(t, stack.worktreeRoot, "search_text",
		map[string]any{"query": "func Keeper"}, func(ctx context.Context) (*mcplib.CallToolResult, error) {
			return stack.srv.handleSearchText(ctx, newSearchTextRequest("func Keeper"))
		})
	if err != nil {
		t.Fatalf("search through the routed view: %v", err)
	}
	text := viewResultText(t, res)
	if !strings.Contains(text, "nothing indexes its working copy") {
		t.Fatalf("a routed checkout with a working-tree layer left the live-root lane:\n%s", text)
	}
}

// TestTextLaneRefusesAWithdrawnWorkingTreeLayer pins the text lane's own use
// of the shared predicate, and it is a backstop test rather than a production
// one — deliberately, and the fixture says so.
//
// On the real shape the capability evaluation refuses FIRST: the commit
// generation on top declares nothing for search.text
// (indexer/builder_generation.go textSearchProducer) and the reader reads that
// silence as StateUnavailable (graphview/materialize.go completeness), which
// TestCommittedTopViewRefusesTextSearchAsACapability already pins. So the
// completeness is overwritten here by hand to reach the arm below it. What
// that arm states is that the two lanes classify a view by one predicate: a
// committed-tree view that somehow declared text search would still not be
// searched off a root it does not read.
func TestTextLaneRefusesAWithdrawnWorkingTreeLayer(t *testing.T) {
	stack := newViewStack(t)
	view := withdrawnWorkingTreeView(t, stack)
	if got := view.materialized.Completeness.State(graphview.CapSearchText); got != graphview.StateUnavailable {
		t.Fatalf("the withdrawn route reports %s = %q; the evaluation, not this arm, is what refuses it",
			graphview.CapSearchText, got)
	}
	view.materialized.Completeness = graphview.Completeness{
		graphview.CapSearchText: graphview.StateComplete,
	}

	matches, refusal := stack.srv.searchTextInView(context.Background(), view, "func Old", false, 10)
	if refusal == nil {
		t.Fatalf("a view whose top layer is a committed tree searched its root: %v", matches)
	}
	text := viewResultText(t, refusal)
	if !strings.Contains(text, "not the snapshot it reads") {
		t.Errorf("the refusal does not say what about this view stops the search:\n%s", text)
	}
	if !strings.Contains(text, string(graphview.CapSearchText)) {
		t.Errorf("the refusal does not name the capability:\n%s", text)
	}
}

// TestRoutedAnswerIsLabelledWhenTheRouteMovesUnderIt is the working copy's
// half of the base corpus's pin: bytes read off a live root are not frozen for
// the length of a request, so when the route the view pinned advances while
// the request reads through it, the answer says so instead of looking like the
// snapshot the rider names.
// The route it moves is the one production installs: CheckoutCoordinator's
// installStack takes the UpsertCheckoutRoute arm for a not-yet-routed checkout
// and that statement writes route_epoch = excluded.route_epoch with no epoch
// set, so a checkout's FIRST published route — RouteActive, both slots
// published, RouteReady, and served — pins epoch 0. The flip under this
// request is therefore 0 -> 1, which is both the most common instance of "the
// route moved under this answer" and the one a `pinned <= 0` guard on the
// pinned epoch would report as unwitnessed.
func TestRoutedAnswerIsLabelledWhenTheRouteMovesUnderIt(t *testing.T) {
	stack := newViewStack(t)
	coherenceWrite(t, filepath.Join(stack.worktreeRoot, "edit.go"),
		"package repo\n\n// "+coherenceWorkingCopyMarker+"\nfunc Old() {}\n")
	ctx := context.Background()
	catalog := stack.store.Catalog()

	materialized, err := stack.srv.Materializer().MaterializeCheckout(ctx, viewTestWorktree)
	if err != nil {
		t.Fatalf("materialize the routed checkout: %v", err)
	}
	defer materialized.Close()
	if materialized.CheckoutRouteEpoch != 0 {
		t.Fatalf("the view pinned route epoch %d; a freshly installed route is at 0",
			materialized.CheckoutRouteEpoch)
	}
	view := &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktreeRoot,
		rider:        coherenceRider(viewTestWorktree),
	}

	// The control: nothing moved, so nothing is claimed.
	if res := coherenceReadFile(t, stack.srv, view, "repo/edit.go"); res.IsError {
		t.Fatalf("the routed read failed before the route moved: %s", viewResultText(t, res))
	}
	if degraded, _ := view.annotations(); len(degraded) != 0 {
		t.Fatalf("a coherent routed answer was labelled: %v", degraded)
	}

	// Move the route the way production moves it, through the epoch's own
	// compare-and-set: 0 -> 1, the checkout's first republication.
	if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         viewTestWorktree,
		Slot:               store_sqlite.RouteSlotDirty,
		GenerationID:       stack.dirty,
		State:              store_sqlite.RouteActive,
		ExpectedRouteEpoch: 0,
	}); err != nil {
		t.Fatalf("move the route under the request: %v", err)
	}

	if res := coherenceReadFile(t, stack.srv, view, "repo/edit.go"); res.IsError {
		t.Fatalf("the routed read failed after the route moved: %s", viewResultText(t, res))
	}
	degraded, _ := view.annotations()
	if !coherenceHasStatus(degraded, graphview.CapSourceSnapshot, graphview.StateIncomplete) {
		t.Fatalf("a routed answer read across a route move was not labelled: %v", degraded)
	}
}

// TestRouteDriftMakesNoClaimWithoutAWitness pins the third arm, and what
// counts as "unwitnessed" is the point of it.
//
// It is NOT a pinned epoch of zero — production installs every first route at
// zero (see TestRoutedAnswerIsLabelledWhenTheRouteMovesUnderIt), so reading
// that as absence blinds the check for a checkout's whole first epoch. What
// leaves nothing to compare against is the absence of a route to compare
// with: no route row, no checkout id, no handle to read the catalog through.
// Absence of evidence is not evidence of a change, the same rule the base
// corpus's unwitnessed arm follows.
func TestRouteDriftMakesNoClaimWithoutAWitness(t *testing.T) {
	stack := newViewStack(t)
	ctx := context.Background()

	materialized, err := stack.srv.Materializer().MaterializeCheckout(ctx, viewTestWorktree)
	if err != nil {
		t.Fatalf("materialize the routed checkout: %v", err)
	}
	defer materialized.Close()
	view := &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktreeRoot,
		rider:        coherenceRider(viewTestWorktree),
	}

	// The coherent control first: the route is still exactly the one the view
	// pinned, at the epoch production installs it with.
	if materialized.CheckoutRouteEpoch != 0 {
		t.Fatalf("the fixture's route carries epoch %d; installStack writes 0",
			materialized.CheckoutRouteEpoch)
	}
	if noteWorktreeRouteDrift(ctx, view, graphview.CapSourceSnapshot) {
		t.Fatal("a route that has not moved was reported as a route that moved")
	}

	// Now take the witness away: no route row at all is the shape that says
	// nothing can be compared, and it must still make no claim.
	if err := stack.store.Catalog().DeleteCheckoutRoute(ctx, viewTestWorktree); err != nil {
		t.Fatalf("delete the route row: %v", err)
	}
	if noteWorktreeRouteDrift(ctx, view, graphview.CapSourceSnapshot) {
		t.Fatal("a checkout with no route row was reported as a route that moved")
	}

	// And a view that names no checkout has nothing to look one up by.
	anonymous := &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktreeRoot,
		rider:        &graphview.ViewRider{RequestedView: "worktree", ActualView: "worktree", Exact: true},
	}
	if noteWorktreeRouteDrift(ctx, anonymous, graphview.CapSourceSnapshot) {
		t.Fatal("a view naming no checkout was reported as a route that moved")
	}
	if degraded, _ := view.annotations(); len(degraded) != 0 {
		t.Fatalf("an unwitnessed route left an annotation: %v", degraded)
	}
	if degraded, _ := anonymous.annotations(); len(degraded) != 0 {
		t.Fatalf("a view naming no checkout left an annotation: %v", degraded)
	}
}

// TestRouteDriftIsSafeUnderConcurrentReaders runs the check the way an
// abandoned handler and its replacement reach it — several goroutines reading
// through one view at once. It exists for the race detector; the assertion is
// only that the observation is recorded at all.
func TestRouteDriftIsSafeUnderConcurrentReaders(t *testing.T) {
	stack := newViewStack(t)
	ctx := context.Background()
	catalog := stack.store.Catalog()
	if err := catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         viewTestWorktree,
		GraphID:            stack.graphID,
		CommitGenerationID: stack.commit,
		DirtyGenerationID:  stack.dirty,
		RouteEpoch:         1,
		State:              store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("install the route at a witnessed epoch: %v", err)
	}
	materialized, err := stack.srv.Materializer().MaterializeCheckout(ctx, viewTestWorktree)
	if err != nil {
		t.Fatalf("materialize the routed checkout: %v", err)
	}
	defer materialized.Close()
	if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         viewTestWorktree,
		Slot:               store_sqlite.RouteSlotDirty,
		GenerationID:       stack.dirty,
		State:              store_sqlite.RouteActive,
		ExpectedRouteEpoch: 1,
	}); err != nil {
		t.Fatalf("move the route: %v", err)
	}

	view := &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktreeRoot,
		rider:        coherenceRider(viewTestWorktree),
	}
	bound := withRequestView(context.Background(), view)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if files := refViewFilesFor(bound); files != nil {
				t.Error("a routed working copy was handed a committed-tree file surface")
			}
		}()
	}
	wg.Wait()

	degraded, _ := view.annotations()
	if !coherenceHasStatus(degraded, graphview.CapSourceSnapshot, graphview.StateIncomplete) {
		t.Fatalf("concurrent readers recorded no route move: %v", degraded)
	}
}

// TestCommittedTreeClassificationCoversEveryViewShape states the predicate
// itself, so the rule the two lanes share is readable in one place.
//
// The withdrawn-working-tree row is the materializer's own output — a route
// whose dirty slot was cleared, materialized — because the shape production
// builds for it is an EMPTY layer stack and not a LayerCommit ref. The
// LayerCommit row beside it is hand-built on purpose: nothing on the
// materialization path mints that kind today, and the predicate covers it so
// that the day something does, the classification does not silently invert.
func TestCommittedTreeClassificationCoversEveryViewShape(t *testing.T) {
	stack := newViewStack(t)
	rooted := workingTreeLayerView(t, stack)
	commitLayer := committedLayerRefView(t, stack)
	withdrawn := withdrawnWorkingTreeView(t, stack)
	for _, tc := range []struct {
		name string
		view *requestView
		want bool
	}{
		{"no view at all", nil, false},
		{"the base corpus", &requestView{kind: requestViewKindBase}, false},
		{"a ref view with a tree", &requestView{files: &refViewFiles{repoDir: stack.repoRoot, treeOID: "tree"}}, true},
		{"a ref view whose tree cannot be opened", &requestView{files: &refViewFiles{}}, true},
		{"a routed checkout with a working-tree layer", rooted, false},
		{"a rooted view with no materialized stack", &requestView{
			kind: requestViewKindWorktree, viewRoot: stack.worktreeRoot,
		}, false},
		// The shape several fixtures in this package carry: a RepoView that
		// names an identity but leased and composed nothing. Its empty Layers
		// are absence of data, not evidence that a working tree was withdrawn,
		// and reading them as the latter refuses path resolution for every
		// routed request built that way (overlay_view_request_test.go's
		// routedViewCtx is one).
		{"a rooted view carrying an identity-only RepoView", &requestView{
			kind:     requestViewKindWorktree,
			viewRoot: stack.worktreeRoot,
			materialized: &graphview.RepoView{ID: graphview.RepoViewID{
				RepoPrefix: "repo", BaseGraphID: stack.graphID, BaseGeneration: 1,
			}},
		}, false},
		{"a routed checkout whose working-tree slot was withdrawn", withdrawn, true},
		{"a stack whose top LayerRef is a commit layer", commitLayer, true},
	} {
		if got := viewReadsCommittedTree(tc.view); got != tc.want {
			t.Errorf("viewReadsCommittedTree(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if len(withdrawn.materialized.ID.Layers) != 0 {
		t.Errorf("the withdrawn route's stack carries layers: %v", withdrawn.materialized.ID.Layers)
	}
	if got := commitLayer.materialized.ID.Layers[0].Kind; got != graphview.LayerCommit {
		t.Errorf("the hand-built commit-layer row is a %q layer", got)
	}
}

// TestRacedTextSearchOverALiveRootIsLabelled is the text lane's half of the
// route pin, end to end: a real worktree, the coordinator production starts,
// and the searcher it owns. The search keeps answering off the live root —
// that is the behaviour this item keeps — and when the route the view pinned
// advances while the view is still open, the answer rides back labelled.
func TestRacedTextSearchOverALiveRootIsLabelled(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	const marker = "zephyr-live-root-marker"

	previous := stack.dirtyGeneration(t)
	refWriteFiles(t, stack.worktree, map[string]string{
		"keep.go": "package repo\n\nfunc Keeper() {\n\t// " + marker + "\n}\n",
	})
	stack.awaitDirtyGenerationAfter(t, previous)

	ctx := context.Background()
	materialized, err := stack.srv.Materializer().MaterializeCheckout(ctx, stack.checkoutID)
	if err != nil {
		t.Fatalf("materialize the routed worktree: %v", err)
	}
	defer materialized.Close()
	if materialized.CheckoutRouteEpoch <= 0 {
		t.Fatalf("the routed worktree pinned epoch %d; a route the daemon published has a witness",
			materialized.CheckoutRouteEpoch)
	}
	view := &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktree,
		rider:        coherenceRider(stack.checkoutID),
	}

	matches, refusal := stack.srv.searchTextInView(ctx, view, marker, false, 10)
	if refusal != nil {
		t.Fatalf("the routed checkout's live root was not searched: %s", viewResultText(t, refusal))
	}
	if len(matches) == 0 {
		t.Fatal("the live root holds the marker and the search found nothing")
	}
	if degraded, _ := view.annotations(); len(degraded) != 0 {
		t.Fatalf("a coherent search over the live root was labelled: %v", degraded)
	}

	// Move the route under the open view, the way a working-copy write does:
	// the cycle samples it and publishes a new working-tree generation.
	previous = stack.dirtyGeneration(t)
	refWriteFiles(t, stack.worktree, map[string]string{
		"raced.go": "package repo\n\nfunc Raced() {}\n",
	})
	stack.awaitDirtyGenerationAfter(t, previous)

	matches, refusal = stack.srv.searchTextInView(ctx, view, marker, false, 10)
	if refusal != nil {
		t.Fatalf("the search stopped answering after the route moved: %s", viewResultText(t, refusal))
	}
	if len(matches) == 0 {
		t.Fatal("the search answered nothing after the route moved")
	}
	degraded, _ := view.annotations()
	if !coherenceHasStatus(degraded, graphview.CapSearchText, graphview.StateIncomplete) {
		t.Fatalf("an answer read across a route move was not labelled: %v", degraded)
	}
}

func coherenceHasStatus(
	statuses []graphview.CapabilityStatus,
	capability graphview.CapabilityID,
	state graphview.CapabilityState,
) bool {
	for _, status := range statuses {
		if status.Capability == capability && status.State == state {
			return true
		}
	}
	return false
}

// W5.7b. The route-drift signal must demote the answer's exactness claim, not
// only annotate the capability.
//
// noteWorktreeRouteDrift names the base corpus's pin as its precedent
// (view_paths.go), and that precedent does two things: markBaseCorpusChange
// (view_request.go) annotates AND calls view.rider.MarkFallback, which clears
// Exact and sets fallback_reason. Without the second half a caller reading a
// routed working copy is told, on the same rider, that it got exactly the view
// it named — while the working copy under that view was re-sampled and
// re-published mid-answer. That is the one claim that is no longer true, and
// it is the claim require_exact / an exactness check on the client side reads.
func TestRouteDriftWithdrawsTheExactnessClaim(t *testing.T) {
	stack := newViewStack(t)
	coherenceWrite(t, filepath.Join(stack.worktreeRoot, "edit.go"),
		"package repo\n\n// "+coherenceWorkingCopyMarker+"\nfunc Old() {}\n")
	ctx := context.Background()
	catalog := stack.store.Catalog()

	materialized, err := stack.srv.Materializer().MaterializeCheckout(ctx, viewTestWorktree)
	if err != nil {
		t.Fatalf("materialize the routed checkout: %v", err)
	}
	defer materialized.Close()
	view := &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktreeRoot,
		rider:        coherenceRider(viewTestWorktree),
	}
	named := view.rider.ActualView

	// The control: a coherent answer keeps the exactness it was given.
	if res := coherenceReadFile(t, stack.srv, view, "repo/edit.go"); res.IsError {
		t.Fatalf("the routed read failed before the route moved: %s", viewResultText(t, res))
	}
	if !view.rider.Exact || view.rider.FallbackReason != "" {
		t.Fatalf("a coherent routed answer was demoted: exact=%v reason=%q",
			view.rider.Exact, view.rider.FallbackReason)
	}

	if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         viewTestWorktree,
		Slot:               store_sqlite.RouteSlotDirty,
		GenerationID:       stack.dirty,
		State:              store_sqlite.RouteActive,
		ExpectedRouteEpoch: materialized.CheckoutRouteEpoch,
	}); err != nil {
		t.Fatalf("move the route under the request: %v", err)
	}

	if res := coherenceReadFile(t, stack.srv, view, "repo/edit.go"); res.IsError {
		t.Fatalf("the routed read failed after the route moved: %s", viewResultText(t, res))
	}
	if view.rider.Exact {
		t.Fatalf("an answer read across a route move still claims exact:true (reason=%q)",
			view.rider.FallbackReason)
	}
	if view.rider.FallbackReason != routeMovedFallbackReason {
		t.Errorf("fallback_reason = %q, want %q", view.rider.FallbackReason, routeMovedFallbackReason)
	}
	if view.rider.ActualView != named {
		t.Errorf("the demotion renamed the view: actual_view = %q, want %q", view.rider.ActualView, named)
	}
	// The annotation half is not traded away for the exactness half.
	degraded, _ := view.annotations()
	if !coherenceHasStatus(degraded, graphview.CapSourceSnapshot, graphview.StateIncomplete) {
		t.Errorf("the capability annotation was lost: %v", degraded)
	}
}

// A rider that is already inexact keeps the reason it already carries: the
// first substitution is the one the caller has to act on, and a route that
// then moved under the substitute must not overwrite it.
func TestRouteDriftKeepsAnEarlierFallbackReason(t *testing.T) {
	stack := newViewStack(t)
	ctx := context.Background()
	catalog := stack.store.Catalog()

	materialized, err := stack.srv.Materializer().MaterializeCheckout(ctx, viewTestWorktree)
	if err != nil {
		t.Fatalf("materialize the routed checkout: %v", err)
	}
	defer materialized.Close()
	rider := coherenceRider(viewTestWorktree)
	if err := rider.MarkFallback(rider.ActualView, string(graphview.CodeViewBuilding)); err != nil {
		t.Fatalf("pre-demote the rider: %v", err)
	}
	view := &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktreeRoot,
		rider:        rider,
	}

	if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         viewTestWorktree,
		Slot:               store_sqlite.RouteSlotDirty,
		GenerationID:       stack.dirty,
		State:              store_sqlite.RouteActive,
		ExpectedRouteEpoch: materialized.CheckoutRouteEpoch,
	}); err != nil {
		t.Fatalf("move the route under the request: %v", err)
	}
	if !noteWorktreeRouteDrift(ctx, view, graphview.CapSourceSnapshot) {
		t.Fatal("the route moved and the drift check said it had not")
	}
	if view.rider.Exact {
		t.Fatal("an already-inexact rider was re-marked exact")
	}
	if got := view.rider.FallbackReason; got != string(graphview.CodeViewBuilding) {
		t.Errorf("the earlier fallback reason was overwritten: %q", got)
	}
}

// The same statement through the production entrypoint: a real tools/call
// frame, the middleware's own view selection, the real read_file handler, and
// the rider the middleware renders at the end of it (overlay.go attachViewRider
// -> viewRiderFields). Without the demotion this response carries exact:true
// and no fallback_reason at all.
func TestRouteDriftWithdrawsExactnessThroughTheMiddleware(t *testing.T) {
	stack := newViewStack(t)
	coherenceWrite(t, filepath.Join(stack.worktreeRoot, "keep.go"),
		"package repo\n\n// selected-worktree-bytes\nfunc Keeper() {}\n")
	catalog := stack.store.Catalog()

	read := func(t *testing.T, moveTheRoute bool) *mcplib.CallToolResult {
		t.Helper()
		args := map[string]any{
			"path": "repo/keep.go",
			"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree},
		}
		res, err := stack.callWithView(t, stack.repoRoot, "read_file", args,
			func(ctx context.Context) (*mcplib.CallToolResult, error) {
				view := requestViewFromContext(ctx)
				if view == nil || view.materialized == nil {
					t.Fatal("the middleware bound no materialized view to the request")
				}
				if !view.rider.Exact {
					t.Fatalf("the middleware's own selection was not exact: %q", view.rider.FallbackReason)
				}
				if moveTheRoute {
					if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
						CheckoutID:         viewTestWorktree,
						Slot:               store_sqlite.RouteSlotDirty,
						GenerationID:       stack.dirty,
						State:              store_sqlite.RouteActive,
						ExpectedRouteEpoch: view.materialized.CheckoutRouteEpoch,
					}); err != nil {
						t.Fatalf("move the route under the request: %v", err)
					}
				}
				req := mcplib.CallToolRequest{}
				req.Params.Name = "read_file"
				req.Params.Arguments = map[string]any{"path": "repo/keep.go"}
				return stack.srv.handleReadFile(ctx, req)
			})
		if err != nil {
			t.Fatalf("read_file through the selected worktree: %v", err)
		}
		if res.IsError {
			t.Fatalf("the selected worktree was refused: %s", viewResultText(t, res))
		}
		return res
	}

	t.Run("a coherent answer still claims exactness", func(t *testing.T) {
		rider := metaFreshness(t, read(t, false))
		if rider == nil {
			t.Fatal("the response carries no view rider")
		}
		if rider["exact"] != true {
			t.Errorf("a coherent routed answer was demoted: %v", rider)
		}
		if _, present := rider["fallback_reason"]; present {
			t.Errorf("a coherent routed answer carries a fallback reason: %v", rider)
		}
	})

	t.Run("an answer read across a route move does not", func(t *testing.T) {
		rider := metaFreshness(t, read(t, true))
		if rider == nil {
			t.Fatal("the response carries no view rider")
		}
		if rider["exact"] != false {
			t.Errorf("an answer read across a route move still claims exact: %v", rider)
		}
		if rider["fallback_reason"] != routeMovedFallbackReason {
			t.Errorf("fallback_reason = %v, want %q", rider["fallback_reason"], routeMovedFallbackReason)
		}
		if _, present := rider["degraded_capabilities"]; !present {
			t.Errorf("the capability annotation did not ride back with the demotion: %v", rider)
		}
	})
}

// The demotion is a property of the answer, not of one capability, so it must
// happen above the per-capability dedupe: a request whose text lane already
// annotated search.text and whose byte lane then finds the same drift must
// still lose its exactness claim. Ordering the two the other way round makes
// the demotion depend on which lane noticed the move first.
func TestRouteDriftDemotesPastTheAnnotationDedupe(t *testing.T) {
	stack := newViewStack(t)
	ctx := context.Background()
	catalog := stack.store.Catalog()

	materialized, err := stack.srv.Materializer().MaterializeCheckout(ctx, viewTestWorktree)
	if err != nil {
		t.Fatalf("materialize the routed checkout: %v", err)
	}
	defer materialized.Close()
	view := &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktreeRoot,
		rider:        coherenceRider(viewTestWorktree),
	}
	// What the other lane left behind before this one ran.
	view.noteDegraded([]graphview.CapabilityStatus{
		{Capability: graphview.CapSourceSnapshot, State: graphview.StateIncomplete},
	})

	if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         viewTestWorktree,
		Slot:               store_sqlite.RouteSlotDirty,
		GenerationID:       stack.dirty,
		State:              store_sqlite.RouteActive,
		ExpectedRouteEpoch: materialized.CheckoutRouteEpoch,
	}); err != nil {
		t.Fatalf("move the route under the request: %v", err)
	}
	if !noteWorktreeRouteDrift(ctx, view, graphview.CapSourceSnapshot) {
		t.Fatal("the route moved and the drift check said it had not")
	}
	if view.rider.Exact {
		t.Fatal("the exactness claim survived because the annotation was already on the rider")
	}
	if view.rider.FallbackReason != routeMovedFallbackReason {
		t.Errorf("fallback_reason = %q, want %q", view.rider.FallbackReason, routeMovedFallbackReason)
	}
}

// --------------------------------------------- W5.7c: require_exact ---

// W5.7c. require_exact refuses ANY non-exact answer — including one selection
// served exactly that stopped being exact while the handler assembled it.
//
// The middleware's require_exact gate runs BEFORE the handler, so it sees only
// the substitutions selection made. Route drift is discovered later, by the
// lanes that read (view_files.go refViewFilesFor, view_search_text.go
// searchTextInView), and W5.7b made it demote the rider — which meant a
// require_exact caller was handed exact:false with fallback_reason
// "route_moved" on a response it had explicitly asked never to receive. A
// fallback the caller must notice and a fallback the caller refused are not
// the same outcome, and require_exact is the knob that says which one this is.
func TestRequireExactRefusesAnAnswerReadAcrossARouteMove(t *testing.T) {
	stack := newViewStack(t)
	coherenceWrite(t, filepath.Join(stack.worktreeRoot, "keep.go"),
		"package repo\n\n// selected-worktree-bytes\nfunc Keeper() {}\n")
	catalog := stack.store.Catalog()

	read := func(t *testing.T, requireExact, moveTheRoute bool) *mcplib.CallToolResult {
		t.Helper()
		args := map[string]any{
			"path": "repo/keep.go",
			"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree},
		}
		if requireExact {
			args[requireExactArgName] = true
		}
		res, err := stack.callWithView(t, stack.repoRoot, "read_file", args,
			func(ctx context.Context) (*mcplib.CallToolResult, error) {
				view := requestViewFromContext(ctx)
				if view == nil || view.materialized == nil {
					t.Error("the middleware bound no materialized view to the request")
					return mcplib.NewToolResultText(`{}`), nil
				}
				if !view.rider.Exact {
					t.Errorf("the middleware's own selection was not exact: %q", view.rider.FallbackReason)
					return mcplib.NewToolResultText(`{}`), nil
				}
				if moveTheRoute {
					if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
						CheckoutID:         viewTestWorktree,
						Slot:               store_sqlite.RouteSlotDirty,
						GenerationID:       stack.dirty,
						State:              store_sqlite.RouteActive,
						ExpectedRouteEpoch: view.materialized.CheckoutRouteEpoch,
					}); err != nil {
						t.Errorf("move the route under the request: %v", err)
						return mcplib.NewToolResultText(`{}`), nil
					}
				}
				req := mcplib.CallToolRequest{}
				req.Params.Name = "read_file"
				req.Params.Arguments = map[string]any{"path": "repo/keep.go"}
				return stack.srv.handleReadFile(ctx, req)
			})
		if err != nil {
			t.Fatalf("read_file through the selected worktree: %v", err)
		}
		return res
	}

	t.Run("require_exact answers a coherent read", func(t *testing.T) {
		res := read(t, true, false)
		if res.IsError {
			t.Fatalf("require_exact refused an answer that stayed exact: %s", viewResultText(t, res))
		}
		if rider := metaFreshness(t, res); rider == nil || rider["exact"] != true {
			t.Errorf("a coherent routed answer was demoted: %v", rider)
		}
	})

	t.Run("without require_exact the same drift still answers", func(t *testing.T) {
		res := read(t, false, true)
		if res.IsError {
			t.Fatalf("a drifted read without require_exact must still answer: %s", viewResultText(t, res))
		}
		rider := metaFreshness(t, res)
		if rider == nil || rider["exact"] != false {
			t.Errorf("a drifted answer still claims exactness: %v", rider)
		}
		if rider["fallback_reason"] != routeMovedFallbackReason {
			t.Errorf("fallback_reason = %v, want %q", rider["fallback_reason"], routeMovedFallbackReason)
		}
	})

	t.Run("require_exact refuses the drifted read", func(t *testing.T) {
		res := read(t, true, true)
		if !res.IsError {
			t.Fatalf("require_exact answered a read taken across a route move: %s", viewResultText(t, res))
		}
		text := viewResultText(t, res)
		for _, want := range []string{
			graphview.CodeViewBuilding,
			routeMovedFallbackReason,
			"require_exact",
			"no fallback was served",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the refusal does not name %q: %s", want, text)
			}
		}
	})
}
