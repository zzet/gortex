package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// osStat is an alias so fileExists() reads linearly without import rearrangement.
var osStat = os.Stat

// fakeController is a Controller stub that records calls and returns
// canned responses — lets the daemon lifecycle tests run without wiring
// in the real MultiIndexer.
type fakeController struct {
	mu                 sync.Mutex
	trackCalls         []TrackParams
	untrackCalls       []UntrackParams
	reloadCalls        int
	reloadServersCalls int
	statusCalls        int
	probeCalls         int
	shutdownCalls      int
	shutdownErr        error
	searchCalls        []SearchSymbolsParams
	searchHits         []SymbolHit
	searchErr          error

	enrichChurnCalls    []EnrichChurnParams
	enrichReleasesCalls []EnrichReleasesParams
	enrichBlameCalls    []EnrichBlameParams
	enrichCoverageCalls []EnrichCoverageParams
	enrichCochangeCalls []EnrichCochangeParams
}

func (f *fakeController) Track(_ context.Context, p TrackParams) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trackCalls = append(f.trackCalls, p)
	return json.RawMessage(fmt.Sprintf(`{"tracked":%q}`, p.Path)), nil
}

func (f *fakeController) Untrack(_ context.Context, p UntrackParams) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.untrackCalls = append(f.untrackCalls, p)
	return json.RawMessage(`{"untracked":true}`), nil
}

func (f *fakeController) Reload(_ context.Context) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadCalls++
	return json.RawMessage(`{"reloaded":true}`), nil
}

func (f *fakeController) ReloadServers(_ context.Context) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadServersCalls++
	return json.RawMessage(`{"servers":0,"router_wired":false}`), nil
}

func (f *fakeController) Status(_ context.Context) (StatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	return StatusResponse{
		TrackedRepos: []TrackedRepoStatus{
			{Prefix: "myrepo", Path: "/tmp/myrepo", Files: 42, Nodes: 100, Edges: 200},
		},
	}, nil
}

func (f *fakeController) Probe(_ context.Context) (ProbeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCalls++
	return ProbeResponse{
		Ready: true,
		TrackedRepos: []ProbeRepo{
			{Prefix: "myrepo", Path: "/tmp/myrepo"},
		},
	}, nil
}

func (f *fakeController) Shutdown(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdownCalls++
	return f.shutdownErr
}

func (f *fakeController) SearchSymbols(_ context.Context, p SearchSymbolsParams) (SearchSymbolsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls = append(f.searchCalls, p)
	if f.searchErr != nil {
		return SearchSymbolsResult{}, f.searchErr
	}
	return SearchSymbolsResult{Hits: f.searchHits}, nil
}

func (f *fakeController) EnrichChurn(_ context.Context, p EnrichChurnParams) (EnrichChurnResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrichChurnCalls = append(f.enrichChurnCalls, p)
	return EnrichChurnResult{Files: 1, Symbols: 2, Branch: p.Branch}, nil
}

func (f *fakeController) EnrichReleases(_ context.Context, p EnrichReleasesParams) (EnrichReleasesResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrichReleasesCalls = append(f.enrichReleasesCalls, p)
	return EnrichReleasesResult{Files: 3, Branch: p.Branch}, nil
}

func (f *fakeController) EnrichBlame(_ context.Context, p EnrichBlameParams) (EnrichBlameResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrichBlameCalls = append(f.enrichBlameCalls, p)
	return EnrichBlameResult{Nodes: 5}, nil
}

func (f *fakeController) EnrichCoverage(_ context.Context, p EnrichCoverageParams) (EnrichCoverageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrichCoverageCalls = append(f.enrichCoverageCalls, p)
	return EnrichCoverageResult{Symbols: 7, Segments: len(p.Segments)}, nil
}

func (f *fakeController) EnrichCochange(_ context.Context, p EnrichCochangeParams) (EnrichCochangeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrichCochangeCalls = append(f.enrichCochangeCalls, p)
	return EnrichCochangeResult{Edges: 11}, nil
}

// newDaemon spins up a Server on a short socket path + Fake controller.
// macOS limits Unix socket paths to ~104 chars (sizeof(sun_path)), and
// Go's t.TempDir() path can exceed that for long test names, so we mint
// our own short directory under /tmp/gx-<random>.
func newDaemon(t *testing.T, ctrl Controller) (*Server, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gx")
	require.NoError(t, err, "short tmp dir for unix socket")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))

	srv := New(socket, "test-0.0.0", zap.NewNop())
	srv.Controller = ctrl
	require.NoError(t, srv.Listen())

	go func() { _ = srv.Serve() }()

	// Wait until the socket is actually accepting connections.
	require.Eventually(t, func() bool {
		return IsRunningAt(socket)
	}, 2*time.Second, 10*time.Millisecond, "daemon socket never came up")

	t.Cleanup(func() { _ = srv.Shutdown() })
	return srv, socket
}

func TestDaemon_ControlStatus(t *testing.T) {
	ctrl := &fakeController{}
	_, socket := newDaemon(t, ctrl)

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	require.NotEmpty(t, c.Ack.SessionID, "ack must carry session id")
	require.Equal(t, "test-0.0.0", c.Ack.DaemonVersion)

	resp, err := c.Control(ControlStatus, nil)
	require.NoError(t, err)
	require.True(t, resp.OK, "status: %+v", resp)

	var st StatusResponse
	require.NoError(t, json.Unmarshal(resp.Result, &st))
	assert.Equal(t, "test-0.0.0", st.Version)
	assert.NotZero(t, st.PID)
	assert.Equal(t, socket, st.SocketPath)
	require.Len(t, st.TrackedRepos, 1)
	assert.Equal(t, "myrepo", st.TrackedRepos[0].Prefix)
}

func TestDaemon_ControlTrackUntrack(t *testing.T) {
	ctrl := &fakeController{}
	_, socket := newDaemon(t, ctrl)

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	trackResp, err := c.Control(ControlTrack, TrackParams{Path: "/tmp/myapp", Name: "myapp"})
	require.NoError(t, err)
	require.True(t, trackResp.OK)
	assert.Contains(t, string(trackResp.Result), "/tmp/myapp")

	untrackResp, err := c.Control(ControlUntrack, UntrackParams{PathOrPrefix: "myapp"})
	require.NoError(t, err)
	require.True(t, untrackResp.OK)

	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	require.Len(t, ctrl.trackCalls, 1)
	assert.Equal(t, "/tmp/myapp", ctrl.trackCalls[0].Path)
	require.Len(t, ctrl.untrackCalls, 1)
	assert.Equal(t, "myapp", ctrl.untrackCalls[0].PathOrPrefix)
}

// TestDaemon_ControlEnrichDispatch exercises the control dispatch for
// every enrich verb — confirming each routes to the matching Controller
// method, round-trips its Params, and decodes the typed Result. This is
// the contract the `gortex enrich` CLI relies on when it forwards to a
// running daemon.
func TestDaemon_ControlEnrichDispatch(t *testing.T) {
	ctrl := &fakeController{}
	_, socket := newDaemon(t, ctrl)

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	// churn
	churnResp, err := c.Control(ControlEnrichChurn, EnrichChurnParams{Path: "/r", Branch: "main"})
	require.NoError(t, err)
	require.True(t, churnResp.OK, "churn: %+v", churnResp)
	var churnOut EnrichChurnResult
	require.NoError(t, json.Unmarshal(churnResp.Result, &churnOut))
	assert.Equal(t, 1, churnOut.Files)
	assert.Equal(t, 2, churnOut.Symbols)
	assert.Equal(t, "main", churnOut.Branch)

	// releases
	relResp, err := c.Control(ControlEnrichReleases, EnrichReleasesParams{Path: "/r", Branch: "main"})
	require.NoError(t, err)
	require.True(t, relResp.OK, "releases: %+v", relResp)
	var relOut EnrichReleasesResult
	require.NoError(t, json.Unmarshal(relResp.Result, &relOut))
	assert.Equal(t, 3, relOut.Files)

	// blame
	blameResp, err := c.Control(ControlEnrichBlame, EnrichBlameParams{Path: "/r"})
	require.NoError(t, err)
	require.True(t, blameResp.OK, "blame: %+v", blameResp)
	var blameOut EnrichBlameResult
	require.NoError(t, json.Unmarshal(blameResp.Result, &blameOut))
	assert.Equal(t, 5, blameOut.Nodes)

	// coverage
	covResp, err := c.Control(ControlEnrichCoverage, EnrichCoverageParams{
		Path: "/r",
		Segments: []EnrichCoverageSegment{
			{File: "a.go", StartLine: 1, EndLine: 3, NumStmt: 2, Count: 1},
			{File: "a.go", StartLine: 4, EndLine: 6, NumStmt: 1, Count: 0},
		},
	})
	require.NoError(t, err)
	require.True(t, covResp.OK, "coverage: %+v", covResp)
	var covOut EnrichCoverageResult
	require.NoError(t, json.Unmarshal(covResp.Result, &covOut))
	assert.Equal(t, 7, covOut.Symbols)
	assert.Equal(t, 2, covOut.Segments)

	// cochange
	coResp, err := c.Control(ControlEnrichCochange, EnrichCochangeParams{Path: "/r"})
	require.NoError(t, err)
	require.True(t, coResp.OK, "cochange: %+v", coResp)
	var coOut EnrichCochangeResult
	require.NoError(t, json.Unmarshal(coResp.Result, &coOut))
	assert.Equal(t, 11, coOut.Edges)

	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	require.Len(t, ctrl.enrichChurnCalls, 1)
	assert.Equal(t, "/r", ctrl.enrichChurnCalls[0].Path)
	require.Len(t, ctrl.enrichReleasesCalls, 1)
	require.Len(t, ctrl.enrichBlameCalls, 1)
	assert.Equal(t, "/r", ctrl.enrichBlameCalls[0].Path)
	require.Len(t, ctrl.enrichCoverageCalls, 1)
	assert.Len(t, ctrl.enrichCoverageCalls[0].Segments, 2)
	require.Len(t, ctrl.enrichCochangeCalls, 1)
}

func TestDaemon_ProtocolMismatchRejected(t *testing.T) {
	_, socket := newDaemon(t, &fakeController{})
	// Bump the version so the daemon rejects us.
	_, err := DialTo(socket, Handshake{Version: ProtocolVersion + 1, Mode: ModeControl})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "protocol")
}

func TestDial_NoDaemon_ReturnsErrUnavailable(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gx")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(dir) }()
	missing := filepath.Join(dir, "missing")
	_, err = DialTo(missing, Handshake{Mode: ModeControl})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDaemonUnavailable),
		"Dial against missing socket must wrap ErrDaemonUnavailable; got %v", err)
}

func TestIsRunningAt(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gx")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(dir) }()
	missing := filepath.Join(dir, "nope")
	assert.False(t, IsRunningAt(missing))

	_, socket := newDaemon(t, &fakeController{})
	assert.True(t, IsRunningAt(socket))
}

func TestDaemon_ConcurrentSessions(t *testing.T) {
	// Multiple clients handshake simultaneously. Each must get a unique
	// session ID and the daemon's session count must reflect them.
	srv, socket := newDaemon(t, &fakeController{})

	const N = 8
	clients := make([]*Client, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: fmt.Sprintf("c%d", i)})
			if err != nil {
				t.Errorf("dial %d: %v", i, err)
				return
			}
			clients[i] = c
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool)
	for _, c := range clients {
		require.NotNil(t, c)
		require.NotEmpty(t, c.Ack.SessionID)
		assert.False(t, seen[c.Ack.SessionID], "session_id collision: %s", c.Ack.SessionID)
		seen[c.Ack.SessionID] = true
	}

	require.Eventually(t, func() bool {
		return srv.Sessions().Count() == N
	}, 2*time.Second, 10*time.Millisecond,
		"daemon should see all %d sessions", N)

	for _, c := range clients {
		_ = c.Close()
	}

	require.Eventually(t, func() bool {
		return srv.Sessions().Count() == 0
	}, 2*time.Second, 10*time.Millisecond,
		"sessions should drain after clients close")
}

func TestDaemon_ShutdownRemovesSocketAndPIDFile(t *testing.T) {
	srv, socket := newDaemon(t, &fakeController{})
	pidFile := PIDFilePath()

	require.True(t, fileExists(socket))
	require.True(t, fileExists(pidFile))

	require.NoError(t, srv.Shutdown())

	require.Eventually(t, func() bool {
		return !fileExists(socket) && !fileExists(pidFile)
	}, 2*time.Second, 10*time.Millisecond)
}

func TestDaemon_ShutdownViaControl(t *testing.T) {
	ctrl := &fakeController{}
	srv, socket := newDaemon(t, ctrl)

	c, err := DialTo(socket, Handshake{Mode: ModeControl})
	require.NoError(t, err)

	resp, err := c.Control(ControlShutdown, nil)
	require.NoError(t, err)
	require.True(t, resp.OK)
	_ = c.Close()

	require.Eventually(t, func() bool { return !IsRunningAt(socket) },
		2*time.Second, 10*time.Millisecond)
	assert.Equal(t, 1, ctrl.shutdownCalls, "Controller.Shutdown must be invoked")

	// Calling Shutdown again must be safe (idempotent).
	require.NoError(t, srv.Shutdown())
}

// --- helpers ---

func fileExists(path string) bool {
	_, err := osStat(path)
	return err == nil
}
