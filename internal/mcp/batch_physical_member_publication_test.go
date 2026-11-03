package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
)

// Pause only the actual set admission, before its background wake. Execution
// and publication remain the real watcher/parser path, not a ticket fixture.
type batchPhysicalMemberWatcher struct {
	*indexer.Watcher
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	sets    atomic.Int64
	points  atomic.Int64
}

func (w *batchPhysicalMemberWatcher) EnqueueFileMutations(ctx context.Context, paths []string) (map[string]*indexer.MutationTicket, error) {
	w.sets.Add(1)
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.Watcher.EnqueueFileMutations(ctx, paths)
}

func (w *batchPhysicalMemberWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	w.points.Add(1)
	return w.Watcher.EnqueueToolMutation(ctx, path)
}

func TestBatchPhysicalMembersRetainOriginalPublicationThroughRealSet(t *testing.T) {
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s, root := setupTestServer(t)
	s.editAckMode = config.EditAckLocal
	s.mutationReindexWait = time.Nanosecond
	paths := []string{filepath.Join(root, "batch-first.go"), filepath.Join(root, "batch-second.go")}
	before := "package main\nfunc BatchBefore() {}\n"
	after := "package main\nfunc BatchAfter() {}\n"
	for _, path := range paths {
		require.NoError(t, os.WriteFile(path, []byte(before), 0600))
	}
	_, err := s.indexer.IncrementalReindexPaths(root, paths)
	require.NoError(t, err)
	real, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, real.Start([]string{t.TempDir()}))
	w := &batchPhysicalMemberWatcher{Watcher: real, entered: make(chan struct{}), release: make(chan struct{})}
	s.SetWatcher(w)
	t.Cleanup(func() { require.NoError(t, real.Stop()); s.DrainBackground(); s.indexer.Close() })
	req := editOperationRequest("batch_edit", "")
	req.Params.Arguments = map[string]any{"transaction_id": "physical-members-original", "edits": []any{
		map[string]any{"op": "edit_file", "path": paths[0], "old_string": before, "new_string": after},
		map[string]any{"op": "edit_file", "path": paths[1], "old_string": before, "new_string": after},
	}}
	done := make(chan error, 1)
	go func() {
		_, callErr := s.boundToolHandler(s.handleAtomicBatchEdit)(canonicalTimingContext(), req)
		done <- callErr
	}()
	var release sync.Once
	unblock := func() { release.Do(func() { close(w.release) }) }
	defer func() { unblock(); require.NoError(t, <-done) }()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("real batch did not reach its one set admission")
	}
	value, ok := s.batchTransactions.Load("physical-members-original")
	require.True(t, ok)
	transaction := value.(*batchTransactionState).snapshot()
	require.Equal(t, "committed", transaction.DiskStatus)
	refs := s.batchFileObservationReferences(transaction)
	require.Len(t, refs, 2, "each real forward writer must retain its original physical member before set wake")
	originals := make(map[string]*indexer.PublicationPhaseRecord, 2)
	records := make([]*mutationCommitRecord, 0, 2)
	for _, ref := range refs {
		s.mutationCommits.mu.Lock()
		record := s.mutationCommits.byID[ref.Receipt]
		s.mutationCommits.mu.Unlock()
		require.NotNil(t, record)
		record.mu.RLock()
		publication, committed, sequence := record.publication, record.disk, record.commitSeq
		record.mu.RUnlock()
		require.Equal(t, mutationDiskCommitted, committed)
		require.Positive(t, sequence)
		require.NotNil(t, publication, "publication must precede the actual set wake")
		require.False(t, publication.Snapshot().Terminal)
		originals[ref.Receipt] = publication
		records = append(records, record)
		pending, _ := s.batchFileObservationPayload(t.Context(), ref.Receipt)
		require.NotContains(t, pending, "shared_owner_accounting_v1")
	}
	unblock()
	for _, ref := range refs {
		var payload map[string]any
		require.Eventually(t, func() bool {
			payload, _ = s.batchFileObservationPayload(t.Context(), ref.Receipt)
			return payload["publication_fresh"] == true && payload["shared_owner_accounting_v1"] != nil
		}, 5*time.Second, time.Millisecond, "batch member needs its own real terminal publication and exact shared owner")
		proof := payload["shared_owner_accounting_v1"].(map[string]any)
		definition := proof["definition"].(map[string]any)
		require.Len(t, definition["members"], 2)
		member := proof["reference"].(map[string]any)["member"].(map[string]any)
		require.Equal(t, ref.Receipt, member["receipt"])
		require.Equal(t, originals[ref.Receipt].Snapshot().Key, member["publication_key"], "the existing recorder rekeys to the actual publication ticket")
		for _, record := range records {
			if record.id != ref.Receipt {
				continue
			}
			record.mu.RLock()
			require.Equal(t, record.absPath, member["path"])
			require.Equal(t, record.commitSeq, member["physical_commit_sequence"])
			require.Equal(t, record.appliedGeneration, member["ticket"])
			sum := sha256.Sum256([]byte(after))
			require.Equal(t, hex.EncodeToString(sum[:]), member["content_sha256"])
			record.mu.RUnlock()
		}
		require.Equal(t, "not_applicable_shared_transaction_rpc", payload["acknowledgement_state"])
		require.True(t, originals[ref.Receipt].Snapshot().Terminal)
	}
	// Each negative invalidates the complete same-status membership. A peer's
	// successful publication cannot donate the changed record's original owner.
	peer := records[1]
	for _, change := range []struct {
		name   string
		mutate func() func()
	}{
		{"same_bytes_new_sequence", func() func() { old := peer.commitSeq; peer.commitSeq++; return func() { peer.commitSeq = old } }},
		{"different_hash", func() func() {
			old := peer.contentSHA256
			peer.contentSHA256 = gitBlobSHA([]byte("foreign"))
			return func() { peer.contentSHA256 = old }
		}},
		{"foreign_path", func() func() { old := peer.absPath; peer.absPath += ".foreign"; return func() { peer.absPath = old } }},
		{"foreign_server", func() func() { old := peer.owner; peer.owner = &Server{}; return func() { peer.owner = old } }},
		{"rebound_publication", func() func() {
			old := peer.publication
			peer.publication = originals[refs[0].Receipt]
			return func() { peer.publication = old }
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			peer.mu.Lock()
			restore := change.mutate()
			peer.mu.Unlock()
			defer func() { peer.mu.Lock(); restore(); peer.mu.Unlock() }()
			payload, _ := s.batchFileObservationPayload(t.Context(), refs[0].Receipt)
			require.NotContains(t, payload, "shared_owner_accounting_v1")
		})
	}
	s.mutationCommits.mu.Lock()
	delete(s.mutationCommits.byID, peer.id)
	s.mutationCommits.mu.Unlock()
	missing, _ := s.batchFileObservationPayload(t.Context(), refs[0].Receipt)
	require.NotContains(t, missing, "shared_owner_accounting_v1", "evicted peer cannot borrow retained recorder evidence")
	s.mutationCommits.mu.Lock()
	s.mutationCommits.byID[peer.id] = peer
	s.mutationCommits.mu.Unlock()
	restored, _ := s.batchFileObservationPayload(t.Context(), refs[0].Receipt)
	require.Contains(t, restored, "shared_owner_accounting_v1")
	_, err = s.boundToolHandler(s.handleAtomicBatchEdit)(canonicalTimingContext(), req)
	require.NoError(t, err)
	require.Equal(t, refs, s.batchFileObservationReferences(transaction), "duplicate keeps original writer observations")
	require.EqualValues(t, 1, w.sets.Load())
	require.Zero(t, w.points.Load(), "an atomic frontier cannot be retried pointwise")
	for _, path := range paths {
		require.Equal(t, after, readAtomicBatchFixture(t, path))
	}
}
