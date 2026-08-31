package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// captureLog redirects the standard logger for the duration of fn and returns
// everything written to it. The periodic checkpoint reports through log.Printf,
// so its message is the contract under test.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	fn()
	log.SetOutput(prevWriter)
	log.SetFlags(prevFlags)
	return buf.String()
}

// A periodic PASSIVE checkpoint that never got the writer connection has
// measured nothing: walCheckpointResult is still the zero value. Printing its
// fields states busy=0 wal_frames=0 checkpointed_frames=0 as if SQLite had
// answered, and calling that "incomplete" describes a checkpoint that ran and
// fell short. Both are wrong — the checkpoint never started.
//
// The contention is real here rather than simulated with an already-expired
// context: writerDB is capped at one physical connection, so holding it is
// exactly the production shape (a bulk index owning the writer) and the
// deadline is spent waiting for acquisition.
// See https://github.com/zzet/gortex/issues/325.
func TestPassiveCheckpointPinnedWriterIsDeferredNotIncomplete(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"})

	pinned, err := store.writerDB.Conn(context.Background())
	require.NoError(t, err, "the sole writer connection must be obtainable")
	defer func() { _ = pinned.Close() }()

	store.passiveCheckpointTimeout = 100 * time.Millisecond
	started := time.Now()
	var retry bool
	out := captureLog(t, func() { retry = store.checkpointWALPassive() })

	assert.Less(t, time.Since(started), 2*time.Second,
		"the periodic checkpoint must give up on the window, not block")
	assert.Contains(t, out, "deferred mode=PASSIVE reason=writer_busy",
		"a checkpoint that never acquired the writer is a deferral, like its writer_gate and bulk_writer siblings")
	assert.NotContains(t, out, "incomplete",
		"a checkpoint that never executed must not be reported as incomplete")
	assert.NotContains(t, out, "wal_frames=",
		"zero-value result fields must not be printed as measurements")
	assert.True(t, retry, "writer-pool deferral must schedule a prompt retry")
}

// The opposite case must keep its warning. Here the PRAGMA does run, returns
// real counters, and leaves frames behind because a reader still holds the
// WAL — that is a measurement an operator should see. This pins the fix to
// classifying the failure rather than silencing the branch.
func TestPassiveCheckpointReaderLimitedStillWarns(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"})

	readConn, err := store.db.Conn(context.Background())
	require.NoError(t, err)
	readTx, err := readConn.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() {
		_ = readTx.Rollback()
		_ = readConn.Close()
	}()
	var count int
	require.NoError(t, readTx.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&count))

	// Written after the reader's snapshot, so these frames cannot be drained
	// while the read transaction is open.
	store.AddNode(&graph.Node{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B"})

	var retry bool
	out := captureLog(t, func() { retry = store.checkpointWALPassive() })

	assert.Contains(t, out, "incomplete",
		"a checkpoint that ran and left frames behind must still warn")
	assert.True(t, retry, "measured incomplete checkpoint must schedule a prompt retry")
}

// The classification order is the substance of the fix, and a real deadline
// cannot pin it: whether SQLite returns its counters just before or just after
// the window expires is a race. Drive the reporter directly instead.
func TestPassiveCheckpointReportPrefersMeasuredCountersOverDeadline(t *testing.T) {
	measured := walCheckpointResult{Busy: 0, WALFrames: 294, CheckpointedFrames: 287}
	incomplete := fmt.Errorf("%w: mode=PASSIVE busy=0 wal_frames=294 checkpointed_frames=287", errSQLiteCheckpointIncomplete)

	// A checkpoint that reported real counters and then blew its window must
	// still be reported by those counters. Reading the deadline first would
	// throw away the only measurement SQLite gave us.
	got := passiveCheckpointReport(measured, incomplete, context.DeadlineExceeded)
	assert.Contains(t, got, "incomplete")
	assert.Contains(t, got, "wal_frames=294")
	assert.NotContains(t, got, "timed out")

	// An execution timeout carries nothing measured, so it must not print
	// zero-value counters as if SQLite had answered.
	got = passiveCheckpointReport(walCheckpointResult{}, context.DeadlineExceeded, context.DeadlineExceeded)
	assert.Contains(t, got, "timed out")
	assert.Contains(t, got, "phase=execute")
	assert.NotContains(t, got, "wal_frames=")

	// A plain driver error is neither a deferral nor a measurement.
	got = passiveCheckpointReport(walCheckpointResult{}, errors.New("disk I/O error"), nil)
	assert.Contains(t, got, "failed")
	assert.NotContains(t, got, "wal_frames=")
	assert.NotContains(t, got, "timed out")
}
