package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/platform"
	"go.uber.org/zap"
)

// Fresh owned physical writes, one actual set admission, and the real parser /
// executor. No arbitrary publication record or fabricated timer stamp enters.
func sharedOwnerPhysicalSetFixture(t *testing.T) (*Server, []*mutationCommitRecord) {
	t.Helper()
	s, root := setupTestServer(t)
	s.editAckMode = config.EditAckLocal
	paths := []string{filepath.Join(root, "first.go"), filepath.Join(root, "second.go")}
	for _, path := range paths {
		require.NoError(t, os.WriteFile(path, []byte("package main\nfunc Before() {}\n"), 0600))
	}
	_, err := s.indexer.IncrementalReindexPaths(root, paths)
	require.NoError(t, err)
	watcher, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, watcher.Start([]string{t.TempDir()}))
	s.SetWatcher(watcher)
	t.Cleanup(func() { require.NoError(t, watcher.Stop()); s.DrainBackground(); s.indexer.Close() })
	ctx, ack, _ := s.withEditAcknowledgement(canonicalTimingContext())
	ctx, unlock, err := acquireMutationPaths(ctx, paths)
	require.NoError(t, err)
	defer unlock()
	records := make([]*mutationCommitRecord, len(paths))
	targets := make([]sourceMutationTarget, len(paths))
	for i, path := range paths {
		content := []byte("package main\nfunc AfterSharedOwner() {}\n")
		records[i], err = s.commitFileMutation(ctx, "write_file", "", "", filepath.Base(path), path, content, 0600)
		require.NoError(t, err)
		require.Positive(t, records[i].snapshot().CommitSeq)
		targets[i] = captureSourceMutationTarget(ctx, path)
	}
	admissions := s.admitSourceMutationFrontier(ctx, targets)
	unlock()
	outcomes := s.awaitSourceMutationFrontier(ctx, admissions)
	for i, record := range records {
		require.NotNil(t, admissions[i].receipt)
		require.NoError(t, outcomes[i].Err)
		record.recordGraph(outcomes[i])
	}
	ack.finishActual()
	ack.finishReply(nil, nil)
	for _, record := range records {
		require.Eventually(t, func() bool {
			status := s.mutationStatusPayload(record)
			window, ok := status["phase_timing"].(indexer.EditTimingWindow)
			return ok && status["graph_status"] == mutationGraphFresh && window.SharedSetExecution != nil
		}, 5*time.Second, time.Millisecond, "real shared executor must complete before checking new proof")
	}
	return s, records
}

func sharedOwnerStatusWire(t *testing.T, status map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(status)
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var wire map[string]any
	require.NoError(t, decoder.Decode(&wire))
	return wire
}

func TestSharedOwnerAccountingBindsBothActualPhysicalReceipts(t *testing.T) {
	s, records := sharedOwnerPhysicalSetFixture(t)
 t.Cleanup(func(){if t.Failed(){body,_:=json.Marshal(s.mutationStatusPayload(records[0]));t.Logf("shared owner status: %s",body)}})
	var definition map[string]any
	for _, record := range records {
		var proof map[string]any
		require.Eventually(t, func() bool {
			wire := sharedOwnerStatusWire(t, s.mutationStatusPayload(record))
			proof, _ = wire["shared_owner_accounting_v1"].(map[string]any)
			return proof != nil
		}, 5*time.Second, time.Millisecond, "current raw shared diagnostic is not a self-contained owner proof")
		reference, ok := proof["reference"].(map[string]any)
		require.True(t, ok)
		currentDefinition, ok := proof["definition"].(map[string]any)
		require.True(t, ok)
		membership, ok := proof["membership"].(map[string]any)
		require.True(t, ok)
		if definition == nil {
			definition = currentDefinition
		} else {
			require.Equal(t, definition, currentDefinition, "one actual executor has one immutable definition, not one pipeline per file")
		}
		require.Equal(t, "shared_mutation_set_executor", definition["scope"])
		require.Equal(t, definition["owner"], reference["owner"])
		require.Equal(t, definition["owner"], membership["owner"])
		require.Equal(t, reference["member"], membership["member"])
		member, ok := reference["member"].(map[string]any)
		require.True(t, ok)
		snap := record.snapshot()
		require.Equal(t, snap.Receipt, member["receipt"])
		require.Equal(t, json.Number(strconv.FormatUint(snap.CommitSeq, 10)), member["physical_commit_sequence"])
		require.Equal(t, snap.ContentSHA256, member["content_sha256"])
		require.Positive(t, snap.AppliedGeneration)
		require.Equal(t, json.Number(strconv.FormatUint(snap.AppliedGeneration, 10)), member["ticket"])
		require.Equal(t, member["ticket"], membership["applied_generation"])
		require.Equal(t, true, membership["same_status_projection"])
		require.Equal(t, true, membership["live_server_ledger_member"])
		require.Equal(t, true, membership["full_membership_retained"])
		require.Len(t, definition["members"], 2)
		owner, ok := definition["owner"].(map[string]any)
		require.True(t, ok)
		require.NotEmpty(t, owner["server_instance"])
		require.NotEmpty(t, owner["recorder_instance"])
		process, ok := owner["process"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, json.Number(strconv.Itoa(os.Getpid())), process["pid"])
		identity, err := platform.CaptureProcessIdentity(os.Getpid())
		require.NoError(t, err)
		birth, err := identity.Token()
		require.NoError(t, err)
		require.Equal(t, birth, process["birth_token"])
		execution, ok := definition["execution"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, true, execution["observed_partition_complete"])
		wire := sharedOwnerStatusWire(t, s.mutationStatusPayload(record))
		phase := wire["phase_timing"].(map[string]any)
		require.Nil(t, phase["patch_wall_ms"])
		require.Nil(t, phase["patch_accounting"])
		require.Nil(t, wire["requested_background"])
	}
	// An aliased ledger in another actual Server cannot borrow the proof.
	other := NewServer(s.engine, s.graph, s.indexer, nil, zap.NewNop(), nil)
	other.mutationCommits.put(records[0])
	require.Same(t, s, records[0].owner, "ledger alias must not change the actual original owner")
	require.Nil(t, s.mutationStatusPayload(records[0])["error"])
	require.Nil(t, other.mutationStatusPayload(records[0])["shared_owner_accounting_v1"])
	literal := newMutationServer()
	literal.mutationCommits.put(records[0])
	require.Nil(t, literal.mutationStatusPayload(records[0])["shared_owner_accounting_v1"], "literal/unnamed Server cannot gain proof from retained records")
}
