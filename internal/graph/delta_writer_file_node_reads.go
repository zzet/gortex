package graph

import (
	"sync/atomic"
	"time"
)

// FileNodeReadCounter is a graph that counts its per-path file-node reads
// (GetFileNodes, GetFileNodesByPaths, NodesInFilesByKind) and their wall
// time, so a caller can split a step into those reads and everything else.
type FileNodeReadCounter interface {
	FileNodeReadStats() (reads int64, wall time.Duration)
}

// fileNodeReadStats is the delta's running count of its file-node reads.
type fileNodeReadStats struct {
	reads atomic.Int64
	nanos atomic.Int64
}

// noteFileNodeRead records one file-node read begun at started.
func (dw *DeltaWriter) noteFileNodeRead(started time.Time) {
	dw.fileNodeReads.reads.Add(1)
	dw.fileNodeReads.nanos.Add(int64(time.Since(started)))
}

// FileNodeReadStats implements FileNodeReadCounter.
func (dw *DeltaWriter) FileNodeReadStats() (int64, time.Duration) {
	return dw.fileNodeReads.reads.Load(), time.Duration(dw.fileNodeReads.nanos.Load())
}
