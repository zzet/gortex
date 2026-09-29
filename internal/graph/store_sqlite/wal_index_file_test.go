//go:build unix

package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// sqliteShmDMSByte is the -shm byte every open SQLite connection of a process
// holds a shared fcntl lock on (UNIX_SHM_DMS); sqliteShmReadMark1 is the lock
// byte of WAL read mark 1, held by a read transaction that uses the log.
const (
	sqliteShmDMSByte   = 128
	sqliteShmReadMark1 = 124
)

// TestHelperShmLockProbe is not a test: run as a separate process (the
// parent sets GORTEX_TEST_SHM_PROBE) it reports which of the probed -shm
// bytes another process holds a lock on. fcntl locks are per process, so only
// a second process can see them.
func TestHelperShmLockProbe(t *testing.T) {
	path := os.Getenv("GORTEX_TEST_SHM_PROBE")
	if path == "" {
		t.Skip("helper process only")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	for _, off := range []int64{sqliteShmDMSByte, sqliteShmReadMark1} {
		lk := syscall.Flock_t{Type: syscall.F_WRLCK, Start: off, Len: 1}
		if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
			fmt.Println("getlk:", err)
			os.Exit(1)
		}
		state := "free"
		if lk.Type != syscall.F_UNLCK {
			state = "held"
		}
		fmt.Printf("byte%d=%s\n", off, state)
	}
	os.Exit(0)
}

func probeShmLocks(t *testing.T, shm string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperShmLockProbe$")
	cmd.Env = append(os.Environ(), "GORTEX_TEST_SHM_PROBE="+shm)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

// Reading the wal-index header from Go (the reclaim's pre-check, the close
// estimate, the per-edit WAL write mark) must leave this process's SQLite
// locks on the -shm in place, as another process sees them. A close of any
// descriptor of the -shm releases them all; another process opening the store
// would then truncate the -shm under this process's mapped hash tables
// (SIGBUS in walFindFrame).
func TestWALIndexHeaderReadsKeepTheProcessShmLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks.sqlite")
	s, err := openPristine(t, path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	_, err = s.writerDB.Exec(`CREATE TABLE IF NOT EXISTS lock_probe(x)`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`INSERT INTO lock_probe VALUES (1)`)
	require.NoError(t, err)
	// A read transaction on the log: holds read mark 1.
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var n int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM lock_probe`).Scan(&n))

	shm := path + "-shm"
	before := probeShmLocks(t, shm)
	require.Contains(t, before, "byte128=held", "precondition: the DMS lock is held")
	require.Contains(t, before, "byte124=held", "precondition: the read mark is held")

	for i := 0; i < 3; i++ {
		_ = readWALWriteMark(path)
		_, _ = readWALIndexSnapshot(path)
		_ = s.WALWriteMark()
		_, _ = s.CloseCheckpointEstimate()
	}
	mark := readWALWriteMark(path)
	require.True(t, mark.Valid)
	require.Positive(t, mark.MxFrame)

	after := probeShmLocks(t, shm)
	t.Logf("locks another process sees: before %q, after the header reads %q", before, after)
	require.Equal(t, before, after, "reading the -shm header released this process's SQLite locks")
}

// A -shm replaced at the same path (a store removed and recreated) is read
// fresh; the stale descriptor is dropped rather than read.
func TestWALIndexHeaderFollowsARecreatedShm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recreated.sqlite")
	write := func(mx uint32) {
		var hdr [48]byte
		hdr[16] = byte(mx)
		require.NoError(t, os.WriteFile(path+"-shm.tmp", hdr[:], 0o644))
		require.NoError(t, os.Rename(path+"-shm.tmp", path+"-shm"))
	}
	write(7)
	require.Equal(t, uint32(7), readWALWriteMark(path).MxFrame)
	write(9)
	require.Equal(t, uint32(9), readWALWriteMark(path).MxFrame)
	require.NoError(t, os.Remove(path+"-shm"))
	require.False(t, readWALWriteMark(path).Valid)
}
