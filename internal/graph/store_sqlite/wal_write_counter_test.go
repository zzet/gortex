package store_sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The per-edit WAL counter: frames appended between two marks, the byte
// figure derived from the page size, and a reset between the marks reported
// as a lower bound rather than a negative or wrapped count.
func TestWALWriteMarkCountsTheFramesAWriteAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	write := func(tag string, n int) {
		nodes := make([]*graph.Node, 0, n)
		for i := 0; i < n; i++ {
			nodes = append(nodes, &graph.Node{
				ID: fmt.Sprintf("repo/%s.go::F%d", tag, i), Kind: graph.KindFunction,
				Name: fmt.Sprintf("F%d", i), FilePath: "repo/" + tag + ".go", RepoPrefix: "repo",
			})
		}
		store.AddBatch(nodes, nil)
	}

	write("warm", 1)
	before := store.WALWriteMark()
	require.True(t, before.Valid, "a file store in WAL mode has a readable wal-index")
	require.NotZero(t, before.PageSize)

	idle := WALWrittenBetween(before, store.WALWriteMark())
	require.True(t, idle.Valid)
	require.Zero(t, idle.Frames, "no write, no frames")
	require.Zero(t, idle.Bytes)

	write("edit", 200)
	after := store.WALWriteMark()
	delta := WALWrittenBetween(before, after)
	require.True(t, delta.Valid)
	require.False(t, delta.Reset)
	require.Positive(t, delta.Frames, "a committed write appends frames")
	require.Equal(t, delta.Frames*int64(after.PageSize+walFrameHeaderBytes), delta.Bytes)

	// A reset between the marks: the log is truncated, then written again.
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	_, err = raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
	write("after-reset", 3)
	reset := WALWrittenBetween(after, store.WALWriteMark())
	require.True(t, reset.Valid)
	require.True(t, reset.Reset, "the salt moved: the frames before the reset are not countable")
	require.Positive(t, reset.Frames, "the frames since the reset are still counted")

	// A reset followed by MORE frames than the earlier mark held: only the
	// salt tells the two log incarnations apart.
	_, err = raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
	write("small", 1)
	small := store.WALWriteMark()
	_, err = raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
	write("big", 400)
	big := store.WALWriteMark()
	require.Greater(t, big.MxFrame, small.MxFrame, "the new incarnation outgrew the old mark")
	grown := WALWrittenBetween(small, big)
	require.True(t, grown.Reset, "a moved salt is a reset even when mxFrame grew")
	require.Equal(t, int64(big.MxFrame), grown.Frames)

	require.False(t, WALWrittenBetween(WALWriteMark{}, after).Valid, "an unreadable mark yields no figure")
	var closed *Store
	require.False(t, closed.WALWriteMark().Valid)
}
