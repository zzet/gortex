package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// blockingController blocks Status (and Shutdown) until released, standing in
// for the real controller's coarse mutex being held by a minutes-long
// track / reload / enrichment.
type blockingController struct {
	fakeController
	release chan struct{}
	entered chan struct{}
}

type blockingSessionEndDispatcher struct {
	entered chan struct{}
	release chan struct{}
}

func (d *blockingSessionEndDispatcher) Dispatch(context.Context, *Session, []byte) ([]byte, error) {
	return nil, nil
}

func (d *blockingSessionEndDispatcher) SessionEnded(*Session) {
	close(d.entered)
	<-d.release
}

func newBlockingController() *blockingController {
	return &blockingController{
		release: make(chan struct{}),
		entered: make(chan struct{}, 8),
	}
}

func (b *blockingController) Status(ctx context.Context) (StatusResponse, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return b.fakeController.Status(ctx)
	case <-time.After(30 * time.Second):
		return StatusResponse{}, errors.New("blockingController: test did not release in time")
	}
}

// TestControlTimeoutPolicy pins which kinds may run long. Getting this wrong in
// either direction is a user-visible regression: bounding track would abort an
// index the user deliberately started, and leaving status unbounded is the
// defect itself.
func TestControlTimeoutPolicy(t *testing.T) {
	for _, kind := range []string{
		ControlTrack, ControlUntrack, ControlReload, ControlShutdown,
		ControlEnrichChurn, ControlEnrichReleases, ControlEnrichBlame,
		ControlEnrichCoverage, ControlEnrichCochange,
	} {
		assert.Zerof(t, ControlTimeoutFor(kind), "%s must stay unbounded — it is long by design", kind)
	}
	for _, kind := range []string{ControlStatus, ControlSearchSymbols, ControlProxy} {
		assert.Equalf(t, DefaultControlTimeout, ControlTimeoutFor(kind),
			"%s sits on the critical path of ordinary commands and must be bounded", kind)
	}
}

// TestShutdownTearsDownEvenWhenTheAckCannotBeDelivered: the stop command now
// bounds its own wait, so a client that gives up on a slow flush is an ordinary
// event. The daemon must still go down — a daemon that flushed its store and
// then kept running would hold the on-disk lock the next `daemon start` needs.
func TestShutdownTearsDownEvenWhenTheAckCannotBeDelivered(t *testing.T) {
	ctrl := &fakeController{}
	srv, socket := newDaemon(t, ctrl)

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)

	// Send the request, then vanish before the ack can be written.
	require.NoError(t, WriteJSONLine(c.Conn, ControlRequest{Kind: ControlShutdown}))
	_ = c.Conn.Close()

	require.Eventually(t, func() bool { return !IsRunningAt(socket) },
		5*time.Second, 20*time.Millisecond,
		"the daemon must tear down even when the shutdown ack could not be delivered")
	_ = srv
}

func TestServeJoinsActiveUnixHandlersBeforeReturning(t *testing.T) {
	ctrl := newBlockingController()
	srv, socket := newDaemonNoServe(t, ctrl)
	srv.ControlTimeout = time.Hour
	require.NoError(t, srv.Listen())
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve() }()

	client, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "blocking-status"})
	require.NoError(t, err)
	requestDone := make(chan error, 1)
	go func() {
		_, requestErr := client.ControlWithTimeout(ControlStatus, nil, time.Minute)
		requestDone <- requestErr
	}()
	select {
	case <-ctrl.entered:
	case <-time.After(time.Second):
		t.Fatal("blocking status handler was not admitted")
	}

	require.NoError(t, srv.Shutdown())
	select {
	case err := <-serveDone:
		t.Fatalf("Serve returned while a control handler still owned graph work: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(ctrl.release)
	select {
	case err := <-serveDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after the admitted handler completed")
	}
	_ = client.Close()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("client request did not finish after handler release")
	}
}

func TestServeJoinsAdmittedMaintenanceSweepBeforeReturning(t *testing.T) {
	dispatcher := &blockingSessionEndDispatcher{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	srv, _ := newDaemonNoServe(t, &fakeController{})
	srv.maintenanceInterval = time.Millisecond
	srv.MCPDispatcher = dispatcher
	srv.sessions.RegisterDetached("dead-client", Handshake{
		Mode: ModeMCP,
		PID:  1 << 30,
	})
	require.NoError(t, srv.Listen())
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve() }()
	select {
	case <-dispatcher.entered:
	case <-time.After(time.Second):
		t.Fatal("maintenance sweep did not enter the session-ended hook")
	}

	require.NoError(t, srv.Shutdown())
	select {
	case err := <-serveDone:
		t.Fatalf("Serve returned while maintenance still owned its callback: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(dispatcher.release)
	select {
	case err := <-serveDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after maintenance callback completed")
	}
}

// TestControlRequestTimesOutInsteadOfHanging is the #370 server-side contract:
// a controller wedged behind its own lock must produce a terminal response, not
// an open-ended silence.
func TestControlRequestTimesOutInsteadOfHanging(t *testing.T) {
	ctrl := newBlockingController()
	srv, socket := newDaemon(t, ctrl)
	srv.ControlTimeout = 150 * time.Millisecond
	defer close(ctrl.release)

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	start := time.Now()
	resp, err := c.ControlWithTimeout(ControlStatus, nil, 5*time.Second)
	require.NoError(t, err, "the daemon must answer, not drop the request")
	assert.False(t, resp.OK)
	assert.Equal(t, ErrTimeout, resp.ErrorCode,
		"a busy controller must be reported as busy, not as an internal error")
	assert.Contains(t, resp.ErrorMsg, "daemon status",
		"the error must tell the user what to do next")
	assert.Less(t, time.Since(start), 3*time.Second, "response must arrive on the server's budget")
}

// TestControlSurvivesAWedgedRequest guards the property that actually rescues a
// user: one stuck control request must not poison the connection. `gortex
// daemon stop` has to work while `daemon status` is wedged.
func TestControlSurvivesAWedgedRequest(t *testing.T) {
	ctrl := newBlockingController()
	srv, socket := newDaemon(t, ctrl)
	srv.ControlTimeout = 150 * time.Millisecond
	defer close(ctrl.release)

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	resp, err := c.ControlWithTimeout(ControlStatus, nil, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, ErrTimeout, resp.ErrorCode)

	// Same connection, immediately after: a kind the controller does not block
	// on must still be served.
	resp, err = c.ControlWithTimeout(ControlSearchSymbols, SearchSymbolsParams{Query: "x"}, 5*time.Second)
	require.NoError(t, err)
	assert.True(t, resp.OK, "the session must stay usable after a timed-out request: %+v", resp)
}

// TestClientControlReportsUnresponsiveDaemon is the #370 client-side contract.
// The daemon here accepts and acks the handshake and then goes silent — the
// shape of a daemon whose controller is wedged. Before this, the CLI blocked in
// ReadBytes forever with nothing printed; `gortex daemon stop` "hanging
// indefinitely (tested up to 5 minutes)" is exactly this read.
func TestClientControlReportsUnresponsiveDaemon(t *testing.T) {
	socket := silentDaemon(t)

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err, "the handshake must still complete — only the control call goes unanswered")
	defer c.Close()

	start := time.Now()
	_, err = c.ControlWithTimeout(ControlShutdown, nil, 200*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDaemonUnresponsive)
	assert.NotErrorIs(t, err, ErrDaemonUnavailable,
		"an unresponsive daemon still holds the store lock — it must not be mistaken for an absent one")
	assert.Contains(t, err.Error(), "daemon status")
	assert.Less(t, time.Since(start), 5*time.Second)
}

// TestDialReportsUnresponsiveDaemonOnHandshake covers the earlier failure
// point: a daemon that accepts the connection but never acks. The handshake ack
// is a real round trip through the server, so it stalls too.
func TestDialReportsUnresponsiveDaemonOnHandshake(t *testing.T) {
	ln := listenShort(t)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and hold: never read, never ack.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	prev := controlHandshakeTimeoutForTest
	controlHandshakeTimeoutForTest = 200 * time.Millisecond
	t.Cleanup(func() { controlHandshakeTimeoutForTest = prev })

	start := time.Now()
	_, err := DialTo(ln.Addr().String(), Handshake{Mode: ModeControl, ClientName: "cli"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDaemonUnresponsive)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// silentDaemon accepts connections, completes the handshake, then never
// answers a control request.
func silentDaemon(t *testing.T) string {
	t.Helper()
	ln := listenShort(t)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 4096)
				if _, err := conn.Read(buf); err != nil {
					return
				}
				ack, _ := json.Marshal(HandshakeAck{OK: true, SessionID: "silent", DaemonVersion: "test"})
				_, _ = conn.Write(append(ack, '\n'))
				// Drain forever without replying.
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// listenShort mints a unix listener under a short path — macOS caps sun_path
// at ~104 bytes and t.TempDir() with a long test name blows past it.
func listenShort(t *testing.T) net.Listener {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gx")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// TestHandleControlBoundedTimesOutWithoutASocket pins the timeout branch
// directly — no listener, no connection, a nil session — so the ErrTimeout
// response is attributable to handleControlBounded itself rather than to
// anything the transport does.
func TestHandleControlBoundedTimesOutWithoutASocket(t *testing.T) {
	ctrl := newBlockingController()
	defer close(ctrl.release)
	srv := New("unused", "test", zap.NewNop())
	srv.Controller = ctrl
	srv.ControlTimeout = 50 * time.Millisecond
	resp := srv.handleControlBounded(nil, ControlRequest{Kind: ControlStatus})
	assert.Equal(t, ErrTimeout, resp.ErrorCode)
}

// blockingTrackController blocks Track — an unbounded kind — so the dispatch
// branch that runs unbounded kinds inline can be pinned.
type blockingTrackController struct {
	fakeController
	release chan struct{}
	entered chan struct{}
}

func (b *blockingTrackController) Track(ctx context.Context, p TrackParams) (json.RawMessage, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return b.fakeController.Track(ctx, p)
	case <-time.After(30 * time.Second):
		return nil, errors.New("blockingTrackController: test did not release in time")
	}
}

// TestUnboundedKindsAreNotClamped is the other half of the timeout policy. The
// bound exists to stop status-shaped calls hanging; clamping a track would abort
// an index the user deliberately started, which is a worse bug than the one
// being fixed. Server.ControlTimeout must not reach the unbounded kinds.
func TestUnboundedKindsAreNotClamped(t *testing.T) {
	ctrl := &blockingTrackController{
		release: make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
	srv, socket := newDaemon(t, ctrl)
	srv.ControlTimeout = 100 * time.Millisecond

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	done := make(chan ControlResponse, 1)
	go func() {
		resp, _ := c.ControlWithTimeout(ControlTrack, TrackParams{Path: "/tmp/myapp"}, 10*time.Second)
		done <- resp
	}()

	<-ctrl.entered
	// Well past the (deliberately tiny) bounded-kind budget: a track must still
	// be running, not cut off.
	select {
	case resp := <-done:
		t.Fatalf("track was clamped by the bounded-kind budget: %+v", resp)
	case <-time.After(500 * time.Millisecond):
	}

	close(ctrl.release)
	resp := <-done
	assert.True(t, resp.OK, "track must complete normally once it finishes: %+v", resp)
	assert.Contains(t, string(resp.Result), "/tmp/myapp")
}
