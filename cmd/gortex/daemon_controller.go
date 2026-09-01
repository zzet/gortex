package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/blame"
	"github.com/zzet/gortex/internal/churn"
	"github.com/zzet/gortex/internal/cochange"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/coverage"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/releases"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

// realController is the production daemon.Controller implementation. It
// wraps the MultiIndexer and ConfigManager so track/untrack/reload/status
// operations go through the same code paths the current `gortex mcp`
// command uses.
//
// Methods are serialized via a mutex — track/reload can race with status
// otherwise. The mutex is coarse; finer locking is a later optimization.
type realController struct {
	mu sync.Mutex
	// graph is assigned once when the controller is constructed and never
	// reassigned, so reading it requires no synchronisation — and must not
	// take mu. Taking mu for a handle that cannot change bought nothing and
	// cost everything: mu is held for the entire duration of a track /
	// reload / enrichment, so the "cheap probe path" the hooks depend on
	// (SearchSymbols) queued behind minutes of indexing. Measured, the
	// UserPromptSubmit probe exceeded its 800ms budget on 82.6% of turns.
	//
	// Mutating operations still take mu; they serialise indexer work, not
	// this pointer. If this field ever becomes reassignable, it needs an
	// atomic — not mu — for the same reason multiWatcher already uses one.
	graph         graph.Store
	indexer       *indexer.Indexer
	multiIndexer  *indexer.MultiIndexer
	configManager *config.ConfigManager
	// lifecycle owns every checkout lifecycle side effect. It is the same
	// instance the MCP tools drive, so a track over the control socket and a
	// track over MCP leave identical state behind. Nil only in tests that
	// build a controller by hand; the methods that need it say so.
	lifecycle *indexer.CheckoutLifecycle
	// viewMaterializer composes the routed stack a probing path reads. It
	// MUST be the one the rest of the stack materializes through: retirement
	// runs with that instance's lease manager as its in-use predicate, so a
	// probe leasing through a second manager would be invisible to the sweep
	// and could have its generations deleted mid-read. Nil leaves every probe
	// on the base corpus.
	viewMaterializer *graphview.Materializer

	// probeNudgeMu guards probeNudgedAt alone. It is deliberately not mu: the
	// whole point of the nudge is to be raised from the probe path, which
	// must never queue behind a track / reload / enrichment.
	probeNudgeMu  sync.Mutex
	probeNudgedAt map[string]time.Time
	// topologyNudgeMu owns the event-driven reconciliation single-flight.
	// Filesystem topology events must not share the probe's 30-second rate
	// limiter: the final event can be the removal of the watched directory,
	// so dropping it can leave a stale checkout until the hourly janitor.
	// A running family records one trailing pass, coalescing event bursts
	// without losing their final state.
	topologyNudgeMu sync.Mutex
	topologyNudges  map[string]*topologyNudgeState
	// probeReconcile is the reconciliation a probe asks for when a working
	// copy has no composed view. nil routes to the lifecycle's own per-family
	// path; tests substitute it to observe the debounce.
	probeReconcile func(familyID string)
	// checkoutStartupBuildStatus is the required-publication status lookup used
	// by TrackReadiness. Production leaves it nil and reads the lifecycle;
	// focused tests substitute it to pin pre-generation pending/failure states
	// without constructing a physical generation worker.
	checkoutStartupBuildStatus func(checkoutID string) (pending bool, failure string)
	// probeViewRevalidateBarrier is a deterministic test seam between leasing
	// a generation-backed view and re-reading its catalog binding. Production
	// leaves it nil. Keeping the seam here lets race regressions move a graph
	// or route at the exact boundary without sleeps or scheduler assumptions.
	probeViewRevalidateBarrier func()
	// topologyReconcile is the context-aware reconciliation a topology nudge
	// runs. Production leaves it nil and uses lifecycle; tests substitute it
	// to exercise retained dispatch teardown without replacing that context.
	topologyReconcile func(context.Context, string)
	// multiWatcher is an atomic pointer, not a mu-guarded field: the daemon's
	// teardown hook reads it, and reading it under mu is what kept `daemon
	// stop` queued behind a running track / reload / enrichment. One writer
	// (AttachWatcher, during warmup); read via watcher().
	multiWatcher   atomic.Pointer[indexer.MultiWatcher]
	watcherGateMu  sync.Mutex
	watcherClosing bool
	logger         *zap.Logger

	// liveRouter is the multi-server Router currently wired into the
	// dispatch path (nil for a local-only daemon with no roster).
	// localExecute + publishRouter let ReloadServers build and publish
	// a router live when the first remote is added after startup, or
	// tear it down when the last remote is removed — all without a
	// daemon restart. Guarded by mu.
	liveRouter    *daemon.Router
	localExecute  daemon.LocalExecutor
	publishRouter func(*daemon.Router)

	// toolSurface reports the active tool-surface preset + mode and the
	// per-workspace learned-promotion count for `gortex daemon status`.
	// Nil when the MCP server isn't wired (control-only daemon).
	toolSurface func() (preset, mode string, learned int)

	// ready flips to true once references are resolved and the graph is
	// queryable — find_usages / get_callers return complete results from
	// this point. The socket accepts connections before this; queries
	// against not-yet-resolved repos return partial results until ready.
	// warmupSeconds records how long the parse + resolve stage took.
	//
	// enriched flips to true once the slow semantic-enrichment pass and the
	// graph-wide derivation passes finish in the background, after ready.
	// Background timers that must not fight the enrichment pipeline for
	// shard locks (the periodic snapshotter) gate on enriched, not ready.
	// enrichSeconds records the full warmup duration.
	// referenceReady is the legacy parse/resolve half of readiness. A cold
	// daemon may finish that half with zero legacy jobs while configured Git
	// repositories are still building their exact dedicated views, so ready is
	// the conjunction of this bit and startupViews.complete().
	referenceReady atomic.Bool
	startupViews   atomic.Pointer[startupViewReadiness]
	ready          atomic.Bool
	warmupSeconds  atomic.Int64
	// referenceEnriched is the legacy parse/resolve/enrichment tail. The
	// externally visible enriched bit is its conjunction with the frozen exact
	// startup-view cohort, just like ready is the conjunction above. Keeping
	// the two facts separate prevents a future caller of MarkEnriched from
	// reintroducing the old false-complete window.
	referenceEnriched atomic.Bool
	enriched          atomic.Bool
	enrichSeconds     atomic.Int64

	// lastAggregate is the mutex-guarded half of the last status pass that
	// managed to take mu: the repo table, the workspace rollup and the rest
	// of what the indexer registry decides. A status caller that cannot get
	// mu inside its budget serves this instead of timing out — see Status.
	//
	// Atomic, deliberately not guarded by mu: the entire point of the cache
	// is to be readable while a minutes-long track holds mu.
	lastAggregate atomic.Pointer[statusAggregate]
	// lastRoutedRepos retains only the last successfully catalogued checkout
	// identities and their immutable status projection. A later catalog read
	// failure uses those identities to degrade routed rows without falsely
	// degrading unrelated legacy/non-Git rows. Snapshots are immutable after
	// publication, so concurrent status calls need no controller lock.
	lastRoutedRepos atomic.Pointer[routedRepoStatusSnapshot]
}

// startupViewReadiness is the frozen configured-Git cohort captured after
// checkout catalog seeding. Counts describe exact routed views, never corpus
// node counts; Failed is an exclusive terminal-attempt subset of Expected.
type startupViewReadiness struct {
	Expected    int
	Ready       int
	Building    int
	Failed      int
	ProbeErrors int
}

func (s startupViewReadiness) complete() bool {
	return s.Expected == 0 || s.Ready >= s.Expected
}

func (s startupViewReadiness) terminal() bool {
	return s.complete() || (s.Expected > 0 && s.Ready+s.Failed >= s.Expected)
}

func (c *realController) startupViewReadiness() startupViewReadiness {
	if c == nil {
		return startupViewReadiness{}
	}
	if snapshot := c.startupViews.Load(); snapshot != nil {
		return *snapshot
	}
	// No daemon startup cohort was installed. This is the legacy/controller
	// unit-test path and is equivalent to an expected cohort of zero.
	return startupViewReadiness{}
}

func (c *realController) setStartupViewReadiness(snapshot startupViewReadiness) {
	if c == nil {
		return
	}
	copy := snapshot
	c.startupViews.Store(&copy)
	c.recomputeReady()
	c.recomputeEnriched()
}

func (c *realController) statusWarmup() (string, *daemon.StartupViewsStatus) {
	if c == nil {
		return "", nil
	}
	snapshot := c.startupViewReadiness()
	var views *daemon.StartupViewsStatus
	if snapshot.Expected > 0 {
		views = &daemon.StartupViewsStatus{
			Expected: snapshot.Expected, Ready: snapshot.Ready,
			Building: snapshot.Building, Failed: snapshot.Failed,
			ProbeErrors: snapshot.ProbeErrors,
		}
	}
	switch {
	case c.ready.Load():
		return "ready", views
	case !c.referenceReady.Load():
		return "resolving_references", views
	case snapshot.Failed > 0:
		return "degraded", views
	case !snapshot.complete():
		return "checkout_builds_pending", views
	default:
		return "finalizing", views
	}
}

func (c *realController) recomputeReady() {
	if c == nil {
		return
	}
	c.ready.Store(c.referenceReady.Load() && c.startupViewReadiness().complete())
}

func (c *realController) recomputeEnriched() {
	if c == nil {
		return
	}
	c.enriched.Store(c.referenceEnriched.Load() && c.startupViewReadiness().complete())
}

// filterReadinessPhase keeps every workspace-readiness publisher honest. The
// warmup pipeline has several post-resolve phases that historically carried a
// hard-coded ready=true; a pending exact-view cohort must downgrade all of
// them until the transition worker publishes every configured Git route.
func (c *realController) filterReadinessPhase(
	phase string, ready bool, extra map[string]any,
) (string, bool, map[string]any) {
	if c == nil || !ready {
		return phase, ready, extra
	}
	snapshot := c.startupViewReadiness()
	if snapshot.complete() {
		return phase, c.IsReady(), extra
	}
	filtered := make(map[string]any, len(extra)+4)
	for key, value := range extra {
		filtered[key] = value
	}
	filtered["startup_views_expected"] = snapshot.Expected
	filtered["startup_views_ready"] = snapshot.Ready
	filtered["startup_views_building"] = snapshot.Building
	filtered["startup_views_failed"] = snapshot.Failed
	if snapshot.ProbeErrors > 0 {
		filtered["startup_views_probe_errors"] = snapshot.ProbeErrors
	}
	// Several legacy warmup callers supplied these facts before exact startup
	// views existed. At the combined workspace boundary they are false until
	// the frozen cohort is complete.
	filtered["queryable"] = false
	filtered["enriched"] = false
	if snapshot.Failed > 0 {
		return "degraded", false, filtered
	}
	return "checkout_builds_pending", false, filtered
}

// Track indexes a new repository and persists it to the global config.
// Path is resolved to an absolute form before the MultiIndexer sees it.
func (c *realController) Track(ctx context.Context, p daemon.TrackParams) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.multiIndexer == nil {
		return nil, fmt.Errorf("multi-repo indexer not initialized")
	}
	absPath, err := filepath.Abs(p.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	if c.lifecycle == nil {
		return nil, fmt.Errorf("checkout lifecycle not initialized")
	}
	entry := config.RepoEntry{Path: absPath, Name: p.Name, Ref: p.Ref, AsWorktree: p.AsWorktree}

	// Project association from TrackParams.Project isn't wired yet — the
	// config package doesn't expose an AddRepoToProject helper. Callers
	// who need project scoping can edit ~/.gortex/config.yaml and
	// run `gortex daemon reload`; track from the daemon-v1 surface just
	// adds to the top-level repo list.
	//
	// Everything else — the index, the catalog identity, the CLI tracking
	// intent, the watcher attach, the config flush and the session
	// invalidation — is the shared registration path, so this surface and
	// the MCP tool leave the same state behind.
	result, err := c.lifecycle.Register(ctx, entry, indexer.TrackSourceCLI)
	if err != nil {
		return nil, err
	}
	if result.CatalogErr != nil {
		c.logger.Warn("track: recording the checkout identity failed",
			zap.String("path", absPath), zap.Error(result.CatalogErr))
	}
	if result.AlreadyTracked {
		if err := c.lifecycle.EnsureTrackedWatcher(ctx, result.Prefix); err != nil {
			return nil, fmt.Errorf("repairing watcher for already tracked repository: %w", err)
		}
		// Already tracked — idempotent.
		return json.RawMessage(fmt.Sprintf(`{"status":"already_tracked","path":%q}`, absPath)), nil
	}

	return json.Marshal(map[string]any{
		"status":     "tracked",
		"path":       absPath,
		"prefix":     result.Prefix,
		"file_count": result.Index.FileCount,
		"node_count": result.Index.NodeCount,
		"edge_count": result.Index.EdgeCount,
	})
}

// EnrichChurn runs the churn enricher in-process against the daemon's
// graph. We hold c.mu for the duration so a concurrent Track/Untrack
// can't reshape the set of files while the enricher walks them. The
// caller (CLI / git hook) picks the params; an empty Path means "every
// tracked repo", an empty Branch means "resolve each repo's default
// branch from its working tree".
func (c *realController) EnrichChurn(ctx context.Context, p daemon.EnrichChurnParams) (daemon.EnrichChurnResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.graph == nil {
		return daemon.EnrichChurnResult{}, fmt.Errorf("graph not initialized")
	}
	if c.multiIndexer == nil {
		return daemon.EnrichChurnResult{}, fmt.Errorf("multi-repo indexer not initialized")
	}

	// Resolve the set of repo roots the call targets. Empty Path =
	// every tracked repo. A path or prefix narrows to one.
	type target struct {
		prefix string
		root   string
	}
	var targets []target
	want := strings.TrimSpace(p.Path)
	for prefix, meta := range c.multiIndexer.AllMetadata() {
		if want != "" && want != prefix && want != meta.RootPath {
			continue
		}
		targets = append(targets, target{prefix: prefix, root: meta.RootPath})
	}
	if len(targets) == 0 {
		return daemon.EnrichChurnResult{}, fmt.Errorf("no tracked repo matches %q", p.Path)
	}

	started := time.Now()
	var combined daemon.EnrichChurnResult
	for _, t := range targets {
		branch := strings.TrimSpace(p.Branch)
		if branch == "" {
			branch = gitDefaultBranch(t.root)
		}
		if branch == "" {
			c.logger.Warn("enrich churn: no default branch resolved",
				zap.String("prefix", t.prefix), zap.String("root", t.root))
			continue
		}
		res, err := churn.EnrichGraph(ctx, c.graph, t.root, churn.Options{Branch: branch})
		if err != nil {
			return daemon.EnrichChurnResult{}, fmt.Errorf("enrich %s: %w", t.prefix, err)
		}
		combined.Files += res.Files
		combined.Symbols += res.Symbols
		combined.Branch = res.Branch
		combined.HeadSHA = res.HeadSHA
	}
	combined.DurationMS = time.Since(started).Milliseconds()
	return combined, nil
}

// EnrichReleases runs the per-file release enricher against the
// daemon's graph. Mirrors EnrichChurn — c.mu is held for the duration,
// targets resolve via the multi-indexer, and an empty Branch lets
// each repo's default branch be resolved on demand (so feature-branch
// tags don't leak into the timeline).
func (c *realController) EnrichReleases(ctx context.Context, p daemon.EnrichReleasesParams) (daemon.EnrichReleasesResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.graph == nil {
		return daemon.EnrichReleasesResult{}, fmt.Errorf("graph not initialized")
	}
	if c.multiIndexer == nil {
		return daemon.EnrichReleasesResult{}, fmt.Errorf("multi-repo indexer not initialized")
	}

	type target struct {
		prefix string
		root   string
	}
	var targets []target
	want := strings.TrimSpace(p.Path)
	for prefix, meta := range c.multiIndexer.AllMetadata() {
		if want != "" && want != prefix && want != meta.RootPath {
			continue
		}
		targets = append(targets, target{prefix: prefix, root: meta.RootPath})
	}
	if len(targets) == 0 {
		return daemon.EnrichReleasesResult{}, fmt.Errorf("no tracked repo matches %q", p.Path)
	}
	_ = ctx // graph mutation is synchronous; no cancellation surface today

	started := time.Now()
	var combined daemon.EnrichReleasesResult
	for _, t := range targets {
		branch := strings.TrimSpace(p.Branch)
		if branch == "" {
			branch = gitDefaultBranch(t.root)
			// Empty branch is still legal — releases.EnrichGraphForBranch
			// treats "" as "every tag", which is the right default when
			// no default branch can be resolved (e.g. a clone without
			// origin/HEAD set yet).
		}
		count, err := releases.EnrichGraphForBranch(c.graph, t.root, t.prefix, branch)
		if err != nil {
			return daemon.EnrichReleasesResult{}, fmt.Errorf("enrich %s: %w", t.prefix, err)
		}
		combined.Files += count
		combined.Branch = branch
	}
	combined.DurationMS = time.Since(started).Milliseconds()
	return combined, nil
}

// enrichTarget is one (prefix, root) pair the enrichers run against.
type enrichTarget struct {
	prefix string
	root   string
}

// resolveEnrichTargets maps the caller-supplied path scope onto the set
// of tracked repos to enrich. An empty path means "every tracked repo";
// a non-empty path narrows to the one repo whose prefix or root matches.
// Returns an error when nothing matches so the control caller gets a
// clear "no tracked repo" message rather than a silent zero-count
// success. Caller must hold c.mu.
func (c *realController) resolveEnrichTargets(path string) ([]enrichTarget, error) {
	if c.graph == nil {
		return nil, fmt.Errorf("graph not initialized")
	}
	if c.multiIndexer == nil {
		return nil, fmt.Errorf("multi-repo indexer not initialized")
	}
	var targets []enrichTarget
	want := strings.TrimSpace(path)
	for prefix, meta := range c.multiIndexer.AllMetadata() {
		if meta == nil || meta.RootPath == "" {
			continue
		}
		if want != "" && want != prefix && want != meta.RootPath {
			continue
		}
		targets = append(targets, enrichTarget{prefix: prefix, root: meta.RootPath})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no tracked repo matches %q", path)
	}
	return targets, nil
}

// EnrichBlame runs the git-blame authorship enricher against the
// daemon's graph. Mirrors EnrichChurn — c.mu is held for the duration
// and targets resolve via the multi-indexer.
func (c *realController) EnrichBlame(_ context.Context, p daemon.EnrichBlameParams) (daemon.EnrichBlameResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	targets, err := c.resolveEnrichTargets(p.Path)
	if err != nil {
		return daemon.EnrichBlameResult{}, err
	}

	started := time.Now()
	var combined daemon.EnrichBlameResult
	for _, t := range targets {
		count, err := blame.EnrichGraph(c.graph, t.root)
		if err != nil {
			return daemon.EnrichBlameResult{}, fmt.Errorf("enrich %s: %w", t.prefix, err)
		}
		combined.Nodes += count
	}
	combined.DurationMS = time.Since(started).Milliseconds()
	return combined, nil
}

// EnrichCoverage projects the caller-parsed cover-profile segments onto
// the daemon's graph. The CLI parses the profile (the path is relative
// to the caller's cwd, not the daemon's), so the daemon only needs the
// segments and resolves each repo's module path from its working tree.
func (c *realController) EnrichCoverage(_ context.Context, p daemon.EnrichCoverageParams) (daemon.EnrichCoverageResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	targets, err := c.resolveEnrichTargets(p.Path)
	if err != nil {
		return daemon.EnrichCoverageResult{}, err
	}

	segments := make([]coverage.Segment, len(p.Segments))
	for i, s := range p.Segments {
		segments[i] = coverage.Segment{
			File:      s.File,
			StartLine: s.StartLine,
			EndLine:   s.EndLine,
			NumStmt:   s.NumStmt,
			Count:     s.Count,
		}
	}

	started := time.Now()
	var combined daemon.EnrichCoverageResult
	combined.Segments = len(segments)
	for _, t := range targets {
		modulePath := coverage.ReadModulePath(t.root)
		combined.Symbols += coverage.EnrichGraph(c.graph, segments, modulePath)
	}
	combined.DurationMS = time.Since(started).Milliseconds()
	return combined, nil
}

// EnrichCochange mines co-change edges against the daemon's graph.
// Mirrors EnrichChurn — c.mu is held for the duration and targets
// resolve via the multi-indexer. The repo prefix scopes the file-node
// match in multi-repo graphs.
func (c *realController) EnrichCochange(ctx context.Context, p daemon.EnrichCochangeParams) (daemon.EnrichCochangeResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	targets, err := c.resolveEnrichTargets(p.Path)
	if err != nil {
		return daemon.EnrichCochangeResult{}, err
	}
	_ = ctx // mining is synchronous; no cancellation surface today

	started := time.Now()
	var combined daemon.EnrichCochangeResult
	for _, t := range targets {
		count, err := cochange.EnrichGraph(c.graph, t.root, t.prefix)
		if err != nil {
			return daemon.EnrichCochangeResult{}, fmt.Errorf("enrich %s: %w", t.prefix, err)
		}
		combined.Edges += count
	}
	combined.DurationMS = time.Since(started).Milliseconds()
	return combined, nil
}

// Untrack evicts a repo from the graph and drops it from config.
// PathOrPrefix accepts either an absolute path or a repo prefix.
//
// What an untrack does is a property of the checkout's family, so the plan is
// read before anything is torn down and the destructive ones are shown rather
// than run. The control socket carries the same gate as the tool surface for
// one reason: an older CLI binary against a newer daemon sends nothing but a
// path, and a request it means as "drop this one checkout" must not become a
// retirement of the family's whole automatic lane because the daemon learned
// how to do that.
func (c *realController) Untrack(ctx context.Context, p daemon.UntrackParams) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.multiIndexer == nil {
		return nil, fmt.Errorf("multi-repo indexer not initialized")
	}
	if c.lifecycle == nil {
		return nil, fmt.Errorf("checkout lifecycle not initialized")
	}

	preview, err := c.lifecycle.PreviewUntrack(ctx, p.PathOrPrefix)
	if err != nil {
		return nil, err
	}
	if !p.Confirm && destructiveUntrackPlan(preview.Plan) {
		return json.Marshal(untrackPreviewPayload(preview))
	}

	// The lifecycle revokes every revocable tracking intent and runs the
	// plan's saga — which detaches the watcher before evicting from the graph
	// (a late fsnotify event must not race the eviction), then persists the
	// config and invalidates every session.
	result, err := c.lifecycle.ApplyUntrack(ctx, preview)
	if err != nil {
		return nil, err
	}

	status := "untracked"
	if result.Demoted {
		status = "demoted"
	}
	payload := map[string]any{
		"status":        status,
		"plan":          string(result.Plan),
		"prefix":        result.Prefix,
		"nodes_removed": result.NodesRemoved,
		"edges_removed": result.EdgesRemoved,
	}
	if len(result.Revoked) > 0 {
		payload["revoked_intents"] = result.Revoked
	}
	if len(result.Dependents) > 0 {
		payload["dependents"] = dependentDetails(result.Dependents)
	}
	return json.Marshal(payload)
}

// destructiveUntrackPlan reports whether a plan removes rows a caller has to be
// asked about. A plan that keeps the checkout — an eviction of a repository
// with no catalog identity, or a demotion into the family's automatic lane —
// is the ordinary untrack and runs as it always has.
func destructiveUntrackPlan(plan indexer.UntrackPlan) bool {
	switch plan {
	case indexer.UntrackPlanForget, indexer.UntrackPlanPrimaryClosure:
		return true
	default:
		return false
	}
}

// untrackPreviewPayload renders a plan that was shown instead of run.
func untrackPreviewPayload(preview indexer.UntrackPreview) map[string]any {
	payload := map[string]any{
		"status":           "preview",
		"plan":             string(preview.Plan),
		"prefix":           preview.Prefix,
		"is_primary":       preview.IsPrimary,
		"confirm_required": true,
		"detail": "nothing was written; repeat the untrack with confirm to run this plan, " +
			"or use the untrack_repository / forget_checkout tools",
	}
	if preview.IsPrimary {
		payload["sole_primary"] = preview.SolePrimary
	}
	if len(preview.Closure) > 0 {
		payload["closure"] = dependentDetails(preview.Closure)
	}
	if len(preview.Preserved) > 0 {
		payload["preserved"] = dependentDetails(preview.Preserved)
	}
	return payload
}

// dependentDetails flattens closure rows to the one-line statements the control
// socket has always carried.
func dependentDetails(dependents []reconcile.Dependent) []string {
	out := make([]string, 0, len(dependents))
	for _, dep := range dependents {
		out = append(out, dep.Detail)
	}
	return out
}

// Reload re-reads the global config, indexes new repos that were added
// via direct config-file edits, and untracks any that were removed.
// Existing, unchanged tracked repos keep their current state.
// ReloadServers re-reads servers.toml and applies the change to the
// running daemon's Router without a restart: an in-place atomic swap
// when a router already exists, a fresh build-and-publish when the first
// remote is added after a router-less startup, or a teardown when the
// last remote is removed.
func (c *realController) ReloadServers(_ context.Context) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	scfg, err := daemon.LoadServersConfig("")
	if err != nil {
		return nil, fmt.Errorf("reload servers.toml: %w", err)
	}
	count := 0
	if scfg != nil {
		count = len(scfg.Server)
	}

	wired := false
	switch {
	case count == 0 && c.liveRouter != nil:
		// Last remote removed — tear the router down so local dispatch
		// returns to the direct in-process path.
		c.liveRouter = nil
		if c.publishRouter != nil {
			c.publishRouter(nil)
		}
	case count == 0:
		// No router and no remotes — nothing to wire.
	case c.liveRouter != nil:
		// In-place atomic swap; the stable *Router pointer keeps every
		// dispatch site (and any in-flight call) consistent.
		c.liveRouter.ReloadConfig(scfg, daemon.NewWorkspaceRosterCache(60*time.Second))
		wired = true
	default:
		// First remote added after a router-less startup — build and
		// publish a fresh router into the dispatch path.
		c.liveRouter = daemon.NewRouter(daemon.RouterConfig{
			Servers:      scfg,
			Rosters:      daemon.NewWorkspaceRosterCache(60 * time.Second),
			LocalSlug:    daemon.LocalServerSentinel,
			LocalExecute: c.localExecute,
			Logger:       c.logger,
			Federation:   resolveFederationConfig(),
		})
		if c.publishRouter != nil {
			c.publishRouter(c.liveRouter)
		}
		wired = true
	}
	return json.Marshal(map[string]any{"servers": count, "router_wired": wired})
}

func (c *realController) Reload(ctx context.Context) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.configManager == nil {
		return nil, fmt.Errorf("config manager not initialized")
	}
	if err := c.configManager.Reload(); err != nil {
		return nil, fmt.Errorf("reload config: %w", err)
	}
	if c.lifecycle == nil {
		return nil, fmt.Errorf("checkout lifecycle not initialized")
	}

	// The diff itself is unchanged — configured entries are matched to
	// tracked instances by root path, never by a recomputed prefix — but both
	// halves now run through the lifecycle: an added entry gets the same
	// identity, watcher and invalidation an explicit track would give it, and
	// a removed one goes through the reconciler's retirement rule instead of
	// a direct eviction, so a config edit can no longer delete a corpus that
	// nothing else can serve.
	result, err := c.lifecycle.ApplyReload(ctx)
	if err != nil {
		return nil, err
	}

	return json.Marshal(map[string]any{
		"added":     result.Added,
		"removed":   result.Removed,
		"pending":   result.Pending,
		"refreshed": result.Refreshed,
	})
}

// trigramCacheForResponse snapshots the process-wide trigram searcher
// cache for `daemon status`. It is reported separately from
// SearchBackend because the two are different indexes with different
// lifetimes: the symbol backend can be disk-resident while this one holds
// the largest in-memory structure in the daemon.
func trigramCacheForResponse() *daemon.TrigramCacheStats {
	s := indexer.TrigramCacheSnapshot()
	return &daemon.TrigramCacheStats{
		Live:      s.Live,
		MaxLive:   s.MaxLive,
		Bytes:     s.Bytes,
		MaxBytes:  s.MaxBytes,
		IdleTTLMs: s.IdleTTL.Milliseconds(),
		BuildsOff: s.BuildsOff,
		Evictions: s.Evictions,
	}
}

// searchBackendInfo bundles the daemon.SearchBackendStats payload with
// the separate text/vector byte counts we need to split per-repo.
type searchBackendInfo struct {
	daemon.SearchBackendStats
	vectorBytes uint64
}

// resolveSearchBackend inspects the live search backend and produces
// the stats needed by status rendering: which backend is active, total
// document count, and its heap footprint.
//
// Real-world unwrap order: Swappable → HybridBackend → (text, vector).
// Both layers have to be peeled; if we stop early we fall into the
// default branch and the status reports "unknown" — which was the bug
// users saw. The text side is a concrete backend that has to be matched
// explicitly, or it lands in that same "unknown" default: the indexer
// wires up a *search.SymbolSearcherBackend when the store implements
// graph.SymbolSearcher and a *search.NullBackend when it does not (see
// initialSearchBackend in internal/indexer/indexer.go).
func resolveSearchBackend(b search.Backend) searchBackendInfo {
	out := searchBackendInfo{}
	if b == nil {
		return out
	}

	// 1) Pin Swappable so the inspected backend cannot be retired while
	// status derives its type, counts, and sizes.
	inner := b
	if sw, ok := inner.(*search.Swappable); ok {
		var release func()
		inner, release = sw.AcquireBackend()
		defer release()
	}
	// 2) If Hybrid is in play, split its text/vector sizes and keep
	//    drilling into the text side for name/doc-count identification.
	if hyb, ok := inner.(*search.HybridBackend); ok {
		out.vectorBytes = hyb.VectorSizeBytes()
		inner = hyb.TextBackend()
		// TextBackend() itself could be a Swappable in some setups. Pin it
		// too so a nested replacement cannot invalidate this inspection.
		if sw, ok := inner.(*search.Swappable); ok {
			var release func()
			inner, release = sw.AcquireBackend()
			defer release()
		}
	}

	switch back := inner.(type) {
	case *search.SymbolSearcherBackend:
		// The FTS5 index lives inside the graph store's own file, not a
		// separate in-memory structure — there is no honest byte count
		// to report here. Report the backend truthfully as disk-resident
		// instead of printing a fabricated "heap=0 B".
		//
		// The document count comes from the index itself, never from
		// Count(): that is a since-construction Add/Remove delta, which
		// is not a corpus size and goes negative as soon as an eviction
		// path drops more than the admit predicate ever added. When the
		// index cannot answer, the figure is omitted rather than filled
		// in with the delta.
		out.Name = "sqlite-fts5"
		out.DiskResident = true
		out.DocCount, out.DocCountKnown = back.DocCount()
	case *search.NullBackend:
		// A store with no native symbol search gets the null text
		// backend: it indexes nothing and the engine falls back to its
		// substring path. Say so — an empty backend is a fact worth
		// reporting, whereas the "unknown" default would imply we failed
		// to recognise whatever is serving queries. Doc count and heap
		// stay zero because both are honestly zero.
		out.Name = "none"
		out.DocCountKnown = true
	default:
		out.Name = "unknown"
		out.DocCount = inner.Count()
		out.DocCountKnown = true
		out.Bytes = search.BackendSize(inner)
	}
	return out
}

// Status gathers per-repo stats and basic process metrics. Daemon-level
// fields (PID, uptime, socket, session count) are filled in by the
// daemon itself before the response goes out.
// Probe answers "is the daemon up, is it ready, and what does it track"
// without taking c.mu and without reading the store.
//
// This is the whole point of the call. Status takes c.mu, and c.mu is held
// for the entire duration of a track / reload / enrichment — the same
// reasoning that already keeps multiWatcher off that mutex and keeps Shutdown
// as a lock-free acknowledgement.
// Measured on a 44-repo workspace, serial Status calls with no concurrent
// load returned the identical payload in 156 ms to 11.5 s depending only on
// what the indexer was doing. Callers on a sub-second budget therefore
// concluded the daemon was unreachable while it was perfectly healthy.
//
// So: no c.mu, no graph handle, no runtime.ReadMemStats, and no per-repo
// stat. ready/enriched are already atomics, and the tracked-repo registry is
// the config — which is the membership source of truth anyway, since a
// status row for a repo the daemon holds no index for is itself synthesised
// from it.
//
// Anything added here that reads the store or takes a lock reintroduces
// exactly the stall this exists to avoid; TestProbeAnswersDuringLongMutation
// pins that.
func (c *realController) Probe(ctx context.Context) (daemon.ProbeResponse, error) {
	if err := ctx.Err(); err != nil {
		return daemon.ProbeResponse{}, err
	}

	resp := daemon.ProbeResponse{
		Ready:    c.ready.Load(),
		Enriched: c.enriched.Load(),
	}
	if c.configManager == nil {
		return resp, nil
	}
	gc := c.configManager.Global()
	if gc == nil {
		return resp, nil
	}

	seen := make(map[string]bool)
	add := func(e config.RepoEntry) {
		path := strings.TrimSpace(e.Path)
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		resp.TrackedRepos = append(resp.TrackedRepos, daemon.ProbeRepo{
			Path:      path,
			Prefix:    config.ResolvePrefix(e),
			Name:      e.Name,
			Workspace: e.Workspace,
			Project:   e.Project,
		})
	}
	for _, e := range gc.Repos {
		add(e)
	}
	// Project-nested entries are tracked the same as top-level ones. Omitting
	// them would report a tracked path as untracked, which downstream reads as
	// "no repo owns this" — the precise misread this call exists to prevent.
	for _, pc := range gc.Projects {
		for _, e := range pc.Repos {
			add(e)
		}
	}
	return resp, nil
}

// StatusExact recounts the per-repo estimates from the stored nodes and
// edges, writes the result back over any counter that had drifted, and then
// reports status the ordinary way — so the numbers it returns are measured,
// and the next cheap poll is measured too.
//
// This is the escape hatch for the routine path's one assumption: that the
// counters the indexer persists still describe the corpus. Answering that
// question costs a full scan (tens of seconds on a large store), which is
// exactly why it is a flag and not the default.
func (c *realController) StatusExact(ctx context.Context) (daemon.StatusResponse, error) {
	g := c.graph // write-once at construction; see the field comment
	scanner, ok := graph.Store(g).(graph.RepoMemoryEstimateScanner)
	if !ok || !c.enriched.Load() {
		// Nothing to reconcile against — an in-memory backend maintains its
		// counters on every mutation, and a graph that has not finished
		// enriching has no counters worth auditing yet. Say so: a user who
		// asked for measured numbers should not have to guess whether the
		// ones they got were measured.
		if c.logger != nil {
			c.logger.Info("daemon: exact status served from maintained counters",
				zap.Bool("backend_can_recount", ok),
				zap.Bool("enriched", c.enriched.Load()))
		}
		return c.status(ctx, true)
	}

	scanned, err := scanner.ScanRepoMemoryEstimates(ctx)
	if err != nil {
		// A scan that ran out of the caller's budget has no numbers to
		// report. Say so rather than quietly serving the counters the user
		// explicitly asked to bypass.
		return daemon.StatusResponse{}, fmt.Errorf("exact repo counts: %w", err)
	}
	if reconciler, ok := graph.Store(g).(interface {
		ReconcileRepoCounters(map[string]graph.RepoMemoryEstimate) error
	}); ok {
		if err := reconciler.ReconcileRepoCounters(scanned); err != nil {
			return daemon.StatusResponse{}, fmt.Errorf("reconcile repo counters: %w", err)
		}
	}
	return c.status(ctx, true)
}

// Status answers within the caller's budget even while the controller mutex
// is held. See status: the aggregate half may be served from the last
// successful pass, marked as such on the response.
func (c *realController) Status(ctx context.Context) (daemon.StatusResponse, error) {
	return c.status(ctx, false)
}

// status assembles a status response. waitForAggregate distinguishes the two
// contracts: the routine pass (false) gives the controller mutex a slice of
// the budget and falls back to the last aggregate it computed, while the
// exact pass (true) waits for the mutex — a caller that paid for a full
// recount asked for measured numbers, and a cached table is not that.
func (c *realController) status(ctx context.Context, waitForAggregate bool) (daemon.StatusResponse, error) {
	// Bail before doing any work if the caller is already gone. Status sits
	// on the critical path of `daemon stop`, `gortex call`, and the agent
	// hooks, all of which now bound the round trip — once their budget has
	// expired, finishing the aggregate only burns store reads that nobody
	// will read.
	if err := ctx.Err(); err != nil {
		return daemon.StatusResponse{}, err
	}

	// Compute exact per-repo estimates only after enrichment. On SQLite,
	// AllRepoMemoryEstimates plus NodeCount/EdgeCount are four corpus-wide
	// scans; status and health polling must not compete with warmup for the
	// four-reader pool. While warming, RepoMetadata below supplies stable
	// advisory counts and byte estimates intentionally remain zero. Once
	// enriched, snapshot the graph handle under a brief lock and run the
	// exact (store-memoised) estimates without holding the controller mutex.
	g := c.graph // write-once at construction; see the field comment
	enriched := c.enriched.Load()
	var memEstimates map[string]graph.RepoMemoryEstimate
	var wholeStoreNodes, wholeStoreEdges int
	if g != nil && enriched {
		memEstimates = g.AllRepoMemoryEstimates()
		// NodeCount/EdgeCount share that profile: COUNT(*) on the SQLite
		// backend, a walk of every shard in memory. They were computed
		// under c.mu until the same stall showed up on the whole-store
		// path, so they are hoisted out alongside the estimate.
		//
		// They are also only read on the sole-repo path below, where the
		// single row reports whole-store totals so status agrees with
		// `query stats`. A multi-repo daemon was paying for both scans —
		// measured 2.25s and 2.03s on a 9.4 GB store, unbounded and growing
		// with the corpus — to fill in one field of a conditional log line.
		// One counter row per indexed repo is enough to tell the two cases
		// apart, and it is already in hand.
		if len(memEstimates) <= 1 {
			wholeStoreNodes = g.NodeCount()
			wholeStoreEdges = g.EdgeCount()
		}
	}

	// runtime.ReadMemStats stops the world. Under reindex allocation churn
	// that pause is long enough to matter, and holding c.mu across it makes
	// every queued control request wait out the pause as well.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	// Stat every configured repo path before taking c.mu. A stat is cheap
	// on a live path and on a deleted one, but an unresponsive network
	// mount can block for seconds, and no status caller should be able to
	// wedge track / untrack / reload behind a dead NFS handle.
	configRepos, repoMissing := c.trackedRepoLiveness()

	// The view census is catalog listings bounded by the number of families,
	// graphs and generations, so it belongs in the slow half rather than
	// under the mutex the coordinators contend for.
	views := c.collectViewsStatus(ctx)
	// The per-repo routed projection is the row-level companion to the census:
	// it selects the exact checkout stack and reads only persisted generation
	// counters/ownership metadata. It must happen outside c.mu for the same
	// reason, and it never recounts graph rows.
	routedRepos := c.collectRoutedRepoStatuses(ctx)

	// Everything above is the slow half and c.mu below is the contended half.
	// Re-check between them: a caller whose budget expired during the scans
	// must not go on to queue behind a mutex held by a minutes-long track.
	if err := ctx.Err(); err != nil {
		return daemon.StatusResponse{}, err
	}

	agg, cached, err := c.statusAggregateFor(ctx, waitForAggregate, statusAggregateInput{
		enriched:        enriched,
		memEstimates:    memEstimates,
		wholeStoreNodes: wholeStoreNodes,
		wholeStoreEdges: wholeStoreEdges,
		repoMissing:     repoMissing,
	})
	if err != nil {
		return daemon.StatusResponse{}, err
	}

	// Reconcile the live indexer registry against the tracked-repo registry
	// in the global config, so `daemon status` and `gortex repos` report one
	// inventory instead of two that can drift apart (#312). A repo whose
	// directory was deleted while the daemon was down fails startup indexing
	// and never reaches AllMetadata — it would silently vanish from this
	// response while `gortex repos`, which reads the config, kept listing it.
	// Synthesised rows carry zero counts and are appended AFTER the workspace
	// rollup, which summarises indexed content only.
	//
	// Both inputs were read without the controller mutex, so this half still
	// answers on a pass that had to serve a cached aggregate: a daemon in its
	// first track reports the repos it is about to index rather than none.
	// The copy keeps the cached slice immutable — it is shared by every
	// subsequent busy pass.
	tracked := append([]daemon.TrackedRepoStatus(nil), agg.tracked...)
	projected := projectRoutedRepoRows(tracked, routedRepos)
	tracked = append(tracked, reconcileUnloadedRepos(configRepos, repoMissing, tracked, routedRepos)...)
	// Workspace totals must be reduced after routed rows replace generation-zero
	// shells. Otherwise the repo table would say "counts unavailable" while its
	// workspace silently published the shell's zero as an exact total.
	workspaces := workspaceSummaries(tracked)
	searchBackend := agg.searchBackend
	if projected > 0 || (routedRepos.enabled && len(routedRepos.rows) > 0) {
		// The process search backend is pinned to one physical generation. It
		// cannot report a coherent document count across several selected
		// checkout views, so omit the number instead of presenting gen-0's count
		// as the routed corpus size.
		searchBackend.DocCount = 0
		searchBackend.DocCountKnown = false
	}

	// mem was sampled before the mutex was taken — see the note at the top
	// of status.

	resp := daemon.StatusResponse{
		TrackedRepos:   tracked,
		MemoryBytes:    mem.Alloc,
		SearchBackend:  searchBackend,
		TrigramCache:   trigramCacheForResponse(),
		GraphIntegrity: daemon.GraphIntegrityStatusFor(g),
		Runtime: daemon.RuntimeStats{
			Alloc:        mem.Alloc,
			Sys:          mem.Sys,
			HeapInuse:    mem.HeapInuse,
			HeapIdle:     mem.HeapIdle,
			HeapReleased: mem.HeapReleased,
			StackInuse:   mem.StackInuse,
			NumGC:        mem.NumGC,
			NumGoroutine: runtime.NumGoroutine(),
		},
		PProfAddr:          daemonPProfAddr(),
		Ready:              c.ready.Load(),
		WarmupSeconds:      c.warmupSeconds.Load(),
		EnrichmentComplete: enriched,
		EnrichSeconds:      c.enrichSeconds.Load(),
		Workspaces:         workspaces,
		ConfiguredServers:  agg.configuredServers,
		LocalServerSlug:    agg.localServerSlug,
		LSPRouter:          agg.lspRouter,
		Enrichment:         agg.enrichment,
		Views:              views,
		ToolPreset:         agg.toolPreset,
		ToolPresetMode:     agg.toolPresetMode,
		LearnedTools:       agg.learnedTools,
	}
	resp.WarmupPhase, resp.StartupViews = c.statusWarmup()
	if cached {
		// Say which half is a snapshot. Without the marker a stale repo
		// table is indistinguishable from a current one, and an empty one
		// reads as "nothing is tracked" rather than "not computed yet".
		resp.AggregateBusy = true
		if !agg.takenAt.IsZero() {
			resp.AggregateCachedUnix = agg.takenAt.Unix()
		}
	}
	return resp, nil
	// MCPSessions is populated by the daemon Server (it owns the
	// SessionRegistry — the controller doesn't have a back-pointer).
	// See internal/daemon/server.go around the ControlStatus handler.
}

// statusAggregate is the half of a status response that only the controller
// mutex can produce: the per-repo table and everything derived from the
// indexer registry behind it. Status caches the last one it managed to
// compute, so a caller arriving while a track holds the mutex gets that
// instead of a timeout.
//
// Treat a stored aggregate as immutable — every busy pass shares the one
// pointer, so a reader that appends to its slices corrupts the next answer.
type statusAggregate struct {
	takenAt           time.Time
	tracked           []daemon.TrackedRepoStatus
	workspaces        []daemon.WorkspaceSummary
	searchBackend     daemon.SearchBackendStats
	configuredServers []daemon.ConfiguredServerStatus
	localServerSlug   string
	lspRouter         *daemon.LSPRouterStatus
	enrichment        *daemon.EnrichmentProgress
	toolPreset        string
	toolPresetMode    string
	learnedTools      int
}

// statusAggregateInput carries the lock-free half of the computation into the
// aggregate pass: the corpus-wide estimates and the repo-path liveness map are
// deliberately gathered before the mutex is taken (see status).
type statusAggregateInput struct {
	enriched        bool
	memEstimates    map[string]graph.RepoMemoryEstimate
	wholeStoreNodes int
	wholeStoreEdges int
	repoMissing     map[string]bool
}

// statusLockPoll is how often a status caller retries the controller mutex
// while it waits for it. sync.Mutex has no context-aware acquire, so the wait
// is a poll: fine-grained enough to pick the mutex up as soon as a track
// releases it, coarse enough to cost nothing over a whole budget.
const statusLockPoll = 5 * time.Millisecond

// statusLockWait caps how long the routine status pass waits for the mutex
// before serving its last aggregate. A mutex that has not come free in this
// long is held by something long — a track, a reload, an enrichment — and
// every further second of waiting trades the caller's whole budget for a
// shrinking chance at a fresh repo table.
const statusLockWait = 2 * time.Second

// statusLockReserve is the slice of the caller's remaining budget kept back
// from the wait. The daemon abandons a control handler the instant its budget
// expires (Server.handleControlBounded), so a wait that runs to the deadline
// produces exactly the timeout it was meant to prevent.
const statusLockReserve = 250 * time.Millisecond

// statusAggregateFor produces the mutex-guarded half of a status response,
// reporting whether it had to be served from the last successful pass.
//
// wait is the StatusExact contract: hold out for the mutex until the caller's
// context ends, and report that expiry rather than substituting a snapshot.
func (c *realController) statusAggregateFor(ctx context.Context, wait bool, in statusAggregateInput) (*statusAggregate, bool, error) {
	if wait {
		if err := lockContext(ctx, &c.mu, 0); err != nil {
			return nil, false, err
		}
		return c.storeStatusAggregate(in), false, nil
	}

	budget := statusLockWait
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline) - statusLockReserve; remaining < budget {
			budget = remaining
		}
	}
	switch {
	case budget <= 0:
		// Not enough budget left to wait in: the answer has to be assembled
		// now. One attempt still catches a free mutex.
		if c.mu.TryLock() {
			return c.storeStatusAggregate(in), false, nil
		}
	default:
		if err := lockContext(ctx, &c.mu, budget); err == nil {
			return c.storeStatusAggregate(in), false, nil
		}
	}

	// The mutex is held by a long operation. Everything else in the response
	// is live; this half is the last one that was computable. A daemon that
	// has never finished a pass has none, and reports an empty aggregate
	// under the same marker — status must degrade, never fail, or the one
	// call that explains a busy daemon is the one the busy daemon eats.
	if agg := c.lastAggregate.Load(); agg != nil {
		return agg, true, nil
	}
	return &statusAggregate{}, true, nil
}

// storeStatusAggregate computes the aggregate under the already-held mutex,
// publishes it as the new last-good snapshot, and releases the mutex.
func (c *realController) storeStatusAggregate(in statusAggregateInput) *statusAggregate {
	agg := c.buildStatusAggregate(in)
	// Publish before releasing, so mu orders the stores: a pass descheduled
	// between building and publishing cannot overwrite a newer snapshot with
	// its own older one.
	c.lastAggregate.Store(agg)
	c.mu.Unlock()
	return agg
}

// lockContext acquires mu without letting the caller's budget expire in the
// queue. budget caps the wait when positive; the context bounds it either way.
//
// sync.Mutex cannot be acquired against a context, so this polls TryLock. A
// caller with nothing that can end its wait blocks outright instead: waiting
// is what it asked for, and it is cheaper than spinning for the length of a
// track.
func lockContext(ctx context.Context, mu *sync.Mutex, budget time.Duration) error {
	if mu.TryLock() {
		return nil
	}
	if ctx.Done() == nil && budget <= 0 {
		mu.Lock()
		return nil
	}
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	ticker := time.NewTicker(statusLockPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if mu.TryLock() {
				return nil
			}
		}
	}
}

// buildStatusAggregate assembles the per-repo table and its rollups. Callers
// must hold c.mu: it reads the indexer registry that track / untrack / reload
// mutate.
func (c *realController) buildStatusAggregate(in statusAggregateInput) *statusAggregate {
	enriched := in.enriched
	memEstimates := in.memEstimates
	repoMissing := in.repoMissing
	wholeStoreNodes, wholeStoreEdges := in.wholeStoreNodes, in.wholeStoreEdges

	var (
		tracked                  []daemon.TrackedRepoStatus
		searchBackendForResponse daemon.SearchBackendStats
		totalNodes               int
	)
	if c.multiIndexer != nil {
		// memEstimates (per-repo node/edge counts + byte estimates) was
		// computed above, before the controller mutex was taken — see the
		// note at the top of Status. The SQLite store memoises it so a
		// burst of status polls collapses onto one COUNT … GROUP BY scan;
		// the in-memory store serves maintained shard counters directly.

		// Every tracked repo carries a prefix, so every one of them has a
		// live per-prefix bucket in memEstimates. That is what makes this
		// block a plain lookup.
		//
		// It used to be ~130 lines of compensation. A lone repo indexed
		// WITHOUT a prefix, and both backends make an empty-prefix bucket
		// invisible to a lookup keyed by the repo's real prefix — the
		// in-memory shard counters skip empty-prefix nodes (shard.repoNodeAdd
		// is a deliberate no-op) and the SQLite GROUP BY excludes
		// repo_prefix="" rows. So status fell back to a frozen
		// RepoMetadata.NodeCount, went stale the moment the graph changed
		// under it, and rendered the near-empty row of #261/#270. Two
		// separate reconstructions existed to undo that: whole-store
		// attribution for a sole repo, and an "unaccounted pool" heuristic
		// for a multi-repo workspace holding one empty-bucket repo.
		allMeta := c.multiIndexer.AllMetadata()
		soleRepo := len(allMeta) == 1
		// wholeStoreNodes / wholeStoreEdges were computed above, before the
		// controller mutex was taken — see the note at the top of Status.

		// Diagnostic: a repo the indexer has produced nodes for, with no
		// counter bucket, means some path cleared the per-repo counters
		// without clearing the underlying nodes. The meta fallback below
		// keeps the table usable in the meantime.
		//
		// Comparing bucket count against tracked count — the earlier
		// shape — reads a repo that has simply not finished its first
		// index as a defect, because a repo earns its counter row by
		// completing one. Requiring meta.NodeCount > 0 asks the question
		// that was actually meant: this repo has content, so where did its
		// counter go?
		if enriched && c.logger != nil {
			var missing []string
			for prefix, meta := range allMeta {
				if meta == nil || meta.NodeCount == 0 {
					continue
				}
				if _, ok := memEstimates[prefix]; !ok {
					missing = append(missing, prefix)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				c.logger.Warn("daemon: indexed repos missing per-repo counters — graph mutation cleared per-repo index?",
					zap.Strings("repos", missing),
					zap.Int("tracked_repos", len(allMeta)),
					zap.Int("counter_buckets", len(memEstimates)))
			}
		}

		// Search and vector backends are process-wide (one shared index
		// across all repos), so we compute the global size once and
		// split it proportionally to each repo's node share. Not exact,
		// but it's the best attribution we can make without indexing
		// per-repo which would double storage for the sake of a status
		// breakdown.
		backendStats := resolveSearchBackend(c.multiIndexer.Search())
		// totalNodes drives the SearchBytes share split below. A sole repo
		// owns the whole store; otherwise sum the per-prefix buckets and, if
		// every counter is empty, fall back to per-repo meta so the share
		// denominator stays nonzero and the search budget gets attributed
		// instead of falling on the floor.
		if soleRepo && enriched && wholeStoreNodes > 0 {
			totalNodes = wholeStoreNodes
		} else {
			for _, est := range memEstimates {
				totalNodes += est.NodeCount
			}
			if totalNodes == 0 {
				for _, meta := range allMeta {
					if meta != nil {
						totalNodes += meta.NodeCount
					}
				}
			}
		}

		for prefix, meta := range allMeta {
			nodes := meta.NodeCount
			edges := meta.EdgeCount
			var mem daemon.MemoryBreakdown
			switch {
			case soleRepo && enriched && wholeStoreNodes > 0:
				// A single tracked repo owns the entire store — including
				// the handful of synthetic global externals that belong to
				// no repo — so reporting whole-store totals keeps `daemon
				// status` in agreement with `gortex query stats`, which is
				// the consistency users reported missing in #261/#270. Byte
				// estimates stay zero: advisory detail, not the bug.
				nodes = wholeStoreNodes
				edges = wholeStoreEdges
			default:
				if est, ok := memEstimates[prefix]; ok {
					nodes = est.NodeCount
					edges = est.EdgeCount
					mem.NodesBytes = est.NodeBytes
					mem.EdgesBytes = est.EdgeBytes
				}
			}
			if totalNodes > 0 && nodes > 0 {
				share := float64(nodes) / float64(totalNodes)
				mem.SearchBytes = uint64(float64(backendStats.Bytes) * share)
				mem.VectorsBytes = uint64(float64(backendStats.vectorBytes) * share)
			}
			mem.TotalBytes = mem.NodesBytes + mem.EdgesBytes + mem.SearchBytes + mem.VectorsBytes

			// Pull the workspace/project slugs straight off the
			// per-repo Indexer — that's the source of truth that
			// stamps every node emitted by this repo. Falls back to
			// the prefix on legacy setups where no .gortex.yaml
			// declares them (the resolveWorkspaceID default).
			var ws, wsProj string
			if idx := c.multiIndexer.GetIndexer(prefix); idx != nil {
				ws = idx.WorkspaceID()
				wsProj = idx.ProjectID()
			}
			if ws == "" {
				ws = prefix
			}
			if wsProj == "" {
				wsProj = prefix
			}

			tracked = append(tracked, daemon.TrackedRepoStatus{
				Prefix:           prefix,
				Path:             meta.RootPath,
				Workspace:        ws,
				WorkspaceProject: wsProj,
				Files:            meta.FileCount,
				Nodes:            nodes,
				Edges:            edges,
				LastIndex:        meta.LastIndexTime.Unix(),
				Memory:           mem,
				Missing:          lookupRepoMissing(repoMissing, meta.RootPath),
			})
		}
		searchBackendForResponse = backendStats.SearchBackendStats
	}

	// Aggregate per-workspace stats so the renderer can emit a
	// "workspaces" block. Hidden when every repo defaults to its own
	// slug (the legacy single-workspace-per-repo case where the
	// summary just duplicates the table).
	wsAgg := make(map[string]*daemon.WorkspaceSummary)
	wsKeys := make([]string, 0)
	for _, r := range tracked {
		s, ok := wsAgg[r.Workspace]
		if !ok {
			s = &daemon.WorkspaceSummary{Slug: r.Workspace}
			wsAgg[r.Workspace] = s
			wsKeys = append(wsKeys, r.Workspace)
		}
		s.Repos = append(s.Repos, r.Prefix)
		seenProj := false
		for _, p := range s.Projects {
			if p == r.WorkspaceProject {
				seenProj = true
				break
			}
		}
		if !seenProj {
			s.Projects = append(s.Projects, r.WorkspaceProject)
		}
		s.Files += r.Files
		s.Nodes += r.Nodes
		s.Edges += r.Edges
	}
	// Always populate the per-workspace rollup — even when every
	// workspace is a default singleton. Hiding it on legacy setups
	// makes the boundary feature invisible, which is the opposite
	// of what users want when they're trying to migrate. Renderer-
	// side compaction (single-line hint vs full table) keeps the
	// output tidy when there's nothing meaningful to summarise.
	sort.Strings(wsKeys)
	workspaces := make([]daemon.WorkspaceSummary, 0, len(wsKeys))
	for _, k := range wsKeys {
		workspaces = append(workspaces, *wsAgg[k])
	}

	agg := &statusAggregate{
		takenAt:           time.Now(),
		tracked:           tracked,
		workspaces:        workspaces,
		searchBackend:     searchBackendForResponse,
		configuredServers: c.collectConfiguredServers(),
		localServerSlug:   c.localServerSlug(),
		lspRouter:         c.collectLSPRouterStatus(),
		enrichment:        c.collectEnrichmentProgress(),
	}
	if c.toolSurface != nil {
		agg.toolPreset, agg.toolPresetMode, agg.learnedTools = c.toolSurface()
	}
	return agg
}

// trackedRepoLiveness snapshots the configured repo registry and stats
// every entry's path, returning the entries alongside a path-keyed
// "directory is gone" map. Called before Status takes c.mu so a wedged
// filesystem can't block the control mutex.
func (c *realController) trackedRepoLiveness() ([]config.RepoEntry, map[string]bool) {
	if c.configManager == nil {
		return nil, nil
	}
	registrations := c.configManager.RepoRegistrations()
	entries := make([]config.RepoEntry, len(registrations))
	for i := range registrations {
		// Status and warmup must agree on physical identity. In particular, a
		// project-only alias is one durable registration, not an absent global
		// repo plus a synthetic row under the alias spelling.
		entries[i] = registrations[i].Entry
		entries[i].Path = registrations[i].CanonicalPath
	}
	missing := make(map[string]bool, len(entries))
	for _, e := range entries {
		missing[e.Path] = config.RepoPathMissing(e.Path)
	}
	return entries, missing
}

// lookupRepoMissing answers the liveness question for one repo root,
// preferring the pre-stat'd map. A root the map doesn't cover (an
// indexer registration with no matching config entry) is stat'd
// directly rather than assumed live — reporting a deleted repo as
// present is the bug, so the fallback errs toward answering.
func lookupRepoMissing(missing map[string]bool, root string) bool {
	if root == "" {
		return false
	}
	if gone, ok := missing[root]; ok {
		return gone
	}
	for path, gone := range missing {
		if pathkey.EqualPaths(path, root) {
			return gone
		}
	}
	return config.RepoPathMissing(root)
}

// reconcileUnloadedRepos returns a synthetic row for every configured
// repo the daemon holds no index for. Matching is by path first (what
// track resolves and persists) and by resolved prefix second, so a repo
// registered under a derived worktree-instance prefix still matches its
// config entry.
func reconcileUnloadedRepos(
	entries []config.RepoEntry,
	missing map[string]bool,
	loaded []daemon.TrackedRepoStatus,
	routed routedRepoStatusSnapshot,
) []daemon.TrackedRepoStatus {
	if len(entries) == 0 {
		return nil
	}
	var out []daemon.TrackedRepoStatus
	for _, e := range entries {
		prefix := config.ResolvePrefix(e)
		isLoaded := false
		for _, r := range loaded {
			if (e.Path != "" && pathkey.EqualPaths(r.Path, e.Path)) || (prefix != "" && r.Prefix == prefix) {
				isLoaded = true
				break
			}
		}
		if isLoaded {
			continue
		}
		row := daemon.TrackedRepoStatus{
			Prefix:           prefix,
			Path:             e.Path,
			Name:             e.Name,
			Workspace:        prefix,
			WorkspaceProject: prefix,
			Ref:              e.Ref,
			Unloaded:         true,
			Missing:          lookupRepoMissing(missing, e.Path),
		}
		if view, found := matchRoutedRepoStatus(e.Path, prefix, routed.rows); found {
			if routed.available {
				applyRoutedRepoStatus(&row, view)
			} else {
				applyUnavailableRoutedRepoStatus(&row)
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Prefix < out[j].Prefix })
	return out
}

// collectConfiguredServers reads `~/.gortex/servers.toml` (best
// effort — a missing or malformed file just returns nil) and
// projects it onto the status response. Auth tokens are NOT
// included; the HasAuth flag is enough for the human-facing
// "yes/no" decision.
func (c *realController) collectConfiguredServers() []daemon.ConfiguredServerStatus {
	cfg, err := daemon.LoadServersConfig("")
	if err != nil || cfg == nil || len(cfg.Server) == 0 {
		return nil
	}
	local := c.localServerSlug()
	out := make([]daemon.ConfiguredServerStatus, 0, len(cfg.Server))
	for _, s := range cfg.Server {
		out = append(out, daemon.ConfiguredServerStatus{
			Slug:       s.Slug,
			URL:        s.URL,
			Default:    s.Default,
			Local:      s.Slug == local,
			Workspaces: s.Workspaces,
			HasAuth:    s.AuthToken != "" || s.AuthTokenEnv != "",
		})
	}
	return out
}

// localServerSlug returns the reserved sentinel identifying the
// daemon's own in-process graph. It is intentionally NOT derived from
// DefaultServer().Slug: a roster row is always a remote now, so no
// roster entry is ever "local" (the status Local flag is false for
// every row), and a remote marked default=true is still proxied.
func (c *realController) localServerSlug() string {
	return daemon.LocalServerSentinel
}

// collectLSPRouterStatus reflects the daemon's LSP router (when
// wired) into a status payload. Returns nil when no router is wired
// (semantic enrichment disabled in `.gortex.yaml`).
func (c *realController) collectLSPRouterStatus() *daemon.LSPRouterStatus {
	if c.indexer == nil {
		return nil
	}
	semMgr := c.indexer.SemanticManager()
	if semMgr == nil {
		return nil
	}
	router, ok := semMgr.LSPRouter().(*lsp.Router)
	if !ok || router == nil {
		return nil
	}
	out := &daemon.LSPRouterStatus{
		DefaultWorkspace: router.DefaultWorkspace(),
		MaxAlive:         router.MaxAlive(),
		Evictions:        router.EvictionCount(),
	}
	for _, name := range router.EnabledSpecNames() {
		out.EnabledSpecs = append(out.EnabledSpecs, daemon.LSPSpecStatus{
			Name:      name,
			Available: router.SpecAvailable(name),
			Languages: strings.Join(router.SpecLanguages(name), ","),
		})
	}
	for _, s := range router.Stats() {
		out.ActiveProviders = append(out.ActiveProviders, daemon.LSPActiveProvider{
			Spec:      s.Spec,
			Workspace: s.Workspace,
			LastUsed:  s.LastUsed.Format(time.RFC3339),
			InUse:     s.InUse,
		})
	}
	return out
}

// collectViewsStatus reflects the checkout-view lifecycle census into the
// status payload.
//
// It runs before the controller mutex is taken, like every other listing
// Status assembles, and it is skipped outright once the caller's budget has
// expired — the block is a report, and a status call that is already out of
// time should not spend its last milliseconds on one. A catalog that cannot
// be read yields nil for the same reason: the rest of the answer is still
// true, and an omitted block reads as "not available" while a zeroed one
// would read as "nothing exists".
func (c *realController) collectViewsStatus(ctx context.Context) *daemon.ViewsStatus {
	if c == nil || c.lifecycle == nil || ctx.Err() != nil {
		return nil
	}
	health, err := c.lifecycle.ViewsHealth(ctx)
	if err != nil {
		if c.logger != nil {
			c.logger.Debug("daemon: view lifecycle census unavailable", zap.Error(err))
		}
		return nil
	}
	return &daemon.ViewsStatus{
		Families:     health.Families,
		Checkouts:    health.Checkouts,
		Coordinators: health.Coordinators,
		BuildQueue: &daemon.ViewBuildQueueStatus{
			Open:              health.BuildQueue.Open,
			RequiredOpen:      health.BuildQueue.RequiredOpen,
			Active:            health.BuildQueue.Active,
			RequiredQueued:    health.BuildQueue.RequiredQueued,
			InteractiveQueued: health.BuildQueue.InteractiveQueued,
			BackgroundQueued:  health.BuildQueue.BackgroundQueued,
			RequiredLimit:     health.BuildQueue.RequiredLimit,
			InteractiveLimit:  health.BuildQueue.InteractiveLimit,
			BackgroundLimit:   health.BuildQueue.BackgroundLimit,
			RequiredHigh:      health.BuildQueue.RequiredHighWater,
			InteractiveHigh:   health.BuildQueue.InteractiveHighWater,
			BackgroundHigh:    health.BuildQueue.BackgroundHighWater,
			MaxWaitMillis:     health.BuildQueue.MaxWait.Milliseconds(),
			WaitSamples:       health.BuildQueue.WaitSamples,
		},
		Generations: health.Generations,
		Leases:      health.Leases,
		RefViews:    health.RefViews,
		Counters:    health.Counters,
	}
}

// collectEnrichmentProgress reflects the semantic manager's per-(repo,
// provider) enrichment statuses into the compact summary the daemon
// status line needs. Returns nil when no semantic manager is wired, or
// it has never recorded a pass — the "enrichment in progress" state
// with nothing behind it is exactly the bug this closes.
func (c *realController) collectEnrichmentProgress() *daemon.EnrichmentProgress {
	if c.indexer == nil {
		return nil
	}
	semMgr := c.indexer.SemanticManager()
	if semMgr == nil {
		return nil
	}
	return enrichmentProgressFromStatuses(semMgr.EnrichmentStatuses())
}

// enrichmentProgressFromStatuses is the pure reduction behind
// collectEnrichmentProgress, split out so it can be unit tested
// against literal semantic.EnrichmentStatus rows without wiring a
// live indexer + semantic manager. A repo counts as done once every
// provider recorded for it has reached a terminal state; the first
// running row (in the manager's stable repo/provider order) becomes
// Current.
func enrichmentProgressFromStatuses(statuses []semantic.EnrichmentStatus) *daemon.EnrichmentProgress {
	if len(statuses) == 0 {
		return nil
	}

	out := &daemon.EnrichmentProgress{}
	repoDone := make(map[string]bool)
	repoSeen := make(map[string]bool)
	for _, st := range statuses {
		repoSeen[st.Repo] = true
		if _, ok := repoDone[st.Repo]; !ok {
			repoDone[st.Repo] = true // assume done until a non-terminal provider says otherwise
		}
		switch st.State {
		case semantic.EnrichStateRunning:
			out.Running = true
			repoDone[st.Repo] = false
			if out.Current == nil {
				cur := &daemon.EnrichmentCurrent{
					Repo:            st.Repo,
					Provider:        st.Provider,
					DeadlineSeconds: st.DeadlineSeconds,
				}
				if !st.StartedAt.IsZero() {
					cur.ElapsedSeconds = time.Since(st.StartedAt).Seconds()
				}
				out.Current = cur
			}
		}
	}
	out.ReposTotal = len(repoSeen)
	for _, done := range repoDone {
		if done {
			out.ReposDone++
		}
	}
	return out
}

// searchSymbolsFetchFactor bounds how many name-index candidates
// SearchSymbols pulls before the File/Import and repo filters run. The name
// index cannot express those filters, so pushing the caller's limit down
// verbatim can return a page saturated with rows the filter then drops.
// Over-fetching absorbs that without giving up the bound; a repo-scoped
// probe over-fetches harder because one repo can be a thin slice of a
// multi-repo workspace.
const (
	searchSymbolsFetchFactor     = 8
	searchSymbolsRepoFetchFactor = 32
	searchSymbolsMaxFetch        = 2000
)

// SearchSymbols runs a substring match over node names and returns the
// matching symbols. It's the cheap probe path for clients (notably the
// Grep-redirect hook) that need a fast yes/no without setting up a full
// MCP session. File and Import nodes are excluded — the hook only cares
// about real symbol matches.
//
// Candidates come from the sharded name index — one shard read-locked at a
// time, with the fetch bound pushed down — not from AllNodes. AllNodes
// read-locks every shard at once and materialises a slice of the entire
// graph, so on a multi-repo daemon this probe queued behind the indexer's
// shard writers and blew past the hook's 200ms budget. The hook then logged
// probed_miss / timed_out and never once produced a hit.
//
// Path scopes the probe to the graph that path reads through. Without one the
// base corpus answers and the response carries no view block at all, which is
// what every client that predates routed views sends and still receives.
func (c *realController) SearchSymbols(ctx context.Context, p daemon.SearchSymbolsParams) (daemon.SearchSymbolsResult, error) {
	// No mu: graph is write-once at construction (see the field comment), and
	// this is the probe path a hook calls on a sub-second budget. Taking mu
	// here is what made it wait out an in-flight reindex.
	var g graph.Reader = c.graph

	if g == nil || p.Query == "" {
		return daemon.SearchSymbolsResult{}, nil
	}

	// The lease is held for exactly as long as the answer is being built: the
	// hits are copied out of the composed reader below, so nothing the caller
	// receives outlives the generations that produced it.
	view := c.resolveProbeView(ctx, p.Path)
	defer view.release()
	if !view.servable {
		// A registered working copy with no composed view. Reporting the
		// primary's symbols would cite another working copy's code as
		// evidence about this one.
		return daemon.SearchSymbolsResult{Hits: []daemon.SymbolHit{}, View: view.answer}, nil
	}
	if view.reader != nil {
		g = view.reader
	}
	if p.Repo == "" {
		p.Repo = view.searchScope
	}

	limit := p.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	hits := make([]daemon.SymbolHit, 0, limit)

	// Fast path. Everything that reaches this probe is a grep pattern the
	// hook already classified as identifier-shaped, and the overwhelmingly
	// common answer is a symbol whose short name matches exactly. byName is
	// a hash bucket per shard, so this is a handful of map lookups rather
	// than a walk over every name in the graph — the difference between
	// microseconds and blowing the hook's probe budget on a large graph.
	for _, n := range g.FindNodesByName(p.Query) {
		if !probeSymbolCandidate(n, p.Repo) {
			continue
		}
		hits = append(hits, probeSymbolHit(n))
		if len(hits) >= limit {
			break
		}
	}
	if len(hits) > 0 {
		return daemon.SearchSymbolsResult{Hits: hits, View: view.answer}, nil
	}

	// Substring fallback, for patterns that name part of a symbol. The name
	// index cannot express the File/Import or repo filters, so a saturated
	// page can come back with every row filtered away — a query matching
	// many file paths crowds real symbols off it. Widen and retry rather
	// than under-report, but stay bounded: a few rounds capped at
	// searchSymbolsMaxFetch, still orders of magnitude below a full scan.
	//
	// The explicit Contains re-check preserves the pre-index semantics
	// exactly (full Unicode case folding, where SQLite's LIKE folds ASCII
	// only).
	needle := strings.ToLower(p.Query)
	fetch := limit * searchSymbolsFetchFactor
	if p.Repo != "" {
		fetch = limit * searchSymbolsRepoFetchFactor
	}
	for {
		if fetch > searchSymbolsMaxFetch {
			fetch = searchSymbolsMaxFetch
		}
		candidates := g.FindNodesByNameContaining(p.Query, fetch)
		hits = hits[:0]
		for _, n := range candidates {
			if !probeSymbolCandidate(n, p.Repo) {
				continue
			}
			if !strings.Contains(strings.ToLower(n.Name), needle) {
				continue
			}
			hits = append(hits, probeSymbolHit(n))
			if len(hits) >= limit {
				break
			}
		}
		// Enough hits, the index is exhausted, or the bound is reached.
		if len(hits) >= limit || len(candidates) < fetch || fetch >= searchSymbolsMaxFetch {
			break
		}
		fetch *= 4
	}
	return daemon.SearchSymbolsResult{Hits: hits, View: view.answer}, nil
}

// probeSymbolCandidate reports whether n can answer a symbol probe. File and
// Import nodes are named for paths rather than code, so they must never make
// a grep pattern look like a real symbol; a repo-scoped probe additionally
// keeps only that repo's nodes.
func probeSymbolCandidate(n *graph.Node, repo string) bool {
	if n == nil {
		return false
	}
	if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
		return false
	}
	if repo != "" && n.RepoPrefix != repo {
		return false
	}
	return true
}

func probeSymbolHit(n *graph.Node) daemon.SymbolHit {
	return daemon.SymbolHit{
		Name:     n.Name,
		Kind:     string(n.Kind),
		FilePath: n.FilePath,
		Line:     n.StartLine,
	}
}

// AttachWatcher is called by warmup to hand over the MultiWatcher once
// it has been initialized. Until this is called, the lifecycle skips the
// per-repo watcher attach — a newly-tracked repo gets its watcher when the
// warmup-constructed MultiWatcher iterates mi.AllMetadata() at startup.
//
// The lifecycle reads the watcher through this same pointer, so every
// surface's attach and detach hit the one live watcher.
func (c *realController) AttachWatcher(mw *indexer.MultiWatcher) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.AttachWatcherContext(ctx, mw); err != nil {
		c.logger.Warn("daemon: watcher attachment reconciliation was incomplete", zap.Error(err))
	}
}

// AttachWatcherContext publishes a live watcher and repairs every durable
// explicit repository that may have been tracked after the warmup snapshot.
// Per-prefix failures are returned for observability after all healthy prefixes
// have been attempted; the durable transition journal remains retryable.
func (c *realController) AttachWatcherContext(ctx context.Context, mw *indexer.MultiWatcher) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.lifecycle.SetWatcherSource(func() indexer.RepoWatcher {
		// A typed nil in an interface is not nil; return the untyped one so
		// the lifecycle's "is there a watcher yet" test stays honest.
		if live := c.watcher(); live != nil {
			return live
		}
		return nil
	})
	if mw == nil {
		c.watcherGateMu.Lock()
		if !c.watcherClosing {
			c.multiWatcher.Store(nil)
		}
		c.watcherGateMu.Unlock()
		return nil
	}
	// Install the topology consumer before publication. A concurrent Track
	// that observes the pointer can then safely start and dispatch a watcher.
	mw.OnWorktreeChangeContext(func(dispatchCtx context.Context, repoPrefix, rootPath string) {
		dispatchCtx = withTopologyReconcileSource(dispatchCtx, rootPath)
		resolveCtx, cancel := context.WithTimeout(dispatchCtx, 10*time.Second)
		familyID, err := c.lifecycle.ResolveFamilyID(resolveCtx, repoPrefix)
		cancel()
		if err != nil {
			c.logger.Debug("worktree topology event could not resolve family",
				zap.String("repo", repoPrefix), zap.Error(err))
			return
		}
		retainedCtx, release := mw.RetainTopologyDispatch(dispatchCtx)
		c.nudgeFamilyTopologyRequest(retainedCtx, familyID, release)
	})
	c.watcherGateMu.Lock()
	if c.watcherClosing {
		c.watcherGateMu.Unlock()
		_ = mw.StopContext(ctx)
		return indexer.ErrIndexerClosed
	}
	c.multiWatcher.Store(mw)
	c.watcherGateMu.Unlock()

	// Repair durable watcher sources before catalog seeding. Healthy sources
	// then contribute their exact dispatch context, while families whose source
	// truly disappeared are still covered by the durable catalog pass below.
	attachErr := c.lifecycle.EnsureConfiguredWatchers(ctx)

	// Registration snapshots only contain sources that still exist. Seed the
	// same single-flight from durable catalog ownership as well, so a primary
	// that disappeared before attachment is reconciled and forgotten rather
	// than surviving until a janitor pass. Reconciliation resolves the probe
	// source at execution time, after watcher repair has converged.
	familyIDs, familyErr := c.lifecycle.CatalogSeedFamilyIDs(ctx)
	if familyErr == nil {
		for _, familyID := range familyIDs {
			// Catalog seeding is not an admitted watcher callback and must not
			// retain a watcher dispatch lease. Its handler can remove the last
			// watcher in this family, which synchronously waits for those leases
			// to drain. The nudge request detaches seedCtx itself, so discovery
			// survives the attachment call without creating that self-cycle.
			seedCtx := withTopologyReconcileSource(ctx, "catalog")
			c.nudgeFamilyTopologyRequest(seedCtx, familyID, nil)
		}
	}

	resumeErr := c.lifecycle.ResumePendingTransitions(ctx)
	return errors.Join(attachErr, familyErr, resumeErr)
}

// watcher reads the attached MultiWatcher without touching the coarse mutex.
// The teardown path needs it, and taking mu there would put the stop request
// back behind the minutes-long track / reload / enrichment it is trying to end.
func (c *realController) watcher() *indexer.MultiWatcher {
	return c.multiWatcher.Load()
}

// StopWatcher seals watcher publication, detaches the stable pointer, and
// synchronously drains filesystem/topology callbacks. Daemon teardown calls it
// after warmup/readiness producers join (warmup is the pointer's publisher)
// and before lifecycle/backend close.
//
// It never takes the coarse controller mutex: doing so would put daemon stop
// behind the long track/reload/enrichment operation it is trying to end.
func (c *realController) StopWatcher() {
	c.watcherGateMu.Lock()
	c.watcherClosing = true
	mw := c.multiWatcher.Swap(nil)
	c.watcherGateMu.Unlock()
	if mw != nil {
		_ = mw.Stop()
	}
}

// MarkReady flips the ready flag once references are resolved and the graph
// is queryable, recording how long the parse + resolve stage took. Safe to
// call concurrently with Status (atomic loads on the read side).
func (c *realController) MarkReady(d time.Duration) {
	c.warmupSeconds.Store(int64(d.Seconds()))
	c.referenceReady.Store(true)
	c.recomputeReady()
}

// IsReady reports whether the graph is resolved and queryable. The socket
// accepts connections before this; callers waiting to issue queries should
// wait for IsReady.
func (c *realController) IsReady() bool {
	return c.ready.Load()
}

// MarkEnriched flips the enrichment-complete flag once semantic enrichment
// and the graph-wide derivation passes finish in the background, recording
// the full warmup duration. It also completes the reference half of readiness,
// so the degenerate path where MarkReady was skipped remains usable; it cannot
// bypass a configured-Git startup cohort whose exact views are still building.
func (c *realController) MarkEnriched(d time.Duration) {
	c.enrichSeconds.Store(int64(d.Seconds()))
	c.referenceEnriched.Store(true)
	c.referenceReady.Store(true)
	c.recomputeReady()
	c.recomputeEnriched()
}

// IsEnriched reports whether the background enrichment + derivation passes
// have finished. Background timers (the periodic snapshotter) gate on this
// rather than IsReady so they don't fight the enrichment pipeline for shard
// locks and GC budget.
func (c *realController) IsEnriched() bool {
	return c.enriched.Load()
}

// Shutdown acknowledges the control request without taking the controller's
// coarse mutex. The daemon server writes that acknowledgement, closes its
// transports, and joins request handlers before runDaemonStart tears down the
// graph stack after Serve returns.
//
// Doing graph teardown here is unsafe: this method itself runs in a request
// handler, and other handlers can still be using the store.
func (c *realController) Shutdown(_ context.Context) error {
	return nil
}
