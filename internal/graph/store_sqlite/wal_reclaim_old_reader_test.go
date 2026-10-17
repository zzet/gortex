package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The live 22 GB case: the log is backfilled except the frames one old reader
// still needs, so every attempt gives up on that reader — and names it. As
// soon as the reader ends, the next attempt resets the log.
func TestWALReclaimResetsAsSoonAsTheOneOldReaderEnds(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	logs := captureReclaimLog(t)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 4)
	old, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = old.Rollback() }()
	var n int
	require.NoError(t, old.QueryRow(`SELECT count(*) FROM wal_churn WHERE id > 0`).Scan(&n))
	growWAL(t, s, 4) // frames the old reader's snapshot predates

	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	cfg := walReclaimConfig{thresholdBytes: 1 << 30, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: 500 * time.Millisecond}

	prevWarn := walReclaimBlockedReaderWarnAge
	walReclaimBlockedReaderWarnAge = 0
	t.Cleanup(func() { walReclaimBlockedReaderWarnAge = prevWarn })
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	logWALReclaimOutcome(res, time.Second, &walReclaimSkipLog{}, time.Now())
	t.Logf("with the old reader: outcome=%s reason=%q blocker=%+v", res.outcome, res.reason, res.blocker)
	require.Equal(t, walReclaimDeferred, res.outcome)
	require.True(t, res.hasBlocker, "the blocking reader must be identified")
	require.Contains(t, res.blocker.Label, "wal_churn", "the blocker's statement must be named")
	require.LessOrEqual(t, res.writerHold, walReclaimMaxWriterHold+100*time.Millisecond)
	require.Contains(t, logs.String(), "WARN wal reclaim blocked by a long reader")
	snap, _ := readWALIndexSnapshot(path)
	require.Less(t, snap.NBackfill, snap.MxFrame, "the old reader pins frames past the backfill")

	require.NoError(t, old.Rollback())
	started := time.Now()
	res = s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	t.Logf("after the reader ended: outcome=%s reason=%q elapsed=%s writer_hold=%s", res.outcome, res.reason, time.Since(started), res.writerHold)
	require.Equal(t, walReclaimReset, res.outcome, "reason=%q", res.reason)
	require.Zero(t, walFileSize(path+"-wal"))
}

// A reader that began after the backfill completed holds read slot 0: it
// never blocks the reset, the reset does not wait for it, and it keeps
// reading correctly afterwards.
func TestWALReclaimIsNotBlockedByAReaderStartedAfterTheBackfill(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 8)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	r, err := checkpointWALOnceOn(context.Background(), ckpt, "PASSIVE")
	require.NoError(t, err)
	require.False(t, r.incomplete(), "the backfill must be complete before the reader starts")

	late, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = late.Rollback() }()
	var before int
	require.NoError(t, late.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&before))

	cfg := walReclaimConfig{thresholdBytes: 1 << 30, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: 20 * time.Second}
	started := time.Now()
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	elapsed := time.Since(started)
	t.Logf("outcome=%s reason=%q elapsed=%s writer_hold=%s", res.outcome, res.reason, elapsed, res.writerHold)
	require.Equal(t, walReclaimReset, res.outcome, "reason=%q", res.reason)
	require.Less(t, elapsed, 2*time.Second, "the reset waited for a reader that does not block it")
	require.Zero(t, walFileSize(path+"-wal"))
	var after int
	require.NoError(t, late.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&after))
	require.Equal(t, before, after, "the late reader's snapshot must survive the reset")
}

// NodesByKinds pages its read (one short read transaction per page) and
// returns exactly the single-statement answer: rows, order, a page boundary,
// generations and kinds included.
func TestNodesByKindsPagesAndMatchesTheSingleStatement(t *testing.T) {
	prev := nodesByKindsPageSize
	nodesByKindsPageSize = 7
	t.Cleanup(func() { nodesByKindsPageSize = prev })
	s, _ := openTempStore(t)
	for _, generation := range []int64{0, 4} {
		var nodes []*graph.Node
		for i := 0; i < 50; i++ {
			kind := []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType}[i%3]
			file := fmt.Sprintf("repo/f%02d.go", i%5)
			nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s::N%03d_g%d", file, i, generation), Kind: kind, Name: fmt.Sprintf("N%d", i), FilePath: file, RepoPrefix: "repo", Language: "go"})
		}
		require.NoError(t, s.AtGeneration(generation).AddBatchChecked(nodes, nil))
	}
	for _, generation := range []int64{0, 4} {
		h := s.AtGeneration(generation)
		kinds := []graph.NodeKind{graph.KindFunction, graph.KindType}
		got := h.NodesByKinds(kinds)
		want := h.queryNodesSQL(`SELECT `+lookupNodeCols+` FROM nodes WHERE kind IN (?, ?) AND view_gen = ? ORDER BY id`, string(graph.KindFunction), string(graph.KindType), generation)
		require.Len(t, got, 33) // 17 functions + 16 types: several 7-row pages
		require.True(t, reflect.DeepEqual(got, want), "generation %d: paged rows differ from the single statement", generation)
		for _, n := range got {
			require.True(t, strings.HasSuffix(n.ID, fmt.Sprintf("_g%d", generation)))
		}
	}
}
