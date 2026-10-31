package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

// TestStatusStorageReportsTheStoreWAL reads the storage block off a real
// SQLite store: file sizes, the pending backlog and the close-checkpoint
// estimate the stop path sizes its wait by, and the reclaim's counters.
func TestStatusStorageReportsTheStoreWAL(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", "full")
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "")
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "status.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "pkg/a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha", FilePath: "pkg/a.go"},
	}, nil)

	// Open starts the checkpoint/reclaim loop asynchronously. Wait for its
	// configured policy before comparing two otherwise independent snapshots.
	require.Eventually(t, func() bool {
		return store.WALReclaimStats().ThresholdBytes > 0
	}, 5*time.Second, time.Millisecond)

	st := storageStatusFor(store)
	require.NotNil(t, st, "a SQLite store must produce a storage block")
	assert.Positive(t, st.DBBytes)
	assert.GreaterOrEqual(t, st.WALPendingFrames, int64(0), "the wal-index of a live store is readable")
	wantEstimate, wantPending := store.CloseCheckpointEstimate()
	assert.Equal(t, wantEstimate.Milliseconds(), st.CloseCheckpointEstimateMS)
	assert.Equal(t, wantPending, st.WALPendingFrames)
	assert.Positive(t, st.CloseCheckpointEstimateMS)
	require.NotNil(t, st.WALReclaim)
	assert.Equal(t, store.WALReclaimStats().ThresholdBytes, st.WALReclaim.ThresholdBytes)

	body, err := json.Marshal(daemon.StatusResponse{Storage: st})
	require.NoError(t, err)
	for _, key := range []string{`"storage"`, `"wal_bytes"`, `"wal_pending_frames"`,
		`"close_checkpoint_estimate_ms"`, `"wal_reclaim"`, `"resets"`, `"deferrals"`, `"pause_max_ms"`} {
		assert.Contains(t, string(body), key)
	}
}

// TestStatusStorageIsOmittedWithoutASQLiteStore: no store (or a backend that
// is not SQLite) leaves the block out rather than rendering zeros.
func TestStatusStorageIsOmittedWithoutASQLiteStore(t *testing.T) {
	assert.Nil(t, storageStatusFor(nil))
}

// TestStatusWALReclaimAveragesAndDurations pins the counter conversion.
func TestStatusWALReclaimAveragesAndDurations(t *testing.T) {
	got := walReclaimStatus(store_sqlite.WALReclaimStats{
		ThresholdBytes: 256 << 20, Attempts: 5, Resets: 3, Deferrals: 1, Skips: 1,
		FramesReclaimed: 24092, BytesReclaimed: 90 << 20,
		PauseCount: 4, PauseTotal: 800 * time.Millisecond, PauseMax: 250400 * time.Microsecond, PauseLast: 190 * time.Millisecond,
		ReaderWaits: 2, ReaderWaitTotal: 300 * time.Millisecond, ReaderWaitMax: 200 * time.Millisecond,
		Backoff: 10 * time.Second, LastOutcome: "deferred", LastReason: "readers_in_flight=2",
	})
	assert.Equal(t, 200.0, got.PauseAvgMS)
	assert.Equal(t, 250.4, got.PauseMaxMS)
	assert.Equal(t, 190.0, got.PauseLastMS)
	assert.Equal(t, 150.0, got.ReaderWaitAvgMS)
	assert.Equal(t, int64(10000), got.BackoffMS)
	assert.Equal(t, "readers_in_flight=2", got.LastReason)
}

// TestStatusBuildLaneNamesTheHolder pins the lane-holder rendering: a build
// holding the lane is reported with its kind, checkout, generation and how
// long it has held it, and an idle lane reports no holder.
func TestStatusBuildLaneNamesTheHolder(t *testing.T) {
	gate := indexer.NewViewBuildGate()
	gate.Open()
	release, err := gate.Acquire(context.Background(), indexer.ViewBuildInteractive)
	require.NoError(t, err)
	withdraw := gate.NoteHolder(indexer.ViewBuildLaneHolder{
		Kind: "dirty_chain_compaction", CheckoutID: "checkout-7", Priority: "background", Generation: 42,
	})

	lane := buildLaneStatusFor(gate, time.Now().Add(1500*time.Millisecond))
	require.NotNil(t, lane)
	assert.True(t, lane.Open)
	assert.True(t, lane.Active)
	require.NotNil(t, lane.Holder)
	assert.Equal(t, "dirty_chain_compaction", lane.Holder.Kind)
	assert.Equal(t, "checkout-7", lane.Holder.CheckoutID)
	assert.Equal(t, int64(42), lane.Holder.Generation)
	assert.Positive(t, lane.Holder.SinceUnixMS)
	assert.GreaterOrEqual(t, lane.Holder.HeldForMS, 1400.0)
	assert.Equal(t, uint64(1), lane.AdmittedInteractive)

	var buf bytes.Buffer
	require.NoError(t, renderDaemonStatusTo(&buf, daemon.StatusResponse{BuildLane: lane}, "text"))
	assert.Contains(t, buf.String(), "build lane:")
	assert.Contains(t, buf.String(), "held by dirty_chain_compaction  checkout=checkout-7  generation=42")

	withdraw()
	release()
	idle := buildLaneStatusFor(gate, time.Now())
	require.NotNil(t, idle)
	assert.False(t, idle.Active)
	assert.Nil(t, idle.Holder, "an idle lane names no holder")
	assert.Nil(t, buildLaneStatusFor(nil, time.Now()), "no gate, no block")
}

// TestStatusBuildLaneReportsAnUndeclaredHolder: a builder that declares
// nothing still shows as holding the lane, so its wait is attributable.
func TestStatusBuildLaneReportsAnUndeclaredHolder(t *testing.T) {
	gate := indexer.NewViewBuildGate()
	gate.Open()
	release, err := gate.Acquire(context.Background(), indexer.ViewBuildInteractive)
	require.NoError(t, err)
	defer release()

	lane := buildLaneStatusFor(gate, time.Now())
	require.NotNil(t, lane.Holder)
	assert.Equal(t, "undeclared", lane.Holder.Kind)
}

// TestTheOneShotStatusRendererReachesTheStorageBlock is the wiring trace for
// the text door: the storage block is rendered, after the views block.
func TestTheOneShotStatusRendererReachesTheStorageBlock(t *testing.T) {
	st := countersStatus()
	st.Storage = &daemon.StorageStatus{
		DBBytes: 3 << 30, WALBytes: 700 << 20, WALPendingFrames: 9045293, CloseCheckpointEstimateMS: 105453,
		WALReclaim: &daemon.WALReclaimStatus{
			ThresholdBytes: 256 << 20, Attempts: 12, Resets: 9, Deferrals: 3,
			FramesReclaimed: 180000, BytesReclaimed: 700 << 20,
			PauseCount: 9, PauseMaxMS: 250.7, PauseAvgMS: 211, PauseLastMS: 198.2,
			BackoffMS: 5000, LastOutcome: "reset",
			OpenGateResets: 7, WriterHoldMaxMS: 2250.9, WriterHoldLastMS: 812.5,
		},
	}
	var buf bytes.Buffer
	require.NoError(t, renderDaemonStatusTo(&buf, st, "text"))
	out := buf.String()
	require.Contains(t, out, "storage:")
	assert.Contains(t, out, "wal pending frames=9045293  close checkpoint estimate=1m45.453s")
	assert.Contains(t, out, "wal reclaim:")
	assert.Contains(t, out, "resets=9  deferrals=3")
	assert.Contains(t, out, "gate pause n=9  max=250.7ms  avg=211.0ms  last=198.2ms")
	assert.Contains(t, out, "open-gate resets=7  writer hold max=2250.9ms  last=812.5ms")
	assert.Less(t, bytes.Index(buf.Bytes(), []byte("views:")), bytes.Index(buf.Bytes(), []byte("storage:")))
}

// TestStatusCarriesStorageAndBuildLaneWhileBusy is the wiring trace through
// the controller: both blocks are part of the lock-free half, so they are in
// the answer even while a track holds the controller mutex — which is when a
// slow edit's lane wait, or a WAL that stopped resetting, needs explaining.
func TestStatusCarriesStorageAndBuildLaneWhileBusy(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", "full")
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "")
	c := probeController(t, "repos:\n  - path: /work/alpha\n")
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "status.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	c.graph = store
	gate := indexer.NewViewBuildGate()
	gate.Open()
	release, err := gate.Acquire(context.Background(), indexer.ViewBuildInteractive)
	require.NoError(t, err)
	defer release()
	defer gate.NoteHolder(indexer.ViewBuildLaneHolder{Kind: "checkout_cycle", CheckoutID: "checkout-3"})()
	c.buildGate = gate

	c.mu.Lock() // stand in for a track / reload / enrichment in flight
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st, err := c.Status(ctx)
	require.NoError(t, err)

	require.NotNil(t, st.Storage, "status must carry the store's WAL block")
	require.NotNil(t, st.Storage.WALReclaim)
	assert.Positive(t, st.Storage.CloseCheckpointEstimateMS)
	require.NotNil(t, st.BuildLane, "status must carry the build lane")
	require.NotNil(t, st.BuildLane.Holder)
	assert.Equal(t, "checkout_cycle", st.BuildLane.Holder.Kind)
	assert.Equal(t, "checkout-3", st.BuildLane.Holder.CheckoutID)
	assert.Nil(t, st.Views, "the holder's identity is its own block, never the views census")
}

func TestStatusWALObservationCausesAndNullablePeak(t *testing.T) {
	for _, observed := range []bool{false, true} {
		snapshot := store_sqlite.WALReclaimStats{
			CancelledWriterParked: 3, CancelledWriteIntent: 2, CancelledOther: 1,
			WALPeakObserved: observed,
		}
		got := walReclaimStatus(snapshot)
		require.EqualValues(t, 3, got.CancelledWriterParked)
		require.EqualValues(t, 2, got.CancelledWriteIntent)
		require.EqualValues(t, 1, got.CancelledOther)
		require.Equal(t, observed, got.WALPeakObserved)
		encoded, err := json.Marshal(got)
		require.NoError(t, err)
		var wire map[string]any
		require.NoError(t, json.Unmarshal(encoded, &wire))
		require.Contains(t, wire, "observed_peak_wal_bytes")
		if observed {
			require.NotNil(t, got.ObservedPeakWALBytes)
			require.Zero(t, *got.ObservedPeakWALBytes)
			require.Equal(t, float64(0), wire["observed_peak_wal_bytes"])
		} else {
			require.Nil(t, got.ObservedPeakWALBytes)
			require.Nil(t, wire["observed_peak_wal_bytes"])
		}
	}
}

func TestStatusStorageReportsTheDaemonsClosePolicy(t *testing.T) {
	for _, mode := range []string{"skip", "full"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", mode)
			t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "")
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			st := storageStatusFor(store)
			require.NotNil(t, st)
			require.Equal(t, mode, st.CloseCheckpointPolicy)
			if mode == "skip" {
				require.Zero(t, st.CloseCheckpointEstimateMS)
			} else {
				require.Positive(t, st.CloseCheckpointEstimateMS)
			}
			body, err := json.Marshal(daemon.StatusResponse{Storage: st})
			require.NoError(t, err)
			require.Contains(t, string(body), `"close_checkpoint_policy":"`+mode+`"`)
		})
	}
}
