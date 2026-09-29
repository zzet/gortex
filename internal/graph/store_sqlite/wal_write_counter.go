package store_sqlite

import "encoding/binary"

// WALWriteMark is one sample of the log position.
type WALWriteMark struct {
	// Valid is false when the store has no readable wal-index (an in-memory
	// database, a closed store, a store not in WAL mode yet).
	Valid bool
	// MxFrame is the number of valid frames in the log.
	MxFrame uint32
	// Salt identifies the log incarnation; a reset changes it.
	Salt [8]byte
	// PageSize is the database page size recorded in the wal-index header.
	PageSize uint32
}

// readWALWriteMark reads the first copy of the wal-index header of dbPath:
// szPage (u16, 1 meaning 65536) at offset 14, mxFrame (u32) at 16 and the
// salt pair (copied verbatim from the WAL header) at 32, in the byte order the
// header was written in by this machine.
func readWALWriteMark(dbPath string) WALWriteMark {
	// Through the process's kept descriptor: closing a descriptor of the -shm
	// would release this process's SQLite locks on it (wal_index_file.go).
	var hdr [48]byte
	if err := readWALIndexHeader(dbPath, hdr[:]); err != nil {
		return WALWriteMark{}
	}
	mark := WALWriteMark{Valid: true, MxFrame: binary.NativeEndian.Uint32(hdr[16:20])}
	copy(mark.Salt[:], hdr[32:40])
	switch size := binary.NativeEndian.Uint16(hdr[14:16]); size {
	case 0:
		mark.PageSize = 0
	case 1:
		mark.PageSize = 65536
	default:
		mark.PageSize = uint32(size)
	}
	return mark
}
