package store_sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	sqlite "modernc.org/sqlite"
)

func TestCheckedBatchContextCancelsGateAndPoolAdmission(t *testing.T) {
	for _, mode := range []string{"gate", "pool"} {
		t.Run(mode, func(t *testing.T) {
			s := openMutationReceiptStore(t)
			var release func()
			waits := s.writerDB.Stats().WaitCount
			if mode == "gate" {
				s.writeMu.Lock()
				release = s.writeMu.Unlock
			} else {
				conn, err := s.writerDB.Conn(t.Context())
				require.NoError(t, err)
				release = func() { require.NoError(t, conn.Close()) }
			}
			var once sync.Once
			unlock := func() { once.Do(release) }
			t.Cleanup(unlock)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				done <- s.AddBatchCheckedContext(ctx, []*graph.Node{{ID: "canceled", Kind: graph.KindFunction}}, nil)
			}()
			t.Cleanup(func() {
				cancel()
				unlock()
				select {
				case <-joined:
				case <-time.After(3 * time.Second):
					t.Error("batch admission fixture did not join")
				}
			})
			require.Eventually(t, func() bool {
				if mode == "gate" {
					return s.writeMu.waiting() > 0
				}
				return s.writerDB.Stats().WaitCount > waits
			}, 3*time.Second, time.Millisecond)
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(3 * time.Second):
				t.Fatal("checked batch did not cancel its actual admission")
			}
			if mode == "gate" {
				require.False(t, s.writeMu.TryLock(), "waiter released another owner's gate")
			}
			unlock()
			require.Nil(t, s.GetNode("canceled"))
			require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "after", Kind: graph.KindFunction}}, nil))
			require.NotNil(t, s.GetNode("after"))
		})
	}
}

// Encoding this real metadata value holds the second bounded chunk after the
// first SQL chunk has executed. There is no production callback or timer seam.
type heldBatchMetadata struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *heldBatchMetadata) MarshalJSON() ([]byte, error) {
	m.once.Do(func() { close(m.entered) })
	<-m.release
	return []byte(`"held"`), nil
}

func TestCheckedBatchContextRollsBackEarlierChunksAndBuiltinAdmission(t *testing.T) {
	for _, ingest := range []string{"0", "1"} {
		t.Run(ingest, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_JSONB_INGEST", ingest)
			s := openMutationReceiptStore(t)
			s.batchVariableLimit = sqliteBatchVariableHardCap
			chunkRows := jsonbIngestNodeRows
			if ingest == "0" {
				chunkRows = batchRowsForVariableLimit(s.batchVariableLimit, nodeInsertParams, nodeInsertMaxChunkSize)
			}
			nodes, _ := addBatchFixture(chunkRows+1, 0)
			marker := &heldBatchMetadata{entered: make(chan struct{}), release: make(chan struct{})}
			var once sync.Once
			release := func() { once.Do(func() { close(marker.release) }) }
			t.Cleanup(release)
			nodes[len(nodes)-1].Meta = map[string]any{"held": marker}
			edges := []*graph.Edge{{From: nodes[0].ID, To: "repo::builtin::ts::array::push", Kind: graph.EdgeCalls, FilePath: nodes[0].FilePath}}
			token := s.BeginMutationReceipt()
			before := nodeRowsWritten.Load()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			joined := make(chan struct{})
			go func() { defer close(joined); done <- s.AddBatchCheckedContext(ctx, nodes, edges) }()
			t.Cleanup(func() {
				cancel()
				release()
				select {
				case <-joined:
				case <-time.After(3 * time.Second):
					t.Error("batch chunk fixture did not join")
				}
			})
			waitCheckpointSignal(t, marker.entered)
			require.Greater(t, nodeRowsWritten.Load(), before, "the first SQL chunk must really execute before cancellation")
			cancel()
			next := make(chan error, 1)
			nextJoined := make(chan struct{})
			go func() {
				defer close(nextJoined)
				next <- s.AtGeneration(41).AddBatchChecked([]*graph.Node{{ID: "next", Kind: graph.KindFunction}}, nil)
			}()
			t.Cleanup(func() {
				cancel()
				release()
				select {
				case <-nextJoined:
				case <-time.After(3 * time.Second):
					t.Error("competing writer fixture did not join")
				}
			})
			require.Eventually(t, func() bool { return s.writeMu.waiting() > 0 }, 3*time.Second, time.Millisecond)
			select {
			case <-done:
				t.Fatal("batch returned while its owned chunk was still executing")
			default:
			}
			select {
			case <-next:
				t.Fatal("next writer escaped the owned batch gate")
			default:
			}
			release()
			require.ErrorIs(t, <-done, context.Canceled)
			require.NoError(t, <-next)
			require.Zero(t, s.NodeCount(), "earlier SQL chunks survived canceled rollback")
			require.Zero(t, s.EdgeCount())
			require.NotNil(t, s.AtGeneration(41).GetNode("next"))
			receipt := s.EndMutationReceipt(token)
			require.True(t, receipt.Complete)
			require.False(t, receipt.ResolutionRelevant, "rolled-back batch leaked a mutation receipt")
			require.Empty(t, receipt.ResolutionFiles())
			_, builtinFound := s.builtinSeen.Load(builtinSeenKey(0, edges[0].To))
			require.False(t, builtinFound)
			nodes[len(nodes)-1].Meta = nil
			require.NoError(t, s.AddBatchCheckedContext(t.Context(), nodes, edges))
			require.NotNil(t, s.GetNode(edges[0].To), "canceled seen-set admission suppressed the later builtin stub")
			require.Equal(t, len(nodes)+1, s.NodeCount())
			require.Equal(t, 1, s.EdgeCount())
		})
	}
}

func TestCheckedBatchContextPreservesReceiptsCountersAndIngestParity(t *testing.T) {
	for _, ingest := range []string{"0", "1"} {
		t.Run(ingest, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_JSONB_INGEST", ingest)
			legacy, live := openMutationReceiptStore(t), openMutationReceiptStore(t)
			nodes, edges := addBatchFixture(jsonbIngestNodeRows+1, edgeInsertMaxChunkSize+1)
			nodes[0].Meta = map[string]any{"clone_shingles": []uint64{1, 3, 5}, "clone_tokens": 9, "clone_sig": "sig"}
			edges = append(edges, edges[0], &graph.Edge{From: nodes[0].ID, To: "repo::builtin::ts::array::push", Kind: graph.EdgeCalls, Line: 12345})
			var receipts []graph.MutationReceipt
			for _, s := range []*Store{legacy, live} {
				require.NoError(t, s.EnsureRowCounters(t.Context()))
				token := s.BeginMutationReceipt()
				var err error
				if s == legacy {
					err = s.AddBatchChecked(nodes, edges)
				} else {
					err = s.AddBatchCheckedContext(t.Context(), nodes, edges)
				}
				require.NoError(t, err)
				receipts = append(receipts, s.EndMutationReceipt(token))
				requireCountersExact(t, s, 0)
			}
			require.Equal(t, receipts[0], receipts[1])
			require.ElementsMatch(t, legacy.AllNodes(), live.AllNodes())
			require.ElementsMatch(t, legacy.AllEdges(), live.AllEdges())
			for _, s := range []*Store{legacy, live} {
				var cloneRows int
				require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM clone_shingles WHERE node_id = ? AND signature = ? AND view_gen = 0`, nodes[0].ID, "sig").Scan(&cloneRows))
				require.Equal(t, 1, cloneRows)
			}
			buildMinimalAnalysisGeneration(t, live, "unchanged", 0, true)
			revision := live.AnalysisMutationRevision()
			require.NoError(t, live.AddBatchCheckedContext(t.Context(), nodes, edges))
			require.Equal(t, revision, live.AnalysisMutationRevision())
			_, found, err := live.LoadActiveAnalysisHeader(77)
			require.NoError(t, err)
			require.True(t, found, "idempotent checked batch invalidated analysis")
		})
	}
}

func TestCheckedBatchContextUsesManagedPinnedBulkAndOwnView(t *testing.T) {
	s, h, id := reparseWindowFixture(t)
	opened, err := h.BeginManagedDedicatedReparseBulkLoad(t.Context(), id)
	require.NoError(t, err)
	require.True(t, opened)
	var once sync.Once
	finish := func() { once.Do(func() { require.NoError(t, h.EndGenerationBulkLoadFor(id)) }) }
	t.Cleanup(finish)
	pinned := s.bulkConn
	token, baseToken := h.BeginMutationReceipt(), s.BeginMutationReceipt()
	nodes, edges := addBatchFixture(2, 1)
	require.NoError(t, h.AddBatchCheckedContext(t.Context(), nodes, edges))
	require.Same(t, pinned, s.bulkConn)
	require.NotNil(t, h.GetNode(nodes[0].ID))
	require.Nil(t, s.GetNode(nodes[0].ID), "selected payload escaped into base")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, h.AddBatchCheckedContext(ctx, []*graph.Node{{ID: "late", Kind: graph.KindFunction}}, nil), context.Canceled)
	require.Nil(t, h.GetNode("late"))
	require.False(t, s.EndMutationReceipt(baseToken).ResolutionRelevant)
	require.True(t, h.EndMutationReceipt(token).ResolutionRelevant)
	finish()
	counts, ok := s.TakeGenerationWriteCounters(id)
	require.True(t, ok)
	require.Equal(t, int64(1), counts.Batches)
	require.Equal(t, int64(2), counts.BulkNodeRows)
	require.Equal(t, int64(1), counts.BulkEdgeRows)
}

func TestCheckedBatchContextCancelsActualSQLiteStatementAndReleasesWriter(t *testing.T) {
	s := openMutationReceiptStore(t)
	conn, err := s.writerDB.Conn(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var inserts int
	require.NoError(t, conn.Raw(func(dc any) error {
		authorizer, ok := dc.(sqlite.AuthorizerRegisterer)
		if !ok {
			return errors.New("missing SQLite authorizer")
		}
		return authorizer.RegisterAuthorizer(func(action sqlite.AuthorizerActionCode, table, _, _, _ string) sqlite.AuthorizerReturnCode {
			if action == sqlite.AuthInsert && table == "nodes" {
				inserts++
				cancel()
			}
			return sqlite.AuthorizerOK
		})
	}))
	require.NoError(t, conn.Close())
	err = s.AddBatchCheckedContext(ctx, []*graph.Node{{ID: "statement-cancel", Kind: graph.KindFunction}}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Positive(t, inserts, "cancellation must originate inside real SQLite statement preparation")
	require.Nil(t, s.GetNode("statement-cancel"))
	// Clear only this owned connection's test authorizer before proving reuse.
	conn, err = s.writerDB.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(dc any) error { return dc.(sqlite.AuthorizerRegisterer).RegisterAuthorizer(nil) }))
	require.NoError(t, conn.Close())
	require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "after-statement", Kind: graph.KindFunction}}, nil))
	require.NotNil(t, s.GetNode("after-statement"))
	var unexpected int
	err = s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = 'statement-cancel'`).Scan(&unexpected)
	require.NoError(t, err)
	require.Zero(t, unexpected)
}
