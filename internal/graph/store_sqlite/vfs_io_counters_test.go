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

func TestVFSIOSplitWriterMainWrites(t *testing.T) {
	s, _ := openWALReclaimStore(t)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	if !vfsIOInstalled.Load() {
		t.Skip("vfs io counters not installed")
	}
	seedWALChurnTable(t, s)
	growWAL(t, s, 4)
	before := WriteMark()
	s.writeMu.Lock()
	var busy, frames, copied int
	err := s.writerDB.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &copied)
	s.writeMu.Unlock()
	require.NoError(t, err)
	require.Zero(t, busy)
	d := WriteMark().Since(before)
	require.Positive(t, d.WriterMainWrites)
	require.Positive(t, d.WriterMainWriteBytes)
	require.Positive(t, d.WriterMainWriteTime)
	require.Zero(t, d.OtherMainWrites)
}

func TestWriteSplitOtherWALAndPositionFallback(t *testing.T) {
	before := VFSIOMark{Available: true}
	after := before
	after.WriteBytes[vfsFileWAL][0] = 64
	after.WriteBytes[vfsFileWAL][1] = 128
	require.Equal(t, int64(192), after.Since(before).WALBytes(), "other store writers must remain visible")
	before = VFSIOMark{fallbackWAL: WALWriteMark{Valid: true, MxFrame: 10, PageSize: 4096}}
	after = VFSIOMark{fallbackWAL: WALWriteMark{Valid: true, MxFrame: 12, PageSize: 4096}}
	d := after.Since(before)
	require.Equal(t, int64(2*(4096+24)), d.WALBytes())
	require.False(t, d.WALReset)
	after.fallbackWAL.Salt[0] = 1
	after.fallbackWAL.MxFrame = 1
	d = after.Since(before)
	require.Equal(t, int64(4096+24), d.WALBytes(), "only the new incarnation is countable")
	require.True(t, d.WALReset)
}
