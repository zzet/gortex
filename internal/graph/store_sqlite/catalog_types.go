package store_sqlite

import (
	"errors"
	"fmt"
)

// State vocabularies for the checkout-lifecycle catalog.
//
// The columns are plain TEXT (see checkoutCatalogSchemaSQL); these constants,
// and the validators below, are the only authority on what may be written.
// Keeping the check in Go rather than in a CHECK constraint means extending a
// vocabulary is a code change, not a migration of every installed database.

// CheckoutState is the lifecycle state of one working copy.
type CheckoutState string

const (
	// CheckoutStateReady is the steady state: the path is present and served.
	CheckoutStateReady CheckoutState = "checkout_ready"
	// CheckoutStateAvailabilityGrace means the path stopped answering but its
	// availability deadline has not expired, so nothing is torn down yet.
	CheckoutStateAvailabilityGrace CheckoutState = "availability_grace"
	// CheckoutStateRemovalGrace means Git authoritatively stopped listing the
	// checkout but its removal deadline has not expired. Queries fall back to
	// the family primary while the identity remains recoverable.
	CheckoutStateRemovalGrace CheckoutState = "removal_grace"
	// CheckoutStateUnavailable is a legacy persisted value from releases that
	// retained an inaccessible checkout after grace. Current reconciliation
	// retires it at the deadline; accepting the value keeps upgrades readable.
	CheckoutStateUnavailable CheckoutState = "checkout_unavailable"
	// CheckoutStateReconciling means a reconciler is bringing the checkout's
	// effective mode in line with its desired mode.
	CheckoutStateReconciling CheckoutState = "reconciling"
	// CheckoutStateDemoting means the checkout is dropping from a dedicated
	// graph back to the automatic lane.
	CheckoutStateDemoting CheckoutState = "demoting"
	// CheckoutStateForgetting means the checkout is being removed from the
	// catalog and its dependent rows released.
	CheckoutStateForgetting CheckoutState = "forgetting_checkout"
	// CheckoutStatePrimaryClosureRetiring means the whole family's primary
	// closure is retiring and this checkout is going with it.
	CheckoutStatePrimaryClosureRetiring CheckoutState = "primary_closure_retiring"
)

var checkoutStates = []CheckoutState{
	CheckoutStateReady,
	CheckoutStateAvailabilityGrace,
	CheckoutStateRemovalGrace,
	CheckoutStateUnavailable,
	CheckoutStateReconciling,
	CheckoutStateDemoting,
	CheckoutStateForgetting,
	CheckoutStatePrimaryClosureRetiring,
}

// CheckoutMode is how a checkout is served: from its own graph, or from the
// family's shared automatic lane.
type CheckoutMode string

const (
	// CheckoutModeDedicated serves the checkout from its own graph.
	CheckoutModeDedicated CheckoutMode = "dedicated"
	// CheckoutModeAutomatic serves the checkout from the family's shared lane.
	CheckoutModeAutomatic CheckoutMode = "automatic"
)

var checkoutModes = []CheckoutMode{CheckoutModeDedicated, CheckoutModeAutomatic}

// ViewGenerationState is the lifecycle state of one view generation. It also
// governs ref_view_builds.state: a build's state is the state of the
// generation it is producing, and the in-flight coalescing index keys on the
// building value.
type ViewGenerationState string

const (
	// ViewGenerationBuilding is the only mutable state.
	ViewGenerationBuilding ViewGenerationState = "building"
	// ViewGenerationReady is published and immutable.
	ViewGenerationReady ViewGenerationState = "ready"
	// ViewGenerationSuperseded is a ready generation a newer one replaced.
	ViewGenerationSuperseded ViewGenerationState = "superseded"
	// ViewGenerationRetiring is queued for deletion once nothing points at it.
	ViewGenerationRetiring ViewGenerationState = "retiring"
	// ViewGenerationFailed never reached ready.
	ViewGenerationFailed ViewGenerationState = "failed"
)

var viewGenerationStates = []ViewGenerationState{
	ViewGenerationBuilding,
	ViewGenerationReady,
	ViewGenerationSuperseded,
	ViewGenerationRetiring,
	ViewGenerationFailed,
}

// IntentSourceKind is who asked for a checkout to be tracked.
type IntentSourceKind string

const (
	// IntentSourceCLITrack is an explicit `gortex` command.
	IntentSourceCLITrack IntentSourceKind = "cli_track"
	// IntentSourceMCPTrack is an explicit tool call over MCP.
	IntentSourceMCPTrack IntentSourceKind = "mcp_track"
	// IntentSourceManualConfig is a checkout named in configuration.
	IntentSourceManualConfig IntentSourceKind = "manual_config"
	// IntentSourceProjectMembership is a checkout tracked because it belongs
	// to a tracked project rather than because anyone named it.
	IntentSourceProjectMembership IntentSourceKind = "project_membership"
)

var intentSourceKinds = []IntentSourceKind{
	IntentSourceCLITrack,
	IntentSourceMCPTrack,
	IntentSourceManualConfig,
	IntentSourceProjectMembership,
}

// IntentTransitionState is the progress of the single in-flight mode change a
// checkout may have. There is no terminal success value: completing a
// transition deletes the row, because UNIQUE(checkout_id) is what stops a
// second transition from starting and a retained success row would block it
// forever. A failed transition is retained deliberately — it keeps the slot
// occupied until something resolves it.
type IntentTransitionState string

const (
	// IntentTransitionPending is created but not yet started.
	IntentTransitionPending IntentTransitionState = "pending"
	// IntentTransitionRunning is being applied.
	IntentTransitionRunning IntentTransitionState = "running"
	// IntentTransitionFailed stopped part-way and holds the slot.
	IntentTransitionFailed IntentTransitionState = "failed"
)

var intentTransitionStates = []IntentTransitionState{
	IntentTransitionPending,
	IntentTransitionRunning,
	IntentTransitionFailed,
}

// RefViewState is the lifecycle state of a named view of a graph.
type RefViewState string

const (
	// RefViewPending has a desired selector but nothing resolved yet.
	RefViewPending RefViewState = "pending"
	// RefViewBuilding has an in-flight build for its desired fingerprint.
	RefViewBuilding RefViewState = "building"
	// RefViewReady is serving its active generation.
	RefViewReady RefViewState = "ready"
	// RefViewStale is still serving, but the desired selector has moved on.
	RefViewStale RefViewState = "stale"
	// RefViewFailed could not resolve or build.
	RefViewFailed RefViewState = "failed"
)

var refViewStates = []RefViewState{
	RefViewPending,
	RefViewBuilding,
	RefViewReady,
	RefViewStale,
	RefViewFailed,
}

// RouteState is the lifecycle state of one checkout's route.
type RouteState string

const (
	// RoutePending is not an exact checkout view. A new route has no generation
	// yet; a dedicated-base refresh may retain the previous sealed pointers so
	// pinned readers and labeled base fallback remain available while exact
	// routing is fenced by the pending state and advanced route epoch.
	RoutePending RouteState = "pending"
	// RouteActive is serving.
	RouteActive RouteState = "active"
	// RouteRetired no longer serves; its generations may be released.
	RouteRetired RouteState = "retired"
)

var routeStates = []RouteState{RoutePending, RouteActive, RouteRetired}

// RouteSlot names one of the two generation pointers a checkout's route
// carries: the generation built from the checkout's commit, and the one built
// from its uncommitted working tree.
type RouteSlot string

const (
	// RouteSlotCommit is the commit_generation_id pointer.
	RouteSlotCommit RouteSlot = "commit"
	// RouteSlotDirty is the dirty_generation_id pointer.
	RouteSlotDirty RouteSlot = "dirty"
)

var routeSlots = []RouteSlot{RouteSlotCommit, RouteSlotDirty}

// CleanupPhase is how far a journal entry has progressed.
type CleanupPhase string

const (
	// CleanupPhasePending is recorded but not started.
	CleanupPhasePending CleanupPhase = "pending"
	// CleanupPhaseGrace is waiting out its grace deadline.
	CleanupPhaseGrace CleanupPhase = "grace"
	// CleanupPhaseDeleting is actively removing its targets.
	CleanupPhaseDeleting CleanupPhase = "deleting"
	// CleanupPhaseDone finished; the entry is kept as evidence.
	CleanupPhaseDone CleanupPhase = "done"
	// CleanupPhaseFailed stopped part-way and needs another attempt.
	CleanupPhaseFailed CleanupPhase = "failed"
)

var cleanupPhases = []CleanupPhase{
	CleanupPhasePending,
	CleanupPhaseGrace,
	CleanupPhaseDeleting,
	CleanupPhaseDone,
	CleanupPhaseFailed,
}

// Catalog errors. Every guarded transition reports its refusal through one of
// these, so a caller can tell "someone else moved first" (retry with fresh
// state) from "this write was malformed" (a bug) without string matching.
var (
	// ErrCatalogStaleGuard means a guarded transition's expected precondition
	// — an incarnation, a route epoch, a primary epoch, or a generation still
	// being in the building state — no longer matched the stored row. Nothing
	// was written.
	ErrCatalogStaleGuard = errors.New("store_sqlite: catalog guard mismatch, no rows changed")

	// ErrCatalogNotFound means the addressed catalog row does not exist.
	ErrCatalogNotFound = errors.New("store_sqlite: catalog row not found")

	// ErrCatalogGenerationReferenced means a view generation cannot be deleted
	// because a route, a ref view, a dedicated graph, or another generation's
	// base pointer still names it.
	ErrCatalogGenerationReferenced = errors.New("store_sqlite: view generation is still referenced")

	// ErrCatalogIntentTransitionActive means the checkout already has an
	// in-flight intent transition; only one may exist at a time.
	ErrCatalogIntentTransitionActive = errors.New("store_sqlite: checkout already has an active intent transition")

	// ErrCatalogInvalidValue means a write carried a value outside the state
	// vocabulary, or left a required identifier empty.
	ErrCatalogInvalidValue = errors.New("store_sqlite: invalid catalog value")

	// ErrRefViewBuildInFlight means another attempt is already building the
	// same work — the same ref view, tree, base generation and fingerprint.
	// It is the coalescing index reporting itself through ClaimRefViewBuild,
	// and the caller's signal to wait on the attempt that is already running
	// rather than start a second one that would produce the same payload.
	ErrRefViewBuildInFlight = errors.New("store_sqlite: a build for this ref view is already in flight")
)

// RepositoryFamily is one physical repository object store — the identity a
// primary checkout and all of its linked worktrees share.
type RepositoryFamily struct {
	FamilyID          string
	CommonDirIdentity string
	DisplayRemote     string
	State             string
	PrimaryEpoch      int64
	CreatedAt         int64 // unix seconds
	LastSeen          int64 // unix seconds
}

// Checkout is one working copy the daemon knows about.
type Checkout struct {
	CheckoutID  string
	Incarnation string
	FamilyID    string
	RootPath    string
	GitDir      string
	AdminName   string

	State         CheckoutState
	DesiredMode   CheckoutMode
	EffectiveMode CheckoutMode

	Locked   bool
	Prunable bool

	HeadRef    string
	HeadCommit string
	HeadTree   string

	LastAccessible       int64 // unix seconds
	UnavailableSince     int64 // unix seconds
	AvailabilityDeadline int64 // unix seconds
	RemovalDetectedAt    int64 // unix seconds
	RemovalDeadline      int64 // unix seconds
	RemovalEvidence      string

	// ActiveIntentTransitionID is empty when no transition is in flight; it is
	// stored as NULL in that case.
	ActiveIntentTransitionID string

	LastSeen  int64 // unix seconds
	LastError string
}

// TrackingIntent is one reason the daemon tracks a checkout.
type TrackingIntent struct {
	IntentID      string
	CheckoutID    string
	SourceKind    IntentSourceKind
	SourceLocator string
	Active        bool
	CreatedAt     int64 // unix seconds
	RevokedAt     int64 // unix seconds
	LastError     string
}

// IntentTransition is the single in-flight mode change for a checkout. The
// prior mode and state fields may be empty when the transition did not need to
// capture them; when set they must name a valid vocabulary value.
type IntentTransition struct {
	TransitionID       string
	CheckoutID         string
	Cause              string
	PriorDesiredMode   CheckoutMode
	PriorEffectiveMode CheckoutMode
	RequestedMode      CheckoutMode
	PriorCheckoutState CheckoutState
	SourceSnapshotHash string
	State              IntentTransitionState
	CreatedAt          int64 // unix seconds
	LastProgress       int64 // unix seconds
	LastError          string
}

// CheckoutPathEvidence is the last filesystem sample taken for a checkout's
// path: enough to tell a deleted working copy from a detached volume.
type CheckoutPathEvidence struct {
	CheckoutID                  string
	RootPathIdentity            string
	RootVolumeKind              string
	RootVolumeToken             string
	NearestExistingAncestorPath string
	AncestorVolumeKind          string
	AncestorVolumeToken         string
	CommonDirVolumeKind         string
	CommonDirVolumeToken        string
	SampledAt                   int64 // unix seconds
	SampleGeneration            int64
}

// DedicatedGraphStateReady is the persisted catalog spelling for a dedicated
// graph that may serve queries. The catalog owns the vocabulary because SQL
// guards must compare the same value lifecycle writers persist.
const DedicatedGraphStateReady = "graph_ready"

// DedicatedGraphStateRefreshing keeps the previous sealed base readable as a
// labeled fallback while a replacement pipeline generation builds off-route.
const DedicatedGraphStateRefreshing = "graph_refreshing"

// DedicatedGraph is a graph built for one checkout. ActiveGenerationID is 0
// when no generation is published yet.
type DedicatedGraph struct {
	GraphID            string
	OwnerCheckoutID    string
	RepoPrefix         string
	FamilyID           string
	IsPrimaryBase      bool
	ActiveGenerationID int64
	State              string
}

// ViewGeneration is one build of a view. GenerationID is assigned by the
// store; BaseGenerationID is 0 when the generation has no lower layer.
type ViewGeneration struct {
	GenerationID         int64
	OwnerKind            string
	GraphID              string
	LayerID              string
	CheckoutID           string
	GenerationKind       string
	BaseGenerationID     int64
	LowerViewFingerprint string
	TreeOID              string
	ProvenanceCommitOID  string
	ConfigHash           string
	ExtractorVersions    string
	ResolverVersion      string
	State                ViewGenerationState
	CoveredFiles         int64
	AffectedFiles        int64
	StorageBytes         int64
	Completeness         string
	CreatedAt            int64 // unix seconds
	PublishedAt          int64 // unix seconds
	LastSelected         int64 // unix seconds
	Error                string
}

// ViewGenerationFilter narrows a ListViewGenerations scan. Every field is
// optional and the ones that are set are ANDed, so the zero filter enumerates
// the newest generations in the table.
//
// It exists because a generation is otherwise only reachable through something
// that points at it — a route, a ref view, a base pointer, or an id an owner
// happens to still be holding. A process that dies between superseding a
// generation and retiring it drops the last of those, and without an
// enumeration the payload is unreachable for the life of the database.
type ViewGenerationFilter struct {
	// States restricts the scan to these lifecycle states. Empty accepts all
	// of them.
	States []ViewGenerationState
	// CheckoutID restricts the scan to the generations built for one
	// checkout. Empty accepts all of them, including the generations that
	// name no checkout at all.
	CheckoutID string
	// GraphID restricts the scan to one graph. Empty accepts all of them.
	GraphID string
	// MissingGraph accepts only generations whose non-empty graph_id no
	// longer names a dedicated graph. This is the durable retirement backlog
	// left when graph deletion wins a race with process shutdown.
	MissingGraph bool
	// OwnerKind restricts the scan to one owner vocabulary. Empty accepts all
	// of them.
	OwnerKind string
	// BeforeGenerationID restricts the scan to generations older than this
	// exclusive cursor. Zero starts at the newest generation.
	BeforeGenerationID int64
	// Limit bounds the rows one call returns. 0 — and anything above the cap —
	// takes maxViewGenerationListing.
	Limit int
}

// ViewLayer names the git state a generation is built over.
type ViewLayer struct {
	LayerID      string
	Kind         string
	GraphID      string
	CheckoutID   string
	TargetRef    string
	TargetCommit string
	TargetTree   string
}

// CheckoutRoute is where a checkout's queries currently land. The generation
// pointers are 0 when unset.
type CheckoutRoute struct {
	CheckoutID         string
	GraphID            string
	CommitGenerationID int64
	DirtyGenerationID  int64
	RouteEpoch         int64
	State              RouteState
}

// RefView is a named view of one graph at a selector.
type RefView struct {
	RefViewID     string
	GraphID       string
	SelectorKind  string
	SelectorValue string

	DesiredRef    string
	DesiredCommit string
	DesiredTree   string

	ActiveGenerationID int64
	ActiveRef          string
	ActiveCommit       string
	ActiveTree         string

	EnrichmentProfile       string
	DesiredBuildFingerprint string
	ActiveBuildFingerprint  string

	RouteEpoch   int64
	State        RefViewState
	ExactView    bool
	LastResolved int64 // unix seconds
	LastSelected int64 // unix seconds
	LastError    string
}

// RefViewBuild is one build attempt for a ref view.
type RefViewBuild struct {
	BuildID            string
	RefViewID          string
	DesiredRef         string
	DesiredCommit      string
	DesiredTree        string
	BaseGenerationID   int64
	EnrichmentProfile  string
	BuildFingerprint   string
	GenerationID       int64
	CapturedRouteEpoch int64
	State              ViewGenerationState
	BuildToken         string
	CreatedAt          int64 // unix seconds
	LastProgress       int64 // unix seconds
	Error              string
}

// UpdateRefViewDesireRequest records what a selection just resolved the view's
// selector to.
//
// The route epoch is not a guard here and not a parameter: the desire write
// bumps it only when the tree or the fingerprint actually changed, so two
// selections that resolve to the same state do not invalidate each other's
// in-flight builds, while a selection that re-targets the view does exactly
// that.
type UpdateRefViewDesireRequest struct {
	RefViewID string

	DesiredRef              string
	DesiredCommit           string
	DesiredTree             string
	DesiredBuildFingerprint string

	State        RefViewState
	LastResolved int64 // unix seconds
	LastSelected int64 // unix seconds
}

// AdoptRefViewGenerationRequest points a ref view at the generation a finished
// build produced, and closes the attempt that produced it.
//
// The three expectations are the compare-and-set: the route epoch the build
// captured when it was claimed, plus the tree and fingerprint it was built
// for. A view that was re-targeted while the build ran matches none of them,
// so the adoption changes nothing and reports ErrCatalogStaleGuard rather than
// serving a payload for a state the selector has left.
//
// BuildID and BuildToken are the fourth expectation, and the reason the two
// writes are one: the claim is the right to publish, so a build whose slot was
// reclaimed while it ran may not adopt behind its successor, and a build the
// view refuses may not be recorded as having published.
type AdoptRefViewGenerationRequest struct {
	RefViewID          string
	ExpectedRouteEpoch int64

	ExpectedDesiredTree             string
	ExpectedDesiredBuildFingerprint string

	// BuildID and BuildToken name the claim the adoption is made under. An
	// empty BuildID adopts without closing an attempt.
	BuildID    string
	BuildToken string
	// LastProgress is the clock stamped on the closed attempt.
	LastProgress int64 // unix seconds

	GenerationID           int64
	ActiveRef              string
	ActiveCommit           string
	ActiveTree             string
	ActiveBuildFingerprint string

	ExactView    bool
	LastResolved int64 // unix seconds
	LastSelected int64 // unix seconds
}

// TouchRefViewSelectionRequest re-stamps what a selection observed without
// changing what the view serves: the ref and commit the selector resolves to
// now, and the selection clock. It is the write behind a ref that moved to a
// different commit carrying the same tree — the payload is unchanged, so only
// the metadata moves.
type TouchRefViewSelectionRequest struct {
	RefViewID          string
	ExpectedRouteEpoch int64

	ActiveRef    string
	ActiveCommit string
	LastResolved int64 // unix seconds
	LastSelected int64 // unix seconds
}

// FailRefViewRequest records why a selection could not be served. It never
// touches the active pointer: a view whose newest build failed keeps serving
// what it was serving, labelled inexact by whoever reads it.
type FailRefViewRequest struct {
	RefViewID          string
	ExpectedRouteEpoch int64

	LastError    string
	LastResolved int64 // unix seconds
}

// CompleteRefViewBuildRequest ends one build attempt. BuildToken is the proof
// that the caller still owns the attempt it started, and the building state is
// the other half of the guard, so an attempt can only be completed once.
type CompleteRefViewBuildRequest struct {
	BuildID    string
	BuildToken string

	State        ViewGenerationState
	GenerationID int64
	LastProgress int64 // unix seconds
	Error        string
}

// CleanupEntry is one unit of deferred deletion work.
type CleanupEntry struct {
	CleanupID       string
	OpaqueTargetIDs string
	Reason          string
	Phase           CleanupPhase
	GraceDeadline   int64 // unix seconds
	PrimaryEpoch    int64
	LastProgress    int64 // unix seconds
	LastError       string
}

// UpdateCheckoutStateRequest is a guarded checkout-state write. Incarnation is
// the expectation, not a new value: a request naming the previous incarnation
// of a recreated path changes nothing and reports ErrCatalogStaleGuard.
type UpdateCheckoutStateRequest struct {
	CheckoutID    string
	Incarnation   string
	State         CheckoutState
	DesiredMode   CheckoutMode
	EffectiveMode CheckoutMode
	LastSeen      int64 // unix seconds
	LastError     string
}

// UpdateCheckoutObservationRequest is everything one reconciliation pass may
// change about a checkout it just looked at: the state axis, the availability
// clock, the removal clock, and the git / filesystem facts it observed.
//
// Incarnation is the expectation, not a new value — the same guard
// UpdateCheckoutStateRequest carries. The identity columns are absent on
// purpose: an observation describes the row it found, it never re-keys it.
// The mode columns are absent for the same kind of reason: how a checkout is
// served is the mode transition's business, and an observer that carried the
// modes along would be able to revert one it never meant to touch.
type UpdateCheckoutObservationRequest struct {
	CheckoutID  string
	Incarnation string

	State CheckoutState

	RootPath string
	GitDir   string
	Locked   bool
	Prunable bool

	HeadRef    string
	HeadCommit string
	HeadTree   string

	LastAccessible       int64 // unix seconds
	UnavailableSince     int64 // unix seconds
	AvailabilityDeadline int64 // unix seconds
	RemovalDetectedAt    int64 // unix seconds
	RemovalDeadline      int64 // unix seconds
	RemovalEvidence      string

	LastSeen  int64 // unix seconds
	LastError string
}

func (r UpdateCheckoutObservationRequest) validate() error {
	if err := requireCatalogID("checkout_id", r.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", r.Incarnation); err != nil {
		return err
	}
	return requireCatalogValue("state", r.State, checkoutStates)
}

// FlipCheckoutRouteRequest repoints one checkout's route. ExpectedRouteEpoch
// is the compare-and-set token; a successful flip stores epoch+1. Generation
// pointers of 0 clear the corresponding column.
type FlipCheckoutRouteRequest struct {
	CheckoutID         string
	ExpectedRouteEpoch int64
	GraphID            string
	CommitGenerationID int64
	DirtyGenerationID  int64
	State              RouteState
	// RequireActiveGraphBase closes publication against a concurrent
	// dedicated-base refresh. Zero preserves catalog-only/legacy callers.
	RequireActiveGraphBase bool
	// ExpectedBaseGenerationID is the immutable base epoch captured before
	// building the route's sparse generations. It is required when
	// RequireActiveGraphBase is true.
	ExpectedBaseGenerationID int64
}

// FlipCheckoutRouteSlotRequest repoints one slot of a checkout's route.
// ExpectedRouteEpoch is the compare-and-set token; a successful flip stores
// epoch+1. A GenerationID of 0 clears the slot. The other slot is not named by
// the statement and keeps whatever it held.
type FlipCheckoutRouteSlotRequest struct {
	CheckoutID         string
	Slot               RouteSlot
	GenerationID       int64
	ExpectedRouteEpoch int64
	State              RouteState
	// RequireActiveGraphBase closes publication against a concurrent
	// dedicated-base refresh. Zero preserves catalog-only/legacy callers.
	RequireActiveGraphBase bool
	// ExpectedBaseGenerationID is the immutable base epoch captured before
	// building the slot's sparse generation. It is required when
	// RequireActiveGraphBase is true.
	ExpectedBaseGenerationID int64
}

// SetPrimaryDedicatedGraphRequest promotes one graph to its family's primary
// base. ExpectedPrimaryEpoch is the compare-and-set token on the family row; a
// successful promotion stores epoch+1.
type SetPrimaryDedicatedGraphRequest struct {
	FamilyID             string
	GraphID              string
	ExpectedPrimaryEpoch int64
	LastSeen             int64 // unix seconds
}

// validCatalogValue reports whether value is one of allowed.
func validCatalogValue[T ~string](value T, allowed []T) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// requireCatalogValue rejects a value outside its vocabulary.
func requireCatalogValue[T ~string](field string, value T, allowed []T) error {
	if validCatalogValue(value, allowed) {
		return nil
	}
	return fmt.Errorf("%w: %s %q", ErrCatalogInvalidValue, field, string(value))
}

// optionalCatalogValue rejects a non-empty value outside its vocabulary and
// accepts the empty string as "not captured".
func optionalCatalogValue[T ~string](field string, value T, allowed []T) error {
	if value == "" {
		return nil
	}
	return requireCatalogValue(field, value, allowed)
}

// requireCatalogID rejects an empty identifier. Every catalog key is a caller-
// supplied opaque string, so the empty one would otherwise become a real row.
func requireCatalogID(field, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s must not be empty", ErrCatalogInvalidValue, field)
	}
	return nil
}

func (f RepositoryFamily) validate() error {
	if err := requireCatalogID("family_id", f.FamilyID); err != nil {
		return err
	}
	if err := requireCatalogID("common_dir_identity", f.CommonDirIdentity); err != nil {
		return err
	}
	return requireCatalogID("state", f.State)
}

func (c Checkout) validate() error {
	if err := requireCatalogID("checkout_id", c.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", c.Incarnation); err != nil {
		return err
	}
	if err := requireCatalogID("family_id", c.FamilyID); err != nil {
		return err
	}
	if err := requireCatalogValue("state", c.State, checkoutStates); err != nil {
		return err
	}
	if err := requireCatalogValue("desired_mode", c.DesiredMode, checkoutModes); err != nil {
		return err
	}
	return requireCatalogValue("effective_mode", c.EffectiveMode, checkoutModes)
}

func (i TrackingIntent) validate() error {
	if err := requireCatalogID("intent_id", i.IntentID); err != nil {
		return err
	}
	if err := requireCatalogID("checkout_id", i.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogValue("source_kind", i.SourceKind, intentSourceKinds); err != nil {
		return err
	}
	return requireCatalogID("source_locator", i.SourceLocator)
}

func (t IntentTransition) validate() error {
	if err := requireCatalogID("transition_id", t.TransitionID); err != nil {
		return err
	}
	if err := requireCatalogID("checkout_id", t.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("cause", t.Cause); err != nil {
		return err
	}
	if err := requireCatalogValue("state", t.State, intentTransitionStates); err != nil {
		return err
	}
	if err := optionalCatalogValue("prior_desired_mode", t.PriorDesiredMode, checkoutModes); err != nil {
		return err
	}
	if err := optionalCatalogValue("prior_effective_mode", t.PriorEffectiveMode, checkoutModes); err != nil {
		return err
	}
	if err := optionalCatalogValue("requested_mode", t.RequestedMode, checkoutModes); err != nil {
		return err
	}
	return optionalCatalogValue("prior_checkout_state", t.PriorCheckoutState, checkoutStates)
}

func (g DedicatedGraph) validate() error {
	if err := requireCatalogID("graph_id", g.GraphID); err != nil {
		return err
	}
	if err := requireCatalogID("repo_prefix", g.RepoPrefix); err != nil {
		return err
	}
	if err := requireCatalogID("family_id", g.FamilyID); err != nil {
		return err
	}
	return requireCatalogID("state", g.State)
}

func (g ViewGeneration) validate() error {
	if err := requireCatalogID("owner_kind", g.OwnerKind); err != nil {
		return err
	}
	if err := requireCatalogID("generation_kind", g.GenerationKind); err != nil {
		return err
	}
	return requireCatalogValue("state", g.State, viewGenerationStates)
}

func (l ViewLayer) validate() error {
	if err := requireCatalogID("layer_id", l.LayerID); err != nil {
		return err
	}
	if err := requireCatalogID("kind", l.Kind); err != nil {
		return err
	}
	return requireCatalogID("graph_id", l.GraphID)
}

func (r CheckoutRoute) validate() error {
	if err := requireCatalogID("checkout_id", r.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", r.GraphID); err != nil {
		return err
	}
	return requireCatalogValue("state", r.State, routeStates)
}

func (v RefView) validate() error {
	if err := requireCatalogID("ref_view_id", v.RefViewID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", v.GraphID); err != nil {
		return err
	}
	if err := requireCatalogID("selector_kind", v.SelectorKind); err != nil {
		return err
	}
	if err := requireCatalogID("selector_value", v.SelectorValue); err != nil {
		return err
	}
	if err := requireCatalogID("enrichment_profile", v.EnrichmentProfile); err != nil {
		return err
	}
	return requireCatalogValue("state", v.State, refViewStates)
}

func (b RefViewBuild) validate() error {
	if err := requireCatalogID("build_id", b.BuildID); err != nil {
		return err
	}
	if err := requireCatalogID("ref_view_id", b.RefViewID); err != nil {
		return err
	}
	if err := requireCatalogID("build_fingerprint", b.BuildFingerprint); err != nil {
		return err
	}
	if err := requireCatalogID("build_token", b.BuildToken); err != nil {
		return err
	}
	return requireCatalogValue("state", b.State, viewGenerationStates)
}

func (e CleanupEntry) validate() error {
	if err := requireCatalogID("cleanup_id", e.CleanupID); err != nil {
		return err
	}
	if err := requireCatalogID("opaque_target_ids", e.OpaqueTargetIDs); err != nil {
		return err
	}
	if err := requireCatalogID("reason", e.Reason); err != nil {
		return err
	}
	return requireCatalogValue("phase", e.Phase, cleanupPhases)
}
