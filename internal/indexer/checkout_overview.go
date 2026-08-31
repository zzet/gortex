package indexer

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/reconcile"
)

// The administrative read model.
//
// Every administrative surface — the listing tool, the CLI verb, the previews
// the destructive flows render — asks the same question: what does the catalog
// currently say about this daemon's families, their corpora, their checkouts
// and the views over them. Answering it once here is what keeps the CLI and
// the tool surface from drifting into two different pictures of one catalog.
//
// The overview reports what the catalog holds and what this process is
// running. It probes no filesystem: a state is what the last reconciliation
// wrote, and taking a fresh sample is the force-reconcile verb's job. So a
// checkout whose root has been unplugged reads exactly as the reconciler last
// left it, deadlines included, rather than as whatever a stat happens to say
// the moment somebody runs a listing.

// FamiliesOverview is every family the catalog holds.
type FamiliesOverview struct {
	Families []FamilyOverview `json:"families"`
}

// FamilyOverview is one repository family and everything hanging off it.
type FamilyOverview struct {
	FamilyID      string `json:"family_id"`
	CommonDir     string `json:"common_dir"`
	DisplayRemote string `json:"display_remote,omitempty"`
	State         string `json:"state"`
	// PrimaryEpoch is the family's compare-and-set token. A caller that
	// previews a primary move and then confirms it carries this value.
	PrimaryEpoch int64 `json:"primary_epoch"`
	CreatedAt    int64 `json:"created_at,omitempty"`
	LastSeen     int64 `json:"last_seen,omitempty"`
	// PrimaryGraphID / PrimaryRepoPrefix name the family's base corpus,
	// empty when the family has none.
	PrimaryGraphID    string `json:"primary_graph_id,omitempty"`
	PrimaryRepoPrefix string `json:"primary_repo_prefix,omitempty"`

	Graphs    []GraphOverview    `json:"graphs,omitempty"`
	Checkouts []CheckoutOverview `json:"checkouts,omitempty"`
	RefViews  []RefViewOverview  `json:"ref_views,omitempty"`
}

// GraphOverview is one dedicated graph of a family.
type GraphOverview struct {
	GraphID            string `json:"graph_id"`
	RepoPrefix         string `json:"repo_prefix"`
	OwnerCheckoutID    string `json:"owner_checkout_id,omitempty"`
	IsPrimary          bool   `json:"is_primary"`
	State              string `json:"state"`
	ActiveGenerationID int64  `json:"active_generation_id,omitempty"`
	// Served reports that this process holds an indexer for the prefix, so
	// something can actually be composed over the corpus.
	Served bool `json:"served"`
}

// CheckoutOverview is one working copy as the catalog describes it.
type CheckoutOverview struct {
	CheckoutID  string `json:"checkout_id"`
	Incarnation string `json:"incarnation"`
	AdminName   string `json:"admin_name"`
	RootPath    string `json:"root_path"`
	GitDir      string `json:"git_dir,omitempty"`

	State         string `json:"state"`
	DesiredMode   string `json:"desired_mode"`
	EffectiveMode string `json:"effective_mode"`
	Locked        bool   `json:"locked,omitempty"`
	Prunable      bool   `json:"prunable,omitempty"`

	HeadRef    string `json:"head_ref,omitempty"`
	HeadCommit string `json:"head_commit,omitempty"`
	HeadTree   string `json:"head_tree,omitempty"`

	// Availability and Removal are the two clocks the reconciler runs, each
	// with the deadline it expires at.
	Availability ClockOverview `json:"availability"`
	Removal      ClockOverview `json:"removal"`

	LastAccessible int64  `json:"last_accessible,omitempty"`
	LastSeen       int64  `json:"last_seen,omitempty"`
	LastError      string `json:"last_error,omitempty"`

	// Evidence is the filesystem sample a removal is judged against.
	Evidence EvidenceOverview `json:"evidence"`

	// GraphID is the dedicated graph this checkout owns, empty for one served
	// through the family's automatic lane.
	GraphID string `json:"graph_id,omitempty"`
	// Route is where the checkout's queries land, present only for a routed
	// automatic checkout.
	Route *RouteOverview `json:"route,omitempty"`
	// CoordinatorLive reports that this process is running the build loop for
	// the checkout.
	CoordinatorLive bool `json:"coordinator_live"`
	// Intents names the active tracking-intent sources.
	Intents []string `json:"intents,omitempty"`
	// Transition is the in-flight mode change, empty when the slot is free.
	Transition string `json:"transition,omitempty"`
}

// ClockOverview is one of the reconciler's clocks: when it started and when it
// expires. Both are zero while the clock is not running.
type ClockOverview struct {
	StartedAt int64 `json:"started_at,omitempty"`
	Deadline  int64 `json:"deadline,omitempty"`
	// Evidence explains a running removal clock; empty on the availability
	// clock, which needs no evidence beyond the path not answering.
	Evidence string `json:"evidence,omitempty"`
	Running  bool   `json:"running"`
}

// EvidenceOverview summarises the stored path sample.
type EvidenceOverview struct {
	Present                     bool   `json:"present"`
	RootPathIdentity            string `json:"root_path_identity,omitempty"`
	RootVolumeKind              string `json:"root_volume_kind,omitempty"`
	NearestExistingAncestorPath string `json:"nearest_existing_ancestor_path,omitempty"`
	SampledAt                   int64  `json:"sampled_at,omitempty"`
	SampleGeneration            int64  `json:"sample_generation,omitempty"`
}

// RouteOverview is one checkout's route row.
type RouteOverview struct {
	GraphID            string `json:"graph_id"`
	CommitGenerationID int64  `json:"commit_generation_id,omitempty"`
	DirtyGenerationID  int64  `json:"dirty_generation_id,omitempty"`
	RouteEpoch         int64  `json:"route_epoch"`
	State              string `json:"state"`
	// Ready reports that the route can serve a composed view: active, with
	// both generation slots filled.
	Ready bool `json:"ready"`
}

// RefViewOverview is one named view of a graph.
type RefViewOverview struct {
	RefViewID     string `json:"ref_view_id"`
	GraphID       string `json:"graph_id"`
	SelectorKind  string `json:"selector_kind"`
	SelectorValue string `json:"selector_value"`
	State         string `json:"state"`

	ActiveGenerationID int64  `json:"active_generation_id,omitempty"`
	ActiveRef          string `json:"active_ref,omitempty"`
	ActiveCommit       string `json:"active_commit,omitempty"`
	ActiveTree         string `json:"active_tree,omitempty"`
	DesiredRef         string `json:"desired_ref,omitempty"`
	DesiredCommit      string `json:"desired_commit,omitempty"`
	DesiredTree        string `json:"desired_tree,omitempty"`

	LastResolved int64  `json:"last_resolved,omitempty"`
	LastSelected int64  `json:"last_selected,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

// FamiliesOverview reads the whole administrative picture out of the catalog.
//
// familyFilter narrows the answer to one family; an empty value takes every
// family the catalog holds. The filter accepts a family id, a repo prefix, a
// graph id or a path inside a tracked repository, so a caller can ask about
// "this checkout" without first having to learn its family's identifier.
func (l *CheckoutLifecycle) FamiliesOverview(ctx context.Context, familyFilter string) (FamiliesOverview, error) {
	if l == nil {
		return FamiliesOverview{}, fmt.Errorf("indexer: checkout lifecycle is not wired")
	}
	if l.catalog == nil {
		return FamiliesOverview{}, errNoCatalog
	}
	wanted := ""
	if familyFilter != "" {
		resolved, err := l.resolveFamilyID(ctx, familyFilter)
		if err != nil {
			return FamiliesOverview{}, err
		}
		wanted = resolved
	}

	families, err := l.catalog.ListRepositoryFamilies(ctx)
	if err != nil {
		return FamiliesOverview{}, err
	}
	out := FamiliesOverview{}
	for _, family := range families {
		if wanted != "" && family.FamilyID != wanted {
			continue
		}
		overview, err := l.familyOverview(ctx, family)
		if err != nil {
			return FamiliesOverview{}, err
		}
		out.Families = append(out.Families, overview)
	}
	return out, nil
}

// CatalogSeedFamilyIDs returns every family the catalog currently owns. It is
// the durable startup-reconciliation source: watcher membership can already be
// gone when the filesystem root disappears, but the catalog must still submit
// the family so its checkout and graph can be retired.
func (l *CheckoutLifecycle) CatalogSeedFamilyIDs(ctx context.Context) ([]string, error) {
	if l == nil {
		return nil, fmt.Errorf("indexer: checkout lifecycle is not wired")
	}
	if l.catalog == nil {
		return nil, errNoCatalog
	}
	families, err := l.catalog.ListRepositoryFamilies(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(families))
	for _, family := range families {
		ids = append(ids, family.FamilyID)
	}
	return ids, nil
}

// familyOverview assembles one family's entry.
func (l *CheckoutLifecycle) familyOverview(
	ctx context.Context, family store_sqlite.RepositoryFamily,
) (FamilyOverview, error) {
	out := FamilyOverview{
		FamilyID:      family.FamilyID,
		CommonDir:     family.CommonDirIdentity,
		DisplayRemote: family.DisplayRemote,
		State:         family.State,
		PrimaryEpoch:  family.PrimaryEpoch,
		CreatedAt:     family.CreatedAt,
		LastSeen:      family.LastSeen,
	}

	graphs, err := l.catalog.ListDedicatedGraphs(ctx, family.FamilyID)
	if err != nil {
		return out, err
	}
	ownedBy := make(map[string]string, len(graphs))
	for _, dedicated := range graphs {
		if dedicated.OwnerCheckoutID != "" {
			ownedBy[dedicated.OwnerCheckoutID] = dedicated.GraphID
		}
		if dedicated.IsPrimaryBase {
			out.PrimaryGraphID, out.PrimaryRepoPrefix = dedicated.GraphID, dedicated.RepoPrefix
		}
		out.Graphs = append(out.Graphs, GraphOverview{
			GraphID:            dedicated.GraphID,
			RepoPrefix:         dedicated.RepoPrefix,
			OwnerCheckoutID:    dedicated.OwnerCheckoutID,
			IsPrimary:          dedicated.IsPrimaryBase,
			State:              dedicated.State,
			ActiveGenerationID: dedicated.ActiveGenerationID,
			Served:             l.mi != nil && l.mi.GetMetadata(dedicated.RepoPrefix) != nil,
		})

		views, err := l.catalog.ListRefViews(ctx, dedicated.GraphID)
		if err != nil {
			return out, err
		}
		for _, view := range views {
			out.RefViews = append(out.RefViews, refViewOverview(view))
		}
	}

	checkouts, err := l.catalog.ListCheckouts(ctx, family.FamilyID)
	if err != nil {
		return out, err
	}
	for _, checkout := range checkouts {
		entry, err := l.checkoutOverview(ctx, checkout, ownedBy[checkout.CheckoutID])
		if err != nil {
			return out, err
		}
		out.Checkouts = append(out.Checkouts, entry)
	}
	return out, nil
}

// checkoutOverview assembles one checkout's entry.
func (l *CheckoutLifecycle) checkoutOverview(
	ctx context.Context, checkout store_sqlite.Checkout, graphID string,
) (CheckoutOverview, error) {
	out := CheckoutOverview{
		CheckoutID:      checkout.CheckoutID,
		Incarnation:     checkout.Incarnation,
		AdminName:       checkout.AdminName,
		RootPath:        checkout.RootPath,
		GitDir:          checkout.GitDir,
		State:           string(checkout.State),
		DesiredMode:     string(checkout.DesiredMode),
		EffectiveMode:   string(checkout.EffectiveMode),
		Locked:          checkout.Locked,
		Prunable:        checkout.Prunable,
		HeadRef:         checkout.HeadRef,
		HeadCommit:      checkout.HeadCommit,
		HeadTree:        checkout.HeadTree,
		LastAccessible:  checkout.LastAccessible,
		LastSeen:        checkout.LastSeen,
		LastError:       checkout.LastError,
		GraphID:         graphID,
		CoordinatorLive: l.hasCoordinator(checkout.CheckoutID),
		Availability: ClockOverview{
			StartedAt: checkout.UnavailableSince,
			Deadline:  checkout.AvailabilityDeadline,
			Running:   checkout.AvailabilityDeadline > 0,
		},
		Removal: ClockOverview{
			StartedAt: checkout.RemovalDetectedAt,
			Deadline:  checkout.RemovalDeadline,
			Evidence:  checkout.RemovalEvidence,
			Running:   checkout.RemovalDeadline > 0,
		},
	}

	evidence, present, err := l.catalog.GetCheckoutPathEvidence(ctx, checkout.CheckoutID)
	if err != nil {
		return out, err
	}
	out.Evidence = EvidenceOverview{Present: present}
	if present {
		out.Evidence.RootPathIdentity = evidence.RootPathIdentity
		out.Evidence.RootVolumeKind = evidence.RootVolumeKind
		out.Evidence.NearestExistingAncestorPath = evidence.NearestExistingAncestorPath
		out.Evidence.SampledAt = evidence.SampledAt
		out.Evidence.SampleGeneration = evidence.SampleGeneration
	}

	route, routed, err := l.catalog.GetCheckoutRoute(ctx, checkout.CheckoutID)
	if err != nil {
		return out, err
	}
	if routed {
		out.Route = routeOverviewOf(route)
	}

	intents, err := l.catalog.ListTrackingIntents(ctx, checkout.CheckoutID)
	if err != nil {
		return out, err
	}
	for _, intent := range intents {
		if intent.Active {
			out.Intents = append(out.Intents, string(intent.SourceKind))
		}
	}

	transition, inFlight, err := l.catalog.GetIntentTransition(ctx, checkout.CheckoutID)
	if err != nil {
		return out, err
	}
	if inFlight {
		out.Transition = string(transition.State)
	}
	return out, nil
}

// routeOverviewOf projects one route row.
func routeOverviewOf(route store_sqlite.CheckoutRoute) *RouteOverview {
	return &RouteOverview{
		GraphID:            route.GraphID,
		CommitGenerationID: route.CommitGenerationID,
		DirtyGenerationID:  route.DirtyGenerationID,
		RouteEpoch:         route.RouteEpoch,
		State:              string(route.State),
		Ready:              graphview.RouteReady(route),
	}
}

// refViewOverview projects one ref-view row.
func refViewOverview(view store_sqlite.RefView) RefViewOverview {
	return RefViewOverview{
		RefViewID:          view.RefViewID,
		GraphID:            view.GraphID,
		SelectorKind:       view.SelectorKind,
		SelectorValue:      view.SelectorValue,
		State:              string(view.State),
		ActiveGenerationID: view.ActiveGenerationID,
		ActiveRef:          view.ActiveRef,
		ActiveCommit:       view.ActiveCommit,
		ActiveTree:         view.ActiveTree,
		DesiredRef:         view.DesiredRef,
		DesiredCommit:      view.DesiredCommit,
		DesiredTree:        view.DesiredTree,
		LastResolved:       view.LastResolved,
		LastSelected:       view.LastSelected,
		LastError:          view.LastError,
	}
}

// --- selector resolution ------------------------------------------------

// resolveFamilyID turns a family id, a graph id, a repo prefix or a path into
// the family it belongs to.
//
// The path branch is last and reads the checkout rows rather than the corpus,
// so a worktree that is served through the family's automatic lane — and
// therefore has no corpus of its own to resolve a prefix from — still names
// its family.
func (l *CheckoutLifecycle) resolveFamilyID(ctx context.Context, selector string) (string, error) {
	if _, found, err := l.catalog.GetRepositoryFamily(ctx, selector); err != nil {
		return "", err
	} else if found {
		return selector, nil
	}
	dedicated, graphErr := l.ResolveGraph(ctx, selector)
	if graphErr == nil {
		return dedicated.FamilyID, nil
	}
	checkout, found, err := l.checkoutForPath(ctx, selector)
	if err != nil {
		return "", err
	}
	if !found {
		return "", graphErr
	}
	return checkout.FamilyID, nil
}

// ResolveGraph resolves a graph id, a repo prefix, or a path inside a tracked
// repository to the dedicated graph that serves it.
//
// It is what lets every administrative surface take the selector a user
// actually has to hand — usually a directory — instead of an opaque graph
// identifier they would first have to look up.
func (l *CheckoutLifecycle) ResolveGraph(ctx context.Context, selector string) (store_sqlite.DedicatedGraph, error) {
	if l == nil || l.catalog == nil {
		return store_sqlite.DedicatedGraph{}, errNoCatalog
	}
	if selector == "" {
		return store_sqlite.DedicatedGraph{}, fmt.Errorf("%w: no target given", ErrCheckoutNotTracked)
	}
	if dedicated, found, err := l.catalog.GetDedicatedGraph(ctx, selector); err != nil {
		return store_sqlite.DedicatedGraph{}, err
	} else if found {
		return dedicated, nil
	}
	prefix := l.ResolvePrefix(selector)
	if prefix == "" {
		return store_sqlite.DedicatedGraph{}, fmt.Errorf("%w: %s", ErrCheckoutNotTracked, selector)
	}
	dedicated, found, err := l.catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
	if err != nil {
		return store_sqlite.DedicatedGraph{}, err
	}
	if !found {
		return store_sqlite.DedicatedGraph{}, fmt.Errorf(
			"%w: corpus %s has no dedicated graph", ErrCheckoutNotTracked, prefix)
	}
	return dedicated, nil
}

// --- view binding -------------------------------------------------------

// ViewBinding explains which graph answers for one filesystem path, and why.
//
// It is the diagnostic behind "why is this worktree answering with the other
// one's code": the chain is walked in the same order the request path walks
// it, and the first step that cannot be taken is the reason the request falls
// through to the base corpus.
type ViewBinding struct {
	Path string `json:"path"`
	// Matched reports that the path lies inside a registered checkout.
	Matched bool `json:"matched"`

	FamilyID      string `json:"family_id,omitempty"`
	CheckoutID    string `json:"checkout_id,omitempty"`
	Incarnation   string `json:"incarnation,omitempty"`
	AdminName     string `json:"admin_name,omitempty"`
	RootPath      string `json:"root_path,omitempty"`
	CheckoutState string `json:"checkout_state,omitempty"`
	DesiredMode   string `json:"desired_mode,omitempty"`
	EffectiveMode string `json:"effective_mode,omitempty"`

	// GraphID / RepoPrefix name the corpus that answers.
	GraphID    string `json:"graph_id,omitempty"`
	RepoPrefix string `json:"repo_prefix,omitempty"`
	// PrimaryGraphID is the family's base corpus, which is what an automatic
	// checkout's layers compose over.
	PrimaryGraphID string `json:"primary_graph_id,omitempty"`

	Route           *RouteOverview `json:"route,omitempty"`
	CoordinatorLive bool           `json:"coordinator_live"`

	// Composed reports that a composed checkout view answers rather than the
	// base corpus on its own.
	Composed bool `json:"composed"`
	// Reason states why the base corpus answers, empty when a composed view
	// does.
	Reason string `json:"reason,omitempty"`
	// Chain is the binding steps in the order they were walked.
	Chain []string `json:"chain"`
}

// ExplainView walks the binding chain for one filesystem path.
//
// Nothing is built and nothing is probed: every step reads a catalog row or
// this process's coordinator registry, so the answer describes the daemon a
// caller is actually talking to rather than one that would exist after a
// reconciliation.
func (l *CheckoutLifecycle) ExplainView(ctx context.Context, path string) (ViewBinding, error) {
	if l == nil {
		return ViewBinding{}, fmt.Errorf("indexer: checkout lifecycle is not wired")
	}
	if l.catalog == nil {
		return ViewBinding{}, errNoCatalog
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	out := ViewBinding{Path: abs}

	checkout, found, err := l.checkoutForPath(ctx, abs)
	if err != nil {
		return out, err
	}
	if !found {
		out.Reason = "no registered checkout contains this path"
		out.Chain = append(out.Chain, "path "+abs+" is inside no registered checkout")
		if prefix := l.ResolvePrefix(abs); prefix != "" {
			out.RepoPrefix = prefix
			out.GraphID = GraphIDFor(prefix)
			out.Chain = append(out.Chain, "corpus "+prefix+" serves it as an ordinary tracked repository")
		}
		return out, nil
	}

	out.Matched = true
	out.FamilyID = checkout.FamilyID
	out.CheckoutID = checkout.CheckoutID
	out.Incarnation = checkout.Incarnation
	out.AdminName = checkout.AdminName
	out.RootPath = checkout.RootPath
	out.CheckoutState = string(checkout.State)
	out.DesiredMode = string(checkout.DesiredMode)
	out.EffectiveMode = string(checkout.EffectiveMode)
	out.CoordinatorLive = l.hasCoordinator(checkout.CheckoutID)
	out.Chain = append(out.Chain,
		"checkout "+checkout.AdminName+" ("+checkout.CheckoutID+") owns the longest root containing the path")

	graphs, err := l.catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return out, err
	}
	var owned, primary *store_sqlite.DedicatedGraph
	for i := range graphs {
		if graphs[i].OwnerCheckoutID == checkout.CheckoutID {
			owned = &graphs[i]
		}
		if graphs[i].IsPrimaryBase {
			primary = &graphs[i]
		}
	}
	if primary != nil {
		out.PrimaryGraphID = primary.GraphID
	}

	if checkout.State != store_sqlite.CheckoutStateReady {
		// Grace is read-only and never exact. Prefer the family's primary even
		// when this checkout used to be dedicated; if the missing checkout owns
		// that primary, the same corpus remains a fallback rather than being
		// presented as a live working-copy view.
		fallback := primary
		if fallback == nil {
			fallback = owned
		}
		if fallback != nil {
			out.GraphID, out.RepoPrefix = fallback.GraphID, fallback.RepoPrefix
		}
		out.Reason = "the checkout is " + string(checkout.State) + ", so its graph is available only as a read-only fallback"
		out.Chain = append(out.Chain, "state is "+string(checkout.State)+": no checkout-specific layers are composed")
		return out, nil
	}

	if checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic {
		out.Reason = "the checkout is dedicated, so its own corpus answers directly"
		out.Chain = append(out.Chain, "mode is "+string(checkout.EffectiveMode)+": no layers are composed")
		if owned != nil {
			out.GraphID, out.RepoPrefix = owned.GraphID, owned.RepoPrefix
			out.Chain = append(out.Chain, "corpus "+owned.RepoPrefix+" holds its nodes")
		}
		return out, nil
	}
	out.Chain = append(out.Chain, "mode is automatic: the family's primary corpus is the base")

	if primary == nil {
		out.Reason = "the family has no primary corpus to compose over"
		return out, nil
	}
	out.GraphID, out.RepoPrefix = primary.GraphID, primary.RepoPrefix

	route, routed, err := l.catalog.GetCheckoutRoute(ctx, checkout.CheckoutID)
	if err != nil {
		return out, err
	}
	if !routed {
		out.Reason = "the checkout has no route, so the base corpus answers"
		return out, nil
	}
	out.Route = routeOverviewOf(route)
	out.GraphID = route.GraphID
	out.Chain = append(out.Chain, fmt.Sprintf(
		"route points at graph %s with commit generation %d and dirty generation %d",
		route.GraphID, route.CommitGenerationID, route.DirtyGenerationID))
	if !graphview.RouteReady(route) {
		out.Reason = "the route is " + string(route.State) + " and does not name both generations yet"
		return out, nil
	}
	out.Composed = true
	out.Chain = append(out.Chain, "both layers are published: the composed checkout view answers")
	return out, nil
}

// checkoutForPath finds the registered checkout a path sits in, preferring the
// longest matching root so a worktree nested inside another checkout resolves
// to itself.
func (l *CheckoutLifecycle) checkoutForPath(
	ctx context.Context, path string,
) (store_sqlite.Checkout, bool, error) {
	families, err := l.catalog.ListRepositoryFamilies(ctx)
	if err != nil {
		return store_sqlite.Checkout{}, false, err
	}
	spellings := pathSpellings(path)
	var (
		best  store_sqlite.Checkout
		found bool
	)
	for _, family := range families {
		checkouts, err := l.catalog.ListCheckouts(ctx, family.FamilyID)
		if err != nil {
			return store_sqlite.Checkout{}, false, err
		}
		for _, checkout := range checkouts {
			if checkout.RootPath == "" {
				continue
			}
			root := filepath.Clean(checkout.RootPath)
			if !containsAnySpelling(spellings, root) {
				continue
			}
			if !found || len(root) > len(filepath.Clean(best.RootPath)) {
				best, found = checkout, true
			}
		}
	}
	return best, found, nil
}

// pathSpellings is the ways one path may be written: as the caller gave it,
// and with its symlinks resolved.
//
// Git spells every worktree root with its symlinks resolved, and a caller
// spells its working directory the way the shell handed it over — which on
// macOS is a path through /var into /private/var. Comparing one spelling only
// would report "no registered checkout contains this path" for a directory
// that plainly is one.
func pathSpellings(path string) []string {
	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(path); err == nil {
		cleaned = abs
	}
	out := []string{cleaned}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		if resolved = filepath.Clean(resolved); resolved != cleaned {
			out = append(out, resolved)
		}
	}
	return out
}

// containsAnySpelling reports whether root contains any spelling of a path.
func containsAnySpelling(spellings []string, root string) bool {
	for _, spelling := range spellings {
		if pathkey.HasPathPrefix(spelling, root) {
			return true
		}
	}
	return false
}

// --- forced reconciliation ----------------------------------------------

// ReconcileFamily runs one family's reconciliation now and applies the
// coordinator dispositions it reports.
//
// It is the administrative force-reconcile: the janitor asks the same question
// on its own schedule, and this is how an operator who has just moved a
// worktree gets the answer without waiting an hour for it. Unlike the read
// model it does probe the filesystem — that is the whole point of asking.
func (l *CheckoutLifecycle) ReconcileFamily(ctx context.Context, familyID string) (reconcile.FamilyReport, error) {
	if l == nil || l.rec == nil {
		return reconcile.FamilyReport{}, errNoCatalog
	}
	if familyID == "" {
		return reconcile.FamilyReport{}, fmt.Errorf("%w: no family given", ErrCheckoutNotTracked)
	}
	defer l.beginBatch()()
	report, err := l.rec.ReconcileFamily(ctx, familyID, l.probeDirFor(ctx, familyID, ""))
	if err != nil {
		return reconcile.FamilyReport{}, err
	}
	convergeErr := l.applyReconcileReport(ctx, report)
	l.sweepRetirements(ctx)
	if familyReportRemoved(report) {
		l.saveConfig("reconcile")
		l.notifyTrackedSetChanged()
	}
	return report, convergeErr
}

// ResolveFamilyID exposes the selector resolution the administrative surfaces
// share: a family id, a graph id, a repo prefix or a path all name one family.
func (l *CheckoutLifecycle) ResolveFamilyID(ctx context.Context, selector string) (string, error) {
	if l == nil || l.catalog == nil {
		return "", errNoCatalog
	}
	return l.resolveFamilyID(ctx, selector)
}
