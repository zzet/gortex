package store_sqlite

import (
	"context"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
)

// Source-only intended-red control pending persisted metadata evidence. A
// seeded store's adoption is metadata work; a missing trigger still requires
// seeding and must continue to yield to the actual edit lane.
func TestRowCountersReopenAdoptsInstalledMetadataDuringAnEdit(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "0")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	for _, mode := range []string{"installed", "missing_trigger", "cancelled"} {
		name := mode
		missing := mode == "missing_trigger"
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "adopt.sqlite")
			s, err := openPristine(t, path)
			require.NoError(t, err)
			nodes, edges := rowCounterFixture("repo/adopt.go", 4)
			require.NoError(t, s.AddBatchChecked(nodes, edges))
			require.NoError(t, s.EnsureRowCounters(t.Context()))
			if missing {
				_, err = s.writerDB.ExecContext(t.Context(), `DROP TRIGGER generation_row_counts_nodes_insert`)
				require.NoError(t, err)
			}
			require.NoError(t, s.Close())
			reopened, err := Open(path)
			require.NoError(t, err)
			defer func() { _ = reopened.Close() }()
			reopened.SetBuildLaneBusy(func() bool { return true })
			defer reopened.SetBuildLaneBusy(nil)
			require.True(t, reopened.cycleYieldEnabled())
			seedStarted := false
			rowCountersBeforeReadConnHook = func() { seedStarted = true }
			defer func() { rowCountersBeforeReadConnHook = nil }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			err = reopened.EnsureRowCounters(ctx)
			require.False(t, seedStarted, "metadata adoption must not reserve a seed snapshot or start scans")
			if mode == "cancelled" {
				require.ErrorIs(t, err, context.Canceled)
				require.False(t, reopened.RowCountersReady())
			} else if missing {
				require.ErrorIs(t, err, errRowCountersEditCycle)
				require.False(t, reopened.RowCountersReady())
			} else {
				require.NoError(t, err)
				require.True(t, reopened.RowCountersReady())
				requireCountersExact(t, reopened, 0)
			}
		})
	}
}
