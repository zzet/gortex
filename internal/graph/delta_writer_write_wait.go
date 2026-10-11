package graph

import (
	"sync"
	"sync/atomic"
	"time"
)

// waitTimedMutex is a mutex that accounts the time callers spent waiting for
// it. An uncontended Lock (TryLock succeeds) costs nothing extra.
type waitTimedMutex struct {
	sync.Mutex
	waited atomic.Int64
}

// Lock acquires the mutex, adding any wait to the running total.
func (m *waitTimedMutex) Lock() {
	if m.TryLock() {
		return
	}
	started := time.Now()
	m.Mutex.Lock()
	m.waited.Add(int64(time.Since(started)))
}

// WriteLockWait is the total time the delta's writes waited for its write
// lock: a write step whose wall time is not covered by this, its reads and its
// CPU was starved.
func (dw *DeltaWriter) WriteLockWait() time.Duration {
	return time.Duration(dw.writeMu.waited.Load())
}
