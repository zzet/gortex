//go:build windows

package store_sqlite

import (
	"errors"
	"os"
)

// Windows byte-range locks belong to their file handle. Closing this separate
// observation handle therefore leaves SQLite's handles and locks intact. Unlike
// the POSIX implementation, retaining os.Open handles here prevents deletion of
// the -shm after Store.Close: Go opens with FILE_SHARE_READ|FILE_SHARE_WRITE, but
// not FILE_SHARE_DELETE.
func readWALIndexHeader(dbPath string, buf []byte) error {
	f, err := os.Open(dbPath + "-shm")
	if err != nil {
		return err
	}
	// SQLite reserves bytes 120..127 for mandatory Windows byte-range locks.
	// A snapshot may be read while our writer owns one of those locks through
	// another handle. The lock bytes carry no header data; avoid reading them.
	_, readErr := f.ReadAt(buf[:min(len(buf), 120)], 0)
	if len(buf) > 120 {
		clear(buf[120:min(len(buf), 128)])
	}
	if readErr == nil && len(buf) > 128 {
		_, readErr = f.ReadAt(buf[128:], 128)
	}
	return errors.Join(readErr, f.Close())
}
