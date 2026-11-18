package store_sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAdaptiveDeclinedEntryPreservesCopyEntitlement(t *testing.T) {
	for _, kind := range []string{"lane", "bulk", "new_wal"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			frontier, ok := readWALReclaimFrontier(s.dbPath)
			require.True(t, ok)
			r := &walReclaimResult{urgent: true, slowTail: &walReclaimSlowTail{salt: frontier.salt, covered: frontier.backfill, copyElapsed: 300 * time.Millisecond}}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			switch kind {
			case "lane":
				lane := &fakeBuildLane{}
				lane.install(s)
				lane.held.Store(true)
				t.Cleanup(func() { lane.held.Store(false) })
			case "bulk":
				engaged, err := s.BeginGenerationBulkLoad(77)
				require.NoError(t, err)
				require.True(t, engaged)
				t.Cleanup(func() { _ = s.EndGenerationBulkLoadFor(77) })
			case "new_wal":
				_, err := checkpointWALOnceOn(ctx, db, "TRUNCATE")
				require.NoError(t, err)
				s.writeMu.Lock()
				_, err = s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = 'new-WAL' WHERE id = 1`)
				s.writeMu.Unlock()
				require.NoError(t, err)
			}
			budget := r.takeAdaptiveWriterBudget()
			require.Positive(t, budget)
			done, _ := s.reclaimWALResetHoldOnce(ctx, walReclaimConfig{}, db, r, false, true, budget)
			require.False(t, done)
			require.False(t, r.adaptiveUsed, "a declined admission consumed the single actual copy entitlement")
			require.Positive(t, r.takeAdaptiveWriterBudget())
			require.Less(t, r.writerSpent, walReclaimResetHold)
		})
	}
}

// This test-only scope publishes a fully initialized TLS observer. It neither
// restores live VFS method tables nor changes SQL/copy contexts or writer credit.
func observeAdaptiveMultiFrameSync(t *testing.T, db *sql.DB, before func(int), after func(int, time.Time, time.Time)) {
	t.Helper()
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	key, ok := pausableConn(conn)
	require.NoError(t, conn.Close())
	require.True(t, ok)
	reclaimSyncStallState.Store(&reclaimSyncStall{tls: key, beforeSync: before, afterSync: after})
	t.Cleanup(func() { reclaimSyncStallState.Store(nil) })
}
