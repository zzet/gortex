package indexer

import (
	"context"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMutationSetBindsEachOwnedRecordBeforeActualDrain(t *testing.T) {
	root, _, w := inertMutationSetWatcher(t, "a.go", "package set\nfunc A(){}\n")
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")
	writeTestFile(t, b, "package set\nfunc B(){}\n")
	owner := NewPublicationPhaseRecorder(8, 2)
	t.Cleanup(func() { runtime.KeepAlive(owner) })
	first := owner.Begin("", "set-first", PublicationSourceMCPEdit, time.Now())
	second := owner.Begin("", "set-second", PublicationSourceMCPEdit, time.Now())
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	w.stormBeforeLock = func() { close(entered); <-release }
	t.Cleanup(func() { unblock.Do(func() { close(release) }); require.NoError(t, w.Stop()) })
	ctx := WithFilePublicationRecords(context.Background(), map[string]*PublicationPhaseRecord{a: first, b: second})
	tickets, err := w.EnqueueFileMutations(ctx, []string{a, b})
	require.NoError(t, err)
	require.Len(t, tickets, 2)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual atomic drain did not enter")
	}
	early := first.Snapshot()
	require.Nil(t, early.SharedSetExecution(), "no shared span exists before the actual executor returns")
	for path, record := range map[string]*PublicationPhaseRecord{a: first, b: second} {
		ticket := tickets[path]
		require.Same(t, record, ticket.PublicationRecord, "actual set ticket dropped its corresponding original record")
		snapshot := record.Snapshot()
		require.Equal(t, ticket.Generation, snapshot.Ticket)
		found := false
		for _, phase := range snapshot.Phases {
			if phase.Phase == PublicationTicketEnqueued {
				found = true
			}
		}
		require.True(t, found, "bind-before-wake must retain actual enqueue, not infer it after admission")
	}
	unblock.Do(func() { close(release) })
	for _, ticket := range tickets {
		result := awaitMutationSetTicket(t, ticket)
		require.NoError(t, result.Err)
		snapshot := ticket.PublicationRecord.Snapshot()
		shared := snapshot.SharedSetExecution()
		require.NotNil(t, shared, "actual set execution dropped its exact member observation")
		require.Equal(t, "shared_mutation_set_executor", shared.Scope)
		require.True(t, shared.NonadditiveAcrossMembers)
		require.Equal(t, ticket.Generation, shared.MemberGeneration)
		require.Equal(t, 2, shared.ObservedMembers)
		require.NotNil(t, shared.Accounting.PhasesMS["patch_parse"], "real parser phases must flow through mode.publication")
		require.Nil(t, snapshot.attemptWindow, "shared executor must never impersonate an independent point attempt")
		timings := snapshot.EditTimings()
		require.NotNil(t, timings.Window.SharedSetExecution)
		require.Nil(t, timings.Window.PatchWallMS)
		require.Nil(t, timings.Window.PatchAccounting)
		require.Nil(t, timings.Milliseconds["patch_parse"], "shared parser work is not an independent per-file P lap")
	}
	require.Nil(t, early.SharedSetExecution(), "prior snapshots stay immutable")
}

func TestMutationSetSingleAndDeduplicatedFallbackKeepExactRecord(t *testing.T) {
	for _, multi := range []bool{false, true} {
		for _, duplicate := range []bool{false, true} {
			name := map[bool]string{false: "watcher", true: "multi"}[multi] + map[bool]string{false: "-single", true: "-deduplicated"}[duplicate]
			t.Run(name, func(t *testing.T) {
				root, idx, w := inertMutationSetWatcher(t, "a.go", "package set\nfunc A(){}\n")
				t.Cleanup(func() { require.NoError(t, w.Stop()) })
				path := filepath.Join(root, "a.go")
				owner := NewPublicationPhaseRecorder(8, 2)
				t.Cleanup(func() { runtime.KeepAlive(owner) })
				record := owner.Begin("", "fallback", PublicationSourceMCPEdit, time.Now())
				ctx := WithFilePublicationRecords(context.Background(), map[string]*PublicationPhaseRecord{path: record})
				paths := []string{path}
				if duplicate {
					paths = append(paths, path)
				}
				var tickets map[string]*MutationTicket
				var err error
				if multi {
					mi := &MultiIndexer{repos: map[string]*RepoMetadata{"repo": {RepoPrefix: "repo", RootPath: root}}, indexers: map[string]*Indexer{"repo": idx}}
					mw := &MultiWatcher{multi: mi, watchers: map[string]*Watcher{"repo": w}, started: map[string]bool{"repo": true}}
					tickets, err = mw.EnqueueFileMutations(ctx, paths)
				} else {
					tickets, err = w.EnqueueFileMutations(ctx, paths)
				}
				require.NoError(t, err)
				require.Len(t, tickets, 1)
				require.Same(t, record, tickets[path].PublicationRecord)
				result := awaitMutationSetTicket(t, tickets[path])
				require.NoError(t, result.Err)
			})
		}
	}
}

func TestMutationSetNewerExecutorCannotDonateSpanToOlderRecords(t *testing.T) {
	root, _, w := inertMutationSetWatcher(t, "a.go", "package set\nfunc A(){}\n")
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")
	writeTestFile(t, b, "package set\nfunc B(){}\n")
	owner := NewPublicationPhaseRecorder(8, 2)
	defer runtime.KeepAlive(owner)
	entered, release := make(chan struct{}), make(chan struct{})
	var first, unblock sync.Once
	w.stormBeforeLock = func() { first.Do(func() { close(entered) }); <-release }
	t.Cleanup(func() { unblock.Do(func() { close(release) }); require.NoError(t, w.Stop()) })
	oldA := owner.Begin("checkout", "old-a", PublicationSourceMCPEdit, time.Now())
	oldB := owner.Begin("checkout", "old-b", PublicationSourceMCPEdit, time.Now())
	older, err := w.EnqueueFileMutations(WithFilePublicationRecords(context.Background(), map[string]*PublicationPhaseRecord{a: oldA, b: oldB}), []string{a, b})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first actual drain did not enter")
	}
	writeTestFile(t, a, "package set\nfunc NewA(){}\n")
	writeTestFile(t, b, "package set\nfunc NewB(){}\n")
	newA := owner.Begin("checkout", "new-a", PublicationSourceMCPEdit, time.Now())
	newB := owner.Begin("checkout", "new-b", PublicationSourceMCPEdit, time.Now())
	newer, err := w.EnqueueFileMutations(WithFilePublicationRecords(context.Background(), map[string]*PublicationPhaseRecord{a: newA, b: newB}), []string{a, b})
	require.NoError(t, err)
	unblock.Do(func() { close(release) })
	for _, tickets := range []map[string]*MutationTicket{older, newer} {
		for _, ticket := range tickets {
			require.NoError(t, awaitMutationSetTicket(t, ticket).Err)
		}
	}
	require.Nil(t, oldA.Snapshot().SharedSetExecution())
	require.Nil(t, oldB.Snapshot().SharedSetExecution())
	for _, ticket := range newer {
		shared := ticket.PublicationRecord.Snapshot().SharedSetExecution()
		require.NotNil(t, shared)
		require.Equal(t, ticket.Generation, shared.MemberGeneration)
		require.True(t, shared.NonadditiveAcrossMembers)
	}
}

func TestMutationSetPostExecutorPanicCannotPublishCompletedBackground(t *testing.T) {
	root, _, w := inertMutationSetWatcher(t, "a.go", "package set\nfunc A(){}\n")
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")
	writeTestFile(t, b, "package set\nfunc B(){}\n")
	owner := NewPublicationPhaseRecorder(8, 2)
	defer runtime.KeepAlive(owner)
	first := owner.Begin("checkout", "panic-a", PublicationSourceMCPEdit, time.Now())
	second := owner.Begin("checkout", "panic-b", PublicationSourceMCPEdit, time.Now())
	_, storageErr := indexCtxRawStorageError(t)
	w.stormDrained = func(int) { panic(storageErr) }
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	tickets, err := w.EnqueueFileMutations(WithFilePublicationRecords(context.Background(), map[string]*PublicationPhaseRecord{a: first, b: second}), []string{a, b})
	require.NoError(t, err)
	for _, ticket := range tickets {
		require.ErrorIs(t, awaitMutationSetTicket(t, ticket).Err, storageErr)
	}
	w.stormWork.Wait()
	for _, record := range []*PublicationPhaseRecord{first, second} {
		shared := record.Snapshot().SharedSetExecution()
		require.NotNil(t, shared, "raw executor actually returned before the injected completion panic")
		require.Nil(t, shared.Background, "a failed completion cannot certify a normal background return")
	}
}
