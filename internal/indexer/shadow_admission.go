package indexer

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	// Keep aggregate cold-start shadow state comfortably below the resident
	// database working set on a typical development machine. Operators can
	// override the process-wide ceiling before daemon start.
	defaultShadowProcessBudgetBytes int64 = 1 << 30 // 1 GiB

	// One shadow already drives its own parser worker pool and retains the
	// decoded graph through the destructive drain. Queue additional eligible
	// repositories instead of multiplying both CPU pressure and resident graph
	// state. Operators can raise this only after measuring their workload.
	defaultShadowMaxConcurrent = 1

	// A source file expands into nodes, edges, indexes, and drain buffers. Raw
	// input bytes alone badly undercount source-heavy repositories, so admission
	// charges both input expansion and a per-file structural estimate.
	shadowInputExpansion        int64 = 2
	shadowEstimatedBytesPerFile int64 = 128 << 10
	shadowMinimumChargeBytes    int64 = 32 << 20
)

// Wait budgets for a filtered pass's shadow admission. A pass without a
// corpus filter (a cold or whole-repository index) waits for as long as its
// context allows: the in-memory route is the only affordable one for it. A
// filtered pass is an optimisation over a path that already works, so its wait
// is bounded — and an edit's own build, which a caller is timing, is bounded
// far tighter than background reconciliation.
const (
	// filteredShadowAdmissionWait bounds how long a background filtered pass
	// (warm-up, compaction, base publication) queues before running
	// store-direct.
	filteredShadowAdmissionWait = 2 * time.Second
	// interactiveShadowAdmissionWait bounds the same wait for the build an
	// edit's publication waits on.
	interactiveShadowAdmissionWait = 100 * time.Millisecond
)

// shadowAdmissionBudget is a context-aware FIFO process admission gate. Local
// shadow candidates wait for both the weighted byte ceiling and the hard slot
// cap. Only disabled or individually oversized candidates fall back to SQLite.
type shadowAdmissionBudget struct {
	mu            sync.Mutex
	capacity      int64
	maxConcurrent int
	used          int64
	peak          int64
	active        int
	peakActive    int
	waiters       []*shadowAdmissionWaiter
	admissions    uint64
	queued        uint64
}

type shadowAdmissionWaiter struct {
	weight  int64
	ready   chan struct{}
	granted bool
}

type shadowAdmissionLease struct {
	budget *shadowAdmissionBudget
	weight int64
	once   sync.Once
}

type shadowAdmissionStats struct {
	capacity      int64
	used          int64
	peak          int64
	maxConcurrent int
	active        int
	peakActive    int
	waiters       int
	admissions    uint64
	queued        uint64
}

var processShadowAdmission = newShadowAdmissionBudget(
	shadowProcessBudgetBytes(),
	shadowMaxConcurrent(),
)

// shadowAdmissionWaitFor is how long a pass queues for a shadow slot. Zero
// means no bound beyond ctx (a pass without a corpus filter).
func shadowAdmissionWaitFor(ctx context.Context, filtered bool) time.Duration {
	if !filtered {
		return 0
	}
	if interactiveBuildContext(ctx) {
		return interactiveShadowAdmissionWait
	}
	return filteredShadowAdmissionWait
}

type interactiveBuildKey struct{}

// withInteractiveBuild marks ctx as carrying the build an edit's publication
// waits on, for callers that serve an edit without a refresh ticket (a
// synchronous checkout mutation's republish, say).
func withInteractiveBuild(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, interactiveBuildKey{}, true)
}

// interactiveBuildContext reports whether ctx carries a build an edit's
// publication is waiting on: one marked withInteractiveBuild, a coordinator
// cycle serving refresh tickets (the cycles that take the lane at interactive
// priority), a cycle that observed a filesystem change and times its
// publication, or a request that brought its own publication record. A
// background chain compaction carries a phase record too, but of its own
// source, and does not count.
func interactiveBuildContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if marked, _ := ctx.Value(interactiveBuildKey{}).(bool); marked {
		return true
	}
	if _, ok := ctx.Value(publicationTargetKey{}).(publicationTarget); ok {
		return true
	}
	if publicationRecordFrom(ctx) != nil {
		return true
	}
	if record := phaseRecordFrom(ctx); record != nil && record.source == publicationSourceObservedChange {
		return true
	}
	return false
}

func newShadowAdmissionBudget(capacity int64, maxConcurrent int) *shadowAdmissionBudget {
	if capacity < 0 {
		capacity = 0
	}
	if maxConcurrent < 0 {
		maxConcurrent = 0
	}
	return &shadowAdmissionBudget{
		capacity:      capacity,
		maxConcurrent: maxConcurrent,
	}
}

// shadowProcessBudgetBytes returns the process-wide in-memory shadow budget.
// Zero explicitly disables shadows. Invalid or negative values use the safe
// default. The environment is read once when the process gate is constructed.
func shadowProcessBudgetBytes() int64 {
	if raw := os.Getenv("GORTEX_SHADOW_BUDGET_BYTES"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && value >= 0 {
			return value
		}
	}
	return defaultShadowProcessBudgetBytes
}

// shadowMaxConcurrent returns the process-wide hard slot cap. Zero explicitly
// disables shadows; invalid and negative values retain the conservative default.
func shadowMaxConcurrent() int {
	if raw := os.Getenv("GORTEX_SHADOW_MAX_CONCURRENT"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err == nil && value >= 0 {
			return value
		}
	}
	return defaultShadowMaxConcurrent
}

func shadowAdmissionWeight(fileCount int, inputBytes int64) int64 {
	if fileCount < 0 {
		fileCount = 0
	}
	if inputBytes < 0 {
		inputBytes = 0
	}
	// Saturate instead of overflowing an admission charge into a small value.
	const maxInt64 = int64(^uint64(0) >> 1)
	weight := inputBytes
	if inputBytes > maxInt64/shadowInputExpansion {
		weight = maxInt64
	} else {
		weight *= shadowInputExpansion
	}
	fileWeight := int64(fileCount)
	if fileWeight > maxInt64/shadowEstimatedBytesPerFile {
		fileWeight = maxInt64
	} else {
		fileWeight *= shadowEstimatedBytesPerFile
	}
	if weight > maxInt64-fileWeight {
		weight = maxInt64
	} else {
		weight += fileWeight
	}
	if weight < shadowMinimumChargeBytes {
		weight = shadowMinimumChargeBytes
	}
	return weight
}

func (b *shadowAdmissionBudget) acquire(
	ctx context.Context,
	weight int64,
) (*shadowAdmissionLease, error) {
	if b == nil || weight <= 0 || b.capacity <= 0 || b.maxConcurrent <= 0 || weight > b.capacity {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	b.mu.Lock()
	select {
	case <-ctx.Done():
		b.mu.Unlock()
		return nil, ctx.Err()
	default:
	}
	if len(b.waiters) == 0 && b.canGrantLocked(weight) {
		b.chargeLocked(weight)
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			b.release(weight)
			return nil, ctx.Err()
		default:
			return &shadowAdmissionLease{budget: b, weight: weight}, nil
		}
	}

	waiter := &shadowAdmissionWaiter{weight: weight, ready: make(chan struct{})}
	b.waiters = append(b.waiters, waiter)
	b.queued++
	b.grantWaitersLocked()
	b.mu.Unlock()

	select {
	case <-waiter.ready:
		select {
		case <-ctx.Done():
			b.release(weight)
			return nil, ctx.Err()
		default:
			return &shadowAdmissionLease{budget: b, weight: weight}, nil
		}
	case <-ctx.Done():
		b.mu.Lock()
		if waiter.granted {
			b.unchargeLocked(weight)
		} else {
			for i, queued := range b.waiters {
				if queued != waiter {
					continue
				}
				b.removeWaiterLocked(i)
				break
			}
		}
		b.grantWaitersLocked()
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

// canGrantLocked reports whether a shadow can enter. Caller holds b.mu.
func (b *shadowAdmissionBudget) canGrantLocked(weight int64) bool {
	return b.active < b.maxConcurrent && weight <= b.capacity-b.used
}

// chargeLocked records one granted shadow. Caller holds b.mu.
func (b *shadowAdmissionBudget) chargeLocked(weight int64) {
	b.used += weight
	b.active++
	b.admissions++
	if b.used > b.peak {
		b.peak = b.used
	}
	if b.active > b.peakActive {
		b.peakActive = b.active
	}
}

// unchargeLocked releases one granted shadow. Caller holds b.mu.
func (b *shadowAdmissionBudget) unchargeLocked(weight int64) {
	b.used -= weight
	if b.used < 0 {
		b.used = 0
	}
	b.active--
	if b.active < 0 {
		b.active = 0
	}
}

// grantWaitersLocked admits strictly from the FIFO head. Caller holds b.mu.
func (b *shadowAdmissionBudget) grantWaitersLocked() {
	for len(b.waiters) > 0 && b.canGrantLocked(b.waiters[0].weight) {
		waiter := b.waiters[0]
		b.removeWaiterLocked(0)
		b.chargeLocked(waiter.weight)
		waiter.granted = true
		close(waiter.ready)
	}
}

func (b *shadowAdmissionBudget) removeWaiterLocked(index int) {
	copy(b.waiters[index:], b.waiters[index+1:])
	last := len(b.waiters) - 1
	b.waiters[last] = nil
	b.waiters = b.waiters[:last]
}

func (b *shadowAdmissionBudget) release(weight int64) {
	if b == nil || weight <= 0 {
		return
	}
	b.mu.Lock()
	b.unchargeLocked(weight)
	b.grantWaitersLocked()
	b.mu.Unlock()
}

func (l *shadowAdmissionLease) Release() {
	if l == nil || l.budget == nil || l.weight <= 0 {
		return
	}
	l.once.Do(func() {
		l.budget.release(l.weight)
	})
}

func (b *shadowAdmissionBudget) snapshot() shadowAdmissionStats {
	if b == nil {
		return shadowAdmissionStats{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return shadowAdmissionStats{
		capacity:      b.capacity,
		used:          b.used,
		peak:          b.peak,
		maxConcurrent: b.maxConcurrent,
		active:        b.active,
		peakActive:    b.peakActive,
		waiters:       len(b.waiters),
		admissions:    b.admissions,
		queued:        b.queued,
	}
}
