package viewmetrics

import "strings"

// The series catalog.
//
// Every series the view lifecycle can emit is declared here with the complete
// vocabulary of each of its labels. Adding a counter is a deliberate edit in
// this file and nowhere else, and a value that is not listed collapses to
// LabelOther — so the number of series this process can ever hold is a
// property of this file rather than of the workload.
//
// Nothing here may carry an identity. Checkout ids, generation ids, ref
// names, paths and view fingerprints are what the log lines at the same seams
// carry; a metric aggregates by family-count, state, slot and reason class.

// Series names. The prefix is the subsystem, the suffix is the unit: _total
// for a counter, _seconds for a duration, and a bare noun for a gauge.
const (
	// FamilyInventorySeconds is how long enumerating one checkout family's
	// worktrees took, split by whether git answered.
	FamilyInventorySeconds = "views_family_inventory_seconds"
	// FamilyDiscoveryLagSeconds is how long a checkout existed before a
	// reconciliation pass first observed it — the staleness window between a
	// worktree being created and the daemon knowing about it.
	FamilyDiscoveryLagSeconds = "views_family_discovery_lag_seconds"
	// Families is how many checkout families the last sweep reconciled.
	Families = "views_families"

	// CheckoutTransitionTotal counts lifecycle state changes, by the states
	// moved between and the evidence class that justified the move.
	CheckoutTransitionTotal = "views_checkout_transition_total"
	// AvailabilityClocks and RemovalClocks are how many checkouts currently
	// have each of the two grace clocks running. They are separate series
	// because the two clocks are separate axes: a checkout can be
	// unreachable for an hour without being any closer to being forgotten.
	AvailabilityClocks = "views_checkout_availability_clocks"
	RemovalClocks      = "views_checkout_removal_clocks"

	// CoordinatorCycleTotal counts reconcile cycles by what the cycle did.
	CoordinatorCycleTotal = "views_coordinator_cycle_total"
	// CoordinatorBuildSeconds is how long one slot's build took.
	CoordinatorBuildSeconds = "views_coordinator_build_seconds"
	// ViewBuildAdmissionTotal counts admissions by priority and terminal outcome.
	ViewBuildAdmissionTotal = "views_build_admission_total"
	// ViewBuildQueue is the number of callers waiting for the physical build lane.
	ViewBuildQueue = "views_build_queue"
	// ViewBuildWaitSeconds is how long a queued caller waited for admission or cancellation.
	ViewBuildWaitSeconds = "views_build_wait_seconds"
	// Coordinators is how many checkout coordinators the registry holds. A
	// coordinator a transition is driving a rebuild with is running and not
	// registered; the administrative surfaces count those, this level does not.
	Coordinators = "views_coordinators"

	// GenerationPublishedTotal, GenerationSupersededTotal and
	// GenerationRetiredTotal are the payload generation lifecycle, by who
	// owns the generation.
	GenerationPublishedTotal  = "views_generation_published_total"
	GenerationSupersededTotal = "views_generation_superseded_total"
	GenerationRetiredTotal    = "views_generation_retired_total"
	// GenerationRetireRefusedTotal counts retirements the guard refused, by
	// what was still holding the generation. It is the direct answer to "why
	// does this generation still exist".
	GenerationRetireRefusedTotal = "views_generation_retire_refused_total"
	// GenerationSweepCollectedTotal counts generations a janitor pass
	// collected that an earlier offer could not.
	GenerationSweepCollectedTotal = "views_generation_sweep_collected_total"

	// The committed-base (dedicated-base) publication family.
	//
	// These six are the measurement evidence for incremental advancement: the
	// generation lifecycle series above say how much payload exists, and these
	// say how it came to exist — whether a committed advance paid for a
	// physical build or re-adopted work that was already there. The ratio
	// between DedicatedBaseClaimTotal{outcome=built} and its reused/coalesced
	// siblings is the literal reuse-vs-rebuild figure; before them the only
	// reuse evidence in the process was CoordinatorCycleTotal{adopted_commit},
	// which the committed-base path never touches.

	// DedicatedBasePublishTotal counts committed-base generations this
	// process published and adopted, by the shape of the generation. A root
	// is self-contained; a delta composes over a parent. A steady state whose
	// roots keep rising is re-rooting rather than advancing.
	DedicatedBasePublishTotal = "views_dedicated_base_publish_total"
	// DedicatedBaseClaimTotal counts committed-base build claims by what the
	// claim actually cost: a physical build, a coalesced join onto somebody
	// else's build, or a re-adoption of the generation that was already
	// active (the zero-catalog-DML replay a warm restart or a same-tree
	// commit takes).
	DedicatedBaseClaimTotal = "views_dedicated_base_claim_total"
	// DedicatedBaseClosureTruncatedTotal counts published committed-base
	// generations whose affected-by closure hit its cap and was cut. Such a
	// generation is knowingly incomplete, so this is a correctness level
	// rather than a cost one: it must stay at zero.
	DedicatedBaseClosureTruncatedTotal = "views_dedicated_base_closure_truncated_total"
	// DedicatedBasePublicationTotal counts queued publication outcomes — both
	// the ones a daemon start scheduled and the ones a live HEAD movement
	// asked for — by what the publication settled as.
	DedicatedBasePublicationTotal = "views_dedicated_base_publication_total"
	// DedicatedBaseAdvanceTotal counts what the live HEAD-movement observer
	// did with an observation, which is not the same question as what the
	// publication behind it settled as: a repeat observation of the commit
	// that already landed never becomes a publication at all.
	DedicatedBaseAdvanceTotal = "views_dedicated_base_advance_total"
	// DedicatedBaseDrainTotal counts publisher-admission closures by whether
	// shutdown had to wait for admitted actors to finish. A waited close is
	// the shape of a shutdown held open by a running publication.
	DedicatedBaseDrainTotal = "views_dedicated_base_drain_total"

	// DependentRecompositionTotal counts the cohort-staleness signals a
	// committed advance raises for the dependents that key their identities
	// on the moved repository's bytes. It is the fan-out counterpart of
	// DedicatedBasePublishTotal: one advance, N dependents told to recompose.
	DependentRecompositionTotal = "views_dependent_recomposition_total"

	// RefViewSelectionTotal counts ref-view selections by what serving the
	// selector took.
	RefViewSelectionTotal = "views_ref_view_selection_total"
	// RefViewEvictedTotal counts ref-view generations the retention bounds
	// collected, by which bound decided it.
	RefViewEvictedTotal = "views_ref_view_evicted_total"

	// MaterializationTotal counts attempts to turn a route or a ref-view
	// generation into a readable view.
	MaterializationTotal = "views_materialization_total"
	// LeasesHeld is how many payload generations live views currently pin.
	LeasesHeld = "views_leases_held"
	// HandoffTotal counts attempts to hand a request's materialized view to
	// work that outlives that request — a detached worker, an admitted
	// publication, a handler the deadline firewall abandoned — by which
	// consumer asked and whether the lease could still be joined.
	HandoffTotal = "views_handoff_total"
	// HandoffsOutstanding is how many handed-off view leases are open right
	// now: detached work still pinning the generations its request read. It
	// is the level paired with HandoffTotal{outcome=joined}, and it must
	// return to zero once every detached worker has released. A level that
	// does not come back down is the observable shape of an unreleased
	// handoff — retirement will keep refusing a generation with nobody left
	// to finish releasing it.
	HandoffsOutstanding = "views_handoffs_outstanding"

	// RequestServedTotal counts requests by the kind of view that answered.
	RequestServedTotal = "views_request_served_total"
	// RequestFallbackTotal counts requests that asked for one view and were
	// answered from another, by the code that explains the substitution.
	RequestFallbackTotal = "views_request_fallback_total"
	// CapabilityRefusedTotal counts requests refused because the view could
	// not serve a capability they required.
	CapabilityRefusedTotal = "views_capability_refused_total"

	// SearchQueryTotal and SearchSourceTotal are the composite-search pair:
	// how many searches ran over a composed view's stack, and how many of
	// the stack's corpora contributed a surviving hit. Read together they
	// answer "which layers served a query".
	SearchQueryTotal  = "views_search_query_total"
	SearchSourceTotal = "views_search_source_total"

	// CheckoutSearcherBuiltTotal and CheckoutSearcherInvalidatedTotal are the
	// per-checkout trigram index: how often one was built over a working
	// tree, and how often a moved working tree made the cached one miss.
	CheckoutSearcherBuiltTotal       = "views_checkout_searcher_built_total"
	CheckoutSearcherInvalidatedTotal = "views_checkout_searcher_invalidated_total"

	// LSPWorkspaceTotal counts admissions to the per-checkout language-server
	// workspace cap, by what the admission did.
	LSPWorkspaceTotal = "views_lsp_workspace_total"

	// ProbeAnswerTotal counts path-scoped probe answers by the kind of view
	// that answered and whether it was the path's own.
	ProbeAnswerTotal = "views_probe_answer_total"
)

// Label names.
const (
	LabelOutcome  = "outcome"
	LabelPriority = "priority"
	LabelFrom     = "from"
	LabelTo       = "to"
	LabelEvidence = "evidence"
	LabelSlot     = "slot"
	LabelOwner    = "owner"
	LabelReason   = "reason"
	LabelSweep    = "sweep"
	LabelKind     = "kind"
	LabelCode     = "code"
	LabelCorpus   = "corpus"
	LabelEvent    = "event"
	LabelExact    = "exact"
	LabelConsumer = "consumer"
	LabelShape    = "shape"
)

// Committed-base generation shapes: self-contained, or composed over a parent.
const (
	DedicatedBaseRoot  = "root"
	DedicatedBaseDelta = "delta"
)

// Committed-base claim outcomes — what one claim cost.
//
// Built is the only one that represents physical payload work. Coalesced
// joined a build somebody else was already running (BuildReport.Coalesced).
// Reused is the catalog's adopted-replay path: the observed identity equalled
// the active generation's, so adoption re-confirmed it and wrote nothing.
const (
	DedicatedBaseBuilt     = "built"
	DedicatedBaseCoalesced = "coalesced"
	DedicatedBaseReused    = "reused"
)

// Publication outcomes, as the publisher's recording point sees them. They are
// mutually exclusive and one is recorded per settled publication: a failure, a
// repository nothing was attempted for, a re-adoption, a publication that
// reused a physical build, or one that paid for it.
const (
	PublicationPublished = "published"
	PublicationCoalesced = "coalesced"
	PublicationReadopted = "readopted"
	PublicationSkipped   = "skipped"
	PublicationFailed    = "failed"
)

// Live advancement dispatch outcomes: what the HEAD-movement observer did with
// one observation. Repeat is the accepted-commit memo hit — the commit that
// already landed, observed again — and Refused is an observation that arrived
// after advancement stopped.
const (
	AdvanceDispatched = "dispatched"
	AdvanceRepeat     = "repeat"
	AdvanceRefused    = "refused"
)

// Publisher-admission drain outcomes: whether closing admission completed on
// the spot or left shutdown waiting for admitted actors.
const (
	DrainImmediate = "immediate"
	DrainWaited    = "waited"
)

// Handed-off view consumers: the classes of work a request can leave running
// behind it while still reading the payload it was served. Each names a call
// site's role, never a checkout, a path or a generation.
const (
	// HandoffCheckoutRefresh is publication admitted on a selected checkout's
	// coordinator after a source mutation committed to disk.
	HandoffCheckoutRefresh = "checkout_refresh"
	// HandoffFileMutation is the watcher-scheduled reindex of one mutated
	// file.
	HandoffFileMutation = "file_mutation"
	// HandoffRepositoryIndex is a first index detached from the request that
	// asked for it.
	HandoffRepositoryIndex = "repository_index"
	// HandoffAbandonedHandler is a handler the deadline firewall stopped
	// waiting for while it was still reading its view.
	HandoffAbandonedHandler = "abandoned_handler"
)

// handoffConsumers is the complete consumer vocabulary.
var handoffConsumers = []string{
	HandoffCheckoutRefresh,
	HandoffFileMutation,
	HandoffRepositoryIndex,
	HandoffAbandonedHandler,
}

// Handoff outcomes: the lease was joined, or every holder had already
// released and the handle was truthfully refused.
const (
	HandoffJoined  = "joined"
	HandoffRefused = "refused"
)

// Checkout lifecycle states, as a metric label sees them. They mirror
// store_sqlite's CheckoutState vocabulary; StateNone is the state a checkout
// that does not exist yet — or no longer does — is in, so a transition label
// is never the empty string.
const (
	StateNone                   = "none"
	StateReady                  = "checkout_ready"
	StateAvailabilityGrace      = "availability_grace"
	StateRemovalGrace           = "removal_grace"
	StateUnavailable            = "checkout_unavailable"
	StateReconciling            = "reconciling"
	StateDemoting               = "demoting"
	StateForgetting             = "forgetting_checkout"
	StatePrimaryClosureRetiring = "primary_closure_retiring"
)

// checkoutStates is the complete state vocabulary a transition label uses.
var checkoutStates = []string{
	StateNone,
	StateReady,
	StateAvailabilityGrace,
	StateRemovalGrace,
	StateUnavailable,
	StateReconciling,
	StateDemoting,
	StateForgetting,
	StatePrimaryClosureRetiring,
}

// Evidence classes. They are the classifier's verdict rather than only its
// removal evidence, because "which evidence classified this checkout" has to
// separate the two non-removal verdicts as well.
const (
	EvidencePresent               = "present"
	EvidenceInaccessible          = "inaccessible"
	EvidenceAuthoritativeOmission = "authoritative_omission"
	EvidencePrunableConfirmed     = "prunable_confirmed"
	EvidenceNone                  = "none"
)

// Coordinator cycle outcomes.
const (
	OutcomeBuiltCommit   = "built_commit"
	OutcomeAdoptedCommit = "adopted_commit"
	OutcomeBuiltDirty    = "built_dirty"
	OutcomeSkipped       = "skipped"
	OutcomeSuperseded    = "superseded"
	OutcomeRescheduled   = "rescheduled"
	OutcomeCASLost       = "cas_lost"
	OutcomeHeadMoved     = "head_moved"
	OutcomeFailed        = "failed"
	// OutcomeDeferred counts cycles held back by warmup or bounded admission
	// capacity. They are not skipped work: opening the gate or the coordinator
	// poll retries them. A sustained count after warmup indicates build-lane
	// saturation rather than a permanently rejected checkout.
	OutcomeDeferred = "deferred"
)

// Physical view-build admission priorities and outcomes. Both vocabularies
// are fixed: no checkout, ref, repository, or path identity enters metrics.
const (
	BuildPriorityInteractive = "interactive"
	BuildPriorityBackground  = "background"
	BuildAdmissionAdmitted   = "admitted"
	BuildAdmissionRejected   = "rejected"
	BuildAdmissionCanceled   = "canceled"
)

// Build slots.
const (
	SlotCommit = "commit"
	SlotDirty  = "dirty"
)

// Generation owners.
const (
	OwnerCheckout = "checkout"
	OwnerRefView  = "ref_view"
)

// Retire refusal reasons. They name what was still holding the generation,
// which is exactly the set of things the retire guard checks.
const (
	RefusedRouted  = "routed"
	RefusedBased   = "based"
	RefusedLeased  = "leased"
	RefusedMissing = "missing"
	RefusedError   = "error"
)

// Sweep lanes: whose backlog a collection came off.
const (
	SweepCheckout = "checkout"
	SweepRefView  = "ref_view"
)

// Ref-view selection outcomes. Every selection reports exactly one of ready,
// adopted, building, deferred, coalesced or failed; reclaimed is the extra
// note a selection makes when it took over a build claim a dead worker left
// behind, and is followed by whichever of the six that selection went on to be.
//
// Deferred is a building answer with a cause: warmup has not admitted the pass,
// or bounded admission closed its claim so the next selection can retry. A
// warmup-deferred token is worth polling; a capacity-deferred result has no
// token because that attempt is already closed.
const (
	RefViewReady     = "ready"
	RefViewAdopted   = "adopted"
	RefViewBuilding  = "building"
	RefViewDeferred  = "deferred"
	RefViewCoalesced = "coalesced"
	RefViewReclaimed = "reclaimed"
	RefViewFailed    = "failed"
)

// Ref-view eviction reasons: which retention bound decided it.
const (
	EvictedStale     = "stale"
	EvictedOverCount = "over_count"
	EvictedOverBytes = "over_bytes"
)

// View kinds. They are the three shapes a request can be answered from, and
// the same vocabulary a path-scoped probe answers with.
const (
	ViewBase     = "base"
	ViewWorktree = "worktree"
	ViewRef      = "ref"
	ViewUnrouted = "unrouted"
)

// Plain success/failure, for series whose only question is whether the step
// completed.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
)

// Probe answer exactness.
const (
	AnswerExact    = "exact"
	AnswerFallback = "fallback"
)

// Search corpora: the two composite lanes a composed view stacks.
const (
	CorpusSymbol  = "symbol"
	CorpusContent = "content"
)

// Language-server workspace admission events.
const (
	WorkspaceAcquired = "acquired"
	WorkspaceReused   = "reused"
	WorkspaceEvicted  = "evicted"
	WorkspaceStarved  = "starved"
)

// ViewErrorCodes is the fallback-reason vocabulary: the stable wire codes
// graphview produces. It is restated here rather than imported because the
// packages that emit these counters are the ones graphview itself is built
// under, and a metrics registry that imported the view layer could not be
// used from inside it. A drift test pins the two lists together.
var ViewErrorCodes = []string{
	"invalid_view_selector",
	"ref_not_commit",
	"selector_conflict",
	"selector_out_of_scope",
	"ref_not_available_locally",
	"view_building",
	"view_read_only",
	"capability_unavailable",
	"required_capability_incomplete",
	"checkout_inaccessible",
	"no_primary",
	"primary_not_ready",
	"source_object_missing",
}

// FallbackReasonCode extracts the stable code from a fallback reason.
//
// A rider's reason is written for a human and can carry detail after the code
// — a build token, a checkout state — which is exactly the kind of unbounded
// tail a label may not hold. The code is the leading token up to the first
// colon; anything the vocabulary does not know is clamped by the registry
// itself, so this only has to find the token.
func FallbackReasonCode(reason string) string {
	if i := strings.IndexByte(reason, ':'); i >= 0 {
		reason = reason[:i]
	}
	return strings.TrimSpace(reason)
}

// catalog declares every series. It is the allow-list: a name absent from it
// records nothing.
var catalog = map[string]spec{
	FamilyInventorySeconds: {kind: kindDuration, labels: []labelSpec{
		{name: LabelOutcome, values: []string{OutcomeOK, OutcomeError}},
	}},
	FamilyDiscoveryLagSeconds: {kind: kindDuration},
	Families:                  {kind: kindGauge},

	CheckoutTransitionTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelFrom, values: checkoutStates},
		{name: LabelTo, values: checkoutStates},
		{name: LabelEvidence, values: []string{
			EvidencePresent, EvidenceInaccessible,
			EvidenceAuthoritativeOmission, EvidencePrunableConfirmed, EvidenceNone,
		}},
	}},
	AvailabilityClocks: {kind: kindGauge},
	RemovalClocks:      {kind: kindGauge},

	CoordinatorCycleTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelOutcome, values: []string{
			OutcomeBuiltCommit, OutcomeAdoptedCommit, OutcomeBuiltDirty,
			OutcomeSkipped, OutcomeSuperseded, OutcomeRescheduled,
			OutcomeCASLost, OutcomeHeadMoved, OutcomeFailed, OutcomeDeferred,
		}},
	}},
	CoordinatorBuildSeconds: {kind: kindDuration, labels: []labelSpec{
		{name: LabelSlot, values: []string{SlotCommit, SlotDirty}},
	}},
	ViewBuildAdmissionTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelPriority, values: []string{BuildPriorityInteractive, BuildPriorityBackground}},
		{name: LabelOutcome, values: []string{
			BuildAdmissionAdmitted, BuildAdmissionRejected, BuildAdmissionCanceled,
		}},
	}},
	ViewBuildQueue: {kind: kindGauge, labels: []labelSpec{
		{name: LabelPriority, values: []string{BuildPriorityInteractive, BuildPriorityBackground}},
	}},
	ViewBuildWaitSeconds: {kind: kindDuration, labels: []labelSpec{
		{name: LabelPriority, values: []string{BuildPriorityInteractive, BuildPriorityBackground}},
	}},
	Coordinators: {kind: kindGauge},

	GenerationPublishedTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelOwner, values: []string{OwnerCheckout, OwnerRefView}},
	}},
	GenerationSupersededTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelOwner, values: []string{OwnerCheckout, OwnerRefView}},
	}},
	GenerationRetiredTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelOwner, values: []string{OwnerCheckout, OwnerRefView}},
	}},
	GenerationRetireRefusedTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelReason, values: []string{
			RefusedRouted, RefusedBased, RefusedLeased, RefusedMissing, RefusedError,
		}},
	}},
	GenerationSweepCollectedTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelSweep, values: []string{SweepCheckout, SweepRefView}},
	}},

	DedicatedBasePublishTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelShape, values: []string{DedicatedBaseRoot, DedicatedBaseDelta}},
	}},
	DedicatedBaseClaimTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelOutcome, values: []string{
			DedicatedBaseBuilt, DedicatedBaseCoalesced, DedicatedBaseReused,
		}},
	}},
	DedicatedBaseClosureTruncatedTotal: {kind: kindCounter},
	DedicatedBasePublicationTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelOutcome, values: []string{
			PublicationPublished, PublicationCoalesced, PublicationReadopted,
			PublicationSkipped, PublicationFailed,
		}},
	}},
	DedicatedBaseAdvanceTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelOutcome, values: []string{
			AdvanceDispatched, AdvanceRepeat, AdvanceRefused,
		}},
	}},
	DedicatedBaseDrainTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelOutcome, values: []string{DrainImmediate, DrainWaited}},
	}},
	DependentRecompositionTotal: {kind: kindCounter},

	RefViewSelectionTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelOutcome, values: []string{
			RefViewReady, RefViewAdopted, RefViewBuilding, RefViewDeferred,
			RefViewCoalesced, RefViewReclaimed, RefViewFailed,
		}},
	}},
	RefViewEvictedTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelReason, values: []string{EvictedStale, EvictedOverCount, EvictedOverBytes}},
	}},

	MaterializationTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelKind, values: []string{ViewWorktree, ViewRef}},
		{name: LabelOutcome, values: []string{OutcomeOK, OutcomeError}},
	}},
	LeasesHeld: {kind: kindGauge},
	HandoffTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelConsumer, values: handoffConsumers},
		{name: LabelOutcome, values: []string{HandoffJoined, HandoffRefused}},
	}},
	HandoffsOutstanding: {kind: kindGauge, labels: []labelSpec{
		{name: LabelConsumer, values: handoffConsumers},
	}},

	RequestServedTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelKind, values: []string{ViewBase, ViewWorktree, ViewRef}},
	}},
	RequestFallbackTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelReason, values: ViewErrorCodes},
	}},
	CapabilityRefusedTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelCode, values: []string{
			"capability_unavailable", "required_capability_incomplete",
		}},
	}},

	SearchQueryTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelCorpus, values: []string{CorpusSymbol, CorpusContent}},
	}},
	SearchSourceTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelCorpus, values: []string{CorpusSymbol, CorpusContent}},
	}},

	CheckoutSearcherBuiltTotal:       {kind: kindCounter},
	CheckoutSearcherInvalidatedTotal: {kind: kindCounter},

	LSPWorkspaceTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelEvent, values: []string{
			WorkspaceAcquired, WorkspaceReused, WorkspaceEvicted, WorkspaceStarved,
		}},
	}},

	ProbeAnswerTotal: {kind: kindCounter, labels: []labelSpec{
		{name: LabelKind, values: []string{ViewBase, ViewWorktree, ViewUnrouted}},
		{name: LabelExact, values: []string{AnswerExact, AnswerFallback}},
	}},
}
