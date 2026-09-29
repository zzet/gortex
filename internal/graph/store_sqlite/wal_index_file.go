package store_sqlite

import (
	"os"
	"sync"
)

// Reading the wal-index (-shm) header from Go without releasing SQLite's
// locks.
//
// SQLite coordinates WAL access between processes with POSIX fcntl record
// locks on the -shm file: the DMS byte every open connection holds shared,
// the read-mark bytes of open read transactions, the writer and checkpointer
// bytes. POSIX releases EVERY fcntl lock a process holds on a file the moment
// the process closes ANY descriptor of that file. Opening the -shm with
// os.Open to read its header and closing it again therefore dropped all of the
// daemon's locks at once (verified from a second process with F_GETLK). The
// daemon kept running as if it held them; the next process to open the store
// found the DMS byte free, took it exclusively, and truncated the -shm to 3
// bytes to re-initialise it — under the hash-table regions the daemon had
// mapped. A daemon read that walked those regions before the other process's
// recovery re-extended the file faulted with SIGBUS in walFindFrame.
//
// So the header is read through one descriptor per -shm inode that this
// process keeps open for as long as that inode is the store's -shm. A
// descriptor is closed only once the path names a different inode (or none):
// SQLite no longer uses the old inode then, so the close cannot release a live
// lock.

type walIndexFile struct {
	f    *os.File
	info os.FileInfo
}

var walIndexFiles = struct {
	mu     sync.Mutex
	byPath map[string]*walIndexFile
}{byPath: map[string]*walIndexFile{}}

// readWALIndexHeader reads len(buf) bytes from the start of dbPath's -shm
// into buf. It never closes a descriptor of the live -shm inode.
func readWALIndexHeader(dbPath string, buf []byte) error {
	path := dbPath + "-shm"
	// Stat opens nothing, so it cannot release a lock.
	current, err := os.Stat(path)
	if err != nil {
		return err
	}
	walIndexFiles.mu.Lock()
	defer walIndexFiles.mu.Unlock()
	entry := walIndexFiles.byPath[path]
	if entry != nil && !os.SameFile(entry.info, current) {
		// The path names a new -shm: the old inode is unlinked and unused
		// by SQLite, so its descriptor can go.
		_ = entry.f.Close()
		delete(walIndexFiles.byPath, path)
		entry = nil
	}
	if entry == nil {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			// Deliberately not closed: it may be the live inode.
			return err
		}
		entry = &walIndexFile{f: f, info: info}
		sweepStaleWALIndexFilesLocked()
		walIndexFiles.byPath[path] = entry
	}
	_, err = entry.f.ReadAt(buf, 0)
	return err
}

// sweepStaleWALIndexFilesLocked closes the descriptors whose path no longer
// names their inode (a store closed and removed, a -shm recreated), so a
// process that opens many stores (tests) keeps one descriptor per live -shm.
func sweepStaleWALIndexFilesLocked() {
	for path, entry := range walIndexFiles.byPath {
		current, err := os.Stat(path)
		if err == nil && os.SameFile(entry.info, current) {
			continue
		}
		_ = entry.f.Close()
		delete(walIndexFiles.byPath, path)
	}
}
