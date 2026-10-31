package serverstack

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/telemetry"
	"go.uber.org/zap"
)

func TestTelemetrySenderCancelPrecedesActualStartupFlushJoinAndBackend(t *testing.T) {
	t.Setenv(telemetry.EnvEndpoint, "") // No HTTP or external provider.
	dir := t.TempDir()
	store := telemetry.NewStore(dir)
	entered, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	var enteredOnce, releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	recorder := telemetry.NewRecorderFunc(func() bool {
		if armed.Load() {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
		return true
	}, store)
	recorder.Record("cli_command", "status")
	armed.Store(true)
	srv := &gortexmcp.Server{}
	srv.SetTelemetryRecorder(recorder)
	s := &SharedServer{MCP: srv, logger: zap.NewNop()}
	backend := make(chan struct{})
	s.addCleanup("backend", func() error {
		rollups, err := store.LoadCompleted("9999-12-31")
		if err != nil {
			return err
		}
		if len(rollups) != 1 || rollups[0].Counts["cli_command:status"] != 1 {
			return fmt.Errorf("backend crossed unfinished actual telemetry flush: %+v", rollups)
		}
		close(backend)
		return nil
	})
	s.installTelemetrySender(srv, store, dir, "test") // Same immutable production registration.
	t.Cleanup(func() { unlock(); _ = s.Close() })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual recorder flush did not enter")
	}
	s.BeginBackgroundClose() // Nonjoining, even with a contextless flush held.
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-backend:
		t.Fatal("backend closed before actual startup flush completion")
	default:
	}
	select {
	case <-closed:
		t.Fatal("stack close abandoned telemetry owner")
	default:
	}
	unlock()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("stack did not join the actual telemetry writer")
	}
	select {
	case <-s.telemetryOwner.done:
	default:
		t.Fatal("named cleanup did not join actual producer")
	}
}

func TestTelemetrySenderCloseJoinsTickAndStartsNoLaterFlush(t *testing.T) {
	t.Setenv(telemetry.EnvEndpoint, "")
	store := telemetry.NewStore(t.TempDir())
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	srv := &gortexmcp.Server{}
	srv.SetTelemetryRecorder(telemetry.NewRecorderFunc(func() bool {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}
		return true
	}, store))
	owner := startTelemetrySenderWithInterval(srv, store, t.TempDir(), "test", time.Millisecond)
	t.Cleanup(func() { unlock(); owner.Close() })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual second tick flush did not enter")
	}
	owner.Cancel()
	joined := make(chan struct{})
	go func() { defer close(joined); owner.Close() }()
	select {
	case <-joined:
		t.Fatal("Close returned while admitted tick flush still ran")
	default:
	}
	unlock()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("tick flush owner did not join")
	}
	require.Equal(t, int32(2), calls.Load(), "canceled ticker admitted another flush")
	owner.Close()
}
