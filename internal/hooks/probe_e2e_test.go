package hooks

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
)

// fakeController implements daemon.Controller with stubbed SearchSymbols
// and no-op everything else. Lives in the hooks package so the e2e test
// can drive it through the real socket without dragging the hooks
// package into internal/daemon (which would create an import cycle).
type fakeController struct {
	hits []daemon.SymbolHit
}

func (f *fakeController) Track(_ context.Context, _ daemon.TrackParams) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (f *fakeController) Untrack(_ context.Context, _ daemon.UntrackParams) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (f *fakeController) Reload(_ context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (f *fakeController) ReloadServers(_ context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (f *fakeController) Status(_ context.Context) (daemon.StatusResponse, error) {
	return daemon.StatusResponse{}, nil
}
func (f *fakeController) Probe(_ context.Context) (daemon.ProbeResponse, error) {
	return daemon.ProbeResponse{Ready: true}, nil
}
func (f *fakeController) Shutdown(_ context.Context) error { return nil }
func (f *fakeController) SearchSymbols(_ context.Context, _ daemon.SearchSymbolsParams) (daemon.SearchSymbolsResult, error) {
	return daemon.SearchSymbolsResult{Hits: f.hits}, nil
}
func (f *fakeController) EnrichChurn(_ context.Context, _ daemon.EnrichChurnParams) (daemon.EnrichChurnResult, error) {
	return daemon.EnrichChurnResult{}, nil
}
func (f *fakeController) EnrichReleases(_ context.Context, _ daemon.EnrichReleasesParams) (daemon.EnrichReleasesResult, error) {
	return daemon.EnrichReleasesResult{}, nil
}
func (f *fakeController) EnrichBlame(_ context.Context, _ daemon.EnrichBlameParams) (daemon.EnrichBlameResult, error) {
	return daemon.EnrichBlameResult{}, nil
}
func (f *fakeController) EnrichCoverage(_ context.Context, _ daemon.EnrichCoverageParams) (daemon.EnrichCoverageResult, error) {
	return daemon.EnrichCoverageResult{}, nil
}
func (f *fakeController) EnrichCochange(_ context.Context, _ daemon.EnrichCochangeParams) (daemon.EnrichCochangeResult, error) {
	return daemon.EnrichCochangeResult{}, nil
}

// startTestDaemon spins up a real daemon on a short-path unix socket and
// points GORTEX_DAEMON_SOCKET at it so daemon.Dial finds it.
func startTestDaemon(t *testing.T, ctrl daemon.Controller) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gx-hook-e2e")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))

	srv := daemon.New(socket, "test-0.0.0", zap.NewNop())
	srv.Controller = ctrl
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	// Wait for the socket to actually accept.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon socket never accepted")
}

func TestProbeViaDaemon_Hit_E2E(t *testing.T) {
	startTestDaemon(t, &fakeController{
		hits: []daemon.SymbolHit{
			{Name: "handleFoo", Kind: "function", FilePath: "x.go", Line: 7},
		},
	})
	hits, err := probeViaDaemon("handleFoo", "", 2*time.Second)
	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "handleFoo" || hits[0].FilePath != "x.go" || hits[0].Line != 7 {
		t.Errorf("unexpected hits: %+v", hits)
	}
}

func TestProbeViaDaemon_NoDaemon_ReturnsUnreachable(t *testing.T) {
	// Point at a path that has no listener.
	dir, err := os.MkdirTemp("/tmp", "gx-hook-empty")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(dir, "missing"))

	_, err = probeViaDaemon("handleFoo", "", 500*time.Millisecond)
	if err != errDaemonUnreachable {
		t.Errorf("expected errDaemonUnreachable, got %v", err)
	}
}
