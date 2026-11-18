package semantic

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

type queueWaitCtxProvider struct {
	mockCtxProvider
}

func (*queueWaitCtxProvider) ExcludesQueueWaitFromCeiling() {}

// queuedPass simulates a pass that queues behind another pass's apply until
// queued returns (an uncancellable wait, bracketed with ExcludeQueueWait), then
// needs work of real time. Like tstypes it records context.Cause when its
// context ends first.
func queuedPass(name string, queued func(), work time.Duration) func(context.Context, graph.Store, string, string, EnrichDeadlinePolicy) (*EnrichResult, error) {
	return func(ctx context.Context, _ graph.Store, _, _ string, _ EnrichDeadlinePolicy) (*EnrichResult, error) {
		res := &EnrichResult{Provider: name, Language: "go"}
		started := time.Now()
		resume := ExcludeQueueWait(ctx)
		queued() // stands in for mu.Lock(): cancellation cannot end it
		resume()
		res.LockWaitMs = time.Since(started).Milliseconds()
		select {
		case <-time.After(work):
			res.NodesEnriched = 1
		case <-ctx.Done():
			res.Partial = true
			res.AbortReason = context.Cause(ctx).Error()
		}
		return res, nil
	}
}

func sleepFor(d time.Duration) func() { return func() { time.Sleep(d) } }

func runQueuedPass(t *testing.T, p Provider, opts EnrichOptions) *EnrichResult {
	t.Helper()
	mgr := NewManager(Config{
		Enabled:   true,
		Providers: []ProviderConfig{{Name: "queued-go", Languages: []string{"go"}, Priority: 1, Enabled: true}},
	}, zap.NewNop())
	t.Cleanup(func() { _ = mgr.Close() })
	mgr.RegisterProvider(p)
	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})
	results, _, err := mgr.EnrichAll(g, map[string]string{"default": "/tmp/test"}, opts)
	require.NoError(t, err)
	require.Len(t, results, 1)
	return results[0]
}

func queuedProvider(optIn bool, queued func(), work time.Duration) Provider {
	p := mockCtxProvider{
		mockProvider:  mockProvider{name: "queued-go", languages: []string{"go"}, available: true},
		enrichCtxFunc: queuedPass("queued-go", queued, work),
	}
	if optIn {
		return &queueWaitCtxProvider{p}
	}
	return &p
}

// A pass that waits on the resolve mutex for longer than the whole outer
// ceiling must still get the ceiling's time once it holds the mutex. Before,
// the ceiling counted the wait, so the pass was cut as soon as the mutex was
// granted and landed nothing.
func TestManager_OuterCeilingExcludesQueueWait(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "800ms")
	res := runQueuedPass(t, queuedProvider(true, sleepFor(2*time.Second), 100*time.Millisecond), EnrichOptions{})
	assert.False(t, res.Partial, "the queue wait consumed the ceiling: %s", res.AbortReason)
	assert.Equal(t, 1, res.NodesEnriched)
}

// A provider that does not opt in keeps the plain timeout: its wait still
// counts, so the same pass is cut, and the ceiling still bounds it.
func TestManager_OuterCeilingStillCountsWaitWithoutOptIn(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "800ms")
	res := runQueuedPass(t, queuedProvider(false, sleepFor(2*time.Second), 100*time.Millisecond), EnrichOptions{})
	assert.True(t, res.Partial)
	assert.Equal(t, context.DeadlineExceeded.Error(), res.AbortReason)
}

// Work after the wait is still bounded: the ceiling resumes with the time
// that was left, and the pass's recorded cause names the ceiling.
func TestManager_PausedOuterCeilingStillBoundsTheApply(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "600ms")
	start := time.Now()
	res := runQueuedPass(t, queuedProvider(true, sleepFor(time.Second), 20*time.Second), EnrichOptions{})
	assert.True(t, res.Partial)
	assert.Equal(t, errEnrichCeiling.Error(), res.AbortReason)
	assert.Less(t, time.Since(start), 10*time.Second, "the ceiling must still cut the apply")
}

// A caller that cancels while the pass is queued is a shutdown, not the
// ceiling, even though the ceiling's time runs out before the pass resumes.
func TestManager_ParentCancelDuringPausedCeilingIsNotTheCeiling(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "300ms")
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued := func() {
		cancel()
		time.Sleep(900 * time.Millisecond) // well past the ceiling
	}
	res := runQueuedPass(t, queuedProvider(true, queued, 20*time.Second), EnrichOptions{Context: parent})
	assert.True(t, res.Partial)
	assert.Equal(t, context.Canceled.Error(), res.AbortReason)
}

func TestEnrichCeilingPauseAndResume(t *testing.T) {
	ctx, c := newEnrichCeiling(context.Background(), 300*time.Millisecond, nil)
	defer c.stop()

	outer := ExcludeQueueWait(ctx)
	inner := ExcludeQueueWait(ctx) // nested waits keep the clock stopped
	time.Sleep(600 * time.Millisecond)
	inner()
	inner()                            // a second call is harmless
	time.Sleep(500 * time.Millisecond) // longer than the ceiling had left
	require.NoError(t, ctx.Err(), "the clock must stay stopped until the last wait ends")
	outer()

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the ceiling did not resume after the wait")
	}
	assert.ErrorIs(t, context.Cause(ctx), errEnrichCeiling)
	assert.True(t, errors.Is(context.Cause(ctx), context.DeadlineExceeded))
}

// tstypes records context.Cause of a phase context derived from the pass
// context: a phase's own deadline, the ceiling and a shutdown stay distinct,
// and a ceiling expiring after a parent cancel does not relabel it.
func TestEnrichCeilingCausesReachPhaseContexts(t *testing.T) {
	ctx, c := newEnrichCeiling(context.Background(), 100*time.Millisecond, nil)
	phase, cancelPhase := context.WithDeadline(ctx, time.Now().Add(time.Hour))
	<-phase.Done()
	cancelPhase()
	assert.ErrorIs(t, context.Cause(phase), errEnrichCeiling)
	c.stop()

	ctx, c = newEnrichCeiling(context.Background(), time.Hour, nil)
	phase, cancelPhase = context.WithDeadline(ctx, time.Now().Add(20*time.Millisecond))
	<-phase.Done()
	assert.Equal(t, context.DeadlineExceeded, context.Cause(phase))
	cancelPhase()
	c.stop()

	parent, cancelParent := context.WithCancel(context.Background())
	ctx, c = newEnrichCeiling(parent, 100*time.Millisecond, nil)
	defer c.stop()
	phase, cancelPhase = context.WithDeadline(ctx, time.Now().Add(time.Hour))
	defer cancelPhase()
	cancelParent()
	<-phase.Done()
	time.Sleep(300 * time.Millisecond) // the armed ceiling fires after the cancel
	assert.Equal(t, context.Canceled, context.Cause(phase))
	assert.Equal(t, context.Canceled, context.Cause(ctx))
}

func TestEnrichCeilingFollowsParentAndStop(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, c := newEnrichCeiling(parent, time.Hour, nil)
	resume := ExcludeQueueWait(ctx)
	cancel()
	<-ctx.Done() // a paused ceiling still ends with its parent
	resume()
	assert.Equal(t, context.Canceled, context.Cause(ctx))
	c.stop()

	ctx2, c2 := newEnrichCeiling(context.Background(), time.Hour, nil)
	c2.stop()
	<-ctx2.Done()
	assert.Equal(t, context.Canceled, context.Cause(ctx2))
	ExcludeQueueWait(ctx2)() // pausing a stopped ceiling is a no-op

	ExcludeQueueWait(context.Background())() // no ceiling: nothing to pause
}

// A wait that outlives the ceiling's remaining time is announced once while
// it is still going; a wait that ends in time is not.
func TestEnrichCeilingAnnouncesAWaitPastTheCeiling(t *testing.T) {
	var calls atomic.Int32
	var queued atomic.Int64
	ctx, c := newEnrichCeiling(context.Background(), 100*time.Millisecond, func(d time.Duration) {
		calls.Add(1)
		queued.Store(int64(d))
	})
	defer c.stop()

	resume := ExcludeQueueWait(ctx)
	require.Eventually(t, func() bool { return calls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	resume()
	assert.Equal(t, int32(1), calls.Load(), "one announcement per wait")
	// The alarm is armed for what the ceiling had left at the pause, a
	// little under its 100 ms; assert a margin timer slop cannot breach.
	assert.GreaterOrEqual(t, time.Duration(queued.Load()), 50*time.Millisecond)
	require.NoError(t, ctx.Err())

	ctx3, c3 := newEnrichCeiling(context.Background(), time.Hour, func(time.Duration) { calls.Add(1) })
	defer c3.stop()
	ExcludeQueueWait(ctx3)()
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), calls.Load(), "a short wait is not announced")
}
