package store_sqlite

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Page I/O timed at the VFS.
//
// The page-cache counters (sqlite3_db_status) say how many pages a connection
// read, not how long the reads took or whether they came from the log or the
// database file. Every pread and pwrite SQLite makes goes through the VFS's
// xRead and xWrite, so the wrappers already installed on the database files'
// io_methods (wal_copy_pause.go) count them: calls and wall time, split by the
// file (the main database or the log) and by the connection (the writer
// connection, or any other). The file kind is recorded when the VFS opens a
// file (a wrapped xOpen sees SQLITE_OPEN_MAIN_DB / SQLITE_OPEN_WAL). The
// writer connection is recognised by its libc TLS, recorded whenever the
// write gate's release hook samples it (writer_cache_counters.go).
//
// Pages served from the memory map do not go through xRead
// (ReaderWaitMark.MappedPages counts those); a page fault in the map is the
// process's major-fault count.

const (
	vfsFileOther = iota
	vfsFileMain
	vfsFileWAL
)

// vfsIOCounters is one (file, connection) cell: calls and nanoseconds.
type vfsIOCounters struct {
	readCalls, readNanos, readBytes    atomic.Int64
	writeCalls, writeNanos, writeBytes atomic.Int64
}

var (
	// vfsIO[file][writer]: file is vfsFileMain or vfsFileWAL; writer is 1
	// for the writer connection.
	vfsIO        [3][2]vfsIOCounters
	vfsFileKinds sync.Map       // pFile (uintptr) → kind
	vfsWriterTLS atomic.Uintptr // the writer connection's TLS
	vfsReadOrig  atomic.Uintptr // the wrapped io_methods' original xRead
	// The log's io_methods table (SQLite opens non-main files with its
	// lockless table) and its original xRead and xWrite.
	vfsWALMethods   atomic.Uintptr
	vfsWALReadOrig  atomic.Uintptr
	vfsWALWriteOrig atomic.Uintptr
	vfsOpenOrig     sync.Map // pVfs → original xOpen
	vfsIOInstalled  atomic.Bool
)

var vfsReadWrapper = func(tls *libc.TLS, pFile, pBuf uintptr, iAmt int32, iOfst int64) int32 {
	fp := vfsReadOrig.Load()
	start := time.Now()
	rc := (*(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&struct{ uintptr }{fp})))(tls, pFile, pBuf, iAmt, iOfst)
	c := vfsCell(tls, pFile)
	c.readCalls.Add(1)
	c.readNanos.Add(int64(time.Since(start)))
	c.readBytes.Add(int64(iAmt))
	return rc
}

var vfsOpenWrapper = func(tls *libc.TLS, pVfs, zName, pFile uintptr, flags int32, pOutFlags uintptr) int32 {
	orig, _ := vfsOpenOrig.Load(pVfs)
	fp, _ := orig.(uintptr)
	rc := (*(*func(*libc.TLS, uintptr, uintptr, uintptr, int32, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{fp})))(tls, pVfs, zName, pFile, flags, pOutFlags)
	if rc == sqlite3.SQLITE_OK {
		kind := vfsFileOther
		switch {
		case flags&sqlite3.SQLITE_OPEN_MAIN_DB != 0:
			kind = vfsFileMain
		case flags&sqlite3.SQLITE_OPEN_WAL != 0:
			kind = vfsFileWAL
		}
		vfsFileKinds.Store(pFile, kind)
	}
	return rc
}

func vfsCell(tls *libc.TLS, pFile uintptr) *vfsIOCounters {
	kind := vfsFileOther
	if v, ok := vfsFileKinds.Load(pFile); ok {
		kind = v.(int)
	}
	writer := 0
	if uintptr(unsafe.Pointer(tls)) == vfsWriterTLS.Load() {
		writer = 1
	}
	return &vfsIO[kind][writer]
}

// noteVFSWrite counts one xWrite (called by the xWrite wrapper).
func noteVFSWrite(tls *libc.TLS, pFile uintptr, n int32, took time.Duration) {
	if !vfsIOInstalled.Load() {
		return
	}
	c := vfsCell(tls, pFile)
	c.writeCalls.Add(1)
	c.writeNanos.Add(int64(took))
	c.writeBytes.Add(int64(n))
}

// installVFSIOCounters wraps xRead of the database files' io_methods table
// (methods, already wrapped for xWrite) and xOpen of every registered VFS.
// Called once, from installWALCopyPause, before the first store connection.
func installVFSIOCounters(tls *libc.TLS, methods, walMethods uintptr) {
	m := ioMethodsAt(methods)
	if m.FxRead == 0 {
		return
	}
	vfsReadOrig.Store(m.FxRead)
	m.FxRead = funcPointer(vfsReadWrapper)
	if walMethods != 0 && walMethods != methods {
		w := ioMethodsAt(walMethods)
		if w.FxRead != 0 && w.FxWrite != 0 {
			vfsWALReadOrig.Store(w.FxRead)
			vfsWALWriteOrig.Store(w.FxWrite)
			w.FxRead = funcPointer(vfsWALReadWrapper)
			w.FxWrite = funcPointer(vfsWALWriteWrapper)
			vfsWALMethods.Store(walMethods)
		}
	}
	open := funcPointer(vfsOpenWrapper)
	for vfs := sqlite3.Xsqlite3_vfs_find(tls, 0); vfs != 0; vfs = vfsAt(vfs).FpNext {
		v := vfsAt(vfs)
		if v.FxOpen == 0 || v.FxOpen == open {
			continue
		}
		vfsOpenOrig.Store(vfs, v.FxOpen)
		v.FxOpen = open
	}
	vfsIOInstalled.Store(true)
	log.Printf("store_sqlite: vfs io counters installed")
}

// VFSIOMark is a sample of the page I/O counters, process-wide.
type VFSIOMark struct {
	// [file][writer]: file 0 other, 1 main database, 2 log; writer 1 is the
	// writer connection.
	ReadCalls, ReadNanos, ReadBytes    [3][2]int64
	WriteCalls, WriteNanos, WriteBytes [3][2]int64
}

func vfsIOMark() VFSIOMark {
	var m VFSIOMark
	for f := 0; f < 3; f++ {
		for w := 0; w < 2; w++ {
			c := &vfsIO[f][w]
			m.ReadCalls[f][w], m.ReadNanos[f][w], m.ReadBytes[f][w] = c.readCalls.Load(), c.readNanos.Load(), c.readBytes.Load()
			m.WriteCalls[f][w], m.WriteNanos[f][w], m.WriteBytes[f][w] = c.writeCalls.Load(), c.writeNanos.Load(), c.writeBytes.Load()
		}
	}
	return m
}

// VFSIOSplit is one lap's page I/O: reads and writes of the main database file
// and of the log, by the writer connection and by every other connection.
type VFSIOSplit struct {
	WriterMainReads, WriterWALReads, OtherMainReads, OtherWALReads int64
	WriterMainReadTime, WriterWALReadTime                          time.Duration
	OtherMainReadTime, OtherWALReadTime                            time.Duration
	WriterWALWrites, OtherMainWrites                               int64
	WriterWALWriteTime, OtherMainWriteTime                         time.Duration
	WriterWALWriteBytes, OtherMainWriteBytes                       int64
}

// Since is the lap between prev and m.
func (m VFSIOMark) Since(prev VFSIOMark) VFSIOSplit {
	d := func(a, b [3][2]int64, f, w int) int64 { return a[f][w] - b[f][w] }
	return VFSIOSplit{
		WriterMainReads:     d(m.ReadCalls, prev.ReadCalls, vfsFileMain, 1),
		WriterWALReads:      d(m.ReadCalls, prev.ReadCalls, vfsFileWAL, 1),
		OtherMainReads:      d(m.ReadCalls, prev.ReadCalls, vfsFileMain, 0),
		OtherWALReads:       d(m.ReadCalls, prev.ReadCalls, vfsFileWAL, 0),
		WriterMainReadTime:  time.Duration(d(m.ReadNanos, prev.ReadNanos, vfsFileMain, 1)),
		WriterWALReadTime:   time.Duration(d(m.ReadNanos, prev.ReadNanos, vfsFileWAL, 1)),
		OtherMainReadTime:   time.Duration(d(m.ReadNanos, prev.ReadNanos, vfsFileMain, 0)),
		OtherWALReadTime:    time.Duration(d(m.ReadNanos, prev.ReadNanos, vfsFileWAL, 0)),
		WriterWALWrites:     d(m.WriteCalls, prev.WriteCalls, vfsFileWAL, 1),
		OtherMainWrites:     d(m.WriteCalls, prev.WriteCalls, vfsFileMain, 0),
		WriterWALWriteTime:  time.Duration(d(m.WriteNanos, prev.WriteNanos, vfsFileWAL, 1)),
		OtherMainWriteTime:  time.Duration(d(m.WriteNanos, prev.WriteNanos, vfsFileMain, 0)),
		WriterWALWriteBytes: d(m.WriteBytes, prev.WriteBytes, vfsFileWAL, 1),
		OtherMainWriteBytes: d(m.WriteBytes, prev.WriteBytes, vfsFileMain, 0),
	}
}

// The log file's wrappers: count, then call the log table's originals.
var vfsWALReadWrapper = func(tls *libc.TLS, pFile, pBuf uintptr, iAmt int32, iOfst int64) int32 {
	fp := vfsWALReadOrig.Load()
	start := time.Now()
	rc := (*(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&struct{ uintptr }{fp})))(tls, pFile, pBuf, iAmt, iOfst)
	c := vfsCell(tls, pFile)
	c.readCalls.Add(1)
	c.readNanos.Add(int64(time.Since(start)))
	c.readBytes.Add(int64(iAmt))
	return rc
}

var vfsWALWriteWrapper = func(tls *libc.TLS, pFile, pBuf uintptr, iAmt int32, iOfst int64) int32 {
	fp := vfsWALWriteOrig.Load()
	start := time.Now()
	rc := (*(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&struct{ uintptr }{fp})))(tls, pFile, pBuf, iAmt, iOfst)
	noteVFSWrite(tls, pFile, iAmt, time.Since(start))
	return rc
}
