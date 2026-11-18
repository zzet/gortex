package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestIndexHealthCarriesLiveReclaimStorage(t *testing.T) {
	spy := &walObservationSpy{Store: graph.New()}
	s := &Server{graph: spy}
	baseline := map[string]any{"node_count": 0}
	for _, expected := range []int64{2, 7} {
		spy.stats.CancelledWriterParked = expected
		result := s.refreshIndexHealthFileFailures(context.Background(), baseline)
		data, err := json.Marshal(result)
		require.NoError(t, err)
		var wire struct {
			Storage *struct {
				Reclaim *struct {
					Cancelled int64           `json:"cancelled_writer_parked"`
					Hold      json.RawMessage `json:"writer_hold"`
				} `json:"wal_reclaim"`
			} `json:"storage"`
		}
		require.NoError(t, json.Unmarshal(data, &wire))
		require.NotNil(t, wire.Storage, "the live physical-store counters must accompany the cached health baseline")
		require.NotNil(t, wire.Storage.Reclaim)
		require.Equal(t, expected, wire.Storage.Reclaim.Cancelled)
		require.NotEmpty(t, wire.Storage.Reclaim.Hold)
		require.Contains(t, compactIndexHealth(result, time.Now(), false), "wal_reclaim: scope=physical_store_since_open writer_hold_count=0")
	}
	require.EqualValues(t, 2, spy.reads.Load(), "exactly one existing I/O-free snapshot per health refresh")
	require.NotContains(t, baseline, "storage", "do not mutate a shared cached baseline")

	// A cached counter from an earlier physical binding cannot leak through a
	// store without the reporting capability. DBStats/index reads above panic.
	result := (&Server{graph: graph.New()}).refreshIndexHealthFileFailures(context.Background(), map[string]any{"storage": "old binding"})
	require.NotContains(t, result, "storage")
}

type healthPassiveSpy struct {
	*walObservationSpy
	passive store_sqlite.WALPassiveStats
}

func (s *healthPassiveSpy) WALPassiveStats() store_sqlite.WALPassiveStats { return s.passive }

func TestIndexHealthCarriesLivePeriodicCheckpointCounters(t *testing.T) {
	spy := &healthPassiveSpy{walObservationSpy: &walObservationSpy{Store: graph.New()}}
	server := &Server{graph: spy}
	for _, attempts := range []int64{3, 9} {
		spy.passive = store_sqlite.WALPassiveStats{Attempts: attempts, Incomplete: 2, ZeroProgress: 1, LastOutcome: "zero_progress"}
		spy.stats.UrgentAttempts = attempts - 1
		result := server.refreshIndexHealthFileFailures(context.Background(), map[string]any{"storage": "old"})
		data, err := json.Marshal(result)
		require.NoError(t, err)
		var wire struct {
			Storage struct {
				Checkpoint *struct {
					Attempts     int64  `json:"attempts"`
					Incomplete   int64  `json:"incomplete"`
					ZeroProgress int64  `json:"zero_progress"`
					Outcome      string `json:"last_outcome"`
				} `json:"wal_checkpoint"`
				Reclaim struct {
					Urgent *int64 `json:"urgent_attempts"`
				} `json:"wal_reclaim"`
			} `json:"storage"`
		}
		require.NoError(t, json.Unmarshal(data, &wire))
		require.NotNil(t, wire.Storage.Checkpoint, "live periodic producer cannot be replaced by stale health-cache counters")
		require.Equal(t, attempts, wire.Storage.Checkpoint.Attempts)
		require.EqualValues(t, 2, wire.Storage.Checkpoint.Incomplete)
		require.EqualValues(t, 1, wire.Storage.Checkpoint.ZeroProgress)
		require.Equal(t, "zero_progress", wire.Storage.Checkpoint.Outcome)
		require.NotNil(t, wire.Storage.Reclaim.Urgent)
		require.Equal(t, attempts-1, *wire.Storage.Reclaim.Urgent)
	}
}
