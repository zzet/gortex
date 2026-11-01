package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// contractJobFixture is one real primary repository with the installed
// journal, a certified baseline and the production capture: a job's reads are
// proven against real receipt stamps, and saves go through the real door.
type contractJobFixture struct {
	t         *testing.T
	store     *store_sqlite.Store
	root      string
	mi        *MultiIndexer
	options   ContractFollowupCaptureOptions
	epoch     atomic.Uint64
	mu        sync.Mutex
	published chan struct{}
	layers    int
	// adjust, when set, edits each job's captured snapshot.
	adjust func(*ContractFollowupSnapshot)
	// onWait, when set, runs as a job starts waiting for a Published event.
	onWait func()
	// logger, when set, receives each job's lines.
	logger *zap.Logger
}

func newContractJobFixture(t *testing.T, files map[string]string) *contractJobFixture {
	t.Helper()
	return newContractJobFixtureWithRegistry(t, files, newTestRegistry())
}

func newContractJobFixtureWithRegistry(t *testing.T, files map[string]string, registry *parser.Registry) *contractJobFixture {
	t.Helper()
	ctx := t.Context()
	_, store := newSQLiteIndexer(t)
	f := &contractJobFixture{t: t, store: store, root: t.TempDir(), published: make(chan struct{})}
	for name, text := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(f.root, name)), 0o755))
		writeFile(t, filepath.Join(f.root, name), text)
	}
	cfg := &config.GlobalConfig{Repos: []config.RepoEntry{{Name: "fixture", Path: f.root}}}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg.SetConfigPath(configPath)
	require.NoError(t, cfg.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	f.mi = NewMultiIndexer(store, registry, search.NewNull(), manager, zap.NewNop())
	t.Cleanup(func() { _ = f.mi.Close(context.Background()) })
	f.mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: func(context.Context, string, string) { f.announce() }})
	results, err := f.mi.IndexAll()
	require.NoError(t, err)
	require.Empty(t, results["fixture"].FailedFiles)
	f.options = ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: graphview.NewLeaseManager()}, MultiIndexer: f.mi, Registry: f.mi.registry, Config: config.Default().Index, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }}
	require.NoError(t, reconcilePrimaryContractBaseline(ctx, f.options, "fixture"))
	return f
}

func (f *contractJobFixture) announce() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.epoch.Add(1)
	close(f.published)
	f.published = make(chan struct{})
}

func (f *contractJobFixture) waitPublished(ctx context.Context, since uint64, until time.Time) bool {
	if f.onWait != nil {
		f.onWait()
	}
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	for {
		f.mu.Lock()
		changed := f.published
		f.mu.Unlock()
		if f.epoch.Load() > since {
			return true
		}
		select {
		case <-changed:
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// save writes a file and runs the real incremental door: journal begin, core
// write, accept, Published.
func (f *contractJobFixture) save(name, text string) {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	require.NoError(f.t, os.MkdirAll(filepath.Dir(path), 0o755))
	writeFile(f.t, path, text)
	future := time.Now().Add(time.Duration(f.epoch.Load()+1) * time.Minute)
	require.NoError(f.t, os.Chtimes(path, future, future))
	_, err := f.mi.IncrementalReindexRepo("fixture", []string{path})
	require.NoError(f.t, err)
}

// saveKeepingKey saves a file through the real door and then restores the
// namespace's input-state row to what it was: the receipt, the core rows and
// the write seq move, the attachment key does not. It is the shape of an
// edit whose receipt carries no key move to the readers it affects (the
// unmoved-key class), placed exactly where a test needs it.
func (f *contractJobFixture) saveKeepingKey(name, text string) {
	f.t.Helper()
	ctx := context.Background()
	before, found, err := f.store.ContractInputStateContext(ctx, "fixture", "")
	require.NoError(f.t, err)
	require.True(f.t, found)
	f.save(name, text)
	f.restoreKey(before)
}

func (f *contractJobFixture) restoreKey(before graph.ContractInputState) {
	f.t.Helper()
	ctx := context.Background()
	after, _, err := f.store.ContractInputStateContext(ctx, "fixture", "")
	require.NoError(f.t, err)
	if after == before {
		return
	}
	restored := before
	restored.Accepted = false
	require.NoError(f.t, f.store.BeginContractInputMutationContext(ctx, &after, restored, nil))
	require.NoError(f.t, f.store.AcceptContractInputMutationContext(ctx, restored))
}

// saveHeld saves a file through the real door with its accept held: the
// mutation completes every core write and ends the way an overtaken one
// does, settled, never accepted, with no Published event.
func (f *contractJobFixture) saveHeld(name, text string) {
	f.t.Helper()
	hold := func(repo string) bool { return repo == "fixture" }
	contractCoreAcceptHold.Store(&hold)
	defer contractCoreAcceptHold.Store(nil)
	f.save(name, text)
}

// saveInFlight is saveHeld followed by a rewrite of the file's receipt,
// unaccepted with the same identity, whose body then ends its core writes:
// the core rows are the save's, complete, the core-write window is closed,
// and the receipt stays in flight, neither accepted nor settled, until the
// next write of the path, the state of a save overtaken before it settles.
func (f *contractJobFixture) saveInFlight(name, text string) {
	f.t.Helper()
	f.saveHeld(name, text)
	path := "fixture/" + name
	f.stageInFlight(path)
	f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{path}))
}

// stageInFlight rewrites path's receipt unaccepted with the same identity and
// leaves its core rows as they are: an apply that staged its receipt and has
// not ended its core writes, its core-write window open.
func (f *contractJobFixture) stageInFlight(path string) {
	f.t.Helper()
	ctx := context.Background()
	row, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", path)
	require.NoError(f.t, err)
	require.NotNil(f.t, row)
	staged := *row
	staged.Accepted = false
	staged.Previous = nil
	require.NoError(f.t, f.store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{staged}))
}

// saveHeldKeepingKey is saveHeld with the namespace's input-state row
// restored, as saveKeepingKey does.
func (f *contractJobFixture) saveHeldKeepingKey(name, text string) {
	f.t.Helper()
	before, found, err := f.store.ContractInputStateContext(context.Background(), "fixture", "")
	require.NoError(f.t, err)
	require.True(f.t, found)
	f.saveHeld(name, text)
	f.restoreKey(before)
}

func (f *contractJobFixture) removeKeepingKey(name string) {
	f.t.Helper()
	before, found, err := f.store.ContractInputStateContext(context.Background(), "fixture", "")
	require.NoError(f.t, err)
	require.True(f.t, found)
	f.remove(name)
	f.restoreKey(before)
}

func (f *contractJobFixture) remove(name string) {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	require.NoError(f.t, os.Remove(path))
	_, err := f.mi.IncrementalReindexRepo("fixture", []string{path})
	require.NoError(f.t, err)
}

// beginHeld rewrites path's receipt unaccepted with the same identity and
// removes its core rows: the state between an apply's begin and its add.
// The returned func re-adds the rows and accepts the receipt.
func (f *contractJobFixture) beginHeld(path string) func() {
	f.t.Helper()
	ctx := context.Background()
	staged, nodes, edges, err := f.evictStaged(path)
	require.NoError(f.t, err)
	return func() {
		f.t.Helper()
		require.NoError(f.t, f.store.AddBatchChecked(nodes, edges))
		require.NoError(f.t, f.store.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{staged}))
		f.announce()
	}
}

// evictStaged rewrites path's receipt unaccepted with the same identity,
// opening its core-write window, and removes its core rows, returning the
// staged row and the rows to re-add. It is safe off the test goroutine.
func (f *contractJobFixture) evictStaged(path string) (graph.ContractBoundaryReceipt, []*graph.Node, []*graph.Edge, error) {
	ctx := context.Background()
	row, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", path)
	if err != nil {
		return graph.ContractBoundaryReceipt{}, nil, nil, err
	}
	if row == nil {
		return graph.ContractBoundaryReceipt{}, nil, nil, fmt.Errorf("no receipt for %s", path)
	}
	staged := *row
	staged.Accepted = false
	staged.Previous = nil
	if err := f.store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{staged}); err != nil {
		return graph.ContractBoundaryReceipt{}, nil, nil, err
	}
	nodes := f.store.GetFileNodes(path)
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	var edges []*graph.Edge
	for _, list := range f.store.GetOutEdgesByNodeIDs(ids) {
		edges = append(edges, list...)
	}
	// Eviction also tears other files' edges into these nodes; the add
	// re-states them, as the incremental apply does.
	type edgeKey struct {
		from, to, file string
		kind           graph.EdgeKind
		line           int
	}
	seen := make(map[edgeKey]bool, len(edges))
	for _, edge := range edges {
		seen[edgeKey{edge.From, edge.To, edge.FilePath, edge.Kind, edge.Line}] = true
	}
	for _, list := range f.store.GetInEdgesByNodeIDs(ids) {
		for _, edge := range list {
			if key := (edgeKey{edge.From, edge.To, edge.FilePath, edge.Kind, edge.Line}); !seen[key] {
				seen[key] = true
				edges = append(edges, edge)
			}
		}
	}
	f.store.EvictFiles([]string{path})
	return staged, nodes, edges, nil
}

// job captures the current inputs and runs one attachment job.
func (f *contractJobFixture) job(hooks *contractConvergenceHooks, yield func(context.Context) error) (ContractFollowupReport, error) {
	f.t.Helper()
	f.layers++
	return runFixtureContractJob(f.t, f.options, contractFixtureJob{hooks: hooks, yield: yield, layer: f.layers, adjust: f.adjust,
		epoch: f.epoch.Load, wait: f.waitPublished, logger: f.logger})
}

// contractFixtureJob is one job's knobs: test hooks, the yield, the payload
// layer's creation order, a snapshot adjustment, the Published source and the
// job's logger.
type contractFixtureJob struct {
	otherActorWaiting func() bool
	scratchParent     string
	cache             **contractDeltaCache
	hooks             *contractConvergenceHooks
	yield             func(context.Context) error
	layer             int
	adjust            func(*ContractFollowupSnapshot)
	epoch             func() uint64
	wait              func(context.Context, uint64, time.Time) bool
	logger            *zap.Logger
}

// runFixtureContractJob captures the primary fixture repository's current
// inputs through the production capture and runs one attachment job.
func runFixtureContractJob(t *testing.T, options ContractFollowupCaptureOptions, job contractFixtureJob) (ContractFollowupReport, error) {
	t.Helper()
	ctx := t.Context()
	inputs, err := options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.True(t, inputs.State.Accepted)
	snapshot, targets, err := NewContractFollowupCapture(options)(ctx, nil, inputs, "fixture", "")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	snapshot.Work = targets[0].Work
	if job.adjust != nil {
		job.adjust(&snapshot)
	}
	_, payload, err := options.Store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "contract-job", LayerID: "job-" + time.Now().Format(time.RFC3339Nano), GenerationKind: "contract_analysis", ConfigHash: "job-config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "contract-only", CreatedAt: int64(job.layer)})
	require.NoError(t, err)
	yield := job.yield
	if yield == nil {
		yield = options.Yield
	}
	logger := job.logger
	if logger == nil {
		logger = zap.NewNop()
	}
	req := ContractFollowupRequest{RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: options.Registry, Config: options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: options.Materializer.Leases, Logger: logger, Yield: yield,
		PublishedEpoch: job.epoch, WaitPublished: job.wait, OtherActorWaiting: job.otherActorWaiting, ScratchParent: job.scratchParent, convergenceHooks: job.hooks}
	if job.cache != nil {
		session, sessionErr := newContractDeltaSession(*job.cache, snapshot, options.Config)
		require.NoError(t, sessionErr)
		req.delta = session
		defer func() { *job.cache = session.cache }()
	}
	return RunContractFollowup(ctx, req)
}

// contracts reads the published attachment of the current inputs.
func (f *contractJobFixture) contracts() map[string]*graph.Node {
	f.t.Helper()
	return publishedFixtureContracts(f.t, f.options)
}

func publishedFixtureContracts(t *testing.T, options ContractFollowupCaptureOptions) map[string]*graph.Node {
	t.Helper()
	ctx := t.Context()
	inputs, err := options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	analysis, err := options.Materializer.OpenContractAnalysisForInputs(ctx, nil, inputs)
	require.NoError(t, err)
	defer analysis.Close()
	nodes, err := analysis.NodesByKindsContext(ctx, []graph.NodeKind{graph.KindContract})
	require.NoError(t, err)
	out := make(map[string]*graph.Node, len(nodes))
	for _, node := range nodes {
		out[node.ID] = node
	}
	return out
}

func contractIDs(nodes map[string]*graph.Node) []string {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

const (
	jobRoutes = "package fixture\nfunc register(r Router) { r.GET(RoutePath, users) }\nfunc users() {}\n"
	jobConsts = "package fixture\nconst RoutePath = \"/before\"\n"
	jobHelper = "package fixture\nfunc Helper() int { return 1 }\n"
)

func newRouteConstantFixture(t *testing.T) *contractJobFixture {
	return newContractJobFixture(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})
}

// coldContracts builds the published contract set an independent job over
// the given final file state produces: the equivalence target.
func coldContracts(t *testing.T, files map[string]string) map[string]*graph.Node {
	t.Helper()
	cold := newContractJobFixture(t, files)
	report, err := cold.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	return cold.contracts()
}

// contractColdOracles memoizes coldContracts by file set across one test's
// rows: a cold build depends on nothing but its files, so rows that end on
// the same files compare against one build. Callers only read the result.
type contractColdOracles map[string]map[string]*graph.Node

func (o contractColdOracles) contracts(t *testing.T, files map[string]string) map[string]*graph.Node {
	t.Helper()
	key := fmt.Sprint(files) // map keys print sorted
	if nodes, ok := o[key]; ok {
		return nodes
	}
	nodes := coldContracts(t, files)
	o[key] = nodes
	return nodes
}

func contractMeta(nodes map[string]*graph.Node) map[string]any {
	out := make(map[string]any, len(nodes))
	for id, node := range nodes {
		out[id] = node.Meta
	}
	return out
}

func contractSymbols(nodes map[string]*graph.Node) map[string]any {
	out := make(map[string]any, len(nodes))
	for id, node := range nodes {
		out[id] = node.Meta["symbol_id"]
	}
	return out
}

// TestContractJobResolvesARouteConstantThroughItsOwner is the fixture's own
// premise: the route path is a constant another file declares, read through a
// follow-up lookup the ledger records with its owner.
func TestContractJobResolvesARouteConstantThroughItsOwner(t *testing.T) {
	f := newRouteConstantFixture(t)
	report, err := f.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Contains(t, contractIDs(f.contracts()), "http::GET::/before")
}

// TestContractJobSurvivesUnrelatedSavesAndConverges: a comment writer and a
// function-adding writer on a file the job read do not kill the job, which
// publishes once. A comment keeps the file's contract content, so the save
// after the enrichment round costs no iteration; an added function changes
// the file's declarations, so that file is re-extracted in a second one.
func TestContractJobSurvivesUnrelatedSavesAndConverges(t *testing.T) {
	for _, mode := range []string{"comment", "function"} {
		t.Run(mode, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			saves := 0
			write := func() {
				saves++
				text := jobHelper
				for i := 0; i < saves; i++ {
					if mode == "comment" {
						text += "// writer tick\n"
					} else {
						text += "func writerTick" + string(rune('A'+i)) + "() int { return 1 }\n"
					}
				}
				f.save("helper.go", text)
			}
			hooks := &contractConvergenceHooks{
				afterExtract: func(path string) {
					if path == "fixture/helper.go" && saves < 2 {
						write()
					}
				},
				afterPP: func() {
					if saves < 3 {
						write()
					}
				},
			}
			report, err := f.job(hooks, nil)
			require.NoError(t, err, "an unrelated writer must not end the job")
			require.True(t, report.Published)
			if mode == "comment" {
				require.Equal(t, 1, report.Iterations, "a save that keeps the contract content costs no iteration")
				require.Positive(t, report.ContentKept)
			} else {
				require.GreaterOrEqual(t, report.Iterations, 2, "the saved file is re-extracted inside the job")
			}
			require.Contains(t, contractIDs(f.contracts()), "http::GET::/before")
		})
	}
}

// TestContractJobOwnerMoveAfterProofIsReextracted: the route's
// owner (consts.go) moves after the route's unit was proven and before the
// fence (iv), or between the route's lookup and its point scan (i). Both end
// equal to a cold build of the final state. The rename row is the one where
// only the owner rule (b) sees the move: the new declaration no longer
// answers the route's key, so the key check does not fire.
func TestContractJobOwnerMoveAfterProofIsReextracted(t *testing.T) {
	rows := []struct {
		name, consts string
		at           string
	}{
		{"value_edit_before_fence", "package fixture\nconst RoutePath = \"/after\"\n", "fence"},
		{"rename_before_fence", "package fixture\nconst RoutePath2 = \"/after\"\n", "fence"},
		{"value_edit_between_lookup_and_point_scan", "package fixture\nconst RoutePath = \"/after\"\n", "extract"},
	}
	oracles := contractColdOracles{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			done := false
			hooks := &contractConvergenceHooks{
				beforeFence: func(iteration int) {
					if row.at == "fence" && !done {
						done = true
						f.saveKeepingKey("consts.go", row.consts)
					}
				},
				afterExtract: func(path string) {
					if row.at == "extract" && path == "fixture/routes.go" && !done {
						done = true
						f.saveKeepingKey("consts.go", row.consts)
					}
				},
			}
			report, err := f.job(hooks, nil)
			require.True(t, done)
			require.NoError(t, err)
			require.True(t, report.Published)
			want := oracles.contracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": row.consts, "helper.go": jobHelper})
			require.Equal(t, contractIDs(want), contractIDs(f.contracts()), "the published attachment equals a cold build of the final state")
		})
	}
}

// TestContractJobSameIdentityRewriteIsUnproven: the owner of the
// route's constant is rewritten with the same identity (a forced reparse)
// around the route's lookup: begun and evicted before it, re-added and
// accepted after it. Only the write seq tells the torn read apart.
func TestContractJobSameIdentityRewriteIsUnproven(t *testing.T) {
	f := newRouteConstantFixture(t)
	var release func()
	hooks := &contractConvergenceHooks{
		// One file per round: the route's lookup reaches the core, not the
		// owner's rows an earlier unit of the same round loaded.
		roundFiles: 1,
		beforeExtract: func(path string) {
			if path == "fixture/routes.go" && release == nil {
				release = f.beginHeld("fixture/consts.go")
			}
		},
		afterExtract: func(path string) {
			if path == "fixture/routes.go" && release != nil {
				release()
				release = func() {}
			}
		},
	}
	report, err := f.job(hooks, nil)
	require.NotNil(t, release)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Contains(t, contractIDs(f.contracts()), "http::GET::/before", "the torn lookup was retried, not published")
}

// TestContractJobKeyCheckReextractsEarlierLookups: a file looks up a
// name while the file declaring it is between evict and add; the declaring
// file settles and is proven in a later round of the same job. The key check
// on that proof makes the earlier unit stale.
func TestContractJobKeyCheckReextractsEarlierLookups(t *testing.T) {
	files := map[string]string{"a_routes.go": jobRoutes, "z_consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	var release func()
	hooks := &contractConvergenceHooks{
		roundFiles: 1,
		beforeExtract: func(path string) {
			if path == "fixture/a_routes.go" && release == nil {
				release = f.beginHeld("fixture/z_consts.go")
			}
		},
		afterExtract: func(path string) {
			if path == "fixture/a_routes.go" && release != nil {
				release()
				release = func() {}
			}
		},
	}
	report, err := f.job(hooks, nil)
	require.NotNil(t, release)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Contains(t, contractIDs(f.contracts()), "http::GET::/before", "the empty lookup was re-run once its answer was declared")
}

// TestContractJobRetriesOnFreshEvidence: a file re-extracted in
// a later round reads its own new rows, never the earlier round's evidence.
func TestContractJobRetriesOnFreshEvidence(t *testing.T) {
	original := "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /items\", serveItems) }\nfunc serveItems(w http.ResponseWriter, r *http.Request) {}\n"
	f := newContractJobFixture(t, map[string]string{"routes.go": original, "helper.go": jobHelper})
	// The file's own first attempt lands between its evict and its add: its
	// load finds no file owner, a sticky evidence error. The retry, after
	// the apply settles, must start from fresh evidence.
	var release func()
	hooks := &contractConvergenceHooks{
		beforeExtract: func(path string) {
			if path == "fixture/routes.go" && release == nil {
				release = f.beginHeld("fixture/routes.go")
			}
		},
		afterExtract: func(path string) {
			if path == "fixture/routes.go" && release != nil {
				release()
				release = func() {}
			}
		},
	}
	report, err := f.job(hooks, nil)
	require.NotNil(t, release)
	require.NoError(t, err, "the retry does not inherit the first attempt's evidence error")
	require.True(t, report.Published)
	require.GreaterOrEqual(t, report.Rounds, 2)
	want := coldContracts(t, map[string]string{"routes.go": original, "helper.go": jobHelper})
	require.Equal(t, contractSymbols(want), contractSymbols(f.contracts()))
}

// TestContractJobEnrichmentFollowsADepartedHandlerFile: the enrichment round
// resolved a route handler in another file; that file becomes an excluded
// failure envelope during the round. No unit read it and its own unit holds
// no records, so only the enrichment round's own recorded inputs (the live
// unit set it consumed and the handler lookup it answered) rerun it.
func TestContractJobEnrichmentFollowsADepartedHandlerFile(t *testing.T) {
	// The handler is a method value: per-file extraction leaves it to the
	// enrichment round's cross-file resolution, so no unit reads serve.go.
	routes := "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /items\", h.serveItems) }\n"
	serve := "package fixture\nimport \"net/http\"\ntype H struct{}\nfunc (H) serveItems(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, Item{}) }\ntype Item struct { Name string `json:\"name\"` }\nfunc writeJSON(w http.ResponseWriter, code int, body any) {}\n"
	files := map[string]string{"routes.go": routes, "serve.go": serve, "helper.go": jobHelper}
	registry := parser.NewRegistry()
	registry.Register(contractFailingGoExtractor{GoExtractor: languages.NewGoExtractor()})
	f := newContractJobFixtureWithRegistry(t, files, registry)
	done := false
	hooks := &contractConvergenceHooks{afterPP: func() {
		if !done {
			done = true
			before, _, err := f.store.ContractInputStateContext(context.Background(), "fixture", "")
			require.NoError(t, err)
			path := filepath.Join(f.root, "serve.go")
			// A save whose bytes fail extraction: the same content failure
			// on every platform.
			writeFile(t, path, serve+"// EXTRACT_FAIL from here on\n")
			future := time.Now().Add(time.Hour)
			require.NoError(t, os.Chtimes(path, future, future))
			_, _ = f.mi.IncrementalReindexRepo("fixture", []string{path})
			f.restoreKey(before)
			row, _, err := f.store.ContractBoundaryReceiptContext(context.Background(), "fixture", "", "fixture/serve.go")
			require.NoError(t, err)
			require.True(t, contractReceiptIsFailureEnvelope(row), "the handler file is now an excluded envelope")
		}
	}}
	report, err := f.job(hooks, nil)
	require.True(t, done)
	require.NoError(t, err)
	require.True(t, report.Published)
	want := coldContracts(t, map[string]string{"routes.go": routes, "helper.go": jobHelper})
	got := f.contracts()
	t.Logf("cold=%v job=%v", contractMeta(want), contractMeta(got))
	require.Equal(t, contractSymbols(want), contractSymbols(got), "the handler resolution follows the departed file")
}

// TestContractJobDerivesSeedsFromTheSelectedCore: a dependency
// injection owner edge from g into f is torn while f sits between its evict
// and add, exactly while g's unit loads its rows. g's receipt never moves, so
// g's unit keeps the torn load; only the enrichment round's own seed query,
// re-run after f settles, sees the edge again.
func TestContractJobDerivesSeedsFromTheSelectedCore(t *testing.T) {
	files := map[string]string{
		"routes.go": jobRoutes, "consts.go": jobConsts,
		"f.go": "package fixture\nfunc Target() int { return 2 }\n",
		"g.go": "package fixture\nfunc Wire() int { return 1 }\n",
	}
	seed := &graph.Edge{From: "fixture/g.go::Wire", To: "fixture/f.go::Target", Kind: graph.EdgeProvides, FilePath: "fixture/g.go", Line: 2,
		Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: "SYNTH"}}
	withSeed := func(f *contractJobFixture) {
		require.NotNil(t, f.store.GetNode(seed.From))
		require.NotNil(t, f.store.GetNode(seed.To))
		copyEdge := *seed
		require.NoError(t, f.store.AddBatchChecked(nil, []*graph.Edge{&copyEdge}))
	}
	cold := newContractJobFixture(t, files)
	withSeed(cold)
	report, err := cold.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Contains(t, contractIDs(cold.contracts()), "di::SYNTH", "the cold build derives the DI contract from the seed edge")

	f := newContractJobFixture(t, files)
	withSeed(f)
	var release func()
	hooks := &contractConvergenceHooks{
		roundFiles: 1,
		beforeExtract: func(path string) {
			if path == "fixture/g.go" && release == nil {
				release = f.beginHeld("fixture/f.go")
			}
		},
		afterExtract: func(path string) {
			if path == "fixture/g.go" && release != nil {
				release()
				release = func() {}
			}
		},
	}
	report, err = f.job(hooks, nil)
	require.NotNil(t, release)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, contractIDs(cold.contracts()), contractIDs(f.contracts()), "seeds come from the core at the enrichment round, not from a torn load")
}

// contractFailingGoExtractor is the Go extractor except that source holding
// EXTRACT_FAIL fails extraction, the content failure an unparseable save is.
type contractFailingGoExtractor struct{ *languages.GoExtractor }

func (e contractFailingGoExtractor) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	return e.ExtractWithOptions(path, src, parser.ExtractionOptions{})
}

func (e contractFailingGoExtractor) ExtractWithOptions(path string, src []byte, opts parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	if bytes.Contains(src, []byte("EXTRACT_FAIL")) {
		return nil, errors.New("go: unparseable source")
	}
	return e.GoExtractor.ExtractWithOptions(path, src, opts)
}

// TestContractJobPinnedReader: the accepted-bytes cache serves a file
// whose disk moved ahead of its receipt; without it the read is unproven and
// retried on the next Published event; a failure envelope is excluded.
func TestContractJobPinnedReader(t *testing.T) {
	t.Run("cache_hit_disk_ahead", func(t *testing.T) {
		f := newRouteConstantFixture(t)
		f.save("consts.go", "package fixture\nconst RoutePath = \"/cached\"\n")
		writeFile(t, filepath.Join(f.root, "consts.go"), "package fixture\nconst RoutePath = \"/disk-ahead\"\n")
		report, err := f.job(nil, nil)
		require.NoError(t, err)
		require.True(t, report.Published)
		require.Contains(t, contractIDs(f.contracts()), "http::GET::/cached", "the stamped bytes come from the cache")
	})
	t.Run("cache_miss_disk_ahead_retries_on_published", func(t *testing.T) {
		f := newRouteConstantFixture(t)
		f.save("consts.go", "package fixture\nconst RoutePath = \"/accepted\"\n")
		f.mi.contractSources = newContractAcceptedSourceCache(contractAcceptedSourceCacheBytes)
		writeFile(t, filepath.Join(f.root, "consts.go"), "package fixture\nconst RoutePath = \"/accepted\"\n// unindexed\n")
		settled := false
		hooks := &contractConvergenceHooks{afterExtract: func(path string) {
			if path == "fixture/routes.go" && !settled {
				settled = true
				// The save the disk was ahead by lands and is announced.
				f.saveKeepingKey("consts.go", "package fixture\nconst RoutePath = \"/accepted\"\n// unindexed\n")
			}
		}}
		report, err := f.job(hooks, nil)
		require.NoError(t, err)
		require.True(t, report.Published)
		require.GreaterOrEqual(t, report.Rounds, 2, "the unproven read was retried in a later round")
		require.Contains(t, contractIDs(f.contracts()), "http::GET::/accepted")
	})
	t.Run("envelope_excluded", func(t *testing.T) {
		// The save's bytes fail extraction: a deterministic content failure
		// on every platform, unlike a mode-000 file, which Windows still reads.
		registry := parser.NewRegistry()
		registry.Register(contractFailingGoExtractor{GoExtractor: languages.NewGoExtractor()})
		extraSource := "package fixture\nfunc registerExtra(r Router) { r.GET(\"/extra\", extra) }\nfunc extra() {}\n"
		f := newContractJobFixtureWithRegistry(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "extra.go": extraSource}, registry)
		require.Contains(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "extra.go": extraSource})), "http::GET::/extra", "the file's records are contracts before it fails")
		extra := filepath.Join(f.root, "extra.go")
		writeFile(t, extra, extraSource+"// EXTRACT_FAIL\n")
		future := time.Now().Add(time.Hour)
		require.NoError(t, os.Chtimes(extra, future, future))
		_, _ = f.mi.IncrementalReindexRepo("fixture", []string{extra})
		receipt, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/extra.go")
		require.NoError(t, err)
		require.True(t, contractReceiptIsFailureEnvelope(receipt), "the failed save is an accepted failure envelope")
		report, err := f.job(nil, nil)
		require.NoError(t, err)
		require.True(t, report.Published)
		ids := contractIDs(f.contracts())
		require.Contains(t, ids, "http::GET::/before")
		require.NotContains(t, ids, "http::GET::/extra", "an envelope's records are excluded")
	})
}

// TestContractJobRetriesOnlyOnPublished: an unproven read is retried on a
// Published or receipt event, or once a wait of the round wait sees none,
// never by polling; with the owner held in flight and no event at all, the
// job ends read_unsettled after contractIdleRounds waited-out rounds that
// proved nothing.
func TestContractJobRetriesOnlyOnPublished(t *testing.T) {
	f := newRouteConstantFixture(t)
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	// The owner is held mid-apply after capture, before the job's first
	// scan, and never released during the job: no event follows.
	f.adjust = func(*ContractFollowupSnapshot) {
		if release == nil {
			release = f.beginHeld("fixture/consts.go")
		}
	}
	const wait = 300 * time.Millisecond
	woke := 0
	start := time.Now()
	report, err := f.job(&contractConvergenceHooks{roundWait: wait, woke: func() { woke++ }}, contractJobDeadline(time.Minute))
	require.ErrorIs(t, err, errContractJobReadUnsettled)
	require.False(t, report.Published)
	require.Zero(t, woke, "no event woke a retry")
	require.GreaterOrEqual(t, time.Since(start), contractIdleRounds*wait, "each counted round waited out the round wait")
	require.Equal(t, viewmetrics.ContractJobSupersededReadUnsettled, contractJobOutcomeOf(err))
}

// TestContractChangeClockMarksStampAndCensusMoves pins the change clock as a
// pure function: a stamp change, an appearance, a disappearance and a census
// join or departure each stamp the path at the new clock; nothing else does.
func TestContractChangeClockMarksStampAndCensusMoves(t *testing.T) {
	stamp := func(fp string, seq uint64, accepted bool) ContractReceiptStamp {
		return ContractReceiptStamp{Fingerprint: fp, SourceFingerprint: fp, Accepted: accepted, Seq: seq}
	}
	var clock contractChangeClock
	require.Equal(t, uint64(1), clock.observe(map[string]ContractReceiptStamp{"a": stamp("1", 1, true), "o": stamp("1", 1, true)}, map[string]bool{"a": true, "o": true}))
	require.Equal(t, uint64(1), clock.changedAt["a"])
	require.Equal(t, uint64(2), clock.observe(map[string]ContractReceiptStamp{"a": stamp("1", 2, true), "o": stamp("1", 1, true)}, map[string]bool{"a": true, "o": true}))
	require.Equal(t, uint64(2), clock.changedAt["a"], "a seq bump at equal identity is a move")
	require.Equal(t, uint64(1), clock.changedAt["o"])
	// o leaves the census with its receipt row unchanged (a reconcile
	// fallback that wrote nothing): still a move.
	require.Equal(t, uint64(3), clock.observe(map[string]ContractReceiptStamp{"a": stamp("1", 2, true), "o": stamp("1", 1, true)}, map[string]bool{"a": true}))
	require.Equal(t, uint64(3), clock.changedAt["o"], "a census departure without a receipt write is a move")
	require.Equal(t, uint64(2), clock.changedAt["a"])
	require.Equal(t, uint64(4), clock.observe(map[string]ContractReceiptStamp{"a": stamp("1", 2, false)}, map[string]bool{"a": true, "n": true}))
	require.Equal(t, uint64(4), clock.changedAt["a"], "losing acceptance is a move")
	require.Equal(t, uint64(4), clock.changedAt["o"], "a disappearing row is a move")
	require.Equal(t, uint64(4), clock.changedAt["n"], "a census join is a move")
	require.True(t, clock.changedSince("n", 3))
	require.False(t, clock.changedSince("n", 4))
	require.Equal(t, uint64(4), clock.maxChanged)

	// A point observation stamps only the scanned paths that moved or lost
	// their row; the rest keep their last observation, and the stamp map a
	// caller holds from an earlier scan is never written.
	held := clock.last
	require.Equal(t, uint64(5), clock.observePoint([]string{"a", "o", "x"}, map[string]ContractReceiptStamp{"a": stamp("2", 3, true), "x": stamp("1", 1, true)}))
	require.Equal(t, uint64(5), clock.changedAt["a"], "a scanned row that moved")
	require.Equal(t, uint64(4), clock.changedAt["o"], "a scanned path with no row and none before is unchanged")
	require.Equal(t, uint64(5), clock.changedAt["x"], "a scanned path's first row")
	require.Equal(t, uint64(4), clock.changedAt["n"], "an unscanned path keeps its observation")
	require.Equal(t, stamp("1", 2, false), held["a"], "an earlier scan's stamps stay as read")
	require.Equal(t, uint64(5), clock.seenAt["a"], "a scan sees each row it read")
	require.Equal(t, uint64(3), clock.seenAt["o"], "a path with no row is not seen")
	require.Equal(t, uint64(6), clock.observePoint([]string{"x"}, map[string]ContractReceiptStamp{}))
	require.Equal(t, uint64(6), clock.changedAt["x"], "a scanned row that disappeared")
	require.Equal(t, uint64(7), clock.observePoint([]string{"a"}, map[string]ContractReceiptStamp{"a": stamp("2", 3, true)}))
	require.Equal(t, uint64(5), clock.changedAt["a"], "an unchanged scanned row is not a move")
	require.Equal(t, uint64(7), clock.seenAt["a"], "an unchanged scanned row is seen again")
	// A later full scan compares against the point observations.
	require.Equal(t, uint64(8), clock.observe(map[string]ContractReceiptStamp{"a": stamp("2", 3, true)}, map[string]bool{"a": true, "n": true}))
	require.Equal(t, uint64(5), clock.changedAt["a"])
	require.Equal(t, uint64(6), clock.changedAt["x"])
}

// newFixtureScanner builds a stamp scanner over the fixture's primary chain.
func newFixtureScanner(t *testing.T, f *contractJobFixture) *contractStampScanner {
	t.Helper()
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	return newContractStampScanner(inputs, map[string][]*store_sqlite.Store{"fixture": {f.store.AtGeneration(0)}}, nil)
}

// rewriteReceipt stages path's receipt with a new identity: one gen-0
// receipt write through a handle other than the scanner's.
func (f *contractJobFixture) rewriteReceipt(path, marker string) {
	f.t.Helper()
	staged := f.stagedReceipt(path, marker)
	require.NoError(f.t, f.store.AtGeneration(0).SetContractBoundaryReceiptsContext(context.Background(), []graph.ContractBoundaryReceipt{staged}))
}

func (f *contractJobFixture) stagedReceipt(path, marker string) graph.ContractBoundaryReceipt {
	f.t.Helper()
	row, _, err := f.store.ContractBoundaryReceiptContext(context.Background(), "fixture", "", path)
	require.NoError(f.t, err)
	staged := *row
	staged.Accepted = false
	staged.Previous = nil
	staged.SourceFingerprint = contractInputHash([]byte(marker))
	return staged
}

// TestContractStampScanSeesEveryOverlappingWrite is the scan interleaving
// pin: a receipt write placed at each phase of one full scan shows in the
// next full scan exactly when the first scan did not read all of it, with
// the reuse skip active. The in-flight row holds a write between its begin
// and its done across the first scan.
func TestContractStampScanSeesEveryOverlappingWrite(t *testing.T) {
	const path = "fixture/consts.go"
	for _, warm := range []bool{false, true} {
		for _, at := range []string{"before_scan", "sample", "rows", "seqs", "inputs", "in_flight"} {
			name := at
			if warm {
				name = "reuse_eligible/" + at
			}
			t.Run(name, func(t *testing.T) {
				contractStampScanPlacement(t, path, at, warm)
			})
		}
	}
}

func contractStampScanPlacement(t *testing.T, path, at string, warm bool) {
	f := newRouteConstantFixture(t)
	scanner := newFixtureScanner(t, f)
	var err error
	if warm {
		// A first quiet scan makes the next one eligible for reuse.
		_, err = scanner.scan(t.Context(), nil)
		require.NoError(t, err)
	}
	var first, second ContractFollowupScan
	switch at {
	case "in_flight":
		// The write begins before the first scan's sample, commits after its
		// rows, and stays undone (done not bumped) across the second scan's
		// sample: the first scan's bracket was not quiet.
		began, firstDone, committed, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		restore := store_sqlite.SetContractReceiptWriteHookForTest(func(stage string, paths []string) {
			if len(paths) != 1 || paths[0] != path {
				return
			}
			switch stage {
			case "begun":
				close(began)
				<-firstDone
			case "ending":
				close(committed)
				<-release
			}
		})
		defer restore()
		staged := f.stagedReceipt(path, at)
		writeErr := make(chan error, 1)
		go func() {
			writeErr <- f.store.AtGeneration(0).SetContractBoundaryReceiptsContext(context.Background(), []graph.ContractBoundaryReceipt{staged})
		}()
		<-began
		first, err = scanner.scan(t.Context(), nil)
		require.NoError(t, err)
		close(firstDone)
		<-committed
		second, err = scanner.scan(t.Context(), nil)
		require.NoError(t, err)
		close(release)
		require.NoError(t, <-writeErr)
	default:
		if at == "before_scan" {
			f.rewriteReceipt(path, at)
		} else {
			scanner.phase = func(phase string) {
				if phase == at {
					scanner.phase = nil
					f.rewriteReceipt(path, at)
				}
			}
		}
		first, err = scanner.scan(t.Context(), nil)
		require.NoError(t, err)
		scanner.phase = nil
		second, err = scanner.scan(t.Context(), nil)
		require.NoError(t, err)
	}
	require.True(t, first.Settled)
	fresh := newFixtureScanner(t, f)
	truth, err := fresh.scan(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, truth.Stamps[path], second.Stamps[path], "the second scan sees the committed write")
	switch {
	case at == "before_scan", at == "sample" && !warm:
		require.Equal(t, first.Stamps[path], second.Stamps[path], "the first scan already read the whole write")
	default:
		require.NotEqual(t, first.Stamps[path], second.Stamps[path], "a write overlapping the interval between two scans shows")
	}
}

// TestContractStampScanRereadsInputsAndChain pins the two reuse terms that
// need no receipt write: the input-state witnesses are re-read on every
// scan, and a scan reuses rows only over the same selected chain.
func TestContractStampScanRereadsInputsAndChain(t *testing.T) {
	t.Run("inputs", func(t *testing.T) {
		f := newRouteConstantFixture(t)
		scanner := newFixtureScanner(t, f)
		first, err := scanner.scan(t.Context(), nil)
		require.NoError(t, err)
		require.True(t, first.Settled)
		// A cold door moves the input state before any receipt write.
		state, _, err := f.store.ContractInputStateContext(t.Context(), "fixture", "")
		require.NoError(t, err)
		next := state
		next.Accepted = false
		next.InputFingerprint = contractInputHash([]byte("cold-door-pending"))
		require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &state, next, nil))
		second, err := scanner.scan(t.Context(), nil)
		require.NoError(t, err)
		require.Equal(t, 1, scanner.reused, "no receipt write: the rows are reused")
		require.False(t, second.Settled, "the witnesses are re-read even when the rows are reused")
	})
	t.Run("chain", func(t *testing.T) {
		f := newRouteConstantFixture(t)
		const path = "fixture/consts.go"
		// A linked actor's row exists before the first scan, outside the
		// chain that scan selects.
		row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", path)
		require.NoError(t, err)
		linked := *row
		linked.CheckoutID = "linked"
		linked.Accepted = false
		linked.Previous = nil
		linked.SourceFingerprint = contractInputHash([]byte("linked-generation"))
		require.NoError(t, f.store.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{linked}))
		scanner := newFixtureScanner(t, f)
		first, err := scanner.scan(t.Context(), nil)
		require.NoError(t, err)
		// The chain now selects the linked actor first, with no receipt
		// write in between: the bracket is unchanged.
		scanner.chains["fixture"][0].actors = []string{"linked", ""}
		second, err := scanner.scan(t.Context(), nil)
		require.NoError(t, err)
		require.NotEqual(t, first.Stamps[path], second.Stamps[path], "rows read over another chain are never reused")
		require.Equal(t, "linked", second.Stamps[path].Actor)
	})
	t.Run("next_job", func(t *testing.T) {
		f := newRouteConstantFixture(t)
		job1 := newFixtureScanner(t, f)
		for range 2 {
			_, err := job1.scan(t.Context(), nil)
			require.NoError(t, err)
		}
		require.Equal(t, 1, job1.reused, "a quiet second scan of one job reuses its rows")
		// No write between the jobs: only the scanner's lifetime keeps the
		// next job from reusing them.
		job2 := newFixtureScanner(t, f)
		_, err := job2.scan(t.Context(), nil)
		require.NoError(t, err)
		require.Zero(t, job2.reused, "a job never reuses another job's scan")
	})
}

// TestContractStampScanSeesALinkedWorktreeGenerationPublishedBetweenJobs is
// the linked-worktree variant of the scan interleaving pin. The linked
// checkout's first job selects only the base corpus; a worktree generation
// with the checkout's own receipts is then published and the next job selects
// it above the base. That job's stamps are the generation's rows, whether the
// generation's receipts were written after the first job's last scan phase
// (the write bracket moved) or before its first scan (only the scanner's
// lifetime and the chain in the reuse key tell the chains apart).
func TestContractStampScanSeesALinkedWorktreeGenerationPublishedBetweenJobs(t *testing.T) {
	const path = "fixture/consts.go"
	for _, placement := range []string{"after_last_sample", "before_first_scan"} {
		t.Run(placement, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			ctx := t.Context()
			generation, handle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "linked-worktree", CheckoutID: "linked", LayerID: "linked-" + placement, GenerationKind: "commit", BaseGenerationID: 0, TreeOID: "tree-linked", CreatedAt: 1})
			require.NoError(t, err)
			writeLinked := func() {
				row, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", path)
				require.NoError(t, err)
				linked := *row
				linked.CheckoutID = "linked"
				linked.Previous = nil
				linked.SourceFingerprint = contractInputHash([]byte("linked-generation"))
				require.NoError(t, handle.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{linked}))
			}
			if placement == "before_first_scan" {
				writeLinked()
			}
			job1 := newFixtureScanner(t, f)
			if placement == "after_last_sample" {
				job1.phase = func(phase string) {
					if phase == "inputs" {
						job1.phase = nil
						writeLinked()
					}
				}
			}
			first, err := job1.scan(ctx, nil)
			require.NoError(t, err)
			require.Equal(t, int64(0), first.Stamps[path].Generation)
			require.NoError(t, f.store.PublishPayloadGeneration(ctx, generation, 2000))
			// The next job's chain: the published generation above the base.
			inputs := &graphview.SelectedContractInputs{Witnesses: []graph.ContractInputWitness{
				{GenerationID: 0, State: graph.ContractInputState{RepoPrefix: "fixture"}},
				{GenerationID: generation, State: graph.ContractInputState{RepoPrefix: "fixture", CheckoutID: "linked"}},
			}}
			job2 := newContractStampScanner(inputs, map[string][]*store_sqlite.Store{"fixture": {f.store.AtGeneration(0), f.store.AtGeneration(generation)}}, nil)
			second, err := job2.scan(ctx, nil)
			require.NoError(t, err)
			require.NotEqual(t, first.Stamps[path], second.Stamps[path], "the next job sees the worktree generation's rows")
			require.Equal(t, generation, second.Stamps[path].Generation)
			require.Equal(t, "linked", second.Stamps[path].Actor)
			truth, err := newContractStampScanner(inputs, map[string][]*store_sqlite.Store{"fixture": {f.store.AtGeneration(0), f.store.AtGeneration(generation)}}, nil).scan(ctx, []string{path})
			require.NoError(t, err)
			require.Equal(t, truth.Stamps[path], second.Stamps[path])
		})
	}
}

// TestContractIncrementalApplyBracketsCoreWritesWithItsReceipt: across
// a real incremental save, the file's receipt is unaccepted with a bumped
// write seq when the journal's begin returns, before any core row of the file
// changes, and accepted only after the core write. The journal's begin and
// accept bracket lines are the seam.
func TestContractIncrementalApplyBracketsCoreWritesWithItsReceipt(t *testing.T) {
	f := newRouteConstantFixture(t)
	const path = "fixture/consts.go"
	scanner := newFixtureScanner(t, f)
	before, err := scanner.scan(t.Context(), nil)
	require.NoError(t, err)
	var checked atomic.Int32
	var failures []string
	var mu sync.Mutex
	fail := func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, msg)
	}
	stampOf := func() ContractReceiptStamp {
		scan, err := newFixtureScanner(t, f).scan(context.Background(), []string{path})
		if err != nil {
			fail(err.Error())
		}
		return scan.Stamps[path]
	}
	core, _ := observer.New(zapcore.DebugLevel)
	logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		switch entry.Message {
		case "contract journal begin":
			stamp := stampOf()
			if stamp.Accepted || stamp.Seq <= before.Stamps[path].Seq {
				fail("begin returned without an unaccepted, re-sequenced receipt")
			}
			if len(f.store.FindNodesByNameInRepo("Added", "fixture")) != 0 {
				fail("a core row changed before the receipt was rewritten")
			}
			checked.Add(1)
		case "contract journal accept":
			stamp := stampOf()
			if !stamp.Accepted {
				fail("accept logged with an unaccepted receipt")
			}
			if len(f.store.FindNodesByNameInRepo("Added", "fixture")) == 0 {
				fail("the receipt was accepted before the core write")
			}
			checked.Add(1)
		}
		return nil
	}))
	f.mi.indexers["fixture"].logger = logger
	f.save("consts.go", jobConsts+"func Added() {}\n")
	require.Empty(t, failures)
	require.Equal(t, int32(2), checked.Load(), "both bracket points were observed")
}

// TestContractFenceMutationPathMatrix is the stage's share of the mutation
// path matrix: each row applies one core mutation path at the job's final
// fence and asserts the job's outcome. A key-moving path ends the job
// superseded (the next job owns the new key); a key-preserving path is
// re-extracted inside the job and equals a cold build; a receipt left
// unaccepted ends it read_unsettled.
func TestContractFenceMutationPathMatrix(t *testing.T) {
	rows := []struct {
		name    string
		apply   func(f *contractJobFixture)
		outcome string
		final   map[string]string
	}{
		{"incremental_key_move", func(f *contractJobFixture) {
			f.save("routes.go", "package fixture\nfunc register(r Router) { r.GET(\"/moved\", users) }\nfunc users() {}\n")
		}, viewmetrics.ContractJobSupersededKeyMoved, nil},
		{"incremental_body_only", func(f *contractJobFixture) {
			f.save("helper.go", jobHelper+"// body-only\n")
		}, viewmetrics.ContractJobPublished, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper + "// body-only\n"}},
		{"deletion", func(f *contractJobFixture) {
			f.removeKeepingKey("helper.go")
		}, viewmetrics.ContractJobPublished, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}},
		{"index_repo", func(f *contractJobFixture) {
			_, err := f.mi.IndexRepo("fixture")
			require.NoError(f.t, err)
		}, viewmetrics.ContractJobSupersededKeyMoved, nil},
		{"baseline_reopened", func(f *contractJobFixture) {
			state, _, err := f.store.ContractInputStateContext(context.Background(), "fixture", "")
			require.NoError(f.t, err)
			next := state
			next.Accepted = false
			next.InputFingerprint = contractInputHash([]byte("baseline-reconciliation"))
			require.NoError(f.t, f.store.BeginContractInputMutationContext(context.Background(), &state, next, nil))
		}, viewmetrics.ContractJobSupersededKeyMoved, nil},
		// A receipt rewritten with its own identity and never accepted: the
		// file's contract content is unchanged and every read preceded the
		// rewrite, so the job publishes what it read.
		{"stuck_receipt_same_content", func(f *contractJobFixture) {
			f.beginHeld("fixture/helper.go")
		}, viewmetrics.ContractJobPublished, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}},
		// A save that changes the route owner's declarations, then held
		// between its evict and its add with no event after: the route's
		// reads of it can never be asked again in a quiescent window.
		{"stuck_receipt_new_content", func(f *contractJobFixture) {
			f.saveKeepingKey("consts.go", jobConsts+"func Extra() int { return 1 }\n")
			f.beginHeld("fixture/consts.go")
		}, viewmetrics.ContractJobSupersededReadUnsettled, nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			applied := false
			hooks := &contractConvergenceHooks{beforeFence: func(int) {
				if !applied {
					applied = true
					row.apply(f)
				}
			}}
			if row.outcome == viewmetrics.ContractJobSupersededReadUnsettled {
				// The held receipt is never released and nothing announces,
				// so every round waits its full wait out: the outcome is
				// counted in rounds, not time. Wait 200ms, not the 3s
				// production default, for each of the contractIdleRounds.
				hooks.roundWait = 200 * time.Millisecond
			}
			report, err := f.job(hooks, nil)
			require.True(t, applied)
			require.Equal(t, row.outcome, contractJobOutcomeOf(err), "job error: %v", err)
			if row.final == nil {
				require.False(t, report.Published)
				return
			}
			require.True(t, report.Published)
			require.Equal(t, contractIDs(coldContracts(t, row.final)), contractIDs(f.contracts()))
		})
	}
}

// TestContractJobEnrichmentReadTornByAnApplyIsRetried holds a file between its
// evict and its add exactly while the enrichment round runs (after its R0,
// before its reads), then lets the apply settle and announce. A torn read in
// the enrichment round is a move: the round retries on the Published event
// and the job publishes the unchanged contracts. The composed-chain row takes
// the path a linked-worktree chain takes, with no physical seed query: the
// round loads every live file, the held one included. The held_across_r0 row
// holds the file before the round's R0: its stamp does not move between R0
// and the failed read, but it was unaccepted at R0.
func TestContractJobEnrichmentReadTornByAnApplyIsRetried(t *testing.T) {
	// Every row publishes the unchanged fixture: one cold build is the oracle.
	oracles := contractColdOracles{}
	for _, row := range []struct {
		name, held         string
		composed, beforeR0 bool
	}{
		{"seed_query", "fixture/routes.go", false, false},
		{"composed_chain", "fixture/helper.go", true, false},
		{"composed_chain_held_across_r0", "fixture/helper.go", true, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			if row.composed {
				f.adjust = func(snapshot *ContractFollowupSnapshot) {
					require.NotNil(t, snapshot.Seeds)
					snapshot.Seeds = func(context.Context, string) ([]*graph.Node, []*graph.Edge, bool, error) {
						return nil, nil, false, nil
					}
				}
			}
			armed, held := false, false
			var release func()
			hold := func() {
				held = true
				release = f.beginHeld(row.held)
			}
			// The held apply settles and announces as soon as the job waits
			// on its torn read, rather than after a fixed delay.
			f.onWait = func() {
				if release != nil {
					release()
					release = nil
				}
			}
			hooks := &contractConvergenceHooks{
				beforeR1: func([]string) { armed = true },
				openedEvidence: func(*store_sqlite.Store) {
					if armed && !held && !row.beforeR0 {
						hold()
					}
				},
				// The last unit of the only round is accepted: the next scan
				// is the enrichment round's R0.
				proven: func(unit *contractUnit) {
					if row.beforeR0 && !held && unit.path == "fixture/routes.go" {
						hold()
					}
				},
			}
			report, err := f.job(hooks, nil)
			f.onWait = nil
			require.True(t, held)
			require.Nil(t, release, "the job waited on the torn read; the apply settled during that wait")
			require.NoError(t, err, "a torn enrichment read is a move, not a job failure")
			require.True(t, report.Published)
			require.Equal(t, contractIDs(oracles.contracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractIDs(f.contracts()))
		})
	}
}

// TestContractJobProvesRetriedUnitsOneByOneUnderAWriter places a save of the
// owner every route reads before each extraction but the first after the
// job starts or wakes on a Published event: the writer's quiet window fits
// one unit's reads, never a whole round's. A retried unit runs in its own
// point round, its R0 right before its reads and its R1 right after, so each
// retry pass proves its first unit and the job publishes; proving a pass's
// units together against one full R0 would never prove any. Each save changes
// the constant's value with the key kept, so a unit proven in a retry before
// a later save must still be re-extracted: the publish equals a cold build of
// the last save.
func TestContractJobProvesRetriedUnitsOneByOneUnderAWriter(t *testing.T) {
	// The slow row stretches each retry pass's proving round past
	// contractIdleRounds round waits: rounds that make progress, not time,
	// bound a job.
	for _, slow := range []bool{false, true} {
		t.Run(map[bool]string{false: "fast", true: "slow_retries"}[slow], func(t *testing.T) {
			routes2 := "package fixture\nfunc registerMore(r Router) { r.POST(RoutePath, more) }\nfunc more() {}\n"
			files := map[string]string{"routes.go": jobRoutes, "routes2.go": routes2, "consts.go": jobConsts, "helper.go": jobHelper}
			f := newContractJobFixture(t, files)
			writing, first, woke, saves := true, true, false, 0
			slowPasses := 0
			consts := func(n int) string { return fmt.Sprintf("package fixture\nconst RoutePath = \"/tick-%d\"\n", n) }
			// Only the ratio matters: a slow round longer than
			// contractIdleRounds waits (4 waits against 3).
			const slowRound, wait = time.Second, 250 * time.Millisecond
			hooks := &contractConvergenceHooks{
				woke: func() { first, woke = true, true },
				// Each retry pass opens its evidence once before it proves
				// anything (an extraction or a revalidation): only that open
				// is slow.
				openedEvidence: func(*store_sqlite.Store) {
					if slow && writing && woke {
						woke = false
						slowPasses++
						time.Sleep(slowRound)
					}
				},
				beforeExtract: func(string) {
					if !writing {
						return
					}
					if first {
						first = false
						return
					}
					saves++
					f.saveKeepingKey("consts.go", consts(saves))
				},
				// The writer stops once the first iteration's rounds are done.
				afterPP: func() { writing = false },
			}
			if slow {
				hooks.roundWait = wait
			}
			start := time.Now()
			report, err := f.job(hooks, nil)
			require.NoError(t, err, "a unit whose reads fit the writer's window is proven")
			require.True(t, report.Published)
			require.Greater(t, saves, 2)
			t.Logf("saves=%d rounds=%d iterations=%d elapsed=%s", saves, report.Rounds, report.Iterations, time.Since(start))
			if slow {
				require.Positive(t, slowPasses, "a retry pass woken by an event ran slow")
				require.Greater(t, slowRound, contractIdleRounds*wait, "a slow pass outlasts the idle rounds' waits")
				require.Greater(t, time.Since(start), contractIdleRounds*wait, "the retry passes outlasted the idle rounds' waits")
			}
			want := coldContracts(t, map[string]string{"routes.go": jobRoutes, "routes2.go": routes2, "consts.go": consts(saves), "helper.go": jobHelper})
			require.Contains(t, contractIDs(want), fmt.Sprintf("http::POST::/tick-%d", saves))
			require.Equal(t, contractIDs(want), contractIDs(f.contracts()))
		})
	}
}

// TestContractAnalysisCoordinatorPublishedIsLevelTriggered pins the
// coordinator's Published epoch a running job waits on: an accept and its
// Published landing between a job's epoch sample and its wait wake the wait
// at once, a Published during the wait wakes it, and with none it returns at
// its deadline.
func TestContractAnalysisCoordinatorPublishedIsLevelTriggered(t *testing.T) {
	f := newContractWorkerFixture(t)
	capture := func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		return ContractFollowupSnapshot{}, nil, graph.ErrContractProjectionStale
	}
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: capture, Yield: func(ctx context.Context) error { return ctx.Err() }})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	since := c.publishedEpoch()
	c.Published(t.Context(), "repo-a", "")
	start := time.Now()
	require.True(t, c.waitPublished(t.Context(), since, time.Now().Add(2*testTimingSlack(30*time.Second))), "an announcement before the wait still wakes it")
	require.Less(t, time.Since(start), testTimingSlack(30*time.Second), "the wait returns at once, not at its deadline")

	since = c.publishedEpoch()
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.Published(context.Background(), "repo-a", "")
	}()
	require.True(t, c.waitPublished(t.Context(), since, time.Now().Add(time.Minute)), "an announcement during the wait wakes it")

	since = c.publishedEpoch()
	start = time.Now()
	require.False(t, c.waitPublished(t.Context(), since, time.Now().Add(100*time.Millisecond)), "no announcement: the deadline ends the wait")
	require.GreaterOrEqual(t, time.Since(start), 100*time.Millisecond)
}

// TestContractJobRetryWaitsWhileAnOwnerIsInFlight holds the owner the route
// read between its evict and its add right after the route's first
// extraction, and announces unrelated acceptances meanwhile. A retry round
// whose R0 finds an owner the last attempt read still in flight ends without
// reading; the route's reads are asked again only once the owner is accepted
// (the owner was rewritten unchanged, so its answers are and the route's unit
// is revalidated, not extracted).
func TestContractJobRetryWaitsWhileAnOwnerIsInFlight(t *testing.T) {
	f := newRouteConstantFixture(t)
	extractions, revalidations := 0, 0
	var done chan struct{}
	hooks := &contractConvergenceHooks{
		revalidated: func(path string, equal bool) {
			if path == "fixture/routes.go" {
				revalidations++
				require.True(t, equal, "the owner came back unchanged")
			}
		},
		afterExtract: func(path string) {
			if path != "fixture/routes.go" {
				return
			}
			extractions++
			if done != nil {
				return
			}
			done = make(chan struct{})
			release := f.beginHeld("fixture/consts.go")
			go func() {
				defer close(done)
				for range 3 {
					time.Sleep(100 * time.Millisecond)
					f.announce()
				}
				time.Sleep(100 * time.Millisecond)
				release()
			}()
		},
	}
	report, err := f.job(hooks, nil)
	require.NotNil(t, done)
	<-done
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 1, extractions, "the route is not extracted again")
	require.Equal(t, 1, revalidations, "no retry reads while the owner is in flight")
	require.Contains(t, contractIDs(f.contracts()), "http::GET::/before")
}

// TestContractJobOwnerInFlightBeforeR0IsDiscardedInRound places the owner's
// value edit before the route's first R0, held mid-apply: with its receipt
// begun and its core not yet written, or with its core written and its accept
// pending. The apply completes after the route's first lookup. The route's
// first unit is discarded in-round, and the publish equals a cold build of the
// edited owner.
func TestContractJobOwnerInFlightBeforeR0IsDiscardedInRound(t *testing.T) {
	const consts = "package fixture\nconst RoutePath = \"/after\"\n"
	for _, row := range []string{"core_not_written", "accept_pending"} {
		t.Run(row, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			ctx := context.Background()
			var release func()
			f.adjust = func(*ContractFollowupSnapshot) {
				if row == "accept_pending" {
					f.saveKeepingKey("consts.go", consts)
				}
				staged := f.stagedReceipt("fixture/consts.go", "in-flight")
				if row == "accept_pending" {
					// The core already holds the edit; only the accept is
					// missing, so the identity stays the edit's own.
					current, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/consts.go")
					require.NoError(t, err)
					staged.SourceFingerprint = current.SourceFingerprint
				}
				require.NoError(t, f.store.AtGeneration(0).SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{staged}))
				release = func() {
					require.NoError(t, f.store.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{staged}))
					if row == "core_not_written" {
						f.saveKeepingKey("consts.go", consts)
						return
					}
					f.announce()
				}
			}
			extractions, revalidations := 0, 0
			hooks := &contractConvergenceHooks{
				afterExtract: func(path string) {
					if path != "fixture/routes.go" {
						return
					}
					extractions++
					if extractions == 1 {
						release()
					}
				},
				revalidated: func(path string, _ bool) {
					if path == "fixture/routes.go" {
						revalidations++
					}
				},
			}
			report, err := f.job(hooks, nil)
			require.NoError(t, err)
			require.True(t, report.Published)
			// The first unit is never published as proven by its first
			// attempt: its reads are asked again (revalidated when the
			// owner's core already held the edit, extracted again otherwise).
			require.GreaterOrEqual(t, extractions+revalidations, 2, "the route's first unit was discarded in-round")
			want := coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": consts, "helper.go": jobHelper})
			require.Contains(t, contractIDs(want), "http::GET::/after")
			require.Equal(t, contractIDs(want), contractIDs(f.contracts()))
		})
	}
}

// TestContractJobShapeFollowsASecondEnrichmentRound: the response type's
// fields change, with the key kept, right after the first enrichment round.
// The second enrichment round must read the type afresh: a shape the first
// round snapshotted onto the type's node in its own store must not survive
// into the publish, which equals a cold build of the edited type.
func TestContractJobShapeFollowsASecondEnrichmentRound(t *testing.T) {
	routes := "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /items\", serveItems) }\nfunc serveItems(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, Item{}) }\nfunc writeJSON(w http.ResponseWriter, code int, body any) {}\n"
	before := "package fixture\ntype Item struct { Name string `json:\"name\"` }\n"
	after := "package fixture\ntype Item struct { Title string `json:\"title\"` }\n"
	f := newContractJobFixture(t, map[string]string{"routes.go": routes, "types.go": before, "helper.go": jobHelper})
	edited := false
	hooks := &contractConvergenceHooks{afterPP: func() {
		if !edited {
			edited = true
			f.saveKeepingKey("types.go", after)
		}
	}}
	report, err := f.job(hooks, nil)
	require.True(t, edited)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.GreaterOrEqual(t, report.Iterations, 2, "the edit after the enrichment round reran it")
	require.Equal(t, 1, report.EnrichReruns, "the type's changed answer reran the round once")
	want := coldContracts(t, map[string]string{"routes.go": routes, "types.go": after, "helper.go": jobHelper})
	require.Contains(t, fmt.Sprint(contractMeta(want)), "title", "the cold build carries the edited shape")
	require.Equal(t, contractMeta(want), contractMeta(f.contracts()))
}

// TestContractJobProvidesEdgeAddedAfterTheEnrichmentRound: a settled edit of a
// contract-free file lands after the enrichment round and brings a dependency
// injection provides edge with it, with the declaration that provides it (an
// extracted provider edge always comes from a declaration the file's receipt
// carries). No unit's records change, so only the enrichment round's own
// staleness reruns its seed query; the publish equals a cold build with the
// edge.
func TestContractJobProvidesEdgeAddedAfterTheEnrichmentRound(t *testing.T) {
	g := "package fixture\nfunc Wire() int { return 1 }\n"
	files := map[string]string{
		"routes.go": jobRoutes, "consts.go": jobConsts,
		"f.go": "package fixture\nfunc Target() int { return 2 }\n",
		"g.go": g,
	}
	seed := graph.Edge{From: "fixture/g.go::Wire", To: "fixture/f.go::Target", Kind: graph.EdgeProvides, FilePath: "fixture/g.go", Line: 2,
		Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: "SYNTH"}}
	addSeed := func(f *contractJobFixture) {
		copyEdge := seed
		require.NoError(t, f.store.AddBatchChecked(nil, []*graph.Edge{&copyEdge}))
	}
	edit := g + "func Provided() int { return 2 }\n"
	final := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "f.go": files["f.go"], "g.go": edit}
	cold := newContractJobFixture(t, final)
	addSeed(cold)
	report, err := cold.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Contains(t, contractIDs(cold.contracts()), "di::SYNTH")

	f := newContractJobFixture(t, files)
	edited := false
	hooks := &contractConvergenceHooks{afterPP: func() {
		if !edited {
			edited = true
			f.saveKeepingKey("g.go", edit)
			addSeed(f)
		}
	}}
	report, err = f.job(hooks, nil)
	require.True(t, edited)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 1, report.EnrichReruns, "the new provider reran the enrichment round exactly once")
	require.Equal(t, contractIDs(cold.contracts()), contractIDs(f.contracts()))
}

// TestContractStampScanHidesRowsBelowAClaimingGeneration: a generation that
// claims a path by a replace or delete mask hides every row below it, as the
// selected core does, in full and point scans; its own row, when it has one,
// wins.
func TestContractStampScanHidesRowsBelowAClaimingGeneration(t *testing.T) {
	const path = "fixture/consts.go"
	for _, row := range []struct {
		name    string
		mode    store_sqlite.OwnershipMode
		ownRow  bool
		visible bool
	}{
		{"replace_without_row", store_sqlite.OwnershipReplace, false, false},
		{"delete_without_row", store_sqlite.OwnershipDelete, false, false},
		{"context_claims_nothing", store_sqlite.OwnershipContext, false, true},
		{"replace_with_row", store_sqlite.OwnershipReplace, true, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			ctx := t.Context()
			generation, handle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "linked-worktree", CheckoutID: "linked", LayerID: "linked-" + row.name, GenerationKind: "commit", BaseGenerationID: 0, TreeOID: "tree-linked", CreatedAt: 1})
			require.NoError(t, err)
			require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "fixture", FilePath: path, Mode: row.mode}}))
			if row.mode == store_sqlite.OwnershipReplace {
				// A replace mask stands for the generation's own payload of
				// the path; whether it also carries a receipt row is the row's
				// subject.
				require.NoError(t, handle.AddBatchChecked([]*graph.Node{{ID: path, Name: "consts.go", Kind: graph.KindFile, FilePath: path, RepoPrefix: "fixture", Language: "go"}}, nil))
			}
			if row.ownRow {
				base, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", path)
				require.NoError(t, err)
				linked := *base
				linked.CheckoutID = "linked"
				linked.Previous = nil
				require.NoError(t, handle.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{linked}))
			}
			require.NoError(t, f.store.PublishPayloadGeneration(ctx, generation, 2000))
			inputs := &graphview.SelectedContractInputs{Witnesses: []graph.ContractInputWitness{
				{GenerationID: 0, State: graph.ContractInputState{RepoPrefix: "fixture"}},
				{GenerationID: generation, State: graph.ContractInputState{RepoPrefix: "fixture", CheckoutID: "linked"}},
			}}
			for _, scan := range []struct {
				name  string
				paths []string
			}{{"full", nil}, {"point", []string{path}}} {
				got, err := newContractStampScanner(inputs, map[string][]*store_sqlite.Store{"fixture": {f.store.AtGeneration(0), f.store.AtGeneration(generation)}}, nil).scan(ctx, scan.paths)
				require.NoError(t, err)
				stamp, ok := got.Stamps[path]
				require.Equal(t, row.visible, ok, "%s scan", scan.name)
				if row.ownRow {
					require.Equal(t, generation, stamp.Generation, "%s scan: the claiming generation's own row wins", scan.name)
				} else if ok {
					require.Equal(t, int64(0), stamp.Generation)
				}
				if scan.paths == nil {
					require.Contains(t, got.Stamps, "fixture/routes.go", "full scan: unclaimed paths still show the base row")
				}
			}
		})
	}
}

// TestContractStampScanFoldLeavesEveryStampEqual: folding a worktree chain
// into one generation copies its receipt rows verbatim and bumps no write seq.
// Every stamp keeps its identity, acceptance, actor and seq; only the
// generation that holds the row changes, and the write bracket does not move.
func TestContractStampScanFoldLeavesEveryStampEqual(t *testing.T) {
	f := newRouteConstantFixture(t)
	ctx := t.Context()
	generation, handle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "linked-worktree", CheckoutID: "linked", LayerID: "linked-member", GenerationKind: "commit", BaseGenerationID: 0, TreeOID: "tree-linked", CreatedAt: 1})
	require.NoError(t, err)
	var linked []graph.ContractBoundaryReceipt
	for _, path := range []string{"fixture/consts.go", "fixture/routes.go"} {
		base, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", path)
		require.NoError(t, err)
		row := *base
		row.CheckoutID = "linked"
		row.Previous = nil
		row.SourceFingerprint = contractInputHash([]byte("linked:" + path))
		linked = append(linked, row)
	}
	require.NoError(t, handle.SetContractBoundaryReceiptsContext(ctx, linked))
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, generation, 2000))
	scanOver := func(gen int64) ContractFollowupScan {
		inputs := &graphview.SelectedContractInputs{Witnesses: []graph.ContractInputWitness{
			{GenerationID: 0, State: graph.ContractInputState{RepoPrefix: "fixture"}},
			{GenerationID: gen, State: graph.ContractInputState{RepoPrefix: "fixture", CheckoutID: "linked"}},
		}}
		scan, err := newContractStampScanner(inputs, map[string][]*store_sqlite.Store{"fixture": {f.store.AtGeneration(0), f.store.AtGeneration(gen)}}, nil).scan(ctx, nil)
		require.NoError(t, err)
		return scan
	}
	before := scanOver(generation)
	begun, done := f.store.ContractReceiptWriteSample("fixture")
	seqs := f.store.ContractReceiptWriteSeqs("fixture", "linked", []string{"fixture/consts.go", "fixture/routes.go"})
	folded, _, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "linked-worktree", CheckoutID: "linked", LayerID: "linked-folded", GenerationKind: "commit", BaseGenerationID: 0, TreeOID: "tree-linked-folded", CreatedAt: 2})
	require.NoError(t, err)
	_, err = f.store.FlattenGenerationChain(ctx, []int64{generation}, folded)
	require.NoError(t, err)
	afterBegun, afterDone := f.store.ContractReceiptWriteSample("fixture")
	require.Equal(t, [2]uint64{begun, done}, [2]uint64{afterBegun, afterDone}, "a fold writes no receipt through the sequenced path")
	require.Equal(t, seqs, f.store.ContractReceiptWriteSeqs("fixture", "linked", []string{"fixture/consts.go", "fixture/routes.go"}), "the building copy preserves every original process write sequence")
	// The production chain selects the folded payload only after publication.
	// Compare stamps with the same immutable-core proof on both sides.
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, folded, 3000))
	after := scanOver(folded)
	require.Len(t, after.Stamps, len(before.Stamps))
	for path, stamp := range before.Stamps {
		moved := after.Stamps[path]
		if stamp.Generation == generation {
			require.Equal(t, folded, moved.Generation, path)
			moved.Generation = generation
		}
		require.Equal(t, stamp, moved, "%s: the fold leaves the stamp equal", path)
	}
}

// TestContractJobEnrichmentHoldsUnderABodyOnlyWriter is the first pin of the
// enrichment round's staleness rule: it reruns only when one of its recorded
// inputs changed. A body-only writer saves one file ten times, as a steady
// writer does while a job runs: nine saves land during the first iteration's
// rounds, the tenth right after the enrichment round, with a declaration
// change of a file nothing reads. Each body-only save keeps the file's
// contract content, so no unit and no enrichment round goes stale for it: the
// second iteration, which the declaration change costs, keeps the round with
// no rerun and no supersession. In the owner row the route's unit read the
// saved file (its constant's owner): it is never extracted again.
func TestContractJobEnrichmentHoldsUnderABodyOnlyWriter(t *testing.T) {
	const renamedPad = "package fixture\nfunc pad0Renamed() {}\n"
	for _, row := range []struct {
		name, file string
		text       func(n int) string
	}{
		{"unread_file", "helper.go", func(n int) string {
			return fmt.Sprintf("package fixture\nfunc Helper() int { return %d }\n", n+1)
		}},
		// The route's constant owner declares a function whose body the
		// writer keeps changing; the constant's own node, the route's
		// answer, holds.
		{"owner_body", "consts.go", func(n int) string {
			return jobConsts + fmt.Sprintf("\nfunc tick() int { return %d }\n", n)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			initial := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
			// Files the first iteration extracts in rounds of their own, each
			// round a window the writer saves in.
			for i := range 9 {
				initial[fmt.Sprintf("pad%d.go", i)] = fmt.Sprintf("package fixture\nfunc pad%d() {}\n", i)
			}
			initial[row.file] = row.text(0)
			f := newContractJobFixture(t, initial)
			saves := 0
			write := func() {
				saves++
				if row.file == "consts.go" {
					// The route's own dependency: a save there moves the
					// receipt dependents' key, so the owner row keeps it to
					// stay a body-level save.
					f.saveKeepingKey(row.file, row.text(saves))
					return
				}
				f.save(row.file, row.text(saves))
			}
			routeExtractions, ran := 0, false
			hooks := &contractConvergenceHooks{
				roundFiles: 1,
				afterExtract: func(path string) {
					if path == "fixture/routes.go" {
						routeExtractions++
					}
					if !ran && strings.HasPrefix(path, "fixture/pad") {
						write()
					}
				},
				afterPP: func() {
					if ran {
						return
					}
					ran = true
					write()
					// A file no read of the job holds changes its declarations
					// too, so the job takes a second iteration in which the
					// enrichment round is asked whether it still holds after
					// the body-only saves.
					f.save("pad0.go", renamedPad)
				},
			}
			report, err := f.job(hooks, nil)
			require.NoError(t, err, "a body-only writer never supersedes the job")
			require.True(t, report.Published)
			t.Logf("iterations=%d rounds=%d files=%d revalidated=%d content_kept=%d pp_runs=%d pp_rechecks=%d", report.Iterations, report.Rounds, report.Files, report.Revalidated, report.ContentKept, report.EnrichRuns, report.EnrichRechecks)
			require.Equal(t, 10, saves)
			require.Equal(t, 2, report.Iterations, "only the renamed declaration cost an iteration")
			require.Equal(t, 1, report.EnrichRuns)
			require.Zero(t, report.EnrichReruns, "unchanged recorded inputs never rerun the enrichment round")
			require.Positive(t, report.ContentKept)
			if row.file == "consts.go" {
				require.Equal(t, 1, routeExtractions, "the route's unit is never extracted again")
			}
			final := maps.Clone(initial)
			final[row.file] = row.text(saves)
			final["pad0.go"] = renamedPad
			require.Equal(t, contractIDs(coldContracts(t, final)), contractIDs(f.contracts()))
		})
	}
}

// contractCountingReader is an instrumented core reader: every method of the
// reader interface and every checked door a contract job's reads reach the
// core through counts one call.
type contractCountingReader struct {
	graph.Reader
	count func()
}

func (r *contractCountingReader) GetNode(id string) *graph.Node {
	r.count()
	return r.Reader.GetNode(id)
}
func (r *contractCountingReader) GetNodeByQualName(q string) *graph.Node {
	r.count()
	return r.Reader.GetNodeByQualName(q)
}
func (r *contractCountingReader) FindNodesByName(name string) []*graph.Node {
	r.count()
	return r.Reader.FindNodesByName(name)
}
func (r *contractCountingReader) FindNodesByNameContaining(s string, limit int) []*graph.Node {
	r.count()
	return r.Reader.FindNodesByNameContaining(s, limit)
}
func (r *contractCountingReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	r.count()
	return r.Reader.GetNodesByIDs(ids)
}
func (r *contractCountingReader) GetFileNodes(path string) []*graph.Node {
	r.count()
	return r.Reader.GetFileNodes(path)
}
func (r *contractCountingReader) GetRepoNodes(repo string) []*graph.Node {
	r.count()
	return r.Reader.GetRepoNodes(repo)
}
func (r *contractCountingReader) GetOutEdges(id string) []*graph.Edge {
	r.count()
	return r.Reader.GetOutEdges(id)
}
func (r *contractCountingReader) GetInEdges(id string) []*graph.Edge {
	r.count()
	return r.Reader.GetInEdges(id)
}
func (r *contractCountingReader) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	r.count()
	return r.Reader.GetInEdgesByNodeIDs(ids)
}
func (r *contractCountingReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	r.count()
	return r.Reader.GetOutEdgesByNodeIDs(ids)
}
func (r *contractCountingReader) AllNodes() []*graph.Node { r.count(); return r.Reader.AllNodes() }
func (r *contractCountingReader) AllEdges() []*graph.Edge { r.count(); return r.Reader.AllEdges() }
func (r *contractCountingReader) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	r.count()
	return r.Reader.EdgesByKind(kind)
}
func (r *contractCountingReader) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	r.count()
	return r.Reader.NodesByKind(kind)
}
func (r *contractCountingReader) NodeCount() int { r.count(); return r.Reader.NodeCount() }
func (r *contractCountingReader) EdgeCount() int { r.count(); return r.Reader.EdgeCount() }
func (r *contractCountingReader) EdgeIdentityRevisions() int {
	r.count()
	return r.Reader.EdgeIdentityRevisions()
}
func (r *contractCountingReader) Stats() graph.GraphStats { r.count(); return r.Reader.Stats() }
func (r *contractCountingReader) RepoStats() map[string]graph.GraphStats {
	r.count()
	return r.Reader.RepoStats()
}

func (r *contractCountingReader) Unwrap() graph.Reader { r.count(); return r.Reader }

func (r *contractCountingReader) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	r.count()
	return graph.VisitNodesByNamesContext(ctx, r.Reader, names, yield)
}

func (r *contractCountingReader) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	r.count()
	return graph.VisitNodesByNameContext(ctx, r.Reader, name, yield)
}

func (r *contractCountingReader) SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	r.count()
	if reader, ok := r.Reader.(graph.SemanticBindingTypeReader); ok {
		return reader.SemanticBindingTypes(sites)
	}
	return nil, nil
}

// TestContractJobEnrichmentReadsTheCoreOnlyThroughTheLedger is the facade
// guard: with an instrumented core reader (every reader method and every
// checked door), seed query and file loader, every core read the enrichment
// round makes happens inside a recorded read of the ledger facade. A pp read
// that bypasses the ledger would escape the round's staleness rule; this test
// counts it. Each row drives doors of its own: the seed query, name lookups
// and binding types (a handler responding with a variable); a composed
// chain's per-file loads; the output endpoints of Spring configuration reads.
func TestContractJobEnrichmentReadsTheCoreOnlyThroughTheLedger(t *testing.T) {
	// The handler responds with a call's result, whose type only a binding
	// lookup can tell.
	routes := "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /items\", serveItems) }\nfunc serveItems(w http.ResponseWriter, r *http.Request) { item := newItem(); writeJSON(w, 200, item) }\nfunc writeJSON(w http.ResponseWriter, code int, body any) {}\n"
	goFiles := map[string]string{
		"routes.go": routes, "types.go": "package fixture\ntype Item struct { Name string `json:\"name\"` }\nfunc newItem() Item { return Item{} }\n",
		"f.go": "package fixture\nfunc Target() int { return 2 }\n", "g.go": "package fixture\nfunc Wire() int { return 1 }\n",
	}
	springFiles := map[string]string{
		"routes.go": jobRoutes, "consts.go": jobConsts,
		"Greeter.java":           "import org.springframework.beans.factory.annotation.Value;\npublic class Greeter {\n  @Value(\"${app.greeting}\")\n  private String text;\n}\n",
		"application.properties": "app.greeting=hello\n",
	}
	for _, row := range []struct {
		name     string
		files    map[string]string
		composed bool
	}{
		{"seed_query", goFiles, false},
		{"composed_chain", goFiles, true},
		{"spring_outputs", springFiles, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			spring := row.files["Greeter.java"] != ""
			var f *contractJobFixture
			if spring {
				registry := parser.NewRegistry()
				languages.RegisterAll(registry)
				f = newContractJobFixtureWithRegistry(t, row.files, registry)
			} else {
				f = newContractJobFixture(t, row.files)
				seed := graph.Edge{From: "fixture/g.go::Wire", To: "fixture/f.go::Target", Kind: graph.EdgeProvides, FilePath: "fixture/g.go", Line: 2,
					Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: "SYNTH"}}
				require.NoError(t, f.store.AddBatchChecked(nil, []*graph.Edge{&seed}))
			}
			var guard *contractReadGuard
			inside, bypass := 0, 0
			count := func() {
				switch {
				case guard == nil || !guard.open:
				case guard.recorded():
					inside++
				default:
					bypass++
				}
			}
			f.adjust = func(snapshot *ContractFollowupSnapshot) {
				snapshot.Core = &contractCountingReader{Reader: snapshot.Core, count: count}
				seeds, readCore := snapshot.Seeds, snapshot.ReadCoreFile
				snapshot.Seeds = func(ctx context.Context, repo string) ([]*graph.Node, []*graph.Edge, bool, error) {
					count()
					if row.composed {
						// A composed chain has no physical seed query: the
						// round loads every live file.
						return nil, nil, false, nil
					}
					return seeds(ctx, repo)
				}
				snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
					count()
					return readCore(ctx, file)
				}
			}
			// A save after the round makes the fence re-check the round's
			// reads too.
			saved := false
			hooks := &contractConvergenceHooks{
				guard: func(g *contractReadGuard) { guard = g },
				afterPP: func() {
					if !saved && !spring {
						saved = true
						f.save("f.go", "package fixture\nfunc Target() int { return 3 }\n")
					}
				},
			}
			report, err := f.job(hooks, nil)
			require.NoError(t, err)
			require.True(t, report.Published)
			require.NotNil(t, guard)
			require.Positive(t, inside, "the enrichment round read the core")
			require.Zero(t, bypass, "every enrichment read of the core went through the ledger facade")
			if spring {
				require.Contains(t, fmt.Sprint(publishedFixtureConfigReads(t, f.options)), "app.greeting", "the round derived a configuration read")
				return
			}
			require.Contains(t, contractIDs(f.contracts()), "di::SYNTH")
		})
	}
}

// publishedFixtureConfigReads lists the published attachment's Spring
// configuration outputs: config-key nodes and reads_config edges.
func publishedFixtureConfigReads(t *testing.T, options ContractFollowupCaptureOptions) []string {
	t.Helper()
	ctx := t.Context()
	inputs, err := options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	analysis, err := options.Materializer.OpenContractAnalysisForInputs(ctx, nil, inputs)
	require.NoError(t, err)
	defer analysis.Close()
	nodes, err := analysis.NodesContext(ctx, graph.ContractProjectionRowLimit)
	require.NoError(t, err)
	var out, ids []string
	for _, node := range nodes {
		ids = append(ids, node.ID)
		if node.Kind == graph.KindConfigKey {
			out = append(out, "key:"+node.ID)
		}
	}
	edges, err := analysis.OutEdgesContext(ctx, ids, graph.ContractProjectionRowLimit)
	require.NoError(t, err)
	for from, list := range edges {
		for _, edge := range list {
			if edge.Kind == graph.EdgeReadsConfig {
				out = append(out, from+"->"+edge.To)
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestContractJobSpringConfigFollowsASecondEnrichmentRound is the Spring half
// of the fresh-evidence pin: the configuration reader's key changes, with the
// attachment key kept, right after the first enrichment round. The second
// round must bind the reader afresh: a reads_config edge the first round
// derived into its own store must not survive into the publish, which equals
// a cold build of the edited reader.
func TestContractJobSpringConfigFollowsASecondEnrichmentRound(t *testing.T) {
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	reader := func(key string) string {
		return "import org.springframework.beans.factory.annotation.Value;\npublic class Greeter {\n  @Value(\"${" + key + "}\")\n  private String text;\n}\n"
	}
	files := map[string]string{
		"routes.go": jobRoutes, "consts.go": jobConsts,
		"Greeter.java":           reader("app.greeting"),
		"application.properties": "app.greeting=hello\napp.farewell=bye\n",
	}
	final := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "Greeter.java": reader("app.farewell"), "application.properties": files["application.properties"]}
	cold := newContractJobFixtureWithRegistry(t, final, registry)
	report, err := cold.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	want := publishedFixtureConfigReads(t, cold.options)
	var reads []string
	for _, entry := range want {
		if strings.Contains(entry, "->") {
			reads = append(reads, entry)
		}
	}
	require.Len(t, reads, 1, "the cold build binds the edited reader once: %v", want)
	require.Contains(t, reads[0], "app.farewell")

	f := newContractJobFixtureWithRegistry(t, files, registry)
	edited := false
	hooks := &contractConvergenceHooks{afterPP: func() {
		if !edited {
			edited = true
			f.saveKeepingKey("Greeter.java", final["Greeter.java"])
		}
	}}
	report, err = f.job(hooks, nil)
	require.True(t, edited)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 1, report.EnrichReruns, "the reader's changed seed reran the enrichment round")
	require.Equal(t, want, publishedFixtureConfigReads(t, f.options), "the publish equals a cold build of the edited reader")
}

// contractLinkedWorktree is one linked checkout of the primary fixture
// repository: its own working tree, catalog rows and a route over sparse
// generations the production builder writes, with the production contract
// journal (receipts and input state under the checkout's actor).
type contractLinkedWorktree struct {
	f       *contractJobFixture
	root    string
	builder *SparseGenerationBuilder
	graphID string
	commit  int64
}

const contractLinkedCheckout = "linked"

func newContractLinkedWorktree(t *testing.T, f *contractJobFixture, files map[string]string, changed ...string) *contractLinkedWorktree {
	t.Helper()
	ctx := t.Context()
	w := &contractLinkedWorktree{f: f, root: t.TempDir(), graphID: GraphIDFor("fixture")}
	for name, text := range files {
		writeFile(t, filepath.Join(w.root, name), text)
	}
	w.builder = &SparseGenerationBuilder{Store: f.store, Registry: f.mi.registry, Config: f.mi.configMgr.GetRepoConfig("fixture").Index, Logger: zap.NewNop()}
	w.builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: func(context.Context, string, string) { f.announce() }})
	catalog := f.store.Catalog()
	require.NoError(t, catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{FamilyID: "family-linked", CommonDirIdentity: filepath.Join(f.root, ".git"), State: reconcile.FamilyStateReady, CreatedAt: 1, LastSeen: 1}))
	require.NoError(t, catalog.UpsertCheckout(ctx, store_sqlite.Checkout{CheckoutID: "primary", Incarnation: "inc-primary", FamilyID: "family-linked", RootPath: f.root, GitDir: filepath.Join(f.root, ".git"), AdminName: "primary",
		State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated, LastSeen: 1}))
	require.NoError(t, catalog.UpsertCheckout(ctx, store_sqlite.Checkout{CheckoutID: contractLinkedCheckout, Incarnation: "inc-linked", FamilyID: "family-linked", RootPath: w.root, GitDir: filepath.Join(f.root, ".git", "worktrees", "linked"), AdminName: "linked",
		State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeAutomatic, EffectiveMode: store_sqlite.CheckoutModeAutomatic, LastSeen: 1}))
	require.NoError(t, catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{GraphID: w.graphID, OwnerCheckoutID: "primary", RepoPrefix: "fixture", FamilyID: "family-linked", IsPrimaryBase: true, State: reconcile.GraphStateReady}))
	w.commit = w.build(t, CommitLayerGenerationKind, 0, nil, changed...)
	w.route(t, w.commit, 0)
	return w
}

// build writes one sparse generation of the worktree's changed paths over
// base (the base corpus when 0) and publishes it.
func (w *contractLinkedWorktree) build(t *testing.T, kind string, base int64, view *graphview.RepoView, changed ...string) int64 {
	t.Helper()
	target, err := source.NewFilesystemSource(w.root)
	require.NoError(t, err)
	defer func() { _ = target.Close() }()
	changes := make([]LayerPathChange, 0, len(changed))
	for _, path := range changed {
		changes = append(changes, LayerPathChange{Path: path, Kind: LayerPathModified})
	}
	var layerBase LayerBase = w.f.store.AtGeneration(0)
	if view != nil {
		layerBase = commitLayerBase{Reader: view.Reader, cloneSources: view.GenerationSources(), cloneCorpusBase: w.f.store.AtGeneration(0)}
	}
	generation, _, err := w.builder.Build(t.Context(), BuildRequest{
		Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: w.graphID, LayerID: fmt.Sprintf("linked-%s-%d", kind, base), CheckoutID: contractLinkedCheckout,
			GenerationKind: kind, BaseGenerationID: base, TreeOID: "tree-linked", ProvenanceCommitOID: "commit-linked", CreatedAt: time.Now().UnixNano()},
		Base: layerBase, Target: target, Changes: changes, RootPath: w.root, RepoPrefix: "fixture", WorkspaceID: "fixture", ProjectID: "fixture",
	})
	require.NoError(t, err)
	return generation
}

func (w *contractLinkedWorktree) route(t *testing.T, commit, dirty int64) {
	t.Helper()
	route, found, err := w.f.store.Catalog().GetCheckoutRoute(t.Context(), contractLinkedCheckout)
	require.NoError(t, err)
	next := store_sqlite.CheckoutRoute{CheckoutID: contractLinkedCheckout, GraphID: w.graphID, CommitGenerationID: commit, DirtyGenerationID: dirty, State: store_sqlite.RouteActive}
	if found {
		next.RouteEpoch = route.RouteEpoch
	}
	require.NoError(t, w.f.store.Catalog().UpsertCheckoutRoute(t.Context(), next))
}

func (w *contractLinkedWorktree) view(t *testing.T) *graphview.RepoView {
	t.Helper()
	view, err := w.f.options.Materializer.MaterializeCheckout(t.Context(), contractLinkedCheckout)
	require.NoError(t, err)
	t.Cleanup(view.Close)
	return view
}

// job captures the linked actor's current inputs through the production
// capture and runs one attachment job; contracts reads what it published.
func (w *contractLinkedWorktree) job(t *testing.T, view *graphview.RepoView, hooks *contractConvergenceHooks) (ContractFollowupReport, error) {
	t.Helper()
	ctx := t.Context()
	options := w.f.options
	inputs, err := options.Materializer.CaptureContractInputs(ctx, view, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.True(t, inputs.State.Accepted)
	snapshot, targets, err := NewContractFollowupCapture(options)(ctx, view, inputs, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	snapshot.Work = targets[0].Work
	_, payload, err := options.Store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "contract-linked", LayerID: "job-" + time.Now().Format(time.RFC3339Nano), GenerationKind: "contract_analysis", ConfigHash: "job-config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "contract-only", CreatedAt: time.Now().UnixNano()})
	require.NoError(t, err)
	req := ContractFollowupRequest{RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: options.Registry, Config: options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: options.Materializer.Leases, Logger: zap.NewNop(), Yield: options.Yield,
		PublishedEpoch: w.f.epoch.Load, WaitPublished: w.f.waitPublished, convergenceHooks: hooks}
	return RunContractFollowup(ctx, req)
}

func (w *contractLinkedWorktree) contracts(t *testing.T, view *graphview.RepoView) map[string]*graph.Node {
	t.Helper()
	ctx := t.Context()
	inputs, err := w.f.options.Materializer.CaptureContractInputs(ctx, view, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	analysis, err := w.f.options.Materializer.OpenContractAnalysisForInputs(ctx, view, inputs)
	require.NoError(t, err)
	defer analysis.Close()
	nodes, err := analysis.NodesByKindsContext(ctx, []graph.NodeKind{graph.KindContract})
	require.NoError(t, err)
	out := make(map[string]*graph.Node, len(nodes))
	for _, node := range nodes {
		out[node.ID] = node
	}
	return out
}

// TestContractJobLinkedWorktreeGenerationPublishedBetweenJobsEqualsCold is
// the end-to-end row of the chain pin: a linked worktree's actor runs job 1
// over its commit generation; a working-tree generation g2 of the same actor
// is then written (its receipts after job 1's last scan), published and
// routed; job 2 runs over the new chain. Job 2 reads g2's receipt rows, not
// job 1's, and its publish equals a cold build of the worktree's final files.
func TestContractJobLinkedWorktreeGenerationPublishedBetweenJobsEqualsCold(t *testing.T) {
	f := newRouteConstantFixture(t)
	consts := func(path string) string { return "package fixture\nconst RoutePath = \"" + path + "\"\n" }
	worktree := map[string]string{"routes.go": jobRoutes, "consts.go": consts("/linked-1"), "helper.go": jobHelper}
	w := newContractLinkedWorktree(t, f, worktree, "consts.go")

	stampOf := func(stamps map[string]ContractReceiptStamp) func(*contractUnit) {
		return func(unit *contractUnit) {
			if unit.path == "fixture/consts.go" {
				stamps["job"] = unit.stamp
			}
		}
	}
	first := make(map[string]ContractReceiptStamp)
	view1 := w.view(t)
	report, err := w.job(t, view1, &contractConvergenceHooks{proven: stampOf(first)})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, w.commit, first["job"].Generation, "job 1 reads the commit generation's row")
	require.Equal(t, contractLinkedCheckout, first["job"].Actor)
	require.Contains(t, contractIDs(w.contracts(t, view1)), "http::GET::/linked-1")

	// g2: the worktree's next state, written after job 1 ended, then routed.
	worktree["consts.go"] = consts("/linked-2")
	writeFile(t, filepath.Join(w.root, "consts.go"), worktree["consts.go"])
	dirty := w.build(t, DirtyLayerGenerationKind, w.commit, view1, "consts.go")
	w.route(t, w.commit, dirty)

	second := make(map[string]ContractReceiptStamp)
	view2 := w.view(t)
	report, err = w.job(t, view2, &contractConvergenceHooks{proven: stampOf(second)})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.NotEqual(t, first["job"], second["job"], "job 2 reads the published generation's row")
	require.Equal(t, dirty, second["job"].Generation)
	require.Equal(t, contractIDs(coldContracts(t, worktree)), contractIDs(w.contracts(t, view2)), "the publish equals a cold build of the worktree")
}

// TestContractJobDITokenChangeAfterTheEnrichmentRound: a dependency injection
// provides edge keeps its endpoints, file and line while its token changes,
// with the attachment key kept, right after the enrichment round; the edit
// that changes it changes the providing file's declarations too, as a token
// edit in a provider's source does. The token is edge Meta, so the seed
// read's recorded answer must carry it: the re-check finds the answer changed
// and the publish equals a cold build with the new token.
func TestContractJobDITokenChangeAfterTheEnrichmentRound(t *testing.T) {
	g := "package fixture\nfunc Wire() int { return 1 }\n"
	files := map[string]string{
		"routes.go": jobRoutes, "consts.go": jobConsts,
		"f.go": "package fixture\nfunc Target() int { return 2 }\n",
		"g.go": g,
	}
	seed := func(token string) *graph.Edge {
		return &graph.Edge{From: "fixture/g.go::Wire", To: "fixture/f.go::Target", Kind: graph.EdgeProvides, FilePath: "fixture/g.go", Line: 2,
			Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: token}}
	}
	edit := g + "func Token2() int { return 2 }\n"
	final := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "f.go": files["f.go"], "g.go": edit}
	cold := newContractJobFixture(t, final)
	require.NoError(t, cold.store.AddBatchChecked(nil, []*graph.Edge{seed("SYNTH2")}))
	report, err := cold.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	want := contractIDs(cold.contracts())
	require.Contains(t, want, "di::SYNTH2")

	f := newContractJobFixture(t, files)
	require.NoError(t, f.store.AddBatchChecked(nil, []*graph.Edge{seed("SYNTH")}))
	edited := false
	hooks := &contractConvergenceHooks{afterPP: func() {
		if edited {
			return
		}
		edited = true
		f.saveKeepingKey("g.go", edit)
		// The apply of g.go may already have evicted the old edge.
		f.store.RemoveEdge("fixture/g.go::Wire", "fixture/f.go::Target", graph.EdgeProvides)
		require.NoError(t, f.store.AddBatchChecked(nil, []*graph.Edge{seed("SYNTH2")}))
	}}
	report, err = f.job(hooks, nil)
	require.True(t, edited)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 1, report.EnrichReruns, "the changed token reran the enrichment round")
	require.Equal(t, want, contractIDs(f.contracts()), "the publish equals a cold build with the new token")
}

// TestContractJobSpringPropertiesEditAfterTheEnrichmentRound: the Spring
// configuration file gains the key a bean reads, with the attachment key
// kept, right after the enrichment round. The round read the file's bytes, a
// byte read whose receipt no longer names them: the round reruns and the
// publish equals a cold build of the edited file.
func TestContractJobSpringPropertiesEditAfterTheEnrichmentRound(t *testing.T) {
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	greeter := "import org.springframework.beans.factory.annotation.Value;\npublic class Greeter {\n  @Value(\"${app.farewell}\")\n  private String text;\n}\n"
	files := map[string]string{
		"routes.go": jobRoutes, "consts.go": jobConsts,
		"Greeter.java":           greeter,
		"application.properties": "app.greeting=hello\n",
	}
	final := maps.Clone(files)
	final["application.properties"] = "app.greeting=hello\napp.farewell=bye\n"
	cold := newContractJobFixtureWithRegistry(t, final, registry)
	report, err := cold.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	want := publishedFixtureConfigReads(t, cold.options)
	reads := 0
	for _, entry := range want {
		if strings.Contains(entry, "->") && strings.Contains(entry, "app.farewell") {
			reads++
		}
	}
	require.Equal(t, 1, reads, "the cold build binds the reader to the new key: %v", want)

	f := newContractJobFixtureWithRegistry(t, files, registry)
	edited := false
	hooks := &contractConvergenceHooks{afterPP: func() {
		if !edited {
			edited = true
			f.saveKeepingKey("application.properties", final["application.properties"])
		}
	}}
	report, err = f.job(hooks, nil)
	require.True(t, edited)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 1, report.EnrichReruns, "the moved byte read reran the enrichment round")
	require.Equal(t, want, publishedFixtureConfigReads(t, f.options), "the publish equals a cold build of the edited configuration")
}

// TestContractJobRenamedHandlerAfterTheEnrichmentRound: the route's handler,
// a method value the enrichment round resolves across files, is renamed with
// the key kept right after the round. Its file holds no records and no longer
// declares the name or the id the round asked for; the round's reads of that
// file are asked again, answer differently, and the publish equals a cold
// build in which the handler no longer resolves.
func TestContractJobRenamedHandlerAfterTheEnrichmentRound(t *testing.T) {
	routes := "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /items\", h.serveItems) }\n"
	before := "package fixture\nimport \"net/http\"\ntype H struct{}\nfunc (H) serveItems(w http.ResponseWriter, r *http.Request) {}\n"
	after := "package fixture\nimport \"net/http\"\ntype H struct{}\nfunc (H) serveItems2(w http.ResponseWriter, r *http.Request) {}\n"
	files := map[string]string{"routes.go": routes, "serve.go": before}
	f := newContractJobFixture(t, files)
	edited := false
	hooks := &contractConvergenceHooks{afterPP: func() {
		if !edited {
			edited = true
			f.saveKeepingKey("serve.go", after)
		}
	}}
	report, err := f.job(hooks, nil)
	require.True(t, edited)
	require.NoError(t, err)
	require.True(t, report.Published)
	final := maps.Clone(files)
	final["serve.go"] = after
	original, want := coldContracts(t, files), coldContracts(t, final)
	require.NotEqual(t, contractSymbols(original), contractSymbols(want), "the rename changes the cold build")
	require.Equal(t, contractSymbols(want), contractSymbols(f.contracts()))
}

// TestContractFenceDefersOnlyMovesAfterTheQuietScan pins the fence's deferral
// rule on a frozen census: a stamp that moved after the quiet point scan
// observed its accepted prior stamp is kept at the prior stamp, its move left
// for the next scan; a stamp that scan did not observe, one whose prior stamp
// was in its core-write window, one that left the census, and every stamp once a read
// followed the quiet scan, are observed as moved.
func TestContractFenceDefersOnlyMovesAfterTheQuietScan(t *testing.T) {
	stamp := func(fp string, seq uint64, accepted bool) ContractReceiptStamp {
		return ContractReceiptStamp{Fingerprint: fp, SourceFingerprint: fp, Accepted: accepted, Seq: seq}
	}
	census := map[string]bool{"a": true, "b": true, "c": true}
	for _, row := range []struct {
		name     string
		quiet    bool
		leaves   string
		deferred []string
	}{
		{"after_the_quiet_scan", true, "", []string{"a"}},
		{"a_read_followed", false, "", nil},
		{"census_departure", true, "a", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			var next ContractFollowupScan
			c := &contractConvergence{ctx: t.Context(), report: &ContractFollowupReport{},
				files: map[string]ContractFollowupFile{"a": {Path: "a"}, "b": {Path: "b"}, "c": {Path: "c"}}}
			c.snap.Scan = func(context.Context, []string) (ContractFollowupScan, error) { return next, nil }
			// A full scan, then the quiet point scan of a (accepted) and c
			// (in flight, its window open); b is not in it.
			writing := stamp("1", 1, false)
			writing.Writing = true
			c.clock.observe(map[string]ContractReceiptStamp{"a": stamp("1", 1, true), "b": stamp("1", 1, true), "c": writing}, census)
			quiet := c.clock.observePoint([]string{"a", "c"}, map[string]ContractReceiptStamp{"a": stamp("1", 1, true), "c": writing})
			if row.quiet {
				c.quietFrom = quiet
			}
			fenceCensus := maps.Clone(census)
			delete(fenceCensus, row.leaves)
			moved := map[string]ContractReceiptStamp{"a": stamp("2", 2, true), "b": stamp("2", 2, true), "c": stamp("2", 2, true)}
			next = ContractFollowupScan{Stamps: moved, Census: fenceCensus, Settled: true, Full: true}
			fence, clock, deferred, err := c.fence()
			require.NoError(t, err)
			require.Equal(t, row.deferred, deferred)
			for _, path := range []string{"a", "b", "c"} {
				if slices.Contains(row.deferred, path) {
					require.Equal(t, stamp("1", 1, true), fence.Stamps[path], "%s keeps its prior stamp", path)
					require.Equal(t, stamp("1", 1, true), c.clock.last[path])
					require.False(t, c.clock.changedSince(path, quiet), "%s did not move on the clock", path)
					require.Equal(t, quiet, c.clock.seenAt[path], "%s was last seen by the quiet scan", path)
					continue
				}
				require.Equal(t, clock, c.clock.changedAt[path], "%s moved at the fence", path)
			}
			// The next scan observes the deferred move.
			_, after, err := c.fullScan()
			require.NoError(t, err)
			for _, path := range row.deferred {
				require.Equal(t, after, c.clock.changedAt[path], "%s moves at the next scan", path)
			}
		})
	}
}

// TestContractJobFenceDefersAMoveAfterItsLastRead pins the deferral end to
// end. In the after_the_last_read row the route constant's owner saves (with
// the key kept) right before the second iteration's fence, after the point
// round that proved it observed it: the save came after every read, so the job
// publishes in that iteration the state that round saw, and the next job
// publishes the save. In the during_the_enrichment_round row a route file
// saves while the second iteration's enrichment round reads: it is observed
// as a move, and the job publishes the save itself.
func TestContractJobFenceDefersAMoveAfterItsLastRead(t *testing.T) {
	helper := func(path, value string) string {
		return "package fixture\nfunc registerHelper(r Router) { r.GET(\"" + path + "\", helped) }\nfunc helped() {}\nfunc other() int { return " + value + " }\n"
	}
	// The value is a status a function writes: its receipt digest changes,
	// and with it the file's contract content, while every row the route
	// reads holds.
	consts := func(path, value string) string {
		return "package fixture\nimport \"net/http\"\nconst RoutePath = \"" + path + "\"\nfunc tick(w http.ResponseWriter) { w.WriteHeader(20" + value + ") }\n"
	}
	routesMore := "package fixture\nfunc register(r Router) { r.GET(RoutePath, users); r.POST(\"/more\", users) }\nfunc users() {}\n"
	// The settled row's first save is never accepted: the stamp the quiet
	// scan observes is settled, and the save after it is deferred all the same.
	// Both after_the_last_read rows end on the same files.
	oracles := contractColdOracles{}
	for _, name := range []string{"after_the_last_read", "after_the_last_read_settled", "during_the_enrichment_round"} {
		row, settled := name, name == "after_the_last_read_settled"
		if settled {
			row = "after_the_last_read"
		}
		t.Run(name, func(t *testing.T) {
			files := map[string]string{"routes.go": jobRoutes, "consts.go": consts("/before", "1"), "helper.go": helper("/helper-1", "1")}
			f := newContractJobFixture(t, files)
			deferred := make(map[int][]string)
			stage := 0
			hooks := &contractConvergenceHooks{
				fenced: func(iteration int, paths []string) { deferred[iteration] = paths },
				afterPP: func() {
					if stage != 0 {
						return
					}
					stage = 1
					if row == "after_the_last_read" {
						// A save that changes the owner's declarations but not
						// the route's constant: the next iteration revalidates
						// the route and re-checks the enrichment round, no
						// rerun.
						if settled {
							f.saveHeldKeepingKey("consts.go", consts("/before", "2"))
							return
						}
						f.saveKeepingKey("consts.go", consts("/before", "2"))
						return
					}
					// A records change: the next iteration reruns it.
					f.saveKeepingKey("routes.go", routesMore)
				},
				beforeFence: func(iteration int) {
					if row == "after_the_last_read" && iteration == 1 && stage == 1 {
						stage = 2
						f.saveKeepingKey("consts.go", consts("/after", "2"))
					}
				},
				proven: func(unit *contractUnit) {
					if row == "during_the_enrichment_round" && stage == 1 && unit.path == "fixture/routes.go" {
						stage = 2
					}
				},
				openedEvidence: func(*store_sqlite.Store) {
					// The first evidence opened after the route's second proof
					// is the enrichment round's.
					if row == "during_the_enrichment_round" && stage == 2 {
						stage = 3
						f.saveKeepingKey("helper.go", helper("/helper-2", "1"))
					}
				},
			}
			report, err := f.job(hooks, nil)
			require.NoError(t, err)
			require.True(t, report.Published)
			got := contractIDs(f.contracts())
			if row == "after_the_last_read" {
				require.Equal(t, 2, stage)
				require.Equal(t, 2, report.Iterations, "the deferred save does not cost an iteration")
				require.Equal(t, []string{"fixture/consts.go"}, deferred[1])
				require.Zero(t, report.EnrichReruns)
				seen := maps.Clone(files)
				seen["consts.go"] = consts("/before", "2")
				require.Equal(t, contractIDs(oracles.contracts(t, seen)), got, "the publish is the state the last point round saw")
				require.Contains(t, got, "http::GET::/before")
				// The deferred save kept the key, so the publish stays current
				// for it; the next save that moves the key starts the next job,
				// which publishes both.
				routes := routesMore
				f.save("routes.go", routes)
				report, err = f.job(nil, nil)
				require.NoError(t, err)
				require.True(t, report.Published)
				final := maps.Clone(files)
				final["consts.go"], final["routes.go"] = consts("/after", "2"), routes
				after := contractIDs(f.contracts())
				require.Contains(t, after, "http::GET::/after")
				require.Equal(t, contractIDs(oracles.contracts(t, final)), after)
				return
			}
			require.Equal(t, 3, stage)
			require.Empty(t, deferred[1], "a move read past the last scan is not deferred")
			final := map[string]string{"routes.go": routesMore, "consts.go": files["consts.go"], "helper.go": helper("/helper-2", "1")}
			require.Contains(t, got, "http::GET::/helper-2")
			require.Equal(t, contractIDs(coldContracts(t, final)), got, "the publish is the save's state")
		})
	}
}

// TestContractJobHasNoFixedIterationBound: a job keeps converging while it
// makes progress, however many iterations that takes and however long they
// run against its first. A writer alternating two files right before every
// fence changes each one's declarations (the attachment key kept), so each
// iteration re-extracts the file it saved and none publishes while it
// writes; each iteration is slowed so the writer's run outlasts the first
// iteration many times over. The job publishes after the writer stops,
// equal to a cold build of the last saves.
func TestContractJobHasNoFixedIterationBound(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper, "helper2.go": "package fixture\nfunc Helper2() int { return 1 }\n"}
	f := newContractJobFixture(t, files)
	helper := func(name string, n int) string {
		return fmt.Sprintf("package fixture\nfunc %s%d() int { return 1 }\n", name, n)
	}
	const writes = 12
	saves := 0
	final := maps.Clone(files)
	var start time.Time
	// Each later iteration is slowed by the first iteration's own length
	// (the first fence ends it), capped at 300ms: the writer's run outlasts
	// the first iteration more than writes times over on any host, without
	// a fixed sleep longer than the iteration it stands for.
	var first, pause time.Duration
	hooks := &contractConvergenceHooks{
		beforeFence: func(int) {
			if saves >= writes {
				return
			}
			if saves == 0 {
				first = time.Since(start)
				pause = min(first, 300*time.Millisecond)
			}
			saves++
			time.Sleep(pause)
			if saves%2 == 1 {
				final["helper.go"] = helper("Helper", saves)
				f.save("helper.go", final["helper.go"])
			} else {
				final["helper2.go"] = helper("Helper2x", saves)
				f.save("helper2.go", final["helper2.go"])
			}
		},
	}
	start = time.Now()
	report, err := f.job(hooks, nil)
	elapsed := time.Since(start)
	require.NoError(t, err, "a job making progress is never superseded")
	require.True(t, report.Published)
	require.Equal(t, writes, saves)
	require.Equal(t, writes+1, report.Iterations, "every save before a fence cost one iteration")
	t.Logf("iterations=%d first=%s pause=%s elapsed=%s", report.Iterations, first, pause, elapsed)
	require.Greater(t, elapsed, first+writes*pause, "the writer's run outlasted the first iteration many times over")
	require.Equal(t, contractIDs(coldContracts(t, final)), contractIDs(f.contracts()))
}

// TestContractJobWithoutProgressIsSuperseded: the file declaring a route's
// handler, which only the enrichment round resolves, is held between its
// evict and its add while the round reads, and never released. The round read
// it torn, so it is current only once those reads are asked again with the
// file's core-write window closed, which never happens. Each re-check is
// blocked by that window and no event comes: after contractIdleRounds rounds
// that each waited out, the re-check ends the job superseded with unsettled
// reads, before a second fence, instead of iterating on.
func TestContractJobWithoutProgressIsSuperseded(t *testing.T) {
	routes := "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /items\", h.serveItems) }\n"
	serve := "package fixture\nimport \"net/http\"\ntype H struct{}\nfunc (H) serveItems(w http.ResponseWriter, r *http.Request) {}\n"
	f := newContractJobFixture(t, map[string]string{"routes.go": routes, "serve.go": serve})
	held, proven, fences := false, 0, 0
	hooks := &contractConvergenceHooks{
		roundWait: 300 * time.Millisecond,
		proven:    func(*contractUnit) { proven++ },
		// The first evidence opened after both units are proven is the
		// enrichment round's.
		openedEvidence: func(*store_sqlite.Store) {
			if proven == 2 && !held {
				held = true
				f.beginHeld("fixture/serve.go")
			}
		},
		beforeFence: func(int) { fences++ },
	}
	// A job iterating on without progress, or asking again without waiting
	// for the window, would spin; bound both.
	spin := contractJobDeadline(time.Minute)
	yield := func(ctx context.Context) error {
		if fences > 5 {
			return fmt.Errorf("the job iterated without progress")
		}
		return spin(ctx)
	}
	report, err := f.job(hooks, yield)
	require.True(t, held)
	require.False(t, report.Published)
	require.Equal(t, viewmetrics.ContractJobSupersededReadUnsettled, contractJobOutcomeOf(err), "job error: %v", err)
	require.Equal(t, 1, fences, "the re-check after the enrichment round ended the job")
}

// ownerConsts declares the route constant and then a function, so a comment
// appended to the file belongs to no declaration: such a save keeps the
// file's contract content and the attachment key.
const ownerConsts = "package fixture\nconst RoutePath = \"/before\"\n\nfunc tick() int {\n\treturn 1\n}\n"

// newOwnerReadersFixture is one constant owner and ten route files whose
// route path is that constant: ten units read the owner's rows.
func newOwnerReadersFixture(t *testing.T) (*contractJobFixture, map[string]string) {
	t.Helper()
	files := map[string]string{"consts.go": ownerConsts}
	for i := range 10 {
		files[fmt.Sprintf("r%d.go", i)] = fmt.Sprintf("package fixture\nfunc register%d(r Router) { r.GET(RoutePath, h%d) }\nfunc h%d() {}\n", i, i, i)
	}
	return newContractJobFixture(t, files), files
}

// TestContractJobPublishesWhileAnOwnerIsNeverAccepted pins that a moved file
// whose contract content is unchanged never blocks publication: the owner ten
// route units and the enrichment round read is rewritten
// body-only whenever the job waits, each save completing its core writes and
// leaving its receipt in flight, never accepted and never settled, with no
// Published event. The first save lands while the routes' first round reads,
// so all ten are suspects torn by it, and the owner's own unit is extracted
// in flight. For the first ten retry rounds a further save lands between a
// round's asks and its closing scan: answers that held while the owner moved
// prove nothing, and those rounds are blocked, never counted. The rounds
// after prove every unit, and the
// enrichment round's reads of the owner, by asking them again once the
// latest save's core-write window closed: the job publishes with no
// supersession and no enrichment rerun, equal to a cold build, without the
// owner ever being accepted or settled.
func TestContractJobPublishesWhileAnOwnerIsNeverAccepted(t *testing.T) {
	f, files := newOwnerReadersFixture(t)
	saves := 0
	write := func() {
		saves++
		files["consts.go"] = ownerConsts + fmt.Sprintf("// tick %d\n", saves)
		f.saveInFlight("consts.go", files["consts.go"])
	}
	const rounds = 10
	retries, waits, wakes := 0, 0, 0
	// Every wait of the job is a writer period: the round after it opens
	// under a later apply of the owner.
	f.onWait = func() {
		if waits < 10*rounds {
			waits++
			write()
		}
	}
	hooks := &contractConvergenceHooks{
		afterExtract: func(path string) {
			if saves == 0 && path == "fixture/r0.go" {
				write()
			}
		},
		woke: func() { wakes++ },
		beforeRevalidateR1: func() {
			if retries < rounds {
				retries++
				write()
			}
		},
	}
	report, err := f.job(hooks, nil)
	f.onWait = nil
	require.NoError(t, err, "a file that is never accepted does not supersede the job")
	require.True(t, report.Published)
	require.Equal(t, rounds, retries)
	require.GreaterOrEqual(t, wakes, rounds, "receipt writes woke every retry round")
	require.Less(t, waits, 10*rounds, "the job published while the writer still wrote")
	require.Greater(t, saves, rounds)
	require.Zero(t, report.EnrichReruns)
	row, _, err := f.store.ContractBoundaryReceiptContext(context.Background(), "fixture", "", "fixture/consts.go")
	require.NoError(t, err)
	require.False(t, row.Accepted, "the owner's receipt was never accepted again")
	stamps, err := newFixtureScanner(t, f).scan(t.Context(), []string{"fixture/consts.go"})
	require.NoError(t, err)
	owner := stamps.Stamps["fixture/consts.go"]
	require.False(t, owner.Accepted || owner.Settled, "the owner was still in flight at the publish")
	want := coldContracts(t, files)
	require.Len(t, contractIDs(want), 1)
	require.Equal(t, contractMeta(want), contractMeta(f.contracts()))
}

// TestContractJobFollowsHeldBoundaryChanges pins that a moved file whose
// contract content changed is re-extracted, never published stale: the same
// owner changes the routes' path twice, each time right after the enrichment
// round and with its accept held. Each change is re-extracted from its
// settled receipt and reruns the enrichment round once; the job publishes
// once, the last change's routes, never a stale one.
func TestContractJobFollowsHeldBoundaryChanges(t *testing.T) {
	f, files := newOwnerReadersFixture(t)
	changes := 0
	hooks := &contractConvergenceHooks{afterPP: func() {
		if changes < 2 {
			changes++
			files["consts.go"] = fmt.Sprintf("package fixture\nconst RoutePath = \"/v%d\"\n", changes)
			f.saveHeldKeepingKey("consts.go", files["consts.go"])
		}
	}}
	report, err := f.job(hooks, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 2, changes)
	require.Equal(t, 2, report.EnrichReruns, "one rerun per change")
	got := contractIDs(f.contracts())
	require.Equal(t, contractIDs(coldContracts(t, files)), got)
	require.Contains(t, got, "http::GET::/v2")
}

// TestContractJobProvesAnOvertakenSave: a save of the route constant's owner
// is overtaken while it writes (a newer mutation of the same owner is
// admitted), so it completes every core write and is never accepted, and no
// event follows. The mutation settles its receipt, and the job proves the
// owner's reads against the settled stamp and publishes.
func TestContractJobProvesAnOvertakenSave(t *testing.T) {
	f := newContractJobFixture(t, map[string]string{"routes.go": jobRoutes, "consts.go": ownerConsts, "helper.go": jobHelper})
	f.mi.mu.RLock()
	idx := f.mi.indexers["fixture"]
	f.mi.mu.RUnlock()
	require.NotNil(t, idx)
	authority, target := idx.outputGenerationAuthority(), idx.legacyOutputTarget()
	newer := make(chan *OutputMutationReceipt, 1)
	overtaken := false
	restore := store_sqlite.SetContractReceiptWriteHookForTest(func(stage string, paths []string) {
		if overtaken || stage != "ending" || !slices.Contains(paths, "fixture/consts.go") {
			return
		}
		overtaken = true
		issued := authority.Stats().Issued
		go func() {
			receipt, err := authority.Begin(context.Background(), OutputEntryIndexFile, target)
			if err != nil {
				receipt = nil
			}
			newer <- receipt
		}()
		// The newer receipt is admitted, and the running one superseded,
		// before its source gate blocks.
		for deadline := time.Now().Add(10 * time.Second); authority.Stats().Issued == issued && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
	})
	consts := ownerConsts + "// overtaken\n"
	path := filepath.Join(f.root, "consts.go")
	writeFile(t, path, consts)
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, future, future))
	_, saveErr := f.mi.IncrementalReindexRepo("fixture", []string{path})
	restore()
	require.True(t, overtaken)
	require.ErrorIs(t, saveErr, ErrOutputMutationReceiptSuperseded)
	if receipt := <-newer; receipt != nil {
		receipt.Abandon()
	}
	row, _, err := f.store.ContractBoundaryReceiptContext(context.Background(), "fixture", "", "fixture/consts.go")
	require.NoError(t, err)
	require.False(t, row.Accepted, "an overtaken save is never accepted")
	stamps, err := newFixtureScanner(t, f).scan(t.Context(), []string{"fixture/consts.go"})
	require.NoError(t, err)
	require.True(t, stamps.Stamps["fixture/consts.go"].Settled, "the overtaken save settled its receipt")
	report, err := f.job(nil, nil)
	require.NoError(t, err, "the settled owner's reads are proven")
	require.True(t, report.Published)
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": consts, "helper.go": jobHelper})), contractIDs(f.contracts()))
}

// TestContractJobDITargetRenamedAfterTheEnrichmentRound: a dependency
// injection provides edge g -> f::Target; right after the enrichment round f
// renames Target with the key kept. The apply of f evicts the edge, which g
// owns, without rewriting g's receipt, and f carries no seed rows of its own:
// only the seed read's dependence on its edges' endpoints reruns the round.
// The publish equals a cold build over the live core's edges.
func TestContractJobDITargetRenamedAfterTheEnrichmentRound(t *testing.T) {
	g := "package fixture\nfunc Wire() int { return 1 }\n"
	files := map[string]string{
		"routes.go": jobRoutes, "consts.go": jobConsts,
		"f.go": "package fixture\nfunc Target() int { return 2 }\n",
		"g.go": g,
	}
	seed := graph.Edge{From: "fixture/g.go::Wire", To: "fixture/f.go::Target", Kind: graph.EdgeProvides, FilePath: "fixture/g.go", Line: 2,
		Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: "SYNTH"}}
	f := newContractJobFixture(t, files)
	require.NoError(t, f.store.AddBatchChecked(nil, []*graph.Edge{&seed}))
	require.Contains(t, contractIDs(coldContractsWithEdges(t, files, []*graph.Edge{&seed})), "di::SYNTH")
	renamed := "package fixture\nfunc Target2() int { return 2 }\n"
	edited := false
	hooks := &contractConvergenceHooks{afterPP: func() {
		if !edited {
			edited = true
			f.saveKeepingKey("f.go", renamed)
		}
	}}
	report, err := f.job(hooks, nil)
	require.True(t, edited)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 1, report.EnrichReruns, "the torn seed edge reran the enrichment round")
	var live []*graph.Edge
	for _, list := range f.store.GetOutEdgesByNodeIDs([]string{"fixture/g.go::Wire"}) {
		for _, edge := range list {
			if edge.Kind == graph.EdgeProvides {
				live = append(live, edge)
			}
		}
	}
	final := maps.Clone(files)
	final["f.go"] = renamed
	want := coldContractsWithEdges(t, final, live)
	require.NotContains(t, contractIDs(want), "di::SYNTH", "the rename leaves no provides edge in the live core")
	require.Equal(t, contractMeta(want), contractMeta(f.contracts()), "the publish equals a cold build over the live core")
}

// TestContractJobRecheckIsCurrentAtItsOpeningScan: a revalidation pass that
// re-checks the enrichment round together with a suspect's revalidation
// scans every path either plan brackets. The route's unit reads the route
// constant of a file that also declares the handler the enrichment round
// resolves; that file moves before the round, so only the suspect route
// brackets it in the next pass, and it moves again between that pass's
// opening and closing scans. The round is current at the opening scan's
// clock only, so the second move is still after it, is re-checked, and reruns
// the round: the publish equals a cold build of the file's last state.
func TestContractJobRecheckIsCurrentAtItsOpeningScan(t *testing.T) {
	routes := "package fixture\nfunc register(r Router) { r.GET(RoutePath, h.users) }\n"
	types := func(method string) string {
		return "package fixture\nconst RoutePath = \"/before\"\ntype H struct{}\nfunc (H) " + method + "() {}\n"
	}
	files := map[string]string{"routes.go": routes, "types.go": types("users"), "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	stage, proven := 0, 0
	hooks := &contractConvergenceHooks{
		// Once every first unit is proven, before the enrichment round's
		// opening scan, the handler is renamed away: the round reads the new
		// state, and the route's unit, which read the old file, goes stale.
		proven: func(*contractUnit) {
			proven++
			if stage == 0 && proven == len(files) {
				stage = 1
				files["types.go"] = types("usersA")
				f.saveKeepingKey("types.go", files["types.go"])
			}
		},
		// A move after the round that has it re-checked, not rerun.
		afterPP: func() {
			if stage == 1 {
				stage = 2
				files["helper.go"] = "package fixture\nfunc Helper2() int { return 1 }\n"
				f.saveKeepingKey("helper.go", files["helper.go"])
			}
		},
		beforeRevalidateR1: func() {
			if stage == 2 {
				stage = 3
				files["types.go"] = types("users")
				f.saveKeepingKey("types.go", files["types.go"])
			}
		},
	}
	report, err := f.job(hooks, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 3, stage, "the file moved between a re-checking pass's scans")
	want := coldContracts(t, files)
	require.Contains(t, fmt.Sprint(contractSymbols(want)), "users", "the cold build resolves the handler")
	require.Equal(t, contractSymbols(want), contractSymbols(f.contracts()))
}

// TestContractJobRevalidationAsksAMoveItsOpeningScanFound: a revalidation
// pass plans each unit's reads from what moved, then takes its opening scan.
// The route reads two constant owners; a comment saved into the second while
// the enrichment round reads makes the round's re-check bracket it, and a
// change of the first makes the route a suspect. The second owner's path
// changes after the pass planned the route's reads, before its opening scan,
// which sees the move: the pass takes its moves again after that scan, so the
// route's read of the second owner is asked again, and the publish carries
// its new path.
func TestContractJobRevalidationAsksAMoveItsOpeningScanFound(t *testing.T) {
	routes := "package fixture\nfunc register(r Router) { r.GET(RoutePath, users); r.POST(OtherPath, users) }\nfunc users() {}\n"
	consts := func(status string) string {
		return "package fixture\nimport \"net/http\"\nconst RoutePath = \"/before\"\nfunc tick(w http.ResponseWriter) { w.WriteHeader(" + status + ") }\n"
	}
	other := func(path, tail string) string {
		return "package fixture\nconst OtherPath = \"" + path + "\"\n\nfunc tock() int {\n\treturn 1\n}\n" + tail
	}
	files := map[string]string{"routes.go": routes, "consts.go": consts("201"), "other.go": other("/o1", "")}
	f := newContractJobFixture(t, files)
	stage, proven := 0, 0
	hooks := &contractConvergenceHooks{
		proven: func(*contractUnit) { proven++ },
		openedEvidence: func(*store_sqlite.Store) {
			// The first evidence opened after every first unit is proven is
			// the enrichment round's.
			if stage == 0 && proven == len(files) {
				stage = 1
				files["other.go"] = other("/o1", "// read torn\n")
				f.saveKeepingKey("other.go", files["other.go"])
			}
		},
		afterPP: func() {
			if stage == 1 {
				stage = 2
				files["consts.go"] = consts("202")
				f.saveKeepingKey("consts.go", files["consts.go"])
			}
		},
		fenced: func(iteration int, _ []string) {
			if iteration == 0 && stage == 2 {
				stage = 3
			}
		},
	}
	yield := func(ctx context.Context) error {
		// The first yield after the first fence is the suspect's
		// revalidation pass, after it planned its reads.
		if stage == 3 {
			stage = 4
			files["other.go"] = other("/o2", "")
			f.saveKeepingKey("other.go", files["other.go"])
		}
		return ctx.Err()
	}
	report, err := f.job(hooks, yield)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 4, stage)
	got := contractIDs(f.contracts())
	require.Contains(t, got, "http::POST::/o2")
	require.Equal(t, contractIDs(coldContracts(t, files)), got)
}

// coldContractsWithEdges is coldContracts over a core that also holds edges.
func coldContractsWithEdges(t *testing.T, files map[string]string, edges []*graph.Edge) map[string]*graph.Node {
	t.Helper()
	cold := newContractJobFixture(t, files)
	for _, edge := range edges {
		copyEdge := *edge
		require.NoError(t, cold.store.AddBatchChecked(nil, []*graph.Edge{&copyEdge}))
	}
	report, err := cold.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	return cold.contracts()
}

// TestContractJobOwnerInFlightAcrossItsReadsIsNotProvenInRound: the route
// constant's owner is in flight across the route's whole first round, at the
// same stamp at its R0 and its R1 (its receipt begun, its core already
// holding the edit, its accept pending until after that R1). Two scans of an
// owner in flight that held still prove nothing: the route's first unit is a
// suspect, proven once, by asking its reads again after the accept, and the
// publish equals a cold build of the edit.
func TestContractJobOwnerInFlightAcrossItsReadsIsNotProvenInRound(t *testing.T) {
	const consts = "package fixture\nconst RoutePath = \"/after\"\n"
	f := newRouteConstantFixture(t)
	ctx := context.Background()
	var staged graph.ContractBoundaryReceipt
	f.adjust = func(*ContractFollowupSnapshot) {
		f.saveKeepingKey("consts.go", consts)
		current, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/consts.go")
		require.NoError(t, err)
		staged = *current
		staged.Accepted = false
		require.NoError(t, f.store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{staged}))
	}
	released, woke := false, false
	routeProofs, inRound := 0, 0
	hooks := &contractConvergenceHooks{
		// Proofs are accepted after the round's R1: the first one releases
		// the owner.
		proven: func(unit *contractUnit) {
			if unit.path == "fixture/routes.go" {
				routeProofs++
				if !woke {
					inRound++
				}
			}
			if !released {
				released = true
				require.NoError(t, f.store.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{staged}))
				f.announce()
			}
		},
		woke: func() { woke = true },
	}
	report, err := f.job(hooks, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.True(t, released)
	require.Zero(t, inRound, "the route's first round did not prove it against an owner in flight")
	require.Equal(t, 1, routeProofs, "the route was proven once, by asking its reads again")
	want := coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": consts, "helper.go": jobHelper})
	require.Contains(t, contractIDs(want), "http::GET::/after")
	require.Equal(t, contractIDs(want), contractIDs(f.contracts()))
}

// TestContractJobOwnerInFlightWithNewContentWaitsForItsApply: after the
// first iteration the route constant's owner renames the constant, and its
// receipt is begun with the new content while its core still holds the old
// rows (an apply between its begin and its core writes). The route's reads of
// the owner are asked again only once the apply's rows are the receipt's: a
// read of the old rows would answer as before and prove a lookup the new
// content no longer declares, which no later check of the owner would find.
// The publish equals a cold build of the rename.
func TestContractJobOwnerInFlightWithNewContentWaitsForItsApply(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	renamed := "package fixture\nconst OtherPath = \"/before\"\n"
	final := maps.Clone(files)
	final["consts.go"] = renamed
	// The receipt the rename's apply begins with, as a cold build of it stores.
	other := newContractJobFixture(t, final)
	row, _, err := other.store.ContractBoundaryReceiptContext(context.Background(), "fixture", "", "fixture/consts.go")
	require.NoError(t, err)
	require.NotNil(t, row)
	begun := *row
	begun.Accepted = false
	f := newContractJobFixture(t, files)
	staged, released, waits := false, false, 0
	// The apply completes at the job's second wait: a retry round runs
	// between them with the owner in flight and its new declarations known.
	f.onWait = func() {
		if staged && !released {
			if waits++; waits == 2 {
				released = true
				f.saveKeepingKey("consts.go", renamed)
			}
		}
	}
	hooks := &contractConvergenceHooks{roundWait: 300 * time.Millisecond, afterPP: func() {
		if staged {
			return
		}
		staged = true
		writeFile(t, filepath.Join(f.root, "consts.go"), renamed)
		require.NoError(t, f.store.SetContractBoundaryReceiptsContext(context.Background(), []graph.ContractBoundaryReceipt{begun}))
	}}
	report, err := f.job(hooks, nil)
	f.onWait = nil
	require.NoError(t, err)
	require.True(t, report.Published)
	require.True(t, released, "the job waited for the apply")
	require.Equal(t, contractIDs(coldContracts(t, final)), contractIDs(f.contracts()))
}

// TestContractJobEnrichmentIgnoresAnUnreadFileInFlight: a file the
// enrichment round never reads is saved while the round reads, and a further
// apply of it stays in its core-write window for the rest of the job. Only
// files the round read can tear it: the job publishes in one iteration with
// no re-check and no rerun, equal to a cold build.
func TestContractJobEnrichmentIgnoresAnUnreadFileInFlight(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	proven, saved := 0, false
	hooks := &contractConvergenceHooks{
		roundWait: 300 * time.Millisecond,
		proven:    func(*contractUnit) { proven++ },
		// The first evidence opened after every unit is proven is the
		// enrichment round's, after its R0.
		openedEvidence: func(*store_sqlite.Store) {
			if proven == len(files) && !saved {
				saved = true
				files["helper.go"] = jobHelper + "// in flight\n"
				f.saveHeld("helper.go", files["helper.go"])
				f.stageInFlight("fixture/helper.go")
			}
		},
	}
	report, err := f.job(hooks, nil)
	require.True(t, saved)
	require.NoError(t, err, "an unread file in flight never holds the enrichment round")
	require.True(t, report.Published)
	require.Equal(t, 1, report.Iterations)
	require.Zero(t, report.EnrichRechecks)
	require.Zero(t, report.EnrichReruns)
	stamps, err := newFixtureScanner(t, f).scan(t.Context(), []string{"fixture/helper.go"})
	require.NoError(t, err)
	require.True(t, stamps.Stamps["fixture/helper.go"].Writing, "the unread file's window was open at the publish")
	require.Equal(t, contractIDs(coldContracts(t, files)), contractIDs(f.contracts()))
}

// TestContractStaleUnitsLeavesEveryUnpendingFileAtItsStamp is the publish
// condition's proof: after staleUnits, every live file is pending or carries
// a unit at its very stamp at the fence, in flight or not. A move that kept
// the file's contract content carries the unit to the new stamp; a move of
// its identity or content, an owner's content move and a missing unit make
// it pending. So nothing pending implies every live file is proven at its
// fence stamp, which the job therefore does not check again.
func TestContractStaleUnitsLeavesEveryUnpendingFileAtItsStamp(t *testing.T) {
	stamp := func(fp string, seq uint64, accepted bool) ContractReceiptStamp {
		return ContractReceiptStamp{Fingerprint: fp, SourceFingerprint: "src-" + fp, Accepted: accepted, Seq: seq}
	}
	const round = 5
	type row struct {
		unit    bool
		at      ContractReceiptStamp
		fence   ContractReceiptStamp
		content uint64
		owner   string
		pending bool
	}
	settled := stamp("1", 2, false)
	settled.Settled = true
	rows := map[string]row{
		"same":           {true, stamp("1", 1, true), stamp("1", 1, true), 1, "", false},
		"in_flight_kept": {true, stamp("1", 1, true), stamp("1", 2, false), 1, "", false},
		"new_bytes_kept": {true, stamp("1", 1, true), stamp("2", 3, false), 1, "", false},
		"settled_kept":   {true, stamp("1", 2, false), settled, 1, "", false},
		"identity_moved": {true, stamp("1", 1, true), stamp("2", 3, true), round + 1, "", true},
		"content_moved":  {true, stamp("1", 1, true), stamp("1", 2, false), round + 1, "", true},
		"owner_moved":    {true, stamp("1", 1, true), stamp("1", 1, true), 1, "identity_moved", true},
		"no_unit":        {false, ContractReceiptStamp{}, stamp("1", 1, true), 1, "", true},
	}
	c := &contractConvergence{ctx: t.Context(), report: &ContractFollowupReport{}, units: make(map[string]*contractUnit),
		suspect: make(map[string]bool), live: make(map[string]bool), excluded: make(map[string]bool), contentAt: make(map[string]uint64)}
	c.clock.last = make(map[string]ContractReceiptStamp)
	c.clock.changedAt = make(map[string]uint64)
	for path, r := range rows {
		c.live[path] = true
		c.clock.last[path] = r.fence
		c.contentAt[path] = r.content
		if !r.unit {
			continue
		}
		owners := map[string]bool{}
		if r.owner != "" {
			owners[r.owner] = true
		}
		c.setUnit(&contractUnit{path: path, stamp: r.at, round: round, owners: owners, keys: map[string]bool{},
			declared: []string{"path:" + path}, ledger: newContractUnitLedger()})
	}
	pending := make(map[string]string)
	c.staleUnits(pending)
	for path, r := range rows {
		_, isPending := pending[path]
		require.Equal(t, r.pending, isPending, "%s pending", path)
		if isPending {
			continue
		}
		unit := c.units[path]
		require.NotNil(t, unit, "%s carries a unit", path)
		require.Equal(t, c.clock.last[path], unit.stamp, "%s's unit is at its fence stamp", path)
	}
}

// TestContractEvidenceDoorsReadTheCoreInsideTheGuard is the facade guard at
// each door: with an enrichment round's guard open, every door the ledger
// facade answers from the selected core (a file load, ids, names, constants,
// binding types, the seed query, output endpoints, and the re-asks of names
// and ids) reads the core only inside a recorded read. A door whose bracket
// is dropped reads the core with the guard open and no read in progress, and
// the instrumented reader counts it.
func TestContractEvidenceDoorsReadTheCoreInsideTheGuard(t *testing.T) {
	f := newRouteConstantFixture(t)
	ctx := t.Context()
	inputs, err := f.options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	snapshot, _, err := NewContractFollowupCapture(f.options)(ctx, nil, inputs, "fixture", "")
	require.NoError(t, err)
	if snapshot.Release != nil {
		t.Cleanup(snapshot.Release)
	}
	var guard *contractReadGuard
	inside, bypass := 0, 0
	count := func() {
		switch {
		case guard == nil || !guard.open:
		case guard.recorded():
			inside++
		default:
			bypass++
		}
	}
	snapshot.Core = &contractCountingReader{Reader: snapshot.Core, count: count}
	seeds, readCore := snapshot.Seeds, snapshot.ReadCoreFile
	require.NotNil(t, seeds)
	snapshot.Seeds = func(ctx context.Context, repo string) ([]*graph.Node, []*graph.Edge, bool, error) {
		count()
		return seeds(ctx, repo)
	}
	snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		count()
		return readCore(ctx, file)
	}
	files := make(map[string]ContractFollowupFile, len(snapshot.Files))
	for _, file := range snapshot.Files {
		files[file.Path] = file
	}
	req := ContractFollowupRequest{Snapshot: snapshot, Registry: f.options.Registry, Config: f.options.Config, Logger: zap.NewNop(), Yield: f.options.Yield}
	c := newContractConvergence(ctx, req, files, map[string]bool{"fixture": true}, &ContractFollowupReport{})
	guard = c.guard
	_, _, err = c.fullScan()
	require.NoError(t, err)
	const constant = "fixture/consts.go::RoutePath"
	sites := []graph.SemanticBindingSite{{RepoPrefix: "fixture", FilePath: "fixture/routes.go", Line: 2, Name: "r"}}
	doors := []struct {
		name string
		read func(*contractFollowupEvidence) error
	}{
		{"file", func(e *contractFollowupEvidence) error { _, _, err := e.loadFile("fixture/consts.go"); return err }},
		{"ids", func(e *contractFollowupEvidence) error { e.GetNodesByIDs([]string{constant}); return e.err }},
		{"names", func(e *contractFollowupEvidence) error { e.FindNodesByNames([]string{"RoutePath"}); return e.err }},
		{"constants", func(e *contractFollowupEvidence) error {
			_, err := e.ConstantValuesByNodeIDs([]string{constant})
			return err
		}},
		{"semantic", func(e *contractFollowupEvidence) error { _, err := e.semanticTypes(sites); return err }},
		{"seeds", func(e *contractFollowupEvidence) error { _, _, _, err := c.seedRows(e, "fixture"); return err }},
		{"endpoints", func(e *contractFollowupEvidence) error { _, err := c.endpointRows(e, []string{constant}); return err }},
		{"ask_names", func(e *contractFollowupEvidence) error { _, err := c.askNames(e, []string{"RoutePath"}); return err }},
		{"ask_ids", func(e *contractFollowupEvidence) error { _, err := c.askIDs(e, []string{constant}); return err }},
	}
	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			evidence, scratch, dir, err := c.openEvidence()
			require.NoError(t, err)
			defer closeContractEvidence(scratch, dir)
			evidence.ledger = newContractUnitLedger()
			guard.open = true
			before, bypassed := inside, bypass
			readErr := door.read(evidence)
			guard.open = false
			require.NoError(t, readErr)
			require.Greater(t, inside, before, "the door read the core")
			require.Equal(t, bypassed, bypass, "the door read the core only inside a recorded read")
		})
	}
}

// TestContractMutationSettlesOnlyACompletedOvertakenBody: a deletion pass
// begins its first file's receipt and then fails, its second begin refused.
// The body returned an error, so the begun receipt is left unaccepted and
// unsettled: a failed body never vouches that its file's rows are complete.
// Only a body that completed and that Complete found overtaken settles
// (TestContractJobProvesAnOvertakenSave).
func TestContractMutationSettlesOnlyACompletedOvertakenBody(t *testing.T) {
	f := newContractJobFixture(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "a.go": "package fixture\nfunc A() int { return 1 }\n", "b.go": "package fixture\nfunc B() int { return 2 }\n"})
	refuse := func(path string) error {
		if path == "fixture/b.go" {
			return fmt.Errorf("injected receipt write failure")
		}
		return nil
	}
	contractCoreBeginFault.Store(&refuse)
	defer contractCoreBeginFault.Store(nil)
	a, b := filepath.Join(f.root, "a.go"), filepath.Join(f.root, "b.go")
	require.NoError(t, os.Remove(a))
	require.NoError(t, os.Remove(b))
	_, saveErr := f.mi.IncrementalReindexRepo("fixture", []string{a, b})
	contractCoreBeginFault.Store(nil)
	require.Error(t, saveErr, "the refused begin fails the pass")
	stamps, err := newFixtureScanner(t, f).scan(t.Context(), []string{"fixture/a.go"})
	require.NoError(t, err)
	stamp, ok := stamps.Stamps["fixture/a.go"]
	require.True(t, ok)
	require.False(t, stamp.Accepted, "the failed pass accepted nothing")
	require.Positive(t, stamp.Seq, "the pass began the first file's receipt")
	require.False(t, stamp.Settled, "a failed body settles nothing")
	require.True(t, stamp.Writing, "a failed body leaves its core-write window open")
	require.False(t, stamp.quiescent())
}

// TestContractMutationEndsItsWindowsWithoutSettlingARefusedComplete: a save
// whose body completes every core write and whose Complete is then refused
// for a reason other than supersession (its source witness moved under it)
// closes its file's core-write window, since its rows are complete, and
// settles nothing: only a completed body that Complete found overtaken
// settles (TestContractJobProvesAnOvertakenSave).
func TestContractMutationEndsItsWindowsWithoutSettlingARefusedComplete(t *testing.T) {
	f := newRouteConstantFixture(t)
	f.mi.mu.RLock()
	idx := f.mi.indexers["fixture"]
	f.mi.mu.RUnlock()
	require.NotNil(t, idx)
	leases := graphview.NewLeaseManager()
	registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: "fixture", RootIdentity: idx.legacyOutputTarget().RootPath, Incarnation: t.Name()}, nil)
	require.NoError(t, err)
	_, err = leases.CaptureInitialRawRepositorySource(t.Context(), registration, "accepted-source")
	require.NoError(t, err)
	f.mi.SetOutputGenerationAuthority(NewOutputGenerationAuthority(leases))
	moved := false
	var moveErr error
	restore := store_sqlite.SetContractReceiptWriteHookForTest(func(stage string, paths []string) {
		if moved || stage != "ending" || !slices.Contains(paths, "fixture/consts.go") {
			return
		}
		// The save's staging write ended: its body runs on, its source
		// witness moved under it.
		moved = true
		_, moveErr = leases.InvalidateBaseCorpusSource("fixture")
	})
	path := filepath.Join(f.root, "consts.go")
	writeFile(t, path, jobConsts+"// witness moved\n")
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, future, future))
	_, saveErr := f.mi.IncrementalReindexRepo("fixture", []string{path})
	restore()
	require.True(t, moved)
	require.NoError(t, moveErr)
	require.ErrorIs(t, saveErr, graphview.ErrRawRepositorySourceChanged, "Complete refused the save")
	require.NotErrorIs(t, saveErr, ErrOutputMutationReceiptSuperseded)
	stamps, err := newFixtureScanner(t, f).scan(t.Context(), []string{"fixture/consts.go"})
	require.NoError(t, err)
	stamp := stamps.Stamps["fixture/consts.go"]
	require.False(t, stamp.Accepted, "a refused save is never accepted")
	require.False(t, stamp.Settled, "a refused Complete settles nothing")
	require.False(t, stamp.Writing, "the completed body closed its core-write window")
	require.True(t, stamp.quiescent(), "its rows are complete though nothing accepted them")
}

// TestContractWatcherPatchOvertakenAfterItsWritesEndsItsWindow: a watcher
// patch whose every core write landed, and that a newer save's generation
// then overtook, reports itself superseded and is never accepted; its file's
// core-write window closes all the same, so a contract job reads the file's
// complete rows without waiting for an accept that never comes. Under a
// writer whose applies take as long as its period, nearly every patch ends
// this way.
func TestContractWatcherPatchOvertakenAfterItsWritesEndsItsWindow(t *testing.T) {
	f := newRouteConstantFixture(t)
	f.mi.mu.RLock()
	idx := f.mi.indexers["fixture"]
	f.mi.mu.RUnlock()
	require.NotNil(t, idx)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, Paths: []string{idx.rootPath}, DebounceMs: 50}, zap.NewNop())
	require.NoError(t, err)
	path := filepath.Join(idx.rootPath, "consts.go")
	const generation = 7
	w.pointReindexRaw = func(p string, _ *PublicationStamps) (*IndexResult, error) {
		result, err := idx.incrementalPointWatcherPath(idx.rootPath, p)
		// A newer save of the file is scheduled while the patch ran.
		w.mu.Lock()
		w.pendingGeneration[p] = generation + 1
		w.mu.Unlock()
		return result, err
	}
	writeFile(t, path, jobConsts+"// overtaken by a newer save\n")
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, future, future))
	w.mu.Lock()
	w.pendingGeneration[path] = generation
	w.mu.Unlock()
	require.ErrorIs(t, w.patchGraph(path, ChangeModified, generation), errMutationSuperseded)
	stamps, err := newFixtureScanner(t, f).scan(t.Context(), []string{"fixture/consts.go"})
	require.NoError(t, err)
	stamp := stamps.Stamps["fixture/consts.go"]
	require.Positive(t, stamp.Seq, "the patch staged the file's receipt")
	require.False(t, stamp.Accepted, "an overtaken patch is never accepted")
	require.True(t, stamp.Settled, "its successful full tail settled the exact receipt sequence")
	require.False(t, stamp.Writing, "its core-write window closed with its writes")
	require.True(t, stamp.quiescent())
}

// TestContractCaptureAdmitsAnUnacceptedRowOnTheRuleAJobProvesItsReadsBy: the
// capture admits an unaccepted receipt row on the rule a job proves its reads
// by. A watcher patch overtaken after its writes leaves its row unaccepted
// but settled with its window closed: a job is admitted and publishes
// the cold build from it. An apply that staged a row and has not ended its
// core writes leaves that row's window open: the capture admits it in flight,
// and the job's reads of the file wait for the window to close
// (TestContractCaptureAdmitsAFileInFlight).
func TestContractCaptureAdmitsAnUnacceptedRowOnTheRuleAJobProvesItsReadsBy(t *testing.T) {
	t.Run("overtaken_patch", func(t *testing.T) {
		f := newRouteConstantFixture(t)
		f.mi.mu.RLock()
		idx := f.mi.indexers["fixture"]
		f.mi.mu.RUnlock()
		require.NotNil(t, idx)
		w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, Paths: []string{idx.rootPath}, DebounceMs: 50}, zap.NewNop())
		require.NoError(t, err)
		path := filepath.Join(idx.rootPath, "consts.go")
		const generation = 7
		w.pointReindexRaw = func(p string, _ *PublicationStamps) (*IndexResult, error) {
			result, err := idx.incrementalPointWatcherPath(idx.rootPath, p)
			w.mu.Lock()
			w.pendingGeneration[p] = generation + 1
			w.mu.Unlock()
			return result, err
		}
		consts := jobConsts + "// overtaken by a newer save\n"
		writeFile(t, path, consts)
		future := time.Now().Add(time.Minute)
		require.NoError(t, os.Chtimes(path, future, future))
		before, found, err := f.store.ContractInputStateContext(context.Background(), "fixture", "")
		require.NoError(t, err)
		require.True(t, found)
		w.mu.Lock()
		w.pendingGeneration[path] = generation
		w.mu.Unlock()
		require.ErrorIs(t, w.patchGraph(path, ChangeModified, generation), errMutationSuperseded)
		// The patch left the input-state header pending for the newer save
		// that overtook it, whose accept restoring it stands in for; the
		// patch's receipt row stays as the patch left it.
		f.restoreKey(before)
		state := f.store.ContractReceiptWriteStates("fixture", "", []string{"fixture/consts.go"})["fixture/consts.go"]
		require.True(t, state.Settled, "the overtaken patch completed its exact core writes")
		require.False(t, state.Writing, "its completed tail closed the window")
		report, err := f.job(nil, contractJobDeadline(time.Minute))
		require.NoError(t, err)
		require.True(t, report.Published)
		require.Equal(t, contractMeta(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": consts, "helper.go": jobHelper})), contractMeta(f.contracts()))
	})
	t.Run("staged_row", func(t *testing.T) {
		// An apply staged the owner's receipt and has not ended its core
		// writes: its rows may be between an evict and its add.
		f := newRouteConstantFixture(t)
		const owner = "fixture/consts.go"
		f.stageInFlight(owner)
		state := f.store.ContractReceiptWriteStates("fixture", "", []string{owner})[owner]
		require.True(t, state.Seq > 0 && state.Writing && !state.Settled, "the row is staged with its window open")
		inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
		require.NoError(t, err)
		snapshot, _, err := NewContractFollowupCapture(f.options)(t.Context(), nil, inputs, "fixture", "")
		require.NoError(t, err, "a row whose core writes may be in flight is admitted in flight")
		require.Equal(t, []string{owner}, snapshot.InFlight)
		require.NotNil(t, snapshot.Scan, "a snapshot with a file in flight takes stamp scans")
		snapshot.Release()
		// The apply ends its core writes; nothing accepts or settles it.
		f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
		report, err := f.job(nil, contractJobDeadline(time.Minute))
		require.NoError(t, err)
		require.True(t, report.Published)
		require.Equal(t, contractMeta(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractMeta(f.contracts()))
	})
	t.Run("rule", func(t *testing.T) {
		// The admission rule itself: an unaccepted row is complete once a
		// mutation of this process staged it and ended its core writes, or
		// settled it; never while its window is open, and never when this
		// process saw no mutation of it end (seq 0: a row left by the
		// process before a restart).
		for _, row := range []struct {
			state    store_sqlite.ContractReceiptWriteState
			complete bool
		}{
			{store_sqlite.ContractReceiptWriteState{}, false},
			{store_sqlite.ContractReceiptWriteState{Seq: 2}, true},
			{store_sqlite.ContractReceiptWriteState{Seq: 2, Settled: true}, true},
			{store_sqlite.ContractReceiptWriteState{Seq: 2, Writing: true}, false},
			{store_sqlite.ContractReceiptWriteState{Seq: 2, Settled: true, Writing: true}, false},
		} {
			require.Equal(t, row.complete, contractReceiptRowsComplete(row.state), "%+v", row.state)
		}
	})
}

// TestContractJobLookupTornByAnApplyIsAskedOnlyOutsideItsWindow: the route's
// constant lookup is taken while its owner is between an evict and its add
// (apply k), and the first retry finds the owner inside a later apply's
// window (k+1), where the lookup would come back as empty as it was
// recorded. An answer asked while a window is open is never compared: the
// route is not proven until the owner's window closes, and then its lookup
// finds the constant and the route is extracted afresh. With k+1 completed
// the job publishes the cold build; with k+1 held it is superseded, never
// published from the torn lookup and never failed.
func TestContractJobLookupTornByAnApplyIsAskedOnlyOutsideItsWindow(t *testing.T) {
	for _, row := range []struct {
		name     string
		complete bool
	}{{"later_apply_completes", true}, {"later_apply_held", false}} {
		t.Run(row.name, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			waits := 0
			var release func()
			f.onWait = func() {
				waits++
				switch {
				case waits == 1 && release != nil:
					release()
					release = f.beginHeld("fixture/consts.go")
				case waits == 2 && row.complete && release != nil:
					release()
					release = nil
				}
			}
			equalInWindow := 0
			hooks := &contractConvergenceHooks{
				roundFiles: 1,
				roundWait:  200 * time.Millisecond,
				beforeExtract: func(path string) {
					if path == "fixture/routes.go" && waits == 0 && release == nil {
						release = f.beginHeld("fixture/consts.go")
					}
				},
				revalidated: func(path string, equal bool) {
					if equal && release != nil {
						equalInWindow++
					}
				},
			}
			report, err := f.job(hooks, contractJobDeadline(time.Minute))
			f.onWait = nil
			if release != nil {
				release()
			}
			require.GreaterOrEqual(t, waits, 1)
			require.Zero(t, equalInWindow, "no answer asked inside the owner's window was compared")
			require.NotEqual(t, viewmetrics.ContractJobFailed, contractJobOutcomeOf(err), "the job is never failed: %v", err)
			if !row.complete {
				require.Error(t, err)
				require.False(t, report.Published, "the torn lookup is never published")
				return
			}
			require.NoError(t, err)
			require.True(t, report.Published)
			want := coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})
			require.Contains(t, contractIDs(want), "http::GET::/before")
			require.Equal(t, contractMeta(want), contractMeta(f.contracts()))
		})
	}
}

// TestContractJobEnrichmentRecheckWaitsOutAnApplysWindow: the enrichment
// round reads the route file while an apply of it has its window open (its
// rows still complete), so the round is torn by it. The re-check first finds
// the file in that window, then in a later apply's, between its evict and its
// add, where its answers would differ from the recorded ones and rerun the
// round. It is asked only once that apply's window closed, finds the recorded
// answers, and the job publishes the cold build with no enrichment rerun.
func TestContractJobEnrichmentRecheckWaitsOutAnApplysWindow(t *testing.T) {
	f := newRouteConstantFixture(t)
	armed, staged := false, false
	waits := 0
	var release func()
	f.onWait = func() {
		waits++
		switch waits {
		case 1:
			release = f.beginHeld("fixture/routes.go")
		case 2:
			if release != nil {
				release()
				release = nil
			}
		}
	}
	hooks := &contractConvergenceHooks{
		beforeR1: func([]string) { armed = true },
		// The first evidence after the units' round is the enrichment
		// round's, after its R0.
		openedEvidence: func(*store_sqlite.Store) {
			if armed && !staged {
				staged = true
				f.stageInFlight("fixture/routes.go")
			}
		},
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	if release != nil {
		release()
	}
	require.NoError(t, err)
	require.True(t, report.Published)
	require.True(t, staged)
	require.GreaterOrEqual(t, waits, 2, "the re-check waited for the later apply's window to close")
	require.Positive(t, report.EnrichRechecks)
	require.Zero(t, report.EnrichReruns, "a re-check is never asked inside an apply's window")
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractIDs(f.contracts()))
}

// TestContractJobEnrichmentRoundTornAcrossAnEvictIsRetried: the enrichment
// round reads while the route file is between an evict and its add, so the
// core answers none of the route record's owners. That round is a read it
// could not complete: it is retried at once, never certified (a re-check
// inside a later apply's evict would agree with it), never rerun as stale
// and never ends the job failed. With the later apply completed the job
// publishes the cold build; with it held the job is superseded.
func TestContractJobEnrichmentRoundTornAcrossAnEvictIsRetried(t *testing.T) {
	for _, row := range []struct {
		name     string
		complete bool
	}{{"later_apply_completes", true}, {"later_apply_held", false}} {
		t.Run(row.name, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			armed, held := false, false
			waits := 0
			var release func()
			f.onWait = func() {
				waits++
				switch {
				case waits == 1 && release != nil:
					release()
					release = f.beginHeld("fixture/routes.go")
				case waits == 2 && row.complete && release != nil:
					release()
					release = nil
				}
			}
			hooks := &contractConvergenceHooks{
				roundWait: 200 * time.Millisecond,
				beforeR1:  func([]string) { armed = true },
				openedEvidence: func(*store_sqlite.Store) {
					if armed && !held {
						held = true
						release = f.beginHeld("fixture/routes.go")
					}
				},
			}
			report, err := f.job(hooks, contractJobDeadline(time.Minute))
			f.onWait = nil
			if release != nil {
				release()
			}
			require.True(t, held)
			require.NotEqual(t, viewmetrics.ContractJobFailed, contractJobOutcomeOf(err), "a torn enrichment round never fails the job: %v", err)
			if !row.complete {
				require.Error(t, err)
				require.False(t, report.Published)
				return
			}
			require.NoError(t, err)
			require.True(t, report.Published)
			require.Zero(t, report.EnrichReruns, "the torn round was retried, not certified and rerun")
			want := coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})
			require.Equal(t, contractMeta(want), contractMeta(f.contracts()))
		})
	}
}

// TestContractJobEnrichmentSeedCarrierTornAwayIsRechecked: the dependency
// injection seed edge g.go carries is torn away while the enrichment round
// reads its seeds (g.go between an evict and its add), so the seed answer
// holds no row of g.go, and the apply then completes with g.go's contract
// content kept. A seed read depends on every file whose unit carries seed
// rows, so the round counts g.go as read and torn: its re-check asks the seed
// read again once g.go's window closed, the answer differs, and the round
// reruns once. The publish carries the DI contract a cold build derives.
func TestContractJobEnrichmentSeedCarrierTornAwayIsRechecked(t *testing.T) {
	files := map[string]string{
		"routes.go": jobRoutes, "consts.go": jobConsts,
		"f.go": "package fixture\nfunc Target() int { return 2 }\n",
		"g.go": "package fixture\nfunc Wire() int { return 1 }\n",
	}
	seed := graph.Edge{From: "fixture/g.go::Wire", To: "fixture/f.go::Target", Kind: graph.EdgeProvides, FilePath: "fixture/g.go", Line: 2,
		Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: "SYNTH"}}
	want := coldContractsWithEdges(t, files, []*graph.Edge{&seed})
	require.Contains(t, contractIDs(want), "di::SYNTH")
	f := newContractJobFixture(t, files)
	require.NoError(t, f.store.AddBatchChecked(nil, []*graph.Edge{&seed}))
	proven := 0
	var release func()
	hooks := &contractConvergenceHooks{
		proven: func(*contractUnit) { proven++ },
		// The first evidence opened after every unit is proven is the
		// enrichment round's, after its R0.
		openedEvidence: func(*store_sqlite.Store) {
			if proven == len(files) && release == nil {
				release = f.beginHeld("fixture/g.go")
			}
		},
		afterPP: func() {
			if release != nil {
				release()
				release = func() {}
			}
		},
	}
	report, err := f.job(hooks, nil)
	require.NotNil(t, release, "the seed carrier was torn away during the enrichment round")
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 1, report.EnrichReruns, "the torn seed answer reran the round once")
	require.Equal(t, contractMeta(want), contractMeta(f.contracts()))
}

// TestContractConvergenceCountsOnlyIdleRoundsThatMovedOrWaitedOut pins the
// idle accounting: a round with progress resets the count, a round an event
// woke that found its inputs unmoved never counts, and a job ends only after
// contractIdleRounds counted rounds with no progress between them, superseded
// by moving inputs when one of them saw a move and by unsettled reads only
// after consecutive rounds that each waited out.
func TestContractConvergenceCountsOnlyIdleRoundsThatMovedOrWaitedOut(t *testing.T) {
	type step struct{ progress, moved, waitedOut, blocked bool }
	run := func(steps ...step) error {
		c := &contractConvergence{}
		for _, s := range steps {
			mark := c.markRound()
			if s.progress {
				c.progress++
			}
			if s.moved {
				c.clock.maxChanged++
			}
			if s.blocked {
				c.block("fixture/consts.go", ContractReceiptStamp{Seq: 1, Writing: true})
			}
			c.endRound(mark, s.waitedOut)
			if err := c.idleErr(); err != nil {
				return err
			}
		}
		return nil
	}
	moved, waited, woke, proof := step{moved: true}, step{waitedOut: true}, step{}, step{progress: true, moved: true}
	// A writer whose moves alternate with proofs never ends the job.
	require.NoError(t, run(moved, moved, proof, moved, moved, proof, moved, moved, proof, moved, moved))
	// An event stream that never moves the inputs never ends it either.
	var stream []step
	for range 4 * contractIdleRounds {
		stream = append(stream, woke)
	}
	require.NoError(t, run(stream...))
	// Waits that ran out, each broken off by an event, are not consecutive.
	require.NoError(t, run(waited, woke, waited, woke, waited, woke, waited, proof, waited, waited))
	require.ErrorIs(t, run(waited, waited, waited), errContractJobReadUnsettled)
	require.ErrorIs(t, run(waited, woke, waited, waited, waited), errContractJobReadUnsettled)
	// Moves count whatever wakes the rounds between them, and the rounds an
	// event woke that found nothing moved never join their count.
	require.ErrorIs(t, run(moved, woke, waited, woke, moved), errContractJobInputsMoving)
	require.NoError(t, run(moved, woke, woke, woke, moved))
	// A blocked round an event woke never counts, moved or not: a writer
	// whose applies hold the inputs' windows open through every ask never
	// ends the job.
	blockedMoved, blockedWoke, blockedWaited := step{moved: true, blocked: true}, step{blocked: true}, step{moved: true, waitedOut: true, blocked: true}
	var blocked []step
	for range 10 * contractIdleRounds {
		blocked = append(blocked, blockedMoved, blockedWoke)
	}
	require.NoError(t, run(blocked...))
	require.NoError(t, run(moved, blockedMoved, blockedMoved, moved, proof, moved, blockedMoved, moved))
	// Nor does it break or extend a run of waits that ran out, or a count
	// with a move in it.
	require.ErrorIs(t, run(waited, blockedWoke, waited, blockedMoved, waited), errContractJobReadUnsettled)
	require.NoError(t, run(waited, blockedWoke, waited, woke, waited))
	require.ErrorIs(t, run(moved, blockedMoved, moved, blockedWoke, moved), errContractJobInputsMoving)
	// Waits that ran out count whatever blocked their rounds, and never as
	// moving inputs: an apply whose window never closes ends the job with
	// unsettled reads.
	require.ErrorIs(t, run(blockedWaited, blockedWaited, blockedWaited), errContractJobReadUnsettled)
	// Blocked rounds never join a count with a move in it: one unblocked
	// round that saw a move and two blocked rounds whose waits ran out leave
	// the job running, and only a third wait that runs out ends it, with
	// unsettled reads.
	require.NoError(t, run(moved, blockedWaited, blockedWaited))
	require.NoError(t, run(blockedWaited, moved, blockedWaited))
	require.ErrorIs(t, run(moved, blockedWaited, blockedWaited, blockedWaited), errContractJobReadUnsettled)
	require.ErrorIs(t, run(moved, blockedWaited, moved, blockedWaited, moved), errContractJobInputsMoving)
}

// TestContractJobEventStreamNeverEndsAJobWaitingOnAnApply: the route's
// constant owner stays between an evict and its add for six retry waits while
// an unrelated file saves at every wait. Each wait is woken by those saves'
// events, and every round finds the route's inputs unmoved: such rounds never
// count, so the job waits the apply out and publishes the cold build once the
// apply completes.
func TestContractJobEventStreamNeverEndsAJobWaitingOnAnApply(t *testing.T) {
	f := newRouteConstantFixture(t)
	const events = 6
	waits, woke := 0, 0
	var release func()
	f.onWait = func() {
		if release == nil {
			return
		}
		if waits++; waits > events {
			release()
			release = nil
			return
		}
		f.saveKeepingKey("helper.go", jobHelper+fmt.Sprintf("// event %d\n", waits))
	}
	hooks := &contractConvergenceHooks{
		roundFiles: 1,
		beforeExtract: func(path string) {
			if path == "fixture/routes.go" && waits == 0 && release == nil {
				release = f.beginHeld("fixture/consts.go")
			}
		},
		woke: func() { woke++ },
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	if release != nil {
		release()
	}
	require.NoError(t, err, "unrelated events never end a job waiting on an apply")
	require.True(t, report.Published)
	require.Greater(t, waits, events)
	require.Greater(t, woke, events, "every round was woken by an event")
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractIDs(f.contracts()))
}

// contractJobDeadline is a job's yield that ends it once it has asked for
// longer than d: a job that asks again without waiting for a window to close
// spins instead of ending, and fails by name instead of hanging the package.
func contractJobDeadline(d time.Duration) func(context.Context) error {
	deadline := time.Now().Add(d)
	return func(ctx context.Context) error {
		if time.Now().After(deadline) {
			return fmt.Errorf("the job was still asking after %s", d)
		}
		return ctx.Err()
	}
}

// contractHeldApplies rewrites one file body-only the way an apply does, one
// apply at a time: begin stages the file's receipt (opening its core-write
// window) and evicts its rows; the window stays open until release, which
// re-adds the rows and ends the apply's core writes (closing the window) from
// another goroutine, the way a concurrent writer finishes. Nothing accepts or
// announces the applies.
type contractHeldApplies struct {
	f    *contractJobFixture
	path string
	wg   sync.WaitGroup
	mu   sync.Mutex
	// held is the open apply's evicted rows, nil while no window is open.
	held   *contractHeldApply
	closes []time.Time
	err    error
}

type contractHeldApply struct {
	nodes []*graph.Node
	edges []*graph.Edge
}

func (a *contractHeldApplies) begin() {
	_, nodes, edges, err := a.f.evictStaged(a.path)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		if a.err == nil {
			a.err = err
		}
		return
	}
	a.held = &contractHeldApply{nodes: nodes, edges: edges}
}

// release closes the open apply's window, if one is open and no release of it
// is under way, and reports whether it started one.
func (a *contractHeldApplies) release() bool {
	a.mu.Lock()
	held := a.held
	a.held = nil
	a.mu.Unlock()
	if held == nil {
		return false
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		err := a.f.store.AddBatchChecked(held.nodes, held.edges)
		if err == nil {
			a.f.store.EndContractCoreWrites("fixture", "", a.f.store.ContractReceiptWriteSeqs("fixture", "", []string{a.path}))
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.closes = append(a.closes, time.Now())
		if err != nil && a.err == nil {
			a.err = err
		}
	}()
	return true
}

// writing reports whether the file has a core-write window open.
func (a *contractHeldApplies) writing() bool {
	return a.f.store.ContractReceiptWriteStates("fixture", "", []string{a.path})[a.path].Writing
}

// lastClose is when the latest apply closed its window.
func (a *contractHeldApplies) lastClose() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.closes) == 0 {
		return time.Time{}
	}
	return a.closes[len(a.closes)-1]
}

// TestContractJobProvesInTheGapAfterApplyWindowsCoveringItsRounds: the
// route's constant owner, the dependency of the job's last unit, is rewritten
// body-only by one apply per round for 30 rounds. Each apply's core-write
// window stays open until the job, its round blocked on it, waits: only then
// does the writer finish, so every round reads across an open window however
// long the round takes on the host. Every other apply begins as a round
// opens, so its opening scan finds the window open; the others begin right
// after the opening scan found it closed, so the round's answers are torn and
// its closing scan sees the move. Each of those rounds is blocked, never
// counted, so the job is never superseded, and each wait wakes when the
// window closes, not when an apply begins: the round after every close starts
// at once, inside the gap. The first round after the last apply lands in that
// gap and the job publishes, equal to a cold build, with no enrichment rerun.
func TestContractJobProvesInTheGapAfterApplyWindowsCoveringItsRounds(t *testing.T) {
	f := newRouteConstantFixture(t)
	const (
		owner   = "fixture/consts.go"
		applies = 30
	)
	// Scale the wait and its half-wait wake bound together, so a timer-only
	// fallback still fails the wake assertion on a slow host.
	roundWait := testTimingSlack(5 * time.Second)
	a := &contractHeldApplies{f: f, path: owner}
	defer a.wg.Wait()
	staged, begun, wakes, released := false, 0, 0, 0
	var latencies []time.Duration
	// The first wait closes the window the route's first round read across;
	// every later one finishes the apply whose window blocked the round.
	f.onWait = func() {
		if staged && begun == 0 {
			f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
		}
		if a.release() {
			released++
		}
	}
	apply := func() {
		if begun < applies && !a.writing() {
			begun++
			a.begin()
		}
	}
	hooks := &contractConvergenceHooks{
		roundWait: roundWait,
		afterExtract: func(path string) {
			if path == "fixture/routes.go" && !staged {
				staged = true
				f.stageInFlight(owner)
			}
		},
		woke: func() {
			wakes++
			if closed := a.lastClose(); begun > 0 && !closed.IsZero() {
				latencies = append(latencies, time.Since(closed))
			}
			if begun%2 == 1 {
				apply()
			}
		},
		beforeAsk: func() {
			if begun%2 == 0 {
				apply()
			}
		},
	}
	start := time.Now()
	// A job that asks again without waiting for a window to close spins on
	// the writes that open them and fails by name instead of hanging.
	yield := contractJobDeadline(3 * time.Minute)
	report, err := f.job(hooks, yield)
	elapsed := time.Since(start)
	f.onWait = nil
	// An apply the job never waited on is finished here, so its writer ends.
	a.release()
	a.wg.Wait()
	a.mu.Lock()
	applyErr := a.err
	a.mu.Unlock()
	slices.Sort(latencies)
	t.Logf("applies=%d released=%d wakes=%d rounds=%d elapsed=%s latencies=%v", begun, released, wakes, report.Rounds, elapsed, latencies)
	require.NoError(t, applyErr)
	require.NoError(t, err, "blocked rounds never end the job")
	require.True(t, report.Published)
	require.Equal(t, applies, begun, "an apply held the owner's window open through 30 rounds")
	require.Equal(t, applies, released, "every apply's window blocked a round that then waited on it")
	require.Zero(t, report.EnrichReruns)
	require.LessOrEqual(t, wakes, applies+5, "each wait woke once, on a window's close")
	require.NotEmpty(t, latencies)
	require.Less(t, latencies[len(latencies)-1], roundWait/2, "the round after a close started inside the gap, woken by the close")
	want := coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})
	require.Contains(t, contractIDs(want), "http::GET::/before")
	require.Equal(t, contractMeta(want), contractMeta(f.contracts()))
}

// TestContractJobBlockedRoundNeverBreaksAWaitedOutRun: the route's constant
// owner is inside an apply's core-write window from before the job's first
// scan, so the route and the owner are both asked again only outside it. Two
// waits run out with the window open. The third wait is woken by the window's
// close, and a further apply opens it again while that round asks: its
// answers hold, but the move makes the round blocked, not progress. The
// window then stays open with no event: one more wait that runs out is the
// third of the run, and the job ends with unsettled reads after four waits.
func TestContractJobBlockedRoundNeverBreaksAWaitedOutRun(t *testing.T) {
	f := newRouteConstantFixture(t)
	const owner = "fixture/consts.go"
	staged := false
	f.adjust = func(*ContractFollowupSnapshot) {
		if !staged {
			staged = true
			f.stageInFlight(owner)
		}
	}
	waits, reopened := 0, false
	f.onWait = func() {
		if waits++; waits == 3 {
			f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
		}
	}
	hooks := &contractConvergenceHooks{
		roundWait: 200 * time.Millisecond,
		beforeAsk: func() {
			if waits == 3 && !reopened && !f.store.ContractReceiptWriteStates("fixture", "", []string{owner})[owner].Writing {
				reopened = true
				f.stageInFlight(owner)
			}
		},
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	require.True(t, reopened, "an apply reopened the window while the woken round asked")
	require.False(t, report.Published)
	require.Equal(t, viewmetrics.ContractJobSupersededReadUnsettled, contractJobOutcomeOf(err), "job error: %v", err)
	require.Equal(t, 4, waits, "the blocked round between the waits that ran out neither counted nor broke their run")
}

// TestContractJobRecheckWaitsNeverCarryIntoTheNextIteration: an iteration
// that made progress leaves no idle count behind. In the second iteration the
// enrichment round's re-check finds the route file inside an apply's open
// core-write window twice in a row with no event (two waits run out). The
// helper then saves again and the window closes: the re-check's next round
// succeeds, and it returns on that success without accounting the round. The
// helper's save makes a third iteration, and its next apply begins before the
// fence, so that iteration's first retry finds its window open and waits out
// once more before it closes. That wait is the first of its iteration: the job
// publishes, and does not end with unsettled reads by joining it to the
// re-check's two.
func TestContractJobRecheckWaitsNeverCarryIntoTheNextIteration(t *testing.T) {
	f := newRouteConstantFixture(t)
	const routes, helper = "fixture/routes.go", "fixture/helper.go"
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	iteration := 0
	routesStaged, helperSaved, helperStaged := false, false, false
	recheckWaits, retryWaits := 0, 0
	f.onWait = func() {
		switch {
		case helperStaged:
			if retryWaits++; retryWaits == 2 {
				f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{helper}))
			}
		case routesStaged:
			if recheckWaits++; recheckWaits == 3 {
				// The helper saves again, and the route file's window
				// closes: the re-check's next round succeeds at once.
				helperSaved = true
				files["helper.go"] = jobHelper + "func Helper3() int { return 3 }\n"
				f.saveKeepingKey("helper.go", files["helper.go"])
				f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{routes}))
			}
		}
	}
	hooks := &contractConvergenceHooks{
		roundWait: 200 * time.Millisecond,
		fenced:    func(i int, _ []string) { iteration = i + 1 },
		// The second iteration's proof of the saved helper is its last: the
		// route file's apply begins right after it, before the re-check.
		proven: func(unit *contractUnit) {
			if iteration == 1 && unit.path == helper && !routesStaged {
				routesStaged = true
				f.stageInFlight(routes)
			}
		},
		beforeFence: func(i int) {
			switch i {
			case 0:
				files["helper.go"] = jobHelper + "func Helper2() int { return 2 }\n"
				f.saveKeepingKey("helper.go", files["helper.go"])
			case 1:
				// The helper's next apply begins before the third iteration
				// extracts its save.
				helperStaged = true
				f.stageInFlight(helper)
			}
		},
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	t.Logf("iterations=%d recheck_waits=%d retry_waits=%d rechecks=%d", report.Iterations, recheckWaits, retryWaits, report.EnrichRechecks)
	require.True(t, routesStaged && helperSaved && helperStaged)
	require.Equal(t, 3, recheckWaits, "the re-check waited out the route file's window twice before its close")
	require.NoError(t, err, "waits counted in an iteration that made progress never carry into the next")
	require.True(t, report.Published)
	require.Equal(t, 2, retryWaits)
	require.Equal(t, 3, report.Iterations)
	require.Equal(t, contractIDs(coldContracts(t, files)), contractIDs(f.contracts()))
}

// TestContractJobRetryWaitsForASuspectsWindowBesideAFirstRound: the second
// iteration's first pass revalidates the route unit, a suspect since its
// constant's owner changed, and refuses it on two open core-write windows: an
// apply of the route file itself and one of the owner. The same pass then
// extracts the owner in a first round, refused on the owner's window. The
// retry waits for the suspect's windows: receipt writes of another file leave
// them open and do not wake it (the wait runs out), the close of the route
// file's window wakes it (the owner's still holds the round, which waits
// again), and the round after the owner's close proves both and the job
// publishes the owner's new path.
func TestContractJobRetryWaitsForASuspectsWindowBesideAFirstRound(t *testing.T) {
	f := newRouteConstantFixture(t)
	const routes, owner, helper = "fixture/routes.go", "fixture/consts.go", "fixture/helper.go"
	consts := "package fixture\nconst RoutePath = \"/after\"\n"
	staged := false
	waits, woke := 0, 0
	f.onWait = func() {
		if !staged {
			return
		}
		waits++
		switch waits {
		case 1:
			// Receipt writes of the helper, no Published event.
			f.stageInFlight(helper)
			f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{helper}))
		case 2:
			f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{routes}))
		case 3:
			f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
		}
	}
	hooks := &contractConvergenceHooks{
		roundWait: 300 * time.Millisecond,
		beforeFence: func(i int) {
			if i == 0 {
				f.saveKeepingKey("consts.go", consts)
				staged = true
				f.stageInFlight(routes)
				f.stageInFlight(owner)
			}
		},
		woke: func() { woke++ },
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	t.Logf("iterations=%d waits=%d woke=%d", report.Iterations, waits, woke)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 3, waits)
	require.Equal(t, 2, woke, "the helper's writes never woke the retry; each close of a window it waited on did")
	want := coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": consts, "helper.go": jobHelper})
	require.Contains(t, contractIDs(want), "http::GET::/after")
	require.Equal(t, contractMeta(want), contractMeta(f.contracts()))
}

// TestContractJobRetryKeepsTheWaitsOfUnprovenFilesAcrossASuspectsPass: the
// first rounds leave the constant's owner refused on its open core-write
// window and the helper a suspect read inside its own. At the first wait the
// owner's window closes; the retry proves the owner, whose key check makes the
// route unit, proven before the owner moved, a suspect again, and the route
// file's own apply begins. The helper's revalidation in that retry is refused
// on its window, and the route unit's pass that follows is refused on the
// route file's window. The retry then waits for both: the close of the
// helper's window, the first to close, wakes it at once, and the job
// publishes the cold set.
func TestContractJobRetryKeepsTheWaitsOfUnprovenFilesAcrossASuspectsPass(t *testing.T) {
	files := map[string]string{"a_routes.go": jobRoutes, "b_consts.go": jobConsts, "c_helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	const routes, owner, helper = "fixture/a_routes.go", "fixture/b_consts.go", "fixture/c_helper.go"
	closeWindow := func(path string) {
		f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{path}))
	}
	staged, routesStaged := false, false
	waits := 0
	wokeAt := make(map[int]bool)
	f.onWait = func() {
		waits++
		switch waits {
		case 1:
			closeWindow(owner)
		case 2:
			closeWindow(helper)
		case 3:
			closeWindow(routes)
		}
	}
	hooks := &contractConvergenceHooks{
		roundFiles: 1,
		roundWait:  300 * time.Millisecond,
		beforeExtract: func(path string) {
			if path == owner && !staged {
				staged = true
				f.stageInFlight(owner)
				f.stageInFlight(helper)
			}
		},
		proven: func(unit *contractUnit) {
			if unit.path == owner && !routesStaged {
				routesStaged = true
				f.stageInFlight(routes)
			}
		},
		woke: func() { wokeAt[waits] = true },
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	t.Logf("waits=%d woke=%v rounds=%d revalidated=%d", waits, wokeAt, report.Rounds, report.Revalidated)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.True(t, staged && routesStaged)
	require.GreaterOrEqual(t, waits, 3)
	require.True(t, wokeAt[1], "the owner's close woke the first retry")
	require.True(t, wokeAt[2], "the helper's close woke the retry, though the route unit's pass came after the helper's refusal")
	require.True(t, wokeAt[3], "the route file's close woke the last retry")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}

// TestContractConvergenceWaitWakesOnTheCloseOfAWindowItWaitsOn: a wait whose
// round refused reads only because an input's core-write window was open
// sleeps through receipt events that leave that window as it was (writes of
// other paths) and wakes on the one that closes it; a wait with a refusal no
// window holds wakes on the first receipt event.
func TestContractConvergenceWaitWakesOnTheCloseOfAWindowItWaitsOn(t *testing.T) {
	const path = "fixture/consts.go"
	var mu sync.Mutex
	events, writing := uint64(0), true
	changed := make(chan struct{})
	emit := func(open bool) {
		mu.Lock()
		defer mu.Unlock()
		events++
		writing = open
		close(changed)
		changed = make(chan struct{})
	}
	snap := ContractFollowupSnapshot{
		ReceiptEvents: func() (uint64, <-chan struct{}) {
			mu.Lock()
			defer mu.Unlock()
			return events, changed
		},
		Scan: func(ctx context.Context, paths []string) (ContractFollowupScan, error) {
			mu.Lock()
			defer mu.Unlock()
			return ContractFollowupScan{Settled: true, Stamps: map[string]ContractReceiptStamp{path: {Seq: 1, Writing: writing}}}, ctx.Err()
		},
	}
	wait := func(ctx context.Context, _ uint64, until time.Time) bool {
		select {
		case <-ctx.Done():
		case <-time.After(time.Until(until)):
		}
		return false
	}
	for _, row := range []struct {
		name   string
		window bool
	}{{"window_open", true}, {"moved_and_closed", false}} {
		t.Run(row.name, func(t *testing.T) {
			c := &contractConvergence{ctx: t.Context(), snap: snap, req: ContractFollowupRequest{WaitPublished: wait, PublishedEpoch: func() uint64 { return 0 }}}
			mu.Lock()
			writing = true
			mu.Unlock()
			c.markRound()
			if row.window {
				c.block(path, ContractReceiptStamp{Seq: 1, Writing: true})
			} else {
				c.blockMoved()
			}
			since := c.wake()
			woke := make(chan time.Time, 1)
			go func() {
				if c.waitWake(since, time.Now().Add(2*testTimingSlack(time.Second))) {
					woke <- time.Now()
				}
				close(woke)
			}()
			time.Sleep(50 * time.Millisecond)
			// A write of another path ends: the window stays open.
			began := time.Now()
			emit(true)
			if !row.window {
				at, ok := <-woke
				require.True(t, ok, "a refusal no window holds wakes on any event")
				require.Less(t, at.Sub(began), testTimingSlack(time.Second))
				return
			}
			select {
			case <-woke:
				t.Fatal("the write that left the window open woke the wait")
			case <-time.After(100 * time.Millisecond):
			}
			closed := time.Now()
			emit(false)
			at, ok := <-woke
			require.True(t, ok, "the close woke the wait")
			require.Less(t, at.Sub(closed), testTimingSlack(time.Second))
		})
	}
}

// TestContractConvergenceWaitWakesOnACloseFollowedAtOnceByTheNextBegin: a
// wait on an open core-write window whose close is followed by the next
// apply's staging before the waiter's scan of the path. The scan finds a
// window open, but not the one the wait was armed on (its seq moved): the
// window waited on closed, and the wait wakes.
func TestContractConvergenceWaitWakesOnACloseFollowedAtOnceByTheNextBegin(t *testing.T) {
	const path = "fixture/consts.go"
	var mu sync.Mutex
	events, seq := uint64(0), uint64(1)
	changed := make(chan struct{})
	snap := ContractFollowupSnapshot{
		ReceiptEvents: func() (uint64, <-chan struct{}) {
			mu.Lock()
			defer mu.Unlock()
			return events, changed
		},
		Scan: func(ctx context.Context, paths []string) (ContractFollowupScan, error) {
			mu.Lock()
			defer mu.Unlock()
			return ContractFollowupScan{Settled: true, Stamps: map[string]ContractReceiptStamp{path: {Seq: seq, Writing: true}}}, ctx.Err()
		},
	}
	wait := func(ctx context.Context, _ uint64, until time.Time) bool {
		select {
		case <-ctx.Done():
		case <-time.After(time.Until(until)):
		}
		return false
	}
	c := &contractConvergence{ctx: t.Context(), snap: snap, req: ContractFollowupRequest{WaitPublished: wait, PublishedEpoch: func() uint64 { return 0 }}}
	c.markRound()
	c.block(path, ContractReceiptStamp{Seq: 1, Writing: true})
	since := c.wake()
	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- c.waitWake(since, start.Add(2*testTimingSlack(time.Second))) }()
	time.Sleep(50 * time.Millisecond)
	// The window of seq 1 closes (an event) and the next apply opens seq 2
	// before any scan runs.
	mu.Lock()
	events += 2
	seq = 2
	close(changed)
	changed = make(chan struct{})
	mu.Unlock()
	woke := <-done
	require.True(t, woke, "the window the wait was armed on closed")
	require.Less(t, time.Since(start), testTimingSlack(time.Second), "the wait woke on the close, not at its deadline")
}

// TestContractConvergenceWaitSeesWhatLandedBeforeItListened: the wait is
// level-triggered. A receipt event, or the close of a window it waits on,
// that lands after the round sampled its wake level and before the wait
// subscribes to the next notification has already announced itself: the wait
// wakes at once instead of sleeping to its deadline and counting the round as
// waited out.
func TestContractConvergenceWaitSeesWhatLandedBeforeItListened(t *testing.T) {
	const path = "fixture/consts.go"
	for _, row := range []struct {
		name string
		// window arms the wait on path's open window, else on any event.
		window bool
		// notified advances the event count; a close whose notification
		// was consumed before the sample only shows in the scan.
		notified bool
	}{{"event", false, true}, {"window_closed", true, true}, {"window_closed_unannounced", true, false}} {
		t.Run(row.name, func(t *testing.T) {
			var mu sync.Mutex
			events, writing := uint64(7), true
			changed := make(chan struct{})
			snap := ContractFollowupSnapshot{
				ReceiptEvents: func() (uint64, <-chan struct{}) {
					mu.Lock()
					defer mu.Unlock()
					return events, changed
				},
				Scan: func(ctx context.Context, paths []string) (ContractFollowupScan, error) {
					mu.Lock()
					defer mu.Unlock()
					return ContractFollowupScan{Settled: true, Stamps: map[string]ContractReceiptStamp{path: {Seq: 1, Writing: writing}}}, ctx.Err()
				},
			}
			wait := func(ctx context.Context, _ uint64, until time.Time) bool {
				select {
				case <-ctx.Done():
				case <-time.After(time.Until(until)):
				}
				return false
			}
			c := &contractConvergence{ctx: t.Context(), snap: snap, req: ContractFollowupRequest{WaitPublished: wait, PublishedEpoch: func() uint64 { return 0 }}}
			c.markRound()
			if row.window {
				c.block(path, ContractReceiptStamp{Seq: 1, Writing: true})
			} else {
				c.blockMoved()
			}
			since := c.wake()
			// What wakes the wait lands now, before it listens.
			mu.Lock()
			writing = false
			if row.notified {
				events++
				close(changed)
				changed = make(chan struct{})
			}
			mu.Unlock()
			start := time.Now()
			woke := c.waitWake(since, start.Add(2*testTimingSlack(time.Second)))
			require.True(t, woke, "what landed before the wait listened woke it")
			require.Less(t, time.Since(start), testTimingSlack(time.Second), "the wait woke at once, not at its deadline")
		})
	}
}

// TestContractConvergenceWaitWakesOnTheCloseOfAnyWindowItWaitsOn: a round
// refused reads on two open core-write windows. The close of either is the
// start of a gap the round can be asked again in, so the wait wakes on the
// first close and does not wait for both.
func TestContractConvergenceWaitWakesOnTheCloseOfAnyWindowItWaitsOn(t *testing.T) {
	const first, second = "fixture/consts.go", "fixture/helper.go"
	var mu sync.Mutex
	events := uint64(0)
	open := map[string]bool{first: true, second: true}
	changed := make(chan struct{})
	snap := ContractFollowupSnapshot{
		ReceiptEvents: func() (uint64, <-chan struct{}) {
			mu.Lock()
			defer mu.Unlock()
			return events, changed
		},
		Scan: func(ctx context.Context, paths []string) (ContractFollowupScan, error) {
			mu.Lock()
			defer mu.Unlock()
			stamps := make(map[string]ContractReceiptStamp, len(paths))
			for _, path := range paths {
				stamps[path] = ContractReceiptStamp{Seq: 1, Writing: open[path]}
			}
			return ContractFollowupScan{Settled: true, Stamps: stamps}, ctx.Err()
		},
	}
	wait := func(ctx context.Context, _ uint64, until time.Time) bool {
		select {
		case <-ctx.Done():
		case <-time.After(time.Until(until)):
		}
		return false
	}
	c := &contractConvergence{ctx: t.Context(), snap: snap, req: ContractFollowupRequest{WaitPublished: wait, PublishedEpoch: func() uint64 { return 0 }}}
	c.markRound()
	c.block(first, ContractReceiptStamp{Seq: 1, Writing: true})
	c.block(second, ContractReceiptStamp{Seq: 1, Writing: true})
	since := c.wake()
	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- c.waitWake(since, start.Add(2*testTimingSlack(time.Second))) }()
	time.Sleep(50 * time.Millisecond)
	// Only the second window closes.
	mu.Lock()
	events++
	open[second] = false
	close(changed)
	changed = make(chan struct{})
	mu.Unlock()
	require.True(t, <-done, "the close of one window woke the wait")
	require.Less(t, time.Since(start), testTimingSlack(time.Second), "the wait did not wait for the other window")
}

// TestContractJobPublishesWhileEveryCloseIsFollowedAtOnceByTheNextBegin: the
// route's constant owner is inside an apply's window from before the job's
// first scan. At each of the first four waits the window closes and the next
// apply begins before the waiter's scan of it, as an overtaken pipeline whose
// next apply is queued does; the fifth close is not followed by another
// begin. Every wait saw the window it waited on close, so no round waited out
// and the job publishes instead of ending with unsettled reads.
func TestContractJobPublishesWhileEveryCloseIsFollowedAtOnceByTheNextBegin(t *testing.T) {
	f := newRouteConstantFixture(t)
	const owner = "fixture/consts.go"
	const reopenings = 4
	var armed atomic.Bool
	var stageErr atomic.Value
	stage := func() {
		ctx := context.Background()
		row, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", owner)
		if err == nil && row != nil {
			staged := *row
			staged.Accepted = false
			staged.Previous = nil
			err = f.store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{staged})
		}
		if err != nil {
			stageErr.Store(err)
		}
	}
	staged := false
	var reopened atomic.Int32
	f.adjust = func(snap *ContractFollowupSnapshot) {
		if !staged {
			staged = true
			f.stageInFlight(owner)
		}
		scan := snap.Scan
		snap.Scan = func(ctx context.Context, paths []string) (ContractFollowupScan, error) {
			if slices.Contains(paths, owner) && armed.CompareAndSwap(true, false) {
				reopened.Add(1)
				stage()
			}
			return scan(ctx, paths)
		}
	}
	waits := 0
	f.onWait = func() {
		waits++
		f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
		if waits <= reopenings {
			armed.Store(true)
		}
	}
	hooks := &contractConvergenceHooks{roundWait: 300 * time.Millisecond}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	t.Logf("waits=%d reopened=%d", waits, reopened.Load())
	require.Nil(t, stageErr.Load())
	require.NoError(t, err, "every wait saw the window it waited on close")
	require.True(t, report.Published)
	require.Positive(t, reopened.Load(), "a close was followed at once by the next begin")
}

// TestContractConvergenceWaitWithoutAPublishedSourceSleepsItsDeadline: with
// no Published source nothing can wake a wait: it sleeps until its deadline
// and reports that it waited out, so a job over a frozen snapshot counts its
// rounds and ends instead of retrying at once forever.
func TestContractConvergenceWaitWithoutAPublishedSourceSleepsItsDeadline(t *testing.T) {
	c := &contractConvergence{ctx: t.Context()}
	const wait = 150 * time.Millisecond
	start := time.Now()
	done := make(chan bool, 1)
	go func() { done <- c.waitWake(contractWake{}, start.Add(wait)) }()
	select {
	case woke := <-done:
		require.False(t, woke, "nothing woke the wait")
		require.GreaterOrEqual(t, time.Since(start), wait, "the wait slept until its deadline")
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never returned")
	}
}

// TestContractJobEnrichmentRecheckTornByAnEvictBegunDuringItsAsksIsAskedAgain:
// the enrichment round read the route file while a later apply had it staged,
// so its reads are re-checked. Once that window closed, the re-check's R0
// finds every input quiescent, and a further apply evicts the route file while
// the re-check asks: its answers differ and R1 sees the move. A torn answer is
// asked again, never acted on: no rerun, and the publish equals a cold build.
func TestContractJobEnrichmentRecheckTornByAnEvictBegunDuringItsAsksIsAskedAgain(t *testing.T) {
	f := newRouteConstantFixture(t)
	armed, openings, waits := false, 0, 0
	var release func()
	f.onWait = func() {
		waits++
		switch waits {
		case 1:
			path := "fixture/routes.go"
			f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{path}))
		case 2:
			if release != nil {
				release()
				release = nil
			}
		}
	}
	hooks := &contractConvergenceHooks{
		beforeR1: func([]string) { armed = true },
		openedEvidence: func(*store_sqlite.Store) {
			if !armed {
				return
			}
			openings++
			switch openings {
			case 1:
				f.stageInFlight("fixture/routes.go")
			case 2:
				release = f.beginHeld("fixture/routes.go")
			}
		},
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	if release != nil {
		release()
	}
	require.NoError(t, err)
	require.True(t, report.Published)
	require.GreaterOrEqual(t, openings, 2)
	require.Zero(t, report.EnrichReruns, "an answer torn by an evict that began during the asks is asked again")
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractIDs(f.contracts()))
}

// TestContractJobEnrichmentReadOfAFileInFlightAtItsR0IsTorn: the file
// declaring the handler only the enrichment round resolves is between its
// evict and its add from right after the units are proven (before the round's
// R0) until the job's first wait. The round read it in flight; an unchanged
// stamp at R1 must not make its reads current. The publish equals a cold
// build.
func TestContractJobEnrichmentReadOfAFileInFlightAtItsR0IsTorn(t *testing.T) {
	routes := "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /items\", h.serveItems) }\n"
	serve := "package fixture\nimport \"net/http\"\ntype H struct{}\nfunc (H) serveItems(w http.ResponseWriter, r *http.Request) {}\n"
	files := map[string]string{"routes.go": routes, "serve.go": serve}
	f := newContractJobFixture(t, files)
	var release func()
	proven := 0
	hooks := &contractConvergenceHooks{
		roundWait: 300 * time.Millisecond,
		proven: func(*contractUnit) {
			if proven++; proven == 2 {
				release = f.beginHeld("fixture/serve.go")
			}
		},
	}
	f.onWait = func() {
		if release != nil {
			r := release
			release = nil
			r()
		}
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	if release != nil {
		release()
	}
	require.NoError(t, err)
	require.True(t, report.Published)
	want := coldContracts(t, files)
	got := f.contracts()
	require.Equal(t, contractIDs(want), contractIDs(got))
	require.Equal(t, contractMeta(want), contractMeta(got))
}

// TestContractJobUnitAnswerTornDuringItsAsksIsAskedAgain: the route read its
// constant owner while a later apply had the owner staged, so the route is
// revalidated. Once that window closed, the re-ask's opening scan finds the
// owner quiescent, and a further apply evicts the owner while the re-ask
// asks: the lookup comes back empty and the closing scan sees the move. A
// torn answer is asked again, never acted on: the route is not extracted
// again, and once that apply completes its re-ask proves it.
func TestContractJobUnitAnswerTornDuringItsAsksIsAskedAgain(t *testing.T) {
	f := newRouteConstantFixture(t)
	const owner = "fixture/consts.go"
	staged, evicted := false, false
	extractions, changed := 0, 0
	var release func()
	f.onWait = func() {
		switch {
		case staged && !evicted:
			f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
		case release != nil:
			release()
			release = nil
		}
	}
	hooks := &contractConvergenceHooks{
		afterExtract: func(path string) {
			if path != "fixture/routes.go" {
				return
			}
			extractions++
			if !staged {
				staged = true
				f.stageInFlight(owner)
			}
		},
		beforeAsk: func() {
			if staged && !evicted && !f.store.ContractReceiptWriteStates("fixture", "", []string{owner})[owner].Writing {
				evicted = true
				release = f.beginHeld(owner)
			}
		},
		revalidated: func(path string, equal bool) {
			if path == "fixture/routes.go" && !equal {
				changed++
			}
		},
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	if release != nil {
		release()
	}
	require.NoError(t, err)
	require.True(t, report.Published)
	require.True(t, evicted, "an apply evicted the owner while the route was asked again")
	require.Positive(t, changed, "the torn answer differed from the recorded one")
	require.Equal(t, 1, extractions, "a torn answer is asked again, not extracted")
	require.Equal(t, contractMeta(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractMeta(f.contracts()))
}

// TestContractWatcherPatchThatFailsKeepsItsWindowOpen: a watcher patch that
// reports failure after staging its file's receipt leaves the file's
// core-write window open: nothing says its core writes are complete, so a
// contract job never reads its rows as if they were.
func TestContractWatcherPatchThatFailsKeepsItsWindowOpen(t *testing.T) {
	f := newRouteConstantFixture(t)
	f.mi.mu.RLock()
	idx := f.mi.indexers["fixture"]
	f.mi.mu.RUnlock()
	require.NotNil(t, idx)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, Paths: []string{idx.rootPath}, DebounceMs: 50}, zap.NewNop())
	require.NoError(t, err)
	path := filepath.Join(idx.rootPath, "consts.go")
	const generation = 7
	w.pointReindexRaw = func(p string, _ *PublicationStamps) (*IndexResult, error) {
		if _, err := idx.incrementalPointWatcherPath(idx.rootPath, p); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("injected patch failure")
	}
	writeFile(t, path, jobConsts+"// a failed patch\n")
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, future, future))
	w.mu.Lock()
	w.pendingGeneration[path] = generation
	w.mu.Unlock()
	require.Error(t, w.patchGraph(path, ChangeModified, generation))
	stamps, err := newFixtureScanner(t, f).scan(t.Context(), []string{"fixture/consts.go"})
	require.NoError(t, err)
	stamp := stamps.Stamps["fixture/consts.go"]
	require.Positive(t, stamp.Seq, "the patch staged the file's receipt")
	require.False(t, stamp.Accepted || stamp.Settled, "a failed patch is neither accepted nor settled")
	require.True(t, stamp.Writing, "a failed patch leaves its core-write window open")
	require.False(t, stamp.quiescent())
}

// TestContractMultiIndexerLaneSettlesOnlyACompletedOvertakenBody: the
// repository lane the MultiIndexer binds (its reconcile door) ends its
// mutations as the Indexer's own lane does. A body that completed every core
// write and that a newer mutation overtook is never accepted, closes its
// file's core-write window and settles it; a completed body whose Complete
// is refused for another reason (its source witness moved) closes the window
// and settles nothing; a body that failed after its writes settles nothing
// and leaves the window open.
func TestContractMultiIndexerLaneSettlesOnlyACompletedOvertakenBody(t *testing.T) {
	for _, row := range []struct {
		name          string
		fail, refused bool
	}{{"overtaken_after_its_writes", false, false}, {"refused_complete", false, true}, {"failed_after_its_writes", true, false}} {
		t.Run(row.name, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			f.mi.mu.RLock()
			idx := f.mi.indexers["fixture"]
			f.mi.mu.RUnlock()
			require.NotNil(t, idx)
			var leases *graphview.LeaseManager
			if row.refused {
				leases = graphview.NewLeaseManager()
				registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: "fixture", RootIdentity: f.mi.repoRootPath("fixture"), Incarnation: t.Name()}, nil)
				require.NoError(t, err)
				_, err = leases.CaptureInitialRawRepositorySource(t.Context(), registration, "accepted-source")
				require.NoError(t, err)
				f.mi.SetOutputGenerationAuthority(NewOutputGenerationAuthority(leases))
			}
			authority := f.mi.outputGenerationAuthority()
			target := legacyOutputTargetFor(outputStoreIdentity(f.mi.graph), "fixture", f.mi.repoRootPath("fixture"), "prefix:fixture")
			path := filepath.Join(f.root, "consts.go")
			writeFile(t, path, jobConsts+"// through the repository lane\n")
			future := time.Now().Add(time.Minute)
			require.NoError(t, os.Chtimes(path, future, future))
			newer := make(chan *OutputMutationReceipt, 1)
			overtaken := false
			err := f.mi.withRepositoryOutputGenerationSource(t.Context(), "fixture", OutputEntryRepositoryReconcileLane, func(func(*OutputSourceContent)) error {
				if _, err := idx.incrementalPointWatcherPath(idx.rootPath, path); err != nil {
					return err
				}
				if row.fail {
					return fmt.Errorf("injected body failure")
				}
				if row.refused {
					// The source witness moves under the completed body.
					_, err := leases.InvalidateBaseCorpusSource("fixture")
					return err
				}
				// A newer mutation of the same owner is admitted as the body
				// ends, before Complete.
				overtaken = true
				issued := authority.Stats().Issued
				go func() {
					receipt, err := authority.Begin(context.Background(), OutputEntryIndexFile, target)
					if err != nil {
						receipt = nil
					}
					newer <- receipt
				}()
				for deadline := time.Now().Add(10 * time.Second); authority.Stats().Issued == issued && time.Now().Before(deadline); {
					time.Sleep(time.Millisecond)
				}
				return nil
			})
			if overtaken {
				if receipt := <-newer; receipt != nil {
					receipt.Abandon()
				}
			}
			require.Error(t, err)
			stamps, scanErr := newFixtureScanner(t, f).scan(t.Context(), []string{"fixture/consts.go"})
			require.NoError(t, scanErr)
			stamp := stamps.Stamps["fixture/consts.go"]
			require.Positive(t, stamp.Seq, "the body staged the file's receipt")
			require.False(t, stamp.Accepted, "the lane accepted nothing")
			if row.fail {
				require.False(t, stamp.Settled, "a failed body settles nothing")
				require.True(t, stamp.Writing, "a failed body leaves its window open")
				return
			}
			if row.refused {
				require.ErrorIs(t, err, graphview.ErrRawRepositorySourceChanged, "Complete refused the body")
				require.NotErrorIs(t, err, ErrOutputMutationReceiptSuperseded)
				require.False(t, stamp.Settled, "a refused Complete settles nothing")
				require.False(t, stamp.Writing, "the completed body closed its window")
				require.True(t, stamp.quiescent())
				return
			}
			require.ErrorIs(t, err, ErrOutputMutationReceiptSuperseded)
			require.True(t, stamp.Settled, "the overtaken body settled its receipt")
			require.False(t, stamp.Writing, "the completed body closed its window")
			require.True(t, stamp.quiescent())
		})
	}
}

// TestContractJobEnrichmentFailureNoMovingReadExplainsIsFailed: the
// enrichment round fails deterministically (its seed read refuses with the
// projection limit) while an apply of a file the round never read holds that
// file's core-write window open. Only a file the round read can make its
// failure a torn read to retry: the job ends failed, not superseded.
func TestContractJobEnrichmentFailureNoMovingReadExplainsIsFailed(t *testing.T) {
	f := newRouteConstantFixture(t)
	f.adjust = func(snapshot *ContractFollowupSnapshot) {
		snapshot.Seeds = func(context.Context, string) ([]*graph.Node, []*graph.Edge, bool, error) {
			return nil, nil, false, fmt.Errorf("%w: injected seed refusal", graph.ErrContractProjectionLimit)
		}
	}
	armed, staged := false, false
	hooks := &contractConvergenceHooks{
		roundWait: 200 * time.Millisecond,
		beforeR1:  func([]string) { armed = true },
		// The first evidence after the units' round is the enrichment
		// round's, after its R0.
		openedEvidence: func(*store_sqlite.Store) {
			if armed && !staged {
				staged = true
				f.stageInFlight("fixture/helper.go")
			}
		},
	}
	report, err := f.job(hooks, nil)
	require.True(t, staged)
	require.False(t, report.Published)
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.Equal(t, viewmetrics.ContractJobFailed, contractJobOutcomeOf(err), "job error: %v", err)
}

// TestContractMutationClosesAnAcceptedSavesWindowWithItsAccept: a save whose
// body completes and whose Complete holds is accepted, and its accept closes
// the file's core-write window as it commits. Both lanes keep the window open
// until then: when the accept's receipt write begins, the window the staging
// opened is still open, so a contract job woken by the close sees the
// accepted row and never the row the accept is about to rewrite.
func TestContractMutationClosesAnAcceptedSavesWindowWithItsAccept(t *testing.T) {
	for _, lane := range []string{"indexer", "multi_indexer"} {
		t.Run(lane, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			f.mi.mu.RLock()
			idx := f.mi.indexers["fixture"]
			f.mi.mu.RUnlock()
			require.NotNil(t, idx)
			const file = "fixture/consts.go"
			var mu sync.Mutex
			var open []bool
			restore := store_sqlite.SetContractReceiptWriteHookForTest(func(stage string, paths []string) {
				if stage != "begun" || !slices.Contains(paths, file) {
					return
				}
				state := f.store.ContractReceiptWriteStates("fixture", "", []string{file})[file]
				mu.Lock()
				open = append(open, state.Writing)
				mu.Unlock()
			})
			path := filepath.Join(f.root, "consts.go")
			writeFile(t, path, jobConsts+"// an accepted save\n")
			future := time.Now().Add(time.Minute)
			require.NoError(t, os.Chtimes(path, future, future))
			var err error
			if lane == "indexer" {
				_, err = f.mi.IncrementalReindexRepo("fixture", []string{path})
			} else {
				err = f.mi.withRepositoryOutputGenerationSource(t.Context(), "fixture", OutputEntryRepositoryReconcileLane, func(func(*OutputSourceContent)) error {
					_, err := idx.incrementalPointWatcherPath(idx.rootPath, path)
					return err
				})
			}
			restore()
			require.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			require.GreaterOrEqual(t, len(open), 2, "the save staged its receipt and accepted it")
			require.NotContains(t, open, false, "every receipt write of the save, its accept included, began inside the window the staging opened")
			stamps, scanErr := newFixtureScanner(t, f).scan(t.Context(), []string{file})
			require.NoError(t, scanErr)
			stamp := stamps.Stamps[file]
			require.True(t, stamp.Accepted, "the save was accepted")
			require.False(t, stamp.Writing, "the accept closed the window")
			require.True(t, stamp.quiescent())
		})
	}
}

// TestContractMutationWhoseAcceptFailsClosesItsWindow: a save whose body
// completed every core write and whose Complete held, but whose accept then
// failed (its lane's context ended), is never accepted; its rows are complete
// all the same, so both lanes close its file's core-write window after the
// failed accept instead of leaving it open until the next save of the file.
func TestContractMutationWhoseAcceptFailsClosesItsWindow(t *testing.T) {
	for _, lane := range []string{"indexer", "multi_indexer"} {
		t.Run(lane, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			f.mi.mu.RLock()
			idx := f.mi.indexers["fixture"]
			f.mi.mu.RUnlock()
			require.NotNil(t, idx)
			const file = "fixture/consts.go"
			path := filepath.Join(f.root, "consts.go")
			writeFile(t, path, jobConsts+"// a save whose accept fails\n")
			future := time.Now().Add(time.Minute)
			require.NoError(t, os.Chtimes(path, future, future))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := func(func(*OutputSourceContent)) error {
				_, err := idx.incrementalPointWatcherPath(idx.rootPath, path)
				// The lane's context ends after the body's writes.
				cancel()
				return err
			}
			var err error
			if lane == "indexer" {
				err = idx.withOutputGenerationSource(ctx, OutputEntryRepositoryReconcileLane, body)
			} else {
				err = f.mi.withRepositoryOutputGenerationSource(ctx, "fixture", OutputEntryRepositoryReconcileLane, body)
			}
			require.ErrorIs(t, err, context.Canceled, "the accept failed")
			stamps, scanErr := newFixtureScanner(t, f).scan(t.Context(), []string{file})
			require.NoError(t, scanErr)
			stamp := stamps.Stamps[file]
			require.Positive(t, stamp.Seq, "the body staged the file's receipt")
			require.False(t, stamp.Accepted || stamp.Settled, "a failed accept accepts and settles nothing")
			require.False(t, stamp.Writing, "the completed body's window closed after the failed accept")
			require.True(t, stamp.quiescent())
		})
	}
}

// TestContractJobRoundWokenByAnApplysCloseAsksOnce: the route's constant
// owner is inside an apply's open core-write window from before the job's
// first scan. At the first wait the apply ends. When the apply ends as the
// lanes end an accepted save, its accept closes the window as it commits: the
// round the close wakes finds the final accepted row and asks once. When the
// window closed before the accept, the accept rewrites the row while the woken
// round asks; it touches no core row, but the round sees its stamp move, so the
// round's answers prove nothing and it is asked again.
func TestContractJobRoundWokenByAnApplysCloseAsksOnce(t *testing.T) {
	for _, row := range []struct {
		name string
		// closedFirst closes the window at the wait and accepts while the
		// woken round asks.
		closedFirst bool
		asks, woke  int
	}{{"accept_closes", false, 1, 1}, {"closed_before_the_accept", true, 2, 2}} {
		t.Run(row.name, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			const owner = "fixture/consts.go"
			staged := false
			f.adjust = func(*ContractFollowupSnapshot) {
				if !staged {
					staged = true
					f.stageInFlight(owner)
				}
			}
			accept := func() (bool, error) {
				ctx := context.Background()
				current, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", owner)
				if err != nil || current == nil || current.Accepted {
					return false, err
				}
				return true, f.store.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{*current})
			}
			waits, woke, asks, accepts := 0, 0, 0, 0
			armed := false
			var acceptErr error
			f.onWait = func() {
				if waits++; waits != 1 {
					return
				}
				if row.closedFirst {
					f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
					armed = true
					return
				}
				accepted, err := accept()
				if accepted {
					accepts++
				}
				acceptErr = err
			}
			hooks := &contractConvergenceHooks{
				roundWait: 300 * time.Millisecond,
				woke:      func() { woke++ },
				beforeAsk: func() {
					asks++
					if !armed {
						return
					}
					armed = false
					accepted, err := accept()
					if accepted {
						accepts++
					}
					acceptErr = err
				},
			}
			report, err := f.job(hooks, contractJobDeadline(time.Minute))
			f.onWait = nil
			t.Logf("waits=%d woke=%d asks=%d accepts=%d rounds=%d revalidated=%d", waits, woke, asks, accepts, report.Rounds, report.Revalidated)
			require.NoError(t, acceptErr)
			require.NoError(t, err)
			require.True(t, report.Published)
			require.Equal(t, 1, accepts, "the apply was accepted once")
			require.Equal(t, row.asks, asks)
			require.Equal(t, row.woke, woke)
		})
	}
}

// TestContractConvergenceWaitWakesWhenAWaitedOnRowDeparts: a wait armed on an
// open core-write window has nothing left to wait on once its path loses its
// receipt row (the file was deleted, or a generation above claims it), or once
// the attachment key it builds moved: it wakes on that event instead of
// sleeping to its deadline, though no window it waited on closed.
func TestContractConvergenceWaitWakesWhenAWaitedOnRowDeparts(t *testing.T) {
	const path = "fixture/consts.go"
	for _, row := range []struct {
		name            string
		departs, unkeys bool
	}{{"row_departed", true, false}, {"key_moved", false, true}} {
		t.Run(row.name, func(t *testing.T) {
			var mu sync.Mutex
			events, present, settled := uint64(0), true, true
			changed := make(chan struct{})
			snap := ContractFollowupSnapshot{
				ReceiptEvents: func() (uint64, <-chan struct{}) {
					mu.Lock()
					defer mu.Unlock()
					return events, changed
				},
				Scan: func(ctx context.Context, paths []string) (ContractFollowupScan, error) {
					mu.Lock()
					defer mu.Unlock()
					stamps := make(map[string]ContractReceiptStamp)
					if present {
						stamps[path] = ContractReceiptStamp{Seq: 1, Writing: true}
					}
					return ContractFollowupScan{Settled: settled, Stamps: stamps}, ctx.Err()
				},
			}
			wait := func(ctx context.Context, _ uint64, until time.Time) bool {
				select {
				case <-ctx.Done():
				case <-time.After(time.Until(until)):
				}
				return false
			}
			c := &contractConvergence{ctx: t.Context(), snap: snap, req: ContractFollowupRequest{WaitPublished: wait, PublishedEpoch: func() uint64 { return 0 }}}
			c.markRound()
			c.block(path, ContractReceiptStamp{Seq: 1, Writing: true})
			since := c.wake()
			done := make(chan bool, 1)
			start := time.Now()
			go func() { done <- c.waitWake(since, start.Add(2*testTimingSlack(time.Second))) }()
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			events++
			present = !row.departs
			settled = !row.unkeys
			close(changed)
			changed = make(chan struct{})
			mu.Unlock()
			require.True(t, <-done, "nothing is left to wait on")
			require.Less(t, time.Since(start), testTimingSlack(time.Second), "the wait woke on the event, not at its deadline")
		})
	}
}

// TestContractStampChangesNamesEveryDifferingField: the retry line's
// moved-fields record names each stamp field that differs between the scans
// around a read, and "gone" for a row that departed, so a round a stamp move
// blocked shows what moved (an accept, a settle, a new write, a window).
func TestContractStampChangesNamesEveryDifferingField(t *testing.T) {
	staged := ContractReceiptStamp{Generation: 0, Actor: "", Fingerprint: "f", SourceFingerprint: "s", Seq: 3}
	accepted := staged
	accepted.Accepted, accepted.Seq = true, 4
	settled := staged
	settled.Settled = true
	next := staged
	next.Seq, next.Writing = 4, true
	rewritten := staged
	rewritten.Generation, rewritten.Actor, rewritten.Fingerprint, rewritten.SourceFingerprint = 2, "wt", "g", "t"
	require.Equal(t, []string{"accepted", "seq"}, contractStampChanges(staged, accepted, true))
	require.Equal(t, []string{"settled"}, contractStampChanges(staged, settled, true))
	require.Equal(t, []string{"seq", "writing"}, contractStampChanges(staged, next, true))
	require.Equal(t, []string{"generation", "actor", "fingerprint", "source"}, contractStampChanges(staged, rewritten, true))
	require.Equal(t, []string{"gone"}, contractStampChanges(staged, ContractReceiptStamp{}, false))
	require.Empty(t, contractStampChanges(staged, staged, true))
	var stats contractRevalidationStats
	stats.noteMoved(staged, accepted, true)
	stats.noteMoved(staged, next, true)
	for _, field := range stats.fields() {
		if field.Key == "revalidate_moved_fields" {
			require.Equal(t, "accepted:1,seq:2,writing:1", field.String)
			return
		}
	}
	t.Fatal("the retry line carries no moved-fields record")
}
