package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"uuid"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The ref-view manager.
//
// A ref view is a named view of one graph at a committed selector — a branch, a
// tag, a commit id — that nobody has checked out. Serving it means holding a
// generation whose payload describes that selector's tree, composed over the
// graph's base corpus, and that is the whole of what this file arranges.
//
// Four properties shape it, and none of them is optional:
//
//   - Selection re-resolves, always. Nothing watches the refs a view names, so
//     a branch that moved while nobody was asking is only ever noticed by the
//     next selection. Idle movement therefore costs nothing: no watcher, no
//     poll, no build. The cost is three git plumbing calls per selection.
//
//   - Two selections of one view share one build. The catalog's partial unique
//     index on the in-flight builds is the lock, so the loser is handed the
//     winner's build token rather than starting a second pass that would
//     produce byte-identical payload.
//
//   - What a build produces is adopted only if the world still agrees with it.
//     A build takes as long as it takes, and a ref can move twice in that
//     window. The publish step re-resolves the selector and re-reads the view
//     under the epoch the build captured; a tree that moved makes the finished
//     generation superseded rather than active. The one movement that does NOT
//     cost a rebuild is a new commit carrying the same tree — a rebase, an
//     amend, an empty commit — because the payload is a function of the tree.
//     That case adopts the generation and stamps the new commit beside it.
//
//   - A build outlives the request that started it and reports for as long as
//     it runs. The request owns only how long it is willing to wait; past that
//     it answers with the build's token and the pass carries on toward
//     publication. Meanwhile the pass re-stamps its claim, because the
//     liveness cutoff cannot otherwise tell a slow build from a dead one — and
//     a pass whose claim is taken over anyway loses the right to publish, so
//     its late result is superseded rather than served.

const (
	// refViewOwnerKind names who owns the generations a ref view's builds
	// produce. It is not the checkout owner kind: these generations belong to
	// no checkout, which is the whole point of a ref view.
	refViewOwnerKind = "ref_view"

	// refViewResolverVersion stamps the resolution contract a ref view's
	// generations were built under. Raising it makes every stored ref-view
	// generation miss, which is what a change in what resolution emits
	// requires — the stored payload is not what this binary would produce.
	refViewResolverVersion = commitLayerPipelineEpoch

	// defaultEnrichmentProfile is the profile a request that names none is
	// served under. The profile is part of the view's catalog key and part of
	// its build fingerprint, so two profiles of one selector are two views
	// with two payloads rather than one view served two ways.
	defaultEnrichmentProfile = "default"

	// refViewBuildLiveness is how long a claimed build may go without progress
	// before the next selection may take it over.
	//
	// A claim outlives the process that made it: a daemon killed mid-build, or
	// a request whose bookkeeping write never landed, leaves the coalescing
	// row in the building state with nobody behind it. Every later selection
	// of that tree would then be handed a dead build's token and answer
	// "building" forever — and for a commit or tag selector the tree never
	// moves, so nothing would ever break the tie. The window is generous
	// because the cost of reclaiming a build that is merely slow is one
	// duplicate pass, while the cost of not reclaiming a dead one is a view
	// that never serves again.
	refViewBuildLiveness = 10 * time.Minute

	// refViewBuildHeartbeat is how often a running build re-stamps the claim
	// it holds. It is what makes the liveness window mean "nobody is behind
	// this claim" rather than "this claim is taking a while": the cutoff reads
	// last_progress and nothing else, and a real build over a large tree
	// outlasts the window comfortably. Well under the window, so a stamp
	// delayed behind the store's writer still lands inside it.
	refViewBuildHeartbeat = 30 * time.Second

	// refViewBuildGrace is how long a selection that claimed a build waits for
	// it before answering "building".
	//
	// The wait belongs to the request and the build does not: a tool call has
	// tens of seconds, a build over a large tree has as long as it has, and a
	// selection that blocked on the whole pass would lose the answer to the
	// deadline. Long enough that a small tree still answers ready in one call,
	// short enough that a big one hands back a token to poll instead.
	refViewBuildGrace = 5 * time.Second

	// refViewWriterBudget bounds how long one selection waits for the store's
	// writer.
	//
	// Everything a selection writes is bookkeeping — the view's row, what the
	// selector resolved to, the claim on a build — and the store's mutation
	// gate is held for as long as a build's transactions run. A selection
	// that queued on the gate would wait out somebody else's whole pass and
	// lose its own answer to the tool deadline, which is strictly worse than
	// saying the store is busy: the caller retries either way, and a typed
	// answer inside a couple of seconds is one it can act on.
	refViewWriterBudget = 2 * time.Second
)

// ErrRefViewStoreBusy is the answer a selection gives when the store's writer
// stayed saturated for its whole budget and the bookkeeping it needed could
// not be written.
//
// It is a retry, not a failure. Nothing about the view is known to be wrong —
// the selection never got far enough to decide anything — and the next
// selection past the contention resolves it afresh.
var ErrRefViewStoreBusy = errors.New("indexer: the store is busy building")

// RefViewRequest names one view of one graph.
type RefViewRequest struct {
	// GraphID is the dedicated graph whose corpus the view composes over.
	GraphID string

	// SelectorKind and SelectorValue are the committed state the view pins.
	// Only the two resolvable kinds are accepted here: a worktree or base
	// selector names something that already has a route.
	SelectorKind  gitstate.ViewSelectorKind
	SelectorValue string

	// RepoDir is the repository the selector resolves against and the trees
	// are read from. It is never written to and never checked out.
	RepoDir string

	// EnrichmentProfile is how deeply the view is enriched. Empty takes the
	// default profile.
	EnrichmentProfile string

	// RepoPrefix, WorkspaceID and ProjectID are stamped onto the payload.
	// They are the GRAPH's, not the view's: the layer composes over the
	// graph's corpus, so its nodes have to live in the same namespace.
	RepoPrefix  string
	WorkspaceID string
	ProjectID   string
}

// RefViewResult is what one EnsureRefView call decided.
//
// State is the answer: ready means GenerationID composes over the graph's
// corpus into the view the caller asked for, building means somebody is
// producing it and the caller should retry, and failed means the selector
// could not be served at all — the error carries why.
type RefViewResult struct {
	RefViewID    string
	GenerationID int64

	// Resolved is what the selector named at the moment the call answered. For
	// a ready view it is what the active generation's metadata was stamped
	// with, so a caller can label the view with the commit it is really at.
	Resolved gitstate.ResolvedSelector

	State store_sqlite.RefViewState

	// BuildToken identifies the in-flight attempt a building answer is waiting
	// on. It is empty when the build that was in flight has just been
	// superseded and the retry will claim a new one.
	BuildToken string

	// Built reports that a build pass finished inside this call. It is the
	// difference between "the view was already current" and "the view was made
	// current", and it stays true for a build whose result was superseded —
	// the pass ran either way. A build still running when the call answered is
	// not built: it is a BuildToken to poll.
	Built bool
}

// RefViewManagerConfig is what one manager needs.
type RefViewManagerConfig struct {
	// Store is any handle on the database. Generations are begun, published
	// and superseded through it.
	Store *store_sqlite.Store
	// Builder builds the sparse generations. It must carry the index
	// configuration the base corpus was indexed with.
	Builder *SparseGenerationBuilder
	// Config is the index configuration the generations are built under. Its
	// digest is part of every build fingerprint, so a configuration change
	// invalidates a view's payload instead of composing two payloads built
	// under different rules.
	Config config.IndexConfig
	Logger *zap.Logger
	// Gate holds a claimed build's pass while the daemon warms up. nil admits
	// every build at once, which is what a manager outside a warmup has.
	Gate *ViewBuildGate

	// buildBarrier is a test seam: it runs between a build pass finishing and
	// the publish step re-resolving the selector, which is exactly the window
	// the revalidation exists to close. nil in production.
	buildBarrier func()
	// cacheMissBarrier is a deterministic test seam after a cache miss and
	// before BuildCommitLayer. nil in production.
	cacheMissBarrier func()

	// buildGrace, buildHeartbeat, buildLiveness and writerBudget are test
	// seams over the manager's four windows. Zero takes the package constant,
	// which is what production runs on; the constants are seconds to minutes
	// wide, which is exactly what a test that drives them cannot wait for.
	buildGrace     time.Duration
	buildHeartbeat time.Duration
	buildLiveness  time.Duration
	writerBudget   time.Duration
}

// RefViewManager serves ref views of one store's graphs. It holds no
// per-request state and is safe to use from many goroutines.
type RefViewManager struct {
	store   *store_sqlite.Store
	catalog *store_sqlite.Catalog
	builder *SparseGenerationBuilder
	logger  *zap.Logger
	gate    *ViewBuildGate

	configHash string
	extractors string

	buildBarrier     func()
	cacheMissBarrier func()

	buildGrace     time.Duration
	buildHeartbeat time.Duration
	buildLiveness  time.Duration
	writerBudget   time.Duration
}

// NewRefViewManager builds a manager over one store.
func NewRefViewManager(cfg RefViewManagerConfig) (*RefViewManager, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("indexer: ref view manager needs a store")
	case cfg.Builder == nil:
		return nil, errors.New("indexer: ref view manager needs a generation builder")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RefViewManager{
		store:            cfg.Store,
		catalog:          cfg.Store.Catalog(),
		builder:          cfg.Builder,
		logger:           logger,
		gate:             cfg.Gate,
		configHash:       indexConfigHash(cfg.Config),
		extractors:       extractorVersionsFingerprint(),
		buildBarrier:     cfg.buildBarrier,
		cacheMissBarrier: cfg.cacheMissBarrier,
		buildGrace:       refViewWindow(cfg.buildGrace, refViewBuildGrace),
		buildHeartbeat:   refViewWindow(cfg.buildHeartbeat, refViewBuildHeartbeat),
		buildLiveness:    refViewWindow(cfg.buildLiveness, refViewBuildLiveness),
		writerBudget:     refViewWindow(cfg.writerBudget, refViewWriterBudget),
	}, nil
}

// withWriter runs one bookkeeping write a selection makes, under a budget of
// its own, and re-types a budget that ran out as the busy answer.
//
// A request whose OWN context ended keeps its own error: that is the caller
// giving up, and calling it a busy store would hide a cancellation behind a
// retry. Everything else the write returns travels unchanged — a stale guard
// is still a stale guard.
func (m *RefViewManager) withWriter(ctx context.Context, write func(context.Context) error) error {
	writeCtx, cancel := context.WithTimeout(ctx, m.writerBudget)
	defer cancel()
	err := write(writeCtx)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrRefViewStoreBusy, err)
	}
	return err
}

// refViewWindow takes the configured build window, or the default when the
// caller set none.
func refViewWindow(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

// EnsureRefView makes one view current and reports what serving it would read.
//
// The order is fixed: find or create the view's row so a failure has somewhere
// to be recorded, resolve the selector, decide whether what the view is
// already serving describes exactly that state, and only then claim a build.
// Re-resolving on every selection is what makes idle movement free; deciding
// against the fingerprint rather than against the ref is what makes a commit
// that changed no tree free too.
func (m *RefViewManager) EnsureRefView(ctx context.Context, req RefViewRequest) (RefViewResult, error) {
	if err := m.validate(&req); err != nil {
		return RefViewResult{}, err
	}
	base, err := m.base(ctx, req.GraphID)
	if err != nil {
		return RefViewResult{}, err
	}
	viewID := refViewID(req)
	view, err := m.row(ctx, viewID, req)
	if err != nil {
		return RefViewResult{}, err
	}

	resolved, err := gitstate.ResolveViewSelector(ctx, req.RepoDir, req.SelectorKind, req.SelectorValue)
	if err != nil {
		return m.failed(ctx, view, err)
	}

	identity := m.identity(viewID, base, resolved.TreeOID)
	fingerprint := refViewBuildFingerprint(identity, req.EnrichmentProfile)
	current, err := m.activeIsCurrent(ctx, view, fingerprint)
	if err != nil {
		return RefViewResult{}, err
	}
	if !current {
		coalesced, onto, err := m.coalesced(ctx, view, base, resolved, fingerprint)
		if err != nil {
			return RefViewResult{}, err
		}
		if onto {
			return coalesced, nil
		}
	}

	view, err = m.desire(ctx, view, resolved, fingerprint, current)
	if err != nil {
		return RefViewResult{}, err
	}
	if current {
		return m.adoptMetadata(ctx, view, resolved)
	}
	return m.startBuild(ctx, req, view, base, resolved, identity, fingerprint)
}

// row finds the view's catalog row, reading before it writes.
//
// The read pool answers while the writer is saturated and the writer does
// not, and for every selection after the first the row is already there. The
// upsert this used to open with therefore charged every selection of an
// established view a place in the writer's queue for a row it was not going
// to change — and the pass ahead of it in that queue is usually the very
// build the selection is about to report.
func (m *RefViewManager) row(
	ctx context.Context,
	viewID string,
	req RefViewRequest,
) (store_sqlite.RefView, error) {
	stored, found, err := m.catalog.GetRefView(ctx, viewID)
	if err != nil {
		return store_sqlite.RefView{}, err
	}
	if found {
		return stored, nil
	}
	var created store_sqlite.RefView
	err = m.withWriter(ctx, func(writeCtx context.Context) error {
		var writeErr error
		created, writeErr = m.catalog.GetOrCreateRefView(writeCtx, store_sqlite.RefView{
			RefViewID:         viewID,
			GraphID:           req.GraphID,
			SelectorKind:      string(req.SelectorKind),
			SelectorValue:     req.SelectorValue,
			EnrichmentProfile: req.EnrichmentProfile,
			State:             store_sqlite.RefViewPending,
			ExactView:         true,
		})
		return writeErr
	})
	if err != nil {
		return store_sqlite.RefView{}, err
	}
	return created, nil
}

// validate refuses a request that cannot name a view, and fills the one
// default a caller may leave unset.
func (m *RefViewManager) validate(req *RefViewRequest) error {
	switch {
	case m == nil:
		return errors.New("indexer: nil ref view manager")
	case req.GraphID == "":
		return errors.New("indexer: ref view request needs a graph id")
	case req.RepoDir == "":
		return errors.New("indexer: ref view request needs a repository directory")
	case req.SelectorValue == "":
		return errors.New("indexer: ref view request needs a selector value")
	}
	switch req.SelectorKind {
	case gitstate.ViewSelectorGitRef, gitstate.ViewSelectorCommit:
	default:
		return fmt.Errorf("indexer: selector kind %q names no committed state", string(req.SelectorKind))
	}
	if req.EnrichmentProfile == "" {
		req.EnrichmentProfile = defaultEnrichmentProfile
	}
	return nil
}

// base resolves the corpus a view's layer sits on.
func (m *RefViewManager) base(ctx context.Context, graphID string) (primaryBase, error) {
	dedicated, found, err := m.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return primaryBase{}, err
	}
	if !found {
		return primaryBase{}, fmt.Errorf("indexer: graph %s has no dedicated-graph row to build over", graphID)
	}
	return graphBase(ctx, m.catalog, dedicated)
}

// activeIsCurrent reports whether the generation the view already serves was
// built from exactly these inputs and its structural payload is still servable.
// A fingerprint match settles the tree, base, and extraction rules. Producer
// capability degradation does not stale that identity: materialization keeps
// the structural view and request capability evaluation refuses only operations
// that need the degraded producer. Ref/commit metadata remains excluded so a
// moved ref with an unchanged tree is a metadata-only update.
func (m *RefViewManager) activeIsCurrent(
	ctx context.Context,
	view store_sqlite.RefView,
	fingerprint string,
) (bool, error) {
	if view.ActiveGenerationID <= 0 || view.ActiveBuildFingerprint != fingerprint {
		return false, nil
	}
	row, found, err := m.catalog.GetViewGeneration(ctx, view.ActiveGenerationID)
	if err != nil {
		return false, err
	}
	return found && servableGeneration(row.State), nil
}

// coalesced answers a selection whose build is already in flight, from the
// read pool alone.
//
// This is the answer that must never queue. Everything it needs is a read —
// that the row already wants exactly this tree under exactly this
// fingerprint, and that the attempt holding the slot is still alive — and the
// store answers reads on a pool the writer's saturation does not reach.
// Reaching the same conclusion through the writer meant every selection of a
// building view waited out the build it was about to report, because the
// build is what holds the mutation gate.
//
// The desire is checked, not re-stamped. The fast path answers only when what
// the desire write WOULD record is already what the row says, so declining to
// write it leaves the catalog in the state the slow path would have left it
// in; anything else falls through and writes. What a coalescing selection
// then skips is the selection clock beside it, which nothing reads back and
// the next selection re-stamps.
func (m *RefViewManager) coalesced(
	ctx context.Context,
	view store_sqlite.RefView,
	base primaryBase,
	resolved gitstate.ResolvedSelector,
	fingerprint string,
) (RefViewResult, bool, error) {
	if view.DesiredTree != resolved.TreeOID || view.DesiredBuildFingerprint != fingerprint {
		return RefViewResult{}, false, nil
	}
	build, inFlight, err := m.catalog.InFlightRefViewBuild(ctx, store_sqlite.RefViewBuildKey{
		RefViewID:        view.RefViewID,
		DesiredTree:      resolved.TreeOID,
		BaseGenerationID: base.generationID,
		BuildFingerprint: fingerprint,
	}, time.Now().Unix()-int64(m.buildLiveness/time.Second))
	if err != nil || !inFlight {
		return RefViewResult{}, false, err
	}
	viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewCoalesced)
	m.logger.Debug("ref view manager: selection coalesced onto a running build off the read pool",
		zap.String("ref_view", view.RefViewID), zap.String("build_token", build.BuildToken))
	return RefViewResult{
		RefViewID:  view.RefViewID,
		Resolved:   resolved,
		State:      store_sqlite.RefViewBuilding,
		BuildToken: build.BuildToken,
	}, true, nil
}

// desire records what this selection resolved to and re-reads the row.
//
// The re-read is not a convenience: the desire write bumps the view's epoch
// exactly when the tree or the fingerprint moved, and the epoch a build
// captures has to be the one that write left behind.
func (m *RefViewManager) desire(
	ctx context.Context,
	view store_sqlite.RefView,
	resolved gitstate.ResolvedSelector,
	fingerprint string,
	current bool,
) (store_sqlite.RefView, error) {
	state := store_sqlite.RefViewBuilding
	if current {
		state = store_sqlite.RefViewReady
	}
	now := time.Now().Unix()
	err := m.withWriter(ctx, func(writeCtx context.Context) error {
		return m.catalog.UpdateRefViewDesire(writeCtx, store_sqlite.UpdateRefViewDesireRequest{
			RefViewID:               view.RefViewID,
			DesiredRef:              resolved.FullRef,
			DesiredCommit:           resolved.CommitOID,
			DesiredTree:             resolved.TreeOID,
			DesiredBuildFingerprint: fingerprint,
			State:                   state,
			LastResolved:            now,
			LastSelected:            now,
		})
	})
	if err != nil {
		return store_sqlite.RefView{}, err
	}
	stored, found, err := m.catalog.GetRefView(ctx, view.RefViewID)
	if err != nil {
		return store_sqlite.RefView{}, err
	}
	if !found {
		return store_sqlite.RefView{}, fmt.Errorf("%w: ref view %s",
			store_sqlite.ErrCatalogNotFound, view.RefViewID)
	}
	return stored, nil
}

// adoptMetadata answers a selection whose payload is already current. The
// generation is untouched; only the ref and commit the selector resolves to
// now are stamped beside it.
//
// A lost epoch guard is not an error here. It means another actor re-targeted
// the view between the two writes, and that does not make the generation this
// call is answering with any less correct — it was built for the tree this
// selection resolved to. A saturated writer is the same shape of nothing: the
// stamp is metadata beside a generation that is already right, and the next
// selection past the contention writes it.
func (m *RefViewManager) adoptMetadata(
	ctx context.Context,
	view store_sqlite.RefView,
	resolved gitstate.ResolvedSelector,
) (RefViewResult, error) {
	now := time.Now().Unix()
	err := m.withWriter(ctx, func(writeCtx context.Context) error {
		return m.catalog.TouchRefViewSelection(writeCtx, store_sqlite.TouchRefViewSelectionRequest{
			RefViewID:          view.RefViewID,
			ExpectedRouteEpoch: view.RouteEpoch,
			ActiveRef:          resolved.FullRef,
			ActiveCommit:       resolved.CommitOID,
			LastResolved:       now,
			LastSelected:       now,
		})
	})
	if err != nil &&
		!errors.Is(err, store_sqlite.ErrCatalogStaleGuard) &&
		!errors.Is(err, ErrRefViewStoreBusy) {
		return RefViewResult{}, err
	}
	viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewReady)
	return RefViewResult{
		RefViewID:    view.RefViewID,
		GenerationID: view.ActiveGenerationID,
		Resolved:     resolved,
		State:        store_sqlite.RefViewReady,
	}, nil
}

// startBuild claims the build for this state and runs it, or reports the
// attempt already running it.
func (m *RefViewManager) startBuild(
	ctx context.Context,
	req RefViewRequest,
	view store_sqlite.RefView,
	base primaryBase,
	resolved gitstate.ResolvedSelector,
	identity GenerationIdentity,
	fingerprint string,
) (RefViewResult, error) {
	now := time.Now().Unix()
	attempt := store_sqlite.RefViewBuild{
		BuildID:            uuid.NewV7().String(),
		RefViewID:          view.RefViewID,
		DesiredRef:         resolved.FullRef,
		DesiredCommit:      resolved.CommitOID,
		DesiredTree:        resolved.TreeOID,
		BaseGenerationID:   base.generationID,
		EnrichmentProfile:  req.EnrichmentProfile,
		BuildFingerprint:   fingerprint,
		CapturedRouteEpoch: view.RouteEpoch,
		State:              store_sqlite.ViewGenerationBuilding,
		BuildToken:         uuid.NewV7().String(),
		CreatedAt:          now,
		LastProgress:       now,
	}
	var claimed store_sqlite.RefViewBuild
	err := m.withWriter(ctx, func(writeCtx context.Context) error {
		var claimErr error
		claimed, claimErr = m.catalog.ClaimRefViewBuild(
			writeCtx, attempt, now-int64(m.buildLiveness/time.Second))
		return claimErr
	})
	if err != nil {
		if errors.Is(err, store_sqlite.ErrRefViewBuildInFlight) {
			viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewCoalesced)
			// The build token is the id the caller polls on, so it is what
			// makes two selections of one tree legible as one build.
			m.logger.Debug("ref view manager: selection coalesced onto a running build",
				zap.String("ref_view", view.RefViewID),
				zap.String("build_token", claimed.BuildToken))
			return RefViewResult{
				RefViewID:  view.RefViewID,
				Resolved:   resolved,
				State:      store_sqlite.RefViewBuilding,
				BuildToken: claimed.BuildToken,
			}, nil
		}
		return RefViewResult{}, err
	}
	return m.runDetached(ctx, req, view, claimed, base, resolved, identity)
}

// runDetached runs a claimed build on a context the request cannot cancel, and
// waits out the grace for it.
//
// The pass is the daemon's work, not the request's. A client that gives up —
// or a tool deadline that expires — must not destroy a build every other
// selection of that tree is coalescing onto, and the pass is also what closes
// the claim, so killing it wedges the view until the liveness window expires.
// What the request keeps is the wait: past the grace it answers with the token
// and the build publishes for whoever selects next.
//
// A build the warmup gate is holding is that same shape with the wait removed.
// The pass parks before it starts, so it looks to everything else exactly like
// a slow one: the claim is held and heartbeaten, later selections coalesce
// onto its token, and the publish happens when builds are admitted. What the
// selection does not do is sit out a grace no build can finish inside.
func (m *RefViewManager) runDetached(
	ctx context.Context,
	req RefViewRequest,
	view store_sqlite.RefView,
	build store_sqlite.RefViewBuild,
	base primaryBase,
	resolved gitstate.ResolvedSelector,
	identity GenerationIdentity,
) (RefViewResult, error) {
	type outcome struct {
		result RefViewResult
		err    error
	}
	// Buffered by one: the grace can end the wait first, and the build must
	// never block on a receiver that has already answered.
	done := make(chan outcome, 1)
	buildCtx := context.WithoutCancel(ctx)
	go func() {
		stop := m.heartbeat(buildCtx, build)
		defer stop()
		release, err := m.gate.Acquire(buildCtx, ViewBuildInteractive)
		if err != nil {
			done <- outcome{err: fmt.Errorf("indexer: wait for ref-view build admission: %w", err)}
			return
		}
		defer release()
		result, err := m.runBuild(buildCtx, req, view, build, base, resolved, identity)
		done <- outcome{result: result, err: err}
	}()

	if !m.gate.Admitted() {
		viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewDeferred)
		m.logger.Debug("ref view manager: build deferred until the daemon has warmed up",
			zap.String("ref_view", view.RefViewID), zap.String("build_token", build.BuildToken))
		return m.building(view, build, resolved), nil
	}
	grace := time.NewTimer(m.buildGrace)
	defer grace.Stop()
	select {
	case finished := <-done:
		return finished.result, finished.err
	case <-grace.C:
	case <-ctx.Done():
	}
	viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewBuilding)
	m.logger.Debug("ref view manager: selection answered while its build runs on",
		zap.String("ref_view", view.RefViewID), zap.String("build_token", build.BuildToken))
	return m.building(view, build, resolved), nil
}

// building is the answer a selection gives when the pass it claimed is still
// running: the token to poll, and the state the selector resolved to.
func (m *RefViewManager) building(
	view store_sqlite.RefView,
	build store_sqlite.RefViewBuild,
	resolved gitstate.ResolvedSelector,
) RefViewResult {
	return RefViewResult{
		RefViewID:  view.RefViewID,
		Resolved:   resolved,
		State:      store_sqlite.RefViewBuilding,
		BuildToken: build.BuildToken,
	}
}

// heartbeat re-stamps a running build's claim, until the returned stop is
// called. The stop waits for the stamping to have finished, so no stamp
// outlives the completion that closes the attempt.
//
// The liveness cutoff reads last_progress and nothing else, so a claim stamped
// only when it was made is indistinguishable from one whose worker died the
// moment it made it. Without this, any build slower than the window — a large
// tree, a cold object store, a long wait behind the store's writer — is
// reclaimed while it is still running, and the duplicate races the original
// for the publish.
//
// A stamp refused as stale means the claim has already been taken over. That
// is not this goroutine's to resolve: the build finds out at publish time,
// where losing the claim costs it the adoption rather than the pass.
func (m *RefViewManager) heartbeat(ctx context.Context, build store_sqlite.RefViewBuild) func() {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(m.buildHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				err := m.catalog.TouchRefViewBuild(ctx, build.BuildID, build.BuildToken, time.Now().Unix())
				if err != nil && !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
					m.logger.Debug("ref view manager: could not stamp a build's progress",
						zap.String("build", build.BuildID), zap.Error(err))
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// runBuild produces the generation and decides whether it may be adopted.
func (m *RefViewManager) runBuild(
	ctx context.Context,
	req RefViewRequest,
	view store_sqlite.RefView,
	build store_sqlite.RefViewBuild,
	base primaryBase,
	resolved gitstate.ResolvedSelector,
	identity GenerationIdentity,
) (RefViewResult, error) {
	key := m.refReadyGenerationKey(base, resolved.TreeOID)
	required := commitLayerRequiredCapabilities()
	claim, found, err := m.catalog.ClaimReadyGeneration(ctx, store_sqlite.ClaimReadyGenerationRequest{
		Key: key, RequiredCapabilities: required,
	})
	if err != nil {
		m.completeBuild(ctx, build, store_sqlite.ViewGenerationFailed, 0, err.Error())
		return RefViewResult{}, err
	}

	built := false
	candidateID := int64(0)
	leaseToken := ""
	buildIdentity := identity
	if claim.CapabilityMiss {
		// Keep the canonical content key unchanged, but avoid owner-identity
		// dedup against the source-withdrawn generation being replaced.
		buildIdentity.LayerID = identity.LayerID + ":source-recovery:" + build.BuildID
	}
	if found {
		leaseToken = claim.LeaseToken
	}
	defer func() { m.releaseRefReadyLease(ctx, leaseToken) }()

	if !found {
		if m.cacheMissBarrier != nil {
			m.cacheMissBarrier()
		}
		generationID, report, buildErr := m.builder.BuildCommitLayer(ctx, CommitLayerRequest{
			Identity: buildIdentity, Base: m.store.AtGeneration(base.generationID), RepoDir: req.RepoDir,
			BaseTreeOID: base.treeOID, TargetTreeOID: resolved.TreeOID, RootPath: req.RepoDir,
			RepoPrefix: req.RepoPrefix, WorkspaceID: req.WorkspaceID, ProjectID: req.ProjectID,
		})
		candidateID = generationID
		built = true
		if m.buildBarrier != nil {
			m.buildBarrier()
		}
		if buildErr != nil {
			failure := classifyRefViewBuildError(buildErr)
			m.completeBuild(ctx, build, store_sqlite.ViewGenerationFailed, 0, failure.Error())
			result, failErr := m.failed(ctx, view, failure)
			result.Built = true
			return result, failErr
		}
		if report.ClosureTruncated {
			m.logger.Warn("ref view manager: build closure truncated", zap.String("ref_view", view.RefViewID), zap.Int64("generation", candidateID), zap.Int("cap", report.ClosureCap))
		}
	}

	published, err := gitstate.ResolveViewSelector(ctx, req.RepoDir, req.SelectorKind, req.SelectorValue)
	if err != nil {
		m.completeBuild(ctx, build, store_sqlite.ViewGenerationFailed, 0, err.Error())
		result, failErr := m.failed(ctx, view, err)
		result.Built = built
		return result, failErr
	}
	if published.TreeOID != resolved.TreeOID {
		return m.retryRefReadyBuild(ctx, build, view, published, built), nil
	}

	if !found {
		claim, found, err = m.catalog.ClaimReadyGeneration(ctx, store_sqlite.ClaimReadyGenerationRequest{
			Key: key, CandidateGenerationID: candidateID, RequiredCapabilities: required,
		})
		if err != nil {
			m.completeBuild(ctx, build, store_sqlite.ViewGenerationFailed, 0, err.Error())
			return RefViewResult{}, err
		}
		if !found {
			err = fmt.Errorf("indexer: ready ref-view candidate %d disappeared before claim", candidateID)
			m.completeBuild(ctx, build, store_sqlite.ViewGenerationFailed, 0, err.Error())
			return RefViewResult{}, err
		}
		leaseToken = claim.LeaseToken
	}

	err = m.catalog.BindReadyGenerationLeaseToRefView(ctx, store_sqlite.BindReadyGenerationLeaseToRefViewRequest{
		Key: key, LeaseToken: claim.LeaseToken, GenerationID: claim.WinnerGenerationID,
		RefViewID: view.RefViewID, ExpectedRouteEpoch: build.CapturedRouteEpoch,
		ExpectedDesiredTree: resolved.TreeOID, ExpectedDesiredBuildFingerprint: build.BuildFingerprint,
		BuildID: build.BuildID, BuildToken: build.BuildToken,
		ActiveRef: published.FullRef, ActiveCommit: published.CommitOID, ActiveTree: published.TreeOID,
		ActiveBuildFingerprint: build.BuildFingerprint, ExactView: true,
	})
	if err != nil {
		m.releaseRefReadyLease(ctx, leaseToken)
		leaseToken = ""
		m.retireRefReadyCandidate(ctx, candidateID, claim.WinnerGenerationID)
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return m.retryRefReadyBuild(ctx, build, view, published, built), nil
		}
		m.completeBuild(ctx, build, store_sqlite.ViewGenerationFailed, 0, err.Error())
		return RefViewResult{}, err
	}
	leaseToken = "" // consumed atomically by bind
	m.retireRefReadyCandidate(ctx, candidateID, claim.WinnerGenerationID)
	viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewAdopted)
	return RefViewResult{RefViewID: view.RefViewID, GenerationID: claim.WinnerGenerationID, Resolved: published, State: store_sqlite.RefViewReady, Built: built}, nil
}

// superseded takes a finished build out of the running and answers with a
// retry. The view's active pointer is untouched: whatever it was serving is
// still a legal thing to serve, and the next selection resolves the state the
// selector actually moved to.
func (m *RefViewManager) superseded(
	ctx context.Context,
	build store_sqlite.RefViewBuild,
	generationID int64,
	view store_sqlite.RefView,
	published gitstate.ResolvedSelector,
) RefViewResult {
	m.supersede(ctx, build, generationID)
	viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewBuilding)
	return RefViewResult{
		RefViewID: view.RefViewID,
		Resolved:  published,
		State:     store_sqlite.RefViewBuilding,
		Built:     true,
	}
}

// supersede retires a generation nothing will adopt and closes its build.
// Both writes are best effort: the caller's answer is "retry" either way, and
// failing the selection because the bookkeeping failed would turn a retryable
// answer into an error. Both are detached from the request for the reason
// completeBuild gives — a cancellation is exactly when they matter most.
func (m *RefViewManager) supersede(ctx context.Context, build store_sqlite.RefViewBuild, generationID int64) {
	ctx = closingContext(ctx)
	if generationID > 0 {
		if err := m.store.MarkPayloadGenerationSuperseded(ctx, generationID); err != nil {
			m.logger.Debug("ref view manager: could not supersede an unadopted generation",
				zap.String("ref_view", build.RefViewID),
				zap.Int64("generation", generationID), zap.Error(err))
		}
	}
	m.completeBuild(ctx, build, store_sqlite.ViewGenerationSuperseded, generationID, "")
}

// completeBuild ends one attempt that will not publish — it failed, or it was
// overtaken. The attempt that DOES publish is closed by the adoption itself,
// in the same transaction that points the view at its generation.
//
// It runs detached from the request, and for the same reason the build does:
// an attempt left in the building state holds the coalescing claim, and the
// claim is what every later selection of that tree waits on, so a pass that
// ends without closing it wedges the view until the liveness window expires.
//
// The write itself is still best effort. A lost guard means the attempt is no
// longer this worker's to close — the row went with its ref view, or the claim
// was reclaimed — and either way the answer this selection gives is already
// decided by the time it runs.
func (m *RefViewManager) completeBuild(
	ctx context.Context,
	build store_sqlite.RefViewBuild,
	state store_sqlite.ViewGenerationState,
	generationID int64,
	buildError string,
) {
	err := m.catalog.CompleteRefViewBuild(closingContext(ctx), store_sqlite.CompleteRefViewBuildRequest{
		BuildID:      build.BuildID,
		BuildToken:   build.BuildToken,
		State:        state,
		GenerationID: generationID,
		LastProgress: time.Now().Unix(),
		Error:        buildError,
	})
	if err != nil && !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		m.logger.Warn("ref view manager: could not close a build attempt",
			zap.String("build", build.BuildID), zap.Error(err))
	}
}

// failed records why a selection could not be served and hands the cause back.
// The active pointer is never touched: a view whose newest build failed keeps
// serving what it was serving, and whoever reads it labels that inexact.
//
// The record is diagnostics, and it is bounded like every other write a
// selection makes. It closes nothing — the claim is released by completeBuild,
// which is not bounded for exactly that reason — so a saturated writer costs
// the last_error stamp and nothing else, where waiting the writer out would
// cost the caller the cause this call is about to return.
func (m *RefViewManager) failed(
	ctx context.Context,
	view store_sqlite.RefView,
	cause error,
) (RefViewResult, error) {
	recordCtx, cancel := context.WithTimeout(closingContext(ctx), m.writerBudget)
	defer cancel()
	err := m.catalog.FailRefView(recordCtx, store_sqlite.FailRefViewRequest{
		RefViewID:          view.RefViewID,
		ExpectedRouteEpoch: view.RouteEpoch,
		LastError:          cause.Error(),
		LastResolved:       time.Now().Unix(),
	})
	if err != nil && !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		m.logger.Warn("ref view manager: could not record a failed selection",
			zap.String("ref_view", view.RefViewID), zap.Error(err))
	}
	viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewFailed)
	return RefViewResult{
		RefViewID: view.RefViewID,
		State:     store_sqlite.RefViewFailed,
	}, cause
}

// closingContext is what a selection's closing writes run under: the request's
// values without its cancellation. Every one of them records something the
// selection has already decided — the attempt is over, the generation is not
// being adopted, the selector could not be served — and a canceled request
// that skipped them would leave that state behind for the next caller to trip
// over rather than saving any work.
func closingContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// identity is the catalog identity of the generation a ref view's build
// produces.
//
// The commit is deliberately not part of it, exactly as it is not part of a
// checkout's commit layer: two commits with the same tree produce the same
// payload, and keying on the commit would rebuild for a rebase that changed
// nothing a reader can see. Which commit the view is AT lives on the view's
// row, where it can be re-stamped without touching the payload.
func (m *RefViewManager) identity(viewID string, base primaryBase, targetTree string) GenerationIdentity {
	return GenerationIdentity{
		OwnerKind:            refViewOwnerKind,
		GraphID:              base.graphID,
		LayerID:              refViewLayerID(viewID),
		GenerationKind:       CommitLayerGenerationKind,
		BaseGenerationID:     base.generationID,
		LowerViewFingerprint: base.treeOID,
		TreeOID:              targetTree,
		ConfigHash:           m.configHash,
		ExtractorVersions:    m.extractors,
		ResolverVersion:      refViewResolverVersion,
	}
}

// refViewLayerID names a ref view's layer. It is derived rather than
// generated so the catalog's in-flight generation coalescing can recognise two
// builds of the same layer as the same build.
func refViewLayerID(viewID string) string { return "refview-layer-" + viewID }

// refViewID derives a view's catalog id from what makes it that view.
//
// Deriving rather than generating is what lets two processes that have never
// spoken agree on which row a selector belongs to: both compute the same id,
// and the second one's insert is declined instead of minting a duplicate the
// UNIQUE selector key would have to refuse anyway.
func refViewID(req RefViewRequest) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		req.GraphID,
		string(req.SelectorKind),
		req.SelectorValue,
		req.EnrichmentProfile,
	}, "\x00")))
	return "refview-" + hex.EncodeToString(sum[:16])
}

// refViewBuildFingerprint digests everything a ref view's payload is a
// function of: the base generation and tree the layer sits on, the tree it
// targets, the extraction rules it was produced under, and the enrichment
// profile it is served at. Two selections that agree on it would produce the
// same payload, which is what makes one able to wait on the other.
func refViewBuildFingerprint(identity GenerationIdentity, profile string) string {
	return commitLayerRouteFingerprint(identity, profile)
}

// classifyRefViewBuildError re-types a build failure the local object store
// caused. A tree that resolved a moment ago and cannot be read now was pruned
// or was never fully fetched, and that is an availability answer the caller
// can act on rather than an opaque build failure.
func classifyRefViewBuildError(err error) error {
	if errors.Is(err, source.ErrObjectMissing) {
		return fmt.Errorf("%w: %w", gitstate.ErrRefNotAvailableLocally, err)
	}
	return err
}
