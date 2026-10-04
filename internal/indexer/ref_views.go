package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"uuid"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
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
	// ConfigSections carries the output-affecting configuration domains that
	// live outside config.IndexConfig — artifacts, semantic, LSP, workspace,
	// project — exactly as a checkout coordinator carries them. They widen the
	// generation's config_hash beyond the index configuration and are part of
	// the dependency cohort.
	//
	// A manager handed none still builds: its config digest then covers the
	// versioned snapshot fingerprint alone (which already carries the whole
	// index configuration plus the repo/workspace/project envelope), and its
	// cohort is degraded rather than certified.
	ConfigSections []DependencyRevisionConfigSection
	// ConfigSectionsFor is ConfigSections re-read per description instead of
	// frozen at construction, and it is what a caller holding a live
	// configuration passes.
	//
	// A manager outlives the configuration it was built under: it is cached
	// per repository for the life of the daemon, while a reload swaps the
	// repository's whole config.Config underneath it. A frozen section list
	// therefore keeps keying generations on the artifacts / semantic / LSP /
	// workspace / project domains as they stood when the first selection
	// happened to build the manager, and a view built after the reload reuses
	// a generation produced under rules that no longer apply. Re-reading costs
	// one config lookup on a memo MISS — the same lookup the fallback derived
	// from the builder link already made — and is paid only when a description
	// is being taken anyway.
	//
	// nil falls back to ConfigSections, then to the builder link.
	ConfigSectionsFor func(repoPrefix string) []DependencyRevisionConfigSection
	// Leases enumerates the repository roster the dependency cohort is
	// described from. nil leaves every identity on the stable degraded
	// revision — honest, and outside the certified vocabulary — rather than on
	// the empty revision, which the reuse guards read as matching anything.
	Leases *graphview.LeaseManager
	Logger *zap.Logger
	// Gate holds a claimed build's pass while the daemon warms up. nil admits
	// every build at once, which is what a manager outside a warmup has.
	Gate *ViewBuildGate
	// RequestBase asks the committed-base publisher for the base of one
	// repository because a ref view of it now needs one.
	//
	// It is the ref view's half of the committed-base consumer gate. The
	// startup publisher and the live advance trigger both decline a family
	// with no reader (InitialBasePublisher.publish's "no dependent checkout"
	// skip), and a ref view IS one of the two readers that gate counts
	// (CheckoutLifecycle.dedicatedBaseConsumers) — so a view created on a
	// running daemon whose family has no other consumer finds generation 0 and
	// nothing would ever publish for it. This is what asks.
	//
	// The same shape a checkout coordinator is wired with
	// (CheckoutCoordinatorConfig.RequestBase): it takes the repository prefix,
	// must never publish on the caller's goroutine and must never block. nil
	// asks nothing, which is the manager a test or a non-lifecycle caller
	// builds, and leaves the view exactly in the legacy regime it had before.
	RequestBase func(repoPrefix string)

	// buildBarrier is a test seam: it runs between a build pass finishing and
	// the publish step re-resolving the selector, which is exactly the window
	// the revalidation exists to close. nil in production.
	buildBarrier func()

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
	lifetime refViewLifetime
	store    *store_sqlite.Store
	catalog  *store_sqlite.Catalog
	builder  *SparseGenerationBuilder
	logger   *zap.Logger
	gate     *ViewBuildGate

	config            config.IndexConfig
	configSections    []DependencyRevisionConfigSection
	configSectionsFor func(string) []DependencyRevisionConfigSection

	leases          *graphview.LeaseManager
	extractors      string
	resolverVersion string

	// identityMu guards the per-target identity keys below.
	//
	// A manager serves one repository, so the target triple every request
	// carries is the same one every time; the keys are memoized rather than
	// re-derived per request because describing a cohort takes a roster lease
	// and a catalog read per in-scope member, and a selection that answers
	// "already current" must not pay that.
	identityMu   sync.Mutex
	identityKeys map[string]refViewIdentityKeys
	// identityRefused is the degraded identity each target was last SERVED
	// under, and it is not a memo: it never suppresses a description. A
	// refusal re-describes on every selection (identityKeysFor says why), and
	// this only keeps the answer that refusal yields steady while it lasts.
	// Without it a refusal class that flaps between transient causes — a
	// roster that moved, a sibling not yet indexed, admissions closing — moves
	// the degraded revision per selection, which re-keys the view's
	// fingerprint and starts a build for a cohort that did not change. Dropped
	// wherever the memo is dropped, and by the first description that
	// certifies.
	identityRefused map[string]refViewIdentityKeys

	buildBarrier func()

	// requestBase is RefViewManagerConfig.RequestBase; nil asks nothing.
	requestBase func(string)
	// baseDemandMu guards the throttle below. It is its own lock because the
	// ask happens on a selection's goroutine, before anything else the
	// selection does, and many selections run at once.
	baseDemandMu sync.Mutex
	// lastBaseDemand is when this manager last asked for one repository's
	// committed base. Keyed by repository prefix rather than held as a single
	// stamp because base() derives the prefix from the graph row it just read:
	// a manager is cached per repository today, and a throttle that assumed it
	// would silently start starving the second repository if that ever stopped
	// being true. Stamps older than the interval are dropped as they are
	// passed, so the map holds only what it can still refuse.
	lastBaseDemand map[string]time.Time

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
	if cfg.Leases == nil {
		// Not a refusal: a ref view with no roster to enumerate still keys its
		// generations on a STABLE degraded revision, which is strictly better
		// than the empty legacy value every stored generation matches. But it
		// is not a certificate either, and a manager that runs this way for
		// the life of a daemon should say so once rather than per selection.
		logger.Warn("ref view: no repository roster to describe this manager's input cohort with; " +
			"every generation it keys will carry a degraded dependency revision")
	}
	return &RefViewManager{
		store:             cfg.Store,
		catalog:           cfg.Store.Catalog(),
		builder:           cfg.Builder,
		logger:            logger,
		gate:              cfg.Gate,
		config:            cfg.Config,
		configSections:    append([]DependencyRevisionConfigSection(nil), cfg.ConfigSections...),
		configSectionsFor: cfg.ConfigSectionsFor,
		leases:            cfg.Leases,
		extractors:        extractorVersionsFingerprint(),
		resolverVersion:   resolverVersionFingerprint(),
		identityKeys:      map[string]refViewIdentityKeys{},
		identityRefused:   map[string]refViewIdentityKeys{},
		buildBarrier:      cfg.buildBarrier,
		requestBase:       cfg.RequestBase,
		lastBaseDemand:    map[string]time.Time{},
		buildGrace:        refViewWindow(cfg.buildGrace, refViewBuildGrace),
		buildHeartbeat:    refViewWindow(cfg.buildHeartbeat, refViewBuildHeartbeat),
		buildLiveness:     refViewWindow(cfg.buildLiveness, refViewBuildLiveness),
		writerBudget:      refViewWindow(cfg.writerBudget, refViewWriterBudget),
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
	ctx, release, err := m.lifetime.admit(ctx, false)
	if err != nil {
		return RefViewResult{}, err
	}
	defer release()
	base, owedBase, err := m.base(ctx, req.GraphID)
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

	// The ask for a committed base the family has not published, placed HERE
	// and not where the absence was noticed (m.base, above).
	//
	// AFTER m.row, because the publication it triggers is itself
	// consumer-gated and THIS view is the consumer: InitialBasePublisher.publish
	// re-reads the census (CheckoutLifecycle.dedicatedBaseConsumers, whose ref
	// view arm is ListRefViews(graph) len > 0) on the publisher's own worker.
	// Asking before the row is written races that worker: it sees zero ref
	// views, skips with "no dependent checkout", and the ask is SPENT — the
	// publisher's pending list no longer holds the prefix and the throttle
	// below refuses the next one for dedicatedBaseDemandInterval. Since
	// nothing re-selects a ref view on its own, a one-shot client could end
	// with no base published at all. The row is committed by the time m.row
	// returns, so a publication demanded from here always finds its consumer.
	//
	// AFTER resolution, because a selector naming a ref that does not exist
	// must not trigger the daemon's single largest write. That selection
	// returns through m.failed above; the row it leaves behind makes the
	// family a census consumer, so the base is still owed and the next
	// publication attempt — a HEAD movement, a daemon start, or the next
	// selection that actually resolves — will publish it.
	//
	// It changes nothing about THIS selection either way: the publication is
	// queued on the publisher's own list and built off this goroutine, this
	// view is built over generation 0, and when the base lands the view
	// re-selects onto it — the build fingerprint carries the base
	// (refViewBuildFingerprint over identity), so activeIsCurrent says no and
	// the next selection rebuilds over the published base.
	m.demandCommittedBase(owedBase)

	identity := m.identity(ctx, req, viewID, base, resolved.TreeOID)
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

// base resolves the corpus a view's layer sits on, and names the repository
// that is OWED a committed base because the graph has published none.
//
// It only names it: base is a read and stays one. The ask itself belongs
// strictly later in the selection, after the view's catalog row exists — see
// EnsureRefView's demand paragraph for why the order is load-bearing.
//
// A graph whose base is already a published generation returns an empty
// prefix, which is what "nothing to ask for" is.
func (m *RefViewManager) base(ctx context.Context, graphID string) (primaryBase, string, error) {
	dedicated, found, err := m.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return primaryBase{}, "", err
	}
	if !found {
		return primaryBase{}, "", fmt.Errorf("indexer: graph %s has no dedicated-graph row to build over", graphID)
	}
	base, err := graphBase(ctx, m.catalog, dedicated)
	if err != nil || base.generationID != 0 {
		return base, "", err
	}
	// graphBase's SECOND arm: the graph has published no generation, so this
	// view composes over the shared indexed corpus — which is rewritten IN
	// PLACE as the primary moves (pinRoutedBase's LEGACY REGIME paragraph).
	// That is a supported regime and the selection carries on in it,
	// truthfully; what it must not be is PERMANENT.
	//
	// Since the committed base became consumer-gated, nothing publishes for a
	// family whose only reader is a ref view unless something asks: a daemon
	// start and a HEAD movement both decline ("no dependent checkout"), and
	// dedicatedBaseConsumers counts this view only once a publication is
	// already being attempted. A view created on a running daemon would
	// therefore wait for the next HEAD movement or the next daemon start,
	// unbounded on an idle-HEAD repository.
	return base, dedicated.RepoPrefix, nil
}

// demandCommittedBase asks the publisher for one repository's committed base,
// at most once per dedicatedBaseDemandInterval.
//
// The throttle is not a de-duplicator — InitialBasePublisher.RequestBase
// already coalesces onto a queued request, so a burst costs one queue lookup
// each. It bounds the case the coalescing cannot: a publication that was
// ATTEMPTED and failed leaves nothing queued, and selection is the only thing
// that ever notices a ref moved, so a client polling a building view would
// otherwise re-enter the whole publication protocol (including the authority
// claim, which writes) once per poll for as long as the failure lasts.
//
// Same interval as the dependent checkout's ask, and for the same reason: the
// two are one policy about how often a reader may re-ask for a base that is
// not there.
func (m *RefViewManager) demandCommittedBase(repoPrefix string) {
	if m == nil || m.requestBase == nil || repoPrefix == "" {
		return
	}
	now := time.Now()
	m.baseDemandMu.Lock()
	if m.lastBaseDemand == nil {
		// A manager built by struct literal rather than by NewRefViewManager
		// (no test does today, and the constructor is the only production
		// door) still throttles instead of panicking on the write below.
		m.lastBaseDemand = map[string]time.Time{}
	}
	// Drop stamps the throttle can no longer act on. An entry older than the
	// interval refuses nothing, so deleting it changes no decision, and it
	// keeps a map written on a CLIENT-DRIVEN path from being unbounded: the
	// map holds at most the repositories asked for within the last interval,
	// however many prefixes a manager is ever handed. The scan is over that
	// same tiny set, on a path that runs at most once per selection.
	for prefix, at := range m.lastBaseDemand {
		if now.Sub(at) >= dedicatedBaseDemandInterval {
			delete(m.lastBaseDemand, prefix)
		}
	}
	if last, asked := m.lastBaseDemand[repoPrefix]; asked && now.Sub(last) < dedicatedBaseDemandInterval {
		m.baseDemandMu.Unlock()
		return
	}
	m.lastBaseDemand[repoPrefix] = now
	m.baseDemandMu.Unlock()
	m.requestBase(repoPrefix)
}

// activeIsCurrent reports whether the generation the view already serves was
// built from exactly these inputs and is still readable. A fingerprint match
// settles the tree, the base and the extraction rules in one comparison; what
// it deliberately says nothing about is the ref or the commit, which is what
// makes a moved ref on an unchanged tree a metadata update.
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
	buildCtx, releaseBuild, admissionErr := m.lifetime.admit(ctx, true)
	if admissionErr != nil {
		closingCtx, cancel := context.WithTimeout(context.Background(), m.writerBudget)
		defer cancel()
		m.completeBuild(closingCtx, build, store_sqlite.ViewGenerationFailed, 0, admissionErr.Error())
		return RefViewResult{}, admissionErr
	}
	go func() {
		defer releaseBuild()
		stop := m.heartbeat(buildCtx, build)
		defer stop()
		release, err := m.gate.Acquire(buildCtx, ViewBuildInteractive)
		if err != nil {
			waitErr := fmt.Errorf("indexer: wait for ref-view build admission: %w", err)
			m.completeBuild(buildCtx, build, store_sqlite.ViewGenerationFailed, 0, waitErr.Error())
			if errors.Is(err, ErrViewBuildQueueFull) {
				viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewDeferred)
				m.logger.Debug("ref view manager: build deferred by admission capacity",
					zap.String("ref_view", view.RefViewID),
					zap.String("build_token", build.BuildToken),
					zap.Error(err))
				done <- outcome{result: RefViewResult{
					RefViewID: view.RefViewID,
					Resolved:  resolved,
					State:     store_sqlite.RefViewBuilding,
				}}
				return
			}
			done <- outcome{err: waitErr}
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
	generationID, report, buildErr := m.builder.BuildCommitLayer(ctx, CommitLayerRequest{
		Identity:      identity,
		Base:          m.store.AtGeneration(base.generationID),
		RepoDir:       req.RepoDir,
		BaseTreeOID:   base.treeOID,
		TargetTreeOID: resolved.TreeOID,
		RootPath:      req.RepoDir,
		RepoPrefix:    req.RepoPrefix,
		WorkspaceID:   req.WorkspaceID,
		ProjectID:     req.ProjectID,
	})
	if m.buildBarrier != nil {
		m.buildBarrier()
	}
	if buildErr != nil {
		failure := classifyRefViewBuildError(buildErr)
		m.completeBuild(ctx, build, store_sqlite.ViewGenerationFailed, 0, failure.Error())
		result, err := m.failed(ctx, view, failure)
		result.Built = true
		return result, err
	}
	if report.ClosureTruncated {
		m.logger.Warn("ref view manager: build closure truncated",
			zap.String("ref_view", view.RefViewID), zap.Int64("generation", generationID),
			zap.Int("cap", report.ClosureCap))
	}

	// Revalidation, git side: what does the selector name now? A tree that
	// moved means the payload describes a state the view has left.
	published, err := gitstate.ResolveViewSelector(ctx, req.RepoDir, req.SelectorKind, req.SelectorValue)
	if err != nil {
		m.supersede(ctx, build, generationID)
		result, failErr := m.failed(ctx, view, err)
		result.Built = true
		return result, failErr
	}
	if published.TreeOID != resolved.TreeOID {
		return m.superseded(ctx, build, generationID, view, published), nil
	}

	// Revalidation, catalog side: is the row still asking for what was built,
	// at the epoch the build captured, and does this pass still hold the claim
	// it started under? The ref and commit stamped beside the generation are
	// the ones current AT PUBLISH, which is how a commit that moved over an
	// unchanged tree lands without a second pass. The adoption closes the
	// attempt in the same transaction, so a pass whose claim was reclaimed
	// while it ran publishes nothing.
	now := time.Now().Unix()
	err = m.catalog.AdoptRefViewGeneration(ctx, store_sqlite.AdoptRefViewGenerationRequest{
		RefViewID:                       view.RefViewID,
		ExpectedRouteEpoch:              build.CapturedRouteEpoch,
		ExpectedDesiredTree:             resolved.TreeOID,
		ExpectedDesiredBuildFingerprint: build.BuildFingerprint,
		BuildID:                         build.BuildID,
		BuildToken:                      build.BuildToken,
		LastProgress:                    now,
		GenerationID:                    generationID,
		ActiveRef:                       published.FullRef,
		ActiveCommit:                    published.CommitOID,
		ActiveTree:                      published.TreeOID,
		ActiveBuildFingerprint:          build.BuildFingerprint,
		ExactView:                       true,
		LastResolved:                    now,
		LastSelected:                    now,
	})
	if err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return m.superseded(ctx, build, generationID, view, published), nil
		}
		return RefViewResult{}, err
	}
	viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewAdopted)
	return RefViewResult{
		RefViewID:    view.RefViewID,
		GenerationID: generationID,
		Resolved:     published,
		State:        store_sqlite.RefViewReady,
		Built:        true,
	}, nil
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
func (m *RefViewManager) identity(
	ctx context.Context, req RefViewRequest, viewID string, base primaryBase, targetTree string,
) GenerationIdentity {
	keys := m.identityKeysFor(ctx, req)
	return GenerationIdentity{
		OwnerKind:            refViewOwnerKind,
		GraphID:              base.graphID,
		LayerID:              refViewLayerID(viewID),
		GenerationKind:       CommitLayerGenerationKind,
		BaseGenerationID:     base.generationID,
		LowerViewFingerprint: base.treeOID,
		TreeOID:              targetTree,
		ConfigHash:           keys.configHash,
		ExtractorVersions:    m.extractors,
		ResolverVersion:      m.resolverVersion,
		DependencyRevision:   keys.dependencyRevision,
	}
}

// InvalidateDependencyCohort drops the manager's cached identity keys so the
// next selection derives them again.
//
// The keys are cached for the same reason the coordinator caches its cohort:
// describing one takes a daemon-wide roster read lease and a catalog read per
// in-scope member, and a selection that answers "already current" must not pay
// that. The cache is therefore refreshed on events — a repository registered or
// closed, a sibling's tree moved, the configuration reloaded — and this is the
// entry point for them. Without it a certified revision would be frozen for the
// life of the manager, which is a freshness certificate that stops being true.
//
// The checkout lifecycle is the event source in production: it calls this (and
// the coordinators' counterpart) when a repository owner is registered, when a
// repository's registry entry is torn down, and when the repository
// configuration is reloaded. The remaining source — a workspace SIBLING's HEAD
// or committed tree moving without any lifecycle event — lives in the git
// watcher and is NOT wired yet; until it is, a certified revision can name a
// sibling tree OID that has since moved. The window is bounded by the topology
// token below (membership moves are self-observed) and by the rule that every
// degraded description is re-derived on the next selection.
func (m *RefViewManager) InvalidateDependencyCohort(reason string) {
	if m == nil {
		return
	}
	m.identityMu.Lock()
	m.identityKeys = map[string]refViewIdentityKeys{}
	// The held degraded identities go with them: they are steady only for as
	// long as nothing said the inputs moved, and this is that statement.
	m.identityRefused = map[string]refViewIdentityKeys{}
	m.identityMu.Unlock()
	m.logger.Debug("ref view: dependency cohort invalidated", zap.String("reason", reason))
}

// InvalidateDependencyCohortFor is InvalidateDependencyCohort narrowed to the
// targets one repository's move can have reached.
//
// A manager serves one repository but is handed its target per request, so its
// memo can hold entries for more than one workspace — and for the empty
// workspace, which is what a request that names none carries. An event that
// moved ONE workspace's inputs must not make every other target pay a fresh
// description — a roster lease and a catalog read per in-scope member — for an
// answer that did not move.
//
// Two things match, and the second is why the repository is an argument at
// all. A target in the event's workspace names the moved repository's bytes
// through the workspace scope. A target whose repository IS the moved one
// names them whatever workspace it was requested under, INCLUDING the empty
// one: such an entry is repository-scoped, its topology token is the constant
// repository-scope string, so nothing else in this manager could ever move it.
//
// Both empty drops every entry, which is what a caller that can name neither
// means.
func (m *RefViewManager) InvalidateDependencyCohortFor(repoPrefix, workspaceID, reason string) {
	if m == nil {
		return
	}
	if workspaceID == "" && repoPrefix == "" {
		m.InvalidateDependencyCohort(reason)
		return
	}
	matches := func(keys refViewIdentityKeys) bool {
		if workspaceID != "" && keys.workspaceID == workspaceID {
			return true
		}
		return repoPrefix != "" && keys.repoPrefix == repoPrefix
	}
	dropped := 0
	m.identityMu.Lock()
	for key, keys := range m.identityKeys {
		if matches(keys) {
			delete(m.identityKeys, key)
			dropped++
		}
	}
	for key, keys := range m.identityRefused {
		if matches(keys) {
			delete(m.identityRefused, key)
		}
	}
	m.identityMu.Unlock()
	if dropped == 0 {
		return
	}
	m.logger.Debug("ref view: dependency cohort invalidated",
		zap.String("repo", repoPrefix), zap.String("workspace", workspaceID),
		zap.String("reason", reason), zap.Int("targets", dropped))
}

// refViewIdentityKeys are the two identity columns a ref view derives from its
// target rather than from its selector: the widened configuration digest and
// the dependency cohort revision.
//
// repoPrefix, workspaceID and topology are not identity columns; they are what
// decides whether this entry may still be served. The first two are the
// target's, so a narrowed invalidation can drop exactly the entries one
// repository's move reached — including a target requested with no workspace
// at all, which no workspace-keyed event could ever name. topology is the
// cheap workspace-membership observation the cohort was described under — the
// same one a coordinator's poll re-checks — so a repository tracked into or
// out of the workspace re-describes without needing an event to reach the
// manager.
type refViewIdentityKeys struct {
	configHash         string
	dependencyRevision string
	repoPrefix         string
	workspaceID        string
	topology           string
}

// identityKeysFor derives — and memoizes — the two target-derived identity
// columns.
//
// They were both wrong before this: the config digest covered
// config.IndexConfig alone, so a ref view stayed reusable across an artifacts /
// semantic / LSP / workspace / project change, and the dependency revision was
// left empty, which generationIdentityKey renders byte-for-byte as the legacy
// pre-cohort key that every stored generation matches. A ref view composes over
// the same corpus a checkout layer does and is built by the same builder, so it
// carries the same two columns, derived the same way.
//
// The target triple is the memo key rather than the manager's construction
// arguments because a manager is built per repository but handed its target by
// each request (RefViewRequest.RepoPrefix / WorkspaceID / ProjectID).
//
// Only a CERTIFIED description is memoized. A refusal is a fact about the
// moment, not about the cohort — a roster that moved under the lease, a
// workspace sibling that is tracked but not yet indexed, admissions closing —
// and every one of them clears on its own. Caching one would pin a degraded
// revision for the life of the manager, which is the state a ref view never
// leaves on its own: unlike a coordinator, nothing here re-describes per build.
func (m *RefViewManager) identityKeysFor(ctx context.Context, req RefViewRequest) refViewIdentityKeys {
	key := req.RepoPrefix + "\x00" + req.WorkspaceID + "\x00" + req.ProjectID
	// Sampled BEFORE the description, for the reason the coordinator samples it
	// there: a membership change that lands while the cohort is being described
	// leaves the entry recorded under the OLDER token, so the next selection
	// describes again rather than settling on a token the description never saw.
	topology := m.cohortTopology(req)
	m.identityMu.Lock()
	if cached, found := m.identityKeys[key]; found && cached.topology == topology {
		m.identityMu.Unlock()
		return cached
	}
	m.identityMu.Unlock()

	sections := m.sectionsFor(req.RepoPrefix)

	cohort := dependencyCohortSource{
		Target: DependencyRevisionTarget{
			RepoPrefix:  req.RepoPrefix,
			WorkspaceID: req.WorkspaceID,
			ProjectID:   req.ProjectID,
		},
		Leases:           m.leases,
		Catalog:          m.catalog,
		WorkspaceMembers: builderWorkspaceMembers(m.builder, req.WorkspaceID),
		Config:           m.config,
		ConfigSections:   sections,
		Ownership: []DependencyRevisionOwnership{{
			RepoPrefix: req.RepoPrefix,
			Language:   "go",
			Owner:      goPackageOwnershipTargetEvidence,
		}},
		Producers:         cohortProducerPolicy(m.config, m.builder != nil && m.builder.Embedder != nil),
		Capabilities:      cohortCapabilityVocabulary(),
		ExtractorVersions: m.extractors,
		SourceBudget:      dependencyRevisionSourceBudget,
	}

	keys := refViewIdentityKeys{
		repoPrefix: req.RepoPrefix, workspaceID: req.WorkspaceID, topology: topology,
	}
	_, fingerprint, err := snapshotDedicatedBaseConfig(
		m.config, req.RepoPrefix, req.WorkspaceID, req.ProjectID)
	if err != nil {
		// The same fail-safe indexConfigHash has always used for a
		// configuration that cannot be encoded: a digest nothing matches, so
		// such a build is its own identity and reuses nothing.
		keys.configHash = "unhashable-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		m.logger.Warn("ref view: the index configuration could not be digested; "+
			"this view's generations reuse nothing",
			zap.String("repo", req.RepoPrefix), zap.Error(err))
	} else {
		keys.configHash = checkoutConfigHash(fingerprint, sections)
	}

	revision, err := cohort.revision(ctx)
	if err != nil {
		revision = cohort.degradedRevision(dependencyCohortRefusalReason(err))
		m.logger.Debug("ref view: the resolver-visible input cohort could not be described; "+
			"this view's generations carry a degraded revision",
			zap.String("repo", req.RepoPrefix), zap.Error(err))
	}
	keys.dependencyRevision = revision

	// Only a certificate is memoized. Every refusal — the request's own context
	// ending, a roster that moved under the lease, admissions stopping, a
	// workspace sibling that is tracked but not yet indexed — is transient by
	// construction, and the manager has no build path that would re-describe it
	// later. Caching one therefore does not save a description, it permanently
	// replaces every future certificate with the degraded value this one moment
	// produced. Answer THIS call with it; let the next selection describe again.
	//
	// What that costs, stated rather than hidden: while a cohort stays
	// undescribable every selection pays one description — a roster read lease
	// and a read per in-scope member — where a memoized refusal would have paid
	// it once. That is the price of noticing the moment it clears, and it is
	// strictly less than the builds a frozen false identity would trigger. It
	// is NOT the coordinator's degraded-poll rule: a poll fires on a timer for
	// every checkout in the daemon, a selection only when someone asks.
	//
	// The one thing that is not left to re-derive is the ANSWER. A refusal's
	// degraded revision carries the refusal's reason, so a transient whose
	// class flaps — roster-moved on one selection, raw-pending on the next —
	// would hand out a different identity each time, and a ref view's
	// fingerprint moving is a BUILD. So the degraded identity a target was
	// last served under is held steady for as long as the refusals last: still
	// re-described every selection, still replaced the moment one certifies,
	// still dropped by any invalidation that says the inputs moved.
	if err != nil {
		m.identityMu.Lock()
		if held, found := m.identityRefused[key]; found && held.topology == topology {
			keys.dependencyRevision = held.dependencyRevision
		} else {
			m.identityRefused[key] = keys
		}
		m.identityMu.Unlock()
		return keys
	}
	m.identityMu.Lock()
	m.identityKeys[key] = keys
	delete(m.identityRefused, key)
	m.identityMu.Unlock()
	return keys
}

// sectionsFor is the configuration domains outside config.IndexConfig that
// this description keys on, in the order of how current each source is.
//
// A live source (ConfigSectionsFor, what CheckoutLifecycle.refViewManager
// passes) is asked first, because a manager outlives the configuration it was
// built under: it is cached per repository for the life of the daemon, and a
// reload swaps that repository's config.Config underneath it. Second is the
// construction-time list, which is what a caller with a fixed configuration
// has. Last is the builder link — the live Indexer's MultiIndexer and ITS
// ConfigManager — which answers for a manager nobody handed sections to, and
// nil for one whose Indexer has no mutation owner. An EMPTY answer is not
// neutral: it collapses the widened configuration digest back onto
// config.IndexConfig alone, so a stored generation stays reusable across an
// artifacts / semantic / LSP / workspace / project change.
//
// Called on a memo MISS only, which is the same call that is about to take a
// roster lease and a catalog read per in-scope member.
func (m *RefViewManager) sectionsFor(repoPrefix string) []DependencyRevisionConfigSection {
	if m.configSectionsFor != nil {
		if live := m.configSectionsFor(repoPrefix); len(live) > 0 {
			return live
		}
	}
	if len(m.configSections) > 0 {
		return m.configSections
	}
	return builderConfigSections(m.builder, repoPrefix)
}

// cohortTopology is the cheap workspace-membership observation the memo is
// keyed on. It reads only the MultiIndexer's repository topology (a map walk
// under that struct's own read lock) — no roster lease, no catalog read — so a
// selection that answers "already current" can pay it every time.
func (m *RefViewManager) cohortTopology(req RefViewRequest) string {
	return dependencyCohortSource{
		Target: DependencyRevisionTarget{
			RepoPrefix:  req.RepoPrefix,
			WorkspaceID: req.WorkspaceID,
			ProjectID:   req.ProjectID,
		},
		WorkspaceMembers: builderWorkspaceMembers(m.builder, req.WorkspaceID),
	}.topologyToken()
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
	sum := sha256.Sum256([]byte(generationIdentityKey(identity) + profile))
	return hex.EncodeToString(sum[:16])
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
