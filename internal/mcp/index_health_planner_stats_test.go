package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// index_health is where an agent looks before trusting a whole-workspace
// answer, and stale query-planner statistics are invisible from every other
// signal: the graph is complete, the counts are right, and the queries simply
// pick the wrong join order. It is also the one place that must NOT fix them —
// the payload is served from a cached snapshot rebuilt in the background, so a
// probe with an ANALYZE inside it would make observing the store change it.

func newSQLiteBackedServer(t *testing.T) (*Server, *store_sqlite.Store) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

type Config struct {
	Port int
}

func (c Config) Addr() string { return "" }

func main() { helper() }

func helper() {}
`), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "health.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	_, err = idx.Index(dir)
	require.NoError(t, err)

	return NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil), store
}

// newEmptySQLiteBackedServer is the same fixture with no index pass. A
// coordinated bulk window engages only on an empty store, and that window is
// the one verdict a health probe can report that is not staleness — so it is
// the only way to reach a non-stale reason from this side of the API.
func newEmptySQLiteBackedServer(t *testing.T) (*Server, *store_sqlite.Store) {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "empty-health.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	return NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil), store
}

// seedGoTypes adds n Go type nodes, which is what the partial receiver index
// is defined over. Enough of them and the bounded probe cannot count the index
// in full, which is the state `complete` exists to report.
func seedGoTypes(store *store_sqlite.Store, n int) {
	seedGoTypesIn(store, "seeded", n)
}

// seedGoTypesIn is the same seed under a namespace, so a test can grow a store
// by adding a second disjoint batch rather than rewriting the first.
func seedGoTypesIn(store *store_sqlite.Store, namespace string, n int) {
	nodes := make([]*graph.Node, 0, n)
	for i := 0; i < n; i++ {
		file := fmt.Sprintf("%s/p%03d/types.go", namespace, i)
		name := fmt.Sprintf("%s_S%03d", namespace, i)
		nodes = append(nodes, &graph.Node{
			ID: file + "::" + name, Name: name, Kind: graph.KindType,
			FilePath: file, Language: "go",
		})
	}
	store.AddBatch(nodes, nil)
}

// newUnanchoredStatsServer is a SQLite-backed server whose nodes family carries
// believed cardinalities but NO growth anchor — the regime in which a verdict is
// judged against the believed row, and therefore the only one in which the held
// base exists at all.
//
// Getting there means populating the store and REOPENING it. The Open-time
// repair is what writes sqlite_stat1 for a populated store that has none, and it
// seeds the growth anchor only from repo_index_state counters — which this store
// does not have yet. Indexing instead would finalize a cold load, and that stamp
// anchors both families from the counters it just wrote, leaving nothing
// unanchored to hold a base for.
func newUnanchoredStatsServer(t *testing.T, types int) (*Server, *store_sqlite.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unanchored-health.sqlite")

	seed, err := store_sqlite.Open(path)
	require.NoError(t, err)
	seedGoTypesIn(seed, "base", types)
	require.NoError(t, seed.Close())

	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	return NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil), store
}

func TestIndexHealth_SurfacesPlannerStats(t *testing.T) {
	srv, store := newSQLiteBackedServer(t)

	payload, err := srv.buildIndexHealthPayloadCtx(context.Background())
	require.NoError(t, err)
	require.NotNil(t, payload)

	plannerStats, ok := payload["planner_stats"].(map[string]any)
	require.True(t, ok, "a SQLite-backed daemon must report planner statistics in index_health")
	assert.Equal(t, false, plannerStats["stale"])
	assert.NotContains(t, plannerStats, "reason", "a fresh store owes no staleness reason")

	nodes, ok := plannerStats["nodes"].(map[string]any)
	require.True(t, ok)
	assert.Positive(t, nodes["believed"], "the planner believes nothing about a freshly indexed store")
	assert.Contains(t, nodes, "actual_from_counters",
		"the counter sum must be named for what it is, not presented as a measured cardinality")

	receivers, ok := plannerStats["receivers"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, receivers["bounded"], "the receiver figure is a LIMIT-bounded probe")
	assert.Equal(t, true, receivers["complete"],
		"a probe that read the whole receiver index must say the figure is comparable to `believed`")

	t.Run("stale is surfaced and never repaired", func(t *testing.T) {
		before, err := store.PlannerStatsHealth(context.Background())
		require.NoError(t, err)

		// Counters that claim far more than the statistics believe: the shape
		// a daemon reaches by tracking more repositories into a live store.
		require.NoError(t, store.SetRepoIndexState(graph.RepoIndexState{
			RepoPrefix: "", NodeCount: 5_000_000, EdgeCount: 20_000_000,
		}))

		stalePayload, err := srv.buildIndexHealthPayloadCtx(context.Background())
		require.NoError(t, err)
		staleStats, ok := stalePayload["planner_stats"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, staleStats["stale"])
		assert.NotEmpty(t, staleStats["reason"])

		recommendation, _ := stalePayload["recommendation"].(string)
		assert.Contains(t, recommendation, "planner statistics are stale",
			"a stale verdict must come with the sentence that explains what it costs")

		after, err := store.PlannerStatsHealth(context.Background())
		require.NoError(t, err)
		assert.Equal(t, before.Refreshes, after.Refreshes,
			"building the health payload rebuilt the statistics; a report must not mutate what it reports on")
		assert.Equal(t, before.LastRefreshAt, after.LastRefreshAt,
			"building the health payload stamped the refresh ledger")
	})

	// A verdict that is not staleness still explains the numbers under it. In
	// a bulk window every figure reads zero because the droppable critical
	// indexes are physically gone; without the reason the payload looks like a
	// store whose planner believes nothing at all.
	t.Run("a non-stale verdict still carries its reason", func(t *testing.T) {
		bulkSrv, bulkStore := newEmptySQLiteBackedServer(t)
		require.True(t, bulkStore.BeginCoordinatedBulkLoad(),
			"coordinated bulk load did not engage on an empty on-disk store")
		t.Cleanup(func() { _ = bulkStore.EndCoordinatedBulkLoad() })

		payload, err := bulkSrv.buildIndexHealthPayloadCtx(context.Background())
		require.NoError(t, err)
		stats, ok := payload["planner_stats"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, false, stats["stale"])
		assert.Equal(t, "bulk_window_active", stats["reason"],
			"a cold load in progress was reported as a store with no verdict at all")

		recommendation, _ := payload["recommendation"].(string)
		assert.NotContains(t, recommendation, "planner statistics are stale",
			"the recommendation is owed by a stale verdict, not by every reason")

		// The receiver index is one of the ones a bulk window drops. Reporting
		// believed=0 / actual=0 for it would read as the poisoned near-zero
		// state issue #651 is about, on a perfectly healthy cold load.
		assert.NotContains(t, stats, "receivers",
			"published a receiver cardinality for an index that is not in the schema")
	})

	// Dense indexes an interrupted bulk load left out are named, with the
	// sentence that says what they cost and how they come back. The real
	// state repairs itself on the store's maintenance lane as soon as the
	// volume has room, so the verdict is fixed here rather than raced.
	t.Run("missing dense indexes are named", func(t *testing.T) {
		_, store := newEmptySQLiteBackedServer(t)
		missing := []string{"nodes_by_kind", "edges_by_kind"}
		wrapped := plannerHealthOverride{Store: store, health: graph.PlannerStatsFreshness{
			Reason: "dense_indexes_missing", DenseIndexesMissing: missing,
		}}
		idx := indexer.New(wrapped, testRegistry(), config.Default().Index, zap.NewNop())
		missingSrv := NewServer(query.NewEngine(wrapped), wrapped, idx, nil, zap.NewNop(), nil)

		payload, err := missingSrv.buildIndexHealthPayloadCtx(context.Background())
		require.NoError(t, err)
		stats, ok := payload["planner_stats"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "dense_indexes_missing", stats["reason"])
		assert.Equal(t, missing, stats["dense_indexes_missing"])
		recommendation, _ := payload["recommendation"].(string)
		assert.Contains(t, recommendation, "nodes_by_kind, edges_by_kind")
		assert.NotContains(t, recommendation, "planner statistics are stale")
	})

	// The window between a deferred cooperative pass and the boundary that
	// finishes it, seen from the side an agent reads.
	//
	// A family with no growth anchor is judged against its BELIEVED row, and the
	// pass rebuilds the index that row is read off FIRST — so from the deferral
	// on, the row agrees with the store while the family's other indexes are
	// still frozen at their pre-growth figures. The store keeps the verdict
	// standing through that window by judging the family against the base the
	// verdict actually fired on, carried on the pass's cursor. index_health is
	// where an agent decides whether to trust a whole-workspace answer, so a
	// payload that went quiet mid-pass would say the join orders are sound while
	// six of the seven nodes indexes still misdescribe the store — and nothing
	// else in the payload would hint otherwise.
	t.Run("a deferred pass keeps the verdict visible until it completes", func(t *testing.T) {
		heldSrv, heldStore := newUnanchoredStatsServer(t, 100)

		before, err := heldStore.PlannerStatsHealth(context.Background())
		require.NoError(t, err)
		require.False(t, before.Stale, "fixture was already stale: %s", before.Reason)
		require.Positive(t, before.Nodes.Believed,
			"the reopened store has no believed cardinality; the Open-time repair did not run and there "+
				"is no unanchored growth verdict to defer")

		// Double the corpus and write the counters that describe it: a growth
		// verdict on a family that has never been anchored.
		seedGoTypesIn(heldStore, "grown", 100)
		require.NoError(t, heldStore.SetRepoIndexState(graph.RepoIndexState{
			RepoPrefix: "", NodeCount: 200, EdgeCount: 0,
		}))

		// A zero budget stops the pass after its first index — the one the
		// verdict was read off — which is exactly the state the held base
		// exists for.
		restoreBudget := store_sqlite.SetPlannerStatsPassBudgetForTest(0)
		deferred, err := heldStore.EnsurePlannerStatsFresh(context.Background())
		restoreBudget()
		require.NoError(t, err)
		require.False(t, deferred.Refreshed, "the pass completed instead of deferring: %s", deferred.Reason)
		require.True(t, strings.HasPrefix(deferred.Reason, "budget:"),
			"the pass stopped for some other reason (%s); the window this subtest is about may not exist",
			deferred.Reason)

		payload, err := heldSrv.buildIndexHealthPayloadCtx(context.Background())
		require.NoError(t, err)
		stats, ok := payload["planner_stats"].(map[string]any)
		require.True(t, ok)
		nodes, ok := stats["nodes"].(map[string]any)
		require.True(t, ok)
		believed, ok := nodes["believed"].(int64)
		require.True(t, ok)
		require.Greater(t, believed, before.Nodes.Believed,
			"the deferred pass did not rewrite the row its verdict was read off, so nothing here needs a "+
				"held base and this subtest proves nothing")

		assert.Equal(t, true, stats["stale"],
			"the payload went quiet while six of the seven nodes indexes still describe the store before "+
				"it doubled; an agent reading this trusts join orders the pass has not repaired yet")
		reason, _ := stats["reason"].(string)
		assert.Contains(t, reason, fmt.Sprintf("base=%d", before.Nodes.Believed),
			"the verdict was re-judged against the row the deferred pass had just rewritten rather than "+
				"against the base it fired on")

		// And the window closes on its own: the next boundary resumes the pass,
		// finishes the family, and the payload goes quiet because the store
		// really is described again.
		completed, err := heldStore.EnsurePlannerStatsFresh(context.Background())
		require.NoError(t, err)
		require.True(t, completed.Refreshed, "the resumed pass did not complete: %s", completed.Reason)

		settled, err := heldSrv.buildIndexHealthPayloadCtx(context.Background())
		require.NoError(t, err)
		settledStats, ok := settled["planner_stats"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, false, settledStats["stale"],
			"the completed pass anchored the family at the size it analyzed, so the verdict must be gone: %v",
			settledStats["reason"])
	})

	// The receiver probe stops at a cap, so `actual` can be a truncated lower
	// bound. Publishing it next to `believed` without saying which is which
	// invites a reader to compare two numbers that are not comparable.
	t.Run("a truncated receiver probe says so", func(t *testing.T) {
		capSrv, capStore := newSQLiteBackedServer(t)
		seedGoTypes(capStore, 200)
		// Counters far past what the statistics believe, so the refresh that
		// re-reads the grown receiver index actually happens.
		require.NoError(t, capStore.SetRepoIndexState(graph.RepoIndexState{
			RepoPrefix: "seeded", NodeCount: 100_000, EdgeCount: 100_000,
		}))
		refreshed, err := capStore.EnsurePlannerStatsFresh(context.Background())
		require.NoError(t, err)
		require.True(t, refreshed.Refreshed, "fixture did not re-analyze the grown receiver index: %s", refreshed.Reason)

		payload, err := capSrv.buildIndexHealthPayloadCtx(context.Background())
		require.NoError(t, err)
		stats, ok := payload["planner_stats"].(map[string]any)
		require.True(t, ok)
		receivers, ok := stats["receivers"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, receivers["bounded"])
		assert.Equal(t, false, receivers["complete"],
			"the probe stopped at its cap and the payload still presented `actual` as a count")
	})
}

// plannerHealthOverride reports a fixed planner verdict over a real store.
type plannerHealthOverride struct {
	*store_sqlite.Store
	health graph.PlannerStatsFreshness
}

func (s plannerHealthOverride) PlannerStatsHealth(context.Context) (graph.PlannerStatsFreshness, error) {
	return s.health, nil
}

// A backend without the capability must omit the key rather than publish a
// struct of zeros, which would read as "the planner believes nothing".
func TestIndexHealth_PlannerStatsAbsentOnBackendWithoutCapability(t *testing.T) {
	srv, _ := setupTestServer(t)

	payload := srv.buildIndexHealthPayload()
	require.NotNil(t, payload)
	assert.NotContains(t, payload, "planner_stats",
		"the in-memory graph has no planner statistics; reporting zeros would invent a defect")
}

func TestIndexHealthReportsLastFirstIndexDrain(t *testing.T) {
	t.Setenv("GORTEX_STREAMING_FLUSH", "0")
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
	t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
	srv, _ := newSQLiteBackedServer(t)
	payload, err := srv.buildIndexHealthPayloadCtx(t.Context())
	require.NoError(t, err)
	observations, ok := payload["last_first_index_drain"].(map[string]graph.FirstIndexDrain)
	require.True(t, ok, "a completed first index must publish its actual drain observation")
	require.Len(t, observations, 1)
	for _, observation := range observations {
		assert.Equal(t, "cold", observation.Window)
		assert.Greater(t, observation.Nodes, 0)
		assert.GreaterOrEqual(t, observation.DrainMS, observation.SealMS)
		assert.False(t, observation.CompletedAt.IsZero())
		assert.Equal(t, "shared_store_drain_chunk_samples", observation.WALPeakScope)
	}
	empty, _ := newEmptySQLiteBackedServer(t)
	noDrain, err := empty.buildIndexHealthPayloadCtx(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, noDrain, "last_first_index_drain", "absence is not a fabricated completed zero drain")
}
