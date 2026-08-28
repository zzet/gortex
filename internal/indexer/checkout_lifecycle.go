package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"uuid"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// Tracking-intent sources, re-exported so an entry point does not have to
// import the catalog package just to say who asked.
const (
	// TrackSourceCLI is an explicit `gortex track` / daemon control call.
	TrackSourceCLI = store_sqlite.IntentSourceCLITrack
	// TrackSourceMCP is an explicit track_repository tool call.
	TrackSourceMCP = store_sqlite.IntentSourceMCPTrack
	// TrackSourceConfig is a repository named in the global configuration.
	TrackSourceConfig = store_sqlite.IntentSourceManualConfig
	// TrackSourceImplicit records a checkout observed without anyone asking
	// for it — the auto-index path. It is deliberately not an intent kind:
	// the constant exists so a caller can name the case, and the lifecycle
	// writes no tracking intent for it.
	TrackSourceImplicit store_sqlite.IntentSourceKind = ""
)

// ErrCheckoutNotTracked reports a lifecycle operation aimed at a path or
// prefix that names nothing this daemon tracks.
var ErrCheckoutNotTracked = errors.New("indexer: no tracked repository matches")

// errNoCatalog reports a flow that only means anything against a catalog: the
// mode changes are moves between catalog rows, so a store without one has no
// automatic lane to move between.
var errNoCatalog = errors.New("indexer: this store keeps no checkout catalog")

// LifecycleNotifier is what has to be told that the tracked-repository set
// changed. The MCP server implements it; a daemon without one still keeps
// its catalog, config and watcher coherent.
type LifecycleNotifier interface {
	// InvalidateSessionScopes drops cached per-session workspace bindings.
	InvalidateSessionScopes()
	// RunAnalysis recomputes the graph-wide rollups.
	RunAnalysis()
}

// RepoWatcher is the part of the live file watcher the lifecycle drives.
// *MultiWatcher implements it.
type RepoWatcher interface {
	AddRepo(repoPrefix string, cfg config.WatchConfig) error
	RemoveRepo(repoPrefix string) error
}

// CheckoutLifecycleConfig is what the lifecycle needs to own its side effects.
type CheckoutLifecycleConfig struct {
	// MultiIndexer is the corpus the lifecycle indexes into and evicts from.
	MultiIndexer *MultiIndexer
	// ConfigManager persists the tracked-repository list.
	ConfigManager *config.ConfigManager
	// Graph is the store; the lifecycle uses its catalog when it has one.
	Graph  graph.Store
	Logger *zap.Logger
	// Reconcile carries the two grace windows. A zero value takes the
	// shipped defaults.
	Reconcile reconcile.Config
	// Clock overrides the lifecycle's and the reconciler's clock.
	Clock func() time.Time
	// ViewLeases is the lease manager materialized views pin their
	// generations with. The lifecycle hands it to every coordinator, so a
	// generation under a live view is refused retirement rather than swept.
	// nil makes the lifecycle own one; a caller that materializes views must
	// pass the manager it materializes through.
	ViewLeases *graphview.LeaseManager
	// RefViews bounds how much ref-view payload the store keeps. A zero value
	// takes the shipped defaults.
	RefViews RefViewRetention

	// indexBarrier is a test seam: it runs inside a promotion, between the
	// sample the new corpus has to describe and the index that builds it,
	// which is exactly the window the re-sample exists to close. nil in
	// production.
	indexBarrier func()
}

// CheckoutLifecycle is the single owner of checkout lifecycle side effects.
//
// Every entry point that tracks, forgets, reloads or sweeps a checkout goes
// through it, so identity (which family and incarnation a path is), intent
// (who asked for it), clocks (how long an outage has run) and cleanup (what
// is detached, evicted and persisted, in what order) are decided once
// instead of once per surface.
//
// Full indexing is unchanged: a tracked repository still indexes into the
// base corpus exactly as before. What the lifecycle adds around it is the
// catalog identity and the ordering of the side effects.
//
// A store with no catalog — or a repository git does not administer — still
// works: the catalog steps are skipped and the real side effects (index,
// watcher, config, invalidation) happen exactly as they did before.
type familyRetry struct {
	deadline int64
	timer    *time.Timer
}

type CheckoutLifecycle struct {
	mi      *MultiIndexer
	cfgMgr  *config.ConfigManager
	catalog *store_sqlite.Catalog
	store   *store_sqlite.Store
	leases  *graphview.LeaseManager
	rec     *reconcile.Reconciler
	logger  *zap.Logger
	now     func() time.Time
	// buildingRecoveryCutoff is this lifecycle process's start. A building
	// generation older than it cannot have been created by this process and is
	// crash residue unless a process-local payload flight has adopted it.
	buildingRecoveryCutoff int64

	// retryMu owns one deadline timer per family. Filesystem events start the
	// grace; these timers guarantee its expiry is reconciled even when Git is
	// otherwise quiet.
	retryMu       sync.Mutex
	familyRetries map[string]familyRetry

	// coordMu guards the coordinator registry alone. It is separate from mu
	// because dropping a coordinator waits for its in-flight build, and
	// holding the collaborator lock across that wait would block every
	// watcher and notifier lookup for the length of an index pass.
	coordMu      sync.Mutex
	coordinators map[string]*CheckoutCoordinator
	// started holds every coordinator this process has started and not yet
	// seen stop, keyed by checkout. The registry is what can be handed a
	// cycle; this is what is running. They come apart for the length of a
	// build: every transition drops the registered coordinator, drives a whole
	// rebuild with the replacement and registers it only afterwards, and a
	// report that read the registry there would call a daemon whose build
	// loops are running one that runs none. Entries are dropped lazily, on the
	// next read or start for the same checkout.
	started map[string][]*CheckoutCoordinator
	// owed holds generations no coordinator is left to retire: the backlog a
	// dropped one handed over, the commit layers its reuse cache was holding,
	// and the two slots of a checkout whose route is being withdrawn. The
	// sweep retries them until the catalog stops refusing.
	owed map[int64]struct{}

	// refViewMu guards the per-repository ref-view manager cache alone. A
	// manager holds no per-request state, so the lock covers only the map.
	refViewMu sync.Mutex
	refViews  map[string]*RefViewManager
	// refViewRetention bounds how much ref-view payload survives a sweep.
	refViewRetention RefViewRetention
	// indexBarrier is the promotion's test seam; nil in production.
	indexBarrier func()
	// routeBarrier stands in for the route withdrawal a promotion runs after
	// the mode flip, which is the one write no fixture can make the catalog
	// refuse. A test seam; nil in production.
	routeBarrier func(context.Context, string) error

	// mu guards only the late-bound collaborators. None of them is held
	// across a saga: the hooks re-enter the lifecycle, and holding a lock
	// over the indexer's own teardown would invert the lock order.
	mu        sync.RWMutex
	watcherFn func() RepoWatcher
	notifier  LifecycleNotifier
	// gate defers build work while the daemon warms up. nil admits every
	// build, which is what every surface that has no warmup runs with.
	gate *ViewBuildGate
	// batchDepth / batchPending coalesce the fan-out across a multi-repo
	// operation. Rerunning the whole-graph analysis once per repository in a
	// reload of twenty of them would cost twenty whole-graph passes to reach
	// the same answer the last one gives.
	batchDepth   int
	batchPending bool

	// transitionCtx owns promotion and demotion workers. Durable transition
	// rows outlive request contexts; this context instead lives for exactly as
	// long as the lifecycle, so a disconnected caller cannot abandon work and
	// daemon shutdown can still stop it deliberately.
	transitionCtx           context.Context
	cancelTransitions       context.CancelFunc
	transitionMu            sync.Mutex
	transitionRuns          map[string]*modeTransitionRun
	transitionQueue         chan *modeTransitionRun
	transitionWorkerStarted bool
	transitionWG            sync.WaitGroup
	transitionClosed        bool
}

// NewCheckoutLifecycle builds the lifecycle. It fails only on a missing
// indexer; everything else degrades to the pre-catalog behaviour.
func NewCheckoutLifecycle(cfg CheckoutLifecycleConfig) (*CheckoutLifecycle, error) {
	if cfg.MultiIndexer == nil {
		return nil, errors.New("indexer: checkout lifecycle needs a multi-repo indexer")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	transitionCtx, cancelTransitions := context.WithCancel(context.Background())
	l := &CheckoutLifecycle{
		mi:                cfg.MultiIndexer,
		cfgMgr:            cfg.ConfigManager,
		logger:                 logger,
		now:                    now,
		buildingRecoveryCutoff: now().Unix(),
		leases:                 cfg.ViewLeases,
		coordinators:      map[string]*CheckoutCoordinator{},
		started:           map[string][]*CheckoutCoordinator{},
		owed:              map[int64]struct{}{},
		familyRetries:     map[string]familyRetry{},
		refViewRetention:  cfg.RefViews.withDefaults(),
		indexBarrier:      cfg.indexBarrier,
		transitionCtx:     transitionCtx,
		cancelTransitions: cancelTransitions,
		transitionRuns:    map[string]*modeTransitionRun{},
		transitionQueue:   make(chan *modeTransitionRun, modeTransitionQueueLimit),
	}
	if l.leases == nil {
		l.leases = graphview.NewLeaseManager()
	}

	provider, ok := cfg.Graph.(interface {
		Catalog() *store_sqlite.Catalog
	})
	if !ok {
		return l, nil
	}
	l.catalog = provider.Catalog()
	// The coordinators build, publish, route and retire payload generations,
	// all of which are store operations rather than catalog ones. A backend
	// that answers with a catalog but is not the SQLite store keeps every
	// pre-layer behaviour and grows no coordinators.
	l.store, _ = cfg.Graph.(*store_sqlite.Store)

	rcfg := cfg.Reconcile
	if rcfg.AvailabilityGrace <= 0 || rcfg.RemovalGrace <= 0 {
		rcfg = reconcile.Default()
	}
	rec, err := reconcile.New(l.catalog, cleanupHooks{l: l}, rcfg,
		reconcile.WithClock(now), reconcile.WithLogger(l.logger))
	if err != nil {
		return nil, fmt.Errorf("indexer: build checkout reconciler: %w", err)
	}
	l.rec = rec
	return l, nil
}

// SetWatcherSource installs the accessor for the live file watcher. The
// watcher is built during warmup, long after the lifecycle, so it is read
// through a function rather than captured. The accessor must return a nil
// interface — not a typed nil — while no watcher exists.
func (l *CheckoutLifecycle) SetWatcherSource(fn func() RepoWatcher) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.watcherFn = fn
	l.mu.Unlock()
}

// SetNotifier installs the session/analysis fan-out.
func (l *CheckoutLifecycle) SetNotifier(n LifecycleNotifier) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.notifier = n
	l.mu.Unlock()
}

// SetBuildGate installs the gate that holds view build work while the daemon
// warms up.
//
// It is read when a coordinator or a ref-view manager is built, so it has to
// be installed before anything starts one — the daemon does it before warmup,
// which is before the seeding that brings the first coordinator up. Nothing
// else is gated: registering a checkout, seeding the catalog, reading a route
// and serving a published generation all run exactly as they do without it.
func (l *CheckoutLifecycle) SetBuildGate(gate *ViewBuildGate) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.gate = gate
	l.mu.Unlock()
}

// buildGate reads the installed gate, nil when nothing gates builds here.
func (l *CheckoutLifecycle) buildGate() *ViewBuildGate {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.gate
}

// Reconciler returns the lifecycle's reconciler, nil when the store has no
// catalog.
func (l *CheckoutLifecycle) Reconciler() *reconcile.Reconciler {
	if l == nil {
		return nil
	}
	return l.rec
}

// --- registration -------------------------------------------------------

// RegisterResult is what one registration did.
type RegisterResult struct {
	// Prefix is the repo prefix the checkout registered under.
	Prefix string
	// Index is the first index's result. It is nil when the repository was
	// already tracked or its dedicated promotion is still pending.
	Index *IndexResult
	// AlreadyTracked reports that the corpus already held this repository,
	// so only the identity and the side effects were brought up to date.
	AlreadyTracked bool
	// CheckoutID / Incarnation / FamilyID / GraphID are the catalog identity,
	// empty when the store has no catalog or git does not administer the path.
	CheckoutID  string
	Incarnation string
	FamilyID    string
	GraphID     string
	// TransitionID identifies a durable asynchronous promotion, when Pending.
	TransitionID string
	// Pending reports that registration succeeded but its immutable dedicated
	// view is waiting for the daemon build gate.
	Pending bool
	// CatalogErr is a registration failure that left the index in place.
	// It is reported rather than returned: the corpus is the user-visible
	// product of a track, and a catalog that could not record the identity
	// must not undo a successful index.
	CatalogErr error
}

// Register indexes a repository and records everything that follows from it.
//
// This is the one path behind every explicit track, whichever surface asked:
// index, family identity, checkout identity, tracking intent, dedicated-graph
// binding, watcher attach, config persist, family reconciliation, session
// invalidation. The source kind is the only thing that differs between
// surfaces.
//
// The reconciliation is what gives the repository's OTHER working copies their
// views. Tracking a repository is usually the first the daemon has heard of
// the worktrees beside it, and every one of them is an automatic checkout of
// the family this registration just gave a primary to.
func (l *CheckoutLifecycle) Register(
	ctx context.Context,
	entry config.RepoEntry,
	sourceKind store_sqlite.IntentSourceKind,
) (RegisterResult, error) {
	if l == nil || l.mi == nil {
		return RegisterResult{}, errors.New("indexer: checkout lifecycle is not wired")
	}
	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("resolve path %s: %w", entry.Path, err)
	}
	defer l.beginBatch()()

	entry.Path = absPath
	if entry.Name == "" {
		if prefix := l.dedicatedPrefixFor(ctx, absPath); prefix != "" {
			entry.Name = prefix
		}
	}
	prefix := config.ResolvePrefix(entry)

	// Implicit registrations and non-Git directories retain the ordinary
	// mutable corpus path. Explicit Git intent is recorded first so promotion
	// can build the full corpus from the captured immutable HEAD and publish
	// its own route without ever exposing a filesystem-backed base.
	if sourceKind == TrackSourceImplicit {
		result, trackErr := l.mi.TrackRepoCtx(ctx, entry)
		if trackErr != nil {
			return RegisterResult{}, trackErr
		}
		out := RegisterResult{Index: result, AlreadyTracked: result == nil, Prefix: prefix}
		if result != nil && result.RepoPrefix != "" {
			out.Prefix = result.RepoPrefix
		}
		identity, catalogErr := l.recordCheckout(ctx, out.Prefix, absPath, sourceKind, false)
		out.CheckoutID, out.Incarnation = identity.checkoutID, identity.incarnation
		out.FamilyID, out.GraphID = identity.familyID, identity.graphID
		out.CatalogErr = catalogErr
		l.attachWatcher(out.Prefix)
		l.saveConfig("track")
		l.reconcileFamilyNow(ctx, out.FamilyID, absPath)
		l.notifyTrackedSetChanged()
		return out, nil
	}

	identity, catalogErr := l.recordCheckout(ctx, prefix, absPath, sourceKind, false)
	out := RegisterResult{
		Prefix: prefix, CheckoutID: identity.checkoutID, Incarnation: identity.incarnation,
		FamilyID: identity.familyID, GraphID: identity.graphID, CatalogErr: catalogErr,
	}
	if catalogErr != nil {
		return out, catalogErr
	}
	if identity.checkoutID == "" {
		result, trackErr := l.mi.TrackRepoCtx(ctx, entry)
		if trackErr != nil {
			return out, trackErr
		}
		out.Index, out.AlreadyTracked = result, result == nil
		if result != nil && result.RepoPrefix != "" {
			out.Prefix = result.RepoPrefix
		}
		l.attachWatcher(out.Prefix)
		l.saveConfig("track")
		l.notifyTrackedSetChanged()
		return out, nil
	}

	promoted, run, promoteErr := l.startPromoteCheckout(ctx, identity.checkoutID, TrackSourceImplicit)
	if promoteErr == nil && run == nil && promoted.GraphID != "" {
		// A ready dedicated route survives daemon restart, but its process-local
		// repository shell does not. Re-entering TrackRepoCtx restores only that
		// shell because the durable route owns the immutable corpus.
		entry.Name = promoted.Prefix
		promoted.Index, promoteErr = l.mi.TrackRepoCtx(ctx, entry)
	}
	if promoteErr == nil && run != nil {
		gate := l.buildGate()
		if gate == nil || gate.IsOpen() {
			outcome, waitErr := waitModeTransition(ctx, run)
			if waitErr != nil {
				promoted.Pending = true
				promoteErr = waitErr
			} else {
				promoted = outcome.promotion
				promoteErr = outcome.err
			}
		} else {
			promoted.Pending = true
		}
	}
	out.Index = promoted.Index
	out.AlreadyTracked = promoted.Index == nil && !promoted.Pending
	out.TransitionID, out.Pending = promoted.TransitionID, promoted.Pending
	if promoted.Prefix != "" {
		out.Prefix = promoted.Prefix
	}
	if promoted.GraphID != "" {
		out.GraphID = promoted.GraphID
	}
	if promoteErr != nil {
		return out, promoteErr
	}
	if promoted.Pending {
		return out, nil
	}
	l.reconcileFamilyNow(ctx, out.FamilyID, absPath)
	l.notifyTrackedSetChanged()
	return out, nil
}

// RecordImplicit records a checkout nobody asked for.
//
// The auto-index path indexes the working directory on its own initiative,
// so the checkout is real but the intent is not: the family, checkout and
// graph-binding rows are written, and no tracking intent is. The watcher and
// the session invalidation match an explicit registration, since an
// implicitly indexed repository is served exactly like any other.
//
// The tracked-repository list is deliberately NOT persisted. The indexer adds
// the path to the in-memory configuration, and writing that out would put an
// entry in the user's config file for a path nobody asked to track — which
// the next boot's seeding would read back as explicit configuration and mint a
// manual_config intent for, turning an intent-less observation into intent one
// restart later.
func (l *CheckoutLifecycle) RecordImplicit(ctx context.Context, root string) error {
	if l == nil || l.mi == nil {
		return nil
	}
	prefix := l.ResolvePrefix(root)
	if prefix == "" {
		return fmt.Errorf("%w: %s", ErrCheckoutNotTracked, root)
	}
	defer l.beginBatch()()

	identity, err := l.recordCheckout(ctx, prefix, root, TrackSourceImplicit, false)
	l.attachWatcher(prefix)
	l.reconcileFamilyNow(ctx, identity.familyID, root)
	l.notifyTrackedSetChanged()
	return err
}

// checkoutIdentity is the catalog identity of one registered checkout.
type checkoutIdentity struct {
	familyID    string
	checkoutID  string
	incarnation string
	graphID     string
}

// recordCheckout writes the catalog rows one tracked root implies.
//
// seeding narrows it to a migration: an identity that already exists is left
// exactly as it is, so persisted clocks are honoured rather than reset and a
// second seeding pass writes the same rows as the first.
func (l *CheckoutLifecycle) recordCheckout(
	ctx context.Context,
	prefix, root string,
	source store_sqlite.IntentSourceKind,
	seeding bool,
) (checkoutIdentity, error) {
	if l.catalog == nil || prefix == "" {
		return checkoutIdentity{}, nil
	}
	inv, err := gitstate.Inventory(ctx, root)
	if err != nil {
		// A directory git does not administer has no family to belong to.
		// It is still indexed and served; it simply has no lifecycle
		// identity, which is what the catalog says by holding no row.
		return checkoutIdentity{}, nil
	}
	record := recordForRoot(inv, root)
	if record == nil || record.AdminName == "" {
		return checkoutIdentity{}, fmt.Errorf(
			"git does not list %s as a worktree of %s", root, inv.CommonDir)
	}

	now := l.now()
	familyID := FamilyIDFor(inv.CommonDir)
	if err := l.upsertFamily(ctx, familyID, inv.CommonDir, now.Unix()); err != nil {
		return checkoutIdentity{}, err
	}

	identity := checkoutIdentity{familyID: familyID}
	existing, err := l.checkoutByAdminName(ctx, familyID, record.AdminName)
	if err != nil {
		return identity, err
	}
	switch {
	case existing != nil:
		identity.checkoutID, identity.incarnation = existing.CheckoutID, existing.Incarnation
		if !seeding {
			if err := l.confirmPresent(ctx, *existing, record, inv, now); err != nil {
				return identity, err
			}
		}
	default:
		minted, err := l.allocateCheckout(ctx, familyID, root, record, inv, now)
		if err != nil {
			return identity, err
		}
		identity.checkoutID, identity.incarnation = minted.CheckoutID, minted.Incarnation
	}

	// A config entry may outlive an authorized demotion until its cleanup
	// worker removes the dedicated corpus. On restart that stale entry must not
	// resurrect the intent the durable transition already revoked.
	restoreExplicitIntent := source != TrackSourceImplicit &&
		!(seeding && existing != nil && existing.ActiveIntentTransitionID != "")
	if restoreExplicitIntent {
		intent := store_sqlite.TrackingIntent{
			IntentID:      uuid.NewV7().String(),
			CheckoutID:    identity.checkoutID,
			SourceKind:    source,
			SourceLocator: root,
			Active:        true,
			CreatedAt:     now.Unix(),
		}
		if err := l.catalog.UpsertTrackingIntent(ctx, intent); err != nil {
			return identity, err
		}
	}

	graphID, err := l.bindDedicatedGraph(ctx, familyID, identity.checkoutID, prefix)
	if err != nil {
		return identity, err
	}
	identity.graphID = graphID

	if err := l.recordPathEvidence(ctx, identity.checkoutID, root, now, seeding); err != nil {
		return identity, err
	}
	return identity, nil
}

// upsertFamily writes the family row, preserving the creation timestamp of
// one that already exists.
func (l *CheckoutLifecycle) upsertFamily(ctx context.Context, familyID, commonDir string, now int64) error {
	family := store_sqlite.RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: commonDir,
		State:             reconcile.FamilyStateReady,
		CreatedAt:         now,
		LastSeen:          now,
	}
	existing, ok, err := l.catalog.GetRepositoryFamily(ctx, familyID)
	if err != nil {
		return err
	}
	if ok {
		// The primary epoch is a compare-and-set token; rewriting it here
		// would silently invalidate a promotion another actor is holding.
		family.CreatedAt = existing.CreatedAt
		family.PrimaryEpoch = existing.PrimaryEpoch
		family.DisplayRemote = existing.DisplayRemote
	}
	return l.catalog.UpsertRepositoryFamily(ctx, family)
}

// checkoutByAdminName finds a family's checkout by the name git administers
// it under, which is the identity the lifecycle keys on.
func (l *CheckoutLifecycle) checkoutByAdminName(
	ctx context.Context, familyID, adminName string,
) (*store_sqlite.Checkout, error) {
	checkouts, err := l.catalog.ListCheckouts(ctx, familyID)
	if err != nil {
		return nil, err
	}
	for i := range checkouts {
		if checkouts[i].AdminName == adminName {
			return &checkouts[i], nil
		}
	}
	return nil, nil
}

// allocateCheckout mints a durable identity through the guarded allocator, so
// two surfaces racing to track the same working copy end with one row.
func (l *CheckoutLifecycle) allocateCheckout(
	ctx context.Context,
	familyID, root string,
	record *gitstate.WorktreeRecord,
	inv *gitstate.FamilyInventory,
	now time.Time,
) (store_sqlite.Checkout, error) {
	checkout := store_sqlite.Checkout{
		CheckoutID:     uuid.NewV7().String(),
		Incarnation:    uuid.NewV7().String(),
		FamilyID:       familyID,
		RootPath:       root,
		GitDir:         gitDirFor(inv, record),
		AdminName:      record.AdminName,
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeAutomatic,
		EffectiveMode:  store_sqlite.CheckoutModeAutomatic,
		Locked:         record.Locked,
		Prunable:       record.Prunable,
		HeadRef:        record.HEADRef,
		HeadCommit:     record.HEADOID,
		LastAccessible: now.Unix(),
		LastSeen:       now.Unix(),
	}
	err := l.catalog.AllocateCheckout(ctx, checkout)
	if err == nil {
		return checkout, nil
	}
	if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		return store_sqlite.Checkout{}, err
	}
	// Another actor allocated this administrative name first. Its row is the
	// identity; adopting it is what keeps one working copy to one identity.
	winner, lookupErr := l.checkoutByAdminName(ctx, familyID, record.AdminName)
	if lookupErr != nil {
		return store_sqlite.Checkout{}, lookupErr
	}
	if winner == nil {
		return store_sqlite.Checkout{}, err
	}
	return *winner, nil
}

// confirmPresent tells an existing identity that its root just answered.
//
// An explicit track is first-hand evidence of presence, so it clears both
// clocks the same way a reconciliation pass would: a path that was inside its
// removal grace must not be deleted moments after someone re-tracked it.
func (l *CheckoutLifecycle) confirmPresent(
	ctx context.Context,
	existing store_sqlite.Checkout,
	record *gitstate.WorktreeRecord,
	inv *gitstate.FamilyInventory,
	now time.Time,
) error {
	req := store_sqlite.UpdateCheckoutObservationRequest{
		CheckoutID:     existing.CheckoutID,
		Incarnation:    existing.Incarnation,
		State:          store_sqlite.CheckoutStateReady,
		RootPath:       record.Path,
		GitDir:         gitDirFor(inv, record),
		Locked:         record.Locked,
		Prunable:       record.Prunable,
		HeadRef:        record.HEADRef,
		HeadCommit:     record.HEADOID,
		HeadTree:       existing.HeadTree,
		LastAccessible: now.Unix(),
		LastSeen:       now.Unix(),
	}
	if err := l.catalog.UpdateCheckoutObservation(ctx, req); err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			// Another actor re-keyed the row between the read and this
			// write. Its incarnation is the current one; the next pass
			// observes the path under it.
			return nil
		}
		return err
	}
	return nil
}

// claimDedicated makes an identity somebody has just tracked explicitly a
// dedicated checkout.
//
// A worktree the reconciler observed before anyone asked for it is minted
// automatic: it is served from the family's primary corpus through a composed
// view. An explicit track says the opposite — this working copy is to have a
// corpus of its own — and the mode is what the rest of the daemon reads to
// decide which of the two a checkout gets. Registering over an adopted
// identity without moving the mode would index the repository and then keep
// serving it from someone else's graph.
//
// A lost incarnation guard is not an error: another actor re-keyed the row, so
// the identity this registration read is not the current one and the next pass
// records the mode against the row that is.
func (l *CheckoutLifecycle) claimDedicated(
	ctx context.Context, existing store_sqlite.Checkout, now time.Time,
) error {
	if existing.DesiredMode == store_sqlite.CheckoutModeDedicated &&
		existing.EffectiveMode == store_sqlite.CheckoutModeDedicated {
		return nil
	}
	err := l.catalog.UpdateCheckoutState(ctx, store_sqlite.UpdateCheckoutStateRequest{
		CheckoutID:    existing.CheckoutID,
		Incarnation:   existing.Incarnation,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      now.Unix(),
	})
	if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		return nil
	}
	return err
}

// bindDedicatedGraph binds a checkout to the repo prefix its nodes live
// under, and makes it the family's primary base when the family has none.
func (l *CheckoutLifecycle) bindDedicatedGraph(
	ctx context.Context, familyID, checkoutID, prefix string,
) (string, error) {
	if checkoutID == "" {
		return "", nil
	}
	reuseOwner := func() (string, bool, error) {
		existing, found, err := l.catalog.GetDedicatedGraphByOwner(ctx, checkoutID)
		if err != nil || !found {
			return "", found, err
		}
		if existing.FamilyID != familyID {
			return "", true, fmt.Errorf(
				"%w: checkout %s already owns dedicated graph %s in family %s",
				store_sqlite.ErrCatalogStaleGuard, checkoutID, existing.GraphID, existing.FamilyID,
			)
		}
		return existing.GraphID, true, nil
	}
	if graphID, found, err := reuseOwner(); found || err != nil {
		return graphID, err
	}

	graphID := GraphIDFor(prefix)
	existing, found, err := l.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return "", err
	}
	if found {
		if existing.OwnerCheckoutID != checkoutID || existing.FamilyID != familyID ||
			existing.RepoPrefix != prefix {
			return "", fmt.Errorf("%w: dedicated graph %s binding moved",
				store_sqlite.ErrCatalogStaleGuard, graphID)
		}
		return graphID, nil
	}
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return "", err
	}
	primaryHeld := false
	for _, g := range graphs {
		if g.IsPrimaryBase && g.GraphID != graphID {
			primaryHeld = true
			break
		}
	}
	row := store_sqlite.DedicatedGraph{
		GraphID:         graphID,
		OwnerCheckoutID: checkoutID,
		RepoPrefix:      prefix,
		FamilyID:        familyID,
		IsPrimaryBase:   !primaryHeld,
		State:           reconcile.GraphStateReady,
	}
	if err := l.catalog.UpsertDedicatedGraph(ctx, row); err != nil {
		if boundGraphID, found, lookupErr := reuseOwner(); found || lookupErr != nil {
			return boundGraphID, lookupErr
		}
		if !row.IsPrimaryBase {
			return "", err
		}
		// A concurrent registration won the family's primary slot; the
		// partial unique index is what refused this one. Bind as an
		// ordinary dedicated graph instead.
		row.IsPrimaryBase = false
		if retryErr := l.catalog.UpsertDedicatedGraph(ctx, row); retryErr != nil {
			if boundGraphID, found, lookupErr := reuseOwner(); found || lookupErr != nil {
				return boundGraphID, lookupErr
			}
			return "", retryErr
		}
	}
	return graphID, nil
}

// recordPathEvidence stores the filesystem sample a later removal has to be
// compared against. Without it a vanished root can never be told apart from
// an unmounted volume, so the checkout would sit in availability grace
// forever instead of being cleaned up.
//
// A seeding pass never overwrites an existing sample: the stored one is the
// older observation, and the removal test wants the sample from when the root
// was last known good.
func (l *CheckoutLifecycle) recordPathEvidence(
	ctx context.Context, checkoutID, root string, now time.Time, seeding bool,
) error {
	if checkoutID == "" {
		return nil
	}
	stored, present, err := l.catalog.GetCheckoutPathEvidence(ctx, checkoutID)
	if err != nil {
		return err
	}
	if present && seeding {
		return nil
	}
	fresh := reconcile.SampledPathEvidence(gitstate.SamplePathEvidence(root))
	return l.catalog.UpsertCheckoutPathEvidence(ctx,
		fresh.CatalogRow(checkoutID, now.Unix(), stored.SampleGeneration+1))
}

// --- forgetting ---------------------------------------------------------

// UntrackResult is what one explicit forget did.
type UntrackResult struct {
	Prefix     string
	CheckoutID string
	// TransitionID names a durable demotion. It is empty for immediate
	// eviction, forget, and primary-closure plans.
	TransitionID string
	// Pending reports that a lifecycle-owned demotion worker is still running
	// or left its durable transition available for a retry.
	Pending      bool
	NodesRemoved int
	EdgesRemoved int
	// Revoked names the intent sources that were withdrawn.
	Revoked []string
	// Dependents is the preview of what the untrack took with it.
	Dependents []reconcile.Dependent
	// Plan is the transaction that ran.
	Plan UntrackPlan
	// Demoted reports that the checkout kept its identity and moved to the
	// family's automatic lane instead of being removed.
	Demoted bool
}

// Untrack stops tracking one checkout, whichever surface asked.
//
// What that means depends on what the family can still serve the checkout
// from, so the plan is read from the catalog first and executed second — see
// PreviewUntrack, which is the same decision and the payload a caller renders
// before asking.
//
// The order inside every plan is the point: every revocable tracking intent is
// withdrawn first (a non-revocable one aborts before anything is torn down),
// then the transaction runs under the checkout's incarnation — or the family's
// primary epoch — and drives the cleanup hooks, so the same sequence happens no
// matter who called.
func (l *CheckoutLifecycle) Untrack(ctx context.Context, pathOrPrefix string) (UntrackResult, error) {
	preview, err := l.PreviewUntrack(ctx, pathOrPrefix)
	if err != nil {
		return UntrackResult{}, err
	}
	return l.ApplyUntrack(ctx, preview)
}

// ApplyUntrack executes one previewed plan.
//
// It is separate from PreviewUntrack because the destructive plans are shown
// before they are run: a caller renders the preview, asks, and then hands the
// same value back here — so what a user confirmed and what happens are one
// decision rather than two reads of a catalog that may have moved between
// them. The guards inside each plan are what catch a catalog that did.
//
// The first of those guards is the identity itself. A checkout that was
// re-keyed between the preview and the confirm is a different incarnation of
// the path, and the plan the caller was shown was decided against the one it
// replaced — so the confirm refuses rather than demoting or forgetting an
// identity nobody was asked about. Nothing has been revoked at that point.
func (l *CheckoutLifecycle) ApplyUntrack(ctx context.Context, preview UntrackPreview) (UntrackResult, error) {
	return l.applyUntrack(ctx, preview, true)
}

// StartApplyUntrack durably admits a demotion and returns once its daemon-owned
// worker is scheduled. Plans without a durable mode transition remain
// synchronous. ApplyUntrack is the wait-by-default compatibility wrapper.
func (l *CheckoutLifecycle) StartApplyUntrack(ctx context.Context, preview UntrackPreview) (UntrackResult, error) {
	return l.applyUntrack(ctx, preview, false)
}

func (l *CheckoutLifecycle) applyUntrack(
	ctx context.Context, preview UntrackPreview, wait bool,
) (UntrackResult, error) {
	out := UntrackResult{
		Prefix:     preview.Prefix,
		CheckoutID: preview.CheckoutID,
		Plan:       preview.Plan,
		Dependents: preview.Closure,
	}
	if preview.Plan == UntrackPlanEvict {
		// No catalog identity: a store without a catalog, or a directory git
		// does not administer. The side effects are the same ones the hooks
		// run, in the same order.
		out.NodesRemoved, out.EdgesRemoved = l.evictRepo(preview.Prefix)
		return out, nil
	}
	if preview.Plan == UntrackPlanBlocked {
		return out, blockedUntrack(preview)
	}

	checkout, err := l.checkoutStateOf(ctx, preview.CheckoutID)
	if err != nil {
		return out, err
	}
	if preview.Incarnation != "" && checkout.Incarnation != preview.Incarnation {
		return out, fmt.Errorf(
			"%w: checkout %s was re-keyed between the preview and the confirm; preview the untrack again",
			store_sqlite.ErrCatalogStaleGuard, preview.Prefix)
	}

	// The demote plan is the one whose precondition is a pair of rows rather
	// than the checkout's own, so it is re-asked here — before anything is
	// revoked, so a refusal leaves the tracked set exactly as the preview
	// found it.
	var owned, primary *store_sqlite.DedicatedGraph
	if preview.Plan == UntrackPlanDemote {
		if owned, primary, err = l.familyGraphsFor(ctx, checkout); err != nil {
			return out, err
		}
		if err := demotableNow(checkout, preview.Prefix, owned, primary); err != nil {
			return out, err
		}
	}

	// The eviction happens inside the cleanup sagas, so capture the last index
	// before authorizing one. Authorization is the first write: its catalog
	// transaction rechecks every preview guard, preflights every active intent,
	// then revokes revocable intent and records either the cleanup journal or
	// the demotion transition atomically.
	before := l.mi.GetMetadata(preview.Prefix)
	opCtx := context.WithoutCancel(ctx)
	appendRevoked := func(revocation reconcile.IntentRevocation) {
		for _, intent := range revocation.Revoked {
			out.Revoked = append(out.Revoked, string(intent.SourceKind))
		}
	}

	switch preview.Plan {
	case UntrackPlanDemote:
		ownedGraphID := ""
		if owned != nil {
			ownedGraphID = owned.GraphID
		}
		authorization, err := l.rec.AuthorizeDemotion(
			opCtx, checkout, ownedGraphID, primary.GraphID, preview.PrimaryEpoch)
		appendRevoked(authorization.Revocation)
		if err != nil {
			return out, err
		}
		out.TransitionID = authorization.Transition.TransitionID
		run := l.scheduleModeTransition(authorization.Transition)
		if !wait {
			out.Pending = true
			return out, nil
		}
		outcome, waitErr := waitModeTransition(ctx, run)
		if waitErr != nil {
			out.Pending = true
			return out, waitErr
		}
		if outcome.err != nil {
			out.Pending = true
			return out, outcome.err
		}
		out.Demoted = outcome.demoted
	case UntrackPlanPrimaryClosure:
		revocation, err := l.rec.RetirePrimaryClosureExplicit(
			opCtx, preview.GraphID, checkout.CheckoutID, checkout.Incarnation,
			checkout.FamilyID, preview.PrimaryEpoch)
		appendRevoked(revocation)
		if err != nil {
			return out, err
		}
	case UntrackPlanForget:
		revocation, err := l.rec.ForgetCheckoutExplicit(
			opCtx, checkout.CheckoutID, checkout.Incarnation,
			checkout.FamilyID, preview.GraphID)
		appendRevoked(revocation)
		if err != nil {
			return out, err
		}
	default:
		return out, fmt.Errorf("indexer: unsupported untrack plan %q", preview.Plan)
	}

	if before != nil {
		out.NodesRemoved, out.EdgesRemoved = before.NodeCount, before.EdgeCount
	}
	// The sagas evict through ReleaseGraph. A checkout that never had a
	// graph binding still has to leave the corpus.
	if l.mi.GetMetadata(preview.Prefix) != nil {
		out.NodesRemoved, out.EdgesRemoved = l.evictRepo(preview.Prefix)
	}
	return out, nil
}

// familyGraphsFor reads the graph a checkout owns and its family's primary,
// which is the pair every mode change is decided against.
func (l *CheckoutLifecycle) familyGraphsFor(
	ctx context.Context, checkout store_sqlite.Checkout,
) (owned, primary *store_sqlite.DedicatedGraph, err error) {
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return nil, nil, err
	}
	for i := range graphs {
		if graphs[i].OwnerCheckoutID == checkout.CheckoutID {
			owned = &graphs[i]
		}
		if graphs[i].IsPrimaryBase {
			primary = &graphs[i]
		}
	}
	return owned, primary, nil
}

// demotableNow re-asks, at confirm time, the question the demote plan was
// chosen by: is the checkout's own graph still not the family's base, and is a
// different ready primary still there to serve it from.
//
// Both are catalog rows another actor can move between the preview and the
// confirm, and the demotion cannot survive either of them moving. Rehoming onto
// a primary that has gone leaves the checkout automatic with nothing under it;
// rehoming onto its OWN graph — the case where that graph became the primary —
// flips the checkout to automatic and then cannot retire the corpus it is being
// served from, leaving a family whose base is owned by an automatic checkout.
func demotableNow(
	checkout store_sqlite.Checkout, prefix string, owned, primary *store_sqlite.DedicatedGraph,
) error {
	var blockers []string
	if owned != nil && owned.IsPrimaryBase {
		blockers = append(blockers, "graph "+owned.GraphID+
			" has become the primary base of family "+checkout.FamilyID+" since the preview")
	}
	if primary == nil || primary.OwnerCheckoutID == checkout.CheckoutID ||
		primary.State != reconcile.GraphStateReady {
		blockers = append(blockers, "family "+checkout.FamilyID+
			" has no other ready primary corpus to serve this checkout from")
	}
	if len(blockers) == 0 {
		return nil
	}
	blockers = append(blockers, "preview the untrack again to see what it would do now")
	return fmt.Errorf("%w: %s: %s", ErrUntrackBlocked, prefix, strings.Join(blockers, "; "))
}

// --- reload -------------------------------------------------------------

// ReloadResult counts what one configuration reload did.
type ReloadResult struct {
	Added   int
	Removed int
	// Pending counts entries whose removal was recorded as an intent
	// transition instead of being applied.
	Pending int
	// Refreshed is the number of tracked repositories whose per-repo config
	// was re-read.
	Refreshed int
}

// ApplyReload brings the tracked set in line with the configuration file.
//
// Additions go through the registration helper, so a repository added by
// editing the config gets the same identity, watcher and invalidation an
// explicit track would have given it. Removals go through the reconciler's
// retirement rule rather than a direct eviction: an entry that cannot be
// dropped safely records a pending transition and stays, which is what stops
// a configuration edit from silently deleting a corpus.
func (l *CheckoutLifecycle) ApplyReload(ctx context.Context) (ReloadResult, error) {
	if l == nil || l.mi == nil || l.cfgMgr == nil {
		return ReloadResult{}, errors.New("indexer: checkout lifecycle is not wired for reload")
	}
	// One fan-out for the whole diff: every add and every removal below
	// changes the tracked set, and telling the sessions after each one would
	// pay for the same answer as many times as the diff is long.
	defer l.beginBatch()()

	out := ReloadResult{Refreshed: l.mi.RefreshRepoConfigs()}

	// Match configured entries to tracked instances by ROOT PATH. A worktree
	// tracked as an independent instance registers under a derived prefix, so
	// a recomputed prefix would not recognise it as wanted.
	trackedByRoot := map[string]string{}
	for prefix, meta := range l.mi.AllMetadata() {
		if meta != nil {
			trackedByRoot[meta.RootPath] = prefix
		}
	}

	wanted := map[string]bool{}
	for _, entry := range l.cfgMgr.Global().Repos {
		abs, err := filepath.Abs(entry.Path)
		if err != nil {
			abs = entry.Path
		}
		if prefix, ok := trackedByRoot[abs]; ok {
			wanted[prefix] = true
			continue
		}
		res, err := l.Register(ctx, entry, TrackSourceConfig)
		if err != nil {
			l.logger.Warn("reload: track failed",
				zap.String("path", entry.Path), zap.Error(err))
			continue
		}
		out.Added++
		if res.Prefix != "" {
			wanted[res.Prefix] = true
		}
	}

	for prefix := range l.mi.AllMetadata() {
		if wanted[prefix] {
			continue
		}
		outcome, err := l.retireOnReload(ctx, prefix)
		if err != nil {
			l.logger.Warn("reload: retire failed",
				zap.String("prefix", prefix), zap.Error(err))
			continue
		}
		switch outcome {
		case reconcile.OutcomeTransitionPending:
			out.Pending++
		default:
			out.Removed++
		}
	}
	return out, nil
}

// retireOnReload applies the reconciler's retirement rule to one prefix that
// left the configuration.
func (l *CheckoutLifecycle) retireOnReload(ctx context.Context, prefix string) (reconcile.RetireOutcome, error) {
	checkout, err := l.checkoutForPrefix(ctx, prefix)
	if err != nil {
		return "", err
	}
	if checkout == nil {
		// No identity to reason about — a store without a catalog, or a
		// directory git does not administer. Keeping the pre-catalog
		// behaviour is what stops such a repository from becoming
		// impossible to remove.
		l.evictRepo(prefix)
		return reconcile.OutcomeForgotten, nil
	}
	outcome, err := l.rec.RetireCheckout(ctx, checkout.CheckoutID, checkout.Incarnation, "reload_removed_from_config")
	if err != nil {
		return "", err
	}
	if outcome == reconcile.OutcomeForgotten && l.mi.GetMetadata(prefix) != nil {
		l.evictRepo(prefix)
	}
	return outcome, nil
}

// --- periodic sweep -----------------------------------------------------

// SweepReport is one janitor pass over every family the daemon knows.
type SweepReport struct {
	// Families is the number of families reconciled.
	Families int
	// Reports are the per-family verdicts, in the order they were taken.
	Reports []reconcile.FamilyReport
	// Removed counts checkouts the pass forgot or retired.
	Removed int
	// Coordinators is how many automatic checkouts hold a live coordinator
	// once the pass has applied the dispositions it read.
	Coordinators int
	// Retired counts payload generations the pass collected that an earlier
	// offer could not: the ones a coordinator's own retire was refused for,
	// and the ones a checkout that stopped being served left behind.
	Retired int
	// RefViewsRetired counts ref-view generations the retention bounds
	// collected. They are counted apart from Retired because nothing else
	// would ever offer them: a ref view belongs to no checkout.
	RefViewsRetired int
}

// Sweep resumes unfinished cleanups and reconciles every known family.
//
// It replaces the old "the directory is gone, evict it" check. That test
// could not tell a deleted worktree from an unmounted volume, so it had to be
// narrowed to linked worktrees to be safe at all; the reconciler decides on
// evidence and two separate clocks, which is what lets it act on any checkout
// without risking a corpus over a transient stat failure.
func (l *CheckoutLifecycle) Sweep(ctx context.Context) (SweepReport, error) {
	var out SweepReport
	if l == nil || l.rec == nil {
		return out, nil
	}
	// One fan-out for the whole sweep, however many families it touches.
	defer l.beginBatch()()

	var errs []error
	if err := l.rec.Resume(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := l.resumeModeTransitions(ctx); err != nil {
		errs = append(errs, err)
	}
	for _, fam := range l.knownFamilies(ctx) {
		report, err := l.rec.ReconcileFamily(ctx, fam.familyID, fam.probeDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("family %s: %w", fam.familyID, err))
			continue
		}
		out.Families++
		out.Reports = append(out.Reports, report)
		l.scheduleFamilyRetry(report)
		for _, checkout := range report.Checkouts {
			switch checkout.Action {
			case reconcile.ActionForgotten, reconcile.ActionPrimaryClosureRetired:
				out.Removed++
			}
		}
		l.applyCoordinators(ctx, report)
	}
	out.Coordinators = l.liveCoordinators("")
	out.Retired = l.sweepRetirements(ctx)
	out.RefViewsRetired = l.sweepRefViewRetention(ctx)
	recordSweepGauges(out)
	if out.Removed > 0 {
		// The cleanup hooks drop the removed repositories from the in-memory
		// configuration; without this the removal is forgotten on restart.
		l.saveConfig("janitor")
		l.notifyTrackedSetChanged()
	}
	return out, errors.Join(errs...)
}

// recordSweepGauges sets the levels only a whole-population pass can know.
//
// The two grace clocks are the ones that matter operationally: a checkout in
// availability grace is one whose layers are about to be purged, and one in
// removal grace is one about to be forgotten. Both are counted from the states
// the pass just wrote, so the gauge is as current as the sweep that set it and
// never drifts the way an incrementally maintained level would.
func recordSweepGauges(report SweepReport) {
	availability, removal := 0, 0
	for _, family := range report.Reports {
		for _, checkout := range family.Checkouts {
			switch checkout.Action {
			case reconcile.ActionAvailabilityGraceStarted, reconcile.ActionAvailabilityHeld:
				availability++
			case reconcile.ActionRemovalGraceStarted, reconcile.ActionRemovalHeld:
				removal++
			}
		}
	}
	viewmetrics.SetGauge(viewmetrics.Families, int64(report.Families))
	viewmetrics.SetGauge(viewmetrics.AvailabilityClocks, int64(availability))
	viewmetrics.SetGauge(viewmetrics.RemovalClocks, int64(removal))
}

// scheduleFamilyRetry arms the earliest grace deadline in one family. A later
// report replaces or cancels the timer, so filesystem events and scheduled
// expiry share the same single reconciliation path.
func (l *CheckoutLifecycle) scheduleFamilyRetry(report reconcile.FamilyReport) {
	deadline := int64(0)
	for _, checkout := range report.Checkouts {
		if checkout.RetryAt > 0 && (deadline == 0 || checkout.RetryAt < deadline) {
			deadline = checkout.RetryAt
		}
	}
	l.scheduleFamilyRetryAt(report.FamilyID, deadline)
}

func (l *CheckoutLifecycle) scheduleFamilyRetryAt(familyID string, deadline int64) {
	if l == nil || familyID == "" {
		return
	}
	l.retryMu.Lock()
	defer l.retryMu.Unlock()
	if current, ok := l.familyRetries[familyID]; ok {
		if current.deadline == deadline {
			return
		}
		current.timer.Stop()
		delete(l.familyRetries, familyID)
	}
	if deadline <= 0 {
		return
	}
	delay := time.Unix(deadline, 0).Sub(l.now())
	if delay <= 0 {
		delay = time.Millisecond
	}
	timer := time.AfterFunc(delay, func() {
		l.runFamilyRetry(familyID, deadline)
	})
	l.familyRetries[familyID] = familyRetry{deadline: deadline, timer: timer}
}

func (l *CheckoutLifecycle) runFamilyRetry(familyID string, deadline int64) {
	l.retryMu.Lock()
	current, ok := l.familyRetries[familyID]
	if !ok || current.deadline != deadline {
		l.retryMu.Unlock()
		return
	}
	delete(l.familyRetries, familyID)
	l.retryMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	_, err := l.ReconcileFamily(ctx, familyID)
	cancel()
	if err == nil || errors.Is(err, store_sqlite.ErrCatalogNotFound) {
		return
	}
	l.logger.Warn("checkout lifecycle: scheduled family reconciliation failed",
		zap.String("family", familyID), zap.Error(err))
	l.scheduleFamilyRetryAt(familyID, l.now().Add(5*time.Second).Unix())
}

func familyReportRemoved(report reconcile.FamilyReport) bool {
	for _, checkout := range report.Checkouts {
		switch checkout.Action {
		case reconcile.ActionForgotten, reconcile.ActionPrimaryClosureRetired:
			return true
		}
	}
	return false
}

// reconcileFamilyNow reconciles one family and applies its coordinator
// dispositions immediately. Failures stay best-effort for registration and
// startup; the next topology event or scheduled sweep asks again.
func (l *CheckoutLifecycle) reconcileFamilyNow(ctx context.Context, familyID, fallbackDir string) {
	if l == nil || l.rec == nil || familyID == "" {
		return
	}
	report, err := l.rec.ReconcileFamily(ctx, familyID, l.probeDirFor(ctx, familyID, fallbackDir))
	if err != nil {
		l.logger.Debug("checkout lifecycle: could not reconcile the family",
			zap.String("family", familyID), zap.Error(err))
		return
	}
	l.applyCoordinators(ctx, report)
	l.scheduleFamilyRetry(report)
	if familyReportRemoved(report) {
		l.saveConfig("reconcile")
		l.notifyTrackedSetChanged()
	}
}

// familyProbe is one family and the directory to read its inventory from.
type familyProbe struct {
	familyID string
	probeDir string
}

// knownFamilies enumerates the families reachable from what is tracked and
// from what is configured.
//
// The family of a prefix is read from its dedicated-graph binding rather than
// from the filesystem, so a checkout whose root has vanished is still
// reconciled — that root is exactly the one that cannot answer.
//
// The corpus alone is not enough to enumerate from. Boot skips a configured
// repository whose root cannot be stat'ed, which leaves it with catalog rows
// and no corpus metadata; enumerating from the corpus only would drop exactly
// the checkout that availability handling exists for. So the configured
// entries are resolved to their families too, by the same prefix rule the
// startup seeding uses, and the two sets are unioned.
//
// The probe directory is chosen for the family, not for the checkout: any
// still-reachable checkout root will do, and the family's shared git
// directory is the fallback that keeps working when every worktree root is
// gone but the repository is not.
func (l *CheckoutLifecycle) knownFamilies(ctx context.Context) []familyProbe {
	seen := map[string]bool{}
	var out []familyProbe
	add := func(familyID, fallbackDir string) {
		if familyID == "" || seen[familyID] {
			return
		}
		seen[familyID] = true
		out = append(out, familyProbe{
			familyID: familyID,
			probeDir: l.probeDirFor(ctx, familyID, fallbackDir),
		})
	}

	for prefix, meta := range l.mi.AllMetadata() {
		if meta == nil {
			continue
		}
		add(l.familyForPrefix(ctx, prefix), meta.RootPath)
	}

	if l.cfgMgr == nil {
		return out
	}
	for _, entry := range l.cfgMgr.Global().Repos {
		abs, err := filepath.Abs(entry.Path)
		if err != nil {
			abs = entry.Path
		}
		prefix := l.ResolvePrefix(abs)
		if prefix == "" {
			prefix = EffectiveRepoPrefix(l.cfgMgr, entry)
		}
		add(l.familyForPrefix(ctx, prefix), abs)
	}
	return out
}

// familyForPrefix reads the family a repo prefix is bound to, empty when the
// prefix has no dedicated-graph binding.
func (l *CheckoutLifecycle) familyForPrefix(ctx context.Context, prefix string) string {
	if l.catalog == nil || prefix == "" {
		return ""
	}
	binding, ok, err := l.catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
	if err != nil || !ok {
		return ""
	}
	return binding.FamilyID
}

// probeDirFor picks the directory a family's inventory is read from.
func (l *CheckoutLifecycle) probeDirFor(ctx context.Context, familyID, fallback string) string {
	checkouts, err := l.catalog.ListCheckouts(ctx, familyID)
	if err == nil {
		for _, checkout := range checkouts {
			if checkout.RootPath != "" && dirExists(checkout.RootPath) {
				return checkout.RootPath
			}
		}
	}
	if family, ok, err := l.catalog.GetRepositoryFamily(ctx, familyID); err == nil && ok {
		if dirExists(family.CommonDirIdentity) {
			return family.CommonDirIdentity
		}
	}
	return fallback
}

// --- per-checkout coordinators ------------------------------------------

// applyCoordinators turns one family's reconciliation verdicts into the
// coordinator registry's shape.
//
// The reconciler has already decided everything the decision needs: which
// identities are durable, what state each is in, and whether the family has a
// primary to serve from. A checkout keeps a coordinator exactly while it is a
// ready automatic checkout in a family with a primary dedicated graph, and
// loses it the moment any of those stops being true — an availability
// expiry, a forget, a primary that went away with its closure.
//
// The mode is read from the catalog rather than from the report, which carries
// states and actions but not modes. A dedicated checkout — the primary itself,
// or a worktree someone tracked explicitly — is served from its own corpus and
// has nothing for a coordinator to do, and the route it may still hold from the
// automatic lane it came from is withdrawn here. Only a coordinator ever writes
// one, so a route under a dedicated checkout routes nothing and holds two
// generations out of the retirement scan until it goes.
func (l *CheckoutLifecycle) applyCoordinators(ctx context.Context, report reconcile.FamilyReport) {
	if l == nil || l.store == nil || l.catalog == nil {
		return
	}
	for _, entry := range report.Checkouts {
		if entry.CheckoutID == "" || !entry.Durable {
			continue
		}
		if entry.State != store_sqlite.CheckoutStateReady {
			l.dropCoordinator(entry.CheckoutID)
			l.withdrawStaleRoute(ctx, entry.CheckoutID)
			continue
		}
		checkout, found, err := l.catalog.GetCheckout(ctx, entry.CheckoutID)
		if err != nil || !found {
			l.dropCoordinator(entry.CheckoutID)
			continue
		}
		graphID := report.PrimaryGraphID
		if checkout.EffectiveMode == store_sqlite.CheckoutModeDedicated {
			prefix := l.prefixForCheckout(ctx, checkout.CheckoutID)
			graphID = GraphIDFor(prefix)
			graph, graphFound, graphErr := l.catalog.GetDedicatedGraph(ctx, graphID)
			if graphErr != nil || !graphFound || graph.OwnerCheckoutID != checkout.CheckoutID {
				l.dropCoordinator(entry.CheckoutID)
				l.withdrawStaleRoute(ctx, entry.CheckoutID)
				continue
			}
		}
		if graphID == "" {
			l.dropCoordinator(entry.CheckoutID)
			l.withdrawStaleRoute(ctx, entry.CheckoutID)
			continue
		}
		l.ensureCoordinator(ctx, graphID, checkout)
	}
}

// ensureCoordinator brings up the coordinator for one automatic checkout, or
// leaves the running one alone.
//
// Everything the coordinator stamps on its payload is the PRIMARY's: the repo
// prefix, the workspace and project slugs, and the index configuration. The
// layers compose over the primary's corpus, so a generation stamped with
// anything else would land beside that corpus instead of over it.
func (l *CheckoutLifecycle) ensureCoordinator(
	ctx context.Context, primaryGraphID string, checkout store_sqlite.Checkout,
) {
	l.coordMu.Lock()
	current := l.coordinators[checkout.CheckoutID]
	l.coordMu.Unlock()
	if current != nil && current.Running() && current.graphID == primaryGraphID {
		return
	}
	if current != nil {
		l.dropCoordinator(checkout.CheckoutID)
	}
	coordinator, err := l.buildCoordinator(ctx, primaryGraphID, checkout)
	if err != nil {
		l.logger.Warn("checkout lifecycle: could not start a checkout coordinator",
			zap.String("checkout", checkout.CheckoutID),
			zap.String("root", checkout.RootPath), zap.Error(err))
		return
	}
	if coordinator == nil {
		return
	}
	if !l.installCoordinator(checkout.CheckoutID, coordinator) {
		return
	}
	coordinator.Signal("checkout registered")
}

// buildCoordinator constructs one checkout's coordinator against a graph,
// without registering it. It reports (nil, nil) when the graph cannot back a
// coordinator yet — a primary that is bound in the catalog but has not
// finished indexing, which the next sweep tries again.
//
// Construction and registration are separate because a transition builds a
// coordinator to drive one off-route rebuild with, and only registers it once
// that rebuild has installed the route. A coordinator that went into the
// registry first would have its loop signalled onto a route it is still
// building the layers for.
func (l *CheckoutLifecycle) buildCoordinator(
	ctx context.Context, primaryGraphID string, checkout store_sqlite.Checkout,
) (*CheckoutCoordinator, error) {
	return l.buildCoordinatorWithPoll(ctx, primaryGraphID, checkout, 0)
}

func (l *CheckoutLifecycle) buildCoordinatorWithPoll(
	ctx context.Context, primaryGraphID string, checkout store_sqlite.Checkout, poll time.Duration,
) (*CheckoutCoordinator, error) {
	if l.store == nil || l.catalog == nil {
		return nil, nil
	}
	primary, found, err := l.catalog.GetDedicatedGraph(ctx, primaryGraphID)
	if err != nil {
		return nil, err
	}
	if !found || primary.RepoPrefix == "" {
		return nil, nil
	}
	idx := l.mi.GetIndexer(primary.RepoPrefix)
	if idx == nil {
		return nil, nil
	}

	index := config.Default().Index
	watch := config.Default().Watch
	if l.cfgMgr != nil {
		repoCfg := l.cfgMgr.GetRepoConfig(primary.RepoPrefix)
		index, watch = repoCfg.Index, repoCfg.Watch
	}
	coordinator, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
		CheckoutID:   checkout.CheckoutID,
		CheckoutRoot: checkout.RootPath,
		FamilyID:     checkout.FamilyID,
		GraphID:      primaryGraphID,
		RepoPrefix:   primary.RepoPrefix,
		WorkspaceID:  idx.WorkspaceID(),
		ProjectID:    idx.ProjectID(),
		Store:        l.store,
		Builder: &SparseGenerationBuilder{
			Store:      l.store,
			Registry:   l.mi.registry,
			Config:     index,
			Logger:     l.logger,
			Admissions: idx,
			Embedder:   l.mi.embedder,
			// The daemon's one enrichment manager, so every checkout's
			// language servers are admitted against the same global cap
			// rather than one cap per coordinator.
			Semantic: l.mi.semanticMgr,
		},
		Leases: l.leases,
		Config: index,
		Logger: l.logger,
		Gate:   l.buildGate(),
		// The watcher's own debounce is the quiet window: both coalesce the
		// same event storms, and a checkout whose watch configuration says how
		// long to wait means it for its views too.
		Debounce:     time.Duration(watch.DebounceMs) * time.Millisecond,
		PollInterval: poll,
	})
	if err != nil {
		return nil, err
	}
	// Recorded here rather than at registration: the loop is already running
	// when the constructor returns, and the transitions register only once the
	// rebuild they drive with it has landed.
	l.trackStarted(checkout.CheckoutID, coordinator)
	return coordinator, nil
}

// trackStarted records a coordinator whose loop is running, and forgets the
// ones started earlier for the same checkout that have since stopped.
func (l *CheckoutLifecycle) trackStarted(checkoutID string, coordinator *CheckoutCoordinator) {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	l.started[checkoutID] = append(stillRunning(l.started[checkoutID]), coordinator)
}

// runningLocked reports whether a coordinator started for one checkout is
// still looping, and drops the stopped ones. The caller holds coordMu.
func (l *CheckoutLifecycle) runningLocked(checkoutID string) bool {
	running := stillRunning(l.started[checkoutID])
	if len(running) == 0 {
		delete(l.started, checkoutID)
		return false
	}
	l.started[checkoutID] = running
	return true
}

// stillRunning keeps the coordinators whose loop has not returned.
func stillRunning(coordinators []*CheckoutCoordinator) []*CheckoutCoordinator {
	out := make([]*CheckoutCoordinator, 0, len(coordinators))
	for _, coordinator := range coordinators {
		if coordinator.Running() {
			out = append(out, coordinator)
		}
	}
	return out
}

// installCoordinator puts a coordinator in the registry, and reports whether
// it got the slot. A coordinator that lost a race is closed here rather than
// handed back, so a caller cannot leak the goroutine it just lost.
func (l *CheckoutLifecycle) installCoordinator(checkoutID string, coordinator *CheckoutCoordinator) bool {
	l.coordMu.Lock()
	if _, raced := l.coordinators[checkoutID]; raced {
		l.coordMu.Unlock()
		_ = coordinator.Close()
		return false
	}
	l.coordinators[checkoutID] = coordinator
	// Published under coordMu, so the gauge write is ordered with the
	// registry mutation it reports. Emitting it after the unlock lets two
	// racing transitions apply their levels in the opposite order and leave
	// the gauge stale until the next install or drop.
	viewmetrics.SetGauge(viewmetrics.Coordinators, int64(len(l.coordinators)))
	l.coordMu.Unlock()
	return true
}

// dropCoordinator stops one checkout's coordinator and takes over what it was
// still going to collect.
//
// It waits for an in-flight cycle, so the generation that cycle is filling
// reaches a terminal state before the checkout's rows are touched. The
// handover matters just as much: a stopped coordinator is the last thing that
// knows which generations were built for its checkout, and the sweep is what
// keeps insisting on them once it is gone.
func (l *CheckoutLifecycle) dropCoordinator(checkoutID string) {
	if l == nil {
		return
	}
	l.coordMu.Lock()
	coordinator := l.coordinators[checkoutID]
	delete(l.coordinators, checkoutID)
	// Under coordMu for the same reason as the install side: the level and
	// the registry it counts move together.
	viewmetrics.SetGauge(viewmetrics.Coordinators, int64(len(l.coordinators)))
	l.coordMu.Unlock()
	if coordinator != nil {
		_ = coordinator.Close()
		l.oweRetirement(coordinator.DrainRetirements()...)
		l.stopCheckoutWorkspaces(coordinator.root)
	}
}

// stopCheckoutWorkspaces stops the language servers a checkout's enrichment
// stage left rooted at its working copy.
//
// It runs after Close, which is what makes the pairs reclaimable: the in-flight
// cycle has finished, so the pass that held them has released them. A checkout
// loses its coordinator when it is forgotten, expires, or has its directory
// removed — which is when a server rooted there stops having anything to answer
// about, so leaving reclamation to the router's idle reaper would keep a
// subprocess alive over a directory nobody can read for the length of its TTL.
func (l *CheckoutLifecycle) stopCheckoutWorkspaces(root string) {
	if l == nil || l.mi == nil || root == "" {
		return
	}
	l.mi.semanticMgr.CheckoutWorkspaces().EvictRoot(root)
}

// oweRetirement records generations the lifecycle has to collect because no
// coordinator is left to offer them.
func (l *CheckoutLifecycle) oweRetirement(generations ...int64) {
	if l == nil || l.store == nil || len(generations) == 0 {
		return
	}
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	for _, generationID := range generations {
		if generationID > 0 {
			l.owed[generationID] = struct{}{}
		}
	}
}

// oweRoutedGenerations remembers what a checkout's route names, so the payload
// survives only as long as the route does.
//
// It is read before the teardown withdraws the route rather than after: once
// the row is gone, the two generation ids it held are unreachable — nothing
// else in the catalog names a checkout's layers — and the payload would sit in
// the database with no id anything could offer for collection.
func (l *CheckoutLifecycle) oweRoutedGenerations(ctx context.Context, checkoutID string) {
	if l == nil || l.catalog == nil || checkoutID == "" {
		return
	}
	route, found, err := l.catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil || !found {
		return
	}
	l.oweRetirement(route.CommitGenerationID, route.DirtyGenerationID)
}

// withdrawStaleRoute removes a route row left under a checkout that has stopped
// being served through the automatic lane, and takes over the generations it
// was naming.
//
// It is the sweep's half of a promotion: the flip is the commit point and the
// withdrawal that follows it is cleanup, so a withdrawal that failed there
// leaves a row for the next pass over the family to find. Reading the route
// first is what keeps the read cheap for the checkouts — every dedicated one,
// every sweep — that have no route at all.
func (l *CheckoutLifecycle) withdrawStaleRoute(ctx context.Context, checkoutID string) {
	if l == nil || l.catalog == nil || checkoutID == "" {
		return
	}
	route, found, err := l.catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil || !found {
		return
	}
	l.oweRetirement(route.CommitGenerationID, route.DirtyGenerationID)
	l.withdrawAutomaticRoute(ctx, checkoutID)
}

// SignalCheckout marks one checkout dirty, and reports whether anything was
// listening. It is the entry point for every signal source outside the
// coordinator — a watcher rooted at the checkout, an editor extension, a
// post-checkout hook — so none of them has to reach into the registry.
func (l *CheckoutLifecycle) SignalCheckout(checkoutID, reason string) bool {
	if l == nil {
		return false
	}
	l.coordMu.Lock()
	coordinator := l.coordinators[checkoutID]
	l.coordMu.Unlock()
	if coordinator == nil {
		return false
	}
	coordinator.Signal(reason)
	return true
}

// ViewLeases is the lease manager every coordinator hands to retirement. A
// caller materializing checkout views must materialize through this manager,
// or a sweep will collect the generations its readers are holding.
func (l *CheckoutLifecycle) ViewLeases() *graphview.LeaseManager {
	if l == nil {
		return nil
	}
	return l.leases
}

// Close stops every coordinator. The lifecycle stays usable afterwards —
// closing is about the goroutines, not about the catalog — and a later sweep
// brings the coordinators back up for whatever is still there.
func (l *CheckoutLifecycle) Close() error {
	if l == nil {
		return nil
	}
	l.transitionMu.Lock()
	if !l.transitionClosed {
		l.transitionClosed = true
		if l.cancelTransitions != nil {
			l.cancelTransitions()
		}
	}
	l.transitionMu.Unlock()
	l.transitionWG.Wait()

	l.retryMu.Lock()
	for familyID, retry := range l.familyRetries {
		retry.timer.Stop()
		delete(l.familyRetries, familyID)
	}
	l.retryMu.Unlock()

	l.coordMu.Lock()
	coordinators := make([]*CheckoutCoordinator, 0, len(l.coordinators))
	for _, coordinator := range l.coordinators {
		coordinators = append(coordinators, coordinator)
	}
	l.coordinators = map[string]*CheckoutCoordinator{}
	l.coordMu.Unlock()

	var errs []error
	for _, coordinator := range coordinators {
		if err := coordinator.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LiveCoordinators counts the checkouts this process is running a build loop
// for. An empty familyID counts every family the daemon holds.
func (l *CheckoutLifecycle) LiveCoordinators(familyID string) int {
	return l.liveCoordinators(familyID)
}

// liveCoordinators counts the checkouts whose build loop is running, whether or
// not the registry holds them yet. An empty familyID counts every family.
func (l *CheckoutLifecycle) liveCoordinators(familyID string) int {
	if l == nil {
		return 0
	}
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	live := make(map[string]struct{}, len(l.coordinators))
	for checkoutID, coordinator := range l.coordinators {
		if familyID == "" || coordinator.familyID == familyID {
			live[checkoutID] = struct{}{}
		}
	}
	for checkoutID, coordinators := range l.started {
		running := stillRunning(coordinators)
		if len(running) == 0 {
			delete(l.started, checkoutID)
			continue
		}
		l.started[checkoutID] = running
		// Every coordinator started for one checkout carries that checkout's
		// family, so the first one answers for the whole entry.
		if familyID == "" || running[0].familyID == familyID {
			live[checkoutID] = struct{}{}
		}
	}
	return len(live)
}

// sweepRetirements retries the generations whose retirement was refused when
// the coordinator offered them — leased by a view that has since closed, or
// still named as the base of a layer that has since been collected itself.
//
// It insists three times over: once through every live coordinator's own
// backlog, once through the lifecycle's, which holds what a coordinator that is
// no longer running left owing, and once through the catalog itself, which is
// the only one of the three a crash cannot erase. A withdrawn route's
// generations are the whole of the second list's reason to exist — they are
// refused while the route still names them and collectable the moment the
// teardown removes it.
func (l *CheckoutLifecycle) sweepRetirements(ctx context.Context) int {
	l.coordMu.Lock()
	coordinators := make([]*CheckoutCoordinator, 0, len(l.coordinators))
	served := make(map[string]struct{}, len(l.coordinators))
	for checkoutID, coordinator := range l.coordinators {
		coordinators = append(coordinators, coordinator)
		served[checkoutID] = struct{}{}
	}
	owed := make([]int64, 0, len(l.owed))
	for generationID := range l.owed {
		owed = append(owed, generationID)
	}
	l.coordMu.Unlock()

	retired := 0
	for _, coordinator := range coordinators {
		retired += coordinator.SweepRetirements(ctx)
	}
	if l.store == nil {
		return retired
	}
	owed = append(owed, l.orphanedGenerations(ctx, served, owed)...)
	retireNewestFirst(owed)

	inUse := func(generationID int64) bool {
		return l.leases.InUse(generationID) || l.store.PayloadBuildFlightActive(generationID)
	}
	for _, generationID := range owed {
		err := l.store.RetirePayloadGeneration(ctx, generationID, inUse)
		if err != nil && !errors.Is(err, store_sqlite.ErrCatalogNotFound) {
			continue
		}
		if err == nil {
			retired++
		}
		l.coordMu.Lock()
		delete(l.owed, generationID)
		l.coordMu.Unlock()
	}
	return retired
}

// orphanedGenerations re-derives, from the catalog, the generations no one is
// left to offer for retirement.
//
// The owed set and every coordinator's backlog live in memory, so a process
// that dies between superseding a generation and retiring it loses the only
// handle on it: nothing in the catalog names a discarded generation, and the
// payload would sit in the database for the life of the installation. The scan
// is the handle that survives — it reads the rows themselves rather than the
// pointers into them.
//
// What it offers is not a decision about whether a generation may go. Every
// candidate goes through RetirePayloadGeneration like any other, so routed,
// based-upon and leased generations are refused there, and the scan can afford
// to be generous: a candidate that is still in use is simply refused again on
// the next sweep.
//
// Two rules keep it off work that is not its own. A checkout with a live
// coordinator owns everything built for it — backlog, reuse cache and both
// route slots — so its generations are skipped entirely; and a ready checkout
// layer is a candidate only once its checkout's route has stopped naming it.
// The listings are capped, so one sweep costs a bounded read and at most one
// route lookup per distinct checkout in the results.
func (l *CheckoutLifecycle) orphanedGenerations(
	ctx context.Context,
	served map[string]struct{},
	known []int64,
) []int64 {
	if l.catalog == nil {
		return nil
	}
	seen := make(map[int64]struct{}, len(known))
	for _, generationID := range known {
		seen[generationID] = struct{}{}
	}
	var out []int64
	collect := func(row store_sqlite.ViewGeneration) {
		if row.GenerationID <= 0 {
			return
		}
		if _, duplicate := seen[row.GenerationID]; duplicate {
			return
		}
		seen[row.GenerationID] = struct{}{}
		out = append(out, row.GenerationID)
	}

	// The states a supersede, a failed publish or an interrupted retire leaves
	// behind. Whoever built one, nothing is meant to still be reading it.
	discarded, err := l.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		States: []store_sqlite.ViewGenerationState{
			store_sqlite.ViewGenerationSuperseded,
			store_sqlite.ViewGenerationRetiring,
		},
	})
	if err != nil {
		l.logger.Debug("checkout lifecycle: could not scan discarded generations", zap.Error(err))
	}
	for _, row := range discarded {
		if _, live := served[row.CheckoutID]; live {
			continue
		}
		collect(row)
	}

	// A graph deletion removes the last durable owner pointer before payload
	// retirement can necessarily finish (a live lease may refuse it). Recover
	// that backlog directly from the surviving generation rows. Pagination is
	// deliberate: healthy or still-referenced rows must not pin older orphaned
	// generations behind the catalog listing bound.
	const retirementScanPageSize = 512
	const abandonedBuildingGrace = time.Minute
	abandonedBuildingBefore := l.now().Add(-abandonedBuildingGrace).Unix()
	scanRetirementState := func(state store_sqlite.ViewGenerationState, label string) {
		var beforeGenerationID int64
		for {
			rows, scanErr := l.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
				States:             []store_sqlite.ViewGenerationState{state},
				BeforeGenerationID: beforeGenerationID,
				Limit:              retirementScanPageSize,
			})
			if scanErr != nil {
				l.logger.Warn("indexer: could not scan retirement generations",
					zap.String("state", label), zap.Error(scanErr))
				return
			}
			for _, row := range rows {
				if state == store_sqlite.ViewGenerationBuilding &&
					row.CreatedAt >= l.buildingRecoveryCutoff &&
					row.CreatedAt > abandonedBuildingBefore {
					continue
				}
				if state == store_sqlite.ViewGenerationBuilding &&
					l.store.PayloadBuildFlightActive(row.GenerationID) {
					continue
				}
				collect(row)
			}
			if len(rows) < retirementScanPageSize {
				return
			}
			beforeGenerationID = rows[len(rows)-1].GenerationID
		}
	}
	scanRetirementState(store_sqlite.ViewGenerationFailed, "failed")
	scanRetirementState(store_sqlite.ViewGenerationBuilding, "building")

	const orphanedGraphPageSize = 512
	var beforeGenerationID int64
	for {
		rows, scanErr := l.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
			States:             []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationReady},
			MissingGraph:       true,
			BeforeGenerationID: beforeGenerationID,
			Limit:              orphanedGraphPageSize,
		})
		if scanErr != nil {
			l.logger.Debug("checkout lifecycle: could not scan deleted-graph generations", zap.Error(scanErr))
			break
		}
		for _, row := range rows {
			collect(row)
		}
		if len(rows) < orphanedGraphPageSize {
			break
		}
		beforeGenerationID = rows[len(rows)-1].GenerationID
	}

	// Ready checkout layers are the other half: a coordinator that stopped
	// without draining leaves its commit cache published and unreferenced, and
	// only the route can say whether a layer is still the one being served.
	layers, err := l.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		States:    []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationReady},
		OwnerKind: checkoutLayerOwnerKind,
	})
	if err != nil {
		l.logger.Debug("checkout lifecycle: could not scan checkout layers", zap.Error(err))
	}
	routes := map[string]store_sqlite.CheckoutRoute{}
	for _, row := range layers {
		if row.CheckoutID == "" {
			// A layer that names no checkout has no route to check it
			// against, so nothing here can tell whether it is still served.
			continue
		}
		if _, live := served[row.CheckoutID]; live {
			continue
		}
		switch row.GenerationKind {
		case CommitLayerGenerationKind, DirtyLayerGenerationKind:
		default:
			continue
		}
		route, cached := routes[row.CheckoutID]
		if !cached {
			// A checkout with no route row names nothing, which is what the
			// zero route already says.
			if current, found, err := l.catalog.GetCheckoutRoute(ctx, row.CheckoutID); err == nil && found {
				route = current
			}
			routes[row.CheckoutID] = route
		}
		if route.CommitGenerationID == row.GenerationID || route.DirtyGenerationID == row.GenerationID {
			continue
		}
		collect(row)
	}
	return out
}

// --- startup ------------------------------------------------------------

// Seed brings the catalog in line with what the daemon already tracks.
//
// It is the migration path for an installation that predates the catalog and
// the restart path for one that does not: every configured repository gets
// its family, checkout, intent and graph rows without being re-indexed, an
// identity that already exists is left untouched so its clocks survive the
// restart, and any teardown that was in flight when the process died is
// resumed.
//
// The families it touched are then reconciled once, which is what brings the
// automatic checkouts' coordinators back up. Leaving that to the janitor would
// mean every restart costs a worktree its view for a whole reconcile interval
// — an hour, by default.
func (l *CheckoutLifecycle) Seed(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if l.rec == nil {
		l.sweepRetirements(ctx)
		return nil
	}
	var errs []error

	// Finish cleanup that committed before a crash before reading config. A
	// demotion may have flipped modes and journalled graph retirement while its
	// stale config entry was still on disk; seeding that entry first would
	// recreate the intent and graph the cleanup is about to remove.
	if err := l.rec.Resume(ctx); err != nil {
		errs = append(errs, err)
	}
	// A crash can leave a populated generation in building state before any
	// cleanup journal exists. Drain prior-process residue during boot instead
	// of leaving it for the hourly janitor.
	l.sweepRetirements(ctx)

	seeded := map[string]string{}
	if l.cfgMgr != nil {
		for _, entry := range l.cfgMgr.Global().Repos {
			abs, err := filepath.Abs(entry.Path)
			if err != nil {
				abs = entry.Path
			}
			prefix := l.ResolvePrefix(abs)
			if prefix == "" {
				prefix = EffectiveRepoPrefix(l.cfgMgr, entry)
			}
			if prefix == "" {
				continue
			}
			identity, err := l.recordCheckout(ctx, prefix, abs, TrackSourceConfig, true)
			if err != nil {
				errs = append(errs, fmt.Errorf("seed %s: %w", abs, err))
			}
			if identity.familyID != "" {
				if _, present := seeded[identity.familyID]; !present {
					seeded[identity.familyID] = abs
				}
			}
		}
	}
	if err := l.resumeModeTransitions(ctx); err != nil {
		errs = append(errs, err)
	}
	// The seeded families are reconciled once here rather than at the janitor's
	// first tick, so a restart's automatic checkouts get their coordinators
	// back within the boot rather than within the hour.
	for familyID, probeDir := range seeded {
		l.reconcileFamilyNow(ctx, familyID, probeDir)
	}
	return errors.Join(errs...)
}

// --- cleanup hooks ------------------------------------------------------

// cleanupHooks binds the reconciler's two extension points to what the
// daemon actually owns today.
type cleanupHooks struct{ l *CheckoutLifecycle }

// PurgeCheckoutLayers drops what has been built for one incarnation.
//
// For an automatic checkout that is its coordinator: stopping it stops the
// builds, and the generations it routed stay in the catalog for the retirement
// path to collect rather than being deleted from under a reader here. For a
// checkout served from the corpus it is the live file watcher, so purging is
// detaching it before the enclosing retirement saga removes the checkout's
// identity and any dedicated corpus it owns.
func (h cleanupHooks) PurgeCheckoutLayers(ctx context.Context, checkoutID, _ string) error {
	h.l.oweRoutedGenerations(ctx, checkoutID)
	h.l.dropCoordinator(checkoutID)
	prefix := h.l.prefixForCheckout(ctx, checkoutID)
	if prefix == "" {
		return nil
	}
	h.l.detachWatcher(prefix)
	return nil
}

// ReleaseGraph gives up whatever holds a dedicated graph open.
//
// The graph row names the repo prefix its nodes live under, so releasing it
// is the repository eviction the untrack path has always run — in the order
// that path established: detach the watcher before evicting, so a late
// filesystem event cannot re-index files whose nodes are already gone.
func (h cleanupHooks) ReleaseGraph(ctx context.Context, graphID string) error {
	row, ok, err := h.l.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	// The reconciler deletes the graph row after this hook returns. Capture
	// every generation it owns while that durable ownership is still
	// queryable; the retirement sweep runs after the graph reference is gone.
	h.l.oweRetirement(h.l.graphGenerations(ctx, graphID)...)
	if row.RepoPrefix == "" {
		return nil
	}
	h.l.evictRepo(row.RepoPrefix)
	return nil
}

// --- side effects -------------------------------------------------------

// evictRepoChecked removes a repository from the live tracked set and persists
// that removal. Durable transitions use the returned save error to stay
// retryable instead of completing with stale explicit configuration.
func (l *CheckoutLifecycle) evictRepoChecked(prefix, rootPath string) (nodesRemoved, edgesRemoved int, err error) {
	if prefix != "" {
		l.detachWatcher(prefix)
		nodesRemoved, edgesRemoved = l.mi.UntrackRepo(prefix)
	}
	if l.cfgMgr != nil {
		if rootPath != "" {
			if _, err := l.cfgMgr.Global().RemoveRepoIfPresent(rootPath); err != nil {
				return nodesRemoved, edgesRemoved, err
			}
		}
		if err := l.cfgMgr.Global().Save(); err != nil {
			return nodesRemoved, edgesRemoved, err
		}
	}
	l.notifyTrackedSetChanged()
	return nodesRemoved, edgesRemoved, nil
}

// evictRepo runs the repository teardown every caller shares: watcher first,
// then the graph, then the persisted configuration, then the sessions.
func (l *CheckoutLifecycle) evictRepo(prefix string) (nodesRemoved, edgesRemoved int) {
	if prefix == "" {
		return 0, 0
	}
	l.detachWatcher(prefix)
	nodesRemoved, edgesRemoved = l.mi.UntrackRepo(prefix)
	l.saveConfig("untrack")
	l.notifyTrackedSetChanged()
	return nodesRemoved, edgesRemoved
}

// attachWatcher wires a tracked prefix into the live file watcher. A failure
// leaves an indexed but unwatched repository, which is queryable and only
// goes stale on edit — not a reason to fail the track.
func (l *CheckoutLifecycle) attachWatcher(prefix string) {
	watcher := l.watcher()
	if watcher == nil || prefix == "" || l.cfgMgr == nil {
		return
	}
	if err := watcher.AddRepo(prefix, l.cfgMgr.GetRepoConfig(prefix).Watch); err != nil {
		l.logger.Warn("checkout lifecycle: attach watcher failed",
			zap.String("prefix", prefix), zap.Error(err))
	}
}

// detachWatcher stops watching a prefix. Detaching one that is not attached
// is not an error worth reporting: every teardown path calls it, and the
// second call is the idempotent one.
func (l *CheckoutLifecycle) detachWatcher(prefix string) {
	watcher := l.watcher()
	if watcher == nil || prefix == "" {
		return
	}
	if err := watcher.RemoveRepo(prefix); err != nil {
		l.logger.Debug("checkout lifecycle: detach watcher",
			zap.String("prefix", prefix), zap.Error(err))
	}
}

// saveConfig flushes the tracked-repository list. The indexer mutates it in
// memory; without this the change vanishes on the next restart.
func (l *CheckoutLifecycle) saveConfig(reason string) {
	if l.cfgMgr == nil {
		return
	}
	if err := l.cfgMgr.Global().Save(); err != nil {
		l.logger.Warn("checkout lifecycle: save config failed",
			zap.String("reason", reason), zap.Error(err))
	}
}

// notifyTrackedSetChanged tells the query surface that the tracked set moved,
// or records that it will have to be told once the running batch ends.
func (l *CheckoutLifecycle) notifyTrackedSetChanged() {
	l.mu.Lock()
	notifier := l.notifier
	if l.batchDepth > 0 {
		l.batchPending = true
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	if notifier == nil {
		return
	}
	notifier.InvalidateSessionScopes()
	notifier.RunAnalysis()
}

// beginBatch coalesces every fan-out until the returned function runs.
func (l *CheckoutLifecycle) beginBatch() func() {
	l.mu.Lock()
	l.batchDepth++
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		l.batchDepth--
		fire := l.batchDepth == 0 && l.batchPending
		if fire {
			l.batchPending = false
		}
		l.mu.Unlock()
		if fire {
			l.notifyTrackedSetChanged()
		}
	}
}

// watcher reads the late-bound watcher accessor.
func (l *CheckoutLifecycle) watcher() RepoWatcher {
	l.mu.RLock()
	fn := l.watcherFn
	l.mu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// --- lookups ------------------------------------------------------------

// ResolvePrefix resolves a repo prefix, an absolute root path, or a path
// inside a tracked repository to the prefix it is served under.
func (l *CheckoutLifecycle) ResolvePrefix(pathOrPrefix string) string {
	if l == nil || l.mi == nil || pathOrPrefix == "" {
		return ""
	}
	if meta := l.mi.GetMetadata(pathOrPrefix); meta != nil {
		return pathOrPrefix
	}
	abs, err := filepath.Abs(pathOrPrefix)
	if err != nil {
		return ""
	}
	best, bestLen := "", -1
	for prefix, meta := range l.mi.AllMetadata() {
		if meta == nil || meta.RootPath == "" {
			continue
		}
		root, err := filepath.Abs(meta.RootPath)
		if err != nil {
			continue
		}
		if pathkey.EqualPaths(root, abs) {
			return prefix
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		if len(root) > bestLen {
			best, bestLen = prefix, len(root)
		}
	}
	return best
}

// checkoutForPrefix reads the checkout a repo prefix is bound to, nil when
// the prefix has no catalog identity.
func (l *CheckoutLifecycle) checkoutForPrefix(ctx context.Context, prefix string) (*store_sqlite.Checkout, error) {
	if l.catalog == nil || prefix == "" {
		return nil, nil
	}
	graph, ok, err := l.catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
	if err != nil || !ok || graph.OwnerCheckoutID == "" {
		return nil, err
	}
	checkout, ok, err := l.catalog.GetCheckout(ctx, graph.OwnerCheckoutID)
	if err != nil || !ok {
		return nil, err
	}
	return &checkout, nil
}

// prefixForCheckout resolves a checkout back to the repo prefix serving it.
func (l *CheckoutLifecycle) prefixForCheckout(ctx context.Context, checkoutID string) string {
	if l.catalog == nil || checkoutID == "" {
		return ""
	}
	checkout, ok, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil || !ok {
		return ""
	}
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err == nil {
		for _, g := range graphs {
			if g.OwnerCheckoutID == checkoutID && g.RepoPrefix != "" {
				return g.RepoPrefix
			}
		}
	}
	return l.ResolvePrefix(checkout.RootPath)
}

// --- identifiers --------------------------------------------------------

// FamilyIDFor derives a checkout family's identifier from the shared git
// directory every worktree of the family reads objects from.
//
// It is derived rather than generated so two processes — and the same daemon
// across restarts — reach the same identity for the same repository without
// having to look one up by common directory first.
func FamilyIDFor(commonDir string) string {
	return "family-" + digest(filepath.Clean(commonDir))
}

// GraphIDFor derives a dedicated graph's identifier from the repo prefix its
// nodes are stored under. The prefix is unique across the corpus, so the
// binding is reproducible from either side.
func GraphIDFor(repoPrefix string) string {
	return "graph-" + digest(repoPrefix)
}

// digest renders a stable short identifier for a string.
func digest(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:16])
}

// recordForRoot finds the inventory record describing one worktree root.
//
// Git spells every path with its symlinks resolved; a tracked root is
// spelled the way the configuration wrote it, which on some platforms is a
// path through a symlink to the very same directory. So a failed string
// comparison falls back to filesystem identity rather than concluding that
// git does not know the checkout.
func recordForRoot(inv *gitstate.FamilyInventory, root string) *gitstate.WorktreeRecord {
	if inv == nil {
		return nil
	}
	for i := range inv.Records {
		if pathkey.EqualPaths(inv.Records[i].Path, root) {
			return &inv.Records[i]
		}
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil
	}
	for i := range inv.Records {
		info, err := os.Stat(inv.Records[i].Path)
		if err == nil && os.SameFile(rootInfo, info) {
			return &inv.Records[i]
		}
	}
	return nil
}

// gitDirFor spells out a record's own git directory: the shared directory for
// the main worktree, an administrative directory underneath it for a linked
// one.
func gitDirFor(inv *gitstate.FamilyInventory, record *gitstate.WorktreeRecord) string {
	if inv == nil || record == nil {
		return ""
	}
	if record.IsMain || record.AdminName == gitstate.MainAdminName {
		return inv.CommonDir
	}
	if record.AdminName == "" {
		return ""
	}
	return filepath.Join(inv.CommonDir, "worktrees", record.AdminName)
}

// dirExists reports whether a directory is reachable right now.
func dirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
