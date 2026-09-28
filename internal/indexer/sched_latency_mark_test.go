package indexer

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// The scheduler-latency mark counts the runnable waits that end between two
// samples and estimates their time: with more runnable goroutines than
// processors the waits are counted and their total is positive; with
// nothing in between, nothing is counted.
func TestSchedMarkCountsTheWaitsBetweenTwoSamples(t *testing.T) {
	prevProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prevProcs)
	start := readSchedMark()
	if len(start.counts) == 0 {
		t.Skip("the runtime reports no scheduler-latency histogram")
	}
	var wg sync.WaitGroup
	deadline := time.Now().Add(50 * time.Millisecond)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				runtime.Gosched()
			}
		}()
	}
	wg.Wait()
	end := readSchedMark()
	waits, total := end.since(start)
	if waits == 0 || total <= 0 {
		t.Fatalf("contended goroutines produced %d waits, %v", waits, total)
	}
	if w, d := end.since(end); w != 0 || d != 0 {
		t.Fatalf("a mark against itself reports %d waits, %v", w, d)
	}
	if w, _ := end.since(schedMark{}); w != 0 {
		t.Fatalf("a mark against an empty one reports %d waits", w)
	}
}
