package main

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

// The CLI enrich path declares BASE as its output generation.
//
// `gortex enrich <kind>` reaches the daemon over the control socket, which has
// no request view at all: it runs against the indexed corpus. Previously that
// was true only by omission — every enricher wrote `c.graph` because nothing
// selected anything else, and no receipt named the output, so nothing
// could order two enrichments of one corpus or tell an enrichment apart from an
// index mutation. Now the declaration is explicit and checkable.
//
// The test is a production-entrypoint trace: it drives realController.EnrichBlame,
// the method the control socket dispatches to, and observes the shared
// authority the MultiIndexer resolves — the same one every index mutation
// admits through.

// trackEnrichRepo commits a one-file git repository and tracks it, returning its
// prefix and root.
func trackEnrichRepo(t *testing.T, c *realController, dir, name string) (string, string) {
	t.Helper()
	root := filepath.Join(dir, name)
	wtGit(t, dir, "init", "-q", "-b", "main", "--", name)
	require.NoError(t, os.WriteFile(filepath.Join(root, "lib.go"),
		[]byte("package lib\n\nfunc L() {}\n"), 0o644))
	wtGit(t, root, "add", ".")
	wtGit(t, root, "commit", "-q", "-m", "init")

	raw, err := c.Track(context.Background(), daemon.TrackParams{Path: root})
	require.NoError(t, err)
	var tracked struct {
		Status string `json:"status"`
		Prefix string `json:"prefix"`
	}
	require.NoError(t, json.Unmarshal(raw, &tracked))
	require.Equal(t, "tracked", tracked.Status)
	return tracked.Prefix, root
}

// TestControllerEnrichDeclaresTheBaseOutputGeneration is the CLI half's
// production-entrypoint trace.
//
// A receipt admitted for the SAME producer and the SAME corpus output before
// the controller runs must be superseded by the controller's own receipt. That
// is only possible if EnrichBlame admitted one, through the shared authority,
// naming the same owner — which is what "the CLI enrich path declares base as
// its output" means operationally.
//
// Revert-red: with `blame.EnrichGraph(c.graph, t.root)` back, the controller
// admits nothing, the pre-admitted receipt settles cleanly, and the authority's
// issue count does not move.
func TestControllerEnrichDeclaresTheBaseOutputGeneration(t *testing.T) {
	ctx := context.Background()
	c, mi, _, dir := buildCatalogController(t)
	prefix, root := trackEnrichRepo(t, c, dir, "enrich-repo")

	authority := mi.ResolvedOutputGenerationAuthority()
	require.NotNil(t, authority, "the stack resolved no output-generation authority")
	before := authority.Stats()

	// The same producer, the same store, the same repository: the same output.
	// The producer name is the shared constant, not a literal — the CLI door
	// and the tool surface must spell one identity the same way or they stop
	// superseding each other for the same corpus.
	standing, err := gortexmcp.BeginBaseEnrichment(ctx, authority, c.graph,
		gortexmcp.EnrichProducerBlame, prefix, root)
	require.NoError(t, err)

	res, err := c.EnrichBlame(ctx, daemon.EnrichBlameParams{Path: prefix})
	require.NoError(t, err, "enrich blame over the control socket")
	require.GreaterOrEqual(t, res.DurationMS, int64(0))

	after := authority.Stats()
	require.Greater(t, after.Issued, before.Issued+1,
		"EnrichBlame admitted no receipt of its own; the write was not declared")

	err = standing.Complete()
	require.Error(t, err, "the standing enrichment was not superseded by the controller's")
	require.True(t, errors.Is(err, indexer.ErrOutputMutationReceiptSuperseded),
		"supersession identity is %v", err)
}

// TestControllerEnrichmentDoesNotSupersedeAnIndexMutation is the other side of
// the producer-scoped owner key: an enrichment over the control socket must not
// take the authority away from a live index mutation of the same repository,
// which would make a legitimate reindex report itself superseded.
func TestControllerEnrichmentDoesNotSupersedeAnIndexMutation(t *testing.T) {
	ctx := context.Background()
	c, mi, _, dir := buildCatalogController(t)
	prefix, root := trackEnrichRepo(t, c, dir, "enrich-vs-index")

	authority := mi.ResolvedOutputGenerationAuthority()
	index, err := authority.Begin(ctx, indexer.OutputEntryIndexFile, indexer.OutputMutationTarget{
		Kind: indexer.OutputGenerationLegacy, OwnerKey: "root:" + root, RepoPrefix: prefix,
	})
	require.NoError(t, err)

	_, err = c.EnrichBlame(ctx, daemon.EnrichBlameParams{Path: prefix})
	require.NoError(t, err)

	require.NoError(t, index.Complete(),
		"a control-socket enrichment took the authority away from a live index mutation")
}

// TestExactStatusDeclaresItsCounterWrite is the production-entrypoint trace for
// the one write on this door that is not an enrichment: `gortex status --exact`
// recounts the per-repo estimates and writes them back over the persisted
// counters. That is a mutation of the corpus, and it names generation zero
// through the same authority every other write does.
//
// Revert-red: drop beginBaseEnrichment from StatusExact and the standing
// receipt settles cleanly, because nothing else in the process names that owner.
func TestExactStatusDeclaresItsCounterWrite(t *testing.T) {
	ctx := context.Background()
	c, mi, _, dir := buildCatalogController(t)
	trackEnrichRepo(t, c, dir, "counter-repo")
	c.enriched.Store(true)

	authority := mi.ResolvedOutputGenerationAuthority()
	require.NotNil(t, authority, "the stack resolved no output-generation authority")

	standing, err := gortexmcp.BeginBaseEnrichment(ctx, authority, c.graph,
		gortexmcp.EnrichProducerRepoCounters, "", "")
	require.NoError(t, err)

	_, err = c.StatusExact(ctx)
	require.NoError(t, err, "exact status")

	err = standing.Complete()
	require.Error(t, err, "the counter write named no output generation")
	require.True(t, errors.Is(err, indexer.ErrOutputMutationReceiptSuperseded),
		"supersession identity is %v", err)
}

// TestExactStatusSurvivesASupersedingRecount is the non-regression half.
//
// StatusExact deliberately does NOT hold c.mu, so two concurrent
// `gortex status --exact` calls supersede each other by construction. The loser
// has already written its counters — the same measured numbers the winner
// wrote — by the time Complete reports the supersession, so turning that into
// an error would invent a failure mode the pre-item code could not produce.
// Naming the output is how the two runs are ordered, not a licence to fail one.
//
// Revert-red: drop the ErrOutputMutationReceiptSuperseded arm from StatusExact
// and this returns "reconcile repo counters: … superseded".
func TestExactStatusSurvivesASupersedingRecount(t *testing.T) {
	ctx := context.Background()
	c, mi, _, dir := buildCatalogController(t)
	trackEnrichRepo(t, c, dir, "counter-race-repo")
	c.enriched.Store(true)

	authority := mi.ResolvedOutputGenerationAuthority()
	require.NotNil(t, authority)

	rival := &supersedingCounterStore{Store: c.graph, authority: authority}
	c.graph = rival

	_, err := c.StatusExact(ctx)
	require.NoError(t, err,
		"a superseded counter reconciliation failed the exact status whose counters it had already written")
	require.Equal(t, int32(1), rival.writes.Load(), "the counters were never written")
}

// supersedingCounterStore admits a competing recount for the same owner from
// inside the counter write itself — the deterministic stand-in for a second
// `gortex status --exact` arriving mid-write. It delegates everything else, so
// StatusExact runs unmodified.
type supersedingCounterStore struct {
	graph.Store
	authority *indexer.OutputGenerationAuthority
	writes    atomic.Int32
}

func (s *supersedingCounterStore) ScanRepoMemoryEstimates(
	ctx context.Context,
) (map[string]graph.RepoMemoryEstimate, error) {
	return s.Store.(graph.RepoMemoryEstimateScanner).ScanRepoMemoryEstimates(ctx)
}

func (s *supersedingCounterStore) ReconcileRepoCounters(scanned map[string]graph.RepoMemoryEstimate) error {
	s.writes.Add(1)
	// Same store handle, same producer, same (empty) scope: the same owner key.
	if rival, err := gortexmcp.BeginBaseEnrichment(context.Background(), s.authority, s,
		gortexmcp.EnrichProducerRepoCounters, "", ""); err == nil {
		rival.Abandon()
	}
	return s.Store.(interface {
		ReconcileRepoCounters(map[string]graph.RepoMemoryEstimate) error
	}).ReconcileRepoCounters(scanned)
}

// TestControllerEnrichReportsASupersededRunAsLanded is the CLI door's half of
// the settlement contract, and it closes the failure mode this door invented.
//
// Both doors — `gortex enrich blame` here and `analyze kind=blame` on the tool
// surface — admit through ONE authority with ONE owner key per (producer,
// corpus), deliberately: that is why the producer names are shared exported
// constants. So an agent's blame run concurrent with a git hook's
// `gortex enrich blame` supersedes one of the two every time. The producer
// stamps as it goes and has written everything it is going to write before it
// settles, so that is an ordering statement — the same one the tool surface
// reports as `superseded: true` beside its counts.
//
// This door used to turn it into `enrich <prefix>: … superseded` AND discard
// `combined` for every repository it had already enriched. Pre-item the error
// could not occur at all, so it was a failure mode introduced by naming the
// output.
//
// Revert-red: with `if err := out.Complete(); err != nil { return
// daemon.EnrichBlameResult{}, … }` back, EnrichBlame returns an error and both
// assertions below fail.
func TestControllerEnrichReportsASupersededRunAsLanded(t *testing.T) {
	ctx := context.Background()
	c, mi, _, dir := buildCatalogController(t)
	prefix, root := trackEnrichRepo(t, c, dir, "superseded-blame")

	authority := mi.ResolvedOutputGenerationAuthority()
	require.NotNil(t, authority)
	rival := &supersedingEnrichStore{
		Store:     c.graph,
		authority: authority,
		producer:  gortexmcp.EnrichProducerBlame,
		targets:   []enrichTarget{{prefix: prefix, root: root}},
	}
	c.graph = rival

	res, err := c.EnrichBlame(ctx, daemon.EnrichBlameParams{Path: prefix})
	require.NoError(t, err,
		"a superseded enrichment was reported as a failed one; its stamps are already on the graph")
	require.True(t, rival.fired.Load(), "the rival never ran; the producer did not read the store")
	require.True(t, res.Superseded,
		"the result does not say a newer run took the output over")
}

// TestControllerEnrichKeepsTheRepositoriesItAlreadyEnriched is the second half
// of the same defect: the hard return sat INSIDE the per-repository loop, so a
// supersession on the first repository threw away the counts of every
// repository the run had already finished — and skipped the ones after it.
//
// Revert-red: with the `return daemon.EnrichBlameResult{}, …` back, this call
// returns an error and zero repositories are reported.
func TestControllerEnrichKeepsTheRepositoriesItAlreadyEnriched(t *testing.T) {
	ctx := context.Background()
	c, mi, _, dir := buildCatalogController(t)
	firstPrefix, firstRoot := trackEnrichRepo(t, c, dir, "superseded-first")
	secondPrefix, secondRoot := trackEnrichRepo(t, c, dir, "superseded-second")

	authority := mi.ResolvedOutputGenerationAuthority()
	// The rival admits for BOTH repositories on its first fire, so whichever
	// one the loop happens to enrich first is the one it supersedes — the test
	// is about the loop surviving a supersession, not about map order.
	rival := &supersedingEnrichStore{
		Store:     c.graph,
		authority: authority,
		producer:  gortexmcp.EnrichProducerBlame,
		targets: []enrichTarget{
			{prefix: firstPrefix, root: firstRoot},
			{prefix: secondPrefix, root: secondRoot},
		},
	}
	c.graph = rival
	before := authority.Stats()

	// Path empty: every tracked repository participates.
	res, err := c.EnrichBlame(ctx, daemon.EnrichBlameParams{})
	require.NoError(t, err)
	require.True(t, res.Superseded)
	// Two repositories were admitted, plus the rival's own receipt: the loop
	// did not abort on the first supersession.
	require.GreaterOrEqual(t, authority.Stats().Issued, before.Issued+3,
		"the enrichment loop stopped at the superseded repository")
}

// supersedingEnrichStore admits a competing enrichment for the SAME owner from
// inside the producer's first store read — the deterministic stand-in for a
// second blame run (from either door) arriving while this one walks the graph.
// Everything else delegates, so the controller method runs unmodified.
type supersedingEnrichStore struct {
	graph.Store
	authority *indexer.OutputGenerationAuthority
	producer  string
	targets   []enrichTarget
	fired     atomic.Bool
}

func (s *supersedingEnrichStore) AllNodes() []*graph.Node {
	s.admitRival()
	return s.Store.AllNodes()
}

// NodesByKind is the co-change enricher's read: cochange.AddEdges asks the
// store for file nodes directly instead of walking AllNodes, so the rival has
// to sit on this door too or the co-change case would silently never fire.
func (s *supersedingEnrichStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	s.admitRival()
	return s.Store.NodesByKind(kind)
}

// admitRival admits the competing enrichment once, on the producer's first
// read of the admitted store.
func (s *supersedingEnrichStore) admitRival() {
	if !s.fired.CompareAndSwap(false, true) {
		return
	}
	for _, t := range s.targets {
		// Same store handle, same producer, same prefix: the same owner key
		// the controller's own admission names.
		if rival, err := gortexmcp.BeginBaseEnrichment(context.Background(), s.authority, s,
			s.producer, t.prefix, t.root); err == nil {
			rival.Abandon()
		}
	}
}

// TestConcurrentExactStatusCallsBothAnswer is the concurrency pin behind
// StatusExact's non-fatal arm.
//
// StatusExact deliberately does not hold c.mu — a full recount would block
// every other control call for the length of the scan — so two `gortex status
// --exact` calls overlap by construction and supersede each other's counter
// write. Both have written the same measured numbers by then, so both must
// answer.
//
// Revert-red: with the supersession arm dropped, one of the two returns
// "reconcile repo counters: … superseded".
func TestConcurrentExactStatusCallsBothAnswer(t *testing.T) {
	ctx := context.Background()
	c, _, _, dir := buildCatalogController(t)
	trackEnrichRepo(t, c, dir, "concurrent-exact")
	c.enriched.Store(true)

	const callers = 4
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.StatusExact(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "concurrent exact status %d failed", i)
	}
}

// TestViewsStatusStatesWhyACheckoutHasNoBuildLoop pins the census projection.
//
// The view lifecycle records why a checkout's build loop could not be started
// (every such path is a background reconciliation with no caller to fail), the
// census carries the ledger, and this projection is what puts it in front of a
// person running `gortex daemon status`. It used to drop the field on the
// floor, which left the reason with no reader anywhere.
//
// Revert-red: delete `CoordinatorStartFailures:` from the payload literal and
// the first assertion fails.
func TestViewsStatusStatesWhyACheckoutHasNoBuildLoop(t *testing.T) {
	health := indexer.ViewsHealth{
		Families:     1,
		Checkouts:    map[string]int{"ready": 3},
		Coordinators: 1,
		Generations:  map[string]int{"ready": 2},
		Leases:       1,
		RefViews:     map[string]int{"ready": 1},
		Counters:     map[string]int64{"views_coordinators": 1},
		CoordinatorStartFailures: []indexer.CoordinatorStartFailure{{
			CheckoutID: "chk-1",
			RootPath:   "/tmp/worktree-a",
			Reason:     "freeze the index configuration: encode dedicated base config",
			At:         1757000000,
		}},
	}

	status := viewsStatusFromHealth(health)
	require.Len(t, status.CoordinatorStartFailures, 1,
		"the status payload counts build loops and says nothing about the checkout that has none")
	got := status.CoordinatorStartFailures[0]
	require.Equal(t, "chk-1", got.CheckoutID)
	require.Equal(t, "/tmp/worktree-a", got.RootPath,
		"the stated reason does not name the working copy that has no view")
	require.Contains(t, got.Reason, "freeze the index configuration")
	require.Equal(t, int64(1757000000), got.At)

	// The rest of the census still arrives: a projection is only trustworthy
	// if the field that was added did not displace one that was there.
	require.Equal(t, 1, status.Families)
	require.Equal(t, 3, status.Checkouts["ready"])
	require.Equal(t, 1, status.Coordinators)
	require.Equal(t, 2, status.Generations["ready"])
	require.Equal(t, 1, status.Leases)
	require.Equal(t, 1, status.RefViews["ready"])
	require.Equal(t, int64(1), status.Counters["views_coordinators"])
}

// TestAHealthyViewCensusCarriesNoFailureList is the cardinality half: the
// ordinary answer is the absent one. A payload that rendered an empty list
// would read as a section someone forgot to fill in, and would put a key on
// every status poll for a state that has no instances.
func TestAHealthyViewCensusCarriesNoFailureList(t *testing.T) {
	status := viewsStatusFromHealth(indexer.ViewsHealth{Families: 1, Coordinators: 1})
	require.Nil(t, status.CoordinatorStartFailures)
	require.Nil(t, status.StorageFailures)

	body, err := json.Marshal(status)
	require.NoError(t, err)
	require.NotContains(t, string(body), "coordinator_start_failures",
		"a healthy census renders the failure key")
	require.NotContains(t, string(body), "storage_failures",
		"a healthy census renders the storage-failure key")
}

// TestViewsStatusStatesWhyAGenerationIsStillThere is the start-failure defect
// one field down, and the census names it in its own doc comment: the
// lifecycle collects the storage layer's maintenance refusals and this
// projection dropped them, so a retirement blocked by a full volume reached no
// reader at all.
//
// Generations says how much derived payload the store holds and in what state.
// A census reading "four generations, one retiring" states a problem it cannot
// explain, and retirement is a background pass with no caller to return the
// error to — so the status payload is the only surface the reason can come out
// of.
//
// Revert-red: delete `StorageFailures:` from the payload literal in
// viewsStatusFromHealth and the first assertion fails.
func TestViewsStatusStatesWhyAGenerationIsStillThere(t *testing.T) {
	health := indexer.ViewsHealth{
		Families:     1,
		Coordinators: 1,
		Generations:  map[string]int{"retiring": 2},
		StorageFailures: []store_sqlite.StorageFailure{
			{GenerationID: 41, Reason: "the store volume is full"},
			{GenerationID: 42, Reason: "the store volume is full"},
		},
	}

	status := viewsStatusFromHealth(health)
	require.Len(t, status.StorageFailures, 2,
		"the status payload counts retiring generations and says nothing about why they are still there")
	require.Equal(t, int64(41), status.StorageFailures[0].GenerationID)
	require.Equal(t, "the store volume is full", status.StorageFailures[0].Reason)
	require.Equal(t, int64(42), status.StorageFailures[1].GenerationID)

	// The rest of the census still arrives: an added field must not displace
	// one that was already there.
	require.Equal(t, 1, status.Families)
	require.Equal(t, 2, status.Generations["retiring"])
}

// TestEveryViewsCensusFieldReachesTheStatusPayload is the structural guard
// behind both reason lists, and the one that turns "a field the census carries
// and the payload silently drops" from a property of a reviewer's attention
// into a compile-time-adjacent check.
//
// Both defects this guard closes had the same shape: indexer.ViewsHealth grew a
// field, the projection literal was not extended, and nothing failed — the
// payload simply carried less than the census did, with no error anywhere. The
// two types are matched on their JSON tags because that is the contract a
// client reads, and the daemon side is allowed to carry MORE than the census
// (it is the protocol), never less.
func TestEveryViewsCensusFieldReachesTheStatusPayload(t *testing.T) {
	payload := map[string]bool{}
	statusType := reflect.TypeOf(daemon.ViewsStatus{})
	for i := range statusType.NumField() {
		payload[jsonTagName(statusType.Field(i))] = true
	}

	censusType := reflect.TypeOf(indexer.ViewsHealth{})
	require.Positive(t, censusType.NumField())
	for i := range censusType.NumField() {
		field := censusType.Field(i)
		name := jsonTagName(field)
		require.True(t, payload[name],
			"indexer.ViewsHealth.%s (json %q) has no field in daemon.ViewsStatus, so the "+
				"census carries it and `gortex daemon status` drops it", field.Name, name)
	}
}

// jsonTagName is the wire name of one struct field: its json tag up to the
// first option, falling back to the Go field name for an untagged field.
func jsonTagName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "" {
		return field.Name
	}
	if comma := strings.IndexByte(tag, ','); comma >= 0 {
		tag = tag[:comma]
	}
	if tag == "" {
		return field.Name
	}
	return tag
}

// TestViewsCensusReachesTheStatusPayload is the production trace for the two
// tests above: the projection is not a helper nobody calls — Status assembles
// the views block through it.
func TestViewsCensusReachesTheStatusPayload(t *testing.T) {
	ctx := context.Background()
	c, _, _, dir := buildCatalogController(t)
	trackEnrichRepo(t, c, dir, "census-repo")

	st, err := c.Status(ctx)
	require.NoError(t, err)
	require.NotNil(t, st.Views, "the status payload carries no views census")
	require.Nil(t, st.Views.CoordinatorStartFailures,
		"this daemon's checkouts all have build loops")
	require.Positive(t, st.Views.Families)
}

// TestAStartFailureReachesTheStatusPayload is the production trace with a
// REASON in it, and the one the projection's correctness actually depends on.
//
// The two tests above check the projection in isolation; this one drives a real
// coordinator start failure into a real lifecycle and then reads
// `c.Status(ctx)` — the method the control socket dispatches `gortex daemon
// status` to. Nothing in between is stubbed, so a Status payload assembled from
// an inline literal that drops the ledger fails here even though the projection
// helper still compiles and its own unit tests still pass.
//
// The failure is a real one, not an injected one: the probe fixture seeds its
// family, checkout and dedicated graph straight into the catalog and never
// registers the graph as a repository owner, so buildCoordinator's very first
// step — AcquireRepositoryRead(primaryGraphID) — refuses with
// graphview.ErrRepositoryOwnerUnknown. That is exactly the shape the ledger
// exists for: a background path with no caller to return the error to.
//
// Revert-red: replace `viewsStatusFromHealth(health)` in collectViewsStatus
// with a payload literal that omits CoordinatorStartFailures and this fails,
// because the reason reaches no reader again.
func TestAStartFailureReachesTheStatusPayload(t *testing.T) {
	ctx := context.Background()
	f := newProbeFixture(t)

	before, err := f.controller.Status(ctx)
	require.NoError(t, err)
	require.NotNil(t, before.Views, "the status payload carries no views census")
	require.Empty(t, before.Views.CoordinatorStartFailures,
		"the fixture already has a checkout with no build loop")

	// The selection path's on-demand activation: the same call a probe makes
	// for a ready, automatic, unrouted working copy. It is fire-and-forget, so
	// the failure lands on the lifecycle's own goroutine.
	require.True(t, f.controller.lifecycle.ActivateCheckout(probeWorktreeID, "test"),
		"the lifecycle refused to activate the fixture's automatic checkout")

	var failures []daemon.CoordinatorStartFailure
	require.Eventually(t, func() bool {
		st, err := f.controller.Status(ctx)
		if err != nil || st.Views == nil {
			return false
		}
		failures = st.Views.CoordinatorStartFailures
		return len(failures) > 0
	}, 10*time.Second, 10*time.Millisecond,
		"the checkout that could not start a build loop is explained nowhere in `daemon status`")

	require.Len(t, failures, 1)
	require.Equal(t, probeWorktreeID, failures[0].CheckoutID)
	require.Equal(t, f.worktreeRoot, failures[0].RootPath,
		"the stated reason does not name the working copy that has no view")
	require.NotEmpty(t, failures[0].Reason,
		"the payload names a checkout with no build loop and does not say why")
	require.Positive(t, failures[0].At)
}

// trackCoupledEnrichRepo commits a two-file repository twice (both files in
// both commits, so the co-change mine finds a pair), tags it (so the release
// enricher has a tag on the default branch), and tracks it. Every enricher on
// this door then reaches the graph rather than returning early on an empty
// git answer — which is what makes "did the rival fire?" a real question.
func trackCoupledEnrichRepo(t *testing.T, c *realController, dir, name string) (string, string) {
	t.Helper()
	root := filepath.Join(dir, name)
	wtGit(t, dir, "init", "-q", "-b", "main", "--", name)
	write := func(body string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"),
			[]byte("package lib\n\nfunc A() {"+body+"}\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(root, "b.go"),
			[]byte("package lib\n\nfunc B() {"+body+"}\n"), 0o644))
	}
	write("")
	wtGit(t, root, "add", ".")
	wtGit(t, root, "commit", "-q", "-m", "init")
	wtGit(t, root, "tag", "v1.0.0")
	write("\n\t_ = 1\n")
	wtGit(t, root, "add", ".")
	wtGit(t, root, "commit", "-q", "-m", "second")

	raw, err := c.Track(context.Background(), daemon.TrackParams{Path: root})
	require.NoError(t, err)
	var tracked struct {
		Status string `json:"status"`
		Prefix string `json:"prefix"`
	}
	require.NoError(t, json.Unmarshal(raw, &tracked))
	require.Equal(t, "tracked", tracked.Status)
	return tracked.Prefix, root
}

// TestEveryControllerEnrichDoorReportsASupersededRunAsLanded is the
// door-by-door pin behind the one settlement rule the enrich doors share.
//
// The five doors are textually identical and each one was independently
// capable of the defect: `if err := out.Complete(); err != nil { return
// <Result>{}, … }` turned an ORDERING statement into a failed enrichment and
// discarded the counts of every repository the run had already stamped.
// Pinning one door leaves the other four free to regress to that shape one
// edit at a time, so each is driven here with a rival admitting for its own
// producer from inside its own store read.
//
// Revert-red: restore the hard-error `out.Complete()` shape in any one of the
// five doors and that subtest fails — the door returns
// "enrich <prefix>: … superseded" instead of the landed counts.
func TestEveryControllerEnrichDoorReportsASupersededRunAsLanded(t *testing.T) {
	cases := []struct {
		name     string
		producer string
		run      func(context.Context, *realController, string) (bool, error)
	}{
		{"blame", gortexmcp.EnrichProducerBlame,
			func(ctx context.Context, c *realController, prefix string) (bool, error) {
				res, err := c.EnrichBlame(ctx, daemon.EnrichBlameParams{Path: prefix})
				return res.Superseded, err
			}},
		{"churn", gortexmcp.EnrichProducerChurn,
			func(ctx context.Context, c *realController, prefix string) (bool, error) {
				res, err := c.EnrichChurn(ctx, daemon.EnrichChurnParams{Path: prefix})
				return res.Superseded, err
			}},
		{"releases", gortexmcp.EnrichProducerReleases,
			func(ctx context.Context, c *realController, prefix string) (bool, error) {
				res, err := c.EnrichReleases(ctx, daemon.EnrichReleasesParams{Path: prefix})
				return res.Superseded, err
			}},
		{"coverage", gortexmcp.EnrichProducerCoverage,
			func(ctx context.Context, c *realController, prefix string) (bool, error) {
				res, err := c.EnrichCoverage(ctx, daemon.EnrichCoverageParams{
					Path: prefix,
					Segments: []daemon.EnrichCoverageSegment{
						{File: "a.go", StartLine: 3, EndLine: 3, NumStmt: 1, Count: 1},
					},
				})
				return res.Superseded, err
			}},
		{"cochange", gortexmcp.EnrichProducerCochange,
			func(ctx context.Context, c *realController, prefix string) (bool, error) {
				res, err := c.EnrichCochange(ctx, daemon.EnrichCochangeParams{Path: prefix})
				return res.Superseded, err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c, mi, _, dir := buildCatalogController(t)
			prefix, root := trackCoupledEnrichRepo(t, c, dir, "superseded-"+tc.name)

			authority := mi.ResolvedOutputGenerationAuthority()
			require.NotNil(t, authority)
			rival := &supersedingEnrichStore{
				Store:     c.graph,
				authority: authority,
				producer:  tc.producer,
				targets:   []enrichTarget{{prefix: prefix, root: root}},
			}
			c.graph = rival

			superseded, err := tc.run(ctx, c, prefix)
			require.NoError(t, err,
				"a superseded %s enrichment was reported as a failed one; its stamps are already on the graph",
				tc.name)
			require.True(t, rival.fired.Load(),
				"the rival never ran; the %s producer read no node from the admitted store", tc.name)
			require.True(t, superseded,
				"the %s result does not say a newer run took the output over", tc.name)
		})
	}
}
