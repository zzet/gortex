package store_sqlite

import (
	"math"
	"slices"
	"sync/atomic"
	"time"
)

// DurationHistogramBounds are the upper edges of a duration histogram's
// buckets; one more bucket holds everything above the last edge. The 50 ms
// edge is exact on purpose: the edit-loop budget caps the reclaim's writer
// holds at p95 50 ms, and a bucket straddling that line could not decide it.
var DurationHistogramBounds = [...]time.Duration{
	time.Millisecond,
	2 * time.Millisecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	5 * time.Second,
}

// durationHistogram counts durations into DurationHistogramBounds with
// atomics only, so recording never takes a lock and a status read never
// waits on a recorder.
type durationHistogram struct {
	counts [len(DurationHistogramBounds) + 1]atomic.Int64
	total  atomic.Int64 // nanoseconds
	max    atomic.Int64 // nanoseconds
}

// observe raises max before it counts the sample, and snapshot reads the
// counts before max, so a snapshot never counts a sample above its Max.
func (h *durationHistogram) observe(d time.Duration) {
	d = max(d, 0)
	for {
		m := h.max.Load()
		if int64(d) <= m || h.max.CompareAndSwap(m, int64(d)) {
			break
		}
	}
	h.total.Add(int64(d))
	i, _ := slices.BinarySearch(DurationHistogramBounds[:], d)
	h.counts[i].Add(1)
}

func (h *durationHistogram) snapshot() DurationHistogram {
	var out DurationHistogram
	for i := range h.counts {
		out.Counts[i] = h.counts[i].Load()
	}
	out.Total = time.Duration(h.total.Load())
	out.Max = time.Duration(h.max.Load())
	return out
}

// DurationHistogram is a snapshot of a duration histogram. Counts[i] holds
// the observations at most DurationHistogramBounds[i] and above the edge
// before it; the last count holds those above every edge. The fields are read
// one by one, so a snapshot taken mid-record may hold a Total or Max that
// includes a sample its Counts do not yet.
type DurationHistogram struct {
	Counts [len(DurationHistogramBounds) + 1]int64
	Total  time.Duration
	Max    time.Duration
}

// Count is the number of observations.
func (h DurationHistogram) Count() int64 {
	var n int64
	for _, c := range h.Counts {
		n += c
	}
	return n
}

// Quantile returns an upper bound on the q-quantile (0 < q <= 1): the edge of
// the bucket holding that observation, or Max when Max is smaller or the
// observation lies above every edge. It is zero for an empty histogram.
func (h DurationHistogram) Quantile(q float64) time.Duration {
	n := h.Count()
	if n == 0 {
		return 0
	}
	rank := max(int64(math.Ceil(q*float64(n))), 1)
	var seen int64
	for i, c := range h.Counts {
		seen += c
		if seen >= rank && i < len(DurationHistogramBounds) {
			return min(DurationHistogramBounds[i], h.Max)
		}
	}
	return h.Max
}
