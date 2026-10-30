package mcp

import (
	"context"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestMoveFileObservationActualDirectReconcileIsBoundPerFile(t *testing.T) {
	s, _ := setupContractMultiRepoServer(t, "fixture")
	t.Cleanup(func() {
		s.DrainBackground()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, s.multiIndexer.Close(ctx))
	})
	root, ok := s.multiIndexer.RepoRoot("fixture")
	require.True(t, ok)
	sourcePath, targetPath := filepath.Join(root, "source.go"), filepath.Join(root, "target.go")
	require.NoError(t, os.WriteFile(sourcePath, []byte("package main\nfunc Relocated() int { return 7 }\nfunc StableCaller() int { return Relocated() }\n"), 0600))
	require.NoError(t, os.WriteFile(targetPath, []byte("package main\nfunc DestinationAnchor() int { return 2 }\n"), 0600))
	_, err := s.multiIndexer.IncrementalReindexRepo("fixture", []string{sourcePath, targetPath})
	require.NoError(t, err)
	oldID, newID := "fixture/source.go::Relocated", "fixture/target.go::Relocated"
	s.facades.capture(mcpgo.NewTool("move_symbol"), s.handleMoveSymbol)
	req := editOperationRequest("refactor", "move")
	req.Params.Arguments = map[string]any{"operation": "move", "target": map[string]any{"symbol": oldID}, "destination": "fixture/target.go", "output": map[string]any{"format": "json"}}
	result, err := s.boundToolHandler(func(ctx context.Context, r mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return s.handleFacade(ctx, "refactor", r)
	})(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)
	body := unmarshalResult(t, result)
	require.Equal(t, oldID, body["source_id"])
	require.Equal(t, newID, body["target_id"])
	operation, ok := body["move_observation_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, operation)
	refs, ok := body["file_observations"].([]any)
	require.True(t, ok)
	require.Len(t, refs, 2)
	sharedRPC := ""
	for _, raw := range refs {
		ref := raw.(map[string]any)
		id := ref["receipt"].(string)
		var status map[string]any
		require.Eventually(t, func() bool {
			status, _ = s.moveFileObservationPayload(t.Context(), id)
			return status["direct_reconcile_state"] == "observed"
		}, time.Second, time.Millisecond, "actual direct owner evidence missing: %v", status)
		require.Equal(t, "move_file_observation", status["receipt_kind"])
		require.Equal(t, operation, status["operation_id"])
		require.Equal(t, oldID, status["source_id"])
		require.Equal(t, newID, status["target_id"])
		require.Equal(t, ref["path"], status["path"])
		require.Equal(t, ref["role"], status["role"])
		require.Equal(t, "all_writes_completed", status["move_write_verdict"])
		require.Equal(t, mutationDiskCommitted, status["file_write_status"])
		require.Equal(t, true, status["writer_return_observed"])
		require.Equal(t, "not_applicable_direct_reconcile", status["requested_background_state"])
		require.NotContains(t, status, "mutation_id")
		require.NotContains(t, status, "transaction_id")
		require.NotContains(t, status, "reindex_receipt")
		ack := status["acknowledgement"].(acknowledgementPayload)
		require.NotNil(t, ack.RPCID)
		if sharedRPC == "" {
			sharedRPC = *ack.RPCID
		} else {
			require.Equal(t, sharedRPC, *ack.RPCID, "shared A must be deduplicated by its actual RPC owner")
		}
		direct := status["direct_reconcile"].(map[string]any)
		require.Equal(t, "move_direct_reconcile", direct["scope"])
		require.Equal(t, "monotonic", direct["clock"])
		require.Equal(t, status["new_sha"], direct["accepted_blob_sha"])
		require.Positive(t, direct["owner_generation"])
		require.Positive(t, direct["window_ns"])
		require.Equal(t, false, direct["additive_with_acknowledgement"])
		phases := direct["phase_accounting"].(map[string]any)
		require.Equal(t, "move_direct_reconcile", phases["scope"])
		require.Equal(t, true, phases["observed_partition_complete"])
		require.Equal(t, direct["window_ns"], phases["window_ns"])
		require.Equal(t, phases["window_ns"], phases["observed_subtotal_ns"])
		states := phases["phase_states"].(map[string]string)
		require.Len(t, states, 9)
		for name, state := range states {
			require.Equal(t, "measured", state, "missing actual move executor phase %s", name)
		}
		direct["accepted_blob_sha"] = "foreign"
		again, _ := s.moveFileObservationPayload(t.Context(), id)
		require.Equal(t, status["new_sha"], again["direct_reconcile"].(map[string]any)["accepted_blob_sha"], "caller mutation changed retained observation")
	}
}

func moveObservationExecutionFixture(t *testing.T) (*Server, *moveObservation, context.Context, *editAcknowledgement, string, string) {
	t.Helper()
	s, _ := setupContractMultiRepoServer(t, "fixture")
	t.Cleanup(func() {
		s.DrainBackground()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, s.multiIndexer.Close(ctx))
	})
	root, ok := s.multiIndexer.RepoRoot("fixture")
	require.True(t, ok)
	source, target := filepath.Join(root, "source.go"), filepath.Join(root, "target.go")
	require.NoError(t, os.WriteFile(source, []byte("package main\nfunc Before() {}\n"), 0600))
	require.NoError(t, os.WriteFile(target, []byte("package main\nfunc OldTarget() {}\n"), 0600))
	_, err := s.multiIndexer.IncrementalReindexRepo("fixture", []string{source, target})
	require.NoError(t, err)
	ctx, ack, _ := s.withEditAcknowledgement(t.Context())
	t.Cleanup(ack.finishActual)
	observation := s.beginMoveObservation(ctx, "fixture/source.go::Before", "fixture/target.go::Before", source, target, "fixture/source.go", "fixture/target.go", true)
	require.NotNil(t, observation)
	require.NoError(t, observation.write(ack, target, []byte("package main\nfunc Moved() {}\n")))
	require.NoError(t, observation.write(ack, source, []byte("package main\nfunc After() { Existing() }\n")))
	observation.finishWrites(true)
	return s, observation, ctx, ack, source, target
}
func captureMoveActualExecution(t *testing.T, s *Server, path string, observer *indexer.ReadRepairObserver) indexer.ReadRepairObservation {
	t.Helper()
	done := make(chan indexer.ReadRepairObservation, 1)
	wrapped := &indexer.ReadRepairObserver{Complete: func(o indexer.ReadRepairObservation) {
		if observer != nil {
			observer.Complete(o)
		}
		done <- o
	}}
	if observer != nil {
		wrapped.Session, wrapped.RequestIDKey = observer.Session, observer.RequestIDKey
	}
	_, err := s.multiIndexer.IncrementalReindexRepoObserved("fixture", []string{path}, wrapped)
	require.NoError(t, err)
	select {
	case actual := <-done:
		return actual
	case <-time.After(5 * time.Second):
		t.Fatal("actual direct observer did not join")
		return indexer.ReadRepairObservation{}
	}
}

func TestMoveDirectRefusesOlderAdmissionWhichActuallyCommitsLaterSameBytes(t *testing.T) {
	s, _ := setupContractMultiRepoServer(t, "fixture")
	t.Cleanup(func() { s.DrainBackground(); require.NoError(t, s.multiIndexer.Close(context.Background())) })
	root, ok := s.multiIndexer.RepoRoot("fixture")
	require.True(t, ok)
	source, target := filepath.Join(root, "source.go"), filepath.Join(root, "target.go")
	bytes := []byte("package main\nfunc After() { Existing() }\n")
	require.NoError(t, os.WriteFile(source, []byte("package main\nfunc Before() {}\n"), 0600))
	require.NoError(t, os.WriteFile(target, []byte("package main\nfunc OldTarget() {}\n"), 0600))
	_, err := s.multiIndexer.IncrementalReindexRepo("fixture", []string{source, target})
	require.NoError(t, err)
	entered, release := make(chan *mutationCommitRecord, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	s.mutationPreCommitHook = func(record *mutationCommitRecord) { entered <- record; <-release }
	type result struct {
		record *mutationCommitRecord
		err    error
	}
	done := make(chan result, 1)
	joined := false
	go func() {
		record, err := s.commitFileMutation(t.Context(), "edit_file", "older-admission", "same-bytes", "fixture/source.go", source, bytes, 0600)
		done <- result{record, err}
	}()
	t.Cleanup(func() {
		unblock()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("competing real writer was not joined")
			}
		}
	})
	var older *mutationCommitRecord
	select {
	case older = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("older writer not admitted")
	}
	ctx, ack, _ := s.withEditAcknowledgement(t.Context())
	defer ack.finishActual()
	observation := s.beginMoveObservation(ctx, "fixture/source.go::Before", "fixture/target.go::Before", source, target, "fixture/source.go", "fixture/target.go", true)
	require.NoError(t, observation.write(ack, target, []byte("package main\nfunc Moved() {}\n")))
	require.NoError(t, observation.write(ack, source, bytes))
	observation.finishWrites(true)
	idx := s.multiIndexer.GetIndexer("fixture")
	observer := observation.observer(ctx, source, idx)
	require.NotNil(t, observer)
	unblock()
	var finished result
	select {
	case finished = <-done:
		joined = true
	case <-time.After(5 * time.Second):
		t.Fatal("older actual writer did not return")
	}
	require.NoError(t, finished.err)
	require.Same(t, older, finished.record)
	record := observation.records[source]
	require.Greater(t, older.commitSeq, record.commitSeq, "guard must exercise actual commit order opposite admission order")
	actual := captureMoveActualExecution(t, s, source, observer)
	require.True(t, actual.AcceptedFor(idx, gitBlobSHA(bytes)), "wrong-stage rejection would not test competing commit fence")
	status, _ := s.moveFileObservationPayload(t.Context(), record.id)
	require.Equal(t, "unavailable_competing_commit", status["direct_reconcile_state"])
	require.NotContains(t, status, "direct_reconcile")
}

func TestMoveDirectRejectsActualEarlyAcceptanceForeignOwnerBytesAndEviction(t *testing.T) {
	for _, kind := range []string{"early_acceptance", "foreign_owner", "foreign_bytes", "evicted"} {
		t.Run(kind, func(t *testing.T) {
			s, observation, ctx, ack, source, target := moveObservationExecutionFixture(t)
			idx := s.multiIndexer.GetIndexer("fixture")
			observer := observation.observer(ctx, source, idx)
			require.NotNil(t, observer)
			record := observation.records[source]
			if kind == "foreign_owner" {
				foreign := indexer.New(graph.New(), testRegistry(), config.Default().Index, zap.NewNop())
				observer = observation.observer(ctx, source, foreign)
				require.NotNil(t, observer)
			}
			if kind == "foreign_bytes" {
				require.NoError(t, writeMoveFileObserved(nil, source, []byte("package main\nfunc OtherBytes() { Existing() }\n")))
			}
			actual := captureMoveActualExecution(t, s, source, nil)
			if kind == "early_acceptance" {
				ctx2, ack2, _ := s.withEditAcknowledgement(t.Context())
				defer ack2.finishActual()
				second := s.beginMoveObservation(ctx2, "fixture/source.go::Before", "fixture/target.go::Before", source, target, "fixture/source.go", "fixture/target.go", true)
				require.NoError(t, second.write(ack2, target, []byte("package main\nfunc Moved() {}\n")))
				require.NoError(t, second.write(ack2, source, []byte("package main\nfunc After() { Existing() }\n")))
				second.finishWrites(true)
				observation, record = second, second.records[source]
				observer = second.observer(ctx2, source, idx)
				require.NotNil(t, observer)
				require.True(t, actual.AcceptedFor(idx, record.newSHA))
				require.True(t, actual.AcceptedAt().Before(record.committedAt), "actual acceptance must precede the new real writer return")
			}
			if kind == "evicted" {
				require.True(t, actual.AcceptedFor(idx, record.newSHA))
				record.mu.Lock()
				record.startedAt = time.Now().Add(-mutationCommitRetention - time.Second)
				record.mu.Unlock()
				s.mutationCommits.mu.Lock()
				s.mutationCommits.evictLocked(time.Now())
				s.mutationCommits.mu.Unlock()
			}
			observer.Complete(actual)
			status, _ := s.moveFileObservationPayload(t.Context(), record.id)
			if kind == "evicted" {
				require.Equal(t, false, status["found"])
			} else {
				require.NotContains(t, status, "direct_reconcile")
				require.Equal(t, "unavailable_owner_bytes_or_acceptance", status["direct_reconcile_state"])
			}
			_ = ack // shared owner remains live until cleanup
			_ = observation
		})
	}
}

func TestMoveFileWriteWindowsPartialFailureAndUnobservedIsolation(t *testing.T) {
	s, _, ctx, ack, source, target := moveObservationExecutionFixture(t)
	observation := s.beginMoveObservation(ctx, "fixture/source.go::Before", "fixture/target.go::Before", source, target, "fixture/source.go", "fixture/target.go", true)
	targetBytes := []byte("package main\nfunc ActuallyWritten() {}\n")
	require.NoError(t, observation.write(ack, target, targetBytes))
	require.NoError(t, os.Remove(source))
	require.NoError(t, os.Mkdir(source, 0700))
	require.Error(t, observation.write(ack, source, []byte("package main\nfunc CannotReplaceDirectory() {}\n")))
	observation.finishWrites(false)
	targetStatus, _ := s.moveFileObservationPayload(t.Context(), observation.records[target].id)
	sourceStatus, _ := s.moveFileObservationPayload(t.Context(), observation.records[source].id)
	require.Equal(t, "partial_or_failed", targetStatus["move_write_verdict"])
	require.Equal(t, "partial_or_failed", sourceStatus["move_write_verdict"])
	require.Equal(t, mutationDiskCommitted, targetStatus["file_write_status"])
	require.Equal(t, mutationDiskFailed, sourceStatus["file_write_status"])
	timing := targetStatus["write_timing"].(map[string]any)
	require.Equal(t, "measured", timing["state"])
	require.Equal(t, timing["window_ns"], timing["returned_ns"].(int64)-timing["started_ns"].(int64))
	require.Positive(t, timing["window_ns"])
	require.Nil(t, observation.observer(ctx, target, s.multiIndexer.GetIndexer("fixture")), "partial move must not gain successful source verdict")
	require.NotContains(t, targetStatus, "direct_reconcile")
	before := len(s.mutationCommits.ordered)
	require.Nil(t, s.beginMoveObservation(t.Context(), "old", "new", source, target, "source.go", "target.go", true))
	require.Equal(t, before, len(s.mutationCommits.ordered), "unobserved path allocated timing records")
	for _, ref := range observation.references() {
		_, physical := s.mutationCommits.byReceipt(ref.Receipt)
		require.False(t, physical, "move timing acquired physical receipt authority")
	}
}
