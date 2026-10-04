package goanalysis

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Foreground cache hits read the saved warm listing duration while the
// asynchronous warm-up merges batches and publishes its aggregate duration.
func TestWarmupListingDurationPublicationIsRaceFree(t *testing.T) {
	root := typecheckCacheFixture(t)
	cached := newTestProvider(t)
	cached.warm.quiet = time.Millisecond
	cached.warm.batchSize = 1
	t.Cleanup(func() { _ = cached.Close() })
	st := cached.typecheckState(root, goManifestDigest(root))
	stop, returned := make(chan struct{}), make(chan struct{})
	var samples, observed atomic.Int64
	go func() {
		defer close(returned)
		for {
			select {
			case <-stop:
				return
			default:
			}
			st.mu.Lock()
			observed.Store(st.warmList.Load())
			st.mu.Unlock()
			samples.Add(1)
			runtime.Gosched()
		}
	}()
	joined := false
	defer func() {
		if !joined {
			close(stop)
			<-returned
		}
	}()
	require.Equal(t, warmupOutcomeStarted, cached.WarmCheckoutCompiler(root, cachedScope))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	status := cached.waitCheckoutWarmup(ctx, root)
	close(stop)
	<-returned
	joined = true
	require.Equal(t, warmupWarm, status.State, "warm-up status %+v", status)
	require.Greater(t, status.Batches, 1, "the regression must merge multiple listings")
	require.GreaterOrEqual(t, observed.Load(), int64(0))
	require.Positive(t, samples.Load(), "a foreground cache reader must overlap the warm-up")
	st.mu.Lock()
	saved := time.Duration(st.warmList.Load())
	st.mu.Unlock()
	require.Positive(t, saved)
	require.Equal(t, status.Ms, saved.Milliseconds(), "completion must publish the aggregate saved listing duration")
}
