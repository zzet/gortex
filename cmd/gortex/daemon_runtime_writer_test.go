package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/testenv"
)

func TestStartupReporterCancelDoesNotJoinPublicationMutexAndRefusesLateWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	t.Setenv("GORTEX_DAEMON_STATEFILE", path)
	r := newDaemonStartupReporter(zap.NewNop())
	r.mu.Lock()
	var once sync.Once
	unlock := func() { once.Do(r.mu.Unlock) }
	t.Cleanup(func() { unlock(); r.Stop() })
	canceled := make(chan struct{})
	go func() { defer close(canceled); r.Cancel() }()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel joined the held publication mutex")
	}
	unlock()
	r.Stop()
	require.NoError(t, os.Remove(path))
	r.Serving("late.sqlite")
	r.Fail(errors.New("late startup error"))
	r.ObserveMigration(store_sqlite.MigrationProgress{Phase: store_sqlite.MigrationStarted, Version: 1})
	_, err := os.Lstat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "a canceled reporter recreated removed runtime")
}

func TestStartupReporterStopJoinsActuallyAdmittedServingPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	t.Setenv("GORTEX_DAEMON_STATEFILE", path)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	r := newDaemonStartupReporterWithWriter(zap.NewNop(), func(state daemon.RuntimeState) error {
		if state.StartupPhase == daemon.StartupServing {
			close(entered)
			<-release
		}
		return daemon.WriteRuntimeState(state)
	})
	publication := make(chan struct{})
	go func() { defer close(publication); r.Serving("admitted.sqlite") }()
	t.Cleanup(func() { unlock(); r.Stop(); <-publication })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("serving write never entered")
	}
	r.Cancel()
	stopped := make(chan struct{})
	go func() { defer close(stopped); r.Stop() }()
	select {
	case <-stopped:
		t.Fatal("stop dropped an actually admitted runtime writer")
	default:
	}
	unlock()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("completed physical publication was not joined")
	}
	<-publication
	state, ok := daemon.ReadRuntimeState()
	require.True(t, ok)
	require.Equal(t, daemon.StartupServing, state.StartupPhase, "admitted physical write did not finish")
	require.NoError(t, os.Remove(path))
	r.Serving("after-stop.sqlite")
	_, err := os.Lstat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRuntimeShutdownJoinsActualStartupWriterBeforeCleanupReceipt(t *testing.T) {
	dir := testenv.ShortTempDir(t)
	path := filepath.Join(dir, "state.json")
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "pid"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", path)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	var written atomic.Bool
	r := newDaemonStartupReporterWithWriter(zap.NewNop(), func(state daemon.RuntimeState) error {
		if state.StartupPhase == daemon.StartupServing {
			close(entered)
			<-release
			if err := daemon.WriteRuntimeState(state); err != nil {
				return err
			}
			written.Store(true)
			return nil
		}
		return daemon.WriteRuntimeState(state)
	})
	t.Cleanup(func() { unlock(); r.Stop() })
	s := daemon.New(filepath.Join(dir, "sock"), "runtime-fixture", zap.NewNop())
	s.Controller = &realController{}
	s.RuntimeStateFile = path
	s.CancelShutdownOwners = r.Cancel
	s.JoinRuntimeWriter = r.Stop
	observed := make(chan error, 1)
	s.OnShutdownComplete = func(err error) {
		if !written.Load() {
			err = errors.Join(err, errors.New("runtime completion preceded admitted physical writer"))
		}
		observed <- err
	}
	t.Cleanup(func() { unlock(); _ = s.Shutdown() })
	require.NoError(t, s.Listen())
	served := make(chan error, 1)
	go func() { served <- s.Serve() }()
	publication := make(chan struct{})
	go func() { defer close(publication); r.Serving(filepath.Join(dir, "fixture.sqlite")) }()
	t.Cleanup(func() { unlock(); r.Stop(); _ = s.Shutdown(); <-publication; <-served })
	// A real request proves the accept loop was admitted before teardown.
	c, err := daemon.DialToWithDeadline(s.SocketPath, daemon.Handshake{Mode: daemon.ModeControl}, time.Now().Add(3*time.Second))
	require.NoError(t, err)
	probe, err := c.ControlWithTimeout(daemon.ControlProbe, nil, time.Second)
	require.NoError(t, err)
	require.True(t, probe.OK)
	require.NoError(t, c.Close())
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("owned serving publication never entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Shutdown() }()
	require.Eventually(t, r.closing.Load, 3*time.Second, time.Millisecond)
	select {
	case <-observed:
		t.Fatal("runtime receipt arrived before physical publication was released")
	default:
	}
	unlock()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("actual runtime cleanup did not complete")
	}
	require.NoError(t, <-observed)
	_, err = os.Lstat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	r.Serving("late.sqlite")
	_, err = os.Lstat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
