package mcp

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// A hot-path component proof, not a registered-search acceptance run. The
// existing fixture supplies a real local ACK and keeps canonical publication
// held. Reserve readers only after that setup so unrelated selection SQL is
// excluded from the measured mechanism.
func TestSameByteSuspectRefreshReusesCheckedLayerWithoutSQL(t *testing.T) {
	s, backend, caller, path, original := searchReaderWaitFixture(t)
	oldFacts, err := s.bindPendingSearch(caller)
	require.NoError(t, err)
	require.NoError(t, validateRequestProjections(oldFacts))
	retirement := s.canonicalProjectionRetirement(path)
	require.NotNil(t, retirement)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	beforeSHA := sha256.Sum256(before)
	require.Nil(t, backend.GetNode("main.go::Target"), "actual canonical rows still precede the ACK")
	pendingFileProjections.markSuspect(path)
	require.True(t, original.suspect.Load())
	require.ErrorIs(t, validateRequestProjections(oldFacts), projectionSuperseded)

	count := min(runtime.NumCPU(), 4)
	poolRelease := make(chan struct{})
	releasePool := sync.OnceFunc(func() { close(poolRelease) })
	entered := make(chan struct{}, count)
	holderErrors := make(chan error, count)
	fixtureRelease := errors.New("same-byte fixture scope released")
	var holders sync.WaitGroup
	t.Cleanup(func() { releasePool(); holders.Wait() })
	for range count {
		holders.Go(func() {
			holderErrors <- backend.WithFileEditingContextReader(t.Context(), func(context.Context, graph.FileEditingContextRows) error {
				entered <- struct{}{}
				<-poolRelease
				return fixtureRelease
			})
		})
	}
	for range count {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("actual SQLite readers were not reserved")
		}
	}
	// The observer delegates to actual SQL and introduces no sleeping/fake
	// metadata result. Its optional gate is already open before the refresh.
	close(backend.query)
	backend.armed.Store(true)
	poolBefore := backend.ReaderWaitMark()
	build, cancel := context.WithTimeout(caller, projectedReadBudget)
	defer cancel()
	type answer struct {
		entry *projectionEntry
		err   error
	}
	answered := make(chan answer, 1)
	var refresh sync.WaitGroup
	refresh.Go(func() {
		entry, err := s.projectCurrentTrackedFile(build, caller, path, "main.go", "main.go")
		answered <- answer{entry: entry, err: err}
	})
	t.Cleanup(func() { cancel(); releasePool(); refresh.Wait() })
	var result answer
	returnedWhileHeld := false
	select {
	case result = <-answered:
		returnedWhileHeld = true
	case <-time.After(time.Second):
		cancel()
		releasePool()
		result = <-answered
	}
	refresh.Wait()
	metadataCalled := false
	select {
	case <-backend.entered:
		metadataCalled = true
	default:
	}
	poolAfter := backend.ReaderWaitMark()
	privateErr := build.Err()
	oldFactsErr := validateRequestProjections(oldFacts)
	after, readErr := os.ReadFile(path)
	backend.armed.Store(false) // Later lifecycle SQL is a separate continuation.
	releasePool()
	holders.Wait()
	for range count {
		require.ErrorIs(t, <-holderErrors, fixtureRelease)
	}
	require.NotNil(t, result.entry)
	require.Same(t, result.entry, liveEntry(pendingFileProjections, path), "actual new COW owner must precede canonical retirement")
	// The pre-refresh captured callback must still retire whichever checked
	// owner is current after real IndexFile publication, including read-COW.
	require.NoError(t, s.indexer.IndexFile(path))
	canonical := backend.GetNode("main.go::Target")
	require.NotNil(t, canonical)
	require.Equal(t, 3, canonical.StartLine)
	retirement.run()
	require.True(t, result.entry.checkedRetired.Load(), "actual canonical callback retires the NEW COW owner")
	require.Nil(t, liveEntry(pendingFileProjections, path))
	t.Logf("same_bytes=%v returned_while_SQL_held=%v metadata_called=%v real_pool_wait_delta=%d private_error=%v refresh_error=%v entry_present=%v canonical_retired=true", beforeSHA == sha256.Sum256(after), returnedWhileHeld, metadataCalled, poolAfter.PoolWaits-poolBefore.PoolWaits, privateErr, result.err, result.entry != nil)
	// Every worker and real SQL scope is joined before the causal assertions.
	require.NoError(t, readErr)
	require.Equal(t, beforeSHA, sha256.Sum256(after))
	require.NoError(t, caller.Err())
	require.ErrorIs(t, oldFactsErr, projectionSuperseded, "same-byte reuse cannot revive a previous request")
	require.True(t, returnedWhileHeld)
	require.NoError(t, result.err, "verified immutable reuse must not wait for optional canonical metadata")
	require.NoError(t, privateErr, "reuse must finish under the original150ms, without extending it")
	require.False(t, metadataCalled)
	require.Equal(t, poolBefore.PoolWaits, poolAfter.PoolWaits)
	require.NotNil(t, result.entry)
	require.NotSame(t, original, result.entry)
	require.Same(t, original.layer, result.entry.layer)
	require.Same(t, original.names, result.entry.names)
	require.Equal(t, original.fileView, result.entry.fileView)
	require.Equal(t, original.rawHash, result.entry.rawHash)
	require.Equal(t, original.sourceCommitSeq, result.entry.sourceCommitSeq)
	require.Zero(t, result.entry.commitSeq)
	require.True(t, result.entry.readVersionChecked)
	require.True(t, original.suspect.Load())
}

// Scheduling observers cannot authorize publication after the original build
// context is canceled, even though the bytes were verified before the callback.
func TestSameByteReuseObserverCannotBypassOriginalCancellation(t *testing.T) {
	s, _, caller, path, original := searchReaderWaitFixture(t)
	pendingFileProjections.markSuspect(path)
	build, cancel := context.WithCancel(caller)
	defer cancel()
	var observed *projectionEntry
	build = context.WithValue(build, checkedProjectionReadObserverKey{}, checkedProjectionReadObserver(func(ctx context.Context, entry *projectionEntry) {
		observed = entry
		cancel()
	}))
	entry, err := s.projectCurrentTrackedFile(build, caller, path, "main.go", "main.go")
	require.Same(t, original, observed)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, entry)
	require.Same(t, original, liveEntry(pendingFileProjections, path))
	require.True(t, original.suspect.Load(), "the observer cannot publish or rehabilitate the old owner")
}
