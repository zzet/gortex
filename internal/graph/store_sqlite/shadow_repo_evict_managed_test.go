package store_sqlite

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManagedShadowReplacementRefusesPublishedReservations(t *testing.T) {
	for _, state := range []ViewGenerationState{ViewGenerationReady, ViewGenerationSuperseded, ViewGenerationRetiring, ViewGenerationFailed} {
		t.Run(string(state), func(t *testing.T) {
			s, nodes := shadowEvictionFixture(t, 3)
			id, h := beginGenerationEvictionHandle(t, s, 1)
			require.NoError(t, h.AddBatchChecked(nodes, nil))
			require.NoError(t, s.Catalog().SetViewGenerationState(t.Context(), id, state, ViewGenerationBuilding))
			n, e, err := h.EvictRepoForShadowReplacement(t.Context(), "shadow")
			require.Error(t, err)
			require.Zero(t, n)
			require.Zero(t, e)
			require.Equal(t, 3, h.NodeCount())
			require.Equal(t, 3, s.NodeCount())
		})
	}
	for _, seal := range []int32{payloadSealSealed, payloadSealRetired} {
		t.Run(fmt.Sprint(seal), func(t *testing.T) {
			s, nodes := shadowEvictionFixture(t, 3)
			id, h := beginGenerationEvictionHandle(t, s, 1)
			require.NoError(t, h.AddBatchChecked(nodes, nil))
			s.setPayloadSeal(id, seal)
			n, e, err := h.EvictRepoForShadowReplacement(t.Context(), "shadow")
			require.Error(t, err)
			require.Zero(t, n)
			require.Zero(t, e)
			require.Equal(t, 3, h.NodeCount())
		})
	}
}

func TestManagedShadowReplacementRechecksLifecycleBetweenChunks(t *testing.T) {
	s, nodes := shadowEvictionFixture(t, 600)
	id, h := beginGenerationEvictionHandle(t, s, 1)
	require.NoError(t, h.AddBatchChecked(nodes, nil))
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	shadowDeleteObserver.Store(&shadowDeleteProbe{fire: func(string) { once.Do(func() { close(entered); <-release }) }})
	shadowDeleteTrigger(t, s, "nodes", "OLD.id")
	type result struct {
		n, e int
		err  error
	}
	done := make(chan result, 1)
	changed := make(chan error, 1)
	writerStarted := false
	t.Cleanup(func() {
		cancel()
		unpark()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("retirement did not join")
		}
		if writerStarted {
			select {
			case <-changed:
			case <-time.After(5 * time.Second):
				t.Error("catalog writer did not join")
			}
		}
	})
	go func() { n, e, err := h.EvictRepoForShadowReplacement(ctx, "shadow"); done <- result{n, e, err} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("no actual managed delete")
	}
	writerStarted = true
	go func() {
		changed <- s.Catalog().SetViewGenerationState(ctx, id, ViewGenerationReady, ViewGenerationBuilding)
	}()
	deadline := time.Now().Add(time.Second)
	for s.writeMu.waiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Positive(t, s.writeMu.waiting())
	unpark()
	var err error
	select {
	case err = <-changed:
	case <-ctx.Done():
		t.Fatal("catalog transition did not complete")
	}
	changed <- err
	require.NoError(t, err)
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		t.Fatal("managed retirement did not complete")
	}
	done <- r
	require.Error(t, r.err)
	require.Equal(t, 256, r.n)
	require.Zero(t, r.e)
	require.Equal(t, 344, h.NodeCount())
	require.Equal(t, 600, s.NodeCount())
	t.Logf("generation=%d committed=%d then refused ready reservation: %v", id, r.n, r.err)
}

func TestManagedShadowReplacementCancellationCanResume(t *testing.T) {
	s, nodes := shadowEvictionFixture(t, 600)
	_, h := beginGenerationEvictionHandle(t, s, 1)
	_, other := beginGenerationEvictionHandle(t, s, 2)
	require.NoError(t, h.AddBatchChecked(nodes, nil))
	require.NoError(t, other.AddBatchChecked(nodes, nil))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var count atomic.Int32
	shadowDeleteObserver.Store(&shadowDeleteProbe{fire: func(string) {
		if count.Add(1) == 257 {
			cancel()
		}
	}})
	shadowDeleteTrigger(t, s, "nodes", "OLD.id")
	n, e, err := h.EvictRepoForShadowReplacement(ctx, "shadow")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 256, n)
	require.Zero(t, e)
	shadowDeleteObserver.Store(nil)
	n, e, err = h.EvictRepoForShadowReplacement(t.Context(), "shadow")
	require.NoError(t, err)
	require.Equal(t, 344, n)
	require.Zero(t, e)
	require.Zero(t, h.NodeCount())
	require.Equal(t, 600, s.NodeCount())
	require.Equal(t, 600, other.NodeCount())
	t.Logf("managed generation %d resumed remaining 344 rows", h.viewGen)
}
