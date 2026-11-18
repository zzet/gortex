package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestWALBurstStatusPreservesUnobservedPopulation(t *testing.T) {
	empty := WALReclaimStatusFrom(store_sqlite.WALReclaimStats{})
	require.Nil(t, empty.EditBurstWriterHold, "no burst acquisition is unavailable, never an observed zero p95")
	hold := store_sqlite.DurationHistogram{Total: time.Millisecond, Max: time.Millisecond}
	hold.Counts[0] = 1
	measured := WALReclaimStatusFrom(store_sqlite.WALReclaimStats{EditBurstWriterHold: hold, DeferredEditBurst: 2, DeferredInteractiveRead: 3, BulkExceptionHolds: 4, HardCapExceptionHolds: 5, EditBurstGateClosures: 6})
	require.NotNil(t, measured.EditBurstWriterHold)
	require.EqualValues(t, 1, measured.EditBurstWriterHold.Count)
	require.EqualValues(t, 1, measured.EditBurstWriterHold.P95MS)
	require.EqualValues(t, 2, measured.DeferredEditBurst)
	require.EqualValues(t, 3, measured.DeferredInteractiveRead)
	require.EqualValues(t, 4, measured.BulkExceptionHolds)
	require.EqualValues(t, 5, measured.HardCapExceptionHolds)
	require.EqualValues(t, 6, measured.EditBurstGateClosures)
}
