package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubStream says what a redialStub connection does once its session is
// initialized: answer every request, stop reading (so a large client write
// blocks until its deadline), or hang up. stubRefuse refuses the handshake;
// stubServeSlow holds its handshake reply for redialStubHold, then serves.
type stubStream int

const (
	stubServe stubStream = iota
	stubStall
	stubHangUp
	stubRefuse
	stubServeSlow
)

// How long a stubServeSlow connection holds its handshake reply. A re-dial
// onto it takes at least this long, which every platform's clock resolves:
// a re-dial over a pipe otherwise takes microseconds, and the Windows
// runner's clock measured that as zero.
const redialStubHold = 30 * time.Millisecond

// startRedialStub is a newline-framed daemon that answers the handshake and
// initialize on every connection; behave picks what connection n (from 1)
// does after that. It returns the socket, the count of accepted sessions,
// and how long connection n held its handshake reply, as measured by the
// clock that times the client's re-dial (zero unless stubServeSlow).
//
// The socket is served over net.Pipe through socketDialers, not a real unix
// socket. A pipe write blocks until the stub reads it and its deadline fires
// in Go, so a stalled stream gives the client's write the timeout class on
// every platform. Over a real socket the OS decides how much an unread stream
// buffers and whether a blocked send is interrupted at its deadline: on the
// Windows runner the stalled write never reached the client's write timeout.
func startRedialStub(t *testing.T, behave func(connection int) stubStream) (string, func() int, func(connection int) time.Duration) {
	t.Helper()
	socket := "redial-stub/" + t.Name()
	var mu sync.Mutex
	var connections []net.Conn
	held := make(map[int]time.Duration)
	var workers sync.WaitGroup
	release := make(chan struct{})
	serve := func(conn net.Conn, connection int) {
		defer conn.Close()
		reader := bufio.NewReader(conn)
		write := func(value any) { _ = json.NewEncoder(conn).Encode(value) }
		if _, err := reader.ReadBytes('\n'); err != nil {
			return
		}
		stream := behave(connection)
		switch stream {
		case stubRefuse:
			write(map[string]any{"ok": false, "error_msg": "stub refused"})
			return
		case stubServeSlow:
			// Measured inside the client's re-dial, so the client's re-dial
			// time is at least this on any monotonic clock, however coarse.
			holding := time.Now()
			time.Sleep(redialStubHold)
			mu.Lock()
			held[connection] = time.Since(holding)
			mu.Unlock()
		}
		write(map[string]any{"ok": true, "daemon_version": "stub-v1"})
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var frame struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(line, &frame) != nil {
				return
			}
			switch {
			case frame.Method == "notifications/initialized":
				switch stream {
				case stubStall:
					<-release
					return
				case stubHangUp:
					return
				}
			case frame.ID == 0:
			case frame.Method == "initialize":
				write(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{}})
			default:
				write(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"method": frame.Method}})
			}
		}
	}
	dial := func(context.Context) (net.Conn, error) {
		client, conn := net.Pipe()
		mu.Lock()
		connections = append(connections, conn)
		connection := len(connections)
		mu.Unlock()
		workers.Go(func() { serve(conn, connection) })
		return client, nil
	}
	if _, registered := socketDialers.LoadOrStore(socket, dial); registered {
		t.Fatalf("stub socket %s is already served", socket)
	}
	t.Cleanup(func() {
		socketDialers.Delete(socket)
		close(release)
		mu.Lock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	sessions := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(connections)
	}
	hold := func(connection int) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return held[connection]
	}
	return socket, sessions, hold
}

// A frame whose write blocks until the client's write deadline once the stub
// stops reading. Over a pipe any frame blocks, so its size does not matter.
var redialLargeParams = map[string]string{"large": strings.Repeat("x", 1<<10)}

func openRedialClient(t *testing.T, ctx context.Context, socket string, limit int) *socketClient {
	t.Helper()
	c, err := openMCP(ctx, socket, "/fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.writeTimeout = 100 * time.Millisecond
	c.allowRedials(limit)
	return c
}

func requireWriteTimeout(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil || !errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("%s: want the write timeout class, got %v", what, err)
	}
}

func TestSessionRedialsAfterWriteTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	socket, sessions, _ := startRedialStub(t, func(connection int) stubStream {
		if connection == 1 {
			return stubStall
		}
		return stubServe
	})
	c := openRedialClient(t, ctx, socket, 2)
	_, err := c.call(ctx, "large", redialLargeParams)
	requireWriteTimeout(t, err, "the stalled write")
	for _, method := range []string{"after", "again"} {
		started := time.Now()
		data, err := c.call(ctx, method, nil)
		if err != nil {
			t.Fatalf("%s failed after %s instead of using a re-dialed session: %v", method, time.Since(started), err)
		}
		if string(data) != `{"method":"`+method+`"}` {
			t.Fatalf("%s: reply %s", method, data)
		}
	}
	if got := sessions(); got != 2 {
		t.Fatalf("sessions accepted: %d, want the first and one re-dial", got)
	}
	if got := c.redialCounts(); got != (sessionRedials{Attempts: 1}) {
		t.Fatalf("re-dial counts: %+v", got)
	}
}

// The call that re-opens a session is marked as re-dialed and carries the
// time it spent doing so, so that time can be told apart from the daemon's in
// its latency. The call that gave up its write, and a call on an open stream,
// carry neither.
func TestRedialedCallCarriesItsRedialTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	socket, _, held := startRedialStub(t, func(connection int) stubStream {
		if connection == 1 {
			return stubStall
		}
		return stubServeSlow
	})
	c := openRedialClient(t, ctx, socket, 2)
	_, stalled, err := c.callWithIdentity(ctx, "large", redialLargeParams)
	requireWriteTimeout(t, err, "the stalled write")
	if stalled.Redialed || stalled.Redial != 0 {
		t.Fatalf("the call that gave up its write carries re-dial mark %t, time %s", stalled.Redialed, stalled.Redial)
	}
	_, redialed, err := c.callWithIdentity(ctx, "after", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !redialed.Redialed {
		t.Fatal("the call that re-dialed is not marked as re-dialed")
	}
	if hold := held(2); hold <= 0 || redialed.Redial < hold {
		t.Fatalf("the call that re-dialed carries re-dial time %s, under the stub's %s handshake hold", redialed.Redial, hold)
	}
	if before := redialed.WriteStarted.Sub(redialed.Started); redialed.Redial > before {
		t.Fatalf("re-dial time %s is longer than the %s the call spent before its write", redialed.Redial, before)
	}
	_, open, err := c.callWithIdentity(ctx, "again", nil)
	if err != nil {
		t.Fatal(err)
	}
	if open.Redialed || open.Redial != 0 {
		t.Fatalf("a call on an open stream carries re-dial mark %t, time %s", open.Redialed, open.Redial)
	}
}

func TestSessionRedialLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	socket, sessions, _ := startRedialStub(t, func(int) stubStream { return stubStall })
	const limit = 2
	c := openRedialClient(t, ctx, socket, limit)
	for i := range limit + 1 {
		_, err := c.call(ctx, "large", redialLargeParams)
		requireWriteTimeout(t, err, "a write on the first or a re-dialed session")
		if got := sessions(); got != i+1 {
			t.Fatalf("after write %d: %d sessions accepted, want %d", i+1, got, i+1)
		}
	}
	started := time.Now()
	_, refused, err := c.callWithIdentity(ctx, "after-limit", nil)
	requireWriteTimeout(t, err, "a call past the re-dial limit")
	if !strings.Contains(err.Error(), "re-dial limit 2 reached") {
		t.Fatalf("a call past the limit does not say the session stayed closed: %v", err)
	}
	if refused.Redialed || refused.Redial != 0 {
		t.Fatalf("a call refused without a re-dial carries re-dial mark %t, time %s", refused.Redialed, refused.Redial)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("a call past the limit waited %s", elapsed)
	}
	if got := sessions(); got != limit+1 {
		t.Fatalf("sessions accepted: %d, want %d", got, limit+1)
	}
	if got := c.redialCounts(); got != (sessionRedials{Attempts: limit, LimitReached: true}) {
		t.Fatalf("re-dial counts: %+v", got)
	}
}

// A re-dial that fails keeps the session closed with the original error, and
// the next one waits out a back-off before it dials again.
func TestSessionRedialBacksOffAfterAFailedRedial(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	socket, sessions, _ := startRedialStub(t, func(connection int) stubStream {
		switch connection {
		case 1:
			return stubStall
		case 2:
			return stubRefuse
		}
		return stubServe
	})
	c := openRedialClient(t, ctx, socket, 2)
	_, err := c.call(ctx, "large", redialLargeParams)
	requireWriteTimeout(t, err, "the stalled write")
	started := time.Now()
	_, err = c.call(ctx, "refused", nil)
	requireWriteTimeout(t, err, "a call whose re-dial was refused")
	if !strings.Contains(err.Error(), "re-dial 1 of 2 failed: handshake refused: stub refused") {
		t.Fatalf("a failed re-dial is not named: %v", err)
	}
	if data, err := c.call(ctx, "after", nil); err != nil || string(data) != `{"method":"after"}` {
		t.Fatalf("the second re-dial: %s %v", data, err)
	}
	if elapsed := time.Since(started); elapsed < redialBackoff(1) {
		t.Fatalf("the second re-dial came %s after the first failed, before its back-off", elapsed)
	}
	if got := sessions(); got != 3 {
		t.Fatalf("sessions accepted: %d, want 3", got)
	}
	if got := c.redialCounts(); got != (sessionRedials{Attempts: 2, Failed: 1}) {
		t.Fatalf("re-dial counts: %+v", got)
	}
}

// A stream the daemon closed stays closed: only a write this client gave up
// on is re-dialed, so a killed daemon is never silently replaced.
func TestSessionClosedByDaemonIsNotRedialed(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	socket, sessions, _ := startRedialStub(t, func(int) stubStream { return stubHangUp })
	c := openRedialClient(t, ctx, socket, 2)
	select {
	case <-c.done:
	case <-ctx.Done():
		t.Fatal("the client never saw the daemon hang up")
	}
	for range 2 {
		_, identity, err := c.callWithIdentity(ctx, "after-hangup", nil)
		if err == nil {
			t.Fatal("a call succeeded on a stream the daemon closed")
		}
		if identity.Redialed || identity.Redial != 0 {
			t.Fatalf("a call on a stream the daemon closed carries re-dial mark %t, time %s", identity.Redialed, identity.Redial)
		}
	}
	if got := sessions(); got != 1 {
		t.Fatalf("sessions accepted: %d, want 1", got)
	}
	if got := c.redialCounts(); got != (sessionRedials{}) {
		t.Fatalf("re-dial counts: %+v", got)
	}
}
