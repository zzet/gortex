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

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

// Only admission is paused: the guarded rename, physical writes, set tickets,
// parser, executor and completion consumer are all the production paths.
type guardedRenameSetAdmission struct {
	ctx   context.Context
	paths []string
}

type guardedRenameSetWatcher struct {
	*indexer.Watcher
	entered  chan guardedRenameSetAdmission
	release  chan struct{}
	admitted chan map[string]*indexer.MutationTicket
	sets     atomic.Int64
	points   atomic.Int64
}

func (w *guardedRenameSetWatcher) EnqueueFileMutations(ctx context.Context, paths []string) (map[string]*indexer.MutationTicket, error) {
	w.sets.Add(1)
	w.entered <- guardedRenameSetAdmission{ctx, append([]string(nil), paths...)}
	<-w.release
	tickets, err := w.Watcher.EnqueueFileMutations(ctx, paths)
	w.admitted <- tickets
	return tickets, err
}

func (w *guardedRenameSetWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	w.points.Add(1)
	return w.Watcher.EnqueueToolMutation(ctx, path)
}

// Exercise the real guarded multi-file operation, not a manual commit/frontier
// composition or the separate point-publication MOVE control. Shared executor
// evidence stays nonadditive and never becomes an independent per-file P/B.
func TestGuardedRenameOriginalOwnersThroughRealSetPublication(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	extractor := &movePhysicalStatusExtractor{Extractor: languages.NewGoExtractor(), entered: make(chan struct{}, 16), release: make(chan struct{})}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	s, root := setupTestServerWithRegistry(t, registry)
	s.editAckMode = config.EditAckLocal
	paths := []string{filepath.Join(root, "caller.go"), filepath.Join(root, "target.go")}
	untouched := filepath.Join(root, "untouched.go")
	initial := map[string]string{paths[0]: renameCallerSrc, paths[1]: renameTargetSrc, untouched: "package main\nfunc Untouched() {}\n"}
	for path, raw := range initial {
		require.NoError(t, os.WriteFile(path, []byte(raw), 0600))
	}
	// These real earlier writes own records but no set ticket. Neither the
	// same-path predecessor nor the untouched file may inherit this rename.
	priorCtx := canonicalTimingContext()
	prior := commitTimingFixture(t, s, priorCtx, paths[0], initial[paths[0]])
	priorPublication := s.openCommittedPublicationRecord(priorCtx, prior)
	untouchedCtx := canonicalTimingContext()
	untouchedRecord := commitTimingFixture(t, s, untouchedCtx, untouched, initial[untouched])
	untouchedPublication := s.openCommittedPublicationRecord(untouchedCtx, untouchedRecord)
	require.NotNil(t, priorPublication)
	require.NotNil(t, untouchedPublication)
	priorBefore, untouchedBefore := priorPublication.Snapshot(), untouchedPublication.Snapshot()
	_, err := s.indexer.IncrementalReindexPaths(root, []string{paths[0], paths[1], untouched})
	require.NoError(t, err)
	s.indexer.ResolveAll()
	real, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	// Keep native events separate from the explicit one-set admission.
	require.NoError(t, real.Start([]string{t.TempDir()}))
	watcher := &guardedRenameSetWatcher{Watcher: real, entered: make(chan guardedRenameSetAdmission, 1), release: make(chan struct{}), admitted: make(chan map[string]*indexer.MutationTicket, 1)}
	s.SetWatcher(watcher)
	unblockAdmission := sync.OnceFunc(func() { close(watcher.release) })
	unblockParser := sync.OnceFunc(func() { close(extractor.release) })
	type reply struct {
		result *mcplib.CallToolResult
		err    error
	}
	finished := make(chan reply, 1)
	actualFinished := make(chan struct{})
	joined := false
	ctx, cancel := context.WithCancel(canonicalTimingContext())
	t.Cleanup(func() {
		unblockAdmission()
		unblockParser()
		if !joined {
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Error("owned guarded rename did not join")
			}
		}
		select {
		case <-actualFinished:
		case <-time.After(5 * time.Second):
			t.Error("actual guarded handler did not join")
		}
		cancel()
		stopErr := real.Stop()
		s.DrainBackground()
		s.indexer.Close()
		require.NoError(t, stopErr)
	})
	request := makeReq("rename_symbol", map[string]any{"id": "target.go::Target", "new_name": "Renamed", "format": "json"})
	// Borrow the same production projection owner inside the firewall, and
	// witness actual completion only after its finalizer has returned.
	projectedCtx, projected := s.withProjectionAcknowledgement(ctx, request, s.handleRenameSymbol)
	go func() {
		result, callErr := s.boundToolHandler(func(actual context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			defer close(actualFinished)
			return projected(actual, req)
		})(projectedCtx, request)
		finished <- reply{result, callErr}
	}()
	var admission guardedRenameSetAdmission
	select {
	case admission = <-watcher.entered:
		require.ElementsMatch(t, paths, admission.paths)
	case <-time.After(5 * time.Second):
		t.Fatal("guarded rename did not reach its complete set admission")
	}
	require.EqualValues(t, 1, watcher.sets.Load())
	require.Zero(t, watcher.points.Load())
	records := make([]*mutationCommitRecord, len(paths))
	publications := make([]*indexer.PublicationPhaseRecord, len(paths))
	written := make([][]byte, len(paths))
	// Rename has the real source/projection lifetime, but intentionally is
	// outside the five measured ACK surfaces. Do not manufacture an ACK.
	cause := projectionAcknowledgementFrom(admission.ctx, s)
	require.NotNil(t, cause)
	note := mutationCommitNoteFrom(admission.ctx)
	require.NotNil(t, note)
	note.mu.Lock()
	causedRecords := append([]*mutationCommitRecord(nil), note.records...)
	note.mu.Unlock()
	require.Len(t, causedRecords, 2)
	for i, path := range paths {
		record, found := s.mutationCommits.recentForPath(path)
		require.True(t, found)
		records[i] = record
		record.mu.RLock()
		publication, acknowledgement := record.publication, record.acknowledgement
		tool, sequence, owner, abs, hash := record.tool, record.commitSeq, record.owner, record.absPath, record.contentSHA256
		record.mu.RUnlock()
		require.Equal(t, "rename_symbol", tool)
		require.Same(t, s, owner)
		require.Equal(t, path, abs)
		require.Positive(t, sequence)
		require.NotSame(t, prior, record)
		require.NotNil(t, publication, "original record must exist before the set can wake")
		require.Nil(t, acknowledgement, "rename does not create a measured five-surface ACK")
		require.Contains(t, causedRecords, record, "the original request note must own each physical write")
		publications[i] = publication
		before := publication.Snapshot()
		require.Zero(t, before.Ticket)
		require.False(t, before.Terminal)
		require.Equal(t, indexer.PublicationSourceMCPEdit, before.Source)
		require.Equal(t, toolReceivedAt(ctx).UTC().Format(time.RFC3339Nano), before.OriginWall)
		require.Contains(t, publicationOffsets(before), indexer.PublicationReceiptCommitted)
		written[i], err = os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(written[i]), "Renamed")
		sum := sha256.Sum256(written[i])
		require.Equal(t, hex.EncodeToString(sum[:]), hash)
	}
	require.NotSame(t, publications[0], publications[1])
	// Foreground candidate preparation is finished. Only the actual queued
	// parser is held from this point, leaving a deterministic pending view.
	extractor.active.Store(true)
	unblockAdmission()
	var tickets map[string]*indexer.MutationTicket
	select {
	case tickets = <-watcher.admitted:
	case <-time.After(5 * time.Second):
		t.Fatal("real set admission did not return")
	}
	require.Len(t, tickets, 2)
	select {
	case <-extractor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("real set executor did not enter its parser")
	}
	select {
	case answer := <-finished:
		joined = true
		require.NoError(t, answer.err)
		require.Equal(t, "applied", decodeRenameResp(t, answer.result)["status"])
	case <-time.After(5 * time.Second):
		t.Fatal("local guarded ACK waited for the held executor")
	}
	pending := make([]indexer.PublicationPhaseSnapshot, len(paths))
	for i, path := range paths {
		require.Same(t, publications[i], tickets[path].PublicationRecord)
		status := s.mutationStatusPayload(records[i])
		require.Equal(t, mutationGraphPending, status["graph_status"])
		require.Equal(t, false, status["graph_status_terminal"])
		pending[i] = status["publication_phases"].(indexer.PublicationPhaseSnapshot)
		require.Equal(t, tickets[path].Generation, pending[i].Ticket)
		require.False(t, pending[i].Terminal)
		require.Nil(t, status["phase_timing"].(indexer.EditTimingWindow).SharedSetExecution)
	}
	unblockParser()
	completed := make([]indexer.PublicationPhaseSnapshot, len(paths))
	for i, record := range records {
		require.Eventually(t, func() bool {
			status := s.mutationStatusPayload(record)
			// The timer publishes executor completion before its owner-proof
			// finalizer returns. Capture the purity baseline after both finish;
			// status reads must not change either completed observation.
			proofState, _ := publications[i].SharedOwnerProofAvailability()
			proofFinished := proofState == "frozen" || proofState == "unavailable"
			return status["graph_status"] == mutationGraphFresh && status["phase_timing"].(indexer.EditTimingWindow).SharedSetExecution != nil && proofFinished
		}, 5*time.Second, time.Millisecond)
		status := s.mutationStatusPayload(record)
		completed[i] = status["publication_phases"].(indexer.PublicationPhaseSnapshot)
		require.True(t, completed[i].Terminal)
		require.Equal(t, pending[i].Key, completed[i].Key)
		require.Equal(t, pending[i].OriginWall, completed[i].OriginWall)
		require.Equal(t, tickets[paths[i]].Generation, completed[i].Ticket)
		window := status["phase_timing"].(indexer.EditTimingWindow)
		shared := window.SharedSetExecution
		require.Equal(t, "shared_mutation_set_executor", shared.Scope)
		require.True(t, shared.NonadditiveAcrossMembers)
		require.Equal(t, 2, shared.ObservedMembers)
		require.Equal(t, 2, shared.ExecutorFiles)
		require.Equal(t, completed[i].Ticket, shared.MemberGeneration)
		require.NotNil(t, shared.Accounting.PhasesMS["patch_parse"])
		require.Nil(t, window.PatchWallMS)
		require.Nil(t, window.PatchAccounting)
		require.NotContains(t, status, "requested_background")
		require.False(t, pending[i].Terminal, "saved pending snapshot must remain historical")
	}
	require.Equal(t, priorBefore, priorPublication.Snapshot())
	require.Equal(t, untouchedBefore, untouchedPublication.Snapshot())
	for _, control := range []*mutationCommitRecord{prior, untouchedRecord} {
		require.Nil(t, s.mutationStatusPayload(control)["phase_timing"].(indexer.EditTimingWindow).SharedSetExecution)
	}
	// A real later same-byte commit has another physical sequence and request
	// origin. It cannot acquire the completed predecessor's ticket/accounting.
	laterCtx := canonicalTimingContext()
	later := commitTimingFixture(t, s, laterCtx, paths[0], string(written[0]))
	require.Greater(t, later.snapshot().CommitSeq, records[0].snapshot().CommitSeq)
	laterPublication := s.openCommittedPublicationRecord(laterCtx, later)
	require.NotSame(t, publications[0], laterPublication)
	require.Zero(t, laterPublication.Snapshot().Ticket)
	require.Nil(t, s.mutationStatusPayload(later)["phase_timing"].(indexer.EditTimingWindow).SharedSetExecution)
	parses := parser.ParseCount()
	for range 3 {
		for i, record := range records {
			status := s.mutationStatusPayload(record)
			require.Equal(t, completed[i], status["publication_phases"])
			raw, readErr := os.ReadFile(paths[i])
			require.NoError(t, readErr)
			require.Equal(t, written[i], raw)
		}
	}
	require.Equal(t, parses, parser.ParseCount(), "status cannot parse/reconcile")
	require.EqualValues(t, 1, watcher.sets.Load(), "status cannot readmit the set")
	require.Zero(t, watcher.points.Load())
	raw, err := os.ReadFile(untouched)
	require.NoError(t, err)
	require.Equal(t, initial[untouched], string(raw))
}
