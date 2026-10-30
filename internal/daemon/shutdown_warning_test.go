package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShutdownAckRemainsReadableAfterOwnedRuntimeIsRemoved(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "runtime.json"))
	_, socket := newDaemon(t, &fakeController{})
	require.NoError(t, WriteRuntimeState(RuntimeState{BackendPath: filepath.Join(t.TempDir(), "store.sqlite")}))
	c, err := DialTo(socket, Handshake{Mode: ModeControl})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Conn.SetDeadline(time.Now().Add(3*time.Second)))
	require.NoError(t, WriteJSONLine(c.Conn, ControlRequest{Kind: ControlShutdown}))
	// Deliberately do not read the response until the daemon has closed the
	// stream and removed its runtime. This tests submitted ACK delivery,
	// rather than relying on a delay to let a fast client read first.
	require.Eventually(t, func() bool {
		return !fileExists(socket) && !fileExists(PIDFilePath()) && !fileExists(RuntimeStatePath())
	}, 3*time.Second, time.Millisecond)
	line, err := c.reader.ReadBytes('\n')
	require.NoError(t, err)
	var ack ControlResponse
	require.NoError(t, json.Unmarshal(line, &ack))
	require.True(t, ack.OK)
	require.Empty(t, ack.ErrorCode)
}

func TestDaemonShutdownCleanupFailureIsWarningAndRemovesRuntime(t *testing.T) {
	ctrl := &fakeController{shutdownErr: errors.New("backend close failed")}
	_, socket := newDaemon(t, ctrl)
	pidFile := PIDFilePath()
	c, err := DialTo(socket, Handshake{Mode: ModeControl})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	resp, err := c.Control(ControlShutdown, nil)
	require.NoError(t, err)
	require.True(t, resp.OK, "shutdown is accepted despite cleanup errors")
	require.Empty(t, resp.ErrorCode)
	require.Equal(t, []string{"backend close failed"}, resp.Warnings)
	require.Eventually(t, func() bool { return !fileExists(socket) && !fileExists(pidFile) }, 3*time.Second, time.Millisecond)
}

type shutdownFailedAckConn struct {
	net.Conn
	attempted chan struct{}
}

func (c *shutdownFailedAckConn) Write([]byte) (int, error) {
	close(c.attempted)
	return 0, errors.New("client disappeared before acknowledgement")
}

func TestDaemonShutdownCleanupFailureAndFailedAckStillRemoveRuntime(t *testing.T) {
	srv, socket := newDaemon(t, &fakeController{shutdownErr: errors.New("lifecycle close failed after joining")})
	pidFile := PIDFilePath()
	server, client := net.Pipe()
	defer func() { _ = client.Close(); _ = server.Close() }()
	failed := &shutdownFailedAckConn{Conn: server, attempted: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveControl(failed, bufio.NewReader(server), nil)
	}()
	require.NoError(t, client.SetWriteDeadline(time.Now().Add(3*time.Second)))
	require.NoError(t, WriteJSONLine(client, ControlRequest{Kind: ControlShutdown}))
	select {
	case <-failed.attempted:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown acknowledgement was not attempted")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("failed acknowledgement stranded the control handler")
	}
	require.Eventually(t, func() bool { return !fileExists(socket) && !fileExists(pidFile) }, 3*time.Second, time.Millisecond)
}

func TestControlShutdownWarningsAreOptionalProtocolFields(t *testing.T) {
	old, err := json.Marshal(ControlResponse{OK: true})
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, string(old))
	var receipt ControlResponse
	require.NoError(t, json.Unmarshal([]byte(`{"ok":true,"warnings":["cleanup failed"]}`), &receipt))
	require.True(t, receipt.OK)
	require.Equal(t, []string{"cleanup failed"}, receipt.Warnings)
}
