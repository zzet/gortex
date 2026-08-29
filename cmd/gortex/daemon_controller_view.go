package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// probeView is the graph one path-scoped control probe reads through, plus
// what the answer says about it.
//
// Every field is derived from the catalog, never guessed: a probe that cannot
// establish which graph serves a path says so instead of answering from a
// graph that happens to be at hand.
type probeView struct {
	// answer rides on the wire. nil when the request carried no path, which
	// is what keeps an older client's response byte for byte what it was.
	answer *daemon.ProbeView
	// reader is the composed routed stack. nil means the base corpus.
	reader graph.Reader
	// repoPrefix is the corpus the path's content lives under. It keys the
	// file lookup and filters the nodes a coverage answer counts.
	repoPrefix string
	// searchScope narrows a symbol probe to one repo prefix. It is empty for
	// a dedicated checkout and for an untracked path, because narrowing them
	// would change the answer those callers already get.
	searchScope string
	// root is the working copy the path is relative to, empty when no
	// checkout owns it.
	root string
	// servable reports that something can answer for this path at all. It is
	// false only for a registered automatic checkout with no composed view
	// yet: nothing has been built that describes that working copy, and the
	// primary's content describes a different one.
	servable bool
	// release drops the lease a materialized view holds. Never nil.
	release func()
}

// noopRelease is the release of a view that leased nothing.
func noopRelease() {}

// probeReconcileDebounce bounds how often one family is reconciled on a
// probe's behalf. A PreToolUse hook probes once per tool call, so an agent
// working inside an unrouted worktree would otherwise raise the same
// reconciliation dozens of times a minute while the first one still runs.
const probeReconcileDebounce = 30 * time.Second

type topologyNudgeLease struct {
	once    sync.Once
	release func()
}

type topologyNudgeRequest struct {
	ctx   context.Context
	lease *topologyNudgeLease
}

func newTopologyNudgeRequest(ctx context.Context, release func()) topologyNudgeRequest {
	if ctx == nil {
		ctx = context.Background()
	}
	request := topologyNudgeRequest{ctx: context.WithoutCancel(ctx)}
	if release != nil {
		request.lease = &topologyNudgeLease{release: release}
	}
	return request
}

func (request *topologyNudgeRequest) finish() {
	if request == nil || request.lease == nil {
		return
	}
	request.lease.once.Do(request.lease.release)
}

type topologyNudgeState struct {
	pending *topologyNudgeRequest
}

// resolveProbeView decides which graph answers a probe about path.
//
// The order is the catalog's: a path no checkout owns and a checkout served
// from its own corpus both read the base, exactly as they did before routed
// views existed. Only an automatic checkout — one served from its family's
// shared lane — has a composed view, and only when its route names both
// generations. Anything short of that is reported rather than approximated.
func (c *realController) resolveProbeView(ctx context.Context, path string) probeView {
	view := c.selectProbeView(ctx, path)
	recordProbeAnswer(view.answer)
	return view
}

// recordProbeAnswer counts one path-scoped probe by the kind of view that
// answered and whether that view was the path's own.
//
// A probe that names no view at all — a daemon with no view catalog — is not
// counted: it has no view model to have an opinion with, and folding it into
// the base bucket would make a daemon without routed views look like one whose
// worktrees keep falling back.
func recordProbeAnswer(answer *daemon.ProbeView) {
	if answer == nil {
		return
	}
	exact := viewmetrics.AnswerFallback
	if answer.Exact {
		exact = viewmetrics.AnswerExact
	}
	viewmetrics.Count(viewmetrics.ProbeAnswerTotal, answer.Kind, exact)
}

// selectProbeView is resolveProbeView's decision, split out so every path
// through it is counted exactly once.
func (c *realController) selectProbeView(ctx context.Context, path string) probeView {
	base := probeView{servable: true, release: noopRelease}
	if c == nil || path == "" || c.lifecycle == nil || c.viewMaterializer == nil {
		// No view catalog is wired, so there are no composed views to route to
		// and the indexed corpus is the only graph there is. The answer names
		// no view at all rather than claiming the base is the path's own: this
		// daemon has no view model to have an opinion with.
		return base
	}

	binding, err := c.lifecycle.ExplainView(ctx, path)
	if err != nil {
		// The binding is an optimization over the base corpus, not a
		// precondition for answering. The path may well sit inside a routed
		// checkout, so the degradation rides on the answer rather than
		// passing for the path's own view.
		base.answer = fallbackProbeView(daemon.ProbeViewBase, "", "", daemon.FallbackCheckoutInaccessible)
		return base
	}

	if binding.Matched && binding.CheckoutState != string(store_sqlite.CheckoutStateReady) {
		// Availability and removal grace are read-only fallbacks even for a
		// checkout that was dedicated. The path is not live, so presenting its
		// retained corpus as an exact working-copy view would make stale data
		// indistinguishable from the checkout itself.
		return probeView{
			answer:      fallbackProbeView(daemon.ProbeViewBase, binding.CheckoutID, binding.RepoPrefix, binding.CheckoutState),
			repoPrefix:  binding.RepoPrefix,
			searchScope: binding.RepoPrefix,
			root:        binding.RootPath,
			servable:    true,
			release:     noopRelease,
		}
	}

	if !binding.Matched || binding.EffectiveMode != string(store_sqlite.CheckoutModeAutomatic) {
		// A live dedicated checkout, the family primary, and every untracked
		// path are read from the indexed corpus directly, unscoped, exactly as
		// they were before routed views existed.
		base.answer = exactProbeView(daemon.ProbeViewBase, binding.CheckoutID, binding.RepoPrefix)
		base.repoPrefix = binding.RepoPrefix
		base.root = binding.RootPath
		return base
	}

	if binding.Composed {
		view, viewErr := c.viewMaterializer.MaterializeCheckout(ctx, binding.CheckoutID)
		if viewErr == nil {
			return probeView{
				answer:      exactProbeView(daemon.ProbeViewWorktree, binding.CheckoutID, binding.RepoPrefix),
				reader:      view.Reader,
				repoPrefix:  binding.RepoPrefix,
				searchScope: binding.RepoPrefix,
				root:        binding.RootPath,
				servable:    true,
				release:     view.Close,
			}
		}
		if c.logger != nil {
			c.logger.Debug("probe view: could not materialize the checkout's view",
				zap.String("checkout", binding.CheckoutID), zap.Error(viewErr))
		}
	}

	// Nothing composed answers this working copy. Ask for a reconciliation so
	// a later probe can, and answer without waiting on it — a probe that
	// blocked on a build would cost the agent the latency the hook exists to
	// save.
	c.nudgeFamily(binding.FamilyID)

	if binding.CheckoutState == string(store_sqlite.CheckoutStateReady) {
		// Registered, live, and unrouted: the view is still being built (or
		// has not been asked for yet). Reporting the primary's content here
		// would describe a different working copy, so nothing is reported.
		return probeView{
			answer:   fallbackProbeView(daemon.ProbeViewUnrouted, binding.CheckoutID, binding.RepoPrefix, daemon.FallbackViewBuilding),
			root:     binding.RootPath,
			servable: false,
			release:  noopRelease,
		}
	}

	// Availability or removal grace: the working copy itself stopped
	// answering, and the family primary serves it by the same fallback rule a
	// read-only query follows. The checkout state is the reason, so a caller
	// logging it sees which grace window is running.
	return probeView{
		answer:      fallbackProbeView(daemon.ProbeViewBase, binding.CheckoutID, binding.RepoPrefix, binding.CheckoutState),
		repoPrefix:  binding.RepoPrefix,
		searchScope: binding.RepoPrefix,
		root:        binding.RootPath,
		servable:    true,
		release:     noopRelease,
	}
}

// exactProbeView names a graph that is the path's own.
func exactProbeView(kind, checkoutID, repoPrefix string) *daemon.ProbeView {
	return &daemon.ProbeView{
		Kind:       kind,
		CheckoutID: checkoutID,
		RepoPrefix: repoPrefix,
		Exact:      true,
	}
}

// fallbackProbeView names a graph that is not the path's own, with the reason
// it stood in. Exact is false by construction: there is no way to build one of
// these that claims to be exact.
func fallbackProbeView(kind, checkoutID, repoPrefix, reason string) *daemon.ProbeView {
	return &daemon.ProbeView{
		Kind:           kind,
		CheckoutID:     checkoutID,
		RepoPrefix:     repoPrefix,
		Exact:          false,
		FallbackReason: reason,
	}
}

// FileCoverage answers whether the graph serving Path holds definition
// symbols for it, and how many.
//
// It is the coverage verdict a PreToolUse hook turns into a deny, so the
// answer is scoped to the view the probing path actually reads: a worktree
// served by its family's automatic lane is answered from its composed view,
// and one whose view is not built yet is answered as uncovered so the hook
// allows the native tool through.
func (c *realController) FileCoverage(ctx context.Context, p daemon.FileCoverageParams) (daemon.FileCoverageResult, error) {
	if c == nil || p.Path == "" {
		return daemon.FileCoverageResult{}, nil
	}
	abs := p.Path
	if resolved, err := filepath.Abs(abs); err == nil {
		abs = resolved
	}

	view := c.resolveProbeView(ctx, abs)
	defer view.release()

	out := daemon.FileCoverageResult{View: view.answer}
	if !view.servable {
		return out, nil
	}
	reader := view.reader
	if reader == nil {
		reader = c.graph
	}
	if reader == nil {
		return out, nil
	}
	prefix, key, ok := c.fileGraphKey(abs, view)
	if !ok {
		return out, nil
	}
	for _, n := range reader.GetFileNodes(key) {
		// The file and import nodes ride on the by-file index for other
		// walkers; the coverage question is "what does this file define".
		if !probeSymbolCandidate(n, prefix) {
			continue
		}
		out.Symbols++
	}
	out.Covered = out.Symbols > 0
	return out, nil
}

// fileGraphKey renders an absolute path the way the graph spells a file key:
// the repo prefix, then the path relative to the working copy's root.
//
// A path the daemon has no root for cannot be keyed, and a path that escapes
// the root it was measured against is not in that repository at all. Both
// report ok=false, which the caller reads as "not covered" rather than
// guessing a key.
func (c *realController) fileGraphKey(abs string, view probeView) (prefix, key string, ok bool) {
	prefix, root := view.repoPrefix, view.root
	if root == "" {
		prefix, root, ok = c.trackedRoot(abs)
		if !ok {
			return "", "", false
		}
	}
	if prefix == "" {
		// A checkout the catalog knows but whose graph row names no prefix
		// still has a corpus its root belongs to.
		prefix = c.lifecycle.ResolvePrefix(root)
	}
	rel, ok := pathRelativeTo(root, abs)
	if !ok {
		return "", "", false
	}
	if prefix == "" {
		return "", rel, true
	}
	return prefix, prefix + "/" + rel, true
}

// pathRelativeTo measures abs against root, retrying with symlinks resolved
// when the two are spelled differently.
//
// Git spells a worktree root with its symlinks resolved and a caller spells
// its path the way the shell handed it over — which on macOS is a path through
// /var into /private/var. Comparing one spelling only reports a file inside the
// checkout as outside it, which reads as "not covered" for a file that plainly
// is. The file itself need not exist: only its directory is resolved, so a
// path naming a file about to be created still measures correctly.
func pathRelativeTo(root, abs string) (string, bool) {
	if rel, ok := relativeUnder(root, abs); ok {
		return rel, true
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	realAbs := abs
	if dir, dirErr := filepath.EvalSymlinks(filepath.Dir(abs)); dirErr == nil {
		realAbs = filepath.Join(dir, filepath.Base(abs))
	}
	return relativeUnder(realRoot, realAbs)
}

// relativeUnder returns abs relative to root, or ok=false when abs is not
// inside it.
func relativeUnder(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return rel, true
}

// trackedRoot finds the tracked repository root that contains abs, longest
// match first so a repository nested inside another resolves to the inner one.
func (c *realController) trackedRoot(abs string) (prefix, root string, ok bool) {
	if c.multiIndexer == nil {
		return "", "", false
	}
	for candidate, meta := range c.multiIndexer.AllMetadata() {
		if meta == nil || meta.RootPath == "" {
			continue
		}
		metaRoot, err := filepath.Abs(meta.RootPath)
		if err != nil {
			continue
		}
		if !pathkey.HasPathPrefix(abs, metaRoot) {
			continue
		}
		if len(metaRoot) > len(root) {
			prefix, root = candidate, metaRoot
		}
	}
	return prefix, root, root != ""
}

// nudgeFamily asks for one family's reconciliation on a probe's behalf,
// debounced per family and never on the probe's own goroutine.
//
// The probe is the first thing that notices a working copy nobody has routed
// yet — the janitor's tick is an hour away — but it is also the call an agent
// waits on before every tool use. Asking and returning is the only shape that
// serves both: the reconciliation runs through the lifecycle's own per-family
// path, and this probe answers from what exists right now.
func (c *realController) nudgeFamily(familyID string) {
	if c == nil || familyID == "" {
		return
	}
	run := c.probeReconcile
	if run == nil {
		if c.lifecycle == nil {
			return
		}
		run = c.reconcileFamilyForProbe
	}
	if !c.claimFamilyNudge(familyID) {
		return
	}
	go run(familyID)
}

// nudgeFamilyTopology reconciles a filesystem topology event immediately.
//
// GitWatcher has already debounce-coalesced the fsnotify burst. This layer
// single-flights duplicate watchers in the same family and remembers one
// trailing pass when an event arrives during a reconciliation. Remembering the
// trailing edge is essential: removal of the last linked worktree also removes
// the watched administration directory, so there may be no later event to
// recover a dropped nudge.
func (c *realController) nudgeFamilyTopology(familyID string) {
	c.nudgeFamilyTopologyContext(context.Background(), familyID)
}

func (c *realController) nudgeFamilyTopologyContext(ctx context.Context, familyID string) {
	c.nudgeFamilyTopologyRequest(ctx, familyID, nil)
}

func (c *realController) nudgeFamilyTopologyRequest(ctx context.Context, familyID string, release func()) {
	request := newTopologyNudgeRequest(ctx, release)
	if c == nil || familyID == "" {
		request.finish()
		return
	}
	run := func(runCtx context.Context, id string) {
		if c.topologyReconcile != nil {
			c.topologyReconcile(runCtx, id)
			return
		}
		if c.probeReconcile != nil {
			c.probeReconcile(id)
			return
		}
		if c.lifecycle != nil {
			c.reconcileFamilyForProbeContext(runCtx, id)
		}
	}
	if c.topologyReconcile == nil && c.probeReconcile == nil && c.lifecycle == nil {
		request.finish()
		return
	}

	c.topologyNudgeMu.Lock()
	if c.topologyNudges == nil {
		c.topologyNudges = make(map[string]*topologyNudgeState)
	}
	if state := c.topologyNudges[familyID]; state != nil {
		superseded := state.pending
		state.pending = &request
		c.topologyNudgeMu.Unlock()
		superseded.finish()
		return
	}
	c.topologyNudges[familyID] = &topologyNudgeState{}
	c.topologyNudgeMu.Unlock()

	go c.runTopologyNudgeLoop(run, familyID, request)
}

func (c *realController) runTopologyNudgeLoop(
	run func(context.Context, string), familyID string, current topologyNudgeRequest,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.topologyNudgeMu.Lock()
			state := c.topologyNudges[familyID]
			var pending *topologyNudgeRequest
			if state != nil {
				pending = state.pending
				delete(c.topologyNudges, familyID)
			}
			c.topologyNudgeMu.Unlock()
			pending.finish()
			panic(recovered)
		}
	}()

	for {
		func() {
			defer current.finish()
			run(current.ctx, familyID)
		}()

		c.topologyNudgeMu.Lock()
		state := c.topologyNudges[familyID]
		if state != nil && state.pending != nil {
			current = *state.pending
			state.pending = nil
			c.topologyNudgeMu.Unlock()
			continue
		}
		delete(c.topologyNudges, familyID)
		c.topologyNudgeMu.Unlock()
		return
	}
}

// claimFamilyNudge reports whether this caller won the right to reconcile the
// family now, stamping the window when it did.
func (c *realController) claimFamilyNudge(familyID string) bool {
	c.probeNudgeMu.Lock()
	defer c.probeNudgeMu.Unlock()
	if last, seen := c.probeNudgedAt[familyID]; seen && time.Since(last) < probeReconcileDebounce {
		return false
	}
	if c.probeNudgedAt == nil {
		c.probeNudgedAt = make(map[string]time.Time)
	}
	c.probeNudgedAt[familyID] = time.Now()
	return true
}

// reconcileFamilyForProbe runs the lifecycle's own family reconciliation. A
// failure is logged and dropped: the janitor asks the same question on its own
// schedule, and a probe must not turn a reconciliation failure into an answer.
func (c *realController) reconcileFamilyForProbe(familyID string) {
	c.reconcileFamilyForProbeContext(context.Background(), familyID)
}

func (c *realController) reconcileFamilyForProbeContext(ctx context.Context, familyID string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := c.lifecycle.ReconcileFamily(ctx, familyID); err != nil && c.logger != nil {
		c.logger.Debug("probe view: reconciling the family failed",
			zap.String("family", familyID), zap.Error(err))
	}
}
