package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type walObservationSpy struct {
	graph.Store
	stats store_sqlite.WALReclaimStats
	reads atomic.Int64
}

func (s *walObservationSpy) WALReclaimStats() store_sqlite.WALReclaimStats {
	s.reads.Add(1)
	return s.stats
}

func (*walObservationSpy) DBStats() (int64, int64) {
	panic("MCP WAL observations must not stat store files")
}

func (*walObservationSpy) CloseCheckpointEstimate() (time.Duration, int64) {
	panic("MCP WAL observations must not inspect the wal-index")
}

func TestWALObservationSnapshotOnlyNullablePeakAndOwnedValues(t *testing.T) {
	spy := &walObservationSpy{Store: graph.New()}
	s := &Server{graph: spy}
	first := s.walReclaimObservation()
	require.True(t, first.SnapshotAvailable)
	require.EqualValues(t, 1, spy.reads.Load())
	require.EqualValues(t, 0, *first.CancelledWriterParked)
	require.EqualValues(t, 0, *first.CancelledWriteIntent)
	require.EqualValues(t, 0, *first.CancelledOther)
	require.Nil(t, first.ObservedPeakWALBytes, "no observation must not become zero")
	require.False(t, first.WALPeakObserved)
	spy.stats = store_sqlite.WALReclaimStats{
		CancelledWriterParked: 3, CancelledWriteIntent: 2, CancelledOther: 1, WALPeakObserved: true,
	}
	second := s.walReclaimObservation()
	require.EqualValues(t, 2, spy.reads.Load())
	require.True(t, second.WALPeakObserved)
	require.NotNil(t, second.ObservedPeakWALBytes)
	require.EqualValues(t, 0, *second.ObservedPeakWALBytes, "an actually observed empty WAL is valid zero")
	require.EqualValues(t, 3, *second.CancelledWriterParked)
	require.EqualValues(t, 2, *second.CancelledWriteIntent)
	require.EqualValues(t, 1, *second.CancelledOther)
	*second.CancelledWriterParked = 999
	*second.ObservedPeakWALBytes = 999
	third := s.walReclaimObservation()
	require.EqualValues(t, 3, *third.CancelledWriterParked)
	require.Zero(t, *third.ObservedPeakWALBytes, "wire pointers must not mutate retained store data")
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	require.Contains(t, wire, "observed_peak_wal_bytes")
	require.Nil(t, wire["observed_peak_wal_bytes"])
	require.Equal(t, float64(0), wire["cancelled_other"], "supported zero counter remains numeric")
}

func TestWALObservationUnsupportedAndMaskedReadersStayUnavailable(t *testing.T) {
	spy := &walObservationSpy{Store: graph.New()}
	var nilSpy *walObservationSpy
	for _, source := range []graph.Store{nil, graph.New(), struct{ graph.Store }{spy}, nilSpy} {
		got := (&Server{graph: source}).walReclaimObservation()
		require.False(t, got.SnapshotAvailable)
		require.NotEmpty(t, got.Reason)
		require.Nil(t, got.CancelledWriterParked)
		require.Nil(t, got.CancelledWriteIntent)
		require.Nil(t, got.CancelledOther)
		require.Nil(t, got.ObservedPeakWALBytes)
	}
	require.Zero(t, spy.reads.Load(), "masked capabilities must not unwrap to a physical store")
}

func TestWALObservationPhysicalScopeAndCommonStatsGetterCensus(t *testing.T) {
	stack := newViewStack(t)
	spy := &walObservationSpy{Store: stack.store, stats: store_sqlite.WALReclaimStats{
		CancelledWriterParked: 5, CancelledWriteIntent: 4, CancelledOther: 3,
		WALPeakObserved: true, ObservedPeakWALBytes: 123456,
	}}
	stack.srv.graph = spy // same physical store, snapshot spy only
	var resource, tool walReclaimObservation
	_, err := stack.readBoundResource(t, stack.worktreeRoot, "gortex://stats", func(ctx context.Context) ([]mcp.ResourceContents, error) {
		resource = stack.srv.buildGraphStatsPayload(ctx)["wal_reclaim_observation"].(walReclaimObservation)
		return nil, nil
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, spy.reads.Load())
	_, err = stack.callWithView(t, "", "graph_stats", nil, func(ctx context.Context) (*mcp.CallToolResult, error) {
		tool = stack.srv.buildGraphStatsPayload(ctx)["wal_reclaim_observation"].(walReclaimObservation)
		return mcp.NewToolResultText(`{"ok":true}`), nil
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, spy.reads.Load(), "one snapshot per common payload, no duplicate collection")
	require.Equal(t, resource, tool, "a view does not scope physical WAL counters")
	require.Equal(t, "physical_store_since_open", tool.Scope)
	require.Equal(t, "sampled_since_store_open_lower_bound", tool.PeakScope)
	require.Equal(t, "server_binding", tool.EpochScope)
	require.Equal(t, stack.srv.editCPU.epoch, tool.ObservationEpoch)
	require.EqualValues(t, 5, *tool.CancelledWriterParked)
}

func TestWALObservationSharedGenerationAndResetNeverClearPeak(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	path := filepath.Join(t.TempDir(), "observations.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	store.AddBatch([]*graph.Node{{ID: "r/a.go::A", Name: "A", FilePath: "r/a.go", Kind: graph.KindFunction}}, nil)
	_, walBytes := store.DBStats() // test setup primes an EXISTING successful observation
	require.Positive(t, walBytes)
	base := (&Server{graph: store}).walReclaimObservation()
	generation := (&Server{graph: store.AtGeneration(7)}).walReclaimObservation()
	require.Equal(t, base, generation, "generation handles share the same physical core observations")
	require.GreaterOrEqual(t, *base.ObservedPeakWALBytes, walBytes)
	// Test-only independent SQL resets this idle owned WAL. The transport itself
	// must not issue SQL, observe a new size or reset the stored high-water mark.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = db.ExecContext(ctx, "PRAGMA busy_timeout=1000") // setup allowance for an existing background reader
	require.NoError(t, err)
	var busy, frames, copied int
	require.NoError(t, db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &copied))
	require.Zero(t, busy, "the actual idle WAL must be reset, not a refused query")
	file, err := os.Stat(path + "-wal")
	require.NoError(t, err)
	require.Zero(t, file.Size())
	require.NotNil(t, store.GetNode("r/a.go::A"), "reset must retain the actual stored payload")
	for range 3 {
		after := (&Server{graph: store}).walReclaimObservation()
		require.Equal(t, base, after, "TRUNCATE and stats reads cannot reset sampled peak/cause counters")
	}
	memory, err := store_sqlite.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, memory.Close()) })
	unobserved := (&Server{graph: memory}).walReclaimObservation()
	require.True(t, unobserved.SnapshotAvailable, "counter snapshot support is distinct from WAL size observation")
	require.False(t, unobserved.WALPeakObserved)
	require.Nil(t, unobserved.ObservedPeakWALBytes)
}
