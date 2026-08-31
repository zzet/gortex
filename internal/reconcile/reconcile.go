// Package reconcile is the checkout lifecycle core.
//
// It answers one question repeatedly: given what git and the filesystem say
// about a checkout family right now, and what the catalog remembers about it,
// what should the catalog say next? The answer is deliberately conservative.
// Anything the daemon cannot positively prove is treated as a checkout that is
// temporarily unreachable, never as one that is gone, because the two look
// identical from a single failed stat and only one of them is recoverable.
//
// Three ideas carry the whole package:
//
//   - Identity is (family_id, admin_name). A path that goes away and comes
//     back is the same checkout as long as git administers it under the same
//     name, so its id and incarnation survive the outage.
//   - Availability and removal are separate clocks. Being unreachable for an
//     hour does not bring a checkout any closer to being forgotten; only
//     evidence of removal starts the removal clock, and it starts from zero.
//   - Deletion happens through journalled sagas, never inline. Every phase is
//     idempotent and its position is durable, so a crash resumes instead of
//     re-running side effects or leaving half-deleted rows.
//
// Everything here is catalog-level. Building routes and layers, promoting a
// checkout to its own graph, watching the filesystem and wiring any of this
// into the daemon are other packages' work.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
	"uuid"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// ErrInvalidConfig reports a Reconciler that cannot be built: a missing
// dependency, or a grace window that is not a positive duration.
var ErrInvalidConfig = errors.New("reconcile: invalid configuration")

// Config holds the two grace windows the lifecycle waits out.
type Config struct {
	// AvailabilityGrace is how long an unreachable checkout keeps its served
	// layers before they are purged. It absorbs a laptop sleeping, a network
	// mount flapping, or a volume being remounted.
	AvailabilityGrace time.Duration
	// RemovalGrace is how long an evidenced removal waits before the checkout
	// is forgotten. It absorbs the window in which someone deletes a worktree
	// and immediately recreates it.
	RemovalGrace time.Duration
}

// Default returns the shipped grace windows.
func Default() Config {
	return Config{
		AvailabilityGrace: 30 * time.Second,
		RemovalGrace:      30 * time.Second,
	}
}

// Validate rejects a config the lifecycle cannot run on. A zero or negative
// window is refused rather than silently treated as "expire immediately":
// both clocks exist to stop a transient observation from destroying state, and
// a window of zero would delete on the first bad stat.
func (c Config) Validate() error {
	if c.AvailabilityGrace <= 0 {
		return fmt.Errorf("%w: availability grace must be positive, got %s", ErrInvalidConfig, c.AvailabilityGrace)
	}
	if c.RemovalGrace <= 0 {
		return fmt.Errorf("%w: removal grace must be positive, got %s", ErrInvalidConfig, c.RemovalGrace)
	}
	return nil
}

// CleanupHooks is the work a cleanup saga has to do outside the catalog.
//
// The catalog knows which rows describe a checkout; it does not know where its
// built layers live or what holds a graph open. Those belong to the layer and
// graph owners, so the sagas call out at the two phases that need them and
// stay ignorant of the rest.
//
// Every hook must be idempotent. A saga resumes from its last durable phase,
// which can re-enter the phase that was already running when the process died;
// terminal edges are deduplicated within one outer saga operation but may be
// replayed by a later, independently idempotent invocation.
// GraphReleaseTarget is the durable identity and filesystem address of one
// graph cleanup. RepoPrefix and RootPath deliberately survive deletion of the
// graph row: a restart between the guarded catalog delete and config commit
// must still be able to finish the same teardown without rediscovering them
// from the row it just removed.
type GraphReleaseTarget struct {
	GraphID     string
	CheckoutID  string
	Incarnation string
	RepoPrefix  string
	RootPath    string
}

// CheckoutRemovalTarget is the durable identity released by a completed
// forget-checkout saga. RootPath is copied into the saga before the checkout
// row can disappear, so consumers never have to infer it from missing state.
type CheckoutRemovalTarget struct {
	CheckoutID  string
	Incarnation string
	RootPath    string
}

// GraphRemovalTarget is the durable binding released by a completed
// graph-removing saga. CheckoutID and Incarnation distinguish a retired
// binding from a later graph that reuses the same stable GraphID.
type GraphRemovalTarget struct {
	GraphID     string
	FamilyID    string
	CheckoutID  string
	Incarnation string
	RepoPrefix  string
	RootPath    string
}

// FamilyRemovalTarget is the stable identity released by a completed
// forget-family saga.
type FamilyRemovalTarget struct {
	FamilyID string
}

type CleanupHooks interface {
	// PurgeCheckoutLayers drops everything built for one incarnation of a
	// checkout. It is called when an unreachable checkout's availability
	// grace expires, and again as a phase of forgetting it.
	PurgeCheckoutLayers(ctx context.Context, checkoutID, incarnation string) error
	// ReleaseGraph gives up whatever holds a dedicated graph open and invokes
	// finalize while the external repository admission remains closed. The
	// finalizer performs the guarded catalog delete; a returned error leaves
	// both the saga and the admission tombstone retryable.
	ReleaseGraph(ctx context.Context, target GraphReleaseTarget, finalize func() error) error
	// CheckoutRemovalCompleted is a process-local terminal edge. It is invoked
	// only after the forget-checkout saga's final cleanup journal row has been
	// successfully released and every inherited topology guard has unwound.
	// Implementations may serialize topology publication and must be idempotent.
	CheckoutRemovalCompleted(target CheckoutRemovalTarget)
	// GraphRemovalCompleted is the corresponding terminal edge for one durable
	// graph binding. It is ordered before the checkout removal that owned it.
	GraphRemovalCompleted(target GraphRemovalTarget)
	// FamilyRemovalCompleted is emitted only after forget-family has completed
	// and is ordered after all nested graph and checkout removals.
	FamilyRemovalCompleted(target FamilyRemovalTarget)
}

// InventoryFunc enumerates a checkout family. It has gitstate.Inventory's
// signature so the real reader is the zero-configuration default.
type InventoryFunc func(ctx context.Context, dir string) (*gitstate.FamilyInventory, error)

// PathSamplerFunc samples filesystem evidence about one checkout root, with
// gitstate.SamplePathEvidence's signature.
type PathSamplerFunc func(root string) gitstate.PathEvidence

// HEADSamplerFunc samples what HEAD points at in one working tree, with
// gitstate.SampleHEAD's signature.
type HEADSamplerFunc func(ctx context.Context, dir string) (gitstate.HEADState, error)

// CheckoutTopologyGuard excludes an already-admitted source mutation from the
// small durable publication tail that changes a checkout's root, serving
// state, route ownership, or identity. Inventory and filesystem sampling run
// before the guard. The returned context carries implementation-specific held
// state into cleanup hooks; release must be idempotent.
type CheckoutTopologyGuard func(
	ctx context.Context, checkoutIDs ...string,
) (context.Context, func(), error)

// FamilyTopologyGuard serializes membership and primary-designation changes
// within the named Git families. Family IDs are sorted by the lifecycle before
// acquisition so multi-family root moves cannot deadlock.
type FamilyTopologyGuard func(
	ctx context.Context, familyIDs ...string,
) (context.Context, func(), error)

// Option overrides a Reconciler dependency.
type Option func(*Reconciler)

// WithClock replaces the clock. Every timestamp and deadline a pass writes
// comes from it, so a test can put the lifecycle anywhere in time.
func WithClock(now func() time.Time) Option {
	return func(r *Reconciler) {
		if now != nil {
			r.now = now
		}
	}
}

// WithInventory replaces the family enumerator, so a test can drive an exact
// set of records — including ones no real git would produce twice in a row.
func WithInventory(fn InventoryFunc) Option {
	return func(r *Reconciler) {
		if fn != nil {
			r.inventory = fn
		}
	}
}

// WithPathSampler replaces the filesystem sampler.
func WithPathSampler(fn PathSamplerFunc) Option {
	return func(r *Reconciler) {
		if fn != nil {
			r.samplePath = fn
		}
	}
}

// WithHEADSampler replaces the HEAD sampler.
func WithHEADSampler(fn HEADSamplerFunc) Option {
	return func(r *Reconciler) {
		if fn != nil {
			r.sampleHEAD = fn
		}
	}
}

// WithLogger installs the logger the pass records its classifications on. A
// nil logger — the default — silences them; nothing about a decision depends
// on it.
func WithLogger(logger *zap.Logger) Option {
	return func(r *Reconciler) {
		if logger != nil {
			r.logger = logger
		}
	}
}

// WithCheckoutTopologyGuard installs the daemon's mutation/topology fence.
// Embedded and catalog-only reconcilers retain a no-op guard.
func WithCheckoutTopologyGuard(guard CheckoutTopologyGuard) Option {
	return func(r *Reconciler) {
		if guard != nil {
			r.guardTopology = guard
		}
	}
}

// WithFamilyTopologyGuard installs the daemon's family-scoped topology fence.
func WithFamilyTopologyGuard(guard FamilyTopologyGuard) Option {
	return func(r *Reconciler) {
		if guard != nil {
			r.guardFamilyTopology = guard
		}
	}
}

// Reconciler drives the checkout lifecycle against one catalog.
//
// It holds no state of its own: everything durable lives in catalog rows, so
// two Reconcilers over the same store are two actors racing through the same
// incarnation guards rather than two divergent caches.
type Reconciler struct {
	catalog *store_sqlite.Catalog
	hooks   CleanupHooks
	cfg     Config

	now                 func() time.Time
	inventory           InventoryFunc
	samplePath          PathSamplerFunc
	sampleHEAD          HEADSamplerFunc
	logger              *zap.Logger
	guardTopology       CheckoutTopologyGuard
	guardFamilyTopology FamilyTopologyGuard
}

// New builds a Reconciler over a catalog handle.
func New(catalog *store_sqlite.Catalog, hooks CleanupHooks, cfg Config, opts ...Option) (*Reconciler, error) {
	if catalog == nil {
		return nil, fmt.Errorf("%w: nil catalog", ErrInvalidConfig)
	}
	if hooks == nil {
		return nil, fmt.Errorf("%w: nil cleanup hooks", ErrInvalidConfig)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	r := &Reconciler{
		catalog:    catalog,
		hooks:      hooks,
		cfg:        cfg,
		now:        time.Now,
		inventory:  gitstate.Inventory,
		samplePath: gitstate.SamplePathEvidence,
		sampleHEAD: gitstate.SampleHEAD,
		logger:     zap.NewNop(),
		guardTopology: func(ctx context.Context, _ ...string) (context.Context, func(), error) {
			return ctx, func() {}, nil
		},
		guardFamilyTopology: func(ctx context.Context, _ ...string) (context.Context, func(), error) {
			return ctx, func() {}, nil
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r, nil
}

type topologyGuardContextKey struct{}
type familyTopologyGuardContextKey struct{}

func (r *Reconciler) withFamilyTopology(
	ctx context.Context,
	familyIDs []string,
	fn func(context.Context) error,
) error {
	if fn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	held, _ := ctx.Value(familyTopologyGuardContextKey{}).(map[string]struct{})
	missing := make([]string, 0, len(familyIDs))
	for _, familyID := range familyIDs {
		if familyID == "" {
			continue
		}
		if _, ok := held[familyID]; !ok {
			missing = append(missing, familyID)
		}
	}
	if len(missing) == 0 {
		return fn(ctx)
	}
	guarded, release, err := r.guardFamilyTopology(ctx, missing...)
	if err != nil {
		return err
	}
	if release == nil {
		release = func() {}
	}
	defer release()
	nextHeld := make(map[string]struct{}, len(held)+len(missing))
	for familyID := range held {
		nextHeld[familyID] = struct{}{}
	}
	for _, familyID := range missing {
		nextHeld[familyID] = struct{}{}
	}
	guarded = context.WithValue(guarded, familyTopologyGuardContextKey{}, nextHeld)
	return fn(guarded)
}

func (r *Reconciler) withCheckoutTopology(
	ctx context.Context,
	checkoutIDs []string,
	fn func(context.Context) error,
) error {
	if fn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	held, _ := ctx.Value(topologyGuardContextKey{}).(map[string]struct{})
	missing := make([]string, 0, len(checkoutIDs))
	for _, checkoutID := range checkoutIDs {
		if checkoutID == "" {
			continue
		}
		if _, ok := held[checkoutID]; !ok {
			missing = append(missing, checkoutID)
		}
	}
	if len(missing) == 0 {
		return fn(ctx)
	}
	guarded, release, err := r.guardTopology(ctx, missing...)
	if err != nil {
		return err
	}
	if release == nil {
		release = func() {}
	}
	defer release()
	nextHeld := make(map[string]struct{}, len(held)+len(missing))
	for checkoutID := range held {
		nextHeld[checkoutID] = struct{}{}
	}
	for _, checkoutID := range missing {
		nextHeld[checkoutID] = struct{}{}
	}
	guarded = context.WithValue(guarded, topologyGuardContextKey{}, nextHeld)
	return fn(guarded)
}

// familyPass is one ReconcileFamily call's shared context. The clock is read
// once for the whole pass so every deadline it writes is measured from the
// same instant — two checkouts reconciled together get the same "now", not
// two microseconds apart.
type familyPass struct {
	family         store_sqlite.RepositoryFamily
	inventory      *gitstate.FamilyInventory
	inventoryErr   error
	primaryGraphID string
	now            time.Time
}

// ReconcileFamily brings one family's catalog rows in line with what git and
// the filesystem say right now.
//
// probeDir is any directory inside the family; the inventory is taken from it
// and then validated against the family's recorded common-dir identity, so an
// inventory of some other repository can never be read as this family's.
//
// The pass never deletes an identity by itself. Deletion happens when a
// removal clock expires, which runs a saga, or when a caller enters one of
// the saga entry points directly.
func (r *Reconciler) ReconcileFamily(ctx context.Context, familyID, probeDir string) (FamilyReport, error) {
	var report FamilyReport
	err := r.withSagaRemovalPublications(ctx, func(publicationCtx context.Context) error {
		var reconcileErr error
		report, reconcileErr = r.reconcileFamily(publicationCtx, familyID, probeDir)
		return reconcileErr
	})
	return report, err
}

func (r *Reconciler) reconcileFamily(ctx context.Context, familyID, probeDir string) (FamilyReport, error) {
	family, ok, err := r.catalog.GetRepositoryFamily(ctx, familyID)
	if err != nil {
		return FamilyReport{}, err
	}
	if !ok {
		return FamilyReport{}, fmt.Errorf("%w: family %s", store_sqlite.ErrCatalogNotFound, familyID)
	}

	started := time.Now()
	inv, invErr := r.inventory(ctx, probeDir)
	elapsed := time.Since(started)
	pass := &familyPass{
		family:       family,
		inventory:    inv,
		inventoryErr: ValidateInventory(inv, invErr, family.CommonDirIdentity),
		now:          r.now(),
	}
	// The inventory is the pass's one unbounded cost — a git plumbing call
	// against a directory that may be on a sleeping volume — so it is timed
	// separately from everything the pass then decides.
	inventoryOutcome := viewmetrics.OutcomeOK
	if pass.inventoryErr != nil {
		inventoryOutcome = viewmetrics.OutcomeError
	}
	viewmetrics.Observe(viewmetrics.FamilyInventorySeconds, elapsed, inventoryOutcome)
	guarded, releaseFamily, err := r.guardFamilyTopology(ctx, familyID)
	if err != nil {
		return FamilyReport{}, err
	}
	if releaseFamily == nil {
		releaseFamily = func() {}
	}
	defer releaseFamily()
	ctx = context.WithValue(
		guarded,
		familyTopologyGuardContextKey{},
		map[string]struct{}{familyID: {}},
	)

	graphs, err := r.catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return FamilyReport{}, err
	}
	for _, graph := range graphs {
		if graph.IsPrimaryBase {
			pass.primaryGraphID = graph.GraphID
			break
		}
	}

	report := FamilyReport{
		FamilyID:        familyID,
		CommonDir:       family.CommonDirIdentity,
		InventoryUsable: pass.inventoryErr == nil,
		PrimaryGraphID:  pass.primaryGraphID,
	}
	if pass.primaryGraphID == "" {
		report.Code = graphview.CodeNoPrimary
	}

	// A record with no administrative name cannot be keyed to an identity at
	// all, so it is left out of the match index and observed on its own below.
	records := map[string]*gitstate.WorktreeRecord{}
	if pass.inventoryErr == nil {
		for i := range inv.Records {
			record := &inv.Records[i]
			if record.AdminName != "" {
				records[record.AdminName] = record
			}
		}
	}

	known, err := r.catalog.ListCheckouts(ctx, familyID)
	if err != nil {
		return FamilyReport{}, err
	}
	matched := map[string]bool{}
	for _, existing := range known {
		matched[existing.AdminName] = true
		entry, err := r.reconcileKnown(ctx, pass, existing, records[existing.AdminName])
		if err != nil {
			return report, err
		}
		report.Checkouts = append(report.Checkouts, entry)
	}

	if pass.inventoryErr == nil {
		for i := range inv.Records {
			record := &inv.Records[i]
			if record.AdminName != "" && matched[record.AdminName] {
				continue
			}
			entry, err := r.observeNew(ctx, pass, record)
			if err != nil {
				return report, err
			}
			report.Checkouts = append(report.Checkouts, entry)
		}
	}
	return report, nil
}

// reconcileKnown advances one identity the catalog already holds.
func (r *Reconciler) reconcileKnown(
	ctx context.Context,
	pass *familyPass,
	existing store_sqlite.Checkout,
	record *gitstate.WorktreeRecord,
) (CheckoutReport, error) {
	root := existing.RootPath
	if record != nil {
		root = record.Path
	}

	storedRow, _, err := r.catalog.GetCheckoutPathEvidence(ctx, existing.CheckoutID)
	if err != nil {
		return CheckoutReport{}, err
	}
	fresh := SampledPathEvidence(r.samplePath(root))
	class := Classify(pass.inventoryErr, record, StoredPathEvidence(storedRow), fresh)
	r.recordClassification(pass, existing, record, fresh, class)

	entry := CheckoutReport{
		AdminName:        existing.AdminName,
		RootPath:         root,
		PreviousRootPath: existing.RootPath,
		Main:             existing.AdminName == gitstate.MainAdminName,
		CheckoutID:       existing.CheckoutID,
		Incarnation:      existing.Incarnation,
		Durable:          true,
		State:            existing.State,
		Classification:   class,
	}

	// The request starts as a copy of what is stored, so any axis a branch
	// below does not touch is written back unchanged. That is what keeps the
	// removal clock from disturbing the availability clock and vice versa.
	req := observationFrom(existing)
	req.LastSeen = pass.now.Unix()
	observeDiscoveryLag(pass.now, existing.LastSeen)

	switch class.Disposition {
	case DispositionPresent:
		entry.Action = r.applyPresent(ctx, pass, existing, record, &req)
	case DispositionInaccessible:
		action, gone, err := r.applyInaccessible(ctx, pass, existing, &req)
		if err != nil {
			return entry, err
		}
		entry.Action = action
		if action == ActionGuardLost {
			return entry, nil
		}
		if gone {
			entry.State = ""
			recordTransition(existing.State, "", class)
			return entry, nil
		}
	case DispositionRemoved:
		action, gone, err := r.applyRemoved(ctx, pass, existing, class, &req)
		if err != nil {
			return entry, err
		}
		entry.Action = action
		if action == ActionGuardLost {
			// Another actor owns this teardown. Its rows are the ones that
			// count, and the rest of the family still has to be reconciled.
			return entry, nil
		}
		if gone {
			// The rows are gone; there is nothing left to write to.
			entry.State = ""
			recordTransition(existing.State, "", class)
			return entry, nil
		}
	}

	switch entry.Action {
	case ActionAvailabilityGraceStarted, ActionAvailabilityHeld:
		entry.RetryAt = req.AvailabilityDeadline
	case ActionRemovalGraceStarted, ActionRemovalHeld:
		entry.RetryAt = req.RemovalDeadline
	}

	guardLost := false
	var guardedCheckoutIDs []string
	if observationChangesCheckoutTopology(existing, req) {
		guardedCheckoutIDs = []string{existing.CheckoutID}
	}
	err = r.withCheckoutTopology(ctx, guardedCheckoutIDs, func(guarded context.Context) error {
		if updateErr := r.catalog.UpdateCheckoutObservation(guarded, req); updateErr != nil {
			if errors.Is(updateErr, store_sqlite.ErrCatalogStaleGuard) {
				// Another actor moved this row first. Its write is the one that
				// counts; re-reading and overwriting would undo it.
				guardLost = true
				return nil
			}
			return updateErr
		}
		if class.Disposition == DispositionPresent {
			row := fresh.CatalogRow(existing.CheckoutID, pass.now.Unix(), storedRow.SampleGeneration+1)
			if evidenceErr := r.catalog.UpsertCheckoutPathEvidence(guarded, row); evidenceErr != nil {
				return evidenceErr
			}
		}
		return nil
	})
	if err != nil {
		return entry, err
	}
	if guardLost {
		entry.Action = ActionGuardLost
		entry.State = existing.State
		return entry, nil
	}
	entry.State = req.State
	entry.RootMoved = class.Disposition == DispositionPresent &&
		!pathkey.EqualPaths(
			pathkey.CanonicalExistingRoot(existing.RootPath),
			pathkey.CanonicalExistingRoot(req.RootPath),
		)
	recordTransition(existing.State, req.State, class)
	return entry, nil
}

// observationChangesCheckoutTopology excludes the high-frequency confirmation
// write (timestamps and path evidence only) from the mutation fence. A slow
// publication in one worktree must not delay discovery of a new sibling in the
// same family. State, root, git administration, and HEAD changes still take the
// exclusive cut because they can change which route and coordinator describe
// bytes written at that root.
func observationChangesCheckoutTopology(
	existing store_sqlite.Checkout,
	req store_sqlite.UpdateCheckoutObservationRequest,
) bool {
	return existing.State != req.State ||
		!pathkey.EqualPaths(
			pathkey.CanonicalExistingRoot(existing.RootPath),
			pathkey.CanonicalExistingRoot(req.RootPath),
		) ||
		existing.GitDir != req.GitDir ||
		existing.HeadRef != req.HeadRef ||
		existing.HeadCommit != req.HeadCommit ||
		existing.HeadTree != req.HeadTree
}

// applyPresent puts a reachable checkout back in the ready state and clears
// both clocks. Clearing the removal clock here is what "the same incarnation
// came back" means: the row was matched by (family, admin name) and its
// incarnation never changed, so whatever the pass thought was removed is the
// very thing that just answered.
func (r *Reconciler) applyPresent(
	ctx context.Context,
	pass *familyPass,
	existing store_sqlite.Checkout,
	record *gitstate.WorktreeRecord,
	req *store_sqlite.UpdateCheckoutObservationRequest,
) CheckoutAction {
	req.State = store_sqlite.CheckoutStateReady
	req.LastAccessible = pass.now.Unix()
	req.UnavailableSince = 0
	req.AvailabilityDeadline = 0
	req.RemovalDetectedAt = 0
	req.RemovalDeadline = 0
	req.RemovalEvidence = ""
	req.RootPath = record.Path
	req.GitDir = pass.gitDirFor(record)
	req.Locked = record.Locked
	req.Prunable = record.Prunable
	req.HeadRef, req.HeadCommit, req.HeadTree = r.headFor(ctx, record)
	req.LastError = ""

	switch {
	case existing.RemovalDetectedAt != 0:
		return ActionRemovalCancelled
	case existing.State != store_sqlite.CheckoutStateReady:
		return ActionAvailabilityRecovered
	}
	return ActionReadyConfirmed
}

// applyInaccessible advances the availability clock without starting a second
// removal grace. Once the availability grace expires, the checkout is retired
// exactly like an authoritatively removed worktree: an inaccessible worktree
// must not leave a shadow graph or tracking state behind indefinitely.
func (r *Reconciler) applyInaccessible(
	ctx context.Context,
	pass *familyPass,
	existing store_sqlite.Checkout,
	req *store_sqlite.UpdateCheckoutObservationRequest,
) (CheckoutAction, bool, error) {
	now := pass.now.Unix()
	if existing.UnavailableSince == 0 {
		req.State = store_sqlite.CheckoutStateAvailabilityGrace
		req.UnavailableSince = now
		req.AvailabilityDeadline = pass.now.Add(r.cfg.AvailabilityGrace).Unix()
		return ActionAvailabilityGraceStarted, false, nil
	}
	if existing.AvailabilityDeadline == 0 || now < existing.AvailabilityDeadline {
		return ActionAvailabilityHeld, false, nil
	}
	return r.retireCheckout(context.WithoutCancel(ctx), pass, existing)
}

// applyRemoved advances the removal clock only, and runs the right teardown
// saga once it expires. The bool reports that the checkout's rows are gone, so
// the caller must not try to write to them.
//
// A saga entry point refused by a guard is not a pass failure. It means
// another actor is already acting on this identity — it re-keyed the row, or
// moved the family's primary out from under the epoch this pass read — and
// the rule for a lost guard is to report the loss and leave the winner alone.
func (r *Reconciler) applyRemoved(
	ctx context.Context,
	pass *familyPass,
	existing store_sqlite.Checkout,
	class Classification,
	req *store_sqlite.UpdateCheckoutObservationRequest,
) (CheckoutAction, bool, error) {
	// Removal grace is a serving state as well as a clock: once Git has
	// authoritatively omitted this checkout, new readers must stop pinning its
	// stale route and use the labeled, read-only family fallback immediately.
	req.State = store_sqlite.CheckoutStateRemovalGrace
	now := pass.now.Unix()
	if existing.RemovalDetectedAt == 0 {
		req.RemovalDetectedAt = now
		req.RemovalDeadline = pass.now.Add(r.cfg.RemovalGrace).Unix()
		req.RemovalEvidence = string(class.Evidence)
		return ActionRemovalGraceStarted, false, nil
	}
	if now < existing.RemovalDeadline {
		return ActionRemovalHeld, false, nil
	}

	return r.retireCheckout(context.WithoutCancel(ctx), pass, existing)
}

// retireCheckout runs the guarded teardown shared by authoritative removal and
// expired inaccessibility. The caller supplies a detached context after all
// classification and identity guards have passed, so a request timeout cannot
// strand a half-finished cleanup saga.
func (r *Reconciler) retireCheckout(
	ctx context.Context,
	pass *familyPass,
	existing store_sqlite.Checkout,
) (CheckoutAction, bool, error) {
	action := CheckoutAction("")
	gone := false
	guardLost := false

	ownedBeforeAdmission, err := r.ownedGraph(ctx, existing.FamilyID, existing.CheckoutID)
	if err != nil {
		return "", false, err
	}
	primaryGraphID := ""
	guardedCheckoutIDs := []string{existing.CheckoutID}
	if ownedBeforeAdmission != nil && ownedBeforeAdmission.IsPrimaryBase {
		primaryGraphID = ownedBeforeAdmission.GraphID
		guardedCheckoutIDs, err = r.familyCheckoutTopologyCohort(
			ctx, existing.FamilyID, existing.CheckoutID,
		)
		if err != nil {
			return "", false, err
		}
	}

	err = r.withCheckoutTopology(ctx, guardedCheckoutIDs, func(guarded context.Context) error {
		current, found, readErr := r.catalog.GetCheckout(guarded, existing.CheckoutID)
		if readErr != nil {
			return readErr
		}
		// Classification happened before mutation admission drained. A newer
		// pass may have recovered or otherwise advanced the same incarnation in
		// that interval; retirement must not turn that fresh observation into a
		// deletion merely because the incarnation itself is unchanged.
		if !found || current != existing {
			guardLost = true
			return nil
		}
		owned, ownedErr := r.ownedGraph(guarded, existing.FamilyID, existing.CheckoutID)
		if ownedErr != nil {
			return ownedErr
		}
		if primaryGraphID != "" {
			if owned == nil || !owned.IsPrimaryBase || owned.GraphID != primaryGraphID {
				guardLost = true
				return nil
			}
			if retireErr := r.RetirePrimaryClosure(
				guarded, owned.GraphID, pass.family.PrimaryEpoch,
			); retireErr != nil {
				if errors.Is(retireErr, store_sqlite.ErrCatalogStaleGuard) {
					guardLost = true
					return nil
				}
				return retireErr
			}
			action, gone = ActionPrimaryClosureRetired, true
			return nil
		}
		if owned != nil && owned.IsPrimaryBase {
			// The pre-admission read did not identify a primary closure, so this
			// operation owns only one checkout guard. Extending it here would
			// violate the family -> complete sorted cohort lock order.
			guardLost = true
			return nil
		}
		if forgetErr := r.ForgetCheckout(
			guarded, existing.CheckoutID, existing.Incarnation,
		); forgetErr != nil {
			if errors.Is(forgetErr, store_sqlite.ErrCatalogStaleGuard) {
				guardLost = true
				return nil
			}
			return forgetErr
		}
		action, gone = ActionForgotten, true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	if guardLost {
		return ActionGuardLost, false, nil
	}
	return action, gone, nil
}

// observeNew decides what to do with a worktree no identity matched.
//
// Durable identity is expensive: it is what later rounds hang graphs, routes
// and layers off. It is only minted for a checkout that is actually usable and
// for a family that already has somewhere to serve it from, so everything else
// is reported and forgotten at the end of the pass.
//
// Minting goes through the catalog's guarded allocation rather than a plain
// insert. The listing this pass matched against is a read that has already
// happened, so it cannot rule out a second reconciler minting the same
// administrative name in between; the guard on the insert can, and the loser
// reports the loss instead of leaving the family with two identities for one
// working copy.
func (r *Reconciler) observeNew(
	ctx context.Context,
	pass *familyPass,
	record *gitstate.WorktreeRecord,
) (CheckoutReport, error) {
	fresh := SampledPathEvidence(r.samplePath(record.Path))
	entry := CheckoutReport{
		AdminName:      record.AdminName,
		RootPath:       record.Path,
		Main:           record.IsMain,
		Action:         ActionObserved,
		Classification: Classify(nil, record, PathEvidence{}, fresh),
	}

	switch {
	case record.AdminName == "":
		entry.Detail = "no administrative name to key an identity on"
		return entry, nil
	case record.Prunable:
		entry.Detail = "git already considers this worktree prunable"
		return entry, nil
	case !record.RootAccessible:
		entry.Detail = "root has never been reachable"
		return entry, nil
	case pass.primaryGraphID == "":
		entry.Detail = "family has no primary dedicated graph to serve from"
		entry.Classification.Code = graphview.CodeNoPrimary
		return entry, nil
	}

	checkout := store_sqlite.Checkout{
		CheckoutID:     uuid.NewV7().String(),
		Incarnation:    uuid.NewV7().String(),
		FamilyID:       pass.family.FamilyID,
		RootPath:       record.Path,
		GitDir:         pass.gitDirFor(record),
		AdminName:      record.AdminName,
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeAutomatic,
		EffectiveMode:  store_sqlite.CheckoutModeAutomatic,
		Locked:         record.Locked,
		Prunable:       record.Prunable,
		LastAccessible: pass.now.Unix(),
		LastSeen:       pass.now.Unix(),
	}
	checkout.HeadRef, checkout.HeadCommit, checkout.HeadTree = r.headFor(ctx, record)
	guardLost := false
	err := r.withCheckoutTopology(ctx, []string{checkout.CheckoutID}, func(guarded context.Context) error {
		if allocateErr := r.catalog.AllocateCheckout(guarded, checkout); allocateErr != nil {
			if errors.Is(allocateErr, store_sqlite.ErrCatalogStaleGuard) {
				// Another actor allocated this administrative name between this
				// pass's listing and its insert. Its identity is the one that
				// counts; the next pass matches the record to that row instead of
				// leaving the family with two identities for one working copy.
				guardLost = true
				return nil
			}
			return allocateErr
		}
		row := fresh.CatalogRow(checkout.CheckoutID, pass.now.Unix(), 1)
		return r.catalog.UpsertCheckoutPathEvidence(guarded, row)
	})
	if err != nil {
		return entry, err
	}
	if guardLost {
		entry.Action = ActionGuardLost
		entry.Detail = "another actor allocated this administrative name first"
		return entry, nil
	}

	entry.CheckoutID = checkout.CheckoutID
	entry.Incarnation = checkout.Incarnation
	entry.Durable = true
	entry.Action = ActionIdentityAllocated
	entry.State = checkout.State
	entry.Detail = "first sighting in a family that has a primary dedicated graph"
	recordTransition("", checkout.State, entry.Classification)
	return entry, nil
}

// observeDiscoveryLag records how stale the daemon's knowledge of one checkout
// was when this pass reached it: the gap between the last observation written
// for it and the one being written now.
//
// It is the answer to "how long can a worktree move before anything notices",
// which is the same window a checkout is created in and the same window a view
// can be stale for. A checkout with no prior observation contributes nothing —
// there is no gap to measure against a first sighting.
func observeDiscoveryLag(now time.Time, lastSeen int64) {
	if lastSeen <= 0 {
		return
	}
	viewmetrics.Observe(viewmetrics.FamilyDiscoveryLagSeconds, now.Sub(time.Unix(lastSeen, 0)))
}

// recordTransition counts one checkout's move between lifecycle states.
//
// Only a real move is counted: a pass that confirms a ready checkout is still
// ready is the common case and would drown the series it shares with the rare
// moves that matter. The empty state on either side is a checkout that did not
// exist yet or no longer does, and is recorded under its own bounded name
// rather than as an empty label.
func recordTransition(from, to store_sqlite.CheckoutState, class Classification) {
	if from == to {
		return
	}
	viewmetrics.Count(viewmetrics.CheckoutTransitionTotal,
		transitionState(from), transitionState(to), evidenceClass(class))
}

// transitionState renders a lifecycle state as a metric label.
func transitionState(state store_sqlite.CheckoutState) string {
	if state == "" {
		return viewmetrics.StateNone
	}
	return string(state)
}

// evidenceClass renders a classification as the bounded evidence label: which
// of the classifier's five verdicts decided this checkout.
func evidenceClass(class Classification) string {
	switch class.Disposition {
	case DispositionPresent:
		return viewmetrics.EvidencePresent
	case DispositionInaccessible:
		return viewmetrics.EvidenceInaccessible
	case DispositionRemoved:
		switch class.Evidence {
		case EvidenceAuthoritativeOmission:
			return viewmetrics.EvidenceAuthoritativeOmission
		case EvidencePrunableConfirmed:
			return viewmetrics.EvidencePrunableConfirmed
		}
	}
	return viewmetrics.EvidenceNone
}

// recordClassification logs which evidence decided one checkout, with the
// probe results the verdict was read off.
//
// The counter beside it carries the class alone; this carries the identity and
// the raw observations — what git said about the record, whether the root
// answered a fresh stat, and whether the ancestor's volume token still matches
// the one recorded while the root existed. That triple is the whole of the
// prunable-confirmed proof, so a removal that surprises someone can be argued
// with from the log rather than guessed at.
//
// It is a Debug line and it is per checkout per pass. That is deliberate: the
// pass runs on the janitor's schedule, not per request, and a classification
// nobody can reconstruct is the failure mode this exists to prevent.
func (r *Reconciler) recordClassification(
	pass *familyPass,
	existing store_sqlite.Checkout,
	record *gitstate.WorktreeRecord,
	fresh PathEvidence,
	class Classification,
) {
	if r.logger == nil {
		return
	}
	fields := []zap.Field{
		zap.String("family", pass.family.FamilyID),
		zap.String("checkout", existing.CheckoutID),
		zap.String("admin_name", existing.AdminName),
		zap.String("root", existing.RootPath),
		zap.String("state", string(existing.State)),
		zap.String("disposition", string(class.Disposition)),
		zap.String("evidence", evidenceClass(class)),
		zap.String("detail", class.Detail),
		zap.Bool("probe_root_exists", fresh.RootExists),
		zap.Bool("probe_ancestor_volume_usable", fresh.ancestorVolumeUsable()),
		zap.Bool("listed_by_git", record != nil),
	}
	if record != nil {
		fields = append(fields,
			zap.Bool("git_root_accessible", record.RootAccessible),
			zap.Bool("git_prunable", record.Prunable))
	}
	r.logger.Debug("checkout reconcile: classified", fields...)
}

// headFor reads HEAD out of a reachable working tree, falling back to what the
// inventory already reported. The inventory carries a ref and a commit but no
// tree, so a successful sample is the only way the tree oid gets filled in;
// a failed one is not worth failing the pass over.
func (r *Reconciler) headFor(ctx context.Context, record *gitstate.WorktreeRecord) (ref, commit, tree string) {
	ref, commit = record.HEADRef, record.HEADOID
	if !record.RootAccessible {
		return ref, commit, ""
	}
	state, err := r.sampleHEAD(ctx, record.Path)
	if err != nil {
		return ref, commit, ""
	}
	if state.Ref != "" {
		ref = state.Ref
	}
	if state.CommitOID != "" {
		commit = state.CommitOID
	}
	return ref, commit, state.TreeOID
}

// gitDirFor spells out a record's own git directory. The main worktree reads
// the shared directory directly; every linked worktree has an administrative
// directory named after it underneath.
func (p *familyPass) gitDirFor(record *gitstate.WorktreeRecord) string {
	if record == nil || p.inventory == nil {
		return ""
	}
	if record.IsMain || record.AdminName == gitstate.MainAdminName {
		return p.inventory.CommonDir
	}
	if record.AdminName == "" {
		return ""
	}
	return filepath.Join(p.inventory.CommonDir, "worktrees", record.AdminName)
}

// observationFrom seeds a guarded write with everything the stored row already
// says, so a branch only has to state what it changes. The mode axis is not
// part of it: a pass observes how a checkout looks, never how it is served.
func observationFrom(c store_sqlite.Checkout) store_sqlite.UpdateCheckoutObservationRequest {
	return store_sqlite.UpdateCheckoutObservationRequest{
		CheckoutID:           c.CheckoutID,
		Incarnation:          c.Incarnation,
		ExpectedRootPath:     c.RootPath,
		State:                c.State,
		RootPath:             c.RootPath,
		GitDir:               c.GitDir,
		Locked:               c.Locked,
		Prunable:             c.Prunable,
		HeadRef:              c.HeadRef,
		HeadCommit:           c.HeadCommit,
		HeadTree:             c.HeadTree,
		LastAccessible:       c.LastAccessible,
		UnavailableSince:     c.UnavailableSince,
		AvailabilityDeadline: c.AvailabilityDeadline,
		RemovalDetectedAt:    c.RemovalDetectedAt,
		RemovalDeadline:      c.RemovalDeadline,
		RemovalEvidence:      c.RemovalEvidence,
		LastSeen:             c.LastSeen,
		LastError:            c.LastError,
	}
}

// ownedGraph returns the dedicated graph a checkout owns, or nil. A checkout
// owns at most one, which the catalog enforces.
func (r *Reconciler) ownedGraph(ctx context.Context, familyID, checkoutID string) (*store_sqlite.DedicatedGraph, error) {
	if familyID == "" || checkoutID == "" {
		return nil, nil
	}
	graphs, err := r.catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return nil, err
	}
	for i := range graphs {
		if graphs[i].OwnerCheckoutID == checkoutID {
			return &graphs[i], nil
		}
	}
	return nil, nil
}
