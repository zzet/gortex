// Package reach precomputes per-depth incoming-reachability sets on
// every impact-seed node so blast-radius queries (AnalyzeImpact,
// explain_change_impact, simulate_chain step-impact, prompt
// SafeToChange / PreCommit, diff_context) answer in O(seeds × reach)
// map lookups instead of a live BFS.
//
// The package depends only on internal/graph; it is imported by the
// indexer (build site) and the analysis package (consumer) so the
// import graph stays acyclic — analysis already imports indexer in
// its bench tests.
package reach

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/progress"
)

// Reachability index keys. Each value is a []string of node IDs that
// can reach the carrier node via incoming edges within the named
// number of hops. Tiers are per-depth (not cumulative) so they map
// 1:1 onto AnalyzeImpact's ByDepth tiers.
//
// Parallel `*_conf` and `*_label` keys carry the representative
// in-edge's Confidence and ConfidenceLabel for each ID, indexed by
// position. They turn the fast path into a pure lookup — no
// GetInEdges calls at query time — so a precomputed AnalyzeImpact
// stays sub-ms even on graphs with high fan-in.
//
// Eager records are stored on Node.Meta. A process epoch deliberately rejects
// records persisted by a prior daemon process: graph topology may have changed
// while the restart-local numeric build counter reused the same value.
const (
	MetaReachD1      = "reach_d1"
	MetaReachD2      = "reach_d2"
	MetaReachD3      = "reach_d3"
	MetaReachD1Conf  = "reach_d1_conf"
	MetaReachD2Conf  = "reach_d2_conf"
	MetaReachD3Conf  = "reach_d3_conf"
	MetaReachD1Label = "reach_d1_label"
	MetaReachD2Label = "reach_d2_label"
	MetaReachD3Label = "reach_d3_label"
	// MetaReachComplete is the publication marker for a reach record.
	// A matching generation is not sufficient on its own: older code
	// stamped MetaReachBuild before it finished writing the tier keys, so
	// a concurrent lookup could observe a matching stamp with empty tiers
	// and return a false-safe zero-impact result. New records are assembled
	// on a copy of the node and published only after this marker is set.
	MetaReachComplete = "reach_complete"
	// MetaReachEpoch identifies the daemon process that published a record.
	// The numeric build counter restarts with each process; without a separate
	// epoch, a warm database can make a new build generation collide with an
	// old persisted record and temporarily expose stale or empty tiers.
	MetaReachEpoch = "reach_epoch"
	// MetaReachTruncated marks a complete-but-bounded record. Its tiers are
	// valid lower-bound evidence, but callers must not interpret their end as
	// proof that no more dependents exist.
	MetaReachTruncated = "reach_truncated"

	// MetaReachBuild is a monotonic build-generation counter stamped
	// on every node the indexer touched in the most recent reach pass.
	// Consumers require both a counter match and MetaReachComplete before
	// trusting the precomputed sets. The Meta value is a uint64.
	MetaReachBuild = "reach_build"

	// reachPublishBatchSize bounds the temporary copy-on-write node set
	// retained by eager builds and clears. A few thousand rows keeps SQLite
	// transaction amortisation while avoiding a full-graph duplicate of every
	// Node and Meta map at peak.
	reachPublishBatchSize = 256

	// Lazy impact lookup is an interactive safety gate. A pathological hub
	// must return a conservative lower bound promptly, not hold the resolver
	// mutex for minutes while expanding an unbounded breadth-first frontier.
	maxLookupEdges = 5000
	lookupTimeout  = 3 * time.Second
	lockPoll       = 2 * time.Millisecond
)

var reachMetaKeys = [...]string{
	MetaReachD1, MetaReachD2, MetaReachD3,
	MetaReachD1Conf, MetaReachD2Conf, MetaReachD3Conf,
	MetaReachD1Label, MetaReachD2Label, MetaReachD3Label,
	MetaReachBuild, MetaReachComplete, MetaReachEpoch, MetaReachTruncated,
}

// ReachableEdge returns true when an edge participates in the impact
// graph. Mirrors AnalyzeImpact's filter exactly so the precomputed
// sets and the live walk agree on membership. Exported so the
// AnalyzeImpact live-walk path can share the same filter and tests
// can assert filter parity across the two code paths.
func ReachableEdge(k graph.EdgeKind) bool {
	return k != graph.EdgeDefines && k != graph.EdgeMemberOf
}

// ImpactSeedKind returns true for node kinds that are sensible impact
// seeds — the symbols a developer actually changes. Files, imports,
// parameters, and similar wiring kinds carry no useful blast radius,
// so we skip them to keep the index lean.
func ImpactSeedKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod,
		graph.KindType, graph.KindInterface,
		graph.KindField, graph.KindEnumMember,
		graph.KindConstant, graph.KindVariable:
		return true
	}
	return false
}

// Stats reports the work BuildIndex did.
type Stats struct {
	NodesIndexed int    // nodes that received reach_d* entries
	EntriesD1    int    // total reach_d1 IDs across all indexed nodes
	EntriesD2    int    // total reach_d2 IDs
	EntriesD3    int    // total reach_d3 IDs
	Build        uint64 // generation the build read the graph at
}

// buildCounter is the process-wide source of reach generations: every
// invalidation, of the whole graph or of some repositories, takes its next
// value, so a generation never repeats within a process. reachProcessEpoch
// prevents a counter value reused by a restarted process from matching a
// record persisted by the previous process.
var (
	buildCounter      uint64
	reachProcessEpoch = newReachProcessEpoch()
)

func newReachProcessEpoch() string {
	var token [16]byte
	if _, err := rand.Read(token[:]); err == nil {
		return hex.EncodeToString(token[:])
	}
	// crypto/rand failure is exceptional. A nanosecond process-start token still
	// keeps the safety marker independent from the restart-local build counter.
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// topologyGate is the writer-preferring reader/writer state of one scope: the
// whole graph, or one repository prefix.
type topologyGate struct {
	readers        int
	writers        int
	writersWaiting int
}

// topologyState serialises impact readers and eager reach maintenance with
// watcher topology transactions, per repository. It is separate from
// Store.ResolveMutex: watcher indexing calls resolver methods that acquire
// ResolveMutex internally, so holding that non-reentrant mutex around the
// whole patch would deadlock. Readers always acquire a gate before
// ResolveMutex; a watcher holds the writer side while its nested resolver
// calls take ResolveMutex.
//
// A scope is nil for the whole graph, or a sorted list of repository
// prefixes. One mutex guards every gate, so a mutation takes all of its gates
// in one step and gates need no lock order among themselves. Waiters sleep on
// wake, which is closed and replaced whenever a holder leaves.
//
// The state is process-global, like buildCounter. Repository gates are created
// on first use and dropped once idle, so the map holds only work in flight.
// Production multi-repo indexers share one Store; tests that reuse a prefix
// across stores only over-serialise.
type topologyState struct {
	mu    sync.Mutex
	wake  chan struct{}
	all   topologyGate
	repos map[string]*topologyGate
	// Readers, writers and waiting writers of repository scopes, so that
	// whole-graph admission need not scan repos.
	repoReaders, repoWriters, repoWritersWaiting int

	// allBuild is the generation of the last whole-graph invalidation, and
	// repoBuild the generation of each repository invalidated since.
	allBuild  uint64
	repoBuild map[string]uint64

	coupling repoCoupling
}

var topology = &topologyState{
	wake:      make(chan struct{}),
	repos:     make(map[string]*topologyGate),
	repoBuild: make(map[string]uint64),
}

// readable reports whether a reader of scope may enter: no writer holds or
// awaits any gate the reader needs. t.mu is held.
func (t *topologyState) readable(scope []string) bool {
	if t.all.writers > 0 || t.all.writersWaiting > 0 {
		return false
	}
	if scope == nil {
		return t.repoWriters == 0 && t.repoWritersWaiting == 0
	}
	for _, prefix := range scope {
		if gate := t.repos[prefix]; gate != nil && (gate.writers > 0 || gate.writersWaiting > 0) {
			return false
		}
	}
	return true
}

// writable reports whether a writer of scope may enter. A repository writer
// also yields to a waiting whole-graph writer so the latter cannot starve.
// t.mu is held.
func (t *topologyState) writable(scope []string) bool {
	if t.all.writers > 0 || t.all.readers > 0 {
		return false
	}
	if scope == nil {
		return t.repoReaders == 0 && t.repoWriters == 0
	}
	if t.all.writersWaiting > 0 {
		return false
	}
	for _, prefix := range scope {
		if gate := t.repos[prefix]; gate != nil && (gate.writers > 0 || gate.readers > 0) {
			return false
		}
	}
	return true
}

// add moves every gate of scope by the given deltas. t.mu is held.
func (t *topologyState) add(scope []string, readers, writers, waiting int) {
	if scope == nil {
		t.all.readers += readers
		t.all.writers += writers
		t.all.writersWaiting += waiting
		return
	}
	t.repoReaders += readers
	t.repoWriters += writers
	t.repoWritersWaiting += waiting
	for _, prefix := range scope {
		gate := t.repos[prefix]
		if gate == nil {
			gate = &topologyGate{}
			t.repos[prefix] = gate
		}
		gate.readers += readers
		gate.writers += writers
		gate.writersWaiting += waiting
		if *gate == (topologyGate{}) {
			delete(t.repos, prefix)
		}
	}
}

// wait sleeps until a holder leaves and reports false when ctx ends first.
// t.mu is held on entry and on return.
func (t *topologyState) wait(ctx context.Context) bool {
	wake := t.wake
	t.mu.Unlock()
	defer t.mu.Lock()
	select {
	case <-wake:
		return true
	case <-ctx.Done():
		return false
	}
}

// wakeWaiters lets every waiter re-check its admission. t.mu is held.
func (t *topologyState) wakeWaiters() {
	close(t.wake)
	t.wake = make(chan struct{})
}

// retire moves the reach generation of scope past every value issued so far.
// t.mu is held.
func (t *topologyState) retire(scope []string) {
	build := atomic.AddUint64(&buildCounter, 1)
	if scope == nil {
		t.allBuild = build
		clear(t.repoBuild)
		return
	}
	for _, prefix := range scope {
		t.repoBuild[prefix] = build
	}
}

// retireWhole is retire for the whole graph, taking t.mu.
func retireWhole() {
	topology.mu.Lock()
	defer topology.mu.Unlock()
	topology.retire(nil)
}

// generation is the build a current record of a seed in scope carries. A seed
// no repository owns follows every invalidation.
func generation(scope []string) uint64 {
	if scope == nil {
		return atomic.LoadUint64(&buildCounter)
	}
	t := topology
	t.mu.Lock()
	defer t.mu.Unlock()
	build := t.allBuild
	for _, prefix := range scope {
		build = max(build, t.repoBuild[prefix])
	}
	return build
}

// beginTopologyRead waits until no writer holds or awaits a gate of scope, or
// until ctx ends.
func beginTopologyRead(ctx context.Context, scope []string) (func(), bool) {
	t := topology
	t.mu.Lock()
	for !t.readable(scope) {
		if !t.wait(ctx) {
			t.mu.Unlock()
			return nil, false
		}
	}
	t.add(scope, 1, 0, 0)
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		t.add(scope, -1, 0, 0)
		if t.all.writersWaiting > 0 || t.repoWritersWaiting > 0 {
			t.wakeWaiters()
		}
		t.mu.Unlock()
	}, true
}

func lockContext(ctx context.Context, mu *sync.Mutex) bool {
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(lockPoll):
		}
	}
	return true
}

// detachedNodeReader is a store whose reads stay consistent without its
// ResolveMutex, so a lookup does not wait behind a resolver or enrichment pass
// that holds it.
type detachedNodeReader interface {
	NodeReadsDetached() bool
}

// lockResolve takes g's ResolveMutex for a read, unless g's reads are detached
// from it. A record write always takes the mutex: it rewrites whole node rows
// that a pass holding the mutex may be rewriting too.
func lockResolve(ctx context.Context, g graph.Store) (unlock func(), ok bool) {
	observer := coordinationWaitObserver(ctx)
	if detached, isDetached := g.(detachedNodeReader); isDetached && detached.NodeReadsDetached() {
		if observer != nil {
			observer(CoordinationWait{Phase: "resolve_mutex_wait", Acquired: true})
		}
		return func() {}, true
	}
	var started time.Time
	if observer != nil {
		started = time.Now()
	}
	mu := g.ResolveMutex()
	ok = lockContext(ctx, mu)
	if observer != nil {
		observer(CoordinationWait{Phase: "resolve_mutex_wait", Duration: time.Since(started), Attempted: true, Acquired: ok})
	}
	if !ok {
		return nil, false
	}
	return mu.Unlock, true
}

// viewGenerationPinned is implemented by a store handle bound to one payload
// view generation. A handle that does not report one reads and writes the base
// corpus.
type viewGenerationPinned interface {
	ViewGeneration() int64
}

// WritesBaseTopology reports whether writes through g can change what the
// reach index describes.
//
// Records are built over the base corpus and stamped on its nodes, and every
// consumer reads them through the base store itself: a routed view answers
// about a checkout the base index never saw, so its reader is sent to a live
// walk instead. A handle pinned to a derived payload generation therefore
// writes rows no record covers and no reach reader visits, and a pass over it
// needs neither side of the gate — its own reads are generation-scoped, so it
// observes nothing a base mutation can tear either.
//
// A store that reports no generation is treated as the base corpus. Taking the
// gate for a mutation that did not need it costs concurrency; skipping it for
// one that did costs correctness.
func WritesBaseTopology(g graph.Store) bool {
	if g == nil {
		return false
	}
	pinned, ok := g.(viewGenerationPinned)
	return !ok || pinned.ViewGeneration() == 0
}

// BeginTopologyMutation prevents reach lookups/builds from observing a
// watcher's parse-then-swap and resolver work half-applied. It holds the whole
// graph: use BeginRepoTopologyMutation when the mutation belongs to one
// repository. The returned function must be called exactly once; changed=true
// invalidates every cached generation, and the repository coupling, before
// waiting readers are released. A separate gate is required because the
// mutation body itself invokes resolver methods that acquire the store's
// non-reentrant ResolveMutex.
//
// A mutation that cannot reach the base corpus takes nothing and invalidates
// nothing. The gates are process-global, so a generation-pinned build that held
// them would serialise every unrelated store and pass in the daemon behind a
// pass whose duration is its payload's, not the corpus's.
func BeginTopologyMutation(g graph.Store) func(changed bool) {
	if !WritesBaseTopology(g) {
		return func(bool) {}
	}
	finish := beginTopologyWrite(g, "")
	return func(changed bool) { finish(changed, nil) }
}

// BeginRepoTopologyMutation is BeginTopologyMutation for a mutation of the
// rows of repository prefix. It waits only for the readers and writers of the
// repositories coupled with prefix. The whole graph is held instead for an
// empty prefix, and while the store's coupling is unknown; the first mutation
// that needs it starts reading it in the background.
//
// The returned function must be called exactly once. frontier lists the files
// the mutation may have added or rebound edges in or at the nodes of. With
// changed=true, the couplings those edges make are learned first, and then the
// records of every repository coupled with prefix are invalidated. A nil
// frontier says edges anywhere may have moved: every record is invalidated and
// the coupling forgotten.
func BeginRepoTopologyMutation(g graph.Store, prefix string) func(changed bool, frontier []string) {
	if !WritesBaseTopology(g) {
		return func(bool, []string) {}
	}
	if prefix != "" {
		ensureRepoCoupling(g)
	}
	return beginTopologyWrite(g, prefix)
}

func beginTopologyWrite(g graph.Store, prefix string) func(changed bool, frontier []string) {
	t := topology
	t.mu.Lock()
	scope, epoch := t.writeScope(g, prefix)
	t.add(scope, 0, 0, 1)
	for !t.writable(scope) {
		t.wait(context.Background())
		if epoch != t.coupling.epoch {
			// The coupling the scope came from was retired while this writer
			// waited.
			t.add(scope, 0, 0, -1)
			scope, epoch = t.writeScope(g, prefix)
			t.add(scope, 0, 0, 1)
		}
	}
	t.add(scope, 0, 1, -1)
	t.mu.Unlock()

	var once sync.Once
	return func(changed bool, frontier []string) {
		once.Do(func() {
			if changed && prefix != "" && frontier != nil {
				learnRepoCoupling(g, frontier)
			}
			t.mu.Lock()
			if changed {
				if prefix == "" || frontier == nil {
					// Edges anywhere may have moved.
					t.coupling.forget(g)
				}
				// What the mutation held, widened by the couplings it bound; the
				// whole graph when either is unknown.
				retired, _ := t.writeScope(g, prefix)
				if scope == nil || retired == nil {
					retired = nil
				} else {
					retired = append(retired, scope...)
				}
				t.retire(retired)
			}
			t.add(scope, 0, -1, 0)
			t.wakeWaiters()
			t.mu.Unlock()
		})
	}
}

// writeScope is the gate set a mutation of prefix holds: the repositories
// coupled with it, or the whole graph (nil) for an empty prefix or while the
// coupling of g is unknown. The epoch tells a waiting writer when to ask
// again. t.mu is held.
func (t *topologyState) writeScope(g graph.Store, prefix string) ([]string, uint64) {
	c := &t.coupling
	if prefix == "" || !c.ready || c.store != g {
		return nil, c.epoch
	}
	return c.component(prefix), c.epoch
}

// repoCoupling groups the repositories of one store whose reach records can
// cross: an edge joins them, directly or through a node no repository owns. A
// mutation of one member can change the records of every member of its group.
//
// It is read from the store in the background once per epoch, grows with what
// each repository mutation learns from its frontier, and is forgotten by any
// mutation that may have moved edges anywhere. A store whose read fails is not
// read again. Only one store is tracked; a mutation of another store holds the
// whole graph until it is read again.
type repoCoupling struct {
	store   graph.Store
	epoch   uint64
	ready   bool
	seeding bool
	missed  bool
	// pending holds what mutations learned while the read was in flight.
	pending [][2]string
	parent  map[string]string
}

// couplingReadBudget bounds each coupling read. A store whose read misses it
// keeps whole-graph mutations until the process restarts.
const couplingReadBudget = 2 * time.Second

// repoCouplingSource reports the distinct repository pairs the store's edges
// join: all of them, or those at the nodes of some files.
type repoCouplingSource interface {
	RepoCouplingPairs(ctx context.Context) ([][2]string, error)
	RepoCouplingPairsForFiles(ctx context.Context, files []string) ([][2]string, error)
}

func (c *repoCoupling) find(prefix string) string {
	for {
		parent, ok := c.parent[prefix]
		if !ok || parent == prefix {
			return prefix
		}
		prefix = parent
	}
}

func (c *repoCoupling) union(a, b string) {
	ra, rb := c.find(a), c.find(b)
	if ra == rb {
		return
	}
	if c.parent == nil {
		c.parent = make(map[string]string)
	}
	c.parent[ra] = rb
	c.parent[rb] = rb
	// Queued writers must recompute their gate set when components grow.
	c.epoch++
}

// component lists prefix and every repository coupled with it, sorted.
func (c *repoCoupling) component(prefix string) []string {
	root := c.find(prefix)
	scope := []string{prefix}
	for member := range c.parent {
		if member != prefix && c.find(member) == root {
			scope = append(scope, member)
		}
	}
	sort.Strings(scope)
	return scope
}

// reset starts a new epoch for g with nothing known. A store whose read failed
// stays missed.
func (c *repoCoupling) reset(g graph.Store) {
	if c.store != g {
		c.missed = false
	}
	c.store, c.epoch = g, c.epoch+1
	c.ready, c.seeding, c.pending, c.parent = false, false, nil, nil
}

// forget drops what is known about g's coupling.
func (c *repoCoupling) forget(g graph.Store) {
	if c.store == g {
		c.reset(g)
	}
}

// miss gives up on the current store's coupling for the life of the process.
func (c *repoCoupling) miss() {
	c.reset(c.store)
	c.missed = true
}

// ensureRepoCoupling starts reading g's coupling in the background, once per
// epoch. Nothing waits for the read.
func ensureRepoCoupling(g graph.Store) {
	source, ok := g.(repoCouplingSource)
	if !ok {
		return
	}
	t := topology
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &t.coupling
	if c.store != g {
		c.reset(g)
	}
	if c.ready || c.seeding || c.missed {
		return
	}
	c.seeding = true
	go readRepoCoupling(g, source, c.epoch)
}

// readRepoCoupling reads g's coupling for epoch. It takes no gate, so its
// snapshot can miss edges of a mutation that runs beside it; such a mutation
// learns its own couplings into pending, which the read adds on landing.
func readRepoCoupling(g graph.Store, source repoCouplingSource, epoch uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), couplingReadBudget)
	pairs, err := source.RepoCouplingPairs(ctx)
	cancel()
	t := topology
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &t.coupling
	if c.store != g || c.epoch != epoch {
		return
	}
	if err != nil {
		c.miss()
		return
	}
	for _, pair := range append(pairs, c.pending...) {
		c.union(pair[0], pair[1])
	}
	c.ready, c.seeding, c.pending = true, false, nil
}

// learnRepoCoupling adds the couplings that the edges at frontier make to g's
// coupling, while it is known or being read. A failed read gives the coupling
// up, since what the mutation bound is then unknown.
func learnRepoCoupling(g graph.Store, frontier []string) {
	source, ok := g.(repoCouplingSource)
	if !ok || len(frontier) == 0 {
		return
	}
	t := topology
	t.mu.Lock()
	c := &t.coupling
	epoch, wanted := c.epoch, c.store == g && (c.ready || c.seeding)
	t.mu.Unlock()
	if !wanted {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), couplingReadBudget)
	pairs, err := source.RepoCouplingPairsForFiles(ctx, frontier)
	cancel()

	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case c.store != g || c.epoch != epoch:
		// Forgotten meanwhile: the next read sees these edges.
	case err != nil:
		c.miss()
	case c.seeding:
		c.pending = append(c.pending, pairs...)
	default:
		for _, pair := range pairs {
			c.union(pair[0], pair[1])
		}
	}
}

// BuildIndex precomputes per-depth incoming reachability sets for
// every impact-seed node in g and stores them under Node.Meta as
// []string slices keyed reach_d1 / reach_d2 / reach_d3. Tiers are
// per-depth (a node appears in at most one tier per seed). The build
// generation is stamped under MetaReachBuild so consumers can detect
// stale entries after partial rebuilds.
//
// Cost: O(N · E_avg) where E_avg is the average reach-3 fan-in
// (typically <200 nodes per seed on real call graphs). Empirically
// completes in well under a second on 50k-node graphs. Run after all
// graph-shaping passes settle (resolver, semantic enrichment, cross-
// repo edges, gRPC stub resolution).
//
// Safe to call repeatedly: existing reach_d* entries are overwritten.
// Each record carries its repository's generation as of the build, which
// the build does not advance: the records of an unchanged graph are as
// current as their rebuild, and advancing it would make every concurrent
// lookup recompute.
func BuildIndex(g graph.Store) *Stats {
	return BuildIndexCtx(context.Background(), g)
}

// BuildIndexCtx is BuildIndex with intra-stage progress reporting.
// Pulls a progress.Reporter from ctx (no-op when none is attached) and
// emits per-seed progress every reachProgressEvery seeds — the pass
// otherwise looks hung from the outside, since "reach" is one of the
// longest stages on monorepo-scale graphs (~200 s on k8s with 150 k
// impact seeds). Pure operator-visibility instrumentation: the per-
// report call is cheap (no I/O when the reporter is the default no-op).
func BuildIndexCtx(ctx context.Context, g graph.Store) *Stats {
	if g == nil {
		return &Stats{}
	}
	reporter := progress.FromContext(ctx)
	releaseTopology, ok := beginTopologyRead(ctx, nil)
	if !ok {
		return &Stats{}
	}
	unlock, ok := lockResolve(ctx, g)
	if !ok {
		releaseTopology()
		return &Stats{}
	}
	build := atomic.LoadUint64(&buildCounter)
	unlock()
	releaseTopology()
	stats := &Stats{Build: build}

	// Reachability only needs identity and kind during the graph-wide seed
	// scan. On SQLite, AllNodes decodes every node's opaque metadata — including
	// prior reach payloads — and retains those blobs for the whole build. Use the
	// metadata-free projection and fetch a full node only for each bounded
	// publication batch below.
	nodes := graph.AllNodesLight(g)
	// Sort by ID so the deterministic iteration order produces stable
	// reach slices — important for snapshot determinism and for tests
	// that compare reach payloads across runs.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	// Pre-count impact seeds so the progress denominator is real, not
	// the total node count (the loop skips ~80% of nodes — files,
	// imports, params, vars, …).
	var seedTotal int
	for _, n := range nodes {
		if n != nil && ImpactSeedKind(n.Kind) {
			seedTotal++
		}
	}
	reporter.Report("reachability index", 0, seedTotal)

	const reachProgressEvery = 1000
	seedsDone := 0
	// Persist complete copy-on-write records in bounded batches. Traversal is
	// deliberately outside topology/resolve locks: a full eager build can take
	// minutes, and holding either lock for that duration starves synchronous
	// edits. Each short publication reacquires both locks and checks the build
	// generation; a topology mutation makes the pending batch stale and aborts
	// the build rather than publishing a mixed snapshot.
	type pendingRecord struct {
		id        string
		tiers     [3]tier
		truncated bool
	}
	batchCap := min(seedTotal, reachPublishBatchSize)
	pending := make([]pendingRecord, 0, batchCap)
	mu := g.ResolveMutex()
	publish := func() bool {
		if len(pending) == 0 {
			return true
		}
		release, acquired := beginTopologyRead(ctx, nil)
		if !acquired {
			return false
		}
		if !lockContext(ctx, mu) {
			release()
			return false
		}
		if build != atomic.LoadUint64(&buildCounter) {
			mu.Unlock()
			release()
			return false
		}
		stamped := make([]*graph.Node, 0, len(pending))
		for _, record := range pending {
			n := g.GetNode(record.id)
			if n == nil || !ImpactSeedKind(n.Kind) {
				continue
			}
			published := cloneNodeWithMeta(n)
			writeRecord(published.Meta, generation(nodeScope(n)), record.tiers, record.truncated)
			stamped = append(stamped, published)
		}
		if len(stamped) > 0 {
			g.AddBatch(stamped, nil)
		}
		mu.Unlock()
		release()
		pending = pending[:0]
		return true
	}
	for _, n := range nodes {
		if n == nil || !ImpactSeedKind(n.Kind) {
			continue
		}
		if ctx.Err() != nil || build != atomic.LoadUint64(&buildCounter) {
			break
		}
		tiers, truncated := compute(ctx, g, n.ID, nil)
		pending = append(pending, pendingRecord{id: n.ID, tiers: tiers, truncated: truncated})
		if len(pending) == reachPublishBatchSize && !publish() {
			break
		}
		stats.NodesIndexed++
		stats.EntriesD1 += len(tiers[0].IDs)
		stats.EntriesD2 += len(tiers[1].IDs)
		stats.EntriesD3 += len(tiers[2].IDs)

		seedsDone++
		if seedsDone%reachProgressEvery == 0 {
			reporter.Report("reachability index", seedsDone, seedTotal)
		}
	}
	// Flush the final partial batch. AddBatch with no edges only upserts the
	// nodes; a stale generation or cancelled context deliberately drops it.
	_ = publish()
	reporter.Report("reachability index", seedsDone, seedTotal)
	return stats
}

// tier holds the per-depth precomputed payload: a parallel triple of
// (ID, edge-confidence, edge-confidence-label) so the fast path can
// hydrate an ImpactEntry without a single GetInEdges call at query
// time. Sorted by ID for stable snapshot output and test parity.
type tier struct {
	IDs    []string
	Conf   []float64
	Labels []string
}

// setOrDeleteStrings keeps Meta lean — empty tiers are removed rather
// than stored as []string{} so cold-start gob payloads stay small and
// downstream code can rely on "key absent" == "no callers at this tier".
func setOrDeleteStrings(m map[string]any, key string, value []string) {
	if len(value) == 0 {
		delete(m, key)
		return
	}
	m[key] = value
}

// setOrDeleteFloats mirrors setOrDeleteStrings for the parallel
// confidence arrays.
func setOrDeleteFloats(m map[string]any, key string, value []float64) {
	if len(value) == 0 {
		delete(m, key)
		return
	}
	m[key] = value
}

// cloneNodeWithMeta makes a copy-on-write carrier for a reach record.
// Node.Meta is otherwise a regular Go map: editing the canonical map in
// place while Lookup reads it is both a data race and a partial-publication
// hazard. Values are shallow-copied because reach only replaces its own
// immutable slices and never mutates metadata owned by another subsystem.
func cloneNodeWithMeta(n *graph.Node) *graph.Node {
	clone := *n
	clone.Meta = make(map[string]any, len(n.Meta)+11)
	for key, value := range n.Meta {
		clone.Meta[key] = value
	}
	return &clone
}

// writeRecord writes every tier onto a private metadata map and sets the
// completeness marker last. The containing node must not be published to the
// Store until this function returns.
func writeRecord(meta map[string]any, build uint64, tiers [3]tier, truncated bool) {
	delete(meta, MetaReachComplete)
	setOrDeleteStrings(meta, MetaReachD1, tiers[0].IDs)
	setOrDeleteStrings(meta, MetaReachD2, tiers[1].IDs)
	setOrDeleteStrings(meta, MetaReachD3, tiers[2].IDs)
	setOrDeleteFloats(meta, MetaReachD1Conf, tiers[0].Conf)
	setOrDeleteFloats(meta, MetaReachD2Conf, tiers[1].Conf)
	setOrDeleteFloats(meta, MetaReachD3Conf, tiers[2].Conf)
	setOrDeleteStrings(meta, MetaReachD1Label, tiers[0].Labels)
	setOrDeleteStrings(meta, MetaReachD2Label, tiers[1].Labels)
	setOrDeleteStrings(meta, MetaReachD3Label, tiers[2].Labels)
	// A node with no callers deliberately has no tier keys. These two
	// fields distinguish that valid empty record from an interrupted write.
	meta[MetaReachBuild] = build
	meta[MetaReachEpoch] = reachProcessEpoch
	meta[MetaReachTruncated] = truncated
	meta[MetaReachComplete] = true
}

// compute walks incoming edges from seed up to depth 3 and returns
// per-depth tiers carrying every ID encountered plus the
// representative in-edge's confidence + label. Each ID appears in at
// most one tier (BFS visited set is shared across depths). Edges are
// filtered with ReachableEdge so the result matches AnalyzeImpact;
// file / import nodes are walked through for fan-out but excluded
// from the tier slices.
//
// A non-nil keep narrows each level's bounded read to the edges a caller's
// reader shows; that read carries each edge's metadata so keep can inspect it.
// The edge budget is charged for the rows read before keep runs, so a hidden
// edge still bounds I/O and can only make the result a lower bound sooner.
func compute(ctx context.Context, g graph.Store, seedID string, keep EdgeFilter) ([3]tier, bool) {
	var result [3]tier
	truncated := false
	edgesRemaining := maxLookupEdges
	visited := map[string]struct{}{seedID: {}}
	current := []string{seedID}
	for depth := 1; depth <= 3 && len(current) > 0; depth++ {
		if ctx.Err() != nil || edgesRemaining <= 0 {
			truncated = true
			break
		}
		// Batch the whole BFS level's incoming-edge fetch into one
		// backend round-trip. The per-node g.GetInEdges(id) form issued
		// one query per node on disk backends — an
		// O(reachable-nodes) query storm that turned a single
		// AnalyzeImpact live walk into a multi-minute (timeout) call on
		// a disk backend. GetInEdgesByNodeIDs collapses it to one query per depth.
		inEdges, limited, err := getInEdgesBounded(ctx, g, current, edgesRemaining, keep != nil)
		if err != nil {
			truncated = true
			break
		}
		edgesRemaining -= edgeCount(inEdges)
		if keep != nil {
			inEdges = keep(inEdges)
		}

		// First pass: discover this level's new From-nodes in
		// deterministic (current-order, edge-order) order, recording the
		// representative in-edge for each.
		type cand struct {
			from string
			conf float64
			kind graph.EdgeKind
		}
		var next []string
		var cands []cand
		for _, id := range current {
			for _, e := range inEdges[id] {
				if ctx.Err() != nil {
					truncated = true
					break
				}
				if !ReachableEdge(e.Kind) {
					continue
				}
				if _, seen := visited[e.From]; seen {
					continue
				}
				visited[e.From] = struct{}{}
				next = append(next, e.From)
				cands = append(cands, cand{from: e.From, conf: e.Confidence, kind: e.Kind})
			}
		}

		// Batch the node-kind lookups too — the original called
		// g.GetNode(e.From) once per discovered node (a second per-node
		// query storm on disk backends). Only presence and kind decide a
		// tier, so the kind projection answers without decoding node
		// bodies. File / import nodes are still walked through for
		// fan-out (they stay in `next`) but excluded from the result
		// tiers, exactly as before.
		ids := make([]string, len(cands))
		for i := range cands {
			ids[i] = cands[i].from
		}
		kinds, nodeErr := graph.GetNodeKindsByIDsContext(ctx, g, ids)
		if nodeErr != nil {
			truncated = true
			break
		}
		slot := depth - 1
		for _, c := range cands {
			row, ok := kinds[c.from]
			if !ok || row.Kind == graph.KindFile || row.Kind == graph.KindImport {
				continue
			}
			result[slot].IDs = append(result[slot].IDs, c.from)
			result[slot].Conf = append(result[slot].Conf, c.conf)
			result[slot].Labels = append(result[slot].Labels,
				graph.ConfidenceLabelFor(c.kind, c.conf))
		}
		current = next
		if limited {
			truncated = true
			break
		}
	}
	for i := range result {
		sortTierByID(&result[i])
	}
	return result, truncated
}

type boundedIncomingEdgeReader interface {
	GetInEdgesByNodeIDsContext(context.Context, []string, int) (map[string][]*graph.Edge, bool, error)
}

// boundedIncomingMetaEdgeReader is the bounded read whose edges keep their
// metadata; the plain bounded read may leave it out.
type boundedIncomingMetaEdgeReader interface {
	GetInEdgesByNodeIDsWithMetaContext(context.Context, []string, int) (map[string][]*graph.Edge, bool, error)
}

// getInEdgesBounded reads at most limit incoming edges of ids. withMeta asks
// for edges that carry their metadata; the unbounded fallback always does.
func getInEdgesBounded(ctx context.Context, g graph.Store, ids []string, limit int, withMeta bool) (map[string][]*graph.Edge, bool, error) {
	if withMeta {
		if reader, ok := g.(boundedIncomingMetaEdgeReader); ok {
			return reader.GetInEdgesByNodeIDsWithMetaContext(ctx, ids, limit)
		}
	} else if reader, ok := g.(boundedIncomingEdgeReader); ok {
		return reader.GetInEdgesByNodeIDsContext(ctx, ids, limit)
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	all := g.GetInEdgesByNodeIDs(ids)
	out := make(map[string][]*graph.Edge, len(all))
	count := 0
	for _, id := range ids {
		for _, edge := range all[id] {
			if count >= limit {
				return out, true, nil
			}
			out[id] = append(out[id], edge)
			count++
		}
	}
	return out, false, nil
}

func edgeCount(byNode map[string][]*graph.Edge) int {
	total := 0
	for _, edges := range byNode {
		total += len(edges)
	}
	return total
}

// sortTierByID sorts a tier's parallel arrays in lock-step by ID so
// repeated builds produce identical snapshots and consumers can
// binary-search for membership.
func sortTierByID(t *tier) {
	n := len(t.IDs)
	if n <= 1 {
		return
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return t.IDs[idx[a]] < t.IDs[idx[b]] })
	ids := make([]string, n)
	conf := make([]float64, n)
	labels := make([]string, n)
	for newPos, oldPos := range idx {
		ids[newPos] = t.IDs[oldPos]
		conf[newPos] = t.Conf[oldPos]
		labels[newPos] = t.Labels[oldPos]
	}
	t.IDs = ids
	t.Conf = conf
	t.Labels = labels
}

// ClearIndex removes reach_d* and reach_build entries from every node
// and bumps the build counter so any cached lookups dated to a prior
// generation are invalidated. Use when the graph topology has shifted
// so far that a full rebuild is cheaper than incremental invalidation.
func ClearIndex(g graph.Store) {
	if g == nil {
		return
	}
	releaseTopology, _ := beginTopologyRead(context.Background(), nil)
	defer releaseTopology()
	mu := g.ResolveMutex()
	mu.Lock()
	defer mu.Unlock()
	retireWhole()
	cleared := make([]*graph.Node, 0, reachPublishBatchSize)
	for _, n := range g.AllNodes() {
		if n == nil || !hasReachMeta(n.Meta) {
			continue
		}
		clone := cloneNodeWithMeta(n)
		for _, k := range reachMetaKeys {
			delete(clone.Meta, k)
		}
		cleared = append(cleared, clone)
		if len(cleared) == reachPublishBatchSize {
			g.AddBatch(cleared, nil)
			cleared = cleared[:0]
		}
	}
	if len(cleared) > 0 {
		g.AddBatch(cleared, nil)
	}
}

func hasReachMeta(meta map[string]any) bool {
	for _, key := range reachMetaKeys {
		if _, ok := meta[key]; ok {
			return true
		}
	}
	return false
}

// Entry is one precomputed reach record: a node ID and the
// representative in-edge's confidence + confidence-label so the
// AnalyzeImpact fast path can hydrate an ImpactEntry with zero
// GetInEdges calls.
type Entry struct {
	ID    string
	Conf  float64
	Label string
}

// Lookup returns the per-depth reach for seedID. A current, complete eager
// record is read in sub-millisecond. On a miss it performs one bounded,
// cancellable BFS and returns that lower-bound-aware result without writing to
// the graph. Read-only lazy traversal is intentional: publishing from an
// interactive safety check can wait behind SQLite writers after the request
// deadline and starve edits. Returns hit=false only when seedID is absent or is
// not an impact-seed kind.
//
// The eager BuildIndex pass is no longer part of cold indexing because its
// monorepo breakeven is poor. It remains available for explicit prebuilding;
// normal impact calls pay only for their requested seed and never recurse over
// every symbol in the repository.
func Lookup(g graph.Store, seedID string) (d1, d2, d3 []Entry, hit bool) {
	d1, d2, d3, hit, truncated := LookupContext(context.Background(), g, seedID)
	// The legacy API has no channel for lower-bound status. Never call a
	// bounded record an exact cache hit; status-aware impact consumers use
	// LookupContext below.
	return d1, d2, d3, hit && !truncated
}

// LookupCached reads an already-published record without triggering BFS.
// Whole-repository ranking calls this form: lazily expanding reach for every
// candidate is an accidental eager index build and can issue hundreds of
// thousands of SQLite queries. Missing records fail closed to the caller's
// direct-fan-in fallback; bounded records retain their truncation signal.
func LookupCached(g graph.Store, seedID string) (d1, d2, d3 []Entry, hit, truncated bool) {
	if g == nil {
		return nil, nil, nil, false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	scope := seedScope(ctx, g, seedID)
	releaseTopology, ok := beginTopologyRead(ctx, scope)
	if !ok {
		return nil, nil, nil, false, true
	}
	unlock, ok := lockResolve(ctx, g)
	if !ok {
		releaseTopology()
		return nil, nil, nil, false, true
	}
	build := generation(scope)
	n := g.GetNode(seedID)
	if n != nil && ImpactSeedKind(n.Kind) {
		d1, d2, d3, truncated, hit = readCached(n, build)
	}
	stable := build == generation(scope)
	unlock()
	releaseTopology()
	if !stable {
		return nil, nil, nil, false, true
	}
	return d1, d2, d3, hit, truncated
}

// LookupContext returns a complete reach record or a bounded lower-bound
// record. Expensive BFS work deliberately happens outside topology and
// resolver locks. Publication re-enters both locks, verifies the generation,
// and retries once if a mutation crossed the optimistic compute window. This keeps
// synchronous edits from waiting behind breadth-first graph traversal.
type contextNodeGetter interface {
	GetNodeContext(context.Context, string) (*graph.Node, error)
}

func getNodeContext(ctx context.Context, g graph.Store, id string) (*graph.Node, error) {
	if getter, ok := g.(contextNodeGetter); ok {
		return getter.GetNodeContext(ctx, id)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return g.GetNode(id), nil
}

func LookupContext(parent context.Context, g graph.Store, seedID string) (d1, d2, d3 []Entry, hit, truncated bool) {
	return lookupContext(parent, g, seedID, nil)
}

// EdgeFilter narrows one breadth-first level's incoming edges, keyed by the
// node they enter, to the edges a reader shows.
type EdgeFilter func(map[string][]*graph.Edge) map[string][]*graph.Edge

// LookupFilteredContext is LookupContext over the edges keep retains from each
// level's bounded read, for a reader that hides part of the store's adjacency.
// It never reads a published record: those were built over every edge, hidden
// ones included. The edge budget still counts every row read. A nil keep makes
// it LookupContext.
func LookupFilteredContext(parent context.Context, g graph.Store, seedID string, keep EdgeFilter) (d1, d2, d3 []Entry, hit, truncated bool) {
	return lookupContext(parent, g, seedID, keep)
}

func lookupContext(parent context.Context, g graph.Store, seedID string, keep EdgeFilter) (d1, d2, d3 []Entry, hit, truncated bool) {
	if g == nil {
		return nil, nil, nil, false, false
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, lookupTimeout)
	defer cancel()

	scope := seedScope(ctx, g, seedID)
	for retried := false; ; retried = true {
		releaseTopology, ok := observedTopologyRead(ctx, scope)
		if !ok {
			return nil, nil, nil, false, true
		}
		unlock, ok := lockResolve(ctx, g)
		if !ok {
			releaseTopology()
			return nil, nil, nil, false, true
		}
		currentBuild := generation(scope)
		n, nodeErr := getNodeContext(ctx, g, seedID)
		if nodeErr != nil {
			unlock()
			releaseTopology()
			return nil, nil, nil, true, true
		}
		if n == nil || !ImpactSeedKind(n.Kind) {
			unlock()
			releaseTopology()
			return nil, nil, nil, false, false
		}
		// A published record describes unfiltered adjacency, so a filtered
		// lookup always computes.
		if keep == nil {
			if d1, d2, d3, cachedTruncated, cached := readCached(n, currentBuild); cached && currentBuild == generation(scope) {
				unlock()
				releaseTopology()
				return d1, d2, d3, true, cachedTruncated
			}
		}
		unlock()
		releaseTopology()

		// The expensive portion is optimistic and lock-free with respect to
		// indexing. Lazy lookups are deliberately read-only: publishing the
		// result through graph.Store.AddNode turns an interactive safety check
		// into an uncancellable SQLite write. Under concurrent indexing that
		// write can wait on the store write mutex long after ctx expires while
		// also retaining topology/resolve coordination, starving the daemon.
		//
		// BuildIndexCtx is the sole persistent reach-cache writer. A lazy miss
		// returns its bounded lower-bound evidence directly. A traversal that a
		// topology change crossed is retried once; a second crossing returns
		// what it read marked truncated, rather than chasing a repository that
		// keeps changing until the deadline.
		tiers, traversalTruncated := compute(ctx, g, seedID, keep)
		if ctx.Err() != nil {
			return entriesForTier(tiers[0]), entriesForTier(tiers[1]), entriesForTier(tiers[2]), true, true
		}
		if currentBuild != generation(scope) {
			if !retried {
				continue
			}
			traversalTruncated = true
		}
		return entriesForTier(tiers[0]), entriesForTier(tiers[1]), entriesForTier(tiers[2]), true, traversalTruncated
	}
}

// seedScope is the gate scope a lookup of seedID waits on: the seed's
// repository, or the whole graph for a seed no repository owns or that is
// missing. It only routes the lookup, so it reads the metadata-free projection
// outside any gate; the lookup reads the seed itself under the gate.
func seedScope(ctx context.Context, g graph.Store, seedID string) []string {
	rows, err := graph.GetNodeKindsByIDsContext(ctx, g, []string{seedID})
	if err != nil || rows[seedID].RepoPrefix == "" {
		return nil
	}
	return []string{rows[seedID].RepoPrefix}
}

// nodeScope is the scope whose generation a record on n follows.
func nodeScope(n *graph.Node) []string {
	if n == nil || n.RepoPrefix == "" {
		return nil
	}
	return []string{n.RepoPrefix}
}

func entriesForTier(t tier) []Entry {
	if len(t.IDs) == 0 {
		return nil
	}
	out := make([]Entry, len(t.IDs))
	for i, id := range t.IDs {
		out[i].ID = id
		if i < len(t.Conf) {
			out[i].Conf = t.Conf[i]
		}
		if i < len(t.Labels) {
			out[i].Label = t.Labels[i]
		}
	}
	return out
}

// readCached reads the stamped reach tiers off n.Meta when the stamp
// matches currentBuild and the record carries a completion marker.
// Returns ok=false when either marker is missing (never built, legacy,
// or interrupted), stale (graph has changed since), or has the wrong
// Go type.
func readCached(n *graph.Node, currentBuild uint64) (d1, d2, d3 []Entry, truncated, ok bool) {
	if n.Meta == nil {
		return nil, nil, nil, false, false
	}
	raw, present := n.Meta[MetaReachBuild]
	if !present {
		return nil, nil, nil, false, false
	}
	stamped, valid := safeUint64(raw)
	if !valid {
		return nil, nil, nil, false, false
	}
	if stamped != currentBuild {
		return nil, nil, nil, false, false
	}
	epoch, valid := n.Meta[MetaReachEpoch].(string)
	if !valid || epoch == "" || epoch != reachProcessEpoch {
		// A restart-local build number may equal a persisted record's number.
		// Never accept that record unless this process published it.
		return nil, nil, nil, false, false
	}
	complete, _ := n.Meta[MetaReachComplete].(bool)
	if !complete {
		// A generation stamp without an explicit completion marker is an
		// interrupted/legacy publication. Treat it as a miss so Lookup
		// recomputes instead of silently interpreting missing tiers as an
		// intentionally empty blast radius.
		return nil, nil, nil, false, false
	}
	if rawTruncated, present := n.Meta[MetaReachTruncated]; present {
		var valid bool
		truncated, valid = rawTruncated.(bool)
		if !valid {
			return nil, nil, nil, false, false
		}
	}
	var tierValid bool
	if d1, tierValid = readTier(n.Meta, MetaReachD1, MetaReachD1Conf, MetaReachD1Label); !tierValid {
		return nil, nil, nil, false, false
	}
	if d2, tierValid = readTier(n.Meta, MetaReachD2, MetaReachD2Conf, MetaReachD2Label); !tierValid {
		return nil, nil, nil, false, false
	}
	if d3, tierValid = readTier(n.Meta, MetaReachD3, MetaReachD3Conf, MetaReachD3Label); !tierValid {
		return nil, nil, nil, false, false
	}
	seen := make(map[string]struct{}, len(d1)+len(d2)+len(d3))
	for _, entries := range [][]Entry{d1, d2, d3} {
		for _, entry := range entries {
			if entry.ID == "" {
				return nil, nil, nil, false, false
			}
			if _, duplicate := seen[entry.ID]; duplicate {
				return nil, nil, nil, false, false
			}
			seen[entry.ID] = struct{}{}
		}
	}
	return d1, d2, d3, truncated, true
}

func safeUint64(v any) (uint64, bool) {
	switch n := v.(type) {
	case uint64:
		return n, true
	case uint:
		return uint64(n), true
	case uint32:
		return uint64(n), true
	case int:
		if n >= 0 {
			return uint64(n), true
		}
	case int64:
		if n >= 0 {
			return uint64(n), true
		}
	case float64:
		// float64(math.MaxUint64) rounds up to 2^64, so the upper bound
		// must be strict or an out-of-range legacy JSON number can wrap.
		if n >= 0 && n < math.Exp2(64) && math.Trunc(n) == n {
			return uint64(n), true
		}
	}
	return 0, false
}

// InvalidateIndex advances the generation of every repository so every
// future Lookup recomputes against the new graph state. Call this whenever
// the graph mutates in a way that could change reach sets — at the
// end of every IndexCtx / IncrementalReindexPaths / global-pass run.
//
// The cached Meta entries on nodes that survived the mutation are
// not deleted; they're simply tagged with a stale build counter, so
// the next Lookup on each falls through to a fresh compute. This is
// strictly cheaper than walking all nodes to clear Meta — the
// invalidation is O(1) and only the seeds actually queried pay the
// recompute cost.
func InvalidateIndex() {
	retireWhole()
}

// readTier reconstructs an []Entry from the parallel arrays. Missing
// confidence / label keys (or shorter slices) zero-fill so older
// snapshots that lack the parallel data degrade gracefully — the
// caller still sees the ID set, just with zero confidence.
func readTier(meta map[string]any, idsKey, confKey, labelKey string) ([]Entry, bool) {
	ids, idsPresent, valid := safeStringSlice(meta[idsKey])
	_, confPresent := meta[confKey]
	_, labelsPresent := meta[labelKey]
	if !idsPresent {
		return nil, !confPresent && !labelsPresent
	}
	if !valid {
		return nil, false
	}
	conf, _, confValid := safeFloatSlice(meta[confKey])
	labels, _, labelsValid := safeStringSlice(meta[labelKey])
	if (confPresent && (!confValid || len(conf) != len(ids))) ||
		(labelsPresent && (!labelsValid || len(labels) != len(ids))) {
		return nil, false
	}
	out := make([]Entry, len(ids))
	for i, id := range ids {
		out[i].ID = id
		if i < len(conf) {
			out[i].Conf = conf[i]
		}
		if i < len(labels) {
			out[i].Label = labels[i]
		}
	}
	return out, true
}

func safeStringSlice(v any) ([]string, bool, bool) {
	if v == nil {
		return nil, false, true
	}
	switch values := v.(type) {
	case []string:
		return values, true, true
	case []any:
		out := make([]string, len(values))
		for i, value := range values {
			text, ok := value.(string)
			if !ok {
				return nil, true, false
			}
			out[i] = text
		}
		return out, true, true
	default:
		return nil, true, false
	}
}

func safeFloatSlice(v any) ([]float64, bool, bool) {
	if v == nil {
		return nil, false, true
	}
	switch values := v.(type) {
	case []float64:
		return values, true, true
	case []any:
		out := make([]float64, len(values))
		for i, value := range values {
			switch number := value.(type) {
			case float64:
				out[i] = number
			case int:
				out[i] = float64(number)
			case int64:
				out[i] = float64(number)
			case uint64:
				out[i] = float64(number)
			default:
				return nil, true, false
			}
		}
		return out, true, true
	default:
		return nil, true, false
	}
}

// BuildCounter returns the current generation tag. Tests use it to
// assert that a rebuild actually bumped the counter.
func BuildCounter() uint64 {
	return atomic.LoadUint64(&buildCounter)
}
