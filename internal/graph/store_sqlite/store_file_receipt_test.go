package store_sqlite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestFileReceiptPagerFreezesHighWaterAndClosesRows(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "receipts.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	require.NoError(t, store.BulkSetFileMtimes("repo", map[string]int64{
		"a.go": 1,
		"b.go": 2,
		"c.go": 3,
		"d.go": 4,
	}))
	require.NoError(t, store.SetFileMetas("repo", []graph.FileMetaRow{
		{FilePath: "repo/a.go", ContentHash: "hash-a", Size: 10, NodeCount: 999, Errors: "large metadata is not projected"},
		{FilePath: "repo/b.go", ContentHash: "hash-b", Size: 20},
		{FilePath: "repo/c.go", ContentHash: "hash-c", Size: 30},
		{FilePath: "repo/d.go", ContentHash: "hash-d", Size: 40},
	}))

	highWater, err := store.FileReceiptHighWater("repo")
	require.NoError(t, err)
	require.Equal(t, "d.go", highWater)

	// One connection turns an accidentally open mtime cursor into a deadlock
	// when includeContent starts its second compact query. Returning proves the
	// first result was closed before content lookup; InUse proves both closed.
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)
	page, err := store.FileReceiptPage("repo", "", highWater, 2, true)
	require.NoError(t, err)
	require.Equal(t, []graph.FileReceipt{
		{FilePath: "a.go", MtimeNS: 1, ContentHash: "hash-a", Size: 10},
		{FilePath: "b.go", MtimeNS: 2, ContentHash: "hash-b", Size: 20},
	}, page)
	require.Zero(t, store.db.Stats().InUse)

	// A path inserted beyond the frozen high-water cannot prolong this scan.
	// An in-place update below it is safe to observe at its latest value.
	require.NoError(t, store.BulkSetFileMtimes("repo", map[string]int64{
		"c.go": 33,
		"z.go": 99,
	}))
	require.NoError(t, store.SetFileMetas("repo", []graph.FileMetaRow{
		{FilePath: "repo/c.go", ContentHash: "hash-c-new", Size: 31},
		{FilePath: "repo/z.go", ContentHash: "hash-z", Size: 99},
	}))
	page, err = store.FileReceiptPage("repo", "b.go", highWater, 8, true)
	require.NoError(t, err)
	require.Equal(t, []graph.FileReceipt{
		{FilePath: "c.go", MtimeNS: 33, ContentHash: "hash-c-new", Size: 31},
		{FilePath: "d.go", MtimeNS: 4, ContentHash: "hash-d", Size: 40},
	}, page)

	planRows, err := store.db.Query(
		"EXPLAIN QUERY PLAN "+fileReceiptPageQuery,
		store.viewGen, "repo", "", highWater, 128,
	)
	require.NoError(t, err)
	var plan []string
	for planRows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, planRows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, planRows.Err())
	require.NoError(t, planRows.Close())
	require.Contains(t, strings.ToUpper(strings.Join(plan, "\n")), "PRIMARY KEY")
}

func TestFileReceiptPagerUsesCanonicalSlashPathAndHandlesDeletedHighWater(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "receipts.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	require.NoError(t, store.BulkSetFileMtimes("repo", map[string]int64{
		"nested/a.go": 1,
		"nested/b.go": 2,
	}))
	graphA := "repo/" + filepath.FromSlash("nested/a.go")
	graphB := "repo/" + filepath.FromSlash("nested/b.go")
	require.NoError(t, store.SetFileMetas("repo", []graph.FileMetaRow{
		{FilePath: graphA, ContentHash: "a", Size: 1},
		{FilePath: graphB, ContentHash: "b", Size: 2},
	}))

	highWater, err := store.FileReceiptHighWater("repo")
	require.NoError(t, err)
	page, err := store.FileReceiptPage("repo", "", highWater, 1, true)
	require.NoError(t, err)
	require.Equal(t, []graph.FileReceipt{{
		FilePath: "nested/a.go", MtimeNS: 1, ContentHash: "a", Size: 1,
	}}, page)

	// Deleting the exact high-water between pages yields a clean empty terminal
	// page; the Poller owns rotation reset and must not hold an open cursor.
	require.NoError(t, store.DeleteFileMtimes("repo", []string{"nested/b.go"}))
	page, err = store.FileReceiptPage("repo", "nested/a.go", highWater, 1, false)
	require.NoError(t, err)
	require.Empty(t, page)
	require.Zero(t, store.db.Stats().InUse)

	empty, err := store.FileReceiptPage("repo", highWater, highWater, 1, true)
	require.NoError(t, err)
	require.Empty(t, empty)
}
