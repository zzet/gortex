package embedding

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type readyTestProvider struct {
	closed atomic.Int32
}

func (*readyTestProvider) Embed(context.Context, string) ([]float32, error) { return []float32{1}, nil }
func (*readyTestProvider) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1}}, nil
}
func (*readyTestProvider) Dimensions() int { return 1 }
func (p *readyTestProvider) Close() error  { p.closed.Add(1); return nil }

func publishReadyForTest(p Provider, usedAt time.Time) *codeEmbedderReady {
	sharedCodeMu.Lock()
	defer sharedCodeMu.Unlock()
	sharedCodeInst = p
	sharedCodeLoaded = true
	ready := &codeEmbedderReady{provider: p, usedAt: usedAt}
	sharedCodeReady.Store(ready)
	return ready
}

func TestLoadedCodeEmbedderDoesNotWaitForColdOrWarmLoadMutex(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			resetSharedCode(t)
			var want Provider
			if warm {
				want = &readyTestProvider{}
				publishReadyForTest(want, time.Now().Add(-time.Minute))
			}
			sharedCodeMu.Lock() // Witness that the entire getter completes while a loader holds this lock.
			done := make(chan Provider, 1)
			go func() { done <- LoadedSharedCodeEmbedder() }()
			select {
			case got := <-done:
				sharedCodeMu.Unlock()
				require.Equal(t, want, got, "warm scoring must survive ordinary model-mutex contention")
			case <-time.After(5 * time.Second):
				sharedCodeMu.Unlock()
				<-done
				t.Fatal("loaded-only getter waited behind the model mutex")
			}
			sharedCodeMu.Lock()
			loaded, reaper := sharedCodeLoaded, sharedCodeReaper
			sharedCodeMu.Unlock()
			if warm {
				require.WithinDuration(t, time.Now(), sharedCodeReady.Load().usedAt, time.Second)
			} else {
				require.False(t, loaded, "cold lookup must not initialize a model")
				require.False(t, reaper, "cold lookup must not start a reaper")
				require.Nil(t, sharedCodeReady.Load())
			}
		})
	}
}

func TestLoadedCodeEmbedderRefreshDefeatsStaleIdleRetirement(t *testing.T) {
	resetSharedCode(t)
	p := &readyTestProvider{}
	expired := publishReadyForTest(p, time.Now().Add(-2*codeEmbedderIdleTTL))
	require.Same(t, p, LoadedSharedCodeEmbedder())
	sharedCodeMu.Lock()
	retired := retireCodeEmbedderLocked(expired, time.Now())
	sharedCodeMu.Unlock()
	require.False(t, retired, "a refreshed snapshot must defeat the reaper's stale idle decision")
	require.Zero(t, p.closed.Load())
	require.Same(t, p, LoadedSharedCodeEmbedder())
}

func TestLoadedCodeEmbedderRetirementAndDisablePreventRepublishing(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(map[bool]string{false: "retire", true: "disable"}[disable], func(t *testing.T) {
			resetSharedCode(t)
			p := &readyTestProvider{}
			expired := publishReadyForTest(p, time.Now().Add(-2*codeEmbedderIdleTTL))
			if disable {
				SetCodeEmbedderEnabled(false)
			} else {
				sharedCodeMu.Lock()
				retired := retireCodeEmbedderLocked(expired, time.Now())
				sharedCodeMu.Unlock()
				require.True(t, retired)
				require.EqualValues(t, 1, p.closed.Load())
			}
			require.False(t, sharedCodeReady.CompareAndSwap(expired, &codeEmbedderReady{provider: p, usedAt: time.Now()}), "a getter holding the old snapshot cannot resurrect it")
			require.Nil(t, LoadedSharedCodeEmbedder())
			require.Nil(t, sharedCodeReady.Load())
			sharedCodeMu.Lock()
			loaded := sharedCodeLoaded
			sharedCodeMu.Unlock()
			require.False(t, loaded)
		})
	}
}

func TestCodeEmbedderCompletedNilLoadCanExpire(t *testing.T) {
	resetSharedCode(t)
	ready := publishReadyForTest(nil, time.Now().Add(-2*codeEmbedderIdleTTL))
	sharedCodeMu.Lock()
	retired := retireCodeEmbedderLocked(ready, time.Now())
	loaded := sharedCodeLoaded
	sharedCodeMu.Unlock()
	require.True(t, retired, "a failed completed load must be eligible for a later eager retry")
	require.False(t, loaded)
	require.Nil(t, LoadedSharedCodeEmbedder())
}

func TestLoadedCodeEmbedderConcurrentUsageAndDisable(t *testing.T) {
	resetSharedCode(t)
	p := &readyTestProvider{}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			for j := 0; j < 1000; j++ {
				if got := LoadedSharedCodeEmbedder(); got != nil && got != p {
					t.Errorf("unexpected ready provider %T", got)
				}
			}
		})
	}
	for i := 0; i < 100; i++ {
		publishReadyForTest(p, time.Now().Add(-2*codeEmbedderIdleTTL))
		sharedCodeMu.Lock()
		retireCodeEmbedderLocked(sharedCodeReady.Load(), time.Now())
		sharedCodeMu.Unlock()
		SetCodeEmbedderEnabled(false)
		SetCodeEmbedderEnabled(true)
	}
	workers.Wait()
	SetCodeEmbedderEnabled(false)
	require.Nil(t, LoadedSharedCodeEmbedder(), "concurrent readers must not republish a disabled provider")
}
