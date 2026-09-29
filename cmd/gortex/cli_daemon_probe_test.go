package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

// routingStubDaemon answers the control kinds the CLI routing pre-flight asks.
// It can refuse probe (a daemon from before probe existed) and delay status (a
// daemon whose status aggregate is busy), and it records every control kind
// it was asked, in order.
type routingStubDaemon struct {
	ln           net.Listener
	trackedRepos []string
	knowsProbe   bool
	statusDelay  time.Duration

	mu     sync.Mutex
	kinds  []string
	mcpCWD string
}

func startRoutingStubDaemon(t *testing.T, trackedRepos []string, knowsProbe bool, statusDelay time.Duration) *routingStubDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "gxr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Setenv("GORTEX_DAEMON_SOCKET", sock)
	t.Cleanup(func() { _ = ln.Close() })
	s := &routingStubDaemon{ln: ln, trackedRepos: trackedRepos, knowsProbe: knowsProbe, statusDelay: statusDelay}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	return s
}

func (s *routingStubDaemon) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var hs daemon.Handshake
	if json.Unmarshal(line, &hs) != nil {
		return
	}
	if hs.Mode == daemon.ModeMCP {
		s.mu.Lock()
		s.mcpCWD = hs.CWD
		s.mu.Unlock()
	}
	if daemon.WriteJSONLine(conn, daemon.HandshakeAck{OK: true, DaemonVersion: "stub"}) != nil || hs.Mode != daemon.ModeControl {
		return
	}
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req daemon.ControlRequest
		if json.Unmarshal(line, &req) != nil {
			return
		}
		s.mu.Lock()
		s.kinds = append(s.kinds, req.Kind)
		s.mu.Unlock()
		switch {
		case req.Kind == daemon.ControlProbe && s.knowsProbe:
			pr := daemon.ProbeResponse{Version: "stub", Ready: true}
			for _, p := range s.trackedRepos {
				pr.TrackedRepos = append(pr.TrackedRepos, daemon.ProbeRepo{Path: p, Prefix: filepath.Base(p)})
			}
			raw, _ := json.Marshal(pr)
			_ = daemon.WriteJSONLine(conn, daemon.ControlResponse{OK: true, Result: raw})
		case req.Kind == daemon.ControlStatus:
			time.Sleep(s.statusDelay)
			st := daemon.StatusResponse{Version: "stub", Ready: true}
			for _, p := range s.trackedRepos {
				st.TrackedRepos = append(st.TrackedRepos, daemon.TrackedRepoStatus{Path: p})
			}
			raw, _ := json.Marshal(st)
			_ = daemon.WriteJSONLine(conn, daemon.ControlResponse{OK: true, Result: raw})
		default:
			_ = daemon.WriteJSONLine(conn, daemon.ControlResponse{OK: false, ErrorCode: "unsupported"})
		}
	}
}

func (s *routingStubDaemon) askedKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.kinds...)
}

func (s *routingStubDaemon) mcpSessionCWD() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcpCWD
}

// TestRoutingPreflightReadsTrackedRootsWithoutStatus pins the cost of the
// pre-flight every CLI graph query pays: the tracked roots come from probe,
// so a status aggregate that takes seconds does not delay the call at all.
func TestRoutingPreflightReadsTrackedRootsWithoutStatus(t *testing.T) {
	repo := t.TempDir()
	stub := startRoutingStubDaemon(t, []string{repo}, true, 2*time.Second)

	started := time.Now()
	exec, err := resolveExecutor(repo)
	elapsed := time.Since(started)
	require.NoError(t, err)
	defer exec.Close()

	assert.Equal(t, []string{daemon.ControlProbe}, stub.askedKinds(),
		"the pre-flight must learn the tracked roots from probe and never wait on status")
	assert.Less(t, elapsed, time.Second,
		"a slow status aggregate must not be on the path of a CLI call (took %s)", elapsed)
	assert.Equal(t, repo, stub.mcpSessionCWD(), "the call must still be relayed with the repo as its cwd")
}

// TestRoutingPreflightRefusesAnUntrackedPathFromProbe keeps the negative arm:
// a path no tracked root reaches is still refused, from the probe answer.
func TestRoutingPreflightRefusesAnUntrackedPathFromProbe(t *testing.T) {
	tracked := t.TempDir()
	other := t.TempDir()
	stub := startRoutingStubDaemon(t, []string{tracked}, true, 0)

	_, err := resolveExecutor(other)
	require.ErrorIs(t, err, ErrNoExecutor)
	kinds := stub.askedKinds()
	require.NotEmpty(t, kinds)
	assert.Equal(t, daemon.ControlProbe, kinds[0])
	assert.NotContains(t, kinds, daemon.ControlStatus)
	assert.Empty(t, stub.mcpSessionCWD(), "a refused pre-flight must not open an MCP session")
}

// TestRoutingPreflightFallsBackToStatusOnADaemonWithoutProbe keeps an older
// daemon working: probe is refused, so the tracked roots come from status.
func TestRoutingPreflightFallsBackToStatusOnADaemonWithoutProbe(t *testing.T) {
	repo := t.TempDir()
	stub := startRoutingStubDaemon(t, []string{repo}, false, 0)

	exec, err := resolveExecutor(repo)
	require.NoError(t, err)
	defer exec.Close()
	assert.Equal(t, []string{daemon.ControlProbe, daemon.ControlStatus}, stub.askedKinds())
	assert.Equal(t, repo, stub.mcpSessionCWD())
}
