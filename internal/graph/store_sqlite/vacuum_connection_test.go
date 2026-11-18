package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVacuumDedicatedConnectionKeepsPoolsInMemory(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "quote's store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := t.Context()
	var before string
	writer, err := s.writerDB.Conn(ctx)
	require.NoError(t, err)
	before, err = vacuumTempDirectory(ctx, writer)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	require.NoError(t, s.withVacuumConnection(ctx, func(conn *sql.Conn) error {
		var mode, pooledWriter, pooledReader int
		require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA temp_store`).Scan(&mode))
		require.Equal(t, 1, mode, "only the dedicated connection uses FILE")
		require.NoError(t, s.writerDB.QueryRowContext(ctx, `PRAGMA temp_store`).Scan(&pooledWriter))
		require.NoError(t, s.db.QueryRowContext(ctx, `PRAGMA temp_store`).Scan(&pooledReader))
		require.Equal(t, 2, pooledWriter)
		require.Equal(t, 2, pooledReader)
		directory, err := vacuumTempDirectory(ctx, conn)
		require.NoError(t, err)
		actual, err := filepath.EvalSymlinks(directory)
		require.NoError(t, err)
		want, err := filepath.EvalSymlinks(filepath.Dir(s.Path()))
		require.NoError(t, err)
		require.Equal(t, want, actual, "SQLite may resolve a /tmp alias in its main filename")
		requireNoCloseCheckpoint(t, conn)
		return nil
	}))
	writer, err = s.writerDB.Conn(ctx)
	require.NoError(t, err)
	defer writer.Close()
	after, err := vacuumTempDirectory(ctx, writer)
	require.NoError(t, err)
	require.Equal(t, before, after, "process-global temp directory restored")
}

func TestVacuumDirectoryRestoredAfterCanceledSQL(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	conn, err := s.writerDB.Conn(t.Context())
	require.NoError(t, err)
	before, err := vacuumTempDirectory(t.Context(), conn)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	ctx, cancel := context.WithCancel(t.Context())
	err = s.withVacuumConnection(ctx, func(conn *sql.Conn) error {
		cancel()
		_, err := conn.ExecContext(ctx, `VACUUM`)
		return err
	})
	require.ErrorIs(t, err, context.Canceled)
	conn, err = s.writerDB.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	after, err := vacuumTempDirectory(t.Context(), conn)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestCompactContextCancelsHeldWriterAdmission(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.CompactContext(ctx) }()
	deadline := time.After(5 * time.Second)
	for s.writeMu.waiting() == 0 {
		select {
		case <-deadline:
			t.Fatal("compaction did not reach held write gate")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, ErrMaintenanceBusy)
	case <-time.After(5 * time.Second):
		t.Fatal("compaction ignored cancellation at write gate")
	}
}

func TestVacuumDirectoryGuardCancelsWithoutRunningCallback(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	vacuumTempDirectoryGate.Lock()
	defer vacuumTempDirectoryGate.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.withVacuumConnection(ctx, func(*sql.Conn) error {
			return errors.New("unexpected callback after canceled guard wait")
		})
	}()
	deadline := time.After(5 * time.Second)
	for vacuumTempDirectoryGate.waiting() == 0 {
		select {
		case <-deadline:
			t.Fatal("compaction did not reach directory guard")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("directory guard ignored cancellation")
	}
}

func TestVacuumUnconfirmedDirectoryDoesNotRun(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	called := false
	err = s.withVacuumConnection(t.Context(), func(*sql.Conn) error { called = true; return nil })
	require.ErrorIs(t, err, ErrVacuumTempDirectory)
	require.False(t, called)
	require.NoError(t, s.CompactContext(t.Context()), "memory compatibility path does not open a second database")
	require.Error(t, s.CompactContext(nil)) //nolint:staticcheck // SA1012: Verify explicit nil-context rejection.
}

func TestVacuumTwoStoresKeepDistinctDirectories(t *testing.T) {
	first, err := Open(filepath.Join(t.TempDir(), "first.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := Open(filepath.Join(t.TempDir(), "second.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer, err := first.writerDB.Conn(ctx)
	require.NoError(t, err)
	previous, err := vacuumTempDirectory(ctx, writer)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	checkAndVacuum := func(conn *sql.Conn, path string) error {
		directory, err := vacuumTempDirectory(ctx, conn)
		if err != nil {
			return err
		}
		actual, err := filepath.EvalSymlinks(directory)
		if err != nil {
			return err
		}
		want, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return err
		}
		if actual != want {
			return fmt.Errorf("vacuum directory=%q, want own physical store %q", actual, want)
		}
		_, err = conn.ExecContext(ctx, `VACUUM`)
		return err
	}
	go func() {
		firstDone <- first.withVacuumConnection(ctx, func(conn *sql.Conn) error {
			close(entered)
			<-release
			return checkAndVacuum(conn, first.Path())
		})
	}()
	var firstResult, secondResult error
	var joinFirstOnce, joinSecondOnce sync.Once
	joinFirst := func() { joinFirstOnce.Do(func() { firstResult = <-firstDone }) }
	joinSecond := func() { joinSecondOnce.Do(func() { secondResult = <-secondDone }) }
	t.Cleanup(func() { cancel(); unblock(); joinFirst() })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first actual dedicated connection did not enter")
	}
	go func() {
		secondDone <- second.withVacuumConnection(ctx, func(conn *sql.Conn) error {
			return checkAndVacuum(conn, second.Path())
		})
	}()
	t.Cleanup(func() { cancel(); unblock(); joinSecond() })
	deadline := time.After(5 * time.Second)
	for vacuumTempDirectoryGate.waiting() == 0 {
		select {
		case <-deadline:
			t.Fatal("second actual store did not wait for the first temp-directory owner")
		case <-time.After(time.Millisecond):
		}
	}
	writer, err = first.writerDB.Conn(ctx)
	require.NoError(t, err)
	during, err := vacuumTempDirectory(ctx, writer)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	actual, err := filepath.EvalSymlinks(during)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(filepath.Dir(first.Path()))
	require.NoError(t, err)
	require.Equal(t, want, actual, "second store cannot redirect first store's scratch")
	unblock()
	joinFirst()
	joinSecond()
	require.NoError(t, firstResult)
	require.NoError(t, secondResult)
	writer, err = first.writerDB.Conn(ctx)
	require.NoError(t, err)
	after, err := vacuumTempDirectory(ctx, writer)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.Equal(t, previous, after)
}
