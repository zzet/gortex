package indexer

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

func TestWatcherSourceEventConcurrentRegistration(t *testing.T) {
	w := &Watcher{}
	var calls atomic.Int64
	callback := func(string) { calls.Add(1) }
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		for range 100 {
			w.OnSourceEvent(callback)
			w.OnSourceEvent(nil)
		}
	}()
	for range 100 {
		w.notifySourceEvent("a.go")
	}
	done.Wait()
	w.OnSourceEvent(callback)
	before := calls.Load()
	w.notifySourceEvent("a.go")
	require.Equal(t, before+1, calls.Load())
	w.OnSourceEvent(nil)
	w.notifySourceEvent("a.go")
	require.Equal(t, before+1, calls.Load())
}

func TestMultiWatcherSourceEventRegistrationFollowsNewWatchers(t *testing.T) {
	mw, mi, repoA, _ := setupMultiWatcherTest(t)
	t.Cleanup(func() { _ = mw.Stop() })
	var first, second atomic.Int64
	eventPath := filepath.Join(repoA, "delivered.go")
	mw.OnSourceEvent(func(path string) {
		if path == eventPath {
			first.Add(1)
		}
	})
	mw.watchers["repo-a"].notifySourceEvent(eventPath)
	require.EqualValues(t, 1, first.Load())

	// Replacement takes effect on already-created watchers. A callback may
	// register its successor because delivery holds no registration lock.
	callback := func(path string) {
		if path == eventPath {
			second.Add(1)
		}
	}
	mw.OnSourceEvent(func(path string) {
		if path == eventPath {
			second.Add(1)
			mw.OnSourceEvent(callback)
		}
	})
	mw.watchers["repo-a"].notifySourceEvent(eventPath)
	require.EqualValues(t, 1, first.Load())
	require.EqualValues(t, 1, second.Load())

	newRepo := t.TempDir()
	writeFile(t, filepath.Join(newRepo, "main.go"), "package main\nfunc Added() {}\n")
	_, err := mi.TrackRepo(config.RepoEntry{Path: newRepo, Name: "repo-new"})
	require.NoError(t, err)
	require.NoError(t, mw.AddRepo("repo-new", config.WatchConfig{Enabled: true, DebounceMs: 60_000}))
	mw.mu.Lock()
	added := mw.watchers["repo-new"]
	mw.mu.Unlock()
	require.NotNil(t, added)
	before := second.Load()
	added.notifySourceEvent(eventPath)
	require.Equal(t, before+1, second.Load())

	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		for range 100 {
			mw.OnSourceEvent(callback)
			mw.OnSourceEvent(nil)
		}
	}()
	for range 100 {
		added.notifySourceEvent(eventPath)
	}
	done.Wait()
	mw.OnSourceEvent(nil)
	before = second.Load()
	added.notifySourceEvent(eventPath)
	require.Equal(t, before, second.Load())
}
