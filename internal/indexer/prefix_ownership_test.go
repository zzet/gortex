package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestHungOwnerStatBounded(t *testing.T) {
	owner, candidate := t.TempDir(), t.TempDir()
	metadata := &RepoMetadata{RepoPrefix: "src", RootPath: owner}
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{"src": metadata}}
	entered, release, physicallyReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		require.Eventually(t, func() bool { return len(mi.prefixStats.slots) == 0 }, time.Second, time.Millisecond)
	})
	mi.prefixStats.stat = func(path string) (os.FileInfo, error) {
		if path == owner {
			close(entered)
			<-release // Models an OS Stat that cannot be canceled.
			defer close(physicallyReturned)
		}
		return os.Stat(path)
	}
	result := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: candidate, Name: "src"})
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owner Stat did not enter")
	}
	latencies := make([]time.Duration, 200)
	for i := range latencies {
		start := time.Now()
		got := mi.GetMetadata("src")
		latencies[i] = time.Since(start)
		require.Same(t, metadata, got)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	require.Less(t, latencies[197], 10*time.Millisecond, "GetMetadata must not queue behind the blocked filesystem call")
	select {
	case err := <-result:
		var collision *PrefixCollisionError
		require.ErrorAs(t, err, &collision)
		require.False(t, collision.OwnerMissing, "deadline is unknown, never missing-owner evidence")
		require.Less(t, time.Since(started), 2500*time.Millisecond)
	case <-time.After(3 * time.Second):
		t.Fatal("ownership observation exceeded its shared two-second budget")
	}
	select {
	case <-physicallyReturned:
		t.Fatal("the caller deadline must not be described as physical OS completion")
	default:
	}
	require.Same(t, metadata, mi.GetMetadata("src"), "a retired stat has no registry/store publication")
}

func TestPrefixStatAllowanceDiscardsLateResults(t *testing.T) {
	var allowance prefixStatAllowance
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releases sync.Once
	unblock := func() { releases.Do(func() { close(release) }) }
	var calls atomic.Int32
	allowance.stat = func(string) (os.FileInfo, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() {
		unblock()
		require.Eventually(t, func() bool { return len(allowance.slots) == 0 }, time.Second, time.Millisecond)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan prefixStatResult, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- allowance.observe(ctx, "immutable-root") }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("bounded stat allowance did not start its admitted worker")
		}
	}
	third := allowance.observe(t.Context(), "another-root")
	require.ErrorIs(t, third.err, errPrefixStatUnavailable)
	require.Equal(t, int32(2), calls.Load(), "saturation must not launch another physically hung call")
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			require.ErrorIs(t, result.err, context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("retired probe waited for the physical stat")
		}
	}
	unblock()
	require.Eventually(t, func() bool { return len(allowance.slots) == 0 }, time.Second, time.Millisecond,
		"late result delivery must fit its buffer and release its allowance")
}

func TestPrefixOwnerWitnessReplacementIsResampledOnce(t *testing.T) {
	for _, unstable := range []bool{false, true} {
		t.Run(map[bool]string{false: "one replacement", true: "unstable twice"}[unstable], func(t *testing.T) {
			first, next, candidate := t.TempDir(), t.TempDir(), t.TempDir()
			mi := &MultiIndexer{repos: map[string]*RepoMetadata{"src": {RepoPrefix: "src", RootPath: first}}}
			var ownerCalls, candidateCalls atomic.Int32
			mi.prefixStats.stat = func(path string) (os.FileInfo, error) {
				if path == candidate {
					candidateCalls.Add(1)
				} else {
					n := ownerCalls.Add(1)
					if n == 1 || unstable {
						mi.mu.Lock()
						// Replacement at the same prefix has a distinct witness,
						// including when the spelling returns to its first root.
						root := next
						if n == 2 {
							root = first
						}
						mi.repos["src"] = &RepoMetadata{RepoPrefix: "src", RootPath: root}
						mi.mu.Unlock()
					}
				}
				return os.Stat(path)
			}
			_, err := mi.observePrefixOwnership(t.Context(), "src", candidate)
			require.Equal(t, int32(2), ownerCalls.Load())
			require.Equal(t, int32(2), candidateCalls.Load(), "replacement must not reuse the first candidate FileInfo")
			if unstable {
				require.ErrorIs(t, err, errPrefixOwnerChanged)
			} else {
				var collision *PrefixCollisionError
				require.ErrorAs(t, err, &collision)
				require.Equal(t, next, collision.ExistingPath)
			}
		})
	}
}

func TestPrefixOwnershipPreservesExactAndPhysicalAliases(t *testing.T) {
	root := t.TempDir()
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{
		"named-owner": {RepoPrefix: "named-owner", RootPath: root},
		"src":         {RepoPrefix: "src", RootPath: t.TempDir()},
	}}
	mi.prefixStats.stat = func(string) (os.FileInfo, error) {
		return nil, errors.New("byte-identical registered root must not stat")
	}
	observation, err := mi.observePrefixOwnership(t.Context(), "src", root)
	require.NoError(t, err)
	require.Equal(t, "named-owner", observation.registeredPrefix, "same root under another name wins over a requested-prefix collision")
	alias := filepath.Join(t.TempDir(), "src")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("physical symlink control unavailable: %v", err)
	}
	mi = &MultiIndexer{repos: map[string]*RepoMetadata{"src": {RepoPrefix: "src", RootPath: root}}}
	observation, err = mi.observePrefixOwnership(t.Context(), "src", alias)
	require.NoError(t, err)
	require.Equal(t, "src", observation.registeredPrefix)
}
