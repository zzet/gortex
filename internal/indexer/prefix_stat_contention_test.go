package indexer

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

// Parallel warmup has more than two healthy callers. The two-OS-call safety
// cap must bound physical calls without treating ordinary overlap as missing
// repository identity. This is a deterministic unit property, not a benchmark.
func TestPrefixStatHealthyContentionPreservesEveryRoot(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		var allowance prefixStatAllowance
		entered := make(chan struct{}, 3)
		release := make(chan struct{})
		var calls, active, peak atomic.Int32
		allowance.stat = func(string) (os.FileInfo, error) {
			calls.Add(1)
			n := active.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			defer active.Add(-1)
			entered <- struct{}{}
			<-release
			return info, nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), prefixOwnershipTimeout)
		defer cancel()
		first := make(chan prefixStatResult, 2)
		for range 2 {
			go func() { first <- allowance.observe(ctx, "healthy-root") }()
		}
		<-entered
		<-entered
		third := make(chan prefixStatResult, 1)
		go func() { third <- allowance.observe(ctx, "another-healthy-root") }()
		synctest.Wait()
		close(release)
		for range 2 {
			require.NoError(t, (<-first).err)
		}
		result := <-third
		require.NoError(t, result.err, "ordinary healthy stat contention must not silently omit a configured warmup root")
		require.Same(t, info, result.info)
		require.Equal(t, int32(3), calls.Load())
		require.LessOrEqual(t, peak.Load(), int32(2), "keep the existing physical OS-call cap")
		require.Zero(t, active.Load())
		require.NoError(t, ctx.Err(), "reuse the original bound; no timeout increase or reset")
	})
}

// Naming/canonical discovery uses the SAME physical allowance and must not
// turn an ordinary healthy burst into missing identity either.
func TestPrefixNamingHealthyContentionPreservesEveryRoot(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		var allowance prefixStatAllowance
		entered := make(chan struct{}, 3)
		release := make(chan struct{})
		var calls, active, peak atomic.Int32
		allowance.stat = func(string) (os.FileInfo, error) {
			calls.Add(1)
			n := active.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			defer active.Add(-1)
			entered <- struct{}{}
			<-release
			return info, nil
		}
		observe := func(ctx context.Context, root string) prefixStatResult {
			facts, err := allowance.observeNaming(ctx, root, func(context.Context, string) trackNamingFacts {
				_, err := allowance.stat(root)
				return trackNamingFacts{canonical: root, err: err}
			})
			if err != nil {
				return prefixStatResult{err: err}
			}
			if facts.canonical != root {
				return prefixStatResult{err: errors.New("naming result changed the requested healthy root")}
			}
			return prefixStatResult{info: info}
		}
		ctx, cancel := context.WithTimeout(t.Context(), prefixOwnershipTimeout)
		defer cancel()
		first := make(chan prefixStatResult, 2)
		for range 2 {
			go func() { first <- observe(ctx, "healthy-root") }()
		}
		<-entered
		<-entered
		third := make(chan prefixStatResult, 1)
		go func() { third <- observe(ctx, "another-healthy-root") }()
		synctest.Wait()
		close(release)
		for range 2 {
			require.NoError(t, (<-first).err)
		}
		result := <-third
		require.NoError(t, result.err, "ordinary healthy stat contention must not silently omit a configured warmup root")
		require.Same(t, info, result.info)
		require.Equal(t, int32(3), calls.Load())
		require.LessOrEqual(t, peak.Load(), int32(2), "keep the existing physical OS-call cap")
		require.Zero(t, active.Load())
		require.NoError(t, ctx.Err(), "reuse the original bound; no timeout increase or reset")
	})
}
