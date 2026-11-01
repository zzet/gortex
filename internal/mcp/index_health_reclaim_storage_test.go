package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
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
