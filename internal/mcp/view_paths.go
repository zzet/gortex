package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Resolving on-disk paths under a view.
//
// A view of a committed tree has no working copy, so its content comes out of
// the object store through refViewFiles and no path it resolves names a file:
// see errViewHasNoWorkingCopy below. A worktree view is the other case — it has
// a working copy, just not the one the path resolvers anchor to. Every resolver
// joins a repo-relative path onto the repository's canonical root, which is the
// checkout the index was built from: a different branch, and different bytes,
// from the one the request is reading through.
//
// The fix is one step at the end of resolution rather than a second resolver:
// the anchoring rules (repo prefixes, containment, sole-repo inference) are
// unchanged, and the checkout they land in is chosen last. That keeps a request
// with no view byte-identical to what it was, and makes "which checkout" a
// property of the request instead of a property of each call site.

// viewPathRoot is the working copy a request reads through, plus the repository
// it serves. The zero value is what every request that is not routed to a
// worktree view carries, and every method on it is then identity.
type viewPathRoot struct {
	root       string
	repoPrefix string
}

// requestViewPathRoot reports the working copy this request's view reads. It is
// empty for the base corpus and for a view of a committed tree — the latter
// serves bytes through refViewFiles, which replaces path resolution rather than
// re-rooting it.
func requestViewPathRoot(ctx context.Context) viewPathRoot {
	view := requestViewFromContext(ctx)
	if view == nil || view.viewRoot == "" {
		return viewPathRoot{}
	}
	return viewPathRoot{root: view.viewRoot, repoPrefix: viewRepoPrefix(view)}
}

// serves reports whether paths in a repository read through this view. An
// unknown prefix on either side means there is only one repository in play,
// which is the single-repo posture every other resolver takes.
func (v viewPathRoot) serves(repoPrefix string) bool {
	if v.root == "" {
		return false
	}
	return v.repoPrefix == "" || repoPrefix == "" || v.repoPrefix == repoPrefix
}

// contains reports whether an absolute path belongs to the selected checkout,
// accepting the physical spelling of a symlinked temp/root path as well as its
// lexical spelling. A symlink beneath the checkout is still checked by the
// existing repository confinement guard after resolution.
func (v viewPathRoot) contains(abs string) bool {
	if v.root == "" || abs == "" {
		return false
	}
	if pathContainedIn(filepath.Clean(abs), filepath.Clean(v.root)) {
		return true
	}
	realRoot, err := filepath.EvalSymlinks(v.root)
	if err != nil || realRoot == "" {
		realRoot = filepath.Clean(v.root)
	}
	realAbs, err := filepath.EvalSymlinks(abs)
	if err != nil || realAbs == "" {
		realAbs = resolveNearestExistingAncestor(abs)
	}
	return realAbs != "" && pathContainedIn(filepath.Clean(realAbs), filepath.Clean(realRoot))
}

// relativeWithinRoot renders target beneath root without letting a filesystem
// alias change the relative suffix. The lexical fast path is allocation-light;
// the canonical fallback handles /tmp versus /private/tmp and symlinked
// workspace roots. A missing target resolves through its nearest existing
// ancestor so newly-created files keep working too.
func relativeWithinRoot(root, target string) (string, bool) {
	if root == "" || target == "" {
		return "", false
	}
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if !pathContainedIn(target, root) {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil || realRoot == "" {
			return "", false
		}
		realTarget, err := filepath.EvalSymlinks(target)
		if err != nil || realTarget == "" {
			realTarget = resolveNearestExistingAncestor(target)
		}
		root, target = filepath.Clean(realRoot), filepath.Clean(realTarget)
		if !pathContainedIn(target, root) {
			return "", false
		}
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// rooted moves an absolute path that resolved against root into the checkout
// this view reads. A path already inside the view, or one that never sat under
// root, is returned untouched — re-rooting is a translation between two
// checkouts of one repository, not a way to invent a location.
func (v viewPathRoot) rooted(abs, root string) string {
	if v.root == "" || abs == "" || root == "" {
		return abs
	}
	if pathContainedIn(abs, v.root) {
		return abs
	}
	rel, ok := relativeWithinRoot(root, abs)
	if !ok {
		return abs
	}
	return filepath.Clean(filepath.Join(v.root, rel))
}

// errViewHasNoWorkingCopy is what node and graph path resolution answers a
// request reading a committed tree.
//
// Refusing is the point: every repository root on disk holds some other state
// of the world, so a path joined against one would name real bytes that are not
// this view's. Callers that tolerate a resolution failure fall back to the
// view's own file surface or report no path at all; callers that do not, refuse
// — which is the honest answer for a location that does not exist.
var errViewHasNoWorkingCopy = errors.New(
	"this request reads a committed tree that is not checked out anywhere, so it has no path on disk")

// requestReadsCommittedTree reports whether this request's view serves its
// content out of the object store rather than a working copy.
func requestReadsCommittedTree(ctx context.Context) bool {
	return viewReadsCommittedTree(requestViewFromContext(ctx))
}

// viewReadsCommittedTree reports whether the bytes a view serves live in a
// committed tree rather than on a working copy. It is the one predicate the
// byte lane classifies a view by, so refViewFilesFor and every path resolver
// answer the same question the same way.
//
// Two shapes answer yes, and a working copy hides the second:
//
//   - A view with no working copy at all: a ref or commit selector. It reads
//     through a committed-tree file surface and names no root, which is the
//     shape this predicate has always recognised.
//   - A routed checkout whose route has withdrawn its working-tree layer. The
//     stack is then the commit generation alone while the request still
//     carries the checkout's root, and the bytes on that root are free to have
//     moved past the tree the view reads. The window is real rather than
//     theoretical: materializeRequestView's RouteReady check
//     (view_request.go: "!found || !graphview.RouteReady(route)",
//     graphview/binding.go RouteReady, which requires DirtyGenerationID > 0)
//     and MaterializeCheckout's own route read (graphview/materialize.go,
//     which appends the dirty generation only when the route still names one)
//     are two separate reads, and CheckoutCoordinator.clearDirtySlot
//     (indexer/checkout_coordinator.go) between them materializes the commit
//     generation alone under a root the request keeps.
//
// The text lane already refuses that second shape from both ends — the
// coordinator's route check (indexer/checkout_text_search.go) and the
// top-layer completeness rule (graphview/materialize.go completeness, whose
// CapSearchText arm reads the top layer's silence as a denial) — so a byte
// lane that answered it off the root would serve bytes out of a tree the same
// request is told it may not search.
//
// A rooted view with no materialized stack is NOT classified as committed:
// nothing produces one in production (view_request.go sets viewRoot and
// materialized in the same literal, and that is the only assignment of
// viewRoot), and reading a root is what such a view has always done.
func viewReadsCommittedTree(v *requestView) bool {
	if v == nil {
		return false
	}
	if v.viewRoot == "" {
		return v.files != nil
	}
	return viewStackReadsCommittedTree(v.materialized)
}

// viewStackReadsCommittedTree reports whether the top of a materialized stack
// names a tree in git history rather than the bytes on a checkout root.
//
// Two spellings say yes, and the FIRST is the only one production builds:
//
//   - No layer at all. MaterializeCheckout leases the commit generation and
//     appends the working-tree one only when the route it re-reads still names
//     it (graphview/materialize.go, "if route.DirtyGenerationID > 0"), and
//     assemble's layer loop then runs zero times — so a checkout routed to its
//     commit generation alone materializes with an empty RepoViewID.Layers.
//     Every LayerRef the checkout path can mint is a LayerDirty (dirtyLayerRef
//     in graphview/materialize.go is the sole constructor on that path), which
//     is why an empty stack rather than a commit layer is what a withdrawn
//     working tree actually looks like.
//   - An explicit commit layer on top. The kind is in the layer vocabulary
//     (graphview/view.go) and indexer/builder_commit.go names it, and a
//     classification that depended on nothing ever minting one would be a
//     silent hole the day something does.
//
// A ref view also materializes with no layers, but it carries no root and is
// classified by its file surface before this is reached.
//
// The precondition is a composed Reader, and it is the same rule as the
// unwitnessed arm of noteWorktreeRouteDrift: an empty layer list is evidence
// only when it comes off a stack that was actually leased and composed
// (Materializer.assemble sets Reader for every stack it builds). A RepoView
// that composed no reader is an identity-only value carrying a prefix and a
// base generation, and reading ITS empty Layers as "the working tree was
// withdrawn" would be treating absence of data as evidence. Such a view keeps
// the working-copy classification it has always had.
func viewStackReadsCommittedTree(view *graphview.RepoView) bool {
	if view == nil || view.Reader == nil {
		return false
	}
	if len(view.ID.Layers) == 0 {
		return true
	}
	return view.ID.Layers[len(view.ID.Layers)-1].Kind == graphview.LayerCommit
}

// checkoutRootedPath places a resolved path in the checkout that owns it for
// this request.
//
// A routed view answers outright: it names the working copy the request reads,
// so the existence heuristic must not get a vote — it would happily move the
// path into a third checkout that happens to carry the file. Every other
// request keeps the heuristic it has always had.
func (s *Server) checkoutRootedPath(ctx context.Context, abs, root, repoPrefix string) string {
	if view := requestViewPathRoot(ctx); view.serves(repoPrefix) {
		return view.rooted(abs, root)
	}
	return worktreeRootedPath(abs, root, s.multiIndexer)
}

// noteWorktreeRouteDrift asks whether the route a routed view pinned moved
// while this request read through it, and records the answer on the rider.
//
// It is the working copy's half of the pin the base corpus already has. A
// request that reads a checkout's live root reads bytes nothing freezes: the
// route the view was materialized under is the one witness that says which
// snapshot those bytes were supposed to be, and the catalog bumps its epoch on
// every flip of either slot (catalog.go:1697, :1718-1726), so a changed epoch
// is evidence the working copy was re-sampled and re-published under the
// answer. Reading it costs one indexed row and no lock (catalog.go:1557-1576),
// which is why this is called once per answer rather than once per resolved
// path.
//
// Three outcomes, and the middle one is the point, exactly as for the base
// corpus (view_request.go: noteBaseCorpusChange):
//
//   - the epoch still matches: the answer is as coherent as the route said.
//   - the epoch moved: the route this answer was read under is gone, so the
//     result may be stitched from two states of the working copy. The
//     capability the caller used is annotated incomplete, and the exactness
//     claim is withdrawn (markWorktreeRouteMoved), so both ride back.
//   - nothing to compare against — no checkout id, no handle to read the
//     catalog through, or no route row to read: that is not evidence of a
//     change and must not be reported as one.
//
// Epoch 0 is a witnessed epoch, not an absent one, and treating it as absent
// blinded this check for a checkout's entire first route epoch. A route is
// installed at the zero value — CheckoutCoordinator.installStack takes the
// UpsertCheckoutRoute arm for a not-yet-routed checkout and ensureRoute does
// the same on first sight, and that statement writes
// route_epoch = excluded.route_epoch (store_sqlite/catalog.go
// UpsertCheckoutRoute) with no epoch set. Such a route is RouteActive with
// both slots published, so RouteReady holds and requests are served off it
// pinning epoch 0; the first flip under them (0 -> 1) is exactly "the route
// moved under this answer" and is the most common instance of it. What says
// "unwitnessed" is the absence of a route to compare against, which is what
// the found/catalog/checkout-id guards below test.
//
// Limitation, stated because the annotation must not be read as more than it
// is: the witness is the published route, not the bytes. A write that lands on
// the root and has not been sampled into a generation yet moves no epoch, so a
// read racing it is not labelled by this. What it does catch is the case the
// view identity cannot: the request kept reading while the checkout's route
// advanced underneath it.
func noteWorktreeRouteDrift(ctx context.Context, view *requestView, capability graphview.CapabilityID) bool {
	if ctx == nil || view == nil || !view.readsOwnCheckout() || view.materialized == nil {
		return false
	}
	checkoutID := viewCheckoutID(view)
	if checkoutID == "" {
		return false
	}
	catalog := viewRouteCatalog(view)
	if catalog == nil {
		return false
	}
	route, found, err := catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil || !found || route.RouteEpoch == view.materialized.CheckoutRouteEpoch {
		return false
	}
	// The exactness claim goes first, and it goes before the per-capability
	// dedupe below: the claim is a property of the whole answer, not of the one
	// capability whichever lane noticed first happened to be using, so a second
	// lane finding the same drift must not skip it.
	markWorktreeRouteMoved(view)
	if viewAlreadyDegraded(view, capability, graphview.StateIncomplete) {
		return true
	}
	view.noteDegraded([]graphview.CapabilityStatus{
		{Capability: capability, State: graphview.StateIncomplete},
	})
	return true
}

// routeMovedFallbackReason is the rider reason for an answer that was read
// across a move of the route the view pinned.
//
// It is a rider reason and not a graphview error code for the same reason
// baseChangedFallbackReason (view_request.go) is one: nothing failed, no
// substitute view was served, and the route named on the rider is still the
// route the caller asked for. What changed is the honesty of the exactness
// claim — the working copy under that route was re-sampled and re-published
// while the request was reading it, so the answer may be stitched from two
// states of it and the view can no longer reproduce it.
const routeMovedFallbackReason = "route_moved"

// markWorktreeRouteMoved withdraws the exactness claim of an answer read
// across a route move.
//
// This is the half of the base corpus's pin that the capability annotation
// alone does not carry. markBaseCorpusChange (view_request.go) reaches for
// view.rider.MarkFallback for exactly this case — the corpus moved under a
// request that still got the view it asked for — and a working copy that was
// re-published mid-answer is the same fact about the other half of the stack.
// Leaving exact:true on it says the caller got an answer the named view can
// reproduce, which is the one thing that is no longer true.
//
// The route stays named: ActualView is untouched, so a client can still see
// which stack answered. A rider that is already inexact keeps its original
// reason — the first substitution is the one the caller has to act on, and
// overwriting it would hide it.
//
// The rider write takes the view's own mutex, which the annotations already
// use, because unlike markBaseCorpusChange this runs INSIDE the handler: two
// lanes of one request (the byte lane's refViewFilesFor and the text lane's
// searchTextInView), or several goroutines of one fanned-out handler, can
// reach it at once.
func markWorktreeRouteMoved(view *requestView) {
	if view == nil {
		return
	}
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.rider == nil || !view.rider.Exact {
		return
	}
	// The reason is a non-empty constant, so the only error MarkFallback
	// defines (a blank reason) is unreachable here.
	_ = view.rider.MarkFallback(view.rider.ActualView, routeMovedFallbackReason)
}

// viewRouteCatalog reads the control plane through one of the view's own
// pinned generation handles. Every handle is a store over the same database
// and Catalog() rebases it (store_sqlite/catalog.go:35), so this needs no
// second store and cannot outlive the view's lease.
func viewRouteCatalog(view *requestView) *store_sqlite.Catalog {
	for _, source := range view.materialized.GenerationSources() {
		if source.Handle != nil {
			return source.Handle.Catalog()
		}
	}
	return nil
}

// viewAlreadyDegraded reports whether this exact annotation is already on the
// rider, so a request whose handlers each check drift says it once.
func viewAlreadyDegraded(view *requestView, capability graphview.CapabilityID, state graphview.CapabilityState) bool {
	degraded, _ := view.annotations()
	for _, status := range degraded {
		if status.Capability == capability && status.State == state {
			return true
		}
	}
	return false
}
