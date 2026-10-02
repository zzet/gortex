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
	_, readErr := f.ReadAt(buf, 0)
	return errors.Join(readErr, f.Close())
}
