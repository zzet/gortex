package store_sqlite

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestBoundAnalysisWriterCancelsEveryGateAdmissionWithoutMutation(t *testing.T) {
	s, err := Open(filepathForAnalysisTest(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	id := buildMinimalAnalysisGeneration(t, s, "building", 0, false)
	revision := s.AnalysisMutationRevision()
	operations := map[string]func(graph.AnalysisGenerationStore) error{
		"begin": func(w graph.AnalysisGenerationStore) error {
			_, _, e := w.BeginAnalysisGeneration(revision, graph.AnalysisGenerationHeader{FormatVersion: 77})
			return e
		},
		"community": func(w graph.AnalysisGenerationStore) error {
			_, e := w.AppendAnalysisCommunities(revision, id, []graph.AnalysisCommunitySummary{{ID: "late", Size: 1}})
			return e
		},
		"node": func(w graph.AnalysisGenerationStore) error {
			_, e := w.AppendAnalysisNodes(revision, id, []graph.AnalysisNodeMetric{{NodeID: "late"}})
			return e
		},
		"process": func(w graph.AnalysisGenerationStore) error {
			_, e := w.AppendAnalysisProcesses(revision, id, nil, nil)
			return e
		},
		"concept": func(w graph.AnalysisGenerationStore) error {
			_, e := w.AppendAnalysisConcepts(revision, id, []graph.AnalysisConcept{{Token: "late"}}, nil)
			return e
		},
		"blob": func(w graph.AnalysisGenerationStore) error {
			_, e := w.PutAnalysisBlob(revision, id, graph.AnalysisBlob{Component: graph.AnalysisBlobAdjacency, Payload: []byte("late")})
			return e
		},
		"seal": func(w graph.AnalysisGenerationStore) error {
			_, e := w.SealAnalysisComponent(revision, id, graph.AnalysisComponentNodes, 1)
			return e
		},
		"activate": func(w graph.AnalysisGenerationStore) error {
			_, e := w.ActivateAnalysisGeneration(revision, id)
			return e
		},
		"abort": func(w graph.AnalysisGenerationStore) error { return w.AbortAnalysisGeneration(id) },
	}
	s.writeMu.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(s.writeMu.Unlock) }
	t.Cleanup(release)
	for name, run := range operations {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bound := s.BindAnalysisWriteContext(ctx)
			done := make(chan error, 1)
			go func() { done <- run(bound) }()
			require.Eventually(t, func() bool { return s.writeMu.waiting() > 0 }, 3*time.Second, time.Millisecond)
			cancel()
			select {
			case e := <-done:
				require.ErrorIs(t, e, context.Canceled)
			case <-time.After(3 * time.Second):
				t.Fatal("bound writer did not cancel its held-gate admission")
			}
			require.False(t, s.writeMu.TryLock(), "canceled waiter released another owner's gate")
		})
	}
	t.Run("snapshot", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var installed atomic.Bool
		done := make(chan bool, 1)
		go func() {
			done <- s.BindAnalysisWriteContext(ctx).CommitAnalysisSnapshot(revision, func() { installed.Store(true) })
		}()
		require.Eventually(t, func() bool { return s.writeMu.waiting() > 0 }, 3*time.Second, time.Millisecond)
		cancel()
		select {
		case accepted := <-done:
			require.False(t, accepted)
		case <-time.After(3 * time.Second):
			t.Fatal("snapshot gate did not cancel")
		}
		require.False(t, installed.Load())
	})
	release()
	var generations int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM analysis_generations`).Scan(&generations))
	require.Equal(t, 1, generations)
	_, found, err := s.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	require.False(t, found)
}

func TestBoundAnalysisWriterCancelsPoolWaitAndLeavesOriginalHandleUsable(t *testing.T) {
	s, err := Open(filepathForAnalysisTest(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	held, err := s.writerDB.Conn(context.Background())
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(func() { _ = held.Close() }) }
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitsBefore := s.writerDB.Stats().WaitCount
	go func() {
		_, _, e := s.BindAnalysisWriteContext(ctx).BeginAnalysisGeneration(s.AnalysisMutationRevision(), graph.AnalysisGenerationHeader{FormatVersion: 77})
		done <- e
	}()
	require.Eventually(t, func() bool { return s.writerDB.Stats().WaitCount > waitsBefore }, 3*time.Second, time.Millisecond)
	cancel()
	select {
	case err = <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("bound writer ignored connection-pool cancellation")
	}
	release()
	buildMinimalAnalysisGeneration(t, s, "after-pool-cancel", 0, true)
}

func TestBoundAnalysisWriterKeepsViewAndPriorActiveGeneration(t *testing.T) {
	s, err := Open(filepathForAnalysisTest(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	old := buildMinimalAnalysisGeneration(t, s, "old", 0, true)
	view := s.AtGeneration(37).ViewScopedAnalysis()
	viewID := buildMinimalAnalysisGeneration(t, view, "other-view", 0, true)
	next := buildMinimalAnalysisGeneration(t, s, "next", 0, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bound := s.BindAnalysisWriteContext(ctx).(*Store)
	require.Same(t, s.storeCore, bound.storeCore)
	require.Equal(t, s.ViewGeneration(), bound.ViewGeneration())
	accepted, err := bound.ActivateAnalysisGeneration(s.AnalysisMutationRevision(), next)
	require.False(t, accepted)
	require.ErrorIs(t, err, context.Canceled)
	header, found, err := s.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, old, header.GenerationID)
	header, found, err = view.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, viewID, header.GenerationID)
	// Binding is non-owning, and the original does not inherit its cancellation.
	require.NoError(t, bound.Close())
	requireAnalysisAccepted(t)(s.ActivateAnalysisGeneration(s.AnalysisMutationRevision(), next))
}

func TestOwnedAnalysisTransactionJoinsPhysicalRollbackBeforeNextWriter(t *testing.T) {
	s, err := Open(filepathForAnalysisTest(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.writeMu.Lock()
	var gateOnce sync.Once
	unlock := func() { gateOnce.Do(s.writeMu.Unlock) }
	t.Cleanup(unlock)
	owner, err := s.beginOwnedAnalysisWrite(ctx)
	require.NoError(t, err)
	var primary error
	var finishOnce sync.Once
	finishOwner := func() { finishOnce.Do(func() { owner.finish(&primary); unlock() }) }
	t.Cleanup(finishOwner)
	_, err = owner.tx.ExecContext(ctx, `INSERT INTO nodes(id,view_gen,kind,name,file_path) VALUES('canceled-owned-analysis',0,'function','Canceled','cancel.go')`)
	require.NoError(t, err)
	rows, err := owner.tx.QueryContext(context.WithoutCancel(ctx), `SELECT 1`)
	require.NoError(t, err)
	require.True(t, rows.Next())
	scanner := foldBlockedScanner{entered: make(chan struct{}), release: make(chan struct{})}
	var scanOnce sync.Once
	releaseScan := func() { scanOnce.Do(func() { close(scanner.release) }) }
	t.Cleanup(releaseScan)
	type scanReceipt struct{ scan, close error }
	scanned := make(chan scanReceipt, 1)
	go func() {
		scanErr := rows.Scan(scanner)
		closeErr := rows.Close()
		scanned <- scanReceipt{scan: scanErr, close: closeErr}
	}()
	waitCheckpointSignal(t, scanner.entered)
	cancel()
	primary = context.Canceled
	finished := make(chan struct{})
	go func() { finishOwner(); close(finished) }()
	next := make(chan error, 1)
	go func() {
		next <- s.AddBatchChecked([]*graph.Node{{ID: "after-owned-rollback", Kind: graph.KindFunction, Name: "After"}}, nil)
	}()
	require.Eventually(t, func() bool { return s.writeMu.waiting() > 0 }, 3*time.Second, time.Millisecond)
	select {
	case <-finished:
		t.Fatal("Tx-done state replaced physical rollback completion")
	default:
	}
	releaseScan()
	scan := <-scanned
	require.NoError(t, scan.scan)
	require.NoError(t, scan.close)
	iterationErr := rows.Err()
	// database/sql.Tx.rollback cancels its own context before joining rows;
	// Rows.awaitDone may retain that exact cancellation. Do not hide another
	// iterator error (including an error joined with context.Canceled).
	require.True(t, iterationErr == nil || iterationErr == context.Canceled, "unexpected iterator completion error: %v", iterationErr)
	waitCheckpointSignal(t, finished)
	require.ErrorIs(t, primary, context.Canceled)
	require.NoError(t, <-next)
	require.Nil(t, s.GetNode("canceled-owned-analysis"))
	require.NotNil(t, s.GetNode("after-owned-rollback"))
}

func TestBoundAnalysisOwnedBeginRetainsNativeBusyResidual(t *testing.T) {
	s, handle, id := reparseWindowFixture(t)
	// Only this test store's busy policy is shortened. This is a residual-control
	// test, not evidence that production's5000ms immediate BEGIN is a2s phase.
	setWriterBusyTimeout(t, s, 250)
	opened, err := handle.BeginManagedDedicatedReparseBulkLoad(t.Context(), id)
	require.NoError(t, err)
	require.True(t, opened)
	t.Cleanup(func() { require.NoError(t, handle.EndGenerationBulkLoadFor(id)) })
	blocker, held := holdExternalWriter(t, s.dbPath)
	t.Cleanup(func() { _ = held.Rollback(); _ = blocker.Close() })
	entered := observeSQLiteBegin(t, s.bulkConn)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, e := s.BindAnalysisWriteContext(ctx).BeginAnalysisGeneration(s.AnalysisMutationRevision(), graph.AnalysisGenerationHeader{FormatVersion: 77})
		done <- e
	}()
	waitCheckpointSignal(t, entered) // Actual SQLite BEGIN authorizer, not a timer.
	started := time.Now()
	cancel()
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("owned BEGIN did not actually complete within fixture busy policy")
	}
	require.ErrorIs(t, err, context.Canceled)
	t.Logf("detached analysis BEGIN fixture busy_timeout=250ms cancellation-to-actual-return=%s", time.Since(started))
	require.NoError(t, held.Rollback())
	require.NoError(t, blocker.Close())
	_, accepted, err := s.BeginAnalysisGeneration(s.AnalysisMutationRevision(), graph.AnalysisGenerationHeader{FormatVersion: 77})
	requireAnalysisAccepted(t)(accepted, err)
}
