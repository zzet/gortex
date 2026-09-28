package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The writer's log appends and a reader's reads of pages whose latest version
// is in the log show up in the split, attributed to the right connection and
// file.
func TestVFSIOCountersSplitByFileAndConnection(t *testing.T) {
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	if !vfsIOInstalled.Load() {
		t.Skip("vfs io counters not installed in this process")
	}
	seedWALChurnTable(t, s)
	before := s.ReaderWaitMark()
	growWAL(t, s, 4) // through the write gate: its release records the writer
	growWAL(t, s, 4)
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM wal_churn WHERE length(payload) > 0`).Scan(&n))
	d := s.ReaderWaitMark().Split(before).VFS
	t.Logf("vfs: %+v", d)
	require.Positive(t, d.WriterWALWrites, "the writer's log appends")
	require.Positive(t, d.WriterWALWriteBytes)
	require.Positive(t, d.OtherWALReads, "the reader's pages from the log")
	require.Zero(t, d.OtherMainWrites, "nothing checkpointed")
}

// The cost of counting one read: the wrapper's bookkeeping around a no-op.
func BenchmarkVFSIOCell(b *testing.B) {
	for i := 0; i < b.N; i++ {
		c := vfsCell(nil, uintptr(i&1023))
		c.readCalls.Add(1)
		c.readNanos.Add(1)
		c.readBytes.Add(4096)
	}
}
