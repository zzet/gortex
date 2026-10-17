package indexer

import (
	"math"
	"runtime/metrics"
	"sync"
	"time"
)

// A step's wall time is its work, its waits for the store (the store's
// gauge), and the time it was runnable but not running: waiting for a
// processor. The last is invisible to every store gauge. The runtime reports
// it as /sched/latencies:seconds, the distribution of the time goroutines sat
// runnable before they ran. schedMark samples that histogram; the difference
// of two samples is the waits that ended between them.
//
// It is process-wide: the waits of every goroutine that ran in the interval —
// the delta's own, the answers', the reclaim loop's, the retirement sweep's —
// not the delta's alone. A phase with many waits and high total says the
// processor was contended over it; it cannot say which goroutine waited, nor
// that the waiting goroutine was the phase's. The total is estimated from
// bucket midpoints (the histogram keeps counts, not sums): exact in count,
// approximate in time, within a bucket's width.

const schedLatencyMetric = "/sched/latencies:seconds"

// schedMark is one sample of the histogram's cumulative bucket counts.
type schedMark struct {
	counts []uint64
}

var schedBuckets struct {
	once    sync.Once
	buckets []float64 // len(counts)+1 boundaries, as the runtime reports them
}

// readSchedMark samples the scheduler-latency histogram.
func readSchedMark() schedMark {
	sample := []metrics.Sample{{Name: schedLatencyMetric}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindFloat64Histogram {
		return schedMark{}
	}
	h := sample[0].Value.Float64Histogram()
	schedBuckets.once.Do(func() { schedBuckets.buckets = append([]float64(nil), h.Buckets...) })
	return schedMark{counts: append([]uint64(nil), h.Counts...)}
}

// schedWaitSince is the scheduler waits that ended between prev and m: how
// many, and their estimated total (each counted at its bucket's midpoint; an
// open-ended bucket at its finite bound).
func (m schedMark) since(prev schedMark) (waits int64, total time.Duration) {
	buckets := schedBuckets.buckets
	if len(m.counts) == 0 || len(prev.counts) != len(m.counts) || len(buckets) != len(m.counts)+1 {
		return 0, 0
	}
	var seconds float64
	for i := range m.counts {
		n := m.counts[i] - prev.counts[i]
		if n == 0 {
			continue
		}
		lo, hi := buckets[i], buckets[i+1]
		var mid float64
		switch {
		case math.IsInf(lo, -1):
			mid = hi
		case math.IsInf(hi, 1):
			mid = lo
		default:
			mid = (lo + hi) / 2
		}
		waits += int64(n)
		seconds += float64(n) * mid
	}
	return waits, time.Duration(seconds * float64(time.Second))
}
