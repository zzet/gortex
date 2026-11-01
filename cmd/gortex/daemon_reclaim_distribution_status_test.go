package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestStatusStorageCarriesReclaimDistributions(t *testing.T) {
	data, err := json.Marshal(walReclaimStatus(store_sqlite.WALReclaimStats{}))
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	for _, name := range []string{"writer_hold", "writer_wait"} {
		t.Run(name, func(t *testing.T) {
			require.Contains(t, fields, name, "supported empty distributions must be present with count 0, not silently unavailable")
			var histogram struct {
				Count  int64     `json:"count"`
				Bounds []float64 `json:"bucket_le_ms"`
				Counts []int64   `json:"bucket_counts"`
			}
			require.NoError(t, json.Unmarshal(fields[name], &histogram))
			require.Zero(t, histogram.Count)
			require.Len(t, histogram.Bounds, len(store_sqlite.DurationHistogramBounds))
			require.Len(t, histogram.Counts, len(histogram.Bounds)+1)
			require.Contains(t, histogram.Bounds, float64(50), "the 50 ms acceptance boundary must remain exact")
		})
	}
}

func TestStatusReclaimDistributionCopiesBucketData(t *testing.T) {
	hold := store_sqlite.DurationHistogram{Total: 75 * time.Millisecond, Max: 50 * time.Millisecond}
	hold.Counts[5] = 2 // two observations in the (20 ms,50 ms] bucket
	wait := store_sqlite.DurationHistogram{Total: time.Millisecond, Max: time.Millisecond}
	wait.Counts[0] = 1
	stats := store_sqlite.WALReclaimStats{WriterHold: hold, WriterWait: wait}
	got := walReclaimStatus(stats)
	require.EqualValues(t, 2, got.WriterHold.Count)
	require.Equal(t, float64(75), got.WriterHold.TotalMS)
	require.Equal(t, float64(50), got.WriterHold.P50MS)
	require.Equal(t, float64(50), got.WriterHold.P95MS)
	require.Equal(t, float64(1), got.WriterWait.P95MS)
	got.WriterHold.BucketCounts[5] = 999
	got.WriterHold.BucketLeMS[5] = 999
	again := walReclaimStatus(stats)
	require.EqualValues(t, 2, again.WriterHold.BucketCounts[5])
	require.Equal(t, float64(50), again.WriterHold.BucketLeMS[5])
}
