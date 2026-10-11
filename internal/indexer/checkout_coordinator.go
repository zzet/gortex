package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search/trigram"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The per-checkout coordinator.
//
// One instance runs per automatic checkout — a worktree of a family whose
// primary checkout owns the base corpus. It keeps that checkout's two routed
// generations in step with what is on its disk: the commit layer, which turns
// the corpus at the primary's tree into the corpus at the checkout's HEAD tree,
// and the dirty layer, which turns that into what the working tree holds.
//
// The loop is signal -> quiet window -> sample -> reconcile, and every part of
// it exists for a reason a simpler shape does not cover:
//
//   - The quiet window coalesces a burst. A branch switch is thousands of
//     filesystem events and one state change; building per event would build
//     thousands of generations of which one describes the checkout.
//
//   - The sample is taken after the window closes, not when the signal
//     arrived. What the coordinator has to reconcile to is the state the
//     checkout is in now; a signal is only the claim that it moved.
//
//   - Each slot is reconciled against the identity of what it is routed to,
//     not against a memory of what was last built. A commit layer's identity is
//     the tree it targets over the base it sits on, so a route that already
//     names a generation with that identity needs no work at all, and a
//     generation built earlier for the same identity can be re-routed without
//     rebuilding. That is the branch-switch cache: A -> B -> A re-routes A's
//     generation instead of re-indexing A's tree.
//
//   - Every flip is a compare-and-set on the route's epoch. Losing it means
//     another actor moved the route; the coordinator supersedes what it built
//     and reschedules rather than forcing its own answer over the winner's.
const (
	// defaultCheckoutQuietWindow is the debounce a coordinator takes when its
	// configuration names none. It matches the watcher's own debounce band:
	// long enough to swallow an editor's save burst and a branch switch's
	// event storm, short enough that a view is current before the next query.
	defaultCheckoutQuietWindow = 300 * time.Millisecond

	// defaultCheckoutPollInterval is how often a coordinator signals itself.
	// It is the only signal source an automatic checkout has today (see
	// CheckoutCoordinatorConfig.PollInterval), so it bounds how stale a
	// worktree's view can be.
	defaultCheckoutPollInterval = 15 * time.Second

	// defaultRetainedCommitLayers bounds how many commit generations one
	// coordinator keeps for re-routing. Each retained generation costs the
	// storage of one branch's difference from the base; four covers the
	// branches an agent switches between in a session without turning the
	// cache into an unbounded ledger of every branch ever visited.
	defaultRetainedCommitLayers = 4

	// maxStoredCommitLayerCandidates bounds the catalog's equal-identity lookup
	// for a commit layer, the way maxDedicatedBaseReuseCandidates bounds the
	// committed base's. The lookup is keyed on the whole build identity, so a
	// servable row is at the head of the list or there is none; the bound is
	// what keeps a pathological store from turning one cache miss into an
	// unbounded read.
	maxStoredCommitLayerCandidates = 16
	// maxSharedCommitLayerCandidates bounds the scan of a graph's layer
	// generations for another checkout's commit layer (sharedCommit).
	maxSharedCommitLayerCandidates = 256

	// checkoutLayerOwnerKind names who owns the generations a coordinator
	// builds: the family's primary dedicated graph, whose corpus they compose
	// over and whose repo prefix their payload is stamped with.
	checkoutLayerOwnerKind = "dedicated_graph"

	// maxCohortBuildDeferrals bounds how many consecutive cycles may be
	// deferred because the resolver-visible input cohort could not be
	// described.
	//
	// Deferring is the right first answer to a transient — a repository being
	// untracked, a raw repository mid-mutation, a sibling that is tracked but
	// not yet indexed — because a build that goes ahead stamps a layer with a
	// revision that is not a certificate, and a checkout whose view is a few
	// seconds behind is a better outcome than a layer nobody can reuse. It is
	// not the right LAST answer: a transient that does not clear would leave
	// the checkout's view frozen forever. After this many deferrals the build
	// proceeds under a stable degraded revision and says so.
	maxCohortBuildDeferrals = 3

	// checkoutConfigDigestDomain versions the configuration digest a
	// generation's config_hash carries.
	//
	// The digest used to cover config.IndexConfig alone (indexConfigHash).
	// That is not the whole of the configuration a payload is a function of:
	// the artifacts list decides which non-code files become nodes, the
	// semantic and LSP settings decide which enrichment runs, and the
	// workspace/project slugs decide the namespace every node is stamped
	// with. A generation built under one of those and reused under another is
	// a payload composed from rules nobody asked for, so they are digested
	// here alongside the index configuration and the frozen snapshot's own
	// repo/workspace/project envelope.
	//
	// The domain string is versioned because widening the digest re-keys every
	// stored generation exactly once: nothing built by an older binary matches,
	// every checkout layer rebuilds on first contact, and from then on the
	// cache is keyed by the complete configuration.
	checkoutConfigDigestDomain = "gortex.checkout.config.v2"

	// dependencyRevisionSourceBudget bounds how long naming one roster
	// member's source identity may take.
	//
	// A raw repository's source witness sits behind the same gate a source
	// mutation takes exclusively, so reading it can wait on a writer. A
	// coordinator cycle must not park there: the budget turns an unavailable
	// witness into a refused cohort — which fails closed onto a non-reusable
	// revision — instead of into a stalled checkout.
	dependencyRevisionSourceBudget = 2 * time.Second

	// goPackageOwnershipTargetEvidence names the ownership evidence a checkout
	// layer's resolver consults.
	//
	// It is one source, measured rather than assumed: Indexer.prepareGoPackageOwnership
	// (go_package_ownership_sources.go:31) builds a lookup only for
	// `prefix == idx.repoPrefix`, and a sparse generation declares
	// graph.resolution.cross_repo incomplete for exactly that reason
	// (builder_generation.go, the CapResolutionCrossRepo row). What the
	// evidence CONTAINS is a function of the target repository's own bytes,
	// which the roster already names by source identity and which the identity
	// names again as its tree; what this fact pins is WHICH repositories may
	// contribute ownership at all, so a build that gains a second ownership
	// source moves the revision.
	goPackageOwnershipTargetEvidence = "go-package-ownership:target-repository-source"
)

// initialCheckoutPollDelay assigns one stable point in the poll interval to a
// checkout. The interval remains the hard staleness bound, but coordinators
// created by one reconciliation no longer all wake and fork git at once.
func initialCheckoutPollDelay(checkoutID string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte("gortex.checkout.poll.v1\x00" + checkoutID))
	var slot uint64
	for _, b := range sum[:8] {
		slot = slot<<8 | uint64(b)
	}
	return time.Duration(slot%uint64(interval)) + 1
}

// errRouteMoved reports that a route flip lost its compare-and-set: another
// actor repointed the route between the epoch this cycle read and the flip it
// attempted. The cycle stops and reschedules; it never re-reads the epoch and
// flips again, because that would be forcing its answer over the winner's.
var errRouteMoved = errors.New("indexer: the checkout route moved under this coordinator")

// errCheckoutUnsettled reports that two working-tree builds in a row were torn
// by edits landing under them. A cycle answers it by rescheduling; a caller
// driving a transition has to decide whether to wait for the checkout to go
// quiet or to give up.
var errCheckoutUnsettled = errors.New("indexer: the working tree moved under two builds")

// errBaseMoved reports that the primary's committed base advanced between the
// read that started this cycle and the flip that would have routed its result.
//
// It is the base half of the guard reconcileDirtySlot already makes for the
// checkout's own HEAD. A cycle reads the base once and builds a layer whose
// identity names it; routing that layer after the base has moved publishes a
// delta against a base the family has left, which is exactly the splice gate 5
// forbids. The cycle stops instead, leaving the route on the pair it already
// serves, and reschedules — the next one composes over the base that is current.
var errBaseMoved = errors.New("indexer: the committed base advanced under this coordinator")

// CheckoutCoordinatorConfig is what one coordinator needs to serve one
// automatic checkout.
type CheckoutCoordinatorConfig struct {
	// CheckoutID is the catalog identity of the checkout, and the key its
	// route row is stored under.
	CheckoutID string
	// GitWork serialises the start-time racily clean index refresh behind the
	// lifecycle's own git work (checkout_racy_index.go); nil runs it
	// unserialised.
	GitWork *checkoutGitWork
	// PrewarmRoute, when set, is called with the generations a route flip is
	// about to name (commit generation first, then the working-tree layer),
	// before the flip: the view reader loads their layer masks then, so the
	// first request on the new route does not (RoutePrewarmer).
	PrewarmRoute RoutePrewarmer
	// CheckoutRoot is the working tree the coordinator samples and builds from.
	CheckoutRoot string
	// FamilyID is the family whose primary dedicated graph the checkout's
	// layers sit on. The primary is re-read every cycle rather than captured,
	// so a primary that moves — a new commit on the primary checkout, a
	// promotion — is picked up on the next pass.
	FamilyID string
	// HeadCommit and HeadTree seed the root-known dirty sampler from the last
	// reconciled checkout row. Porcelain must report HeadCommit before HeadTree
	// can be reused, so stale catalog state can only cause one extra resolution.
	HeadCommit string
	HeadTree   string

	// RepoPrefix, WorkspaceID and ProjectID are stamped onto the payload. They
	// are the PRIMARY's, not the checkout's: the layers compose over the
	// primary's corpus, so their nodes have to live in the same namespace.
	RepoPrefix  string
	WorkspaceID string
	ProjectID   string

	// Store is any handle on the database. Generations are begun, published,
	// routed and retired through it.
	Store *store_sqlite.Store
	// Builder builds the sparse generations. It must carry the index
	// configuration the base corpus was indexed with.
	Builder *SparseGenerationBuilder
	// Leases is the lease manager the materializer pins routed generations
	// with. Retirement consults it, so a generation under a live view is
	// refused rather than swept. nil means nothing leases generations.
	Leases *graphview.LeaseManager
	// Config is the index configuration the generations are built under. Its
	// digest is part of every generation's identity, so a configuration change
	// invalidates the cache instead of composing two payloads built under
	// different rules.
	Config config.IndexConfig
	// ConfigSections carries the configuration domains that are output
	// affecting but live outside config.IndexConfig — artifacts, semantic,
	// LSP, workspace, project — as the name/digest pairs the dependency
	// revision and the widened config digest both consume. The lifecycle
	// computes them from the same repo configuration it freezes the index
	// half of (see dedicatedBaseConfigSections).
	//
	// A coordinator handed none can still build: its config digest simply
	// covers the index configuration alone, and its dependency cohort is
	// refused — which fails closed onto a non-reusable revision rather than
	// onto a certificate over configuration nobody enumerated.
	ConfigSections []DependencyRevisionConfigSection

	// WorkspaceMembers reports which repositories share this checkout's
	// workspace, and therefore which of them may appear in its dependency
	// cohort at all.
	//
	// nil takes the production default: the repository topology the builder's
	// own Indexer belongs to (MultiIndexer.ReposInWorkspace, the same authority
	// the request surface scopes a workspace session with). A coordinator whose
	// builder has no such topology — a focused fixture, a transition built by
	// hand — scopes its cohort to the target repository alone and declares that
	// scope, rather than silently digesting repositories the target's
	// resolution can never consult.
	WorkspaceMembers func() map[string]bool

	Logger *zap.Logger
	// Gate holds the loop's build cycles while the daemon warms up. nil admits
	// every cycle at once, which is what a coordinator outside a warmup has.
	Gate *ViewBuildGate

	// RequestBase asks the committed-base publisher for the primary's base.
	//
	// It is called with the PRIMARY's repo prefix from primaryBase's
	// unpublished arm, and it is how the committed-base consumer gate stays
	// deferral rather than refusal: a daemon start and a HEAD movement both
	// decline to publish a base for a family with no reader, so the first
	// reader asks. The lifecycle supplies it (buildCoordinator); nil means
	// nothing is asked and the checkout stays in the legacy regime, which is a
	// correct place for it to be.
	RequestBase func(repoPrefix string)

	// Debounce is the quiet window; <= 0 takes defaultCheckoutQuietWindow.
	Debounce time.Duration
	// PollInterval is how often the coordinator signals itself; < 0 disables
	// the self-signal and 0 takes defaultCheckoutPollInterval. Missing roots are
	// probed with exponential backoff up to five minutes (or PollInterval if
	// longer); lifecycle reconciliation owns retirement.
	PollInterval time.Duration
	// Retain bounds the commit generations kept for re-routing; <= 0 takes
	// defaultRetainedCommitLayers.
	Retain int

	// cycleDone is a test seam: it runs at the end of every reconcile cycle
	// with what that cycle did. nil in production.
	cycleDone func(CheckoutCycle)
	// debounceDemand is a test seam: demand wakes (SignalDemand) take the
	// quiet window like any other signal, so a fixture that parks the loop
	// behind a long window keeps driving every cycle by hand. false in
	// production.
	debounceDemand bool
	// dirtyBarrier is a test seam handed to the dirty build: it runs inside
	// the window between a payload being complete and the checkout being
	// re-sampled, which is the window the supersede rule exists to close.
	// nil in production.
	dirtyBarrier func()
	// snapshotConfig is snapshotDedicatedBaseConfig behind a seam one test can
	// substitute; nil takes the real function.
	//
	// The constructor REFUSES a configuration it cannot freeze, because the
	// alternative hands the builder the ConfigManager's own nested values
	// while stamping an identity that names the configuration the build
	// started with. No config.IndexConfig value json.Marshal rejects exists
	// today, so without this seam that refusal would be unreachable and
	// unpinned — and the next lane to add a field that can fail to encode
	// would find the guard untested. It is a per-construction field rather
	// than a package-level var so two tests substituting it cannot race.
	snapshotConfig func(config.IndexConfig, string, string, string) (config.IndexConfig, string, error)
}

// CheckoutCycle is what one reconcile pass did. Every field is a decision, not
// a metric: a test asserting that a branch switch back re-routed instead of
// rebuilding reads CommitReused, and one asserting that a torn build left the
// route alone reads Rescheduled.
type CheckoutCycle struct {
	// CommitGenerationID and DirtyGenerationID are what the checkout is routed
	// to when the cycle ends, 0 for a slot the cycle could not settle.
	CommitGenerationID int64
	DirtyGenerationID  int64
	// CommitBuilt / DirtyBuilt report that the cycle ran a build for that slot.
	CommitBuilt bool
	DirtyBuilt  bool
	// CommitReused reports that the commit slot was pointed at a generation
	// built by an earlier cycle — the branch-switch cache hit.
	CommitReused bool
	// DirtyReused reports that the working-tree slot was pointed at a
	// generation built by an earlier cycle — undo/redo, and a working tree
	// that came back to a state this coordinator has already indexed over the
	// same commit layer.
	DirtyReused bool
	// Recomposed reports that the cycle rebuilt BOTH layers over a committed
	// base that advanced under a checkout whose own tree did not move, and
	// installed them in one route write. It is the difference between "the
	// base moved, so the checkout's view was torn down and put back up" and
	// "the base moved, so the checkout was recomposed over it" — the old pair
	// keeps serving for the whole of the rebuild.
	Recomposed bool
	// DirtyReparented reports that a recomposition carried the working-tree
	// chain over to the new commit layer by copying its rows (no parse, no
	// pass) instead of rebuilding it: the layers below compose to the same
	// committed tree over either base, so the chain's payload is still exact
	// (checkout_propagation.go). DirtyBuilt is false when it is set.
	DirtyReparented bool
	// ImportFolded reports that a file-by-file import cycle folded the chain
	// it extended into one generation by copy (checkout_import.go); the
	// route names the fold and DirtyChainDepth is 1.
	ImportFolded bool
	// RebaseDeferred reports that the primary's base advanced under a
	// checkout nobody is using and the cycle left the routed stack exactly as
	// it was: the rebase is applied when the checkout is next selected,
	// edited or asked to release its base (checkout_propagation.go).
	RebaseDeferred bool
	// Propagation is the per-path scope of a base advance for this checkout,
	// by content, when the cycle computed one.
	Propagation *PropagationScope
	// BasePinned reports that the cycle served the checkout from the committed
	// base generation its routed layers were BUILT against, while the family's
	// primary has already advanced past it. It is the dependent pin's saving
	// made observable: the pair is still exactly this checkout's tree (the
	// materializer composes the ancestry the routed generation itself names),
	// so the cycle built nothing and wrote nothing.
	//
	// It is not an outcome of its own in the metric vocabulary. A pinned cycle
	// that also had nothing else to do counts as skipped, which is what it is:
	// the checkout is already serving the right answer.
	BasePinned bool
	// Rescheduled reports that the cycle stopped short and signalled itself:
	// a lost route flip, or a working tree that moved under two builds in a
	// row. The route is left exactly as the cycle found it.
	Rescheduled bool
	// YieldedTo names what a background cycle gave the build lane up to
	// ("interactive_build"), or why it abandoned its build
	// ("working_tree_moved": the checkout's working tree moved under it,
	// checkout_motion.go): it was canceled before its commit point, nothing
	// it began was published, and it is Rescheduled. Empty otherwise.
	YieldedTo string
	// Held reports a background cycle kept off the build lane because the
	// working tree was still changing (checkout_motion.go): it built nothing,
	// it is Rescheduled, and the quiet window runs the next one.
	Held bool
	// rescheduledBy names what stopped a Rescheduled cycle that published
	// nothing, for the publication-stall run (checkout_publication_stall.go):
	// torn_by_motion (the working tree moved under two builds), route_moved
	// (a lost route flip), base_moved or head_moved. Empty for every other
	// cycle, including one batch of a large working tree, which is routed.
	rescheduledBy string
	// Deferred reports that the cycle never ran. Three causes share the field:
	// daemon warmup has not opened the build lane, its bounded background queue
	// was saturated, or the resolver-visible input cohort could not be
	// described and the coordinator would rather wait than stamp a layer nobody
	// validated the inputs of. Opening the gate, an invalidation event or the
	// 15-second coordinator poll retries the demand. Nothing was read or
	// written, so every other field is zero.
	Deferred bool
	// DirtyParentCandidate and DirtyChainReason report, for a cycle that
	// reached a working-tree build, which generation a chained build could
	// stand on (selectDirtyParent) and, when none, the fallback reason code.
	DirtyParentCandidate int64
	DirtyChainReason     string
	// DirtyBatchRemaining is how many changed paths the cycle's working-tree
	// build left for the next batch: the routed layer is a step towards the
	// working tree, not the working tree, so the cycle completes no refresh
	// ticket and the next batch is scheduled at once (0: whole sample).
	DirtyBatchRemaining int
	// DirtyOutpaced reports that the routed working-tree generation describes
	// the sample its build took, and the prepublish fence saw the working copy
	// already past it (a parsed file stamped past the sample and confirmed by
	// its parsed bytes, or a full sample that differs): a publication, and the
	// next cycle is signalled at once for the newer state. A move only in files
	// the build never read is not seen by the fence and leaves it false. A
	// refresh ticket admitted after that sample stays waiting for it.
	DirtyOutpaced bool
	// dirtySample is the sample the cycle's own working-tree build described,
	// held from the build: the refresh tickets its publication answers are
	// completed against it when a sample of another state has since become
	// the latest (completeCheckoutRefreshTickets). Zero when the cycle built
	// nothing.
	dirtySample gitstate.DirtySnapshot
	// DirtyParentGenerationID is the physical parent the cycle's working-tree
	// build actually stood on when it was built over a working-tree parent,
	// and DirtyChainDepth the published generation's chain depth (1 = built
	// direct over the commit generation). Both are 0 when nothing was built.
	// A chained attempt the builder refused carries its reason in
	// DirtyChainReason and was rebuilt direct.
	DirtyParentGenerationID int64
	DirtyChainDepth         int
	// DirtyParentPreferred reports that the working-tree build stood on the
	// coordinator's preferred parent — a compacted generation the route did
	// not name yet, or the generation an edit lease withdrew from the route —
	// rather than on the routed top.
	DirtyParentPreferred bool
	// CompactionScheduled reports that the cycle's build reached the soft
	// chain depth and a background compaction was owed for it
	// (dirty_chain_compaction.go). The compaction itself runs after the cycle,
	// off the cycle lock, and reports separately.
	CompactionScheduled bool
	// DirtyWork is the physical work of the cycle's working-tree build, nil
	// when the cycle built none.
	DirtyWork *GenerationWorkCounters
	// Admission is how long the cycle waited before it could build, by
	// stage, and what held the build lane when it queued for it. Zero for a
	// cycle that never queued (a settled one) or was driven by hand.
	Admission cycleAdmission
	// cycleStarted is when the cycle began: its high-water tickets arrived
	// before it. Later riders must also pass their own post-arrival sample
	// fence at completion. Zero outside a loop cycle.
	cycleStarted time.Time
	// Err is what stopped the cycle, nil when it settled both slots.
	Err error
	// PlanLaps are the reconcile's reads before the dirty slot (cohort,
	// primary base, head sample, route, base recomposition, commit slot),
	// logged with the dirty slot's plan record.
	PlanLaps []CycleLap
}

// CheckoutCoordinator keeps one automatic checkout's routed view in step with
// its disk. It is created by the checkout lifecycle when a family's
// reconciliation reports an accessible automatic checkout, and closed when
// that checkout leaves.
type CheckoutCoordinator struct {
	// racyHeal tracks the start-time index refresh (checkout_racy_index.go).
	racyHeal racyHealState
	// prewarmCommitGeneration is the commit generation whose stack was last
	// pre-warmed (RewarmEditDeltaStacks warms it again when its key moved).
	prewarmCommitGeneration atomic.Int64
	// prewarmState is the last pre-warm decision (edit_delta_stack_prewarm.go).
	prewarmState prewarmState

	checkoutID  string
	root        string
	sampler     *gitstate.DirtySampler
	familyID    string
	repoPrefix  string
	workspaceID string
	projectID   string

	store   *store_sqlite.Store
	catalog *store_sqlite.Catalog
	builder *SparseGenerationBuilder
	leases  *graphview.LeaseManager
	logger  *zap.Logger
	gate    *ViewBuildGate

	// baseViewsOnce and baseViews are the one materializer every build of
	// this checkout opens its layer below through: it keeps each published
	// generation's masks between builds (graphview's per-generation layer
	// cache), so an edit's plan opens only the levels it has not seen
	// instead of re-reading the identity masks of the whole chain.
	baseViewsOnce sync.Once
	baseViews     *graphview.Materializer
	// announceWrite replaces the store's AnnounceWrite for a waiting refresh
	// ticket (a test seam; nil uses the store).
	announceWrite func() func()

	// requestBase is CheckoutCoordinatorConfig.RequestBase; nil asks nothing.
	requestBase func(string)
	// baseDemandMu guards the throttle below. It is its own lock because
	// primaryBase is reached from the poll's no-op path, from a build and from
	// the moved-base guard, and none of those holds the cycle lock in common.
	baseDemandMu sync.Mutex
	// lastBaseDemand is when this coordinator last asked for the family's
	// committed base. The ask is throttled rather than made once: a
	// publication that FAILS must be re-enterable — the catalog's failed-claim
	// recovery is built for exactly that — while a 15 s poll over a family
	// whose publication keeps failing must not re-enter it every 15 s.
	lastBaseDemand time.Time

	quiet           time.Duration
	poll            time.Duration
	retain          int
	configHash      string
	extractors      string
	resolverVersion string

	// config is the FROZEN index configuration this coordinator's generations
	// are built under — the deep snapshot, not the ConfigManager's shallow
	// result, so a later edit to the live configuration cannot change what an
	// already-running coordinator says it built under.
	config         config.IndexConfig
	configSections []DependencyRevisionConfigSection

	// cohort describes the resolver-visible input set this coordinator's
	// layers are built under. It is a value rather than a set of fields
	// because a ref view's producer assembles the same one.
	cohort dependencyCohortSource

	// cohortCost counts what describing the cohort has cost: one roster lease
	// and one source read per in-scope member, each time it is described. The
	// counters are the evidence that an idle poll describes nothing.
	cohortCost dependencyCohortCounters

	// revisionMu guards the cached cohort the identities carry. It is its own
	// lock because commitIdentity is called both under mu (retainCommit's key)
	// and outside it.
	//
	// The cohort is CACHED rather than re-described per cycle. Describing it
	// takes a daemon-wide roster read lease and one catalog read per in-scope
	// roster member; doing that on every 15-second poll of every checkout is
	// K×N reads per interval for an answer that changes only when the topology
	// does. The cache is refreshed on events instead: construction, a build
	// (reconcile and RehomeTo both describe it afresh, so what a layer is
	// stamped with is always freshly validated), and InvalidateDependencyCohort
	// for everything the coordinator cannot see for itself.
	revisionMu sync.RWMutex
	revision   string
	// degraded is the stable revision the identities carry while revision is
	// empty. It is computed once per description rather than per identity read:
	// commitIdentity is called several times a cycle, and the degraded value
	// digests the whole frozen configuration.
	degraded string
	// revisionReason is the stable refusal reason when the cohort could not be
	// described; empty when revision is a real certificate.
	revisionReason string
	// cohortStale marks the cached cohort as needing a fresh description at
	// the next opportunity that is allowed to take one.
	cohortStale bool
	// cohortTopology is the workspace topology the cached cohort was described
	// under (dependencyCohortSource.topologyToken). The poll re-reads the token
	// — a map walk, no lease and no catalog read — and re-describes when it
	// moves, so a repository tracked into or out of this checkout's workspace
	// un-settles the poll without anything having to tell the coordinator.
	cohortTopology string
	// cohortDeferrals counts consecutive cycles deferred for want of a
	// describable cohort, and is reset by the first cycle that describes one.
	cohortDeferrals int
	// cohortEverDescribed records that this coordinator has described its
	// cohort at least once. Deferring is a response to a TRANSIENT — something
	// that was describable and stopped being so — and a coordinator that has
	// never described one has no route to keep serving in the meantime, so it
	// builds under the degraded revision immediately rather than leaving the
	// checkout with no view at all for several poll intervals.
	cohortEverDescribed bool

	// signal carries a wake to the run loop. It is buffered to one: a burst of
	// signals has exactly one thing to say, and the loop re-arms the quiet
	// window once per wake it reads.
	signal chan struct{}
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	// lifetime is canceled before Close waits. Every loop-owned admission and
	// build derives from it, so removing a checkout cannot sit behind an
	// unrelated generation for minutes.
	lifetime       context.Context
	cancelLifetime context.CancelFunc

	// cycleMu serializes the loop's own cycles against a caller-driven
	// transition. Both move this checkout's route, and a transition that
	// builds off-route only keeps its promise if the loop cannot flip the
	// route to a half-built state while it is building.
	cycleMu sync.Mutex

	mu sync.Mutex
	// External source mutations and off-route RehomeTo builds are admitted
	// under mu and joined by CloseContext before lifecycle teardown retires
	// the checkout. The loop itself is joined separately through done.
	sourceMutationsClosing bool
	sourceMutations        int
	sourceMutationsDrained chan struct{}
	// Refresh tickets observe this coordinator's existing loop; they never
	// create a competing builder or retain request contexts.
	refreshMu        sync.Mutex
	refreshWaiters   map[uint64]*checkoutRefreshRequest
	refreshHighWater uint64
	refreshReserved  int
	refreshClosed    bool
	// retained is the commit-layer reuse cache, most recently routed first.
	retained []retainedCommitLayer
	// retainedDirty is the working-tree-layer reuse cache, most recently
	// routed first. It is what makes undo/redo cost nothing: the layer the
	// route leaves is kept instead of retired, and a working tree that comes
	// back to a state this coordinator has already indexed — over the same
	// commit layer, under the same configuration and cohort — is re-routed
	// rather than re-indexed.
	retainedDirty []retainedDirtyLayer
	// backlog holds generations a retire refused. The janitor retries them.
	backlog map[int64]struct{}
	// publication is the run of cycles that published nothing
	// (checkout_publication_stall.go).
	publication checkoutPublicationStallRun
	// basePinned is the committed base generation this checkout's ROUTE is
	// composed over while the family's primary has moved past it — the
	// dependent pin, as the last cycle resolved it, and 0 when the route is on
	// the family's current base or the family publishes no generation at all.
	//
	// It exists so the retirement sweep can ask a live coordinator what its
	// route is holding without re-deriving it from the catalog per candidate
	// base. It is a cache of a catalog fact, and both ways of being wrong are
	// safe: reporting 0 while pinning offers a base the catalog's own
	// reference guard then refuses (the route's layer names it as its base),
	// and reporting a stale id keeps a base one sweep longer and asks a
	// coordinator to release something it is no longer holding, which is a
	// no-op. What it must never do is authorize a delete, and it cannot: the
	// sweep only ever uses it to RETAIN.
	basePinned int64
	// basePinRelease is the one pinned base a sweep has asked this coordinator
	// to stop holding, so the generation can finally retire. The next cycle
	// refuses to pin it and recomposes over the family's current base instead,
	// installing the replacement stack in one compare-and-set before the old
	// one is given up. Requesting the same generation twice is a no-op, so an
	// hourly sweep does not re-signal a recomposition that is already owed.
	basePinRelease int64
	// routedDirty is the working-tree generation the route names. The reuse
	// cache already remembers the commit half; this is the other one, and
	// together they are the only record of a checkout's payload once its route
	// row has been withdrawn — see DrainRetirements.
	routedDirty int64
	// reason is the last signal's reason, carried into the cycle's logging so
	// a build can be traced back to what asked for it.
	reason string
	// dirtyFingerprint is the working tree the last cycle sampled. It is what
	// the checkout's text searcher is keyed by — see checkout_text_search.go.
	dirtyFingerprint string

	// textMu guards the checkout's own trigram searcher and is held across the
	// build, so concurrent searches on one checkout pay for one index.
	textMu    sync.Mutex
	textIndex *trigram.Searcher
	textKey   string
	textState checkoutTextState

	cycleDone func(CheckoutCycle)
	// prewarm loads the view reader's masks for a stack a flip will route
	// (CheckoutCoordinatorConfig.PrewarmRoute); nil when none is installed.
	prewarm      RoutePrewarmer
	dirtyBarrier func()

	// cyclePreflight and cycleBarrier are focused test seams. Production uses
	// settledWithoutBuild and has no barrier.
	cyclePreflight func(context.Context) (CheckoutCycle, bool)
	cycleBarrier   func(context.Context)
	// importPreambleBarrier delays the read-only pin/recomposition decisions
	// in progress tests; production has no barrier.
	importPreambleBarrier func(context.Context)
	// importAdmissionBarrier observes a grant before its initial yield arm (tests only).
	importAdmissionBarrier func(context.Context)
	// importFoldPlanningBarrier delays read-only post-publication fold planning
	// in admission tests; production has no barrier.
	importFoldPlanningBarrier func(context.Context)
	// importPublicationTailBarrier parks metadata cleanup after a retained route (tests only).
	importPublicationTailBarrier func(context.Context)
	// holdSample is a focused test seam for the working-copy sample a
	// background cycle takes before it queues (holdBackgroundCycle); nil
	// takes cycleSample.
	holdSample func(context.Context) error
	// refreshSample is a focused test seam for the working-copy samples refresh
	// tickets are captured and completed against (refreshSampleSince); nil
	// takes the sampler's.
	refreshSample func(context.Context, time.Time) (gitstate.DirtySnapshot, time.Time, error)
	// cycleSampleHook is a focused test seam for a cycle's shared working-copy
	// sample (cycleSample): an error it returns is the sample's; nil, or a
	// nil error, takes the sampler's.
	cycleSampleHook func(context.Context) error

	// laneYields is how many background cycles in a row gave the build lane
	// up to an interactive build (ViewBuildGate.NoteYieldable). A cycle that
	// runs to an outcome resets it; past maxViewBuildYields the gate arms
	// nothing and the background work runs to completion. Guarded by mu.
	laneYields int

	// selectionDemand promotes a queued build independently of the debounce
	// signal. mu guards initialization; the buffered channel coalesces demand.
	selectionDemand chan struct{}

	// demand wakes the loop for a cycle without a quiet window: a refresh
	// ticket (an MCP edit's committed content, or a request that needs a
	// fresh answer) is demand, not an event storm, and waiting for the
	// window proves nothing its cycle does not prove again. Buffered to one:
	// a burst coalesces into one cycle, and demand that arrives during a
	// cycle is answered by exactly one more.
	demand chan struct{}
	// ticketDemand is when this checkout's newest refresh ticket was
	// admitted (Unix nanoseconds, 0 before the first): the rank its cycles
	// carry on the shared build lane (ViewBuildGate.AcquireRanked), so the
	// checkout a caller is waiting on right now is built before another
	// checkout's cycle queued earlier.
	ticketDemand atomic.Int64
	// signaledAt is when Signal last claimed the checkout moved (under mu):
	// a cycle's shared sample must postdate it (cycleSample).
	signaledAt time.Time

	// propagation is the primary-to-worktree propagation state: a base
	// advance noted without a cycle, and whether a use asked for it to be
	// applied (checkout_propagation.go).
	propagation basePropagation

	// retireCalled is a test seam: it observes every generation offerRetire
	// retires inline. nil in production.
	retireCalled func(int64)

	// compaction is the background working-tree chain compaction's state
	// (dirty_chain_compaction.go).
	compaction dirtyChainCompactor

	// followup is the enrichment follow-up's schedule
	// (enrichment_followup.go).
	followup enrichmentFollowup

	// motion is what the checkout's file watcher reported, and the admission
	// state of background cycles over a moving working tree
	// (checkout_motion.go).
	motion checkoutMotion
}

func (c *CheckoutCoordinator) selectionRequests() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selectionDemand == nil {
		c.selectionDemand = make(chan struct{}, 1)
	}
	return c.selectionDemand
}

// PrioritizeSelection promotes queued work without cancelling an active build
// or resetting the quiet window. A repeated request contributes at most one
// buffered demand token; it does not schedule another reconcile cycle.
func (c *CheckoutCoordinator) PrioritizeSelection() {
	if c == nil {
		return
	}
	// A selection is a use: a base advance left pending for this checkout's
	// next use is applied now (checkout_propagation.go).
	c.wantRebase("selection", true)
	select {
	case c.selectionRequests() <- struct{}{}:
	default:
	}
}

// retainedCommitLayer is one commit generation kept for re-routing, keyed by
// the build identity that produced it.
type retainedCommitLayer struct {
	key          string
	generationID int64
}

// retainedDirtyLayer is one working-tree generation kept for re-routing, keyed
// by the build identity that produced it.
//
// It is a separate list from the commit one rather than a shared cache with a
// kind column: the two are evicted against each other only by accident, and a
// branch switch that fills the commit cache must not evict the working-tree
// layer the checkout is about to come back to. The bound is the same, and for
// the same reason — a retained layer costs the storage of one difference from
// the layer below it.
type retainedDirtyLayer struct {
	key          string
	generationID int64
}

// primaryBase is the family's primary dedicated graph and the state the base
// corpus underneath every layer is at.
type primaryBase struct {
	// graphID is the primary dedicated graph. It is what the route names and
	// what the materializer resolves the repo prefix from.
	graphID string
	// generationID is the primary's published generation, 0 when the corpus
	// itself is the base — which is what a plainly indexed primary checkout
	// looks like today.
	generationID int64
	// treeOID is the committed tree the base corpus holds. It is the left-hand
	// side of the commit layer's diff.
	treeOID string
	// pinned marks a base this coordinator is deliberately staying on rather
	// than the one the family's active pointer names right now — the committed
	// regime's dependent pin, minted by pinRoutedBase and by nothing else.
	//
	// It is a field rather than a caller's side note because the guards read
	// it: baseMovedUnderCycle's "is this still the base I planned against"
	// question has a different answer for a pinned base (is the generation
	// still servable and still the tree I named) than for the family's active
	// one (is the family still on it), and a value that did not say which it
	// was would have to be guessed at. graphBase never sets it, so every
	// unpinned path keeps the exact comparison it always had.
	pinned bool
}

// NewCheckoutCoordinator builds a coordinator and starts its loop. The caller
// owns it and must Close it; a coordinator that is not closed keeps a
// goroutine and a timer alive.
func NewCheckoutCoordinator(cfg CheckoutCoordinatorConfig) (*CheckoutCoordinator, error) {
	switch {
	case cfg.CheckoutID == "":
		return nil, errors.New("indexer: checkout coordinator needs a checkout id")
	case cfg.CheckoutRoot == "":
		return nil, errors.New("indexer: checkout coordinator needs a checkout root")
	case cfg.FamilyID == "":
		return nil, errors.New("indexer: checkout coordinator needs a family id")
	case cfg.Store == nil:
		return nil, errors.New("indexer: checkout coordinator needs a store")
	case cfg.Builder == nil:
		return nil, errors.New("indexer: checkout coordinator needs a generation builder")
	}
	// Freeze the concrete builder before freezing its producer identity. Live
	// admission/provider handles remain shared, but later caller installation
	// cannot retarget this owner's contract runtime.
	builder := *cfg.Builder
	cfg.Builder = &builder
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	sampler, err := gitstate.NewDirtySampler(cfg.CheckoutRoot, cfg.HeadCommit, cfg.HeadTree)
	if err != nil {
		return nil, fmt.Errorf("indexer: create checkout sampler: %w", err)
	}
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	// The coordinator owns its configuration outright. The lifecycle already
	// freezes it before handing it over; freezing again here is a no-op on a
	// frozen value and is what makes a coordinator constructed by any other
	// caller — a test, a transition — own its config too, rather than sharing
	// the ConfigManager's nested maps and slices with whoever else holds them.
	snapshot := cfg.snapshotConfig
	if snapshot == nil {
		snapshot = snapshotDedicatedBaseConfig
	}
	frozen, configFingerprint, snapshotErr := snapshot(
		cfg.Config, cfg.RepoPrefix, cfg.WorkspaceID, cfg.ProjectID)
	if snapshotErr != nil {
		// Fail CLOSED. The alternative — carry on with the caller's value and
		// a unique digest — keeps the identity honest but leaves the BUILDER
		// holding the ConfigManager's own nested maps and slices, which the
		// next configuration reload can change underneath a build that has
		// already stamped an identity naming the configuration it started
		// with. That is the exact mis-description this freeze exists to
		// prevent, so a configuration that cannot be frozen is a refusal to
		// construct rather than a warning.
		cancelLifetime()
		return nil, fmt.Errorf(
			"indexer: freeze the index configuration for checkout %s: %w", cfg.CheckoutID, snapshotErr)
	}
	c := &CheckoutCoordinator{
		checkoutID:      cfg.CheckoutID,
		root:            cfg.CheckoutRoot,
		sampler:         sampler,
		familyID:        cfg.FamilyID,
		repoPrefix:      cfg.RepoPrefix,
		workspaceID:     cfg.WorkspaceID,
		projectID:       cfg.ProjectID,
		store:           cfg.Store,
		catalog:         cfg.Store.Catalog(),
		builder:         cfg.Builder,
		leases:          cfg.Leases,
		logger:          logger,
		gate:            cfg.Gate,
		requestBase:     cfg.RequestBase,
		quiet:           cfg.Debounce,
		poll:            cfg.PollInterval,
		retain:          cfg.Retain,
		config:          frozen,
		configSections:  slices.Clone(cfg.ConfigSections),
		configHash:      checkoutConfigHash(configFingerprint, cfg.ConfigSections),
		extractors:      cfg.Builder.extractorVersionsFingerprint(),
		resolverVersion: resolverVersionFingerprint(),

		signal:         make(chan struct{}, 1),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		lifetime:       lifetime,
		cancelLifetime: cancelLifetime,
		backlog:        map[int64]struct{}{},
		cycleDone:      cfg.cycleDone,
		prewarm:        cfg.PrewarmRoute,
		dirtyBarrier:   cfg.dirtyBarrier,
	}
	if !cfg.debounceDemand {
		c.demand = make(chan struct{}, 1)
	}
	if c.quiet <= 0 {
		c.quiet = defaultCheckoutQuietWindow
	}
	if c.poll == 0 {
		c.poll = defaultCheckoutPollInterval
	}
	if c.retain <= 0 {
		c.retain = defaultRetainedCommitLayers
	}
	workspaceMembers := cfg.WorkspaceMembers
	if workspaceMembers == nil {
		workspaceMembers = builderWorkspaceMembers(cfg.Builder, cfg.WorkspaceID)
	}
	c.cohort = dependencyCohortSource{
		Target: DependencyRevisionTarget{
			RepoPrefix:  cfg.RepoPrefix,
			WorkspaceID: cfg.WorkspaceID,
			ProjectID:   cfg.ProjectID,
		},
		Leases:           cfg.Leases,
		Catalog:          c.catalog,
		WorkspaceMembers: workspaceMembers,
		Config:           frozen,
		ConfigSections:   c.configSections,
		// The ownership evidence is one measured source; see
		// goPackageOwnershipTargetEvidence for why that is a fact about this
		// build rather than an absence nobody looked for.
		Ownership: []DependencyRevisionOwnership{{
			RepoPrefix: cfg.RepoPrefix,
			Language:   "go",
			Owner:      goPackageOwnershipTargetEvidence,
		}},
		Producers:         cohortProducerPolicy(frozen, cfg.Builder != nil && cfg.Builder.Embedder != nil),
		Capabilities:      cohortCapabilityVocabulary(),
		ExtractorVersions: c.extractors,
		SourceBudget:      dependencyRevisionSourceBudget,
		Counters:          &c.cohortCost,
	}
	// Describe the cohort before the loop, and before any caller can ask for an
	// identity. Every read of the revision after this point sees a real cohort
	// or this coordinator's stable degraded revision; none of them sees an
	// empty revision, which is the one value the reuse guards read as "matches
	// anything".
	c.describeDependencyCohort(lifetime)
	c.racyHeal.begin(cfg.GitWork)
	go c.healRacyIndex(lifetime)
	go c.run()
	return c, nil
}

// Signal marks the checkout dirty. Any caller may signal, as often as it
// likes: the quiet window coalesces a burst into one reconcile cycle, and a
// signal that arrives while a cycle is running schedules the next one rather
// than being dropped.
func (c *CheckoutCoordinator) Signal(reason string) {
	c.signalWindow(reason, true)
}

// signalWindow wakes the loop through the quiet window. claim records the
// wake as a claim that the checkout moved (signaledAt), which a cycle's
// shared sample must postdate; a ticket's demand routed through the window
// (debounceDemand) claims nothing its ticket does not already bound.
func (c *CheckoutCoordinator) signalWindow(reason string, claim bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.reason = reason
	if claim {
		c.signaledAt = time.Now()
	}
	c.mu.Unlock()
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

// SignalDemand wakes the loop for a cycle now, without the quiet window. It is
// the wake for refresh tickets: their content is already bound (an MCP edit's
// committed hash) or they ask for a fresh answer, and a ticket completes only
// through the post-admission verification against a sample taken after it
// arrived, so the window would add delay and prove nothing. A burst coalesces
// into one cycle; demand during a running cycle is answered by exactly one
// more. Filesystem, poll and HEAD signals keep the debounced Signal.
func (c *CheckoutCoordinator) SignalDemand(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.reason = reason
	c.mu.Unlock()
	demand := c.demand
	if demand == nil {
		c.signalWindow(reason, false)
		return
	}
	select {
	case demand <- struct{}{}:
	default:
	}
}

// Close stops the loop and waits for cooperative cancellation to finish.
func (c *CheckoutCoordinator) Close() error {
	return c.CloseContext(context.Background())
}

// CloseContext cancels queued admission and in-flight cooperative work before
// waiting. The cancellation is permanent even when the caller's wait deadline
// expires; a later CloseContext may wait for the same shutdown to finish.
func (c *CheckoutCoordinator) CloseContext(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.once.Do(func() {
		c.mu.Lock()
		c.sourceMutationsClosing = true
		c.mu.Unlock()
		c.closeDirtyChainCompactor()
		c.cancelEnrichmentFollowup()
		if c.cancelLifetime != nil {
			c.cancelLifetime()
		}
		c.closeFilesystemWatch()
		if c.stop != nil {
			close(c.stop)
		}
	})
	select {
	case <-c.done:
		// The start-time Git index refresh has its own goroutine. Cancellation
		// alone does not prove its process and index lock have been released.
		if err := c.waitRacyIndexHeal(ctx); err != nil {
			return err
		}
		if err := c.waitDirtyChainCompactions(ctx); err != nil {
			return err
		}
		// A follow-up was canceled with the lifetime; its goroutine ends at
		// its next step.
		c.waitEnrichmentFollowups()
		return c.waitSourceMutations(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Running reports whether the loop goroutine is still there.
//
// It is what the administrative surfaces mean by a live coordinator, and it is
// not the same question as whether the lifecycle's registry holds one: a
// coordinator built for a transition runs a whole rebuild before anything
// registers it, and one being dropped keeps running until its in-flight cycle
// ends.
func (c *CheckoutCoordinator) Running() bool {
	if c == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// run is the loop. It owns the quiet-window timer and the poll ticker, so
// there is exactly one goroutine per coordinator and closing it stops
// everything it armed.
//
// The warmup gate is a fourth wake. A window that closes while builds are
// deferred spends the claim it was armed for — the checkout is dirty and
// nothing is going to say so again, because the signal that said it has been
// consumed — so the loop remembers the claim and runs it when the gate opens.
// Waiting for the gate inside the window instead would hold a wake the loop
// still has to answer for stop and for signals.
func (c *CheckoutCoordinator) run() {
	defer close(c.done)
	defer c.closeCheckoutRefreshTickets()
	defer c.releaseTextSearcher()
	lifetime := c.lifetimeContext()

	quiet := time.NewTimer(c.quiet)
	stopTimer(quiet)
	defer stopTimer(quiet)

	pollDelay := c.poll
	var pollC <-chan time.Time
	var pollTimer *time.Timer
	if c.poll > 0 {
		pollTimer = time.NewTimer(initialCheckoutPollDelay(c.checkoutID, c.poll))
		defer stopTimer(pollTimer)
		pollC = pollTimer.C
	}

	// nil once builds are admitted: an open gate's channel is always ready,
	// and selecting on it would spin the loop.
	admitted := c.admissionWait()
	claimed := false
	var armed <-chan time.Time
	// armedSince is when the armed window was first armed: a window re-armed
	// by a stream of signals is not extended past the coalesce cap, so a
	// tree that never stops changing still gets a cycle.
	var armedSince time.Time
	for {
		select {
		case <-c.stop:
			return
		case <-lifetime.Done():
			return
		case <-c.signal:
			// Re-arm on every signal: the window is quiet time since the LAST
			// claim that the checkout moved, not since the first.
			now := time.Now()
			if armed == nil {
				armedSince = now
			} else if now.Sub(armedSince) >= c.backgroundCoalesceCap() {
				continue
			}
			stopTimer(quiet)
			quiet.Reset(c.quiet)
			armed = quiet.C
		case <-c.demand:
			// Demand is owed only while a ticket waits. A ticket the
			// previous cycle already answered (one admitted while that
			// cycle's build was in flight, completed by its pre-publish
			// sample) leaves a spent wake behind: running a cycle for it
			// would only sample to find nothing owed. An armed window is
			// left armed.
			if c.checkoutRefreshHighWater() == 0 {
				continue
			}
			// Demand runs now. An armed window is spent by this cycle: the
			// cycle samples after every claim the window was coalescing.
			stopTimer(quiet)
			armed = nil
			if admitted != nil {
				claimed = true
				c.deferCycle()
				continue
			}
			c.cycle(lifetime)
		case <-pollC:
			// Absence is not removal evidence. Keep the coordinator available
			// for explicit demand and leave clocks, routes and leases to the
			// lifecycle, but do not fork git against a missing directory.
			if _, err := os.Stat(c.root); errors.Is(err, os.ErrNotExist) {
				limit := max(c.poll, 5*time.Minute)
				if pollDelay > limit/2 {
					pollDelay = limit
				} else {
					pollDelay *= 2
				}
			} else {
				pollDelay = c.poll
				c.Signal("poll")
			}
			pollTimer.Reset(pollDelay)
		case <-armed:
			armed = nil
			if admitted != nil {
				claimed = true
				c.deferCycle()
				continue
			}
			c.cycle(lifetime)
		case <-admitted:
			admitted = nil
			if !claimed {
				continue
			}
			claimed = false
			c.cycle(lifetime)
		}
	}
}

// admissionWait is the channel the loop waits for build admission on, nil when
// builds are already admitted.
func (c *CheckoutCoordinator) admissionWait() <-chan struct{} {
	if c.gate.Admitted() {
		return nil
	}
	return c.gate.Opened()
}

// deferCycle records a cycle the warmup gate held back. Nothing is read and
// nothing is written: the route keeps serving what the last run published, and
// the claim this window spent is carried to the gate's own wake.
func (c *CheckoutCoordinator) deferCycle() {
	c.mu.Lock()
	reason := c.reason
	c.mu.Unlock()
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeDeferred)
	c.logger.Debug("checkout coordinator: build deferred until the daemon has warmed up",
		zap.String("checkout", c.checkoutID), zap.String("reason", reason))
	if c.cycleDone != nil {
		c.cycleDone(CheckoutCycle{Deferred: true})
	}
}

// stopTimer stops a timer and drains a callback that already fired, so a
// following Reset arms a window that is empty.
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// cycle runs one reconcile pass and reports it.
func (c *CheckoutCoordinator) cycle(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	through := c.checkoutRefreshHighWater()
	// Taken after through: every ticket at or below it was admitted before
	// this instant, which is what lets the cycle's steps share one sample.
	cycleStarted := time.Now()
	// Each cycle owns its event history; a successful late rider may absorb
	// these actual instants after its exact completion checks pass.
	ctx = context.WithValue(ctx, publicationStampsKey{}, &PublicationStamps{})
	ctx = withCycleStart(ctx, cycleStarted)
	ctx = withPublicationTarget(ctx, c.checkoutID, through)
	markPublicationPhase(ctx, PublicationCycleStarted)
	defer c.guardCheckoutRefreshCycle(ctx, through)
	// A cycle that serves a ticket or builds a working tree is foreground
	// work: its end is what the ready-time warm-up's idle rule measures from
	// (noteForegroundCycle, lifecycleForegroundActivity).
	foreground := through != 0
	defer func() {
		if foreground {
			c.noteForegroundCycle(time.Now())
		}
	}()
	c.mu.Lock()
	reason := c.reason
	c.mu.Unlock()

	// A background cycle over a working tree that is still changing builds a
	// state that is gone before it can publish: it is held here, before its
	// sample decides anything and before it queues for the lock or the lane
	// (checkout_motion.go). A ticket's cycle is never held. The hold's
	// sample is the one the preflight then shares, so its time is counted
	// as preflight.
	//
	// Until it queues for the lane, a background cycle also steps aside for
	// a refresh ticket of its own checkout (preemptBackgroundOnDemand): its
	// pre-build steps run under preCtx, which a ticket's arrival cancels.
	preflightStarted := time.Now()
	preCtx, stopDemandWatch := ctx, func() bool { return false }
	if through == 0 {
		preCtx, stopDemandWatch = c.preemptBackgroundOnDemand(ctx)
	}
	demandWatched := true
	endDemandWatch := func() bool {
		if !demandWatched {
			return false
		}
		demandWatched = false
		return stopDemandWatch()
	}
	defer endDemandWatch()
	steppedAside := func() bool {
		if preCtx.Err() == nil || !endDemandWatch() {
			return false
		}
		out := c.ticketPreemptedCycle()
		out.cycleStarted = cycleStarted
		c.reportCheckoutCycle(ctx, through, out)
		return true
	}
	if through == 0 {
		if why, held := c.holdBackgroundCycle(preCtx); held {
			if steppedAside() {
				return
			}
			out := c.heldBackgroundCycle(why)
			out.cycleStarted = cycleStarted
			c.reportCheckoutCycle(ctx, through, out)
			return
		}
	}

	preflight := c.settledWithoutBuild
	if c.cyclePreflight != nil {
		preflight = c.cyclePreflight
	}
	if out, settled := preflight(preCtx); settled {
		out.cycleStarted = cycleStarted
		recordCoordinatorCycle(out)
		c.reportCheckoutCycle(ctx, through, out)
		return
	}
	if steppedAside() {
		return
	}

	// This cycle needs the build lane, and a background chain compaction of
	// this checkout would hold it: foreground work goes first, so the
	// compaction is canceled before the cycle queues (it is rescheduled by the
	// next build that reaches the soft depth).
	preflightDone := time.Now()
	if through == 0 {
		// No ticket names this cycle's work: if its sample shows a change the
		// route does not describe yet (a filesystem edit, a checkout, a
		// revert), the cycle opens a record for it so the edit's publication
		// is timed like a ticket's.
		ctx = withPhaseRecord(ctx, c.openObservedChangeRecord(preCtx))
	}
	c.cancelDirtyChainCompaction("foreground cycle")
	priority := ViewBuildBackground
	if through != 0 {
		priority = ViewBuildInteractive
	}
	// Lock, then lane: the order every builder uses (RehomeTo, a lease's
	// Refresh), so a lease that holds this lock while it queues for the lane is
	// never waited on by a lane owner. The lock is held through the queue. A
	// settled poll never gets here, and an edit on a checkout that needs a
	// build is refused before it waits on this lock: view_building on a
	// withdrawn route, stale on a moved HEAD or changed tree
	// (refuseStaleAdmission). What remains behind the lock is this checkout's
	// own work. The wait itself is cancellable, so stop and shutdown still
	// reach a loop parked here.
	if err := acquireCycleLock(preCtx, c); err != nil {
		if steppedAside() {
			return
		}
		out := CheckoutCycle{Err: err}
		recordCoordinatorCycle(out)
		c.reportCheckoutCycle(ctx, through, out)
		return
	}
	if endDemandWatch() {
		c.cycleMu.Unlock()
		out := c.ticketPreemptedCycle()
		out.cycleStarted = cycleStarted
		c.reportCheckoutCycle(ctx, through, out)
		return
	}
	defer c.cycleMu.Unlock()
	// A hint changes only read-only preparation admission; live reentry still
	// confirms route, source and immutable ancestry before any payload mutation.
	earlyImport := priority == ViewBuildBackground && c.importPreparationHint(ctx)
	laneQueued := time.Now()
	laneBefore := c.gate.Stats()
	release, err := c.gate.AcquireRanked(ctx, priority, c.selectionRequests(), c.ticketDemand.Load)
	if err != nil {
		if errors.Is(err, ErrViewBuildQueueFull) {
			c.logger.Debug("checkout coordinator: build deferred by admission capacity",
				zap.String("checkout", c.checkoutID),
				zap.String("reason", reason),
				zap.Error(err))
			viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeDeferred)
			c.reportCheckoutCycle(ctx, through, CheckoutCycle{Deferred: true})
			return
		}
		out := CheckoutCycle{Err: fmt.Errorf("indexer: wait for checkout build admission: %w", err)}
		recordCoordinatorCycle(out)
		c.reportCheckoutCycle(ctx, through, out)
		return
	}
	defer func() { release() }()
	admission := cycleAdmission{
		Preflight: preflightDone.Sub(preflightStarted),
		CycleLock: laneQueued.Sub(preflightDone),
		Lane:      time.Since(laneQueued),
	}
	if laneBefore.Active {
		admission.LaneHeldBy = laneBefore.Holder
		if admission.LaneHeldBy == nil {
			admission.LaneHeldBy = &ViewBuildLaneHolder{Kind: "undeclared", Since: laneBefore.ActiveSince}
		}
	}
	defer c.gate.NoteHolder(ViewBuildLaneHolder{
		Kind: "checkout_cycle", CheckoutID: c.checkoutID, Priority: viewBuildPriorityLabel(priority),
		Root: c.root, Reason: reason,
	})()
	// A background cycle gives the lane up to an interactive build that
	// starts waiting for it (another checkout's edit), up to its commit
	// point: the builder withdraws the yield right before it publishes.
	// A yielded cycle publishes nothing, releases the lane and queues again
	// at background priority (yieldedCycle).
	var laneYield *backgroundLaneYield
	// treeMove abandons the background build when the checkout's watcher
	// reports a change to a path it reads (checkout_motion.go), up to the
	// same commit point.
	var treeMove *backgroundLaneYield
	if priority == ViewBuildBackground && c.importAdmissionBarrier != nil {
		c.importAdmissionBarrier(ctx)
	}
	if priority == ViewBuildBackground {
		// Always armed: a background cycle gives the lane up to every
		// interactive build, however often it has yielded. Its work is
		// bounded so it still completes between edits (a large working tree
		// is built in batches that survive a yield), whereas a cycle that
		// stopped yielding held other checkouts' edits for its whole build
		// (live: 95 s and 313 s behind a 389-file working tree).
		if earlyImport {
			// Keep one cancellation lineage while private work is off lane.
			// Ordinary fallback re-arms this same lineage on live reentry.
			ctx, laneYield = privateImportLaneContext(ctx)
		} else {
			ctx, laneYield = armBackgroundLaneYield(ctx, c.gate, 0)
		}
		defer laneYield.close()
		ctx = context.WithValue(ctx, importBuildLaneKey{}, &importBuildLane{
			detach: func() bool {
				if !laneYield.commit() {
					return false
				}
				release()
				release = func() {}
				return true
			},
			resume: func(ctx context.Context, yieldable bool) (context.Context, error) {
				next, err := c.gate.AcquireRanked(ctx, ViewBuildBackground, c.selectionRequests(), c.ticketDemand.Load)
				if err != nil {
					return ctx, err
				}
				withdraw := c.gate.NoteHolder(ViewBuildLaneHolder{
					Kind: "checkout_import_publication", CheckoutID: c.checkoutID,
					Priority: viewBuildPriorityLabel(ViewBuildBackground), Root: c.root, Reason: reason,
				})
				release = func() { withdraw(); next() }
				return ctx, nil
			},
			arm: func(ctx context.Context) error {
				var err error
				laneYield, err = rearmBackgroundLaneYield(ctx, c.gate, laneYield)
				return err
			},
			gate: c.gate,
		})
		ctx, treeMove = c.armTreeMoveAbort(ctx, cycleStarted)
		defer c.disarmTreeMoveAbort(treeMove)
	}
	if earlyImport {
		lane, _ := ctx.Value(importBuildLaneKey{}).(*importBuildLane)
		finish, err := lane.begin(ctx)
		if err != nil {
			c.reportCheckoutCycle(ctx, through, CheckoutCycle{Err: err, Admission: admission, cycleStarted: cycleStarted})
			return
		}
		defer finish()
	}
	markPublicationPhase(ctx, PublicationAdmitted)
	if c.cycleBarrier != nil {
		c.cycleBarrier(ctx)
	}
	if err := ctx.Err(); err != nil {
		out := CheckoutCycle{Err: err}
		switch {
		case laneYield.Yielded():
			out = c.yieldedCycle(out, admission)
		case treeMove.Yielded():
			out = c.treeMovedCycle(out, admission, "watcher")
		}
		c.notePublicationOutcome(out, time.Now())
		recordCoordinatorCycle(out)
		c.reportCheckoutCycle(ctx, through, out)
		return
	}
	out := c.reconcile(ctx)
	out.cycleStarted = cycleStarted
	out.Admission = admission
	// The stack a working-tree delta over the routed commit generation reads
	// is warmed in the background, once, after the cycle that routed it
	// (edit_delta_stack_prewarm.go).
	if out.Err == nil {
		c.prewarmEditDeltaStackOnce(out.CommitGenerationID)
	}
	// A cycle the yield canceled ends in an error; one that finished its
	// work anyway (the cancel landed after its last step) keeps its outcome.
	// Any other background cycle ran to its end, failed or not, so the run
	// of consecutive yields is over: a checkout whose builds fail on their
	// own (a working tree edited faster than it is sampled) must not stay
	// unpreemptible once it has yielded maxViewBuildYields times.
	//
	// A background build the working tree moved under — the watcher's abort,
	// or a sample or prepublish fence refusing a moved tree — is rescheduled
	// through the quiet window, not failed: it published nothing, and the
	// state it would have built is already gone.
	switch {
	case laneYield.Yielded() && out.Err != nil:
		out = c.yieldedCycle(out, admission)
	case treeMove.Yielded() && out.Err != nil:
		c.resetBackgroundLaneYields()
		out = c.treeMovedCycle(out, admission, "watcher")
	case priority == ViewBuildBackground && workingTreeMovedWhileSampling(out.Err):
		c.resetBackgroundLaneYields()
		out = c.treeMovedCycle(out, admission, "sample")
	case workingTreeMovedWhileSampling(out.Err):
		out = c.sampleMovedTicketCycle(out, admission)
	case priority == ViewBuildBackground:
		c.resetBackgroundLaneYields()
		// A failed cycle keeps the motion backoff, or a checkout whose builds
		// keep failing would retry at the bare quiet window. So does one whose
		// two builds the prepublish fence tore: the fence confirms a build by
		// the bytes it parsed, so a torn pair is a working tree moving under
		// the reads themselves, the very motion the backoff paces. Every other
		// cycle that ended without an error resets it.
		if out.Err == nil && out.rescheduledBy != stallReasonTornByMotion {
			c.settleTreeMoveAborts()
		}
	}
	foreground = foreground || out.DirtyBuilt
	c.notePublicationOutcome(out, time.Now())
	recordCoordinatorCycle(out)
	c.logSlowAdmission(reason, through, admission)
	if out.CompactionScheduled {
		c.scheduleDirtyChainCompaction(out)
	}
	if out.DirtyBuilt && out.Err == nil {
		// The edit published without its semantic enrichment: the
		// follow-up owes it once the checkout is quiet.
		c.scheduleEnrichmentFollowup()
	}
	switch {
	case out.Err != nil && !errors.Is(out.Err, context.Canceled):
		c.logger.Warn("checkout coordinator: reconcile failed",
			zap.String("checkout", c.checkoutID), zap.String("root", c.root),
			// reason is what admitted the cycle; cause is the class of
			// what failed it (checkoutCycleFailureClass).
			zap.String("reason", reason), zap.String("cause", checkoutCycleFailureClass(out.Err)),
			zap.Error(out.Err))
	case out.CommitBuilt || out.DirtyBuilt || out.CommitReused || out.DirtyReused || out.Recomposed:
		// Every arm that moved the route logs, reuse included. A cycle that
		// adopted a retained working-tree layer has no outcome label of its
		// own in the shipped metric vocabulary (recordCoordinatorCycle says
		// why), so this line is the only place it is visible at all — leaving
		// it out would make the cheapest cycle the coordinator has the one
		// that reports nothing anywhere.
		c.logger.Debug("checkout coordinator: route updated",
			zap.String("checkout", c.checkoutID), zap.String("reason", reason),
			zap.Int64("commit_generation", out.CommitGenerationID),
			zap.Int64("dirty_generation", out.DirtyGenerationID),
			zap.Bool("commit_reused", out.CommitReused),
			zap.Bool("dirty_reused", out.DirtyReused),
			zap.Bool("recomposed", out.Recomposed))
	}
	c.reportCheckoutCycle(ctx, through, out)
}

// refreshWantsNewSample reports whether a refresh ticket waits whose
// freshAfter is later than the latest working-copy sample: completing it with
// this cycle's publication takes a sample begun after it arrived.
func (c *CheckoutCoordinator) refreshWantsNewSample() bool {
	if c == nil || c.sampler == nil {
		return false
	}
	latest := c.sampler.LastSampleStarted()
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	for _, request := range c.refreshWaiters {
		if request != nil && request.freshAfter.After(latest) {
			return true
		}
	}
	return false
}

// backgroundLaneYields is how many background cycles in a row yielded the lane.
func (c *CheckoutCoordinator) backgroundLaneYields() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.laneYields
}

// resetBackgroundLaneYields records a background cycle that ran to an outcome.
func (c *CheckoutCoordinator) resetBackgroundLaneYields() {
	c.mu.Lock()
	c.laneYields = 0
	c.mu.Unlock()
}

// yieldedCycle turns the outcome of a background cycle that was canceled to
// give the lane up into a rescheduled one: nothing it began was published
// (the builder abandons an unpublished generation on cancel), the route is as
// the cycle found it, and the loop runs the cycle again through the quiet
// window, queueing at background priority behind the interactive build. The
// lane itself is released by the caller's deferred release when cycle returns.
//
// A cycle the lifetime canceled is not a yield, whatever the gate asked: it
// stays an error so shutdown is not rescheduled.
func (c *CheckoutCoordinator) yieldedCycle(out CheckoutCycle, admission cycleAdmission) CheckoutCycle {
	if c.lifetimeContext().Err() != nil {
		return out
	}
	c.mu.Lock()
	c.laneYields++
	yields := c.laneYields
	c.mu.Unlock()
	if out.Err != nil && !errors.Is(out.Err, context.Canceled) {
		// Not necessarily the cancel itself (a wrapped build error, say);
		// logged so a real failure hidden behind the yield stays visible.
		c.logger.Debug("checkout coordinator: background cycle ended with an error while yielding the build lane",
			zap.String("checkout", c.checkoutID), zap.Error(out.Err))
	}
	out.Err = nil
	out.Rescheduled = true
	out.YieldedTo = laneYieldedToInteractive
	out.Admission = admission
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeRescheduled)
	c.logger.Info("checkout coordinator: background build yielded the lane to an interactive build",
		zap.String("checkout", c.checkoutID),
		zap.String("yielded_to", laneYieldedToInteractive),
		zap.Int("attempt", yields),
		zap.Int("max_yields", maxViewBuildYields),
		zap.Bool("dirty_built", out.DirtyBuilt),
		zap.Bool("commit_built", out.CommitBuilt))
	c.Signal("background build yielded the lane to an interactive build")
	return out
}

// settledWithoutBuild recognizes the overwhelmingly common poll result before
// it queues for the one build lane. It performs the same identity and dirty
// fingerprint checks as reconcile, but makes no catalog route change.
func (c *CheckoutCoordinator) settledWithoutBuild(ctx context.Context) (CheckoutCycle, bool) {
	var out CheckoutCycle
	if err := ctx.Err(); err != nil {
		return out, false
	}
	// The poll compares identities against the CACHED cohort. It does not
	// describe one: describing takes a daemon-wide roster read lease and a
	// catalog read per in-scope roster member, and paying that on every poll of
	// every checkout is K×N reads per interval — behind a lease every
	// repository registration and every admission close waits on — for an
	// answer that moves only when the topology does. Events refresh the cache
	// (InvalidateDependencyCohort), and every build path describes it afresh,
	// so what a layer is STAMPED with is always freshly validated.
	c.ensureDependencyCohort(ctx)
	base, err := c.primaryBase(ctx)
	if err != nil {
		return out, false
	}
	sample, err := c.cycleSample(ctx)
	if err != nil || sample.HeadTree == "" {
		return out, false
	}
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil || !found || route.State != store_sqlite.RouteActive ||
		route.GraphID != base.graphID || route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0 {
		return out, false
	}
	// The routed commit layer re-keys against the base it is composed over,
	// which is the base the family is on now UNLESS this checkout is pinned to
	// the one it was built against — the committed-regime dependent pin,
	// resolved by pinRoutedBase, whose doc carries the two-regime argument in
	// full.
	//
	// The short version, because this is the predicate the saving is actually
	// taken in. In the committed regime the routed pair is the base B1 plus the
	// delta diffTreeChanges(B1_tree, T), the materializer composes the ancestry
	// the routed generation itself names, and B1 is kept servable by the very
	// reference the routed delta makes: so the pair still equals this
	// checkout's tree exactly and the cheapest possible cycle — no build, no
	// route write, nothing — is the correct one. In the legacy regime there is
	// no generation to stay on: the base is the owner's recorded tree and the
	// corpus underneath is rewritten in place, so pinRoutedBase refuses and
	// this predicate re-keys against the family's current base exactly as it
	// always did, leaving the base advance to recomposeOverAdvancedBase.
	base, pinned := c.pinRoutedBase(ctx, base, route)
	out.BasePinned = pinned
	commit, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil || !found || !servableGeneration(commit.State) ||
		!c.routedCommitLayerFor(commit, base, sample.HeadTree) {
		return out, false
	}
	dirty, found, err := c.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
	if err != nil || !found || !servableGeneration(dirty.State) || dirty.LowerViewFingerprint != sample.Fingerprint {
		return out, false
	}
	// The routed working-tree layer must be rooted at the routed commit layer:
	// built directly over it, or a chain of this checkout's working-tree
	// generations that ends at it (dirtyChainRoot).
	if rooted, err := c.dirtyRootedAtCommit(ctx, dirty, commit); err != nil || !rooted {
		return out, false
	}

	c.noteDirtyFingerprint(sample.Fingerprint)
	c.retainCommit(ctx, generationIdentityKey(c.commitIdentity(base, sample.HeadTree)), commit.GenerationID)
	c.rememberRoutedDirty(dirty.GenerationID)
	out.CommitGenerationID = commit.GenerationID
	out.DirtyGenerationID = dirty.GenerationID
	return out, true
}

func (c *CheckoutCoordinator) lifetimeContext() context.Context {
	if c != nil && c.lifetime != nil {
		return c.lifetime
	}
	return context.Background()
}

// recordCoordinatorCycle counts what one cycle did.
//
// A cycle can settle both slots, so it can carry more than one outcome; the
// counter reads as "how often did a cycle do this", not as a partition of the
// cycles. The rescheduled case is deliberately silent here — a lost route flip,
// a working tree that moved under two builds and a checkout that committed
// under the cycle share the field but are different failures, so each is
// counted where it is decided.
func recordCoordinatorCycle(out CheckoutCycle) {
	switch {
	case out.Err != nil:
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeFailed)
	case out.Rescheduled:
	case out.Deferred:
		// A cycle held back for want of a describable cohort is the same
		// outcome as one held back by warmup or a saturated queue: nothing was
		// read, nothing was written, and the demand is still owed. The two
		// sites that decide it before reconcile is entered count it themselves;
		// without this arm the third one falls through to "skipped", which is
		// the label for a cycle that found nothing to do.
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeDeferred)
	case out.CommitBuilt || out.CommitReused || out.DirtyBuilt || out.DirtyReused:
		// A cycle that adopted a retained working-tree layer did something —
		// it re-routed the checkout — so it is not the "found nothing to do"
		// case, and it must not be counted as one. It has no label of its own
		// in the shipped outcome vocabulary, which is not this file's to
		// widen, so like the rescheduled arm it is deliberately silent here.
		// What the measurement reads is built_dirty NOT rising: an undo that
		// reuses is an undo that did not index.
		if out.CommitBuilt {
			viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeBuiltCommit)
		}
		if out.CommitReused {
			viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeAdoptedCommit)
		}
		if out.DirtyBuilt {
			viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeBuiltDirty)
		}
	default:
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeSkipped)
	}
}

// reconcile brings the checkout's route in line with the state its disk is in
// right now.
func (c *CheckoutCoordinator) reconcile(ctx context.Context) CheckoutCycle {
	var out CheckoutCycle
	// A working-tree build confirms its inputs by its read set unless a
	// refresh ticket waits that only a sample taken after it arrived can
	// complete: then its prepublish fence is that sample.
	ctx = withPrepublishSampleDemand(ctx, c.refreshWantsNewSample)
	// The plan's reads before the dirty slot, lapped for its plan record.
	lapStarted := time.Now()
	lap := func(name string) {
		now := time.Now()
		out.PlanLaps = append(out.PlanLaps, CycleLap{Name: name, Duration: now.Sub(lapStarted)})
		lapStarted = now
	}

	// One cohort per cycle, described afresh. Every identity this cycle mints
	// carries the same revision, so the layer it builds and the cache entry it
	// files it under cannot disagree — and the revision a layer is stored with
	// names inputs that were validated when the build started.
	//
	// A cohort that cannot be described defers the cycle rather than stamping a
	// layer nobody validated the inputs of. Nothing was read and nothing was
	// written at this point, so a deferred cycle leaves the route exactly as it
	// found it and the poll retries it.
	if !c.describeDependencyCohort(ctx) && !c.cohortAllowsBuild() {
		out.Deferred = true
		return out
	}
	lap("cohort")
	base, err := c.primaryBase(ctx)
	if err != nil {
		out.Err = err
		return out
	}
	lap("primary_base")
	head, err := c.cycleSample(ctx)
	if err != nil {
		out.Err = fmt.Errorf("indexer: sample checkout %s: %w", c.root, err)
		return out
	}
	lap("head_sample")
	if head.HeadTree == "" {
		// An unborn branch has no tree to build a commit layer from, and a
		// checkout with no commit generation has no view. There is nothing to
		// reconcile to until it has one commit.
		out.Err = fmt.Errorf("indexer: checkout %s has no HEAD tree", c.root)
		return out
	}

	route, err := c.ensureRoute(ctx, base)
	if err != nil {
		if errors.Is(err, errRouteMoved) {
			out.Rescheduled, out.rescheduledBy = true, stallReasonRouteMoved
			c.rescheduleOnLostRoute("route moved under the graph reset")
			return out
		}
		out.Err = err
		return out
	}
	lap("ensure_route")
	finishPreamble, err := c.prepareImportPreamble(ctx, head, route)
	if err != nil {
		out.Err = err
		return out
	}
	defer finishPreamble()
	if c.importPreambleBarrier != nil {
		c.importPreambleBarrier(ctx)
	}

	// The dependent pin: a checkout whose routed layers were built over a
	// committed base the family has since advanced past stays on that base. The
	// substitution is the whole mechanism — everything below then finds the
	// route already describing exactly the state it is asked to reconcile to,
	// so the cheap arms take themselves: reconcileCommitSlot's "already routed
	// to exactly this state" returns the routed generation without a build, and
	// reconcileDirtySlot keeps the working-tree layer sitting on it. A
	// dependent whose OWN tree moved rebuilds its delta against the base it is
	// pinned to rather than the family's current one, which is what keeps its
	// ancestry and its reuse cache stable across its own commits.
	//
	// pinRoutedBase refuses in the legacy regime and whenever anything but the
	// base moved, so both fall through to the paths they always took.
	base, pinned := c.pinRoutedBase(ctx, base, route)
	out.BasePinned = pinned

	// A committed base that advanced under a checkout whose own tree did not
	// move is recomposed, not torn down: both layers are rebuilt off-route and
	// installed in one write, so the pair the checkout already serves stays
	// coherent and routed for the whole of the rebuild. With the pin taken
	// there is nothing here to recompose — recomposableStack finds the routed
	// identity equal to the one a build would mint and declines — so this path
	// is now reached by the legacy regime, by a released pin, and by a base
	// that stopped being servable under the route.
	if handled, err := c.recomposeOverAdvancedBase(ctx, base, head, &route, &out); handled || err != nil {
		if err != nil {
			if errors.Is(err, errRouteMoved) {
				out.Rescheduled, out.rescheduledBy = true, stallReasonRouteMoved
				c.rescheduleOnLostRoute("route moved under the recomposition")
				return out
			}
			out.Err = err
		}
		return out
	}

	lap("base_recompose")
	commitGeneration, err := c.reconcileCommitSlot(ctx, base, head.HeadTree, &route, &out)
	lap("commit_slot")
	if err != nil {
		if errors.Is(err, errRouteMoved) {
			out.Rescheduled, out.rescheduledBy = true, stallReasonRouteMoved
			c.rescheduleOnLostRoute("route moved under the commit flip")
			return out
		}
		if errors.Is(err, errBaseMoved) {
			// Already counted, logged and signalled beside the decision, the
			// way the HEAD-move guard records its own.
			return out
		}
		out.Err = err
		return out
	}
	out.CommitGenerationID = commitGeneration

	if err := c.reconcileDirtySlot(ctx, commitGeneration, head.HeadTree, &route, &out); err != nil {
		if errors.Is(err, errRouteMoved) {
			out.Rescheduled, out.rescheduledBy = true, stallReasonRouteMoved
			c.rescheduleOnLostRoute("route moved under the dirty flip")
			return out
		}
		out.Err = err
	}
	return out
}

// recomposeOverAdvancedBase rebuilds a dependent checkout's whole stack over a
// committed base that advanced under it, and installs it in one route write.
//
// It is the difference gate 5 asks for between rebuilding and recomposing. The
// ordinary path settles the commit slot first, and moveCommitSlot clears the
// working-tree slot in the same compare-and-set, because a working-tree layer
// over a DIFFERENT commit layer is a state the checkout was never in. That is
// the right answer when the checkout itself moved: the pair it was serving no
// longer describes anything. It is the wrong answer when the checkout did not
// move at all and only the family's base advanced underneath it — the pair it
// is serving is still exactly this checkout's tree, composed over an immutable
// ancestor that is pinned for as long as a reader names it, and taking it away
// for the length of a rebuild costs the checkout its uncommitted edits for no
// coherence that was at risk.
//
// So the two layers are built off-route and installed together. Until that
// write lands the route names the old pair — old base, old delta, old
// working-tree layer — which composes to this checkout's tree exactly as it
// did before the base moved. After it lands the route names the new pair. No
// reader ever sees the new base under the old delta, which is what the splice
// prohibition means, and the materializer could not compose one anyway: it
// walks the routed generation's own immutable BaseGenerationID ancestry
// (materialize.go pinCheckoutRoute -> generationAncestry), never the primary
// graph's current active pointer.
//
// The new delta is a bounded one. resolveCommitLayer reaches BuildCommitLayer,
// whose change set is `git diff-tree newBase..thisTree` (builder_commit.go
// diffTreeChanges) — the difference between the two committed trees, not a
// fresh index of the checkout.
//
// BOUNDED RECOMPOSITION IS WHAT GATE 5'S "while reusing valid payload" MEANS
// HERE. Why a recomposition is taken at all is NOT the same answer in the two
// regimes graphBase serves, and the difference is the whole of whether keeping
// the dependent on the base it was built against is an unimplemented saving or
// a bug. Stating it unconditionally either way is wrong, so:
//
// A dependent's commit layer IS a diff-tree against its base — the base tree
// is the left-hand side of the diff and is stamped on the generation as its
// lower_view_fingerprint (commitIdentity) — so the same payload over a
// DIFFERENT base is not a valid delta and there is no key under which it would
// be. What the two regimes disagree about is whether the base underneath a
// routed delta can change at all.
//
//   - The primary graph HAS published a generation (graphBase's first arm,
//     ActiveGenerationID > 0). commitIdentity stamps BaseGenerationID = that
//     generation; the generation is immutable; retirement refuses one anything
//     still names (ErrCatalogGenerationReferenced); and the materializer
//     composes the ancestry the ROUTED generation itself names, walking its own
//     BaseGenerationID chain rather than the family's current pointer
//     (graphview generationAncestry). So B1 plus that delta is still exactly
//     this checkout's tree after the family moves to B2, and an un-recomposed
//     dependent is NOT serving stale content. Recomposing here buys currency
//     and availability, not correctness — so HERE THE DEPENDENT IS NOT
//     RECOMPOSED AT ALL. It stays on B1: pinRoutedBase substitutes the base the
//     route was built against before this path is reached, the routed identity
//     then equals the one a build would mint, and recomposableStack declines.
//     That is the dependent pin, and it costs a base advance zero dependent
//     builds and zero route writes. This path stays reachable in this regime
//     for the three cases the pin refuses: a base a retirement sweep has asked
//     the coordinator to release (RequestBaseRelease), a pinned generation that
//     stopped being servable, and an identity that moved in more than its base
//     fields.
//
//   - The primary graph has NOT published one (graphBase's second arm: the
//     base is the owner checkout's recorded committed tree). commitIdentity
//     stamps BaseGenerationID = 0, so the delta names no immutable ancestor at
//     all — generationAncestry's walk terminates at zero rather than including
//     it, and what the layer composes over is the shared indexed corpus, which
//     carries no version identity and is re-indexed in place as the primary
//     moves. baseMovedUnderCycle exists for exactly this and says so: the base
//     "moves without any pointer moving with it". Here the old delta really
//     does go stale at the paths the two bases differ by, because the layer
//     beneath it was rewritten under it, and recomposition is the correctness
//     requirement — the same hazard the pre-build guard at reconcileCommitSlot
//     refuses.
//
// settledWithoutBuild accepts a slot whose parent is no longer the family's
// active base ONLY through pinRoutedBase, and pinRoutedBase refuses the second
// regime outright: there the parent is not immutable and the slot is simply
// wrong. In the first regime the pin comes with the three things it needs, and
// none of them is optional. The base stays servable because the routed delta
// names it and the catalog refuses to retire a generation another generation
// is based on; the dependent does not freeze on it because the retirement
// sweep asks for it back (RequestBaseRelease) as soon as the superseded-chain
// retention window stops covering it; and the release lands as one
// recomposition here, which installs the replacement stack in a single
// compare-and-set before the old base is given up.
//
// So the reuse this path delivers is three narrower guarantees, each of them
// observable:
//
//   - the old stack keeps serving until the new one is installed, in ONE
//     compare-and-set (installStack). This is what the ordinary slot-by-slot
//     path cannot offer: moveCommitSlot flips the commit slot with
//     DirtyGenerationID 0 and State RoutePending, so a dependent taking a base
//     advance through it serves a commit-only, pending route for the whole
//     duration of the working-tree rebuild;
//   - the rebuild never widens beyond the dependent's OWN change sets — the
//     commit layer is the two trees' diff, the working-tree layer is the
//     checkout's own dirty set — so a base advance costs each dependent one
//     bounded delta plus one working-tree layer, never an index of its tree;
//   - a dependent whose own tree did not move at all still gets that bounded
//     rebuild rather than a full one — in the legacy regime, where it must be
//     rebuilt at all. In the committed regime that dependent is pinned and
//     never reaches this path until its base is asked for back.
//
// TestBaseAdvanceRecomposesTwoDependentsOnceEachWithABoundedDelta measures all
// three: two dependents, one advance, exactly two commit-delta builds and two
// working-tree builds, each claiming exactly the paths its own change set
// names.
//
// It handles the base-advance case ONLY, and says so by returning false for
// everything else: a checkout that also moved its own HEAD, a route that is
// not serving a complete pair, or an identity that differs by anything other
// than the base it names. Those go to the ordinary slot-by-slot path, which
// already does the right thing for them.
func (c *CheckoutCoordinator) recomposeOverAdvancedBase(
	ctx context.Context,
	base primaryBase,
	head gitstate.DirtySnapshot,
	route *store_sqlite.CheckoutRoute,
	out *CheckoutCycle,
) (bool, error) {
	commitRow, dirtyRow, ok, err := c.recomposableStack(ctx, base, head, *route)
	if err != nil || !ok {
		return false, err
	}
	ctx, err = resumeImportBuildLane(ctx, true)
	if err != nil {
		return true, err
	}
	// Only the base moved. Nobody is using the checkout: leave the stack it
	// is routed to and apply the advance on its next use
	// (checkout_propagation.go).
	if !c.rebaseWantedNow() {
		c.deferRebase(base, out)
		return true, nil
	}
	scope, err := c.propagationScopeFor(ctx, commitRow.LowerViewFingerprint, base.treeOID,
		commitRow.GenerationID, dirtyRow.GenerationID)
	if err != nil {
		c.logger.Debug("checkout coordinator: could not scope the base advance by content",
			zap.String("checkout", c.checkoutID), zap.Error(err))
	} else {
		out.Propagation = scope
	}

	previousCommit, previousDirty := commitRow.GenerationID, dirtyRow.GenerationID
	commitGeneration, reused, err := c.resolveCommitLayer(ctx, base, head.HeadTree)
	if err != nil {
		return true, err
	}
	// The checkout is free to commit while the delta over the new base is
	// being built. A working-tree layer built afterwards describes the tree of
	// a HEAD the layer beneath knows nothing about, so the cycle stops and the
	// next one rebuilds for the head the checkout is really at — the same
	// guard, and the same counter, reconcileDirtySlot makes for itself.
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		c.abandonBuild(ctx, commitGeneration, !reused)
		return true, fmt.Errorf("indexer: sample %s: %w", c.root, err)
	}
	c.noteDirtyFingerprint(sample.Fingerprint)
	if sample.HeadTree != head.HeadTree {
		c.abandonBuild(ctx, commitGeneration, !reused)
		out.Rescheduled, out.rescheduledBy = true, stallReasonHeadMoved
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeHeadMoved)
		c.logger.Debug("checkout coordinator: the checkout committed under the recomposition",
			zap.String("checkout", c.checkoutID),
			zap.String("built_for", head.HeadTree), zap.String("now_at", sample.HeadTree))
		c.Signal("the checkout moved to another commit under the cycle")
		return true, nil
	}

	// The working-tree chain is carried over by copy: base + commit layer
	// composes to the same committed tree over either base, so the chain's
	// rows are still exact (checkout_propagation.go).
	var (
		dirtyGeneration int64
		dirtyKey        string
		reparented      bool
	)
	dirtyGeneration, dirtyKey, err = c.reparentDirtyChain(ctx, base.graphID, dirtyRow.GenerationID,
		commitRow, commitGeneration, sample.Fingerprint)
	switch {
	case err == nil:
		reparented = true
		// The carried-over chain is written; the same window a rebuilt
		// working-tree layer opens before it is installed.
		if c.dirtyBarrier != nil {
			c.dirtyBarrier()
		}
	case errors.Is(err, errReparentRefused):
		c.logger.Debug("checkout coordinator: working-tree chain rebuilt instead of carried over",
			zap.String("checkout", c.checkoutID), zap.Error(err))
		dirtyGeneration, dirtyKey = 0, ""
	default:
		c.abandonBuild(ctx, commitGeneration, !reused)
		return true, err
	}
	if !reparented {
		dirtyGeneration, dirtyKey, err = c.buildDirtyLayerOver(ctx, base.graphID, commitGeneration, out)
	}
	if err != nil && !reparented {
		c.abandonBuild(ctx, commitGeneration, !reused)
		return true, err
	}
	if dirtyGeneration == 0 {
		c.abandonBuild(ctx, commitGeneration, !reused)
		out.Rescheduled, out.rescheduledBy = true, stallReasonTornByMotion
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeRescheduled)
		c.Signal("the working tree moved under two builds")
		return true, nil
	}
	// The base may have advanced again while this pair was being built.
	// Installing now would route a delta over a base the family has already
	// left, which is the splice this whole path exists to avoid.
	abandonDirty := func() {
		if !reparented {
			c.abandonBuild(ctx, dirtyGeneration, true)
			return
		}
		// A carried-over chain is new generations top to bottom, none of
		// them routed; the old chain is still the routed one.
		for _, id := range c.dirtyChainMembers(ctx, dirtyGeneration) {
			if id != dirtyRow.GenerationID {
				c.abandonBuild(ctx, id, true)
			}
		}
	}
	moved, err := c.baseMovedUnderCycle(ctx, base)
	if err != nil || moved {
		abandonDirty()
		c.abandonBuild(ctx, commitGeneration, !reused)
		if err != nil {
			return true, err
		}
		c.rescheduleOnMovedBase(base, out)
		return true, nil
	}

	if err := c.installStack(ctx, *route, true, base.graphID, commitGeneration, dirtyGeneration); err != nil {
		abandonDirty()
		c.abandonBuild(ctx, commitGeneration, !reused)
		return true, err
	}
	route.RouteEpoch++
	route.State = store_sqlite.RouteActive
	route.CommitGenerationID = commitGeneration
	route.DirtyGenerationID = dirtyGeneration
	c.rememberRoutedDirty(dirtyGeneration)

	out.Recomposed = true
	out.CommitGenerationID, out.DirtyGenerationID = commitGeneration, dirtyGeneration
	out.CommitBuilt, out.CommitReused, out.DirtyBuilt = !reused, reused, !reparented
	out.DirtyReparented = reparented
	c.rebaseApplied()
	c.retainCommit(ctx, generationIdentityKey(c.commitIdentity(base, head.HeadTree)), commitGeneration)
	// The recomposed pair is filed in both reuse caches, and the pair it
	// replaced is RELEASED into them rather than retired. The old
	// working-tree layer names the old commit layer as its base, so it can
	// only ever be re-routed over that same layer — but it is exactly what a
	// checkout coming back to this base (a revert of the advance, a rehome
	// that lands where it started) would otherwise re-index.
	c.retainDirty(ctx, dirtyKey, dirtyGeneration)
	c.releaseCommit(ctx, previousCommit)
	c.releaseDirty(ctx, previousDirty)
	c.signalDirtyOutpaced(*out)
	return true, nil
}

// recomposableStack decides whether this cycle is looking at a checkout whose
// own state did not move and whose base did.
//
// Every clause is a refusal to take the recomposition path for a case the
// ordinary one owns. The route must name a complete, servable pair this
// coordinator built; the commit layer must describe the tree the checkout is
// at right now; the working-tree layer must sit on that commit layer and match
// the fingerprint the cycle just sampled; and the commit layer's identity must
// differ from the one a build would mint now in the base fields ALONE. That
// last one is the whole test: rendering the current identity with the routed
// row's base fields substituted and comparing it against the row's own key
// says "nothing but the base moved" in terms of the same renderer the reuse
// cache and the catalog compare, so a configuration change, a cohort change,
// an extractor bump or a resolver bump all fall through to a rebuild.
func (c *CheckoutCoordinator) recomposableStack(
	ctx context.Context,
	base primaryBase,
	head gitstate.DirtySnapshot,
	route store_sqlite.CheckoutRoute,
) (commitRow, dirtyRow store_sqlite.ViewGeneration, ok bool, err error) {
	if route.State != store_sqlite.RouteActive || route.GraphID != base.graphID ||
		route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0 ||
		head.HeadTree == "" || head.Fingerprint == "" {
		return commitRow, dirtyRow, false, nil
	}
	commitRow, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil {
		return commitRow, dirtyRow, false, err
	}
	if !found || !servableGeneration(commitRow.State) ||
		commitRow.OwnerKind != checkoutLayerOwnerKind ||
		commitRow.GenerationKind != CommitLayerGenerationKind ||
		// The routed layer may be another checkout's the coordinator adopted
		// (sharedCommit): the identity comparisons below accept it by inputs.
		commitRow.GraphID != base.graphID ||
		// Defence in depth, and knowingly redundant: TreeOID is part of the
		// commit identity, so the substituted-identity comparison below
		// refuses a moved HEAD on its own (that is what the moved-HEAD case
		// in TestRecompositionRefusesAnythingButABaseAdvance binds). It is
		// kept because this clause is what makes the sentence "the commit
		// layer must describe the tree the checkout is at right now" true by
		// reading, and because it stays correct if the identity renderer ever
		// stops carrying the tree.
		commitRow.TreeOID != head.HeadTree {
		return commitRow, dirtyRow, false, nil
	}
	current := c.commitIdentity(base, head.HeadTree)
	if commitLayerMatches(commitRow, current) {
		// Nothing moved at all. The ordinary path recognises this in one read
		// and keeps the route exactly as it is.
		return commitRow, dirtyRow, false, nil
	}
	pinned := current
	pinned.BaseGenerationID = commitRow.BaseGenerationID
	pinned.LowerViewFingerprint = commitRow.LowerViewFingerprint
	if !commitLayerMatches(commitRow, pinned) {
		return commitRow, dirtyRow, false, nil
	}
	dirtyRow, found, err = c.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
	if err != nil {
		return commitRow, dirtyRow, false, err
	}
	if !found || !servableGeneration(dirtyRow.State) ||
		dirtyRow.GenerationKind != DirtyLayerGenerationKind ||
		dirtyRow.LowerViewFingerprint != head.Fingerprint {
		return commitRow, dirtyRow, false, nil
	}
	// Rooted at the routed commit layer, directly or through a chain. The
	// recomposition that follows always rebuilds the working tree DIRECT over
	// the new commit layer: a chain is never re-parented onto another commit.
	rooted, err := c.dirtyRootedAtCommit(ctx, dirtyRow, commitRow)
	if err != nil {
		return commitRow, dirtyRow, false, err
	}
	if !rooted {
		return commitRow, dirtyRow, false, nil
	}
	return commitRow, dirtyRow, true, nil
}

// rescheduleOnLostRoute records a compare-and-set this cycle lost and signals
// the retry. It is the cas_lost half of a rescheduled cycle: another actor
// moved the route, which is a different condition from a working tree that
// would not settle, and the two must not add up to one number.
func (c *CheckoutCoordinator) rescheduleOnLostRoute(reason string) {
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeCASLost)
	c.logger.Debug("checkout coordinator: route flip lost",
		zap.String("checkout", c.checkoutID), zap.String("reason", reason))
	c.Signal(reason)
}

// rescheduleOnMovedBase records a cycle that refused to route a layer over a
// base the family has left, and asks for the one that will compose over the
// base that is current. It counts as rescheduled rather than as a lost
// compare-and-set: nothing was written and no route was contended, the inputs
// simply moved — the same shape as the working tree moving under two builds,
// and it is recorded at the same place that one is, beside the decision.
func (c *CheckoutCoordinator) rescheduleOnMovedBase(base primaryBase, out *CheckoutCycle) {
	out.Rescheduled, out.rescheduledBy = true, stallReasonBaseMoved
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeRescheduled)
	c.logger.Debug("checkout coordinator: the primary base advanced under the cycle",
		zap.String("checkout", c.checkoutID),
		zap.Int64("built_over_generation", base.generationID),
		zap.String("built_over_tree", base.treeOID))
	c.Signal("the primary base advanced under the cycle")
}

// RehomeTo rebuilds this checkout's whole stack over another dedicated graph
// and installs it in one route write.
//
// It is the transition primitive both mode changes are built on. The layers
// are built while the route still says whatever it said before, and the graph
// and both generation pointers move together in a single compare-and-set, so a
// reader materializing during the rebuild gets the old stack or the new one —
// never a route naming the new graph with no layers over it, which is what the
// ordinary cycle leaves for the length of a build when it finds the primary
// moved under it.
//
// A checkout with no route at all — a dedicated one being demoted — has its
// route installed by the same write. There is nothing to compare against in
// that case: only a coordinator writes a checkout's route, and a checkout that
// has none has no coordinator either.
func (c *CheckoutCoordinator) RehomeTo(ctx context.Context, graphID string) (CheckoutCycle, error) {
	var out CheckoutCycle
	if c == nil {
		return out, errors.New("indexer: no coordinator to rehome")
	}
	if !c.admitSourceMutation() {
		return out, context.Canceled
	}
	defer c.releaseSourceMutation()
	ctx, cancel := checkoutMutationContext(ctx, c.lifetimeContext())
	defer cancel()
	if err := ctx.Err(); err != nil {
		return out, err
	}
	// Lock before lane, as cycle does; the rebuild budget and the coordinator
	// lifetime bound the lock wait as they bound the lane wait.
	if err := acquireCycleLock(ctx, c); err != nil {
		return out, fmt.Errorf("indexer: wait for checkout route lock: %w", err)
	}
	defer c.cycleMu.Unlock()
	c.cancelDirtyChainCompaction("checkout transition")
	release, err := c.gate.Acquire(ctx, ViewBuildInteractive)
	if err != nil {
		return out, fmt.Errorf("indexer: wait for checkout build admission: %w", err)
	}
	defer release()
	defer c.gate.NoteHolder(ViewBuildLaneHolder{
		Kind: "checkout_transition", CheckoutID: c.checkoutID, Priority: viewBuildPriorityLabel(ViewBuildInteractive),
	})()

	if err := ctx.Err(); err != nil {
		return out, err
	}
	// A transition builds a whole stack; it describes its cohort the same way a
	// cycle does, and for the same reason. A transition is a caller-driven
	// promise about a route, so it does not defer on an undescribable cohort
	// the way the loop's own cycle does — it builds under the stable degraded
	// revision and the Warn says why.
	c.describeDependencyCohort(ctx)

	dedicated, found, err := c.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("%w: dedicated graph %s", store_sqlite.ErrCatalogNotFound, graphID)
	}
	base, err := graphBase(ctx, c.catalog, dedicated)
	if err != nil {
		return out, err
	}
	head, err := gitstate.SampleHEAD(ctx, c.root)
	if err != nil {
		return out, fmt.Errorf("indexer: sample HEAD of %s: %w", c.root, err)
	}
	if head.TreeOID == "" {
		return out, fmt.Errorf("indexer: checkout %s has no HEAD tree", c.root)
	}
	route, routed, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return out, err
	}

	commitGeneration, reused, err := c.resolveCommitLayer(ctx, base, head.TreeOID)
	if err != nil {
		return out, err
	}
	out.CommitGenerationID, out.CommitBuilt, out.CommitReused = commitGeneration, !reused, reused

	// The transition drops the whole working-tree cache below, sparing only
	// the layer it routes, so the build's key is not filed here.
	dirtyGeneration, _, err := c.buildDirtyLayerOver(ctx, base.graphID, commitGeneration, &out)
	if err != nil {
		c.abandonBuild(ctx, commitGeneration, !reused)
		return out, err
	}
	if dirtyGeneration == 0 {
		c.abandonBuild(ctx, commitGeneration, !reused)
		out.Rescheduled = true
		return out, errCheckoutUnsettled
	}
	out.DirtyGenerationID, out.DirtyBuilt = dirtyGeneration, true

	if err := c.installStack(ctx, route, routed, base.graphID, commitGeneration, dirtyGeneration); err != nil {
		c.abandonBuild(ctx, dirtyGeneration, true)
		c.abandonBuild(ctx, commitGeneration, !reused)
		if errors.Is(err, errRouteMoved) {
			out.Rescheduled = true
		}
		return out, err
	}

	// Everything the cache held was built over the graph the checkout has just
	// left, so none of it can ever be routed here again — except the layer
	// this transition routed, which the cache may have supplied.
	c.dropRetained(ctx, commitGeneration)
	// The same is true of every retained working-tree layer, except the one
	// this transition has just routed. It is not filed under a key here — the
	// transition never sampled the tree itself, the builder did — so the first
	// cycle after the transition retains it from the route.
	c.dropRetainedDirty(ctx, dirtyGeneration)
	c.retainCommit(ctx, generationIdentityKey(c.commitIdentity(base, head.TreeOID)), commitGeneration)
	c.rememberRoutedDirty(dirtyGeneration)
	if routed {
		c.offerRetire(ctx, route.DirtyGenerationID)
		c.offerRetire(ctx, route.CommitGenerationID)
	}
	c.signalDirtyOutpaced(out)
	return out, nil
}

// installStack points a checkout's route at a graph and both of its
// generations in one write.
func (c *CheckoutCoordinator) installStack(
	ctx context.Context,
	route store_sqlite.CheckoutRoute,
	routed bool,
	graphID string,
	commitGeneration, dirtyGeneration int64,
) error {
	if !routed {
		return c.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
			CheckoutID:         c.checkoutID,
			GraphID:            graphID,
			CommitGenerationID: commitGeneration,
			DirtyGenerationID:  dirtyGeneration,
			State:              store_sqlite.RouteActive,
		})
	}
	err := c.catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID:         c.checkoutID,
		ExpectedRouteEpoch: route.RouteEpoch,
		GraphID:            graphID,
		CommitGenerationID: commitGeneration,
		DirtyGenerationID:  dirtyGeneration,
		State:              store_sqlite.RouteActive,
	})
	if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		return fmt.Errorf("%w: whole stack", errRouteMoved)
	}
	return err
}

// abandonBuild gives up a generation a transition will not route. One this
// call built is superseded and offered for collection; one it took from the
// reuse cache belongs to the cycle that built it and is left alone.
func (c *CheckoutCoordinator) abandonBuild(ctx context.Context, generationID int64, built bool) {
	if generationID <= 0 || !built {
		return
	}
	c.supersede(ctx, generationID)
	c.offerRetire(ctx, generationID)
}

// primaryBase reads the family's primary dedicated graph and the committed
// state the corpus under it is at.
//
// The tree comes from the primary's published generation when it has one, and
// from the primary checkout's recorded HEAD tree when it does not — which is
// what a plainly indexed primary looks like today, its corpus being the whole
// of its content. The primary's own uncommitted edits are not in that tree and
// are not described by any layer: they belong to the live incremental index,
// which keeps writing them into the corpus. A commit layer built over a dirty
// primary therefore diffs from the primary's HEAD while the corpus holds the
// primary's working tree, and the difference between the two shows through at
// exactly the paths the primary has edited and the checkout has not.
func (c *CheckoutCoordinator) primaryBase(ctx context.Context) (primaryBase, error) {
	graphs, err := c.catalog.ListDedicatedGraphs(ctx, c.familyID)
	if err != nil {
		return primaryBase{}, err
	}
	var primary *store_sqlite.DedicatedGraph
	for i := range graphs {
		if graphs[i].IsPrimaryBase {
			primary = &graphs[i]
			break
		}
	}
	if primary == nil {
		return primaryBase{}, fmt.Errorf("indexer: family %s has no primary dedicated graph", c.familyID)
	}
	base, err := graphBase(ctx, c.catalog, *primary)
	if err == nil && base.generationID == 0 {
		// graphBase's SECOND arm: the primary has published no generation and
		// the base is its owner checkout's recorded committed tree.
		//
		// This is the one place in the daemon where the absence of a committed
		// base is noticed by something that would read one. The startup
		// publisher and the live advance trigger both decline to publish for a
		// family with no reader (InitialBasePublisher.publish's "no dependent
		// checkout" skip) — and this coordinator IS that reader: it exists
		// only for a ready automatic checkout, which is precisely what
		// dedicatedBaseConsumers counts. So the ask belongs here, at the first
		// moment the absence matters to anyone.
		//
		// Asking changes nothing about THIS cycle. The publication is queued
		// on the publisher's own list and built off this goroutine; the cycle
		// carries on over generation 0 in the legacy regime, and when the base
		// lands the family fan-out (CheckoutLifecycle's base-advance signal)
		// wakes this coordinator to recompose onto it — bounded, off-route and
		// installed in one compare-and-set (recomposeOverAdvancedBase).
		c.demandCommittedBase()
	}
	return base, err
}

// dedicatedBaseDemandInterval bounds how often one coordinator re-asks for its
// family's committed base. See CheckoutCoordinator.lastBaseDemand.
const dedicatedBaseDemandInterval = time.Minute

// demandCommittedBase asks the publisher for the family primary's committed
// base, at most once per dedicatedBaseDemandInterval.
//
// The throttle is not a de-duplicator — RequestBase already coalesces onto a
// queued request, so a burst costs one queue lookup each. It bounds the case
// the coalescing cannot: a publication that was ATTEMPTED and failed leaves
// nothing queued, and a 15 s poll would otherwise re-enter the whole protocol
// (including the authority claim, which writes) four times a minute for as
// long as the failure lasts.
func (c *CheckoutCoordinator) demandCommittedBase() {
	if c == nil || c.requestBase == nil || c.repoPrefix == "" {
		return
	}
	now := time.Now()
	c.baseDemandMu.Lock()
	if !c.lastBaseDemand.IsZero() && now.Sub(c.lastBaseDemand) < dedicatedBaseDemandInterval {
		c.baseDemandMu.Unlock()
		return
	}
	c.lastBaseDemand = now
	c.baseDemandMu.Unlock()
	c.requestBase(c.repoPrefix)
}

// graphBase resolves the corpus state a layer over one dedicated graph sits
// on: the graph's published generation when it has one, and the tree its
// owning checkout is committed at otherwise. Every layer built over a graph —
// a checkout's commit layer, a ref view's — reads its base from here, so the
// two cannot disagree about what "the corpus" is.
func graphBase(
	ctx context.Context,
	catalog *store_sqlite.Catalog,
	dedicated store_sqlite.DedicatedGraph,
) (primaryBase, error) {
	out := primaryBase{graphID: dedicated.GraphID}
	if dedicated.ActiveGenerationID > 0 {
		row, found, err := catalog.GetViewGeneration(ctx, dedicated.ActiveGenerationID)
		if err != nil {
			return primaryBase{}, err
		}
		if !found {
			return primaryBase{}, fmt.Errorf(
				"indexer: primary graph %s active generation %d does not exist",
				dedicated.GraphID, dedicated.ActiveGenerationID)
		}
		if row.OwnerKind != checkoutLayerOwnerKind || row.GraphID != dedicated.GraphID ||
			row.CheckoutID != dedicated.OwnerCheckoutID || row.GenerationKind != "dedicated" {
			return primaryBase{}, fmt.Errorf(
				"indexer: primary graph %s active generation %d has incompatible ownership or kind",
				dedicated.GraphID, dedicated.ActiveGenerationID)
		}
		if !servableGeneration(row.State) || row.TreeOID == "" {
			return primaryBase{}, fmt.Errorf(
				"indexer: primary graph %s active generation %d is not a servable committed tree (state %s)",
				dedicated.GraphID, dedicated.ActiveGenerationID, row.State)
		}
		out.generationID = row.GenerationID
		out.treeOID = row.TreeOID
		return out, nil
	}
	owner, found, err := catalog.GetCheckout(ctx, dedicated.OwnerCheckoutID)
	if err != nil {
		return primaryBase{}, err
	}
	if !found || owner.HeadTree == "" {
		return primaryBase{}, fmt.Errorf(
			"indexer: primary graph %s names no committed tree to build over", dedicated.GraphID)
	}
	out.treeOID = owner.HeadTree
	return out, nil
}

// pinRoutedBase substitutes, for one cycle, the committed base this checkout's
// route was BUILT against for the one the family's primary is on now.
//
// THIS IS THE DEPENDENT PIN, AND IT IS A TWO-REGIME CONTRACT. Which regime a
// family is in is graphBase's two arms, and the pin is only valid in one of
// them:
//
//   - COMMITTED REGIME (graphBase's first arm, ActiveGenerationID > 0). The
//     base is an immutable published generation. commitIdentity stamps it as
//     the layer's BaseGenerationID; the materializer composes the ancestry the
//     ROUTED generation itself names, walking its own BaseGenerationID chain
//     and never the family's active pointer (graphview generationAncestry);
//     and the catalog refuses to retire a generation another generation names
//     as its base (viewGenerationReferencedSQL's base_generation_id term), so
//     the routed delta IS the reference that keeps its base servable. A
//     dependent's routed pair is B1 plus the delta diffTreeChanges(B1_tree,
//     T), so the pair equals this checkout's own tree T exactly, for every
//     path, however far the primary has advanced past B1. The view stays exact
//     and the freshness rider stays truthful: the rider answers for this
//     checkout's own head and working tree, which is what the pair describes.
//     Staying on B1 is therefore correct, and it is free — no commit-layer
//     build, no working-tree build, no route write.
//
//   - LEGACY REGIME (graphBase's second arm: the base is the owner checkout's
//     recorded committed tree, generationID 0). There is nothing to pin. The
//     layer names no immutable ancestor, it composes over the shared indexed
//     corpus, and that corpus is rewritten IN PLACE as the primary moves —
//     baseMovedUnderCycle's doc says it: the base "moves without any pointer
//     moving with it". Keeping the old delta there serves the paths the two
//     bases differ by from a base that has been replaced underneath it, which
//     is the staleness TestCoordinatorRefusesACommitLayerOverAMovedBase pins.
//     So the first clause below refuses the pin outright for generationID 0
//     and the bounded recomposition path (recomposeOverAdvancedBase) keeps
//     that regime exactly as it was.
//
// Two further refusals keep the pin honest in the regime it does apply to:
//
//   - the routed layer's identity must differ from the one a build would mint
//     now in the BASE fields alone. The probe re-renders the current identity
//     over the routed row's OWN tree and substitutes the routed row's base
//     fields, so a configuration change, a cohort change, an extractor bump or
//     a resolver bump all fail it and fall through to a rebuild against the
//     base that is current. Substituting the row's tree rather than the
//     sample's is deliberate: a checkout whose own tree moved still pins, and
//     rebuilds its delta against the base it is pinned to, which is what keeps
//     its ancestry — and its reuse cache — stable across its own commits.
//
//   - a base a sweep has asked this coordinator to release is never pinned
//     again. That is the bound: the pin holds until the superseded-chain
//     retention window decides the base must go, and the recomposition that
//     follows installs the replacement stack in ONE compare-and-set
//     (installStack) before the old one is given up, so the base is recomposed
//     off BEFORE it retires and the checkout never serves a torn route for the
//     length of the rebuild.
//
// It reports the base the cycle should use and whether that is a pin. Every
// refusal — a read that failed, a row that is gone, anything that is not
// exactly this shape — returns the family's current base unchanged, which is
// the behaviour this path replaced.
func (c *CheckoutCoordinator) pinRoutedBase(
	ctx context.Context, base primaryBase, route store_sqlite.CheckoutRoute,
) (primaryBase, bool) {
	pinned, ok, err := c.reconcilePinnedBaseFor(ctx, base, route)
	if err != nil {
		c.logger.Debug("checkout coordinator: could not resolve the routed base pin",
			zap.String("checkout", c.checkoutID), zap.Error(err))
	}
	if !ok {
		c.notePinnedBase(0)
		return base, false
	}
	c.notePinnedBase(pinned.generationID)
	return pinned, true
}

// pinnedBaseFor is the strict routed-view decision used by mutation admission.
// A writable snapshot must still name a complete active pair; pending routes
// are considered only by reconcilePinnedBaseFor after admission withdraws DIRTY.
func (c *CheckoutCoordinator) pinnedBaseFor(
	ctx context.Context, base primaryBase, route store_sqlite.CheckoutRoute,
) (primaryBase, bool, error) {
	var out primaryBase
	if route.State != store_sqlite.RouteActive || route.GraphID != base.graphID ||
		route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0 {
		return out, false, nil
	}
	return c.validatedPinnedCommitBaseFor(ctx, base, route.CommitGenerationID)
}

// reconcilePinnedBaseFor also recognizes the exact route shape Prepare leaves
// while a source mutation is pending: COMMIT is retained, DIRTY is withdrawn,
// and no incomplete route is active or servable. The shared validation below
// still proves that the retained COMMIT was built against a live pinned base.
func (c *CheckoutCoordinator) reconcilePinnedBaseFor(
	ctx context.Context, base primaryBase, route store_sqlite.CheckoutRoute,
) (primaryBase, bool, error) {
	if route.State == store_sqlite.RouteActive {
		return c.pinnedBaseFor(ctx, base, route)
	}
	var out primaryBase
	if route.State != store_sqlite.RoutePending || route.GraphID != base.graphID ||
		route.CommitGenerationID <= 0 || route.DirtyGenerationID != 0 {
		return out, false, nil
	}
	return c.validatedPinnedCommitBaseFor(ctx, base, route.CommitGenerationID)
}

// validatedPinnedCommitBaseFor proves that one routed COMMIT still names an
// independently servable dedicated base with the exact identity it was built
// against. Both mutation admission and reconcile use this authority; only the
// accepted route shapes differ between their callers.
func (c *CheckoutCoordinator) validatedPinnedCommitBaseFor(
	ctx context.Context, base primaryBase, commitGenerationID int64,
) (primaryBase, bool, error) {
	var out primaryBase
	if base.generationID <= 0 || base.pinned {
		// The legacy regime has no immutable ancestor to stay on, and a base
		// that is already a pin is not re-pinned.
		return out, false, nil
	}
	commitRow, found, err := c.catalog.GetViewGeneration(ctx, commitGenerationID)
	if err != nil {
		return out, false, err
	}
	if !found || !servableGeneration(commitRow.State) ||
		commitRow.OwnerKind != checkoutLayerOwnerKind ||
		commitRow.GenerationKind != CommitLayerGenerationKind ||
		commitRow.GraphID != base.graphID ||
		commitRow.BaseGenerationID <= 0 ||
		commitRow.BaseGenerationID == base.generationID {
		return out, false, nil
	}
	if commitRow.BaseGenerationID == c.releaseRequestedBasePin() {
		return out, false, nil
	}
	probe := c.commitIdentity(base, commitRow.TreeOID)
	probe.BaseGenerationID = commitRow.BaseGenerationID
	probe.LowerViewFingerprint = commitRow.LowerViewFingerprint
	if !commitLayerMatches(commitRow, probe) {
		return out, false, nil
	}
	baseRow, found, err := c.catalog.GetViewGeneration(ctx, commitRow.BaseGenerationID)
	if err != nil {
		return out, false, err
	}
	if !found || !servableGeneration(baseRow.State) ||
		baseRow.GenerationKind != DedicatedBaseGenerationKind ||
		baseRow.GraphID != base.graphID || baseRow.TreeOID == "" ||
		baseRow.TreeOID != commitRow.LowerViewFingerprint {
		// The generation is gone, retiring, or is not the tree this delta was
		// diffed from. Composing over it is no longer the identity the route
		// claims, so the cycle goes to the base the family is on.
		return out, false, nil
	}
	return primaryBase{
		graphID:      base.graphID,
		generationID: baseRow.GenerationID,
		treeOID:      baseRow.TreeOID,
		pinned:       true,
	}, true, nil
}

// notePinnedBase records what this checkout's route is composed over, for the
// retirement sweep to read. See the basePinned field.
func (c *CheckoutCoordinator) notePinnedBase(generationID int64) {
	c.mu.Lock()
	c.basePinned = generationID
	c.mu.Unlock()
}

// PinnedBaseGeneration reports the replaced committed base this checkout's
// route is still composed over, 0 when it is on the family's current one.
//
// The retirement sweep calls it to decide what it may OFFER, never what it may
// delete: the authority on that is the catalog's own reference guard, which
// refuses any generation another generation names as its base.
func (c *CheckoutCoordinator) PinnedBaseGeneration() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.basePinned
}

// RequestBaseRelease asks this coordinator to stop pinning one committed base
// so the generation can retire, and reports whether the request was new.
//
// It is the only way out of the pin, and it is a REQUEST: the cycle it wakes
// recomposes over the base the family is on now and installs the replacement
// stack in one compare-and-set, so the route serves the old, coherent pair for
// the whole of the rebuild and the base is released only once there is
// something to replace it with. Nothing here retires anything; the sweep that
// asked comes back for the generation on its next pass.
//
// The same generation asked for twice is not a new request, so an hourly sweep
// that keeps finding the pin does not keep re-signalling a recomposition that
// is already owed.
func (c *CheckoutCoordinator) RequestBaseRelease(generationID int64, reason string) bool {
	if c == nil || generationID <= 0 {
		return false
	}
	c.mu.Lock()
	if c.basePinRelease == generationID {
		c.mu.Unlock()
		return false
	}
	c.basePinRelease = generationID
	c.mu.Unlock()
	c.Signal(reason)
	return true
}

// releaseRequestedBasePin is the generation a sweep has asked this coordinator
// to stop holding, 0 when none has.
func (c *CheckoutCoordinator) releaseRequestedBasePin() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.basePinRelease
}

// ensureRoute reads the checkout's route, installing one when the checkout has
// never been routed and repointing one that names a different graph.
//
// A route whose graph changed is reset to pending with both slots cleared
// rather than repointed slot by slot: its generations were built over another
// graph's corpus and compose over nothing here. The reset is a compare-and-set
// like every other route write, so a coordinator that loses it leaves the
// winner's route alone.
func (c *CheckoutCoordinator) ensureRoute(ctx context.Context, base primaryBase) (store_sqlite.CheckoutRoute, error) {
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return route, err
	}
	if !found {
		ctx, err = resumeImportBuildLane(ctx, true)
		if err != nil {
			return route, err
		}
		route = store_sqlite.CheckoutRoute{
			CheckoutID: c.checkoutID,
			GraphID:    base.graphID,
			State:      store_sqlite.RoutePending,
		}
		if err := c.catalog.UpsertCheckoutRoute(ctx, route); err != nil {
			return route, err
		}
		return route, nil
	}
	if route.GraphID == base.graphID {
		return route, nil
	}
	ctx, err = resumeImportBuildLane(ctx, true)
	if err != nil {
		return route, err
	}
	err = c.catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID:         c.checkoutID,
		GraphID:            base.graphID,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              store_sqlite.RoutePending,
	})
	if err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return route, errRouteMoved
		}
		return route, err
	}
	previousCommit, previousDirty := route.CommitGenerationID, route.DirtyGenerationID
	route.GraphID = base.graphID
	route.CommitGenerationID, route.DirtyGenerationID = 0, 0
	route.RouteEpoch++
	route.State = store_sqlite.RoutePending
	c.dropRetained(ctx, 0)
	// The working-tree cache goes with it. Every layer it holds was built over
	// a commit generation composed on the graph this checkout has just left, so
	// none of them can ever be routed here again.
	c.dropRetainedDirty(ctx, 0)
	c.offerRetire(ctx, previousDirty)
	c.offerRetire(ctx, previousCommit)
	return route, nil
}

// reconcileCommitSlot points the commit slot at a generation describing the
// checkout's HEAD tree over the primary's base, building one only when no
// generation with that identity can be reached.
func (c *CheckoutCoordinator) reconcileCommitSlot(
	ctx context.Context,
	base primaryBase,
	targetTree string,
	route *store_sqlite.CheckoutRoute,
	out *CheckoutCycle,
) (int64, error) {
	identity := c.commitIdentity(base, targetTree)
	key := generationIdentityKey(identity)

	// Already routed to exactly this state: the cheapest outcome, and the one
	// every poll takes on a checkout nobody has touched.
	if route.CommitGenerationID > 0 {
		row, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
		if err != nil {
			return 0, err
		}
		if found && servableGeneration(row.State) &&
			row.OwnerKind == checkoutLayerOwnerKind &&
			row.GenerationKind == CommitLayerGenerationKind {
			// A fresh coordinator can inherit route A while the filesystem
			// already names branch B. Preserve that persisted, still-servable
			// commit before resolving B; otherwise releaseCommit sees an empty
			// process-local cache and retires A, forcing a rebuild on the switch
			// back. Retaining by the row's own identity also handles a canonical
			// generation adopted from another checkout.
			rowKey := generationRowKey(row)
			if rowKey != key && c.adoptedCommitRowFor(row, key) {
				// Another checkout's layer this checkout adopted for exactly
				// this state (sharedCommit): filed under this checkout's key.
				rowKey = key
			}
			if rowKey != key {
				ctx, err = resumeImportBuildLane(ctx, true)
				if err != nil {
					return 0, err
				}
			}
			c.retainCommit(ctx, rowKey, row.GenerationID)
			if rowKey == key {
				return row.GenerationID, nil
			}
		}
	}

	var admissionErr error
	ctx, admissionErr = resumeImportBuildLane(ctx, true)
	if admissionErr != nil {
		return 0, admissionErr
	}
	previous := route.CommitGenerationID
	generationID, reused, err := c.resolveCommitLayer(ctx, base, targetTree)
	if err != nil {
		return 0, err
	}
	// The base half of the HEAD-move guard reconcileDirtySlot makes. A commit
	// layer takes as long as the tree it indexes, and the primary is free to
	// publish a new committed base while one is being built over the old one:
	// routing the result then serves this checkout's delta over a base the
	// family has left. The route keeps the pair it already holds — coherent,
	// over a generation that is retained for as long as a reader names it — and
	// the next cycle composes over the base that is current.
	if moved, err := c.baseMovedUnderCycle(ctx, base); err != nil || moved {
		if !reused {
			c.supersede(ctx, generationID)
			c.offerRetire(ctx, generationID)
		}
		if err != nil {
			return 0, err
		}
		c.rescheduleOnMovedBase(base, out)
		return 0, fmt.Errorf("%w: built for %s", errBaseMoved, base.treeOID)
	}
	if !reused {
		out.CommitBuilt = true
	}
	if err := c.moveCommitSlot(ctx, route, generationID); err != nil {
		if !reused {
			c.supersede(ctx, generationID)
			c.offerRetire(ctx, generationID)
		}
		// A cached generation is another cycle's work, not this one's, so a
		// lost flip leaves it exactly as it was.
		return 0, err
	}
	if reused {
		out.CommitReused = true
	}
	c.retainCommit(ctx, key, generationID)
	c.releaseCommit(ctx, previous)
	return generationID, nil
}

// baseMovedUnderCycle re-reads the family's primary base and reports whether it
// is still the one the cycle was planned against.
//
// It is one metadata read, and it is taken on the build path only — after a
// layer has been built or pulled from the cache, before anything is routed.
// Comparing the whole primaryBase rather than the generation id alone covers
// the regime where the base has no published generation: there the base is the
// owner checkout's recorded committed tree, which moves without any pointer
// moving with it.
//
// A PINNED base is the one case where "the family is on another base" is not
// the question. The cycle chose that base deliberately (pinRoutedBase), the
// family having moved past it is the premise rather than a hazard, and what
// has to still hold is what makes composing over it correct: the generation is
// still servable and still names the tree the delta was diffed from. Anything
// else — it is retiring, it is gone, it is not that tree — and the layer this
// cycle built composes over nothing the route can claim, so it is refused
// exactly as a moved base is. Only a value pinRoutedBase minted takes this
// arm; graphBase never sets the flag, so every other caller keeps the exact
// comparison it always had.
func (c *CheckoutCoordinator) baseMovedUnderCycle(ctx context.Context, base primaryBase) (bool, error) {
	if base.pinned {
		if base.generationID <= 0 || base.treeOID == "" {
			return true, nil
		}
		if base.generationID == c.releaseRequestedBasePin() {
			return true, nil
		}
		row, found, err := c.catalog.GetViewGeneration(ctx, base.generationID)
		if err != nil {
			return false, err
		}
		return !found || !servableGeneration(row.State) || row.TreeOID != base.treeOID, nil
	}
	current, err := c.primaryBase(ctx)
	if err != nil {
		return false, err
	}
	return current != base, nil
}

// resolveCommitLayer reaches a commit generation describing one tree over one
// base, re-routing a retained generation with that identity when the cache
// holds one and building otherwise.
//
// It touches the route not at all. That is what lets a transition build a
// whole stack for a graph the checkout is not being served from yet and only
// then decide, in one write, that it is.
func (c *CheckoutCoordinator) resolveCommitLayer(
	ctx context.Context, base primaryBase, targetTree string,
) (generationID int64, reused bool, err error) {
	identity := c.commitIdentity(base, targetTree)
	if cached, ok := c.cachedCommit(ctx, generationIdentityKey(identity)); ok {
		return cached, true, nil
	}
	if stored, ok := c.storedCommit(ctx, identity); ok {
		return stored, true, nil
	}
	started := time.Now()
	var baseReader LayerBase = c.store.AtGeneration(base.generationID)
	if base.generationID > 0 {
		view, openErr := c.baseViewMaterializer().MaterializeRefView(ctx, base.graphID, base.generationID)
		if openErr != nil {
			return 0, false, fmt.Errorf("indexer: open primary generation %d: %w", base.generationID, openErr)
		}
		defer view.Close()
		// Closure reads need the complete committed ancestry, and so do the
		// ref-fact hints: they are scoped to the same materialized stack the
		// structural reads compose rather than to the base generation alone,
		// whose handle answers with that one generation's rows — none, for
		// every generation a sparse build produces. See ancestryLayerBase.
		baseReader = c.ancestryLayerBase(view)
	}
	generationID, report, err := c.builder.BuildCommitLayer(ctx, CommitLayerRequest{
		Identity:      identity,
		Base:          baseReader,
		RepoDir:       c.root,
		BaseTreeOID:   base.treeOID,
		TargetTreeOID: targetTree,
		RootPath:      c.root,
		RepoPrefix:    c.repoPrefix,
		WorkspaceID:   c.workspaceID,
		ProjectID:     c.projectID,
	})
	viewmetrics.Observe(viewmetrics.CoordinatorBuildSeconds, time.Since(started), viewmetrics.SlotCommit)
	if err != nil {
		return 0, false, err
	}
	if report.ClosureTruncated {
		c.logger.Warn("checkout coordinator: commit layer closure truncated",
			zap.String("checkout", c.checkoutID), zap.Int64("generation", generationID),
			zap.Int("cap", report.ClosureCap))
	}
	return generationID, false, nil
}

// moveCommitSlot points the commit slot at a different generation, dropping
// the working-tree slot in the same write.
//
// A routed dirty generation describes the working tree over the commit layer
// it was built against. Once the commit slot names a different layer, that
// generation over the new one is a state of the world the checkout was never
// in — one branch's uncommitted edits laid over another branch's tree — and
// the route would still report itself ready to serve it for the whole of the
// rebuild that follows. Clearing both pointers in one compare-and-set makes
// the route say what is true instead: it is mid-build, and the reader takes
// its base-corpus fallback until the working-tree layer has been rebuilt.
//
// The slot-at-a-time flip is kept for the case it is safe in: a route with no
// dirty generation has nothing to tear.
func (c *CheckoutCoordinator) moveCommitSlot(
	ctx context.Context,
	route *store_sqlite.CheckoutRoute,
	generationID int64,
) error {
	if route.DirtyGenerationID <= 0 {
		return c.flip(ctx, route, store_sqlite.RouteSlotCommit, generationID)
	}
	err := c.catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID:         c.checkoutID,
		ExpectedRouteEpoch: route.RouteEpoch,
		GraphID:            route.GraphID,
		CommitGenerationID: generationID,
		DirtyGenerationID:  0,
		State:              store_sqlite.RoutePending,
	})
	if err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return fmt.Errorf("%w: %s slot", errRouteMoved, store_sqlite.RouteSlotCommit)
		}
		return err
	}
	dropped := route.DirtyGenerationID
	route.RouteEpoch++
	route.State = store_sqlite.RoutePending
	route.CommitGenerationID = generationID
	route.DirtyGenerationID = 0
	c.rememberRoutedDirty(0)
	// Released rather than retired: the layer describes a working tree over
	// the commit generation it names, and that pair is exactly what a switch
	// back to this branch composes again. reconcileDirtySlot filed it in the
	// reuse cache before this write; a layer the cache is not holding is
	// retired here as it always was.
	c.releaseDirty(ctx, dropped)
	return nil
}

// clearDirtySlot withdraws the working-tree slot and offers what it named for
// collection.
//
// moveCommitSlot closes the window this exists for whenever the coordinator
// moves the commit slot itself. What is left is a route some other writer left
// naming a working-tree layer built over a commit layer the route no longer
// names — a store written by a binary that flipped the two slots separately,
// or a slot-at-a-time flip from another surface. The rebuild that follows
// would serve that pair for its whole duration, so the slot goes first.
func (c *CheckoutCoordinator) clearDirtySlot(ctx context.Context, route *store_sqlite.CheckoutRoute) error {
	var admissionErr error
	ctx, admissionErr = resumeImportBuildLane(ctx, false)
	if admissionErr != nil {
		return admissionErr
	}
	err := c.catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         c.checkoutID,
		Slot:               store_sqlite.RouteSlotDirty,
		GenerationID:       0,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              store_sqlite.RoutePending,
	})
	if err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return fmt.Errorf("%w: %s slot", errRouteMoved, store_sqlite.RouteSlotDirty)
		}
		return err
	}
	dropped := route.DirtyGenerationID
	route.RouteEpoch++
	route.State = store_sqlite.RoutePending
	route.DirtyGenerationID = 0
	c.rememberRoutedDirty(0)
	c.releaseDirtyChain(ctx, dropped, 0)
	noteRetirementReferencesReleased(dropped)
	return nil
}

// reconcileDirtySlot points the dirty slot at a generation describing the
// working tree over the commit layer beneath it.
//
// The comparison that decides whether to build is between the working tree's
// fingerprint and the one recorded on the routed generation. BuildDirtyLayer
// stamps that fingerprint into lower_view_fingerprint, which is the honest
// column for it: the dirty layer's lower view IS the working tree it was read
// from, and the fingerprint is what identifies it. The routed generation's
// base is checked alongside, because a commit layer that was just rebuilt puts
// the same working tree over different content underneath — and a routed
// generation that fails that check is withdrawn before the rebuild starts
// rather than after it ends, since the pair it forms with the routed commit
// generation describes no state the checkout has ever been in.
//
// targetTree is the committed tree the commit slot was settled against, and the
// sample has to agree with it before anything is built or kept. A commit layer
// takes as long as the tree it indexes, and a checkout is free to commit while
// one is being built for it: the sample taken afterwards then describes the
// working tree of a HEAD the layer beneath knows nothing about, and a layer
// built from it would carry no payload for the paths the new commit moved —
// they are committed, not dirty — so the pair would serve the old tree's
// content as the checkout's current state. The cycle reschedules instead, and
// the next one rebuilds the commit slot for the head the checkout is really at.
func (c *CheckoutCoordinator) reconcileDirtySlot(
	ctx context.Context,
	commitGeneration int64,
	targetTree string,
	route *store_sqlite.CheckoutRoute,
	out *CheckoutCycle,
) (slotErr error) {
	// The plan's own reads between the cycle's admission and the build are
	// lapped (the sample, the routed row's chain check, the reuse lookup,
	// the parent selection) and logged once the slot builds.
	planStarted := time.Now()
	var planLaps []zap.Field
	planLap := func(name string) {
		now := time.Now()
		planLaps = append(planLaps, zap.Duration(name, now.Sub(planStarted)))
		planStarted = now
	}
	// The cycle's shared sample, unless this cycle just built a commit layer:
	// the checkout is free to commit while one builds, so the working tree is
	// sampled again after it.
	sample, err := c.cycleSample(ctx)
	if out.CommitBuilt {
		sample, err = c.sampler.Sample(ctx)
	}
	if err != nil {
		return fmt.Errorf("indexer: sample %s: %w", c.root, err)
	}
	c.noteDirtyFingerprint(sample.Fingerprint)
	if sample.HeadTree != targetTree {
		out.Rescheduled, out.rescheduledBy = true, stallReasonHeadMoved
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeHeadMoved)
		c.logger.Debug("checkout coordinator: the checkout committed under the cycle",
			zap.String("checkout", c.checkoutID),
			zap.String("built_for", targetTree), zap.String("now_at", sample.HeadTree))
		c.Signal("the checkout moved to another commit under the cycle")
		return nil
	}
	planLap("sample")
	finishPlanning, err := c.prepareImportPlan(ctx, commitGeneration, sample, *route)
	if err != nil {
		return err
	}
	defer func() {
		if ctx.Err() == nil {
			_, err := resumeImportBuildLane(ctx, false)
			if slotErr == nil {
				slotErr = err
			}
		}
		finishPlanning()
	}()
	key := c.dirtySampleKey(route.GraphID, commitGeneration, sample)
	if route.DirtyGenerationID > 0 {
		row, found, err := c.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
		if err != nil {
			return err
		}
		servable := found && servableGeneration(row.State)
		rooted := false
		if servable {
			// Rooted at the routed commit generation: built directly over it,
			// or a chain of this checkout's working-tree generations ending at
			// it. A chain that passes keeps serving exactly like a direct
			// layer; one that does not is foreign or unservable and is
			// withdrawn below as before.
			if rooted, err = c.dirtyRootedAt(ctx, row, commitGeneration); err != nil {
				return err
			}
		}
		if servable {
			// Whatever the route already names is filed in the reuse cache
			// before anything replaces it — including a layer a fresh
			// coordinator inherited and has no other record of. Without this
			// the undo that follows the next edit retires the very payload it
			// is about to ask for, exactly as reconcileCommitSlot's
			// route-preserving arm exists to stop on the commit half. A rooted
			// layer is filed under its logical key (logicalDirtyKey), which is
			// what this cycle's lookup renders whatever its physical parent.
			filed := generationRowKey(row)
			if rooted {
				filed = logicalDirtyKey(row, commitGeneration)
			}
			c.retainDirty(ctx, filed, row.GenerationID)
		}
		if rooted {
			// A layer over the routed commit generation describes a state the
			// checkout really was in, so it keeps serving while the working
			// tree it no longer matches is rebuilt underneath the route.
			if row.LowerViewFingerprint == sample.Fingerprint {
				c.rememberRoutedDirty(row.GenerationID)
				out.DirtyGenerationID = row.GenerationID
				return nil
			}
		} else if err := c.clearDirtySlot(ctx, route); err != nil {
			return err
		}
	}

	planLap("routed")
	// The reuse path, and the whole of dirty-layer reuse as it ships — an
	// in-process, fingerprint-keyed cache with no catalog-backed,
	// survive-restart half: a working tree that has come back to a state this
	// coordinator already described over this commit layer is re-routed rather
	// than re-indexed. The route flip below is the only write it makes.
	if cached, ok := c.cachedDirty(ctx, key); ok {
		previous := route.DirtyGenerationID
		if err := c.flip(ctx, route, store_sqlite.RouteSlotDirty, cached); err != nil {
			// A cached generation is another cycle's work, not this one's, so
			// a lost flip leaves it exactly as it was.
			return err
		}
		markPublicationPhase(ctx, PublicationRouteFlipped)
		out.DirtyReused = true
		out.DirtyGenerationID = cached
		c.retainDirty(ctx, key, cached)
		c.releaseImportPublicationLane(ctx)
		c.releaseDirtyChain(ctx, previous, cached)
		return nil
	}

	planLap("reuse_lookup")
	selection := c.selectDirtyParentForSlot(ctx, *route, commitGeneration, sample, out)
	planLap("parent_selection")
	for _, l := range out.PlanLaps {
		planLaps = append(planLaps, zap.Duration(l.Name, l.Duration))
	}
	c.logger.Info("checkout coordinator: dirty slot plan",
		append([]zap.Field{zap.String("checkout", c.checkoutID), zap.Int("dirty_entries", len(sample.Entries)),
			zap.Int64("parent", selection.Parent), zap.Int("parent_depth", selection.Depth)}, planLaps...)...)
	endDelta := editDeltaBegin() // a background stack pre-warm yields to the build
	generationID, builtKey, err := c.buildDirtyLayerForSlot(ctx, route.GraphID, commitGeneration, selection, sample, out)
	endDelta()
	if err != nil {
		return err
	}
	if generationID == 0 {
		// The route still names the last coherent state, which is the point: a
		// stale view of a real state beats a torn view of a state that never was.
		out.Rescheduled, out.rescheduledBy = true, stallReasonTornByMotion
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeRescheduled)
		c.Signal("the working tree moved under two builds")
		return nil
	}
	out.DirtyBuilt = true
	previous := route.DirtyGenerationID
	if err := c.flip(ctx, route, store_sqlite.RouteSlotDirty, generationID); err != nil {
		c.supersede(ctx, generationID)
		c.deferRetire(generationID, "lost route flip")
		return err
	}
	markPublicationPhase(ctx, PublicationRouteFlipped)
	out.DirtyGenerationID = generationID
	// The built layer is filed under the key the BUILD stamped rather than
	// the one this cycle looked it up by, and the one the route leaves is
	// released into the cache rather than retired: together they are what
	// makes the NEXT undo free.
	//
	// The two keys are the same whenever the checkout held still, and when it
	// did not the build's is the truthful one: the payload describes the tree
	// the builder sampled, so that is the state a future cycle may re-route it
	// for. Filing under this cycle's key would put an entry in the cache that
	// the row cannot render — dead weight in a bounded cache, evicting an
	// entry that could still be hit.
	c.retainDirty(ctx, builtKey, generationID)
	c.releaseImportPublicationLane(ctx)
	c.releaseDirtyChain(ctx, previous, generationID)
	if out.DirtyBatchRemaining > 0 {
		// One batch of a large working tree: routed (it is a state closer to
		// the working tree than what the route named, and the next batch
		// stands on it), but not the working tree, so no ticket completes on
		// it and no compaction folds a chain that is still growing.
		out.Rescheduled = true
		// An import folds its own chain by copy whenever it reaches the
		// compaction depth, so the next file always chains and the import
		// never falls back to a direct build of what it already imported.
		c.foldImportChain(ctx, commitGeneration, route, out)
		c.logger.Info("checkout coordinator: working tree built in batches",
			zap.String("checkout", c.checkoutID),
			zap.Int64("generation", generationID),
			zap.Int("chain_depth", out.DirtyChainDepth),
			zap.Int("remaining_paths", out.DirtyBatchRemaining))
		c.Signal("the next batch of the working tree")
		return nil
	}
	out.CompactionScheduled = c.dirtyChainCompactionDue(*out) || (out.DirtyBuilt && c.foldDue(ctx, *out))
	c.signalDirtyOutpaced(*out)
	return nil
}

// signalDirtyOutpaced wakes the next cycle after a cycle routed a generation
// that describes a sample the working copy had already left (out.DirtyOutpaced).
// The route now names a state the checkout really was in, and the saves since
// are the next cycle's change set over it. A refresh ticket admitted after
// this generation's sample cannot complete on it (completeCheckoutRefreshTickets
// samples after the ticket), so a waiting ticket's cycle runs at once instead
// of after the window.
func (c *CheckoutCoordinator) signalDirtyOutpaced(out CheckoutCycle) {
	if !out.DirtyOutpaced {
		return
	}
	c.Signal(dirtyOutpacedReason)
	if c.checkoutRefreshHighWater() != 0 {
		c.SignalDemand(dirtyOutpacedReason)
	}
}

// dirtyOutpacedReason is the wake a cycle that published a sample the working
// copy had already left raises for the newer state.
const dirtyOutpacedReason = "the working tree moved past the published sample"

// dirtySampleKey renders the reuse key of the working-tree layer a build from
// one sample would produce, without building it.
//
// It is the cache key, and it is assembled from the coordinator's half
// (dirtyIdentity: who owns the layer, what it sits on, and the configuration
// and cohort it is built under) plus the builder's own stamping of the sample
// (StampDirtyLayerIdentity). Using the builder's function rather than a second
// copy of it is the point: the key a lookup renders and the identity a build
// stamps cannot drift apart.
//
// A sample the builder would refuse to stamp — an unborn branch, with no tree
// and no fingerprint — renders an empty key, which cachedDirty and retainDirty
// both decline rather than treat as an identity.
func (c *CheckoutCoordinator) dirtySampleKey(
	graphID string, commitGeneration int64, sample gitstate.DirtySnapshot,
) string {
	if sample.HeadTree == "" || sample.Fingerprint == "" {
		return ""
	}
	return generationIdentityKey(StampDirtyLayerIdentity(c.dirtyIdentity(graphID, commitGeneration), sample))
}

// buildDirtyLayerOver builds the working-tree layer over one commit
// generation, and reports 0 with a nil error when two attempts in a row were
// torn by edits landing under them.
//
// Two attempts, no more. Each build re-samples the checkout itself and refuses
// to publish a payload the working tree has already moved past, so the second
// attempt is the "one more try against what it is now" the refusal is worth. A
// checkout under a stream of edits would invalidate every attempt, and a caller
// that kept trying would spin instead of letting the next quiet window decide.
//
// Like resolveCommitLayer it writes nothing to the route: what the checkout
// reads is the caller's decision.
//
// The second return is the reuse key of the generation that was actually
// published — rendered from the sample the BUILD took, not from the caller's
// earlier one. A caller files its result in the working-tree reuse cache under
// this key and never under a key of its own: the checkout is free to move
// between the two samples, and an entry filed under a key its row does not
// render is an entry no lookup can hit.
func (c *CheckoutCoordinator) buildDirtyLayerOver(
	ctx context.Context, graphID string, commitGeneration int64, out *CheckoutCycle,
) (int64, string, error) {
	built, err := c.buildDirtyLayerAttempts(ctx, graphID, commitGeneration, dirtyParentSelection{}, nil, "")
	if err == nil && built.GenerationID > 0 && out != nil {
		// What a cycle's own build reports (buildDirtyLayerPreferChain):
		// an outpaced publication wakes the next cycle once it is routed, and
		// tickets complete against the sample the generation describes.
		out.DirtyOutpaced, out.dirtySample = built.Outpaced, built.Sample
	}
	return built.GenerationID, built.Key, err
}

// dirtyLayerBuild is what buildDirtyLayerAttempts produced: the published
// generation and its logical reuse key, or the chained delta's refusal reason,
// and the physical work of the attempt that decided it.
type dirtyLayerBuild struct {
	GenerationID int64
	Key          string
	Reason       string
	Work         *GenerationWorkCounters
	// Remaining is BuildReport.BatchRemaining: > 0 when the build carried one
	// batch of a larger change set.
	Remaining int
	// Outpaced is BuildReport.outpacedSample: the generation describes its
	// sample, which the working copy had already moved past.
	Outpaced bool
	// Sample is the sample the generation describes (BuildReport.dirtySample).
	Sample gitstate.DirtySnapshot
}

// buildDirtyLayerAttempts is buildDirtyLayerOver with a physical parent: a
// selection with Parent > 0 builds the working tree as a delta over that
// published working-tree generation (its composed view is the base reader,
// leased for the whole build, and it is the new row's BaseGenerationID), and a
// zero selection builds direct over the commit generation. first, when set, is
// the cycle's own sample and is the first attempt's change set; a second
// attempt samples afresh.
//
// A chained attempt the builder refuses returns the reason with no error and
// no generation, before anything was written; the caller then builds direct.
// fallbackReason is carried into a direct build's report.
//
// The key returned is the LOGICAL reuse key (the identity over the commit
// generation) whatever the physical parent, which is what the undo cache
// looks states up by. The build's work counters are reported as well. A
// chained parent whose view can no longer be opened (retired or unservable
// since it was selected) is a no_parent refusal, not a cycle failure: the
// caller builds direct instead.
func (c *CheckoutCoordinator) buildDirtyLayerAttempts(
	ctx context.Context, graphID string, commitGeneration int64,
	parent dirtyParentSelection, first *gitstate.DirtySnapshot, fallbackReason string,
	options ...func(*DirtyLayerRequest),
) (dirtyLayerBuild, error) {
	baseGeneration := commitGeneration
	if parent.Parent > 0 {
		baseGeneration = parent.Parent
	}
	baseOpenStarted := time.Now()
	dirtyBase, releaseBase, err := c.generationLayerReader(ctx, baseGeneration)
	baseOpen := time.Since(baseOpenStarted)
	if err != nil {
		if parent.Parent > 0 && ctx.Err() == nil {
			return dirtyLayerBuild{Reason: dirtyChainFallbackNoParent}, nil
		}
		return dirtyLayerBuild{}, err
	}
	defer func() {
		if releaseBase != nil {
			releaseBase()
		}
	}()
	identity := c.dirtyIdentity(graphID, commitGeneration)
	identity.BaseGenerationID = baseGeneration
	// The committed state's language census is read only when the build's
	// own files leave an enrichable language below the admission floor (the
	// floor check calls it): counting the committed ancestry is a grouped scan
	// of every full generation beneath the chain, tens of seconds on a cold
	// store, and an ordinary edit's own files clear the floor without it.
	var baseCensus func(context.Context) (map[string]int, error)
	if c.builder != nil && c.builder.Semantic != nil {
		baseCensus = func(ctx context.Context) (map[string]int, error) {
			return c.checkoutLanguageCensusContext(ctx, commitGeneration)
		}
	}
	var stamped GenerationIdentity
	var work *GenerationWorkCounters
	for attempt := 0; attempt < 2; attempt++ {
		req := DirtyLayerRequest{
			Identity:            identity,
			Base:                dirtyBase,
			CheckoutRoot:        c.root,
			RepoPrefix:          c.repoPrefix,
			WorkspaceID:         c.workspaceID,
			ProjectID:           c.projectID,
			buildBarrier:        c.dirtyBarrier,
			stamped:             &stamped,
			Sampler:             c.sampler,
			chainFallbackReason: fallbackReason,
			baseCensusFunc:      baseCensus,
			baseOpen:            baseOpen,
			narrowMotion:        c.narrowTreeMoveAbort,
			// A large working-tree change is imported file by file
			// (checkout_import.go); the builder judges that by the change set
			// it carries, so a delta over a parent that covers the dirty set
			// stays one delta.
			importLarge:    true,
			continueImport: c.importInProgress(ctx, parent.Parent),
			// Enrichment after publication (enrichment_followup.go).
			deferEnrichment: c.defersEnrichment(),
		}
		for _, option := range options {
			option(&req)
		}
		if attempt == 0 {
			req.before = first
		}
		if parent.Parent > 0 {
			req.parent, req.parentManifest, req.parentDepth = parent.Parent, parent.Manifest, parent.Depth
		}
		started := time.Now()
		generationID, report, err := c.builder.BuildDirtyLayer(ctx, req)
		viewmetrics.Observe(viewmetrics.CoordinatorBuildSeconds, time.Since(started), viewmetrics.SlotDirty)
		if report.Work != nil {
			work = report.Work
		}
		if err == nil {
			stamped.BaseGenerationID = commitGeneration
			if report.outpacedSample {
				c.noteBuildMotion()
			}
			return dirtyLayerBuild{
				GenerationID: generationID, Key: generationIdentityKey(stamped), Work: work,
				Remaining: report.BatchRemaining, Outpaced: report.outpacedSample, Sample: report.dirtySample,
			}, nil
		}
		var fallback *DirtyChainFallbackError
		if errors.As(err, &fallback) {
			return dirtyLayerBuild{Reason: fallback.Reason, Work: work}, nil
		}
		if !errors.Is(err, ErrDirtySnapshotChanged) || errors.Is(err, errImportPreparationChanged) {
			// A build that died part way left its generation failed; it is
			// owed a retirement like a torn attempt, so a canceled compaction
			// or a failed edit leaks no payload.
			c.deferFailedGeneration(ctx, generationID)
			return dirtyLayerBuild{Work: work}, err
		}
		// The refused attempt is a whole payload for a state the checkout has
		// already left. confirmDirtySnapshotWith superseded it, and nothing will
		// ever route it, so it is owed a retirement — an editor saving over a
		// build would otherwise leak one payload per save for the life of the
		// daemon. The retirement runs off the cycle, from the backlog.
		var torn *DirtySnapshotChangedError
		if errors.As(err, &torn) {
			c.deferRetire(torn.GenerationID, "torn working-tree build")
		}
		// The tree moved under the build (a contract-input correction is not a
		// move): the checkout's edits defer their enrichment until it settles
		// (enrichmentDeferredByMotion), so the next attempt is not torn by it.
		if !errors.Is(err, errContractInputsChanged) {
			c.noteBuildMotion()
		}
		// A pass that read a file saved after the sample (editDeltaReadMoved)
		// or a contract-input correction abandoned its generation to failed
		// before the fence; it is owed the same retirement as a torn one.
		if errors.Is(err, errContractInputsChanged) || errors.Is(err, errFileVersionChanged) {
			c.deferFailedGeneration(ctx, generationID)
		}
		if errors.Is(err, errContractInputsChanged) && attempt+1 < 2 {
			// A source correction invalidated construction-time layer masks.
			// Re-sampling the checkout with the same reader cannot repair that;
			// rebuild the materialized ancestry for the existing bounded retry.
			releaseBase()
			releaseBase = nil
			baseOpenStarted = time.Now()
			freshBase, freshRelease, openErr := c.generationLayerReader(ctx, baseGeneration)
			if openErr != nil {
				return dirtyLayerBuild{Work: work}, openErr
			}
			dirtyBase, releaseBase = freshBase, freshRelease
			baseOpen = time.Since(baseOpenStarted)
		}
	}
	return dirtyLayerBuild{Work: work}, nil
}

// commitLayerReader is the reader a dirty-layer build computes its affected
// closure against: the checkout's commit generation and its complete ancestry.
// The caller must release the pinned view after every build attempt has ended.
func (c *CheckoutCoordinator) commitLayerReader(ctx context.Context, commitGeneration int64) (LayerBase, func(), error) {
	return c.generationLayerReader(ctx, commitGeneration)
}

// generationLayerReader is the reader a working-tree build stands on: the
// composed view of one published generation and its whole ancestry — the
// commit generation for a direct build, a working-tree parent (its chain, the
// commit generation and the base beneath) for a chained one. The view's lease
// is held until the returned release runs.
func (c *CheckoutCoordinator) generationLayerReader(ctx context.Context, commitGeneration int64) (LayerBase, func(), error) {
	row, found, err := c.catalog.GetViewGeneration(ctx, commitGeneration)
	if err != nil {
		return nil, nil, fmt.Errorf("indexer: read commit generation %d: %w", commitGeneration, err)
	}
	if !found || !servableGeneration(row.State) {
		return nil, nil, fmt.Errorf("indexer: commit generation %d is not servable", commitGeneration)
	}
	view, err := c.baseViewMaterializer().MaterializeRefView(ctx, row.GraphID, commitGeneration)
	if err != nil {
		return nil, nil, fmt.Errorf("indexer: open commit generation %d: %w", commitGeneration, err)
	}
	// The hints are scoped to the same ancestry as the structural reads — this
	// commit generation and everything under it — rather than to the one
	// generation beneath it. See ancestryLayerBase.
	return c.ancestryLayerBase(view), view.Close, nil
}

// baseViewMaterializer is the checkout's long-lived materializer for the
// layer below its builds (see baseViews).
func (c *CheckoutCoordinator) baseViewMaterializer() *graphview.Materializer {
	c.baseViewsOnce.Do(func() {
		c.baseViews = &graphview.Materializer{
			Store: c.store, Catalog: c.catalog, Leases: c.leases, Logger: c.logger,
		}
		// A generation that stopped being servable leaves the dirty chain's
		// kept layers too (edit_delta_contract_cache.go).
		store := c.store
		c.baseViews.OnForgetGeneration(func(generation int64) {
			forgetEditDeltaChainLayer(store, generation)
		})
	})
	return c.baseViews
}

// ancestryLayerBase is the LayerBase a build reads a materialized view
// through: the view's own structural reader, plus reference-fact hints scoped
// to the SAME materialized ancestry that reader composes.
//
// There is one definition, and both call sites — the commit-layer build's
// primary base (resolveCommitLayer) and the working-tree build's commit base
// (commitLayerReader) — go through it, so neither can drift back into handing
// a build a single generation handle. That is not a style preference: a handle
// pinned to one generation answers the closure's fact question from one layer
// of a stack the structural reads compose in full, and since no sparse
// generation writes facts at all, it answers "no facts" for every file in the
// repository. See ancestryRefFacts.
func (c *CheckoutCoordinator) ancestryLayerBase(view *graphview.RepoView) LayerBase {
	base := commitLayerBase{
		Reader: view.Reader, facts: newAncestryRefFacts(c.store, view),
		cloneSources: view.GenerationSources(),
	}
	if view.ComposesBaseCorpus() {
		base.cloneCorpusBase = c.store.AtGeneration(graphview.BaseCorpusGeneration)
	}
	if !view.ComposesBaseCorpus() {
		base.stack = view.Generations()
	}
	return base
}

// flip repoints one slot under the route epoch this cycle read, and advances
// the caller's copy of the route so the next flip carries the epoch the
// database now holds.
func (c *CheckoutCoordinator) flip(
	ctx context.Context,
	route *store_sqlite.CheckoutRoute,
	slot store_sqlite.RouteSlot,
	generationID int64,
) error {
	// Warming an already sealed generation is read-only and may outlast an
	// interactive request. An import retains its checkout/preparation ownership
	// but gives back the physical lane until the guarded route mutation below.
	if lane, _ := ctx.Value(importBuildLaneKey{}).(*importBuildLane); lane != nil {
		if c.leases != nil {
			pin := c.leases.Acquire(generationID)
			defer pin.Release()
		}
		lane.leave()
	}
	c.prewarmRoute(ctx, route, slot, generationID)
	var admissionErr error
	ctx, admissionErr = resumeImportBuildLane(ctx, false)
	if admissionErr != nil {
		return admissionErr
	}
	// The builder publishes as its last step, so a generation reaching here is
	// already ready and PublishAndRoute — which publishes and then flips —
	// would refuse it for not being in the building state. The flip alone is
	// what is left of that pair for a caller holding a published generation.
	err := c.catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         c.checkoutID,
		Slot:               slot,
		GenerationID:       generationID,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              store_sqlite.RouteActive,
	})
	if err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return fmt.Errorf("%w: %s slot", errRouteMoved, slot)
		}
		return err
	}
	route.RouteEpoch++
	route.State = store_sqlite.RouteActive
	// Only the generation leaving the slot lost a reference.
	switch slot {
	case store_sqlite.RouteSlotCommit:
		noteRetirementReferencesReleased(route.CommitGenerationID)
		route.CommitGenerationID = generationID
	case store_sqlite.RouteSlotDirty:
		noteRetirementReferencesReleased(route.DirtyGenerationID)
		route.DirtyGenerationID = generationID
		c.rememberRoutedDirty(generationID)
	default:
		noteRetirementReferenceReleased()
	}
	return nil
}

// rememberRoutedDirty records which working-tree generation the route names.
func (c *CheckoutCoordinator) rememberRoutedDirty(generationID int64) {
	c.mu.Lock()
	c.routedDirty = generationID
	c.mu.Unlock()
}

// supersede marks a generation nothing will read. It is the answer to a lost
// route flip: the payload is whole and published, and the checkout it was
// built for is being served from something else.
func (c *CheckoutCoordinator) supersede(ctx context.Context, generationID int64) {
	if generationID <= 0 {
		return
	}
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeSuperseded)
	if err := c.store.MarkPayloadGenerationSuperseded(ctx, generationID); err != nil {
		c.logger.Debug("checkout coordinator: could not supersede an unrouted generation",
			zap.String("checkout", c.checkoutID),
			zap.Int64("generation", generationID), zap.Error(err))
	}
}

// --- the commit-layer reuse cache ---------------------------------------

// cachedCommit returns a retained generation for an identity, after confirming
// the catalog still holds it in a state that can be served. A generation that
// has gone is dropped from the cache rather than re-routed.
func (c *CheckoutCoordinator) cachedCommit(ctx context.Context, key string) (int64, bool) {
	c.mu.Lock()
	var generationID int64
	for _, entry := range c.retained {
		if entry.key == key {
			generationID = entry.generationID
			break
		}
	}
	c.mu.Unlock()
	if generationID == 0 {
		return 0, false
	}
	row, found, err := c.catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !found || !servableGeneration(row.State) || generationRowKey(row) != key && !c.adoptedCommitRowFor(row, key) {
		c.forgetRetained(generationID)
		return 0, false
	}
	return generationID, true
}

// adoptedCommitRowFor reports whether row is a commit layer another checkout
// built from the inputs key names (sharedCommit): it is filed under this
// checkout's key while its own row names its builder. Renamed to this
// checkout, its identity renders key exactly.
func (c *CheckoutCoordinator) adoptedCommitRowFor(row store_sqlite.ViewGeneration, key string) bool {
	if row.GenerationKind != CommitLayerGenerationKind || row.CheckoutID == c.checkoutID {
		return false
	}
	own := row
	own.CheckoutID, own.LayerID = c.checkoutID, commitLayerID(c.checkoutID)
	return generationRowKey(own) == key
}

// storedCommit is the durable half of the reuse cache: the catalog's own
// equal-identity lookup for a commit layer this checkout has already built.
//
// The process-local cache above holds four identities and dies with the
// process. Everything outside that window — a daemon restart, a branch visited
// once more than the cache is wide, a transition that dropped the cache
// wholesale — re-indexed a tree whose payload is still in the database and
// still servable. The identity is the same string both halves compare
// (generationIdentityKey), so "the same build" means exactly one thing here,
// in the cache, and in the catalog's own in-flight coalescing.
//
// The adoption itself writes nothing. The lookup is a bounded metadata read;
// what it returns is adopted by the route flip the caller was going to make
// anyway. Nothing is allocated, published, superseded or re-keyed, and the
// adopted row is not touched at all. A row that no longer holds up — retired
// under the reader, or an identity the renderer disagrees with — is skipped
// rather than routed.
//
// The one write the call can reach is not the adoption's. The adopted
// generation is filed in the process cache so the next switch back does not pay
// the read again, and a full cache evicts its tail; retainCommit offers every
// evicted generation for retirement, which is catalog + payload DML when
// nothing refuses it. That is the cache's own bookkeeping and is paid
// identically on the build path — reconcileCommitSlot files the built
// generation under the same key at the end of the same cycle — so it is a cost
// reuse moves earlier within a cycle, never one it adds.
// TestStoredCommitLayerReuseWritesNothing measures the non-evicting case at
// zero; TestStoredCommitLayerReuseWritesOnlyTheCacheEviction measures the
// evicting one and attributes every write to the generation the cache gave up.
func (c *CheckoutCoordinator) storedCommit(ctx context.Context, identity GenerationIdentity) (int64, bool) {
	key := generationIdentityKey(identity)
	rows, err := c.catalog.FindReusableViewGenerations(ctx, store_sqlite.ViewGeneration{
		OwnerKind:            identity.OwnerKind,
		GraphID:              identity.GraphID,
		LayerID:              identity.LayerID,
		CheckoutID:           identity.CheckoutID,
		GenerationKind:       identity.GenerationKind,
		BaseGenerationID:     identity.BaseGenerationID,
		LowerViewFingerprint: identity.LowerViewFingerprint,
		TreeOID:              identity.TreeOID,
		ProvenanceCommitOID:  identity.ProvenanceCommitOID,
		ConfigHash:           identity.ConfigHash,
		ExtractorVersions:    identity.ExtractorVersions,
		ResolverVersion:      identity.ResolverVersion,
		DependencyRevision:   identity.DependencyRevision,
	}, maxStoredCommitLayerCandidates)
	if err != nil {
		// A reuse lookup that cannot be answered is not a failed cycle: the
		// build below produces the same payload, at the price this item exists
		// to avoid.
		c.logger.Debug("checkout coordinator: stored commit layer lookup failed",
			zap.String("checkout", c.checkoutID), zap.Error(err))
		return 0, false
	}
	generationID, ok := selectReusableCommitGeneration(rows, key)
	if !ok {
		if generationID, ok = c.sharedCommit(ctx, identity); !ok {
			return 0, false
		}
	}
	c.retainCommit(ctx, key, generationID)
	return generationID, true
}

// sharedCommit adopts a commit layer another checkout of the graph built for
// the same state: same base generation and base tree, same target tree, same
// configuration, extractor, resolver and dependency identity. A commit layer
// is a function of exactly those inputs — the checkout only names who built
// it — so a new checkout at a tree another checkout already routes (a fresh
// worktree of a branch, a restart that minted a new identity) serves that
// layer instead of indexing the tree again. Retirement is safe to share: a
// generation any route, ref view or dependent generation references is never
// retired (the catalog's reference guard), whichever checkout offers it.
func (c *CheckoutCoordinator) sharedCommit(ctx context.Context, identity GenerationIdentity) (int64, bool) {
	sharedCommitLookups.Add(1)
	rows, err := c.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		GraphID:   identity.GraphID,
		OwnerKind: identity.OwnerKind,
		States:    []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationReady, store_sqlite.ViewGenerationSuperseded},
		Limit:     maxSharedCommitLayerCandidates,
	})
	if err != nil {
		c.logger.Debug("checkout coordinator: shared commit layer lookup failed",
			zap.String("checkout", c.checkoutID), zap.Error(err))
		return 0, false
	}
	var best int64
	for _, row := range rows {
		if row.GenerationKind != identity.GenerationKind || row.CheckoutID == identity.CheckoutID {
			continue
		}
		if !servableGeneration(row.State) || !sameCommitLayerInputs(row, identity) {
			continue
		}
		if row.GenerationID > best {
			best = row.GenerationID
		}
	}
	if best == 0 {
		return 0, false
	}
	c.logger.Info("checkout coordinator: adopted another checkout's commit layer",
		zap.String("checkout", c.checkoutID), zap.Int64("generation", best),
		zap.String("tree", identity.TreeOID))
	return best, true
}

// sharedCommitLookups counts the catalog scans sharedCommit made (tests).
var sharedCommitLookups atomic.Int64

// sameCommitLayerInputs reports whether a stored generation was built from
// the same inputs as identity, whoever built it.
func sameCommitLayerInputs(row store_sqlite.ViewGeneration, identity GenerationIdentity) bool {
	return row.GraphID == identity.GraphID &&
		row.BaseGenerationID == identity.BaseGenerationID &&
		row.LowerViewFingerprint == identity.LowerViewFingerprint &&
		row.TreeOID == identity.TreeOID &&
		row.ProvenanceCommitOID == identity.ProvenanceCommitOID &&
		row.ConfigHash == identity.ConfigHash &&
		row.ExtractorVersions == identity.ExtractorVersions &&
		row.ResolverVersion == identity.ResolverVersion &&
		row.DependencyRevision == identity.DependencyRevision
}

// selectReusableCommitGeneration is the Go half of the identity check: of the
// candidates the catalog offered, the first one this coordinator will actually
// route.
//
// It is exactly redundant with the lookup's SQL by construction — that filters
// the same thirteen columns generationRowKey renders, and the same servable
// states — so against a real catalog no input separates the two halves. It is
// kept because redundancy is the point: if a column is ever added to the
// identity and only one side learns about it, the side that did not is wrong,
// and this is the arm that fails closed. It refuses the candidate instead of
// routing a payload built under a rule the caller does not know it stated.
// cachedCommit's re-check is the same guard over the process-local half.
//
// It is a function of the rows alone so that the guard can be pinned on its own
// terms — TestStoredCommitLayerIdentityRecheckRefusesAForeignRow hands it a row
// no real lookup would return, which is the only way to state "and if the SQL
// ever did, this refuses it". The SQL half is pinned in its own package, by
// TestFindReusableViewGenerationsMatchesTheWholeIdentity.
func selectReusableCommitGeneration(rows []store_sqlite.ViewGeneration, key string) (int64, bool) {
	for _, row := range rows {
		if !servableGeneration(row.State) || generationRowKey(row) != key {
			continue
		}
		return row.GenerationID, true
	}
	return 0, false
}

// retainCommit records a commit generation as re-routable and retires whatever
// the cache had to give up to hold it.
func (c *CheckoutCoordinator) retainCommit(ctx context.Context, key string, generationID int64) {
	c.mu.Lock()
	retained := c.retained[:0:0]
	retained = append(retained, retainedCommitLayer{key: key, generationID: generationID})
	for _, entry := range c.retained {
		if entry.key == key || entry.generationID == generationID {
			continue
		}
		retained = append(retained, entry)
	}
	var evicted []int64
	if len(retained) > c.retain {
		for _, entry := range retained[c.retain:] {
			evicted = append(evicted, entry.generationID)
		}
		retained = retained[:c.retain]
	}
	c.retained = retained
	c.mu.Unlock()

	for _, generation := range evicted {
		if lane, _ := ctx.Value(importBuildLaneKey{}).(*importBuildLane); lane != nil && lane.detached {
			c.deferRetire(generation, "evicted during import planning")
		} else {
			c.offerRetire(ctx, generation)
		}
	}
}

// releaseCommit is what a replaced commit generation gets: a place in the
// reuse cache rather than immediate retirement.
//
// This is the whole of the branch-switch cache. Retiring the generation the
// route just left would make the next switch back re-index a tree whose
// payload is still sitting in the database; keeping it costs one branch's
// difference from the base until the cache evicts it.
func (c *CheckoutCoordinator) releaseCommit(ctx context.Context, generationID int64) {
	if generationID <= 0 {
		return
	}
	c.mu.Lock()
	held := false
	for _, entry := range c.retained {
		if entry.generationID == generationID {
			held = true
			break
		}
	}
	c.mu.Unlock()
	if !held {
		c.offerRetire(ctx, generationID)
	}
}

// forgetRetained drops one generation from the reuse cache.
func (c *CheckoutCoordinator) forgetRetained(generationID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.retained[:0]
	for _, entry := range c.retained {
		if entry.generationID != generationID {
			kept = append(kept, entry)
		}
	}
	c.retained = kept
}

// dropRetained empties the reuse cache and offers everything it held for
// retirement. The graph the cached generations composed over is gone, so none
// of them will ever be routed again.
//
// keep names the one generation to spare — the layer a transition has just
// routed, which is in the cache because it was reachable there and must not be
// collected out from under the route it now serves. 0 spares nothing.
func (c *CheckoutCoordinator) dropRetained(ctx context.Context, keep int64) {
	c.mu.Lock()
	retained := c.retained
	c.retained = nil
	c.mu.Unlock()
	for _, entry := range retained {
		if entry.generationID == keep {
			continue
		}
		c.offerRetire(ctx, entry.generationID)
	}
}

// --- the dirty-layer reuse cache ----------------------------------------
//
// The working-tree half of the commit cache above, and it exists for the same
// measured reason: a coordinator that retires the layer its route just left
// re-indexes the working tree from scratch the moment the tree comes back to a
// state it has already described. Undo/redo is that case, and so is a save
// that restores a file's previous bytes, and so is a branch switch back onto a
// worktree whose uncommitted edits never moved.
//
// The key is the whole build identity, rendered by the same function the
// catalog's coalescing and the commit cache use (generationIdentityKey), with
// the sample-derived fields stamped by the builder's own definition
// (StampDirtyLayerIdentity). That makes "the same build" mean exactly one
// thing across the cache, the builder and the catalog — and it means the
// commit generation the layer sits on is PART of the key, so a rebuilt commit
// layer can never hand a working-tree layer to a base it was not built over.
//
// The catalog-backed, survive-restart half of this cache — storedCommit's twin
// — is deliberately NOT implemented. It is a declared limitation of this item:
// a daemon restart between two identical dirty states pays one rebuild.

// cachedDirty returns a retained working-tree generation for an identity,
// after confirming the catalog still holds it in a state that can be served
// and that the row's own identity still renders to the key it was filed under.
// A generation that has gone, or one the renderer disagrees with, is dropped
// from the cache rather than re-routed — the same fail-closed guard cachedCommit
// makes over the commit half.
func (c *CheckoutCoordinator) cachedDirty(ctx context.Context, key string) (int64, bool) {
	// An unborn sample renders no key (dirtySampleKey). The guard is a
	// readability one and is knowingly redundant with the row re-key check
	// below — a real generation's row never renders the empty key, so a
	// lookup by it would fail closed there anyway. It is kept so that the two
	// halves of "the empty key is not an identity" sit beside the cache they
	// govern rather than being an emergent property of the renderer.
	if key == "" {
		return 0, false
	}
	c.mu.Lock()
	var generationID int64
	for _, entry := range c.retainedDirty {
		if entry.key == key {
			generationID = entry.generationID
			break
		}
	}
	c.mu.Unlock()
	if generationID == 0 {
		return 0, false
	}
	row, found, err := c.catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !found || !servableGeneration(row.State) ||
		row.GenerationKind != DirtyLayerGenerationKind || !c.dirtyRowRendersKey(ctx, row, key) {
		c.forgetRetainedDirty(generationID)
		if err != nil || found {
			// The row is still there (or could not be read): with the cache
			// no longer holding it, it is owed unless the route names it.
			c.oweUnroutedDirty(generationID, "dropped from the working-tree reuse cache")
		}
		return 0, false
	}
	return generationID, true
}

// retainDirty records a working-tree generation as re-routable and retires
// whatever the cache had to give up to hold it.
func (c *CheckoutCoordinator) retainDirty(ctx context.Context, key string, generationID int64) {
	// As in cachedDirty, the empty key is refused for readability rather than
	// for safety: an entry filed under it could never be hit, because no row
	// renders it.
	if key == "" || generationID <= 0 {
		return
	}
	c.mu.Lock()
	retained := c.retainedDirty[:0:0]
	retained = append(retained, retainedDirtyLayer{key: key, generationID: generationID})
	// A different generation filed under the same key — the chain top a fold
	// reproduces, a layer re-filed under its logical key — leaves the cache
	// here. The cache was its only holder unless the route names it (the
	// route's own release owes it then), so it is owed like an evicted layer:
	// dropped silently it was never retired, and pinned its whole chain.
	var displaced []int64
	for _, entry := range c.retainedDirty {
		if entry.generationID == generationID {
			continue
		}
		if entry.key == key {
			if entry.generationID != c.routedDirty {
				displaced = append(displaced, entry.generationID)
			}
			continue
		}
		retained = append(retained, entry)
	}
	var evicted []int64
	if len(retained) > c.retain {
		for _, entry := range retained[c.retain:] {
			evicted = append(evicted, entry.generationID)
		}
		retained = retained[:c.retain]
	}
	c.retainedDirty = retained
	c.mu.Unlock()

	// An evicted layer is owed a retirement, not given one here: the payload
	// delete is a background sweep's work, never a foreground cycle's.
	for _, generation := range evicted {
		c.deferRetire(generation, "evicted from the working-tree reuse cache")
	}
	for _, generation := range displaced {
		c.deferRetire(generation, "displaced in the working-tree reuse cache")
	}
}

// releaseDirty is what a replaced working-tree generation gets: a place in the
// reuse cache rather than immediate retirement. It is releaseCommit's twin,
// and the generation is retired only when the cache is not holding it.
func (c *CheckoutCoordinator) releaseDirty(ctx context.Context, generationID int64) {
	if generationID <= 0 {
		return
	}
	c.mu.Lock()
	held := false
	for _, entry := range c.retainedDirty {
		if entry.generationID == generationID {
			held = true
			break
		}
	}
	c.mu.Unlock()
	if !held {
		c.deferRetire(generationID, "released by the working-tree route")
	}
}

// oweUnroutedDirty owes a working-tree generation the cache let go of a
// retirement, unless the route names it (its release owes it then).
func (c *CheckoutCoordinator) oweUnroutedDirty(generationID int64, why string) {
	c.mu.Lock()
	routed := c.routedDirty == generationID
	c.mu.Unlock()
	if !routed {
		c.deferRetire(generationID, why)
	}
}

// forgetRetainedDirty drops one generation from the working-tree reuse cache.
func (c *CheckoutCoordinator) forgetRetainedDirty(generationID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.retainedDirty[:0]
	for _, entry := range c.retainedDirty {
		if entry.generationID != generationID {
			kept = append(kept, entry)
		}
	}
	c.retainedDirty = kept
}

// dropRetainedDirty empties the working-tree reuse cache and offers everything
// it held for retirement. keep names the one generation to spare — the layer a
// transition has just routed. 0 spares nothing.
func (c *CheckoutCoordinator) dropRetainedDirty(ctx context.Context, keep int64) {
	c.mu.Lock()
	retained := c.retainedDirty
	c.retainedDirty = nil
	c.mu.Unlock()
	for _, entry := range retained {
		if entry.generationID == keep {
			continue
		}
		c.offerRetire(ctx, entry.generationID)
	}
}

// --- retirement ---------------------------------------------------------

// offerRetire tries to collect a generation nothing should be reading. A
// refusal is not an error: the generation may be leased by a live view, routed
// by another checkout that adopted the same build, or still named as the base
// of a layer above it. Whatever refused it is expected to stop refusing later,
// so the generation goes on the backlog the janitor retries.
func (c *CheckoutCoordinator) offerRetire(ctx context.Context, generationID int64) {
	if generationID <= 0 {
		return
	}
	if c.retireCalled != nil {
		c.retireCalled(generationID)
	}
	if err := c.store.RetirePayloadGeneration(ctx, generationID, c.inUse()); err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogNotFound) {
			return
		}
		c.mu.Lock()
		_, known := c.backlog[generationID]
		c.backlog[generationID] = struct{}{}
		held := len(c.backlog)
		c.mu.Unlock()
		if !known {
			// A refused offer is owed to the background sweep like a deferral.
			countRetirementOwed(c.store, generationID, viewmetrics.OwedReleased)
			notifyDeferredRetirementWork()
		}
		// The blocking reason and how many generations this coordinator is
		// now owing a retirement for: together they say whether one holder is
		// stuck or the backlog is growing.
		c.logger.Debug("checkout coordinator: generation retirement deferred",
			zap.String("checkout", c.checkoutID),
			zap.Int64("generation", generationID),
			zap.String("blocked_by", retireBlockReason(err)),
			zap.Int("backlog", held), zap.Error(err))
	}
}

// retireBlockReason names what refused a retirement, in the same bounded
// vocabulary the metric label uses. The error carries the id; this carries the
// class, so a log line and a counter can be read against each other.
func retireBlockReason(err error) string {
	switch {
	case errors.Is(err, store_sqlite.ErrPayloadGenerationInUse):
		return viewmetrics.RefusedLeased
	case errors.Is(err, store_sqlite.ErrCatalogGenerationReferenced):
		return viewmetrics.RefusedRouted
	default:
		return viewmetrics.RefusedError
	}
}

// SweepRetirements retries every generation a retire refused, and reports how
// many were collected. It is the janitor's half of retirement: the coordinator
// offers, the sweep insists.
func (c *CheckoutCoordinator) SweepRetirements(ctx context.Context) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	pending := make([]int64, 0, len(c.backlog))
	for generationID := range c.backlog {
		pending = append(pending, generationID)
	}
	c.mu.Unlock()
	retireNewestFirst(pending)

	retired := 0
	for _, generationID := range pending {
		err := c.store.RetirePayloadGeneration(ctx, generationID, c.inUse())
		if err != nil && !errors.Is(err, store_sqlite.ErrCatalogNotFound) {
			continue
		}
		retired++
		viewmetrics.Count(viewmetrics.GenerationSweepCollectedTotal, viewmetrics.SweepCheckout)
		c.mu.Lock()
		delete(c.backlog, generationID)
		c.mu.Unlock()
	}
	return retired
}

// DrainRetirements hands over every generation this coordinator still owes a
// retirement for, and forgets them.
//
// Three sets go: the offers a refusal deferred, the commit layers the reuse
// cache was holding for a branch switch back, and the working-tree generation
// the route names. A closed coordinator has no loop left to retry the first
// and no cycle left to make use of the second; the third is there because the
// teardown that closes a coordinator withdraws the route first, and once that
// row is gone nothing in the catalog can be asked which generations a checkout
// had — a payload with no reachable id is a payload nothing can ever collect.
//
// What the route still names is refused retirement while it does, which is the
// correct answer for a coordinator that was dropped for any other reason: the
// offer stays on the owner's list and succeeds when — and only when — the
// route stops naming it.
func (c *CheckoutCoordinator) DrainRetirements() []int64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int64, 0, len(c.backlog)+len(c.retained)+len(c.retainedDirty)+1)
	for generationID := range c.backlog {
		out = append(out, generationID)
	}
	for _, entry := range c.retained {
		out = append(out, entry.generationID)
	}
	// The working-tree layers the reuse cache was holding for an undo go for
	// the same reason the commit layers do: a closed coordinator has no cycle
	// left to route them, and once the route row is withdrawn nothing in the
	// catalog can be asked which generations this checkout had.
	for _, entry := range c.retainedDirty {
		out = append(out, entry.generationID)
	}
	out = append(out, c.routedDirty)
	c.backlog = map[int64]struct{}{}
	c.retained = nil
	c.retainedDirty = nil
	c.routedDirty = 0
	return out
}

// retireNewestFirst orders a batch of retirements so a layer is offered before
// the generation it sits on.
//
// Retirement refuses a generation another one names as its base, so a working-
// tree layer and the commit layer under it can only be collected in one order,
// and a single pass that took them the other way round would leave the commit
// layer for the next sweep. Generation ids are an ascending sequence and a base
// is always created before what sits on it, so descending id IS that order.
func retireNewestFirst(generations []int64) {
	slices.Sort(generations)
	slices.Reverse(generations)
}

// inUse is the lease predicate retirement consults, nil when nothing leases
// generations.
func (c *CheckoutCoordinator) inUse() func(int64) bool {
	if c.leases == nil {
		return nil
	}
	return c.leases.InUse
}

// --- identity -----------------------------------------------------------

// commitIdentity is the catalog identity of the commit layer for one tree.
//
// It names everything the payload is a function of and nothing else. The base
// the layer sits on is carried as lower_view_fingerprint — the base corpus's
// committed tree — so a primary that moves invalidates the cache rather than
// leaving a layer composed over content it was not built against. The commit
// oid is deliberately NOT part of it: two commits with the same tree produce
// the same payload, and keying on the commit would rebuild for a rebase that
// changed nothing a reader can see.
func (c *CheckoutCoordinator) commitIdentity(base primaryBase, targetTree string) GenerationIdentity {
	return GenerationIdentity{
		OwnerKind:            checkoutLayerOwnerKind,
		GraphID:              base.graphID,
		LayerID:              commitLayerID(c.checkoutID),
		CheckoutID:           c.checkoutID,
		GenerationKind:       CommitLayerGenerationKind,
		BaseGenerationID:     base.generationID,
		LowerViewFingerprint: base.treeOID,
		TreeOID:              targetTree,
		ConfigHash:           c.configHash,
		ExtractorVersions:    c.extractors,
		ResolverVersion:      c.resolverVersion,
		DependencyRevision:   c.dependencyRevision(),
	}
}

// dirtyIdentity is the catalog identity of the working-tree layer. The three
// fields that identify WHICH working-tree state it describes — the tree, the
// commit and the content fingerprint — are stamped by BuildDirtyLayer from its
// own sample, so a caller cannot name one state and build another.
func (c *CheckoutCoordinator) dirtyIdentity(graphID string, commitGeneration int64) GenerationIdentity {
	return GenerationIdentity{
		OwnerKind:          checkoutLayerOwnerKind,
		GraphID:            graphID,
		LayerID:            dirtyLayerID(c.checkoutID),
		CheckoutID:         c.checkoutID,
		GenerationKind:     DirtyLayerGenerationKind,
		BaseGenerationID:   commitGeneration,
		ConfigHash:         c.configHash,
		ExtractorVersions:  c.extractors,
		ResolverVersion:    c.resolverVersion,
		DependencyRevision: c.dependencyRevision(),
	}
}

// --- the dependency cohort ----------------------------------------------

// dependencyRevision is the cohort revision this coordinator's identities
// currently carry. It is never empty: an empty revision is the legacy identity
// the reuse guards treat as matching anything, and a coordinator that cannot
// describe its cohort must not claim that.
func (c *CheckoutCoordinator) dependencyRevision() string {
	c.revisionMu.RLock()
	revision, degraded := c.revision, c.degraded
	c.revisionMu.RUnlock()
	switch {
	case revision != "":
		return revision
	case degraded != "":
		return degraded
	default:
		// A coordinator the constructor built has always described its cohort
		// by now, so this is the hand-assembled shape a focused fixture uses.
		// It still must not read as the legacy empty revision.
		return c.cohort.degradedRevision(dependencyCohortReasonUnavailable)
	}
}

// dependencyCohortDescribable reports whether the cached cohort is a real
// certificate rather than the degraded fallback.
func (c *CheckoutCoordinator) dependencyCohortDescribable() bool {
	c.revisionMu.RLock()
	defer c.revisionMu.RUnlock()
	return c.revision != ""
}

// InvalidateDependencyCohort marks the cached cohort as needing a fresh
// description.
//
// It is the event entry point for everything the coordinator cannot observe
// for itself: a repository registered or closed, a sibling's HEAD or tree
// moved, the configuration reloaded. Nothing is read here — the next
// opportunity that is allowed to describe the cohort does the work — so an
// event source may call it as often as it likes.
func (c *CheckoutCoordinator) InvalidateDependencyCohort(reason string) {
	if c == nil {
		return
	}
	c.revisionMu.Lock()
	c.cohortStale = true
	c.revisionMu.Unlock()
	c.logger.Debug("checkout coordinator: dependency cohort invalidated",
		zap.String("checkout", c.checkoutID), zap.String("reason", reason))
}

// ensureDependencyCohort describes the cohort only if the cached one is stale.
//
// This is what the 15-second poll and the retirement janitor use. Describing a
// cohort takes a daemon-wide roster read lease and one catalog read per
// in-scope roster member; a poll that did that would pay K×N reads per interval
// across the daemon for an answer that moves only when the topology does, and
// would hold a lease that every repository registration and every admission
// close has to wait behind. The poll compares identities — it allocates no
// generation and writes nothing — so a cached cohort is the right input for it.
// Two things make the cache stale. An EVENT source calls
// InvalidateDependencyCohort — that is how a member's bytes moving, a
// configuration reload or an admission close reaches it. And the poll itself
// re-reads the cheap workspace topology token: a repository tracked into or
// untracked out of this checkout's workspace changes which repositories are
// inputs at all, and that one the coordinator can observe for itself without a
// lease or a catalog read.
func (c *CheckoutCoordinator) ensureDependencyCohort(ctx context.Context) {
	token := c.cohort.topologyToken()
	c.revisionMu.RLock()
	stale := c.cohortStale || token != c.cohortTopology
	c.revisionMu.RUnlock()
	if !stale {
		return
	}
	c.describeDependencyCohort(ctx)
}

// describeDependencyCohort describes the cohort afresh, whatever the cache
// says, and reports whether the result is a certificate.
//
// Every build path calls it: the identity a layer is STORED with must name
// inputs that were validated at the moment the build started, not inputs that
// were validated some cycles ago. Once per cycle, not once per identity —
// reconcileCommitSlot and resolveCommitLayer both name the same commit layer,
// and a revision that moved between the two would have the cycle build under
// one identity and cache it under another.
func (c *CheckoutCoordinator) describeDependencyCohort(ctx context.Context) bool {
	// Sampled BEFORE the description, deliberately: a topology that moves
	// while the cohort is being described leaves the cache marked as described
	// under the OLDER token, so the next poll re-describes. The other order
	// would record a token the description had not seen and settle on it.
	//
	// The stale mark is cleared HERE, before the description, for the same
	// reason. Everything this run reads happens from now on, so an event that
	// lands while it is in flight names inputs it never saw — including, on
	// the failure path, the very change that would let the next description
	// succeed. Clearing after the description would clear that event's mark
	// too and nothing would retry it: a poll re-describes only when the mark
	// or the topology token has moved. Clearing before costs nothing extra —
	// a coordinator nobody invalidated during the description still ends with
	// the mark down, so a degraded poll still settles instead of paying a
	// roster lease every fifteen seconds for the whole of a transient.
	c.revisionMu.Lock()
	c.cohortStale = false
	c.revisionMu.Unlock()
	token := c.cohort.topologyToken()
	revision, err := c.cohort.revision(ctx)
	reason, degraded := "", ""
	if err != nil {
		reason = dependencyCohortRefusalReason(err)
		revision, degraded = "", c.cohort.degradedRevision(reason)
	}
	c.revisionMu.Lock()
	previous, previousReason := c.revision, c.revisionReason
	c.revision, c.degraded, c.revisionReason = revision, degraded, reason
	// cohortStale is deliberately NOT touched here: it was cleared before the
	// description started, so whatever it holds now is an event this run did
	// not cover.
	c.cohortTopology = token
	if revision != "" {
		c.cohortEverDescribed = true
		// The deferral budget is per-TRANSIENT, not per-coordinator-lifetime.
		// It is reset here rather than in cohortAllowsBuild because every
		// build path short-circuits that call away on the success path
		// (`!c.describeDependencyCohort(ctx) && !c.cohortAllowsBuild()`), so a
		// budget reset that lived there would never run after the first
		// exhausted transient — and every later transient would then stamp a
		// degraded layer on its FIRST cycle instead of deferring.
		c.cohortDeferrals = 0
	}
	c.revisionMu.Unlock()
	if err != nil {
		if previous != "" || previousReason != reason {
			c.logger.Warn("checkout coordinator: the resolver-visible input cohort could not be "+
				"described; this checkout's layers carry a degraded revision until it can",
				zap.String("checkout", c.checkoutID),
				zap.String("reason", reason), zap.Error(err))
		}
		return false
	}
	if previous != revision {
		c.logger.Debug("checkout coordinator: dependency revision updated",
			zap.String("checkout", c.checkoutID), zap.String("revision", revision))
	}
	return true
}

// cohortAllowsBuild decides what a cycle does when the cohort cannot be
// described.
//
// It prefers to DEFER. A build that goes ahead stamps its layer with a
// degraded revision — honest, but not a certificate — and a transient
// (a repository being untracked, a raw repository mid-mutation, a sibling that
// is tracked but not yet indexed) usually clears within a cycle or two. A
// checkout whose view is a few seconds behind is a better outcome than a layer
// stamped with a value that says nobody could name its inputs.
//
// It does not defer forever: after maxCohortBuildDeferrals consecutive
// deferrals the build proceeds under the degraded revision and says so, so a
// transient that never clears cannot freeze a checkout's view. The counter is
// reset by describeDependencyCohort on the first cycle that describes a cohort
// — which is also the cycle that re-keys the layers back onto a real revision —
// rather than here, because every build path short-circuits this function away
// on that path.
func (c *CheckoutCoordinator) cohortAllowsBuild() bool {
	c.revisionMu.Lock()
	if c.revision != "" {
		c.cohortDeferrals = 0
		c.revisionMu.Unlock()
		return true
	}
	if !c.cohortEverDescribed {
		reason := c.revisionReason
		c.revisionMu.Unlock()
		// Nothing to keep serving: this coordinator has never had a certified
		// cohort, so deferring would leave the checkout with no view at all
		// rather than with a slightly stale one. It builds, and says what it
		// built under.
		c.logger.Warn("checkout coordinator: building under a degraded dependency revision; "+
			"the input cohort has never been describable for this checkout",
			zap.String("checkout", c.checkoutID), zap.String("reason", reason))
		return true
	}
	c.cohortDeferrals++
	deferrals, reason := c.cohortDeferrals, c.revisionReason
	c.revisionMu.Unlock()
	if deferrals <= maxCohortBuildDeferrals {
		c.logger.Debug("checkout coordinator: build deferred until the input cohort can be described",
			zap.String("checkout", c.checkoutID),
			zap.String("reason", reason), zap.Int("deferrals", deferrals))
		return false
	}
	c.logger.Warn("checkout coordinator: building under a degraded dependency revision; "+
		"the input cohort has not been describable for several cycles",
		zap.String("checkout", c.checkoutID),
		zap.String("reason", reason), zap.Int("deferrals", deferrals))
	return true
}

// builderWorkspaceMembers derives the workspace topology a coordinator's
// cohort is scoped by from the builder it was handed.
//
// The builder's Admissions handle is the live per-repository Indexer
// (`checkout_lifecycle.go` hands it `mi.GetIndexer(prefix)`), and every
// Indexer a MultiIndexer creates carries a link back to it (`multi.go`,
// `idx.repositoryMutationOwner = mi`). ReposInWorkspace is that MultiIndexer's
// own answer to "which repositories share this workspace" — the same one the
// request surface scopes a workspace-scoped session with — so the cohort and
// the request surface cannot disagree about the boundary.
//
// nil when there is no such topology (a focused fixture, a standalone
// Indexer): the caller then scopes the cohort to the target repository alone
// and declares that scope.
func builderWorkspaceMembers(builder *SparseGenerationBuilder, workspaceID string) func() map[string]bool {
	if builder == nil || builder.Admissions == nil || workspaceID == "" {
		return nil
	}
	owner := builder.Admissions.repositoryMutationOwner
	if owner == nil {
		return nil
	}
	return func() map[string]bool { return owner.ReposInWorkspace(workspaceID) }
}

// builderConfigSections derives the output-affecting configuration domains
// that live OUTSIDE config.IndexConfig from the builder a producer was handed.
//
// The checkout lifecycle passes them explicitly (`ConfigSections:
// dedicatedBaseConfigSections(repoCfg)`, checkout_lifecycle.go), because it is
// holding the repository's whole config.Config at that point. A producer built
// from the IndexConfig alone — the ref view manager is the one in the tree —
// has no such value, and an EMPTY section list collapses the widened
// configuration digest back onto the narrow one: checkoutConfigHash renders a
// zero-length section list, so an artifacts / semantic / LSP / workspace /
// project change stops re-keying anything.
//
// The same link builderWorkspaceMembers rides — the live per-repository
// Indexer's MultiIndexer — carries the ConfigManager those sections come from,
// so the derivation reaches the same value the lifecycle would have passed
// rather than a second opinion about it.
//
// nil when there is nothing to read (a standalone Indexer, a fixture): the
// caller keeps whatever it was given.
func builderConfigSections(
	builder *SparseGenerationBuilder, repoPrefix string,
) []DependencyRevisionConfigSection {
	if builder == nil || builder.Admissions == nil || repoPrefix == "" {
		return nil
	}
	owner := builder.Admissions.repositoryMutationOwner
	if owner == nil || owner.configMgr == nil {
		return nil
	}
	return dedicatedBaseConfigSections(owner.configMgr.GetRepoConfig(repoPrefix))
}

// cohortProducerPolicy renders the producer policy a build under this
// configuration will declare, as far as the configuration decides it.
//
// SparseGenerationBuilder.declareProducers is the authority on what is actually
// written (builder_generation.go). Two groups of its rows are deliberately NOT
// digested here:
//
//   - the rows the IDENTITY decides — search.text and the lsp.* family are a
//     function of the generation kind, which is already an identity column of
//     its own; digesting them again would move two fields on one change.
//   - the rows a build OUTCOME narrows — a truncated closure, a language
//     server that was cut short. Those describe what a build produced, not
//     what it was given, and a cohort digest is over inputs.
//
// What is left is the policy the configuration fixes before anything is built,
// which is what a reader comparing two builds' inputs needs. Because it mirrors
// declareProducers rather than calling it, the mirror is pinned by a test that
// reads a real build's stored producer rows back and compares them row for row
// (checkout_identity_revision_test.go).
func cohortProducerPolicy(cfg config.IndexConfig, hasEmbedder bool) []DependencyRevisionProducer {
	vector := DependencyRevisionProducer{
		Producer: string(graphview.CapSearchVector),
		State:    string(store_sqlite.ProducerStateDisabledByConfig),
		Reason:   "no embedding provider is configured for the build",
	}
	if hasEmbedder {
		vector.State = string(store_sqlite.ProducerStateComplete)
		vector.Reason = ""
	}
	similarity := DependencyRevisionProducer{
		Producer: string(graphview.CapSimilarity),
		State:    string(store_sqlite.ProducerStateDisabledByConfig),
		Reason:   "near-duplicate detection is switched off for the build",
	}
	if cfg.Coverage.IsEnabled("clones") {
		similarity.State = string(store_sqlite.ProducerStateIncomplete)
		similarity.Reason = "near-duplicate detection ranks bodies against a corpus; " +
			"a sparse generation ranks them against its file set"
	}
	complete := func(capability graphview.CapabilityID) DependencyRevisionProducer {
		return DependencyRevisionProducer{
			Producer: string(capability),
			State:    string(store_sqlite.ProducerStateComplete),
		}
	}
	return []DependencyRevisionProducer{
		complete(graphview.CapSourceSnapshot),
		{
			Producer: string(graphview.CapSourceConfig),
			State:    string(store_sqlite.ProducerStateComplete),
			Reason:   sourceConfigNarrowingReason,
		},
		complete(graphview.CapSyntaxGraph),
		complete(graphview.CapResolutionLocal),
		complete(graphview.CapIncomingEdges),
		complete(graphview.CapSearchSymbols),
		complete(graphview.CapSearchContent),
		vector,
		similarity,
		{
			Producer: string(graphview.CapResolutionCrossRepo),
			State:    string(store_sqlite.ProducerStateIncomplete),
			Reason:   "a sparse generation is resolved within one repository",
		},
	}
}

// cohortCapabilityVocabulary is the capability set the producer policy is
// stated over. A capability that joins the vocabulary changes what silence
// about it means, so it is a cohort member in its own right.
func cohortCapabilityVocabulary() []string {
	known := graphview.KnownCapabilities()
	out := make([]string, 0, len(known))
	for _, capability := range known {
		out = append(out, string(capability))
	}
	return out
}

// commitLayerID and dirtyLayerID name a checkout's two layers. They are
// derived rather than generated so the catalog's in-flight coalescing can
// recognise two builds of the same layer as the same build.
func commitLayerID(checkoutID string) string { return "commit-" + checkoutID }
func dirtyLayerID(checkoutID string) string  { return "dirty-" + checkoutID }

// generationIdentityKey renders the build identity as one comparable string.
// It carries exactly the columns the catalog's in-flight coalescing compares,
// so the reuse cache and the catalog agree on what "the same build" means.
func generationIdentityKey(identity GenerationIdentity) string {
	var b strings.Builder
	for _, field := range []string{
		identity.OwnerKind,
		identity.GraphID,
		identity.LayerID,
		identity.CheckoutID,
		identity.GenerationKind,
		strconv.FormatInt(identity.BaseGenerationID, 10),
		identity.LowerViewFingerprint,
		identity.TreeOID,
		identity.ProvenanceCommitOID,
		identity.ConfigHash,
		identity.ExtractorVersions,
		identity.ResolverVersion,
	} {
		b.WriteString(field)
		b.WriteByte(0)
	}
	// Empty is the legacy identity: keep its existing persisted fingerprints.
	// Nonempty dependency inputs add a separate length-delimited output key.
	if identity.DependencyRevision != "" {
		b.WriteString("dependency-revision:")
		b.WriteString(strconv.Itoa(len(identity.DependencyRevision)))
		b.WriteByte(':')
		b.WriteString(identity.DependencyRevision)
	}
	return b.String()
}

// generationRowKey renders a stored generation's identity the same way, so a
// row read back from the catalog can be compared with a request.
func generationRowKey(row store_sqlite.ViewGeneration) string {
	return generationIdentityKey(GenerationIdentity{
		OwnerKind:            row.OwnerKind,
		GraphID:              row.GraphID,
		LayerID:              row.LayerID,
		CheckoutID:           row.CheckoutID,
		GenerationKind:       row.GenerationKind,
		BaseGenerationID:     row.BaseGenerationID,
		LowerViewFingerprint: row.LowerViewFingerprint,
		TreeOID:              row.TreeOID,
		ProvenanceCommitOID:  row.ProvenanceCommitOID,
		ConfigHash:           row.ConfigHash,
		ExtractorVersions:    row.ExtractorVersions,
		ResolverVersion:      row.ResolverVersion,
		DependencyRevision:   row.DependencyRevision,
	})
}

// servableGeneration mirrors the materializer's rule for a generation a route
// may name: ready serves, and so does superseded — superseded says only that a
// newer generation exists, and the route decides what a checkout reads.
func servableGeneration(state store_sqlite.ViewGenerationState) bool {
	return state == store_sqlite.ViewGenerationReady || state == store_sqlite.ViewGenerationSuperseded
}

// indexConfigHash digests the index configuration a generation was built
// under. A payload built under different extraction rules does not compose
// with one built under these, so the digest is part of the build identity
// rather than a diagnostic.
func indexConfigHash(cfg config.IndexConfig) string {
	normalized := cfg
	if cfg.FrameworkSynthesizers != nil {
		names := slices.Clone(*cfg.FrameworkSynthesizers)
		slices.Sort(names)
		names = slices.Compact(names)
		normalized.FrameworkSynthesizers = &names
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		// An unencodable configuration cannot be compared, and treating that
		// as "matches everything" would reuse a payload built under rules
		// nobody can name. A unique digest makes every such build its own.
		return "unhashable-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:16])
}

// checkoutConfigHash widens the configuration digest a generation's identity
// carries from "the index configuration" to "the configuration".
//
// fingerprint is snapshotDedicatedBaseConfig's versioned digest — the index
// configuration inside an explicit repo/workspace/project envelope, which is
// what makes the same index settings in two namespaces two identities. The
// named sections carry the domains that live outside config.IndexConfig and
// still decide what a payload contains: artifacts, semantic, LSP, workspace,
// project, source selection.
//
// The encoding is the length-delimited one generationIdentityKey uses, so no
// section name or digest can imitate a delimiter and make two configurations
// collide. Sections are sorted, so the caller's enumeration order is not part
// of the answer.
//
// The unencodable-configuration fail-safe is preserved by the caller: it hands
// a unique fingerprint in, and a unique fingerprint makes a unique digest.
func checkoutConfigHash(fingerprint string, sections []DependencyRevisionConfigSection) string {
	ordered := slices.Clone(sections)
	slices.SortFunc(ordered, func(a, b DependencyRevisionConfigSection) int {
		if a.Name != b.Name {
			return strings.Compare(a.Name, b.Name)
		}
		return strings.Compare(a.Digest, b.Digest)
	})
	var b strings.Builder
	field := func(label, value string) {
		b.WriteString(label)
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(len(value)))
		b.WriteByte(':')
		b.WriteString(value)
		b.WriteByte(0)
	}
	field("domain", checkoutConfigDigestDomain)
	field("index", fingerprint)
	field("sections", strconv.Itoa(len(ordered)))
	for _, section := range ordered {
		field("section.name", section.Name)
		field("section.digest", section.Digest)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:16])
}

// resolverVersionFingerprint stamps the resolution contract a generation was
// built under, for every producer in this package.
//
// It used to be the compile-time literal "1", in two copies (here and in
// ref_views.go). A literal only invalidates when a human remembers to raise it,
// and two copies can disagree; resolver.Version derives the value from the
// resolver's own registered pass set plus its hand-maintained semantics
// version, so a pass that joins, leaves or is renamed re-keys every stored
// generation without anyone having to notice.
//
// Computed once: the registry is fixed for the life of the process, and a
// per-call digest over fifty pass names would run on every identity a
// coordinator mints.
var resolverVersionFingerprint = sync.OnceValue(resolver.Version)

// extractorVersionsFingerprint renders the extractor policy versions the same
// way the per-repo freshness row does, so a language whose extractor was
// bumped re-builds the layers that carry its files.
func extractorVersionsFingerprint() string {
	encoded, err := json.Marshal(extractorVersionsSnapshot())
	if err != nil {
		return ""
	}
	return string(encoded)
}

// commitLayerBase presents a checkout's commit layer, composed over the base
// corpus, as the base a dirty-layer build reads.
//
// The composition answers every identity read on its own. The two reads it
// does not carry are served from the corpus handle underneath it: the batched
// file-node lookup, which is the per-file read in a loop, and the durable
// reference facts, whose rows are the corpus's. The facts are a hint the
// closure adds to its edge walk rather than its only source, so serving the
// corpus's rows leaves the walk — which does see the commit layer — as the
// authority on what depends on what.
type commitLayerBase struct {
	graph.Reader
	// The clone follow-up uses the pinned ancestry's per-generation corpus
	// sidecars with the same ownership layers as Reader's composition.
	cloneSources    []graphview.GenerationSource
	cloneCorpusBase *store_sqlite.Store
	// facts serves the reference-fact hints from the whole materialized
	// ancestry. A base assembled with one takes it; a base assembled with a
	// single corpus handle below keeps the older scope. See ancestryRefFacts
	// for the evidence that one generation handle is not enough.
	facts ancestryRefFacts
	// corpus is the single-generation fact handle the older callers hand in.
	// It is consulted only when no ancestry was composed.
	corpus *store_sqlite.Store
	// stack is the view's generation ancestry, bottom first, when every
	// generation in it is immutable (the view does not compose the mutable
	// generation zero); nil otherwise. Equal stacks read equal rows, so a
	// per-file delta keys what it derives from the base by it.
	stack []int64
	// chainDepth is how many generations at the top of stack are the dirty
	// chain a working-tree build stands on (its parent and the parent's
	// chain); zero for a base that is a commit stack. The per-stack caches
	// are kept for the generations below them (edit_delta_contract_cache.go),
	// and a delta composes the chain over them per read.
	chainDepth int
}

// commitStack is the part of the stack the per-stack caches are kept for:
// every generation below the dirty chain.
func (b commitLayerBase) commitStack() []int64 {
	if b.chainDepth <= 0 || b.chainDepth >= len(b.stack) {
		if b.chainDepth > 0 {
			return nil
		}
		return b.stack
	}
	return b.stack[:len(b.stack)-b.chainDepth]
}

// chainGenerations is the dirty chain's generations, bottom first.
func (b commitLayerBase) chainGenerations() []int64 {
	if b.chainDepth <= 0 || b.chainDepth >= len(b.stack) {
		return nil
	}
	return b.stack[len(b.stack)-b.chainDepth:]
}

// editDeltaChainOverlay turns the keying below a dirty chain on; off, a
// chained build's caches are keyed by its whole stack, the chain included
// (the measurements of the keying compare the two).
var editDeltaChainOverlay = true

// withChainDepth marks base's top depth generations as the dirty chain a
// build over parent stands on. A base whose stack does not end in parent, or
// is not deeper than the chain, is returned unmarked: its caches stay keyed
// by the whole stack.
func withChainDepth(base LayerBase, parent int64, depth int) LayerBase {
	layer, ok := base.(commitLayerBase)
	if !ok || !editDeltaChainOverlay || parent <= 0 || depth <= 0 || len(layer.stack) <= depth || layer.stack[len(layer.stack)-1] != parent {
		return base
	}
	layer.chainDepth = depth
	return layer
}

var _ LayerBase = commitLayerBase{}
var _ graph.RefFactsReader = commitLayerBase{}

// GetFileNodesByPaths answers the batched read from the composed view one path
// at a time. The closure asks it once per build with the change set's paths,
// so the loop is bounded by the change rather than by the repository.
func (b commitLayerBase) GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node {
	out := make(map[string][]*graph.Node, len(filePaths))
	for _, path := range filePaths {
		if nodes := b.GetFileNodes(path); len(nodes) > 0 {
			out[path] = nodes
		}
	}
	return out
}

// LoadRefFactsByFiles serves the composed ancestry's persisted forward facts,
// falling back to the single corpus handle for a base assembled without one.
func (b commitLayerBase) LoadRefFactsByFiles(repoPrefix string, files []string) ([]graph.RefFact, error) {
	if len(b.facts.handles) > 0 {
		return b.facts.LoadRefFactsByFiles(repoPrefix, files)
	}
	if b.corpus == nil {
		return []graph.RefFact{}, nil
	}
	return b.corpus.LoadRefFactsByFiles(repoPrefix, files)
}

// LoadRefFactsByTargets serves the composed ancestry's persisted reverse
// facts, falling back to the single corpus handle for a base assembled without
// one.
func (b commitLayerBase) LoadRefFactsByTargets(repoPrefix string, targetIDs []string) (map[string][]graph.RefFact, error) {
	if len(b.facts.handles) > 0 {
		return b.facts.LoadRefFactsByTargets(repoPrefix, targetIDs)
	}
	if b.corpus == nil {
		return map[string][]graph.RefFact{}, nil
	}
	return b.corpus.LoadRefFactsByTargets(repoPrefix, targetIDs)
}

var _ graph.RefFactsChainSplitter = commitLayerBase{}

// RefFactsSplitAt implements graph.RefFactsChainSplitter: the facts of the
// generations below the top chainLayers, and the top ones' union over them.
func (b commitLayerBase) RefFactsSplitAt(chainLayers int) (graph.RefFactsReader, graph.RefFactsChainOverlay, bool) {
	if chainLayers <= 0 || len(b.facts.handles) <= chainLayers {
		return nil, nil, false
	}
	cut := len(b.facts.handles) - chainLayers
	below := ancestryRefFacts{handles: b.facts.handles[:cut]}
	chain := ancestryRefFacts{handles: b.facts.handles[cut:]}
	return below, chain.overlayByTargets, true
}

// ancestryRefFacts serves the durable reference-fact hints from every
// generation the structural reads compose, instead of from one of them.
//
// The rows are scoped by generation: ref_facts carries a view_gen column and
// Store.AtGeneration(g) answers with generation g's rows ALONE
// (store_reffacts.go LoadRefFactsByFiles / LoadRefFactsByTargets bind
// s.viewGen; store_generation.go AtGeneration). A base handed one generation
// handle therefore answers the closure's fact question from one layer of a
// stack the structural reads compose in full — and today every fact in the
// database was written by the legacy indexer at generation zero
// (ref_facts.go persistRefFactsForFiles is reached only from the Indexer's
// incremental and full paths; the sparse generation builder writes none), so a
// handle pinned to a published committed base answers "no facts" for every
// file in the repository. The hint does not fail loudly when that happens: the
// closure logs nothing, adds nothing, and returns a NARROWER affected set than
// the resolver will bind over — which is the one direction the closure is not
// allowed to be wrong in.
//
// So the hints are composed from the same materialized ancestry the structural
// reads use: the view's generation sources, plus the base corpus at generation
// zero underneath them. The composition is a UNION rather than a
// topmost-claim-wins mask, deliberately. A fact is a hint the closure adds to
// its edge walk, never its only source; a stale row from a lower layer can
// only widen the closure, and a superset is what the closure owes the
// resolver, while masking a lower layer's rows behind an upper layer that
// writes none would narrow it to nothing at exactly the changed files the
// closure exists for.
//
// The handles are the view's own — pinned to generations its lease holds — so
// reading through them is bounded by the caller's view lifetime and needs no
// pin of its own.
type ancestryRefFacts struct {
	handles []*store_sqlite.Store
}

// newAncestryRefFacts composes the fact readers for one materialized view:
// generation zero first, then every generation the view stacks on it, bottom
// first.
func newAncestryRefFacts(corpus *store_sqlite.Store, view *graphview.RepoView) ancestryRefFacts {
	var out ancestryRefFacts
	if corpus != nil {
		if base := corpus.AtGeneration(0); base != nil {
			out.handles = append(out.handles, base)
		}
	}
	for _, source := range view.GenerationSources() {
		if source.Handle != nil {
			out.handles = append(out.handles, source.Handle)
		}
	}
	return out
}

// refFactIdentity is what makes two rows from two generations the same fact.
// Origin, tier and candidates are provenance rather than identity: a later
// generation re-deriving the same reference with a better tier must not double
// the row the closure reads.
type refFactIdentity struct {
	from, to, kind, refName, filePath string
	line                              int
}

func identifyRefFact(fact graph.RefFact) refFactIdentity {
	return refFactIdentity{
		from: fact.FromID, to: fact.ToID, kind: fact.Kind,
		refName: fact.RefName, filePath: fact.FilePath, line: fact.Line,
	}
}

// LoadRefFactsByFiles unions the forward facts every composed generation holds
// for these files. A handle that fails is reported and skipped rather than
// failing the whole read: the hint is additive, and losing one layer's rows
// must not lose the others'.
func (a ancestryRefFacts) LoadRefFactsByFiles(repoPrefix string, files []string) ([]graph.RefFact, error) {
	out := []graph.RefFact{}
	seen := map[refFactIdentity]struct{}{}
	var firstErr error
	for _, handle := range a.handles {
		facts, err := handle.LoadRefFactsByFiles(repoPrefix, files)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, fact := range facts {
			key := identifyRefFact(fact)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, fact)
		}
	}
	return out, firstErr
}

// LoadRefFactsByTargets unions the reverse facts every composed generation
// holds for these targets, keeping the grouping by source file path the
// caller reads.
func (a ancestryRefFacts) LoadRefFactsByTargets(
	repoPrefix string, targetIDs []string,
) (map[string][]graph.RefFact, error) {
	out := map[string][]graph.RefFact{}
	seen := map[refFactIdentity]struct{}{}
	var firstErr error
	for _, handle := range a.handles {
		byFile, err := handle.LoadRefFactsByTargets(repoPrefix, targetIDs)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for file, facts := range byFile {
			for _, fact := range facts {
				key := identifyRefFact(fact)
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				out[file] = append(out[file], fact)
			}
		}
	}
	return out, firstErr
}

// overlayByTargets adds a's facts for targets to the facts the generations
// below a hold for them (below): the union LoadRefFactsByTargets composes
// over every generation, with below's rows first and a's in generation order
// after, each fact kept once.
func (a ancestryRefFacts) overlayByTargets(
	repoPrefix string, targetIDs []string, below map[string][]graph.RefFact,
) (map[string][]graph.RefFact, error) {
	out := make(map[string][]graph.RefFact, len(below))
	seen := map[refFactIdentity]struct{}{}
	for file, facts := range below {
		for _, fact := range facts {
			seen[identifyRefFact(fact)] = struct{}{}
		}
		out[file] = append([]graph.RefFact(nil), facts...)
	}
	chain, err := a.LoadRefFactsByTargets(repoPrefix, targetIDs)
	for file, facts := range chain {
		for _, fact := range facts {
			key := identifyRefFact(fact)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out[file] = append(out[file], fact)
		}
	}
	return out, err
}

// CycleLap is one timed step of a checkout cycle.
type CycleLap struct {
	Name     string
	Duration time.Duration
}
