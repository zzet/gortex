package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

type publicationIntentWatcher struct {
	watcherHistory
	store     *store_sqlite.Store
	tickets   map[string]*indexer.MutationTicket
	protected atomic.Bool
	reject    bool
}

func (w *publicationIntentWatcher) EnqueueFileMutation(_ context.Context, path string) (*indexer.MutationTicket, error) {
	w.protected.Store(w.store.WriteWanted())
	if w.reject {
		return nil, nil
	}
	return w.tickets[path], nil
}

func (w *publicationIntentWatcher) EnqueueFileMutations(_ context.Context, paths []string) (map[string]*indexer.MutationTicket, error) {
	w.protected.Store(w.store.WriteWanted())
	return w.tickets, nil
}

func primaryPublicationIntentFixture(t *testing.T, releasePermit <-chan struct{}, paths ...string) (context.Context, context.CancelFunc, *sourceMutationWriteIntent, *publicationIntentWatcher, <-chan struct{}, func()) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "intent.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	released := make(chan struct{}, 1)
	intent := &sourceMutationWriteIntent{announce: func() func() {
		release := store.AnnounceWrite()
		return sync.OnceFunc(func() {
			release()
			select {
			case released <- struct{}{}:
			default:
			}
			if releasePermit != nil {
				<-releasePermit
			}
		})
	}}
	intent.release = intent.announce()
	ctx = context.WithValue(ctx, sourceMutationWriteIntentKey{}, intent)
	w := &publicationIntentWatcher{store: store, tickets: make(map[string]*indexer.MutationTicket)}
	var results []chan indexer.MutationResult
	for i, path := range paths {
		result := make(chan indexer.MutationResult, 1)
		results = append(results, result)
		w.tickets[path] = &indexer.MutationTicket{Path: path, Generation: uint64(i + 1), Done: result}
	}
	complete := sync.OnceFunc(func() {
		for i, result := range results {
			result <- indexer.MutationResult{RequestedGeneration: uint64(i + 1), AppliedGeneration: uint64(i + 1), Reindexed: true}
			close(result)
		}
	})
	t.Cleanup(cancel)
	return ctx, cancel, intent, w, released, complete
}

func awaitPrimaryPublicationIntentRelease(t *testing.T, released <-chan struct{}) {
	t.Helper()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("pending publication retained the ordinary write intent")
	}
}

func joinPrimaryPublicationWait(t *testing.T, done <-chan struct{}) bool {
	t.Helper()
	select {
	case <-done:
		return true
	case <-time.After(5 * time.Second):
		t.Error("publication wait did not finish during cleanup")
		return false
	}
}

func TestPrimaryMutationReceiptWaitSuspendsWriteIntent(t *testing.T) {
	for _, exit := range []string{"published", "canceled", "timer"} {
		t.Run(exit, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "edited.go")
			permit := make(chan struct{})
			unpark := sync.OnceFunc(func() { close(permit) })
			ctx, cancel, intent, w, released, complete := primaryPublicationIntentFixture(t, permit, path)
			s := &Server{watcher: w, mutationReindexWait: 500 * time.Millisecond}
			done := make(chan struct{})
			var outcome mutationReindexOutcome
			go func() {
				defer close(done)
				outcome = s.mutationReindexState(ctx, path)
			}()
			t.Cleanup(func() {
				cancel()
				unpark()
				complete()
				joinPrimaryPublicationWait(t, done)
				intent.close()
			})
			awaitPrimaryPublicationIntentRelease(t, released)
			require.True(t, w.protected.Load(), "ticket admission retains foreground preemption")
			require.False(t, w.store.WriteWanted(), "an admitted passive wait announces no new writer")
			unpark()
			switch exit {
			case "published":
				complete()
			case "canceled":
				cancel()
			}
			require.True(t, joinPrimaryPublicationWait(t, done))
			if exit == "published" {
				require.True(t, outcome.Reindexed)
				require.False(t, outcome.Pending)
			} else {
				require.True(t, outcome.Pending)
				require.NotEmpty(t, outcome.Receipt)
			}
			require.Equal(t, exit != "canceled", w.store.WriteWanted(), "live outer handlers regain admission priority; canceled handlers announce no new write")
		})
	}
}

func TestPrimaryBatchReceiptWaitSuspendsWriteIntentBeforeJournal(t *testing.T) {
	paths := []string{filepath.Join(t.TempDir(), "caller.go"), filepath.Join(t.TempDir(), "target.go")}
	permit := make(chan struct{})
	unpark := sync.OnceFunc(func() { close(permit) })
	ctx, cancel, intent, w, released, complete := primaryPublicationIntentFixture(t, permit, paths...)
	s := &Server{watcher: w, mutationReindexWait: 3 * time.Second}
	var journalProtected atomic.Bool
	s.batchDurabilityOverride = &batchDurabilityOps{
		writeFile: func(string, []byte, os.FileMode) error {
			journalProtected.Store(w.store.WriteWanted())
			return nil
		},
		syncDirectory: func(string) error { return nil },
	}
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	state := batchSetState(paths...)
	receipt := state.snapshot()
	receipt.TransactionID = "intent-batch"
	state.publish(receipt, false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.refreshBatchGraph(ctx, state)
	}()
	t.Cleanup(func() {
		cancel()
		unpark()
		complete()
		joinPrimaryPublicationWait(t, done)
		intent.close()
	})
	awaitPrimaryPublicationIntentRelease(t, released)
	require.True(t, w.protected.Load(), "the complete frontier is admitted with ordinary priority")
	require.False(t, w.store.WriteWanted())
	unpark()
	complete()
	require.True(t, joinPrimaryPublicationWait(t, done))
	require.Equal(t, "fresh", state.snapshot().GraphStatus)
	require.True(t, journalProtected.Load(), "the passive wait resumes before durable receipt writes")
	require.True(t, w.store.WriteWanted())
}

func TestPrimaryMutationFallbackKeepsOrdinaryWriteIntent(t *testing.T) {
	for _, mode := range []string{"nil_ticket", "no_watcher", "failed_ticket"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "edited.go")
			ctx, cancel, intent, w, released, complete := primaryPublicationIntentFixture(t, nil, path)
			defer cancel()
			defer intent.close()
			defer complete()
			s := &Server{watcher: w}
			switch mode {
			case "nil_ticket":
				w.reject = true
			case "no_watcher":
				s.watcher = nil
			case "failed_ticket":
				s.watcher = mutationTestWatcher{scheduleErr: errors.New("admission refused")}
			}
			s.mutationReindexState(ctx, path)
			select {
			case <-released:
				t.Fatal("non-admitted/synchronous path released ordinary admission intent")
			default:
			}
			require.True(t, w.store.WriteWanted())
		})
	}
}
