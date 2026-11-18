package store_sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDurationHistogramBucketsAndQuantiles(t *testing.T) {
	var h durationHistogram
	require.Zero(t, h.snapshot().Quantile(0.95), "an empty histogram has no quantile")

	// The 50 ms edge is exact: 50 ms counts as within it, one nanosecond more
	// does not.
	h.observe(50 * time.Millisecond)
	h.observe(50*time.Millisecond + 1)
	s := h.snapshot()
	require.Equal(t, int64(1), s.Counts[5])
	require.Equal(t, int64(1), s.Counts[6])
	require.Equal(t, 50*time.Millisecond, DurationHistogramBounds[5])

	// 19 short holds and one long one: p95 is the short holds' edge, the
	// top quantile is the exact maximum, and Total sums every sample.
	h = durationHistogram{}
	for range 19 {
		h.observe(8 * time.Millisecond)
	}
	h.observe(3 * time.Second)
	s = h.snapshot()
	require.Equal(t, int64(20), s.Count())
	require.Equal(t, 10*time.Millisecond, s.Quantile(0.95))
	require.Equal(t, 3*time.Second, s.Quantile(1))
	require.Equal(t, 19*8*time.Millisecond+3*time.Second, s.Total)

	// A bucket's edge never reports more than the largest sample, and a
	// sample above every edge reports as the maximum.
	h = durationHistogram{}
	h.observe(30 * time.Millisecond)
	require.Equal(t, 30*time.Millisecond, h.snapshot().Quantile(0.5))
	h.observe(7 * time.Second)
	s = h.snapshot()
	require.Equal(t, int64(1), s.Counts[len(DurationHistogramBounds)])
	require.Equal(t, 7*time.Second, s.Quantile(1))

	// A negative reading (a clock that stepped) counts as zero.
	h = durationHistogram{}
	h.observe(-time.Second)
	s = h.snapshot()
	require.Equal(t, int64(1), s.Counts[0])
	require.Zero(t, s.Max)
}
