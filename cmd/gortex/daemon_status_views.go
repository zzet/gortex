package main

import (
	"context"
	"fmt"
	"sort"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// routedRepoStatus is the cheap status projection of one checkout's selected
// graph. It contains catalog rows and counters the indexer already persisted;
// constructing it never counts nodes or edges.
type routedRepoStatus struct {
	checkoutID string
	rootPath   string
	repoPrefix string

	viewState   string
	countsKnown bool
	generations []int64

	files     int
	nodes     int
	edges     int
	lastIndex int64
}

// routedRepoStatusSnapshot distinguishes an installation with no view catalog
// from a catalog read that failed. In the latter case rows carries the bounded
// last-successful identity set, so only known routed repos degrade instead of
// falling back to generation-zero shell counters and calling them true.
type routedRepoStatusSnapshot struct {
	enabled   bool
	available bool
	rows      []routedRepoStatus
}

var (
	statusCountsKnown   = true
	statusCountsUnknown = false
	statusMemoryUnknown = false
)

// collectRoutedRepoStatuses snapshots the selected route of every catalogued
// checkout. The read is bounded by catalog cardinality and generation ancestry;
// payload access is limited to persisted repo counters and the sparse ownership
// metadata needed to prove that upper layers are no-ops.
func (c *realController) collectRoutedRepoStatuses(ctx context.Context) routedRepoStatusSnapshot {
	out := routedRepoStatusSnapshot{}
	if c == nil || c.viewMaterializer == nil || c.viewMaterializer.Catalog == nil ||
		c.viewMaterializer.Store == nil {
		return out
	}
	out.enabled = true
	rows, err := c.readRoutedRepoStatuses(ctx)
	if err != nil {
		if c.logger != nil {
			c.logger.Debug("daemon: routed status projection unavailable", zap.Error(err))
		}
		if previous := c.lastRoutedRepos.Load(); previous != nil {
			// Do not reuse the previous counters or health state. The bounded
			// identity set is enough to prevent generation-zero shell counters
			// from masquerading as the selected view during this failed read.
			out.rows = previous.rows
		}
		return out
	}
	out.available = true
	out.rows = rows
	cached := out
	c.lastRoutedRepos.Store(&cached)
	return out
}

func (c *realController) readRoutedRepoStatuses(ctx context.Context) ([]routedRepoStatus, error) {
	catalog := c.viewMaterializer.Catalog
	families, err := catalog.ListRepositoryFamilies(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repository families: %w", err)
	}

	var checkouts []store_sqlite.Checkout
	graphsByID := make(map[string]store_sqlite.DedicatedGraph)
	graphsByOwner := make(map[string]store_sqlite.DedicatedGraph)
	primaryByFamily := make(map[string]store_sqlite.DedicatedGraph)
	for _, family := range families {
		familyCheckouts, listErr := catalog.ListCheckouts(ctx, family.FamilyID)
		if listErr != nil {
			return nil, fmt.Errorf("list family %s checkouts: %w", family.FamilyID, listErr)
		}
		checkouts = append(checkouts, familyCheckouts...)
		graphs, listErr := catalog.ListDedicatedGraphs(ctx, family.FamilyID)
		if listErr != nil {
			return nil, fmt.Errorf("list family %s graphs: %w", family.FamilyID, listErr)
		}
		for _, graph := range graphs {
			graphsByID[graph.GraphID] = graph
			graphsByOwner[graph.OwnerCheckoutID] = graph
			if graph.IsPrimaryBase {
				primaryByFamily[family.FamilyID] = graph
			}
		}
	}
	if len(checkouts) == 0 {
		return nil, nil
	}

	checkoutIDs := make([]string, 0, len(checkouts))
	for _, checkout := range checkouts {
		checkoutIDs = append(checkoutIDs, checkout.CheckoutID)
	}
	routes, err := catalog.GetCheckoutRoutes(ctx, checkoutIDs)
	if err != nil {
		return nil, fmt.Errorf("read checkout routes: %w", err)
	}
	transitions, err := catalog.ListIntentTransitions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list checkout transitions: %w", err)
	}
	transitionByCheckout := make(map[string]store_sqlite.IntentTransition, len(transitions))
	for _, transition := range transitions {
		transitionByCheckout[transition.CheckoutID] = transition
	}

	generations := &statusGenerationCache{
		catalog: catalog,
		store:   c.viewMaterializer.Store,
		rows:    make(map[int64]store_sqlite.ViewGeneration),
	}
	rows := make([]routedRepoStatus, 0, len(checkouts))
	for _, checkout := range checkouts {
		graph, graphFound := graphsByOwner[checkout.CheckoutID]
		if checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic {
			graph, graphFound = primaryByFamily[checkout.FamilyID]
		}
		prefix := graph.RepoPrefix
		if prefix == "" && c.lifecycle != nil {
			prefix = c.lifecycle.ResolvePrefix(checkout.RootPath)
		}
		row := routedRepoStatus{
			checkoutID: checkout.CheckoutID,
			rootPath:   checkout.RootPath,
			repoPrefix: prefix,
			viewState:  daemon.RepoViewStateBuilding,
		}
		transition, transitioning := transitionByCheckout[checkout.CheckoutID]
		projectCheckoutRouteStatus(
			ctx, &row, checkout, graph, graphFound, routes[checkout.CheckoutID],
			transition, transitioning, graphsByID, generations,
		)
		rows = append(rows, row)
	}

	// A route may move while its ancestry is read. Re-read every route in one
	// batch and refuse the mixed snapshot instead of publishing counts for the
	// route that just lost the race.
	currentRoutes, err := catalog.GetCheckoutRoutes(ctx, checkoutIDs)
	if err != nil {
		return nil, fmt.Errorf("revalidate checkout routes: %w", err)
	}
	for i := range rows {
		if currentRoutes[rows[i].checkoutID] == routes[rows[i].checkoutID] {
			continue
		}
		rows[i].viewState = daemon.RepoViewStateBuilding
		rows[i].countsKnown = false
		rows[i].files, rows[i].nodes, rows[i].edges = 0, 0, 0
	}

	// Stable ordering makes cached status responses and benchmark profiles
	// deterministic even though families may have been assembled separately.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].rootPath != rows[j].rootPath {
			return rows[i].rootPath < rows[j].rootPath
		}
		return rows[i].checkoutID < rows[j].checkoutID
	})
	return rows, nil
}

// statusGenerationCache de-duplicates immutable generation reads shared by a
// family. Automatic worktrees commonly share the same dedicated base, so the
// cache turns N identical catalog probes into one.
type statusGenerationCache struct {
	catalog *store_sqlite.Catalog
	store   *store_sqlite.Store
	rows    map[int64]store_sqlite.ViewGeneration
}

func (c *statusGenerationCache) generation(
	ctx context.Context, id int64,
) (store_sqlite.ViewGeneration, bool, error) {
	if id <= 0 {
		return store_sqlite.ViewGeneration{}, false, nil
	}
	if row, ok := c.rows[id]; ok {
		return row, true, nil
	}
	row, found, err := c.catalog.GetViewGeneration(ctx, id)
	if err == nil && found {
		c.rows[id] = row
	}
	return row, found, err
}

func projectCheckoutRouteStatus(
	ctx context.Context,
	out *routedRepoStatus,
	checkout store_sqlite.Checkout,
	selectedGraph store_sqlite.DedicatedGraph,
	graphFound bool,
	route store_sqlite.CheckoutRoute,
	transition store_sqlite.IntentTransition,
	transitioning bool,
	graphsByID map[string]store_sqlite.DedicatedGraph,
	generations *statusGenerationCache,
) {
	if out == nil {
		return
	}
	degrade := func() {
		out.viewState = daemon.RepoViewStateDegraded
		out.countsKnown = false
	}
	build := func() {
		out.viewState = daemon.RepoViewStateBuilding
		out.countsKnown = false
	}

	if checkout.State != store_sqlite.CheckoutStateReady {
		switch checkout.State {
		case store_sqlite.CheckoutStateAvailabilityGrace,
			store_sqlite.CheckoutStateRemovalGrace,
			store_sqlite.CheckoutStateUnavailable:
			degrade()
		default:
			build()
		}
		return
	}
	if transitioning {
		if transition.State == store_sqlite.IntentTransitionFailed {
			degrade()
		} else {
			build()
		}
		return
	}
	if !graphFound || selectedGraph.GraphID == "" || selectedGraph.ActiveGenerationID <= 0 {
		build()
		return
	}
	if selectedGraph.State != store_sqlite.DedicatedGraphStateReady {
		build()
		return
	}
	active, found, err := generations.generation(ctx, selectedGraph.ActiveGenerationID)
	if err != nil {
		degrade()
		return
	}
	if !found || active.State == store_sqlite.ViewGenerationBuilding {
		build()
		return
	}
	if active.State != store_sqlite.ViewGenerationReady {
		degrade()
		return
	}

	if route.CheckoutID == "" || route.State != store_sqlite.RouteActive ||
		route.GraphID == "" || route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0 {
		build()
		return
	}
	routeGraph, routeGraphFound := graphsByID[route.GraphID]
	if !routeGraphFound || route.GraphID != selectedGraph.GraphID ||
		routeGraph.RepoPrefix != selectedGraph.RepoPrefix {
		degrade()
		return
	}

	stack, state, err := routedGenerationStack(
		ctx, route.CommitGenerationID, route.DirtyGenerationID, generations,
	)
	if err != nil {
		degrade()
		return
	}
	out.generations = generationIDs(stack)
	if state != daemon.RepoViewStateReady {
		out.viewState = state
		return
	}
	commit := stack[len(stack)-2]
	if commit.GenerationID != route.CommitGenerationID || commit.TreeOID != checkout.HeadTree {
		build()
		return
	}

	out.viewState = daemon.RepoViewStateReady
	for _, generation := range stack {
		if generation.PublishedAt > out.lastIndex {
			out.lastIndex = generation.PublishedAt
		}
	}
	if !exactStatusCountsAvailable(out, stack, generations) {
		out.countsKnown = false
		return
	}
	out.countsKnown = true
}

func routedGenerationStack(
	ctx context.Context,
	commitID, dirtyID int64,
	cache *statusGenerationCache,
) ([]store_sqlite.ViewGeneration, string, error) {
	var reversed []store_sqlite.ViewGeneration
	seen := make(map[int64]struct{}, 4)
	for id := commitID; id > 0; {
		if _, duplicate := seen[id]; duplicate {
			return nil, daemon.RepoViewStateDegraded, nil
		}
		seen[id] = struct{}{}
		row, found, err := cache.generation(ctx, id)
		if err != nil {
			return nil, daemon.RepoViewStateDegraded, err
		}
		state := statusGenerationState(row, found)
		if state != daemon.RepoViewStateReady {
			return nil, state, nil
		}
		reversed = append(reversed, row)
		id = row.BaseGenerationID
	}
	stack := make([]store_sqlite.ViewGeneration, len(reversed), len(reversed)+1)
	for i := range reversed {
		stack[len(reversed)-1-i] = reversed[i]
	}
	dirty, found, err := cache.generation(ctx, dirtyID)
	if err != nil {
		return nil, daemon.RepoViewStateDegraded, err
	}
	if state := statusGenerationState(dirty, found); state != daemon.RepoViewStateReady {
		return nil, state, nil
	}
	if _, duplicate := seen[dirtyID]; duplicate || dirty.BaseGenerationID != commitID {
		return nil, daemon.RepoViewStateDegraded, nil
	}
	return append(stack, dirty), daemon.RepoViewStateReady, nil
}

func statusGenerationState(row store_sqlite.ViewGeneration, found bool) string {
	if !found || row.State == store_sqlite.ViewGenerationBuilding {
		return daemon.RepoViewStateBuilding
	}
	switch row.State {
	case store_sqlite.ViewGenerationReady, store_sqlite.ViewGenerationSuperseded:
		return daemon.RepoViewStateReady
	default:
		return daemon.RepoViewStateDegraded
	}
}

func generationIDs(rows []store_sqlite.ViewGeneration) []int64 {
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.GenerationID)
	}
	return out
}

// exactStatusCountsAvailable fills counters only when the stack has a full
// dedicated base and every generation above it is provably a no-op. A changed
// sparse layer can replace rows below it; summing payload counters would double
// count those rows, while deriving the delta would require the graph scan this
// status path is expressly forbidden to perform.
func exactStatusCountsAvailable(
	out *routedRepoStatus,
	stack []store_sqlite.ViewGeneration,
	cache *statusGenerationCache,
) bool {
	if out == nil || len(stack) == 0 || out.repoPrefix == "" {
		return false
	}
	base := stack[0]
	if base.BaseGenerationID != 0 || base.GenerationKind != "dedicated_base" {
		return false
	}
	for _, upper := range stack[1:] {
		if upper.CoveredFiles != 0 || upper.AffectedFiles != 0 || upper.StorageBytes != 0 {
			return false
		}
		handle := cache.store.AtGeneration(upper.GenerationID)
		if handle == nil {
			return false
		}
		tombstones, err := handle.NodeTombstones()
		if err != nil || len(tombstones) != 0 {
			return false
		}
		edgeSources, err := handle.EdgeSourceMasks()
		if err != nil || len(edgeSources) != 0 {
			return false
		}
	}

	handle := cache.store.AtGeneration(base.GenerationID)
	if handle == nil {
		return false
	}
	state, found, err := handle.GetRepoIndexState(out.repoPrefix)
	if err != nil {
		return false
	}
	if !found && base.CoveredFiles != 0 {
		return false
	}
	out.files = int(base.CoveredFiles)
	if found {
		out.nodes = state.NodeCount
		out.edges = state.EdgeCount
		if state.IndexedAt > out.lastIndex {
			out.lastIndex = state.IndexedAt
		}
	}
	return true
}

// projectRoutedRepoRows overlays catalog truth on process-local indexer rows.
// It returns how many rows selected a routed view; callers use that to suppress
// the generation-zero FTS document count, which cannot describe those views.
func projectRoutedRepoRows(
	rows []daemon.TrackedRepoStatus, snapshot routedRepoStatusSnapshot,
) int {
	if !snapshot.enabled {
		return 0
	}
	byPath := make(map[string]routedRepoStatus, len(snapshot.rows))
	byPrefix := make(map[string]routedRepoStatus, len(snapshot.rows))
	for _, view := range snapshot.rows {
		if view.rootPath != "" {
			byPath[view.rootPath] = view
		}
		if view.repoPrefix != "" {
			if _, exists := byPrefix[view.repoPrefix]; !exists {
				byPrefix[view.repoPrefix] = view
			}
		}
	}
	projected := 0
	for i := range rows {
		view, found := byPath[rows[i].Path]
		if !found && rows[i].Path != "" {
			// The map is the allocation-free common path. The folded fallback
			// preserves macOS/Windows path identity when config and catalog use
			// different Unicode composition or case spellings.
			view, found = matchRoutedRepoStatus(rows[i].Path, "", snapshot.rows)
		}
		if !found {
			view, found = byPrefix[rows[i].Prefix]
		}
		if !found {
			continue
		}
		if snapshot.available {
			applyRoutedRepoStatus(&rows[i], view)
		} else {
			applyUnavailableRoutedRepoStatus(&rows[i])
		}
		projected++
	}
	return projected
}

func matchRoutedRepoStatus(path, prefix string, rows []routedRepoStatus) (routedRepoStatus, bool) {
	best := -1
	for i := range rows {
		if path != "" && rows[i].rootPath != "" && pathkey.EqualPaths(path, rows[i].rootPath) {
			if best < 0 || rows[i].checkoutID > rows[best].checkoutID {
				best = i
			}
		}
	}
	if best >= 0 {
		return rows[best], true
	}
	for i := range rows {
		if prefix != "" && rows[i].repoPrefix == prefix {
			return rows[i], true
		}
	}
	return routedRepoStatus{}, false
}

func applyRoutedRepoStatus(row *daemon.TrackedRepoStatus, view routedRepoStatus) {
	if row == nil {
		return
	}
	legacyPrefix := row.Prefix
	row.ViewState = view.viewState
	row.ViewGenerations = append(row.ViewGenerations[:0], view.generations...)
	row.CountsKnown = &statusCountsUnknown
	row.MemoryKnown = &statusMemoryUnknown
	row.Unloaded = false
	row.Files, row.Nodes, row.Edges = 0, 0, 0
	row.Memory = daemon.MemoryBreakdown{}
	row.LastIndex = view.lastIndex
	if view.repoPrefix != "" {
		row.Prefix = view.repoPrefix
		if row.Workspace == "" || row.Workspace == legacyPrefix {
			row.Workspace = view.repoPrefix
		}
		if row.WorkspaceProject == "" || row.WorkspaceProject == legacyPrefix {
			row.WorkspaceProject = view.repoPrefix
		}
	}
	if view.rootPath != "" {
		row.Path = view.rootPath
	}
	if view.viewState != daemon.RepoViewStateReady || !view.countsKnown {
		return
	}
	row.CountsKnown = &statusCountsKnown
	row.Files, row.Nodes, row.Edges = view.files, view.nodes, view.edges
}

// applyUnavailableRoutedRepoStatus invalidates only fields whose truth depends
// on the selected route. Callers invoke it solely for a path/prefix present in
// the last successful catalog snapshot, so an independent legacy row remains
// untouched when the catalog is unavailable.
func applyUnavailableRoutedRepoStatus(row *daemon.TrackedRepoStatus) {
	if row == nil {
		return
	}
	row.ViewState = daemon.RepoViewStateDegraded
	row.ViewGenerations = nil
	row.CountsKnown = &statusCountsUnknown
	row.MemoryKnown = &statusMemoryUnknown
	row.Files, row.Nodes, row.Edges = 0, 0, 0
	row.Memory = daemon.MemoryBreakdown{}
	row.LastIndex = 0
}

func repoStatusCountsKnown(row daemon.TrackedRepoStatus) bool {
	return row.CountsKnown == nil || *row.CountsKnown
}

func repoStatusMemoryKnown(row daemon.TrackedRepoStatus) bool {
	return row.MemoryKnown == nil || *row.MemoryKnown
}

func workspaceStatusCountsKnown(row daemon.WorkspaceSummary) bool {
	return row.CountsKnown == nil || *row.CountsKnown
}

func workspaceSummaries(rows []daemon.TrackedRepoStatus) []daemon.WorkspaceSummary {
	bySlug := make(map[string]*daemon.WorkspaceSummary)
	keys := make([]string, 0)
	for _, row := range rows {
		if row.Unloaded {
			continue
		}
		summary, ok := bySlug[row.Workspace]
		if !ok {
			summary = &daemon.WorkspaceSummary{Slug: row.Workspace}
			bySlug[row.Workspace] = summary
			keys = append(keys, row.Workspace)
		}
		summary.Repos = append(summary.Repos, row.Prefix)
		if row.WorkspaceProject != "" && !containsStatusString(summary.Projects, row.WorkspaceProject) {
			summary.Projects = append(summary.Projects, row.WorkspaceProject)
		}
		if !repoStatusCountsKnown(row) {
			summary.CountsKnown = &statusCountsUnknown
			continue
		}
		summary.Files += row.Files
		summary.Nodes += row.Nodes
		summary.Edges += row.Edges
	}
	sort.Strings(keys)
	out := make([]daemon.WorkspaceSummary, 0, len(keys))
	for _, key := range keys {
		out = append(out, *bySlug[key])
	}
	return out
}

func containsStatusString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
