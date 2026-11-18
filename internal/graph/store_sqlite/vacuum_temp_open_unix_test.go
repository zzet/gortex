//go:build darwin || linux

package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Observe the real native open, not a directory listing: SQLite unlinks its
// temporary files immediately after open. SQLite's supported VFS system-call
// API exposes the generated filename before that unlink, on Unix. No driver
// struct offsets or production hooks are added. Other OSes retain the portable
// FILE/MEMORY policy tests; this is actual Unix file-placement evidence only.
// _sqlite3RunVacuum copies the main schema cache_size into vacuum_db;
// _sqlite3PagerOpen delays opening a temporary file until its first OsWrite.
// Our real policy is 32MiB, so the fixture must exceed that cache to prove
// actual FILE spill rather than accepting a successful entirely cached copy.
const vacuumTempFixtureRows = 16384 // 64MiB payload at the production cache policy

var vacuumOpenTestMu sync.Mutex
var vacuumOpenOriginal atomic.Uintptr
var vacuumOpenObservation atomic.Pointer[vacuumOpenProbe]

type vacuumOpenProbe struct {
	directory string
	mu        sync.Mutex
	opens     []vacuumObservedOpen
	err       error
	entered   chan struct{}
	release   <-chan struct{}
	once      sync.Once
}

type vacuumObservedOpen struct {
	path, directory      string
	anonymous            bool
	device, inode, links uint64
	mode                 uint32
}

// Inspect the actual successful OS descriptor while SQLite still owns it.
// Never wrap it in os.File: its finalizer must not close SQLite's descriptor.
func inspectVacuumNativeOpen(fd int32, path string, flags int32, wanted string) (opened vacuumObservedOpen, recognized bool, err error) {
	opened.path, opened.anonymous = path, vacuumOpenIsAnonymous(flags)
	if !opened.anonymous && !strings.HasPrefix(filepath.Base(path), "etilqs_") {
		return opened, false, nil
	}
	parent := filepath.Dir(path)
	if opened.anonymous {
		parent = path // O_TMPFILE opens an anonymous regular inode in this directory.
	}
	opened.directory, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return opened, true, err
	}
	if opened.directory != wanted {
		return opened, true, fmt.Errorf("temporary open directory=%q, want %q", opened.directory, wanted)
	}
	var actual, directory unix.Stat_t
	if err = unix.Fstat(int(fd), &actual); err != nil {
		return opened, true, err
	}
	opened.device, opened.inode, opened.links, opened.mode = uint64(actual.Dev), uint64(actual.Ino), uint64(actual.Nlink), uint32(actual.Mode)
	if actual.Mode&unix.S_IFMT != unix.S_IFREG {
		return opened, true, fmt.Errorf("temporary descriptor is not a regular file: %s", path)
	}
	if err = unix.Stat(wanted, &directory); err != nil {
		return opened, true, err
	}
	if uint64(actual.Dev) != uint64(directory.Dev) {
		return opened, true, errors.New("temporary descriptor is on a different device from the store directory")
	}
	if opened.anonymous {
		if actual.Nlink != 0 {
			return opened, true, errors.New("anonymous temporary descriptor has a linked name")
		}
	} else {
		var named unix.Stat_t
		if err = unix.Stat(path, &named); err != nil {
			return opened, true, err
		}
		if actual.Dev != named.Dev || actual.Ino != named.Ino {
			return opened, true, errors.New("named temporary path does not match the actual descriptor")
		}
	}
	return opened, true, nil
}

var observeVacuumNativeOpen = func(tls *libc.TLS, name uintptr, flags, mode int32) int32 {
	original := vacuumOpenOriginal.Load()
	fd := (*(*func(*libc.TLS, uintptr, int32, int32) int32)(unsafe.Pointer(&struct{ uintptr }{original})))(tls, name, flags, mode)
	probe := vacuumOpenObservation.Load()
	if probe == nil || fd < 0 {
		return fd
	}
	opened, recognized, err := inspectVacuumNativeOpen(fd, libc.GoString(name), flags, probe.directory)
	if !recognized {
		return fd
	}
	probe.mu.Lock()
	probe.opens = append(probe.opens, opened)
	probe.err = errors.Join(probe.err, err)
	probe.mu.Unlock()
	if err == nil && probe.entered != nil {
		probe.once.Do(func() { close(probe.entered); <-probe.release })
	}
	return fd
}

func TestVacuumPhysicallyOpensTempOnStoreVolume(t *testing.T) {
	vacuumOpenTestMu.Lock()
	defer vacuumOpenTestMu.Unlock()
	s, err := Open(filepath.Join(t.TempDir(), "physical.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	_, err = s.writerDB.Exec(fmt.Sprintf(`CREATE TABLE vacuum_fixture (id INTEGER PRIMARY KEY, payload BLOB);
		WITH RECURSIVE ids(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM ids WHERE n<%d)
		INSERT INTO vacuum_fixture SELECT n, randomblob(4096) FROM ids`, vacuumTempFixtureRows))
	require.NoError(t, err)
	directory, err := filepath.EvalSymlinks(filepath.Dir(s.Path()))
	require.NoError(t, err)
	probe := &vacuumOpenProbe{directory: directory}
	require.NoError(t, withVacuumOpenObservation(probe, func() error { return s.CompactContext(t.Context()) }))
	probe.mu.Lock()
	defer probe.mu.Unlock()
	require.NoError(t, probe.err)
	require.NotEmpty(t, probe.opens, "a successful pragma alone is not physical placement proof")
	want, err := filepath.EvalSymlinks(filepath.Dir(s.Path()))
	require.NoError(t, err)
	var parent unix.Stat_t
	require.NoError(t, unix.Stat(want, &parent))
	for _, opened := range probe.opens {
		require.Equal(t, want, opened.directory)
		require.Equal(t, uint64(parent.Dev), opened.device)
		require.Equal(t, uint32(unix.S_IFREG), opened.mode&unix.S_IFMT)
		if opened.anonymous {
			require.Zero(t, opened.links)
		} else {
			directory, err := filepath.EvalSymlinks(filepath.Dir(opened.path))
			require.NoError(t, err)
			require.Equal(t, want, directory)
		}
		t.Logf("VACUUM actual native temp open: path=%s anonymous=%v device=%d inode=%d links=%d", opened.path, opened.anonymous, opened.device, opened.inode, opened.links)
	}
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM vacuum_fixture`).Scan(&count))
	require.Equal(t, vacuumTempFixtureRows, count)
}

// SQLite's native VFS open signature is documented in the pinned generated
// _robust_open: (TLS, pathname, flags, mode). Restore its exact original
// pointer; no sandbox/environment/filename substitution is performed.
func withVacuumOpenObservation(probe *vacuumOpenProbe, run func() error) (result error) {
	tls := libc.NewTLS()
	defer tls.Close()
	vfs := sqlite3.Xsqlite3_vfs_find(tls, 0)
	if vfs == 0 || vfsAt(vfs).FxGetSystemCall == 0 || vfsAt(vfs).FxSetSystemCall == 0 {
		return errors.New("vacuum physical test: native VFS system-call API unavailable")
	}
	name, err := libc.CString("open")
	if err != nil {
		return err
	}
	defer libc.Xfree(tls, name)
	get, set := vfsAt(vfs).FxGetSystemCall, vfsAt(vfs).FxSetSystemCall
	original := (*(*func(*libc.TLS, uintptr, uintptr) uintptr)(unsafe.Pointer(&struct{ uintptr }{get})))(tls, vfs, name)
	if original == 0 {
		return errors.New("vacuum physical test: native open unavailable")
	}
	setOpen := func(pointer uintptr) int32 {
		return (*(*func(*libc.TLS, uintptr, uintptr, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{set})))(tls, vfs, name, pointer)
	}
	vacuumOpenOriginal.Store(original)
	vacuumOpenObservation.Store(probe)
	defer vacuumOpenObservation.Store(nil)
	if rc := setOpen(funcPointer(observeVacuumNativeOpen)); rc != sqlite3.SQLITE_OK {
		return fmt.Errorf("vacuum physical test: set native open rc=%d", rc)
	}
	defer func() {
		if rc := setOpen(original); rc != sqlite3.SQLITE_OK {
			result = errors.Join(result, fmt.Errorf("restore native open rc=%d", rc))
		}
	}()
	return run()
}

func TestCompactContextInterruptsInFlightVacuumAndRestoresDirectory(t *testing.T) {
	vacuumOpenTestMu.Lock()
	defer vacuumOpenTestMu.Unlock()
	path := filepath.Join(t.TempDir(), "cancel.sqlite")
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	_, err = s.writerDB.Exec(fmt.Sprintf(`CREATE TABLE vacuum_fixture (id INTEGER PRIMARY KEY, payload BLOB);
		WITH RECURSIVE ids(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM ids WHERE n<%d)
		INSERT INTO vacuum_fixture SELECT n, randomblob(4096) FROM ids`, vacuumTempFixtureRows))
	require.NoError(t, err)
	require.NoError(t, s.CheckpointWAL())
	var firstPayload string
	require.NoError(t, s.db.QueryRow(`SELECT hex(payload) FROM vacuum_fixture WHERE id=1`).Scan(&firstPayload))
	writer, err := s.writerDB.Conn(t.Context())
	require.NoError(t, err)
	previous, err := vacuumTempDirectory(t.Context(), writer)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	directory, err := filepath.EvalSymlinks(filepath.Dir(path))
	require.NoError(t, err)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defer unblock()
	probe := &vacuumOpenProbe{directory: directory, entered: make(chan struct{}), release: release}
	done := make(chan error, 1)
	go func() { done <- withVacuumOpenObservation(probe, func() error { return s.CompactContext(ctx) }) }()
	// Cleanup joins the operation before store.Close, even on an assertion
	// failure. This barrier is the actual successful native temp-file open
	// inside VACUUM; no cancellation sleep or canceled-before-Exec shortcut.
	var result error
	var joinOnce sync.Once
	join := func() { joinOnce.Do(func() { cancel(); unblock(); result = <-done }) }
	t.Cleanup(join)
	select {
	case <-probe.entered:
	case early := <-done:
		joinOnce.Do(func() { cancel(); unblock(); result = early })
		probe.mu.Lock()
		observed, observerErr := append([]vacuumObservedOpen(nil), probe.opens...), probe.err
		probe.mu.Unlock()
		t.Fatalf("VACUUM ended before a verified physical temporary open: result=%v observer=%v opens=%+v", early, observerErr, observed)
	case <-time.After(10 * time.Second):
		t.Fatal("VACUUM never reached its physical temporary open")
	}
	join()
	require.ErrorIs(t, result, context.Canceled)
	probe.mu.Lock()
	require.NoError(t, probe.err)
	require.NotEmpty(t, probe.opens)
	probe.mu.Unlock()
	writer, err = s.writerDB.Conn(t.Context())
	require.NoError(t, err)
	after, err := vacuumTempDirectory(t.Context(), writer)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.Equal(t, previous, after)
	require.NoError(t, s.Close())
	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	var check, payload string
	var count, bytes int
	require.NoError(t, reopened.db.QueryRow(`PRAGMA quick_check`).Scan(&check))
	require.Equal(t, "ok", check)
	require.NoError(t, reopened.db.QueryRow(`SELECT count(*), sum(length(payload)) FROM vacuum_fixture`).Scan(&count, &bytes))
	require.Equal(t, vacuumTempFixtureRows, count)
	require.Equal(t, vacuumTempFixtureRows*4096, bytes)
	require.NoError(t, reopened.db.QueryRow(`SELECT hex(payload) FROM vacuum_fixture WHERE id=1`).Scan(&payload))
	require.Equal(t, firstPayload, payload, "canceled VACUUM retained durable survivor bytes")
}

func TestVacuumPhysicalProbeRequiresMatchingRegularDescriptorAndDirectory(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	path := filepath.Join(directory, "etilqs_probe")
	file, err := os.Create(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	observed, recognized, err := inspectVacuumNativeOpen(int32(file.Fd()), path, 0, directory)
	require.NoError(t, err)
	require.True(t, recognized)
	require.False(t, observed.anonymous)
	require.Equal(t, directory, observed.directory)
	var actual unix.Stat_t
	require.NoError(t, unix.Fstat(int(file.Fd()), &actual))
	require.Equal(t, uint64(actual.Dev), observed.device)
	require.Equal(t, uint64(actual.Ino), observed.inode)
	wrong, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	_, recognized, err = inspectVacuumNativeOpen(int32(file.Fd()), path, 0, wrong)
	require.True(t, recognized)
	require.ErrorContains(t, err, "temporary open directory")
	parent, err := os.Open(directory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, parent.Close()) })
	_, recognized, err = inspectVacuumNativeOpen(int32(parent.Fd()), path, 0, directory)
	require.True(t, recognized)
	require.ErrorContains(t, err, "not a regular file")
	_, recognized, err = inspectVacuumNativeOpen(-1, path, 0, directory)
	require.True(t, recognized)
	require.Error(t, err, "invalid actual descriptor cannot become placement evidence")
	_, recognized, err = inspectVacuumNativeOpen(int32(file.Fd()), filepath.Join(directory, "ordinary.sqlite"), 0, directory)
	require.NoError(t, err)
	require.False(t, recognized, "an unrelated regular open is not VACUUM temporary evidence")
}
