package store_sqlite

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modernsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/zzet/gortex/internal/graph"
)

// Keep the legacy conservative statement boundary in collision/order tests;
// production reindexing now derives statement sizes from SQLite's live limit.
const reindexCompatibilityChunkSize = 70

func TestReindexEdgesUsesBoundedSetStatementsAndFirstDuplicateWins(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const (
		fromID = "repo/caller.go::Caller"
		newTo  = "repo/target.go::Target"
	)
	oldEdges := make([]*graph.Edge, 0, reindexCompatibilityChunkSize+1)
	batch := make([]graph.EdgeReindex, 0, reindexCompatibilityChunkSize+1)
	for i := 0; i < reindexCompatibilityChunkSize+1; i++ {
		oldTo := fmt.Sprintf("repo/old-%03d.go::Target", i)
		oldEdges = append(oldEdges, &graph.Edge{
			From: fromID, To: oldTo, Kind: graph.EdgeCalls,
			FilePath: "repo/caller.go", Line: 7, Origin: "old",
		})
		batch = append(batch, graph.EdgeReindex{
			OldTo: oldTo,
			Edge: &graph.Edge{
				From: fromID, To: newTo, Kind: graph.EdgeCalls,
				FilePath: "repo/caller.go", Line: 7,
				Confidence: 0.8, ConfidenceLabel: "confirmed",
				Origin: fmt.Sprintf("candidate-%03d", i), Tier: "semantic",
			},
		})
	}
	store.AddBatch([]*graph.Node{{
		ID: fromID, Kind: graph.KindFunction, Name: "Caller",
		FilePath: "repo/caller.go", RepoPrefix: "repo",
	}}, oldEdges)

	beforeRevision := store.AnalysisMutationRevision()
	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.selectStatements, "all affected identities fit one bounded prefetch")
	assert.Equal(t, 1, stats.deleteStatements)
	assert.Equal(t, 1, stats.insertStatements)
	assert.Equal(t, 2, stats.writeStatements(), "set-oriented writes replace 142 per-edge DELETE/INSERT statements")
	assert.Equal(t, len(batch), stats.deletedRows)
	assert.Equal(t, 1, stats.insertedRows)
	assert.Equal(t, beforeRevision+1, store.AnalysisMutationRevision())

	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	assert.Equal(t, newTo, persisted[0].To)
	assert.Equal(t, "candidate-000", persisted[0].Origin, "INSERT OR IGNORE ordering keeps the first converging payload")
	assert.InDelta(t, 0.8, persisted[0].Confidence, 0.0001)
	assert.Equal(t, "confirmed", persisted[0].ConfidenceLabel)
	assert.Equal(t, "semantic", persisted[0].Tier)

	buildMinimalAnalysisGeneration(t, store, "set-reindex-noop", 0, true)
	beforeRevision = store.AnalysisMutationRevision()
	stats, err = store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.selectStatements)
	assert.Equal(t, 0, stats.writeStatements(), "an idempotent replay must not rewrite rows")
	assert.Equal(t, beforeRevision, store.AnalysisMutationRevision())
	_, found, err := store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	assert.True(t, found, "an idempotent replay must preserve active warm analysis")
}

func TestReindexEdgesRefreshDuplicateUsesLastWriteAndPreservesQuality(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const (
		fromID   = "repo/caller.go::Caller"
		targetID = "repo/target.go::Target"
		filePath = "repo/caller.go"
		line     = 11
	)
	store.AddBatch(nil, []*graph.Edge{{
		From: fromID, To: targetID, Kind: graph.EdgeCalls,
		FilePath: filePath, Line: line,
		Confidence: 0.2, ConfidenceLabel: "heuristic", Origin: "old", Tier: "syntax",
		Meta: map[string]any{"opaque": "old"},
	}})

	first := &graph.Edge{
		From: fromID, To: targetID, Kind: graph.EdgeCalls,
		FilePath: filePath, Line: line,
		Confidence: 0.6, ConfidenceLabel: "candidate", Origin: "first", Tier: "semantic",
		Meta: map[string]any{"opaque": "first"},
	}
	last := &graph.Edge{
		From: fromID, To: targetID, Kind: graph.EdgeCalls,
		FilePath: filePath, Line: line,
		Confidence: 0.91, ConfidenceLabel: "confirmed", Origin: "last", Tier: "compiler",
		CrossRepo: true,
		Meta: map[string]any{
			"opaque":                  "keep",
			"resolve_terminal":        true,
			"resolve_terminal_reason": "resolved",
		},
	}
	batch := []graph.EdgeReindex{
		{Edge: first, OldTo: targetID, RefreshIdentity: true, OldFilePath: filePath, OldLine: line},
		{Edge: last, OldTo: targetID, RefreshIdentity: true, OldFilePath: filePath, OldLine: line},
	}

	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.selectStatements)
	assert.Equal(t, 1, stats.deleteStatements)
	assert.Equal(t, 1, stats.insertStatements)

	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	edge := persisted[0]
	assert.Equal(t, "last", edge.Origin)
	assert.Equal(t, "compiler", edge.Tier)
	assert.InDelta(t, 0.91, edge.Confidence, 0.0001)
	assert.Equal(t, "confirmed", edge.ConfidenceLabel)
	assert.True(t, edge.CrossRepo)
	assert.Equal(t, "keep", edge.Meta["opaque"])
	assert.Equal(t, true, edge.Meta["resolve_terminal"])
	assert.Equal(t, "resolved", edge.Meta["resolve_terminal_reason"])

	beforeRevision := store.AnalysisMutationRevision()
	stats, err = store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.writeStatements(), "transient first-write followed by identical last-write has no final state change")
	assert.Equal(t, beforeRevision, store.AnalysisMutationRevision())
}

func TestReindexEdgesNetCancellationAcrossSQLChunksIsNoop(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const (
		fromID       = "repo/caller.go::Caller"
		originalTo   = "unresolved::Original"
		intermediate = "unresolved::Intermediate"
		fillerFrom   = "repo/filler.go::Caller"
		fillerTo     = "repo/filler-target.go::Target"
	)
	original := &graph.Edge{
		From: fromID, To: originalTo, Kind: graph.EdgeCalls,
		FilePath: "repo/caller.go", Line: 17,
		Confidence: 0.85, ConfidenceLabel: "confirmed", Origin: "compiler", Tier: "semantic",
		Meta: map[string]any{"opaque": "preserve"},
	}
	filler := &graph.Edge{
		From: fillerFrom, To: fillerTo, Kind: graph.EdgeCalls,
		FilePath: "repo/filler.go", Line: 3, Origin: "filler",
	}
	store.AddBatch(nil, []*graph.Edge{original, filler})
	buildMinimalAnalysisGeneration(t, store, "set-reindex-net-noop", 0, true)
	beforeRevision := store.AnalysisMutationRevision()

	batch := make([]graph.EdgeReindex, 0, reindexCompatibilityChunkSize+1)
	intermediateEdge := *original
	intermediateEdge.To = intermediate
	batch = append(batch, graph.EdgeReindex{Edge: &intermediateEdge, OldTo: originalTo})
	for i := 0; i < reindexCompatibilityChunkSize-1; i++ {
		fillerCandidate := *filler
		batch = append(batch, graph.EdgeReindex{
			Edge:  &fillerCandidate,
			OldTo: fmt.Sprintf("repo/missing-%03d.go::Target", i),
		})
	}
	restored := *original
	batch = append(batch, graph.EdgeReindex{Edge: &restored, OldTo: intermediate})

	token := store.BeginMutationReceipt()
	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	receipt := store.EndMutationReceipt(token)

	assert.Equal(t, 1, stats.selectStatements)
	assert.Equal(t, 0, stats.writeStatements(), "net cancellation must not issue transient writes")
	assert.Equal(t, 0, stats.deletedRows)
	assert.Equal(t, 0, stats.insertedRows)
	assert.Equal(t, beforeRevision, store.AnalysisMutationRevision())
	assert.True(t, receipt.Complete)
	assert.False(t, receipt.ResolutionRelevant, "a net-noop must not create resolver catch-up work")

	_, found, err := store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	assert.True(t, found, "a net-noop must preserve active warm analysis")
	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	assert.Equal(t, originalTo, persisted[0].To)
	assert.Equal(t, "preserve", persisted[0].Meta["opaque"])
}

func TestReindexEdgesResolvedConversionChunksByRows(t *testing.T) {
	const edgeCount = 2000
	// The resolved-conversion arm rides one json_each variable per statement,
	// so the connection variable limit no longer shapes it: both budget
	// extremes must produce the identical row-bounded statement count.
	for _, variableLimit := range []int{sqliteFallbackVariableLimit, sqliteBatchVariableHardCap} {
		t.Run(fmt.Sprintf("limit_%d", variableLimit), func(t *testing.T) {
			store := openReindexReceiptTestStore(t)
			oldEdges := make([]*graph.Edge, 0, edgeCount)
			batch := make([]graph.EdgeReindex, 0, edgeCount)
			for i := 0; i < edgeCount; i++ {
				from := fmt.Sprintf("repo/caller-%04d.go::Caller", i)
				oldTo := fmt.Sprintf("unresolved::Old%04d", i)
				oldEdges = append(oldEdges, &graph.Edge{
					From: from, To: oldTo, Kind: graph.EdgeCalls,
					FilePath: "repo/callers.go", Line: i + 1,
				})
				batch = append(batch, graph.EdgeReindex{
					OldTo: oldTo,
					Edge: &graph.Edge{
						From: from, To: fmt.Sprintf("repo/target-%04d.go::Target", i),
						Kind: graph.EdgeCalls, FilePath: "repo/callers.go", Line: i + 1,
					},
				})
			}
			store.AddBatch(nil, oldEdges)
			store.batchVariableLimit = variableLimit

			stats, err := store.reindexEdgesSetOriented(batch)
			require.NoError(t, err)
			assert.Zero(t, stats.selectStatements, "resolved conversions must not prefetch full edge rows")
			wantStatements := (edgeCount + reindexRowMaxChunkSize - 1) / reindexRowMaxChunkSize
			assert.Equal(t, wantStatements, stats.updateStatements,
				"json_each chunks by rows, not by the connection variable limit")
			assert.Equal(t, edgeCount, stats.updatedRows)
			assert.Zero(t, stats.deleteStatements)
			assert.Zero(t, stats.insertStatements)
			assert.Zero(t, stats.deletedRows)
			assert.Zero(t, stats.insertedRows)
			assert.Equal(t, edgeCount, store.EdgeCount())
		})
	}
}

func TestReindexInsertChunksRespectBoundArgumentBytes(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	tx, err := store.beginWrite()
	require.NoError(t, err)
	largeMeta := bytes.Repeat([]byte("x"), 3<<20)
	rows := make([]sqliteReindexRow, 3)
	for i := range rows {
		rows[i] = sqliteReindexRow{
			key: sqliteReindexKey{
				fromID: fmt.Sprintf("repo/caller-%d.go::Caller", i),
				toID:   fmt.Sprintf("repo/target-%d.go::Target", i),
				kind:   string(graph.EdgeCalls), filePath: "repo/callers.go", line: i + 1,
			},
			meta: largeMeta,
		}
	}
	variableLimit := sqliteBatchVariableHardCap
	inserted, statements, err := insertSQLiteReindexRowsTxLimited(tx, baseViewGeneration, rows, &variableLimit)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	assert.Equal(t, len(rows), inserted)
	assert.Equal(t, len(rows), statements, "each 3 MiB row must stay below the 4 MiB statement budget")
}

func TestReindexEdgesDownshiftsConnectionVariableLimit(t *testing.T) {
	// The resolved-conversion arm no longer binds per-row variables, so the
	// downshift is exercised through the generic simulator path: mixing
	// conversion directions disqualifies the fast path, and the prefetch /
	// delete / insert arms still bind bounded per-row VALUES relations that
	// must discover and persist a lowered connection limit.
	store := openReindexReceiptTestStore(t)
	oldEdges := make([]*graph.Edge, 0, 120)
	batch := make([]graph.EdgeReindex, 0, 120)
	for i := 0; i < 120; i++ {
		from := fmt.Sprintf("repo/caller-%03d.go::Caller", i)
		oldTo := fmt.Sprintf("unresolved::Old%03d", i)
		newTo := fmt.Sprintf("repo/target-%03d.go::Target", i)
		if i%2 == 1 {
			// Reverse conversion (guard-revert shape) breaks direction
			// uniformity for the whole batch.
			oldTo, newTo = newTo, oldTo
		}
		oldEdges = append(oldEdges, &graph.Edge{
			From: from, To: oldTo, Kind: graph.EdgeCalls,
			FilePath: "repo/callers.go", Line: i + 1,
		})
		batch = append(batch, graph.EdgeReindex{
			OldTo: oldTo,
			Edge: &graph.Edge{
				From: from, To: newTo,
				Kind: graph.EdgeCalls, FilePath: "repo/callers.go", Line: i + 1,
			},
		})
	}
	store.AddBatch(nil, oldEdges)

	conn, err := store.writerDB.Conn(context.Background())
	require.NoError(t, err)
	const connectionLimit = 220
	_, err = modernsqlite.Limit(conn, int(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER), connectionLimit)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store.batchVariableLimit = sqliteBatchVariableHardCap

	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Equal(t, len(batch), stats.deletedRows)
	assert.Equal(t, len(batch), stats.insertedRows)
	assert.Zero(t, stats.updatedRows)
	assert.LessOrEqual(t, store.batchVariableLimit, connectionLimit)
	assert.Greater(t, stats.selectStatements, 1, "the first oversized prefetch must downshift and retry")
	assert.Equal(t, len(batch), store.EdgeCount())
}

func TestReindexEdgesProbesVariableLimitBeforeWriterCheckout(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	store.writerDB.SetMaxOpenConns(1)
	old := &graph.Edge{
		From: "repo/caller.go::Caller", To: "unresolved::Target", Kind: graph.EdgeCalls,
		FilePath: "repo/caller.go", Line: 7,
	}
	store.AddBatch(nil, []*graph.Edge{old})
	store.batchVariableLimit = 0 // exercise the fresh-store probe path

	done := make(chan error, 1)
	go func() {
		_, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{{
			OldTo: old.To,
			Edge: &graph.Edge{
				From: old.From, To: "repo/target.go::Target", Kind: old.Kind,
				FilePath: old.FilePath, Line: old.Line,
			},
		}})
		done <- err
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		// Let an implementation that checked out the sole connection before
		// probing its limit unwind, so test cleanup cannot remain wedged.
		store.writerDB.SetMaxOpenConns(2)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("ReindexEdges waited for its own writer connection while probing SQLite limits")
	}
	assert.Positive(t, store.batchVariableLimit)
}

func TestReindexEdgesResolvedConversionUpdatesInPlace(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const (
		fromID    = "repo/caller.go::Caller"
		oldTarget = "unresolved::Target"
		newTarget = "repo/target.go::Target"
		filePath  = "repo/caller.go"
	)
	store.AddBatch(nil, []*graph.Edge{{
		From: fromID, To: oldTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 17,
		Confidence: 0.2, ConfidenceLabel: "heuristic", Origin: "syntax", Tier: "syntax",
		Meta: map[string]any{"payload": "old"},
	}})
	oldKey := sqliteReindexKey{
		fromID: fromID, toID: oldTarget, kind: string(graph.EdgeCalls), filePath: filePath, line: 17,
	}
	beforeID := reindexStoredEdgeID(t, store, oldKey)
	beforeAnalysisRevision := store.AnalysisMutationRevision()
	beforeMutationRevision := store.MutationRevision()

	batch := []graph.EdgeReindex{{
		OldTo: oldTarget,
		Edge: &graph.Edge{
			From: fromID, To: newTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 17,
			Confidence: 0.95, ConfidenceLabel: "confirmed", Origin: "compiler", Tier: "semantic",
			CrossRepo: true,
			Meta: map[string]any{
				"payload":                 "new",
				"resolve_terminal":        true,
				"resolve_terminal_reason": "bound",
				"semantic_source":         "lsp",
			},
		},
	}}

	token := store.BeginMutationReceipt()
	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	receipt := store.EndMutationReceipt(token)
	assert.Zero(t, stats.selectStatements)
	assert.Equal(t, 1, stats.updateStatements)
	assert.Equal(t, 1, stats.updatedRows)
	assert.Zero(t, stats.deleteStatements)
	assert.Zero(t, stats.insertStatements)
	assert.Zero(t, stats.deletedRows)
	assert.Zero(t, stats.insertedRows)
	assert.Equal(t, 1, stats.writeStatements())
	assert.Equal(t, beforeAnalysisRevision+1, store.AnalysisMutationRevision())
	assert.Equal(t, beforeMutationRevision+1, store.MutationRevision())
	assert.True(t, receipt.Complete)
	assert.False(t, receipt.ResolutionRelevant)

	newKey := sqliteReindexKey{
		fromID: fromID, toID: newTarget, kind: string(graph.EdgeCalls), filePath: filePath, line: 17,
	}
	assert.Equal(t, beforeID, reindexStoredEdgeID(t, store, newKey), "in-place conversion must preserve the SQLite row id")
	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	edge := persisted[0]
	assert.Equal(t, newTarget, edge.To)
	assert.Equal(t, 0.95, edge.Confidence)
	assert.Equal(t, "confirmed", edge.ConfidenceLabel)
	assert.Equal(t, "compiler", edge.Origin)
	assert.Equal(t, "semantic", edge.Tier)
	assert.True(t, edge.CrossRepo)
	assert.Equal(t, "new", edge.Meta["payload"])
	assert.Equal(t, true, edge.Meta["resolve_terminal"])
	assert.Equal(t, "bound", edge.Meta["resolve_terminal_reason"])
	assert.Equal(t, "lsp", edge.Meta["semantic_source"])
}

func TestReindexEdgesResolvedConversionKindChangeUpdatesInPlace(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const (
		fromID    = "repo/caller.go::Caller"
		oldTarget = "unresolved::Target"
		newTarget = "repo/target.go::Target"
		filePath  = "repo/caller.go"
	)
	store.AddBatch(nil, []*graph.Edge{{
		From: fromID, To: oldTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 23,
	}})
	oldKey := sqliteReindexKey{
		fromID: fromID, toID: oldTarget, kind: string(graph.EdgeCalls), filePath: filePath, line: 23,
	}
	beforeID := reindexStoredEdgeID(t, store, oldKey)

	stats, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{{
		OldTo: oldTarget, OldKind: graph.EdgeCalls,
		Edge: &graph.Edge{
			From: fromID, To: newTarget, Kind: graph.EdgeReferences, FilePath: filePath, Line: 23,
			Confidence: 0.8, ConfidenceLabel: "confirmed", Origin: "kind-change", Tier: "semantic",
		},
	}})
	require.NoError(t, err)
	assert.Zero(t, stats.selectStatements)
	assert.Equal(t, 1, stats.updateStatements)
	assert.Equal(t, 1, stats.updatedRows)
	assert.Zero(t, stats.deleteStatements)
	assert.Zero(t, stats.insertStatements)

	newKey := sqliteReindexKey{
		fromID: fromID, toID: newTarget, kind: string(graph.EdgeReferences), filePath: filePath, line: 23,
	}
	assert.Equal(t, beforeID, reindexStoredEdgeID(t, store, newKey))
	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	assert.Equal(t, graph.EdgeReferences, persisted[0].Kind)
	assert.Equal(t, "kind-change", persisted[0].Origin)
}

func TestReindexEdgesResolvedConversionShortUpdateRepairsWholeChunk(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const (
		filePath      = "repo/caller.go"
		presentFrom   = "repo/caller.go::Present"
		missingFrom   = "repo/caller.go::Missing"
		presentOld    = "unresolved::Present"
		missingOld    = "unresolved::Missing"
		presentTarget = "repo/present.go::Target"
		missingTarget = "repo/missing.go::Target"
	)
	store.AddBatch(nil, []*graph.Edge{{
		From: presentFrom, To: presentOld, Kind: graph.EdgeCalls, FilePath: filePath, Line: 31,
	}})
	beforeAnalysisRevision := store.AnalysisMutationRevision()
	beforeMutationRevision := store.MutationRevision()
	batch := []graph.EdgeReindex{
		{OldTo: presentOld, Edge: &graph.Edge{
			From: presentFrom, To: presentTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 31,
			Origin: "updated-before-repair",
		}},
		{OldTo: missingOld, Edge: &graph.Edge{
			From: missingFrom, To: missingTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 32,
			Origin: "inserted-by-repair",
		}},
	}

	token := store.BeginMutationReceipt()
	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	receipt := store.EndMutationReceipt(token)
	assert.Zero(t, stats.selectStatements)
	assert.Equal(t, 1, stats.updateStatements)
	assert.Equal(t, 1, stats.updatedRows, "only the stored source can update before repair")
	assert.Equal(t, 1, stats.deleteStatements)
	assert.Zero(t, stats.deletedRows, "the successful update already removed its old identity")
	assert.Equal(t, 1, stats.insertStatements)
	assert.Equal(t, 1, stats.insertedRows, "repair must insert only the missing destination")
	assert.Equal(t, 3, stats.writeStatements())
	assert.Equal(t, beforeAnalysisRevision+1, store.AnalysisMutationRevision())
	assert.Equal(t, beforeMutationRevision+1, store.MutationRevision())
	assert.True(t, receipt.Complete)
	assert.False(t, receipt.ResolutionRelevant)

	present := store.GetOutEdges(presentFrom)
	require.Len(t, present, 1)
	assert.Equal(t, presentTarget, present[0].To)
	assert.Equal(t, "updated-before-repair", present[0].Origin)
	missing := store.GetOutEdges(missingFrom)
	require.Len(t, missing, 1)
	assert.Equal(t, missingTarget, missing[0].To)
	assert.Equal(t, "inserted-by-repair", missing[0].Origin)
}

func reindexStoredEdgeID(t *testing.T, store *Store, key sqliteReindexKey) int64 {
	t.Helper()
	var id int64
	err := store.db.QueryRow(
		`SELECT id FROM edges WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`,
		key.fromID, key.toID, key.kind, key.filePath, key.line,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func TestReindexEdgesResolvedConversionFastPathPreservesSetSemantics(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const (
		fromID          = "repo/caller.go::Caller"
		filePath        = "repo/caller.go"
		sharedTarget    = "repo/shared.go::Target"
		existingTarget  = "repo/existing.go::Target"
		firstOldTarget  = "unresolved::First"
		secondOldTarget = "unresolved::Second"
		thirdOldTarget  = "unresolved::Third"
	)
	oldEdges := []*graph.Edge{
		{From: fromID, To: firstOldTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 7, Origin: "old-first"},
		{From: fromID, To: secondOldTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 7, Origin: "old-second"},
		{From: fromID, To: thirdOldTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 8, Origin: "old-third"},
		{From: fromID, To: existingTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 8, Origin: "existing"},
	}
	store.AddBatch(nil, oldEdges)
	buildMinimalAnalysisGeneration(t, store, "resolved-conversion-fast-path", 0, true)
	beforeRevision := store.AnalysisMutationRevision()

	batch := []graph.EdgeReindex{
		{OldTo: firstOldTarget, Edge: &graph.Edge{
			From: fromID, To: sharedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 7,
			Confidence: 0.9, ConfidenceLabel: "confirmed", Origin: "first", Tier: "semantic",
			Meta: map[string]any{"winner": "first"},
		}},
		{OldTo: secondOldTarget, Edge: &graph.Edge{
			From: fromID, To: sharedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 7,
			Confidence: 0.4, ConfidenceLabel: "heuristic", Origin: "second", Tier: "syntax",
			Meta: map[string]any{"winner": "second"},
		}},
		{OldTo: thirdOldTarget, Edge: &graph.Edge{
			From: fromID, To: existingTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 8,
			Confidence: 1, ConfidenceLabel: "confirmed", Origin: "replacement", Tier: "compiler",
		}},
	}

	token := store.BeginMutationReceipt()
	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	receipt := store.EndMutationReceipt(token)
	assert.Zero(t, stats.selectStatements, "resolved conversions must not prefetch full edge rows")
	assert.Equal(t, 1, stats.updateStatements)
	assert.Zero(t, stats.updatedRows, "the pre-existing destination must force exact chunk repair")
	assert.Equal(t, 1, stats.deleteStatements)
	assert.Equal(t, 1, stats.insertStatements)
	assert.Equal(t, len(batch), stats.deletedRows)
	assert.Equal(t, 1, stats.insertedRows, "one duplicate and one existing destination must be ignored")
	assert.Equal(t, beforeRevision+1, store.AnalysisMutationRevision())
	assert.True(t, receipt.Complete)
	assert.False(t, receipt.ResolutionRelevant, "resolved destinations cannot create pending resolver work")

	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 2)
	byTarget := make(map[string]*graph.Edge, len(persisted))
	for _, edge := range persisted {
		byTarget[edge.To] = edge
	}
	shared := byTarget[sharedTarget]
	require.NotNil(t, shared)
	assert.Equal(t, "first", shared.Origin, "input order must preserve first-candidate-wins semantics")
	assert.Equal(t, "semantic", shared.Tier)
	assert.Equal(t, "first", shared.Meta["winner"])
	existing := byTarget[existingTarget]
	require.NotNil(t, existing)
	assert.Equal(t, "existing", existing.Origin, "a pre-existing destination must win over a conversion")

	buildMinimalAnalysisGeneration(t, store, "resolved-conversion-replay", 0, true)
	replayRevision := store.AnalysisMutationRevision()
	token = store.BeginMutationReceipt()
	replayStats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	replayReceipt := store.EndMutationReceipt(token)
	assert.Zero(t, replayStats.selectStatements)
	assert.Equal(t, 1, replayStats.updateStatements)
	assert.Zero(t, replayStats.updatedRows)
	assert.Zero(t, replayStats.deletedRows)
	assert.Zero(t, replayStats.insertedRows)
	assert.Equal(t, replayRevision, store.AnalysisMutationRevision(), "an idempotent replay must not invalidate analysis")
	assert.True(t, replayReceipt.Complete)
	assert.False(t, replayReceipt.ResolutionRelevant)
	_, found, err := store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	assert.True(t, found, "an idempotent replay must preserve active warm analysis")
}

func TestReindexEdgesResolvedConversionReverseUpdatesInPlace(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const (
		fromID           = "repo/caller.go::Caller"
		resolvedTarget   = "repo/target.go::Target"
		unresolvedTarget = "unresolved::Target"
		filePath         = "repo/caller.go"
	)
	store.AddBatch(nil, []*graph.Edge{{
		From: fromID, To: resolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 41,
		Confidence: 0.7, ConfidenceLabel: "heuristic", Origin: "syntax", Tier: "syntax",
		Meta: map[string]any{"payload": "old"},
	}})
	oldKey := sqliteReindexKey{
		fromID: fromID, toID: resolvedTarget, kind: string(graph.EdgeCalls), filePath: filePath, line: 41,
	}
	beforeID := reindexStoredEdgeID(t, store, oldKey)

	stats, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{{
		OldTo: resolvedTarget,
		Edge: &graph.Edge{
			From: fromID, To: unresolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 41,
			Confidence: 0,
			Meta: map[string]any{
				"guard_reverted": true,
			},
		},
	}})
	require.NoError(t, err)
	assert.Zero(t, stats.selectStatements)
	assert.Equal(t, 1, stats.updateStatements)
	assert.Equal(t, 1, stats.updatedRows)
	assert.Zero(t, stats.deleteStatements)
	assert.Zero(t, stats.insertStatements)

	newKey := sqliteReindexKey{
		fromID: fromID, toID: unresolvedTarget, kind: string(graph.EdgeCalls), filePath: filePath, line: 41,
	}
	assert.Equal(t, beforeID, reindexStoredEdgeID(t, store, newKey), "reverse conversion must preserve the SQLite row id")
	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	assert.Equal(t, unresolvedTarget, persisted[0].To)
	assert.Zero(t, persisted[0].Confidence)
	assert.Equal(t, true, persisted[0].Meta["guard_reverted"])
}

func TestReindexEdgesResolvedConversionReverseUsesReceiptSafeGenericPath(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const (
		fromID           = "repo/caller.go::ReceiptCaller"
		resolvedTarget   = "repo/target.go::ReceiptTarget"
		unresolvedTarget = "unresolved::ReceiptTarget"
		filePath         = "repo/caller.go"
	)
	store.AddBatch(nil, []*graph.Edge{{
		From: fromID, To: resolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 45,
	}})

	token := store.BeginMutationReceipt()
	stats, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{{
		OldTo: resolvedTarget,
		Edge: &graph.Edge{
			From: fromID, To: unresolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 45,
			Origin: "guard",
		},
	}})
	require.NoError(t, err)
	receipt := store.EndMutationReceipt(token)

	assert.Equal(t, 1, stats.selectStatements)
	assert.Zero(t, stats.updateStatements, "active receipts require exact per-edge insert accounting")
	assert.Equal(t, 1, stats.deleteStatements)
	assert.Equal(t, 1, stats.deletedRows)
	assert.Equal(t, 1, stats.insertStatements)
	assert.Equal(t, 1, stats.insertedRows)
	assert.True(t, receipt.Complete)
	assert.True(t, receipt.ResolutionRelevant)
	assert.Equal(t, []string{filePath}, receipt.ChangedFiles)
	assert.Equal(t, []string{filePath}, receipt.ResolutionFiles())
	assert.Equal(t, []string{"ReceiptTarget"}, receipt.TargetNames)
	assert.Equal(t, []string{unresolvedTarget}, receipt.TargetIDs)
}

func TestReindexEdgesResolvedConversionReverseReceiptIgnoresExistingDestination(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const (
		fromID           = "repo/caller.go::CollisionCaller"
		resolvedTarget   = "repo/target.go::CollisionTarget"
		unresolvedTarget = "unresolved::CollisionTarget"
		filePath         = "repo/caller.go"
	)
	store.AddBatch(nil, []*graph.Edge{
		{From: fromID, To: resolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 47, Origin: "resolved"},
		{From: fromID, To: unresolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 47, Origin: "existing"},
	})

	token := store.BeginMutationReceipt()
	stats, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{{
		OldTo: resolvedTarget,
		Edge: &graph.Edge{
			From: fromID, To: unresolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 47,
			Origin: "replacement",
		},
	}})
	require.NoError(t, err)
	receipt := store.EndMutationReceipt(token)

	assert.Equal(t, 1, stats.selectStatements)
	assert.Zero(t, stats.updateStatements)
	assert.Equal(t, 1, stats.deletedRows)
	assert.Zero(t, stats.insertedRows, "the pre-existing unresolved destination is not new resolver work")
	assert.True(t, receipt.Complete)
	assert.False(t, receipt.ResolutionRelevant)
	assert.Empty(t, receipt.ResolutionFiles())
	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	assert.Equal(t, unresolvedTarget, persisted[0].To)
	assert.Equal(t, "existing", persisted[0].Origin)
}

func TestReindexEdgesResolvedConversionRejectsMixedDirections(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const filePath = "repo/caller.go"
	store.AddBatch(nil, []*graph.Edge{
		{From: "repo/caller.go::Forward", To: "unresolved::Forward", Kind: graph.EdgeCalls, FilePath: filePath, Line: 51},
		{From: "repo/caller.go::Reverse", To: "repo/target.go::Reverse", Kind: graph.EdgeCalls, FilePath: filePath, Line: 52},
	})

	stats, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{
		{OldTo: "unresolved::Forward", Edge: &graph.Edge{
			From: "repo/caller.go::Forward", To: "repo/target.go::Forward", Kind: graph.EdgeCalls, FilePath: filePath, Line: 51,
		}},
		{OldTo: "repo/target.go::Reverse", Edge: &graph.Edge{
			From: "repo/caller.go::Reverse", To: "unresolved::Reverse", Kind: graph.EdgeCalls, FilePath: filePath, Line: 52,
		}},
	})
	require.NoError(t, err)
	assert.NotZero(t, stats.selectStatements)
	assert.Zero(t, stats.updateStatements)
	assert.Equal(t, 2, stats.deletedRows)
	assert.Equal(t, 2, stats.insertedRows)
}

func TestReindexEdgesResolvedConversionRefreshIdentityUsesGenericPath(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const (
		fromID           = "repo/caller.go::Refresh"
		resolvedTarget   = "repo/target.go::Refresh"
		unresolvedTarget = "unresolved::Refresh"
		filePath         = "repo/caller.go"
	)
	store.AddBatch(nil, []*graph.Edge{{
		From: fromID, To: resolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 61,
	}})

	stats, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{{
		OldFrom: fromID, OldTo: resolvedTarget, OldKind: graph.EdgeCalls,
		OldFilePath: filePath, OldLine: 61, RefreshIdentity: true,
		Edge: &graph.Edge{
			From: fromID, To: unresolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 61,
		},
	}})
	require.NoError(t, err)
	assert.NotZero(t, stats.selectStatements)
	assert.Zero(t, stats.updateStatements)
	assert.Equal(t, 1, stats.deletedRows)
	assert.Equal(t, 1, stats.insertedRows)
}

func TestReindexEdgesResolvedConversionReverseConvergencePreservesFirstCandidate(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const (
		fromID           = "repo/caller.go::Caller"
		firstResolved    = "repo/first.go::Target"
		secondResolved   = "repo/second.go::Target"
		unresolvedTarget = "unresolved::Target"
		filePath         = "repo/caller.go"
	)
	store.AddBatch(nil, []*graph.Edge{
		{From: fromID, To: firstResolved, Kind: graph.EdgeCalls, FilePath: filePath, Line: 71, Origin: "old-first"},
		{From: fromID, To: secondResolved, Kind: graph.EdgeCalls, FilePath: filePath, Line: 71, Origin: "old-second"},
	})

	stats, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{
		{OldTo: firstResolved, Edge: &graph.Edge{
			From: fromID, To: unresolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 71,
			Origin: "first", Meta: map[string]any{"winner": "first"},
		}},
		{OldTo: secondResolved, Edge: &graph.Edge{
			From: fromID, To: unresolvedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 71,
			Origin: "second", Meta: map[string]any{"winner": "second"},
		}},
	})
	require.NoError(t, err)
	assert.Zero(t, stats.selectStatements)
	assert.Zero(t, stats.updateStatements)
	assert.Equal(t, 1, stats.deleteStatements)
	assert.Equal(t, 2, stats.deletedRows)
	assert.Equal(t, 1, stats.insertStatements)
	assert.Equal(t, 1, stats.insertedRows)

	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	assert.Equal(t, unresolvedTarget, persisted[0].To)
	assert.Equal(t, "first", persisted[0].Origin)
	assert.Equal(t, "first", persisted[0].Meta["winner"])
}

func TestSQLiteResolvedConversionUpdatePlanDefersFallbackBuffers(t *testing.T) {
	mutations := []sqliteReindexMutation{
		{
			oldKey:             sqliteReindexKey{fromID: "repo/a.go::A", toID: "unresolved::A", kind: string(graph.EdgeCalls)},
			newRow:             sqliteReindexRow{key: sqliteReindexKey{fromID: "repo/a.go::A", toID: "repo/target.go::A", kind: string(graph.EdgeCalls)}},
			resolvedConversion: true,
		},
		{
			oldKey:             sqliteReindexKey{fromID: "repo/b.go::B", toID: "unresolved::B", kind: string(graph.EdgeCalls)},
			newRow:             sqliteReindexRow{key: sqliteReindexKey{fromID: "repo/b.go::B", toID: "repo/target.go::B", kind: string(graph.EdgeReferences)}},
			resolvedConversion: true,
		},
	}

	plan, ok := sqliteResolvedConversionUpdatePlan(mutations)
	require.True(t, ok)
	assert.True(t, plan.oldUnresolved)
	assert.Len(t, plan.oldCounts, len(mutations))
	assert.Len(t, plan.newCounts, len(mutations))
	assert.Nil(t, plan.fallbackDeletes, "unique conversions must not allocate fallback key storage")
	assert.Nil(t, plan.fallbackInserts, "unique conversions must not copy full rows into fallback storage")
	assert.True(t, plan.updateCandidate(mutations[0], false))
	assert.True(t, plan.updateCandidate(mutations[1], true))
}

func BenchmarkSQLiteResolvedConversionPlanning50K(b *testing.B) {
	const edgeCount = 50_000
	mutations := make([]sqliteReindexMutation, edgeCount)
	for i := range mutations {
		from := fmt.Sprintf("repo/caller-%05d.go::Caller", i)
		mutations[i] = sqliteReindexMutation{
			oldKey: sqliteReindexKey{
				fromID: from, toID: fmt.Sprintf("unresolved::Target%05d", i), kind: string(graph.EdgeCalls),
			},
			newRow: sqliteReindexRow{key: sqliteReindexKey{
				fromID: from, toID: fmt.Sprintf("repo/target-%05d.go::Target", i), kind: string(graph.EdgeCalls),
			}},
			resolvedConversion: true,
		}
	}
	b.ReportAllocs()
	b.ReportMetric(edgeCount, "edges/op")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		plan, ok := sqliteResolvedConversionUpdatePlan(mutations)
		if !ok || len(plan.oldCounts) != edgeCount || len(plan.newCounts) != edgeCount ||
			plan.fallbackDeletes != nil || plan.fallbackInserts != nil {
			b.Fatalf("unexpected conversion plan: ok=%t old=%d new=%d fallback_deletes=%d fallback_inserts=%d",
				ok, len(plan.oldCounts), len(plan.newCounts), len(plan.fallbackDeletes), len(plan.fallbackInserts))
		}
	}
}

func BenchmarkReindexEdgesResolvedConversions50K(b *testing.B) {
	const edgeCount = 50_000
	b.StopTimer()
	oldEdges := make([]*graph.Edge, 0, edgeCount)
	batch := make([]graph.EdgeReindex, 0, edgeCount)
	for i := 0; i < edgeCount; i++ {
		from := fmt.Sprintf("repo/caller-%05d.go::Caller", i)
		oldTo := fmt.Sprintf("unresolved::Target%05d", i)
		oldEdges = append(oldEdges, &graph.Edge{
			From: from, To: oldTo, Kind: graph.EdgeCalls,
			FilePath: "repo/callers.go", Line: i + 1,
		})
		batch = append(batch, graph.EdgeReindex{
			OldTo: oldTo,
			Edge: &graph.Edge{
				From: from, To: fmt.Sprintf("repo/target-%05d.go::Target", i),
				Kind: graph.EdgeCalls, FilePath: "repo/callers.go", Line: i + 1,
			},
		})
	}
	b.ReportAllocs()
	b.ReportMetric(edgeCount, "edges/op")
	benchDir := b.TempDir()
	for i := 0; i < b.N; i++ {
		store, err := Open(filepath.Join(benchDir, fmt.Sprintf("graph-%d.sqlite", i)))
		if err != nil {
			b.Fatal(err)
		}
		store.AddBatch(nil, oldEdges)
		b.StartTimer()
		stats, err := store.reindexEdgesSetOriented(batch)
		b.StopTimer()
		if err != nil {
			_ = store.Close()
			b.Fatal(err)
		}
		if stats.selectStatements != 0 || stats.updatedRows != edgeCount || stats.deletedRows != 0 || stats.insertedRows != 0 {
			_ = store.Close()
			b.Fatalf("unexpected fast-path stats: %+v", stats)
		}
		if err := store.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
