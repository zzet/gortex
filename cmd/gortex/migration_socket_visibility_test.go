//go:build linux || darwin

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
)

func migrationProbeReply(id int) []byte {
	payload, _ := json.Marshal(map[string]any{"results": []any{map[string]any{"name": "MigrationSocketProbe", "id": "owned-node"}}})
	reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(payload)}}}})
	return append(reply, '\n')
}

func TestMigrationSocketPostReleasePartialFrameKeepsOriginalBudget(t *testing.T) {
	parent, peer := net.Pipe()
	defer parent.Close()
	defer peer.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer peer.Close()
		reader := bufio.NewReader(peer)
		if _, err := reader.ReadBytes('\n'); err != nil {
			return
		}
		reply := migrationProbeReply(4)
		if _, err := peer.Write(reply[:15]); err != nil {
			return
		}
		timer := time.NewTimer(1100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-t.Context().Done():
			return
		}
		_, _ = peer.Write(reply[15:])
	}()
	t.Cleanup(func() { _ = parent.Close(); _ = peer.Close(); <-done })
	c := &daemon.Client{Conn: parent}
	require.NoError(t, migrationSocketAwaitProbe(c, time.Now().Add(2*time.Second)))
}

// Keep the original initialized Client and its handshake-owned buffered reader.
// The absolute visibility deadline is installed once, never renewed per frame.
func migrationSocketAwaitProbe(c *daemon.Client, deadline time.Time) error {
	if err := c.Conn.SetDeadline(deadline); err != nil {
		return err
	}
	for id := 4; ; id++ {
		if !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
		frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"MigrationSocketProbe","format":"json"}}}`, id)
		if err := c.WriteMCPFrame([]byte(frame)); err != nil {
			return fmt.Errorf("post-migration visibility write: %w", err)
		}
		var response map[string]any
		for {
			raw, err := c.ReadMCPFrame()
			if err != nil {
				return fmt.Errorf("post-migration visibility read: %w", err)
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				return fmt.Errorf("post-migration visibility frame: %w", err)
			}
			if !time.Now().Before(deadline) {
				return context.DeadlineExceeded
			}
			if response["id"] == nil {
				if _, notification := response["method"].(string); notification {
					continue
				}
				return fmt.Errorf("post-migration visibility response lacks request identity")
			}
			if response["id"] != float64(id) {
				return fmt.Errorf("post-migration visibility response id=%v want=%d", response["id"], id)
			}
			break
		}
		if rpcError, failed := response["error"].(map[string]any); failed {
			data, _ := rpcError["data"].(map[string]any)
			if data["error_code"] != "repo_loading" {
				return fmt.Errorf("post-migration visibility error: %v", rpcError)
			}
		} else {
			result, _ := response["result"].(map[string]any)
			content, _ := result["content"].([]any)
			for _, block := range content {
				text, _ := block.(map[string]any)["text"].(string)
				var payload struct {
					Results []struct {
						Name string `json:"name"`
						ID   string `json:"id"`
					} `json:"results"`
				}
				if json.Unmarshal([]byte(text), &payload) == nil {
					for _, node := range payload.Results {
						if node.Name == "MigrationSocketProbe" && node.ID != "" {
							// Payload decoding remains inside the same absolute budget.
							if !time.Now().Before(deadline) {
								return context.DeadlineExceeded
							}
							return nil
						}
					}
				}
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		timer := time.NewTimer(min(20*time.Millisecond, remaining))
		<-timer.C
	}
}

type migrationDeadlineConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *migrationDeadlineConn) SetDeadline(d time.Time) error {
	c.deadlines = append(c.deadlines, d)
	return c.Conn.SetDeadline(d)
}

func migrationVisibilityPeer(t *testing.T, serve func(net.Conn)) *daemon.Client {
	t.Helper()
	parent, peer := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); defer peer.Close(); serve(peer) }()
	t.Cleanup(func() { _ = parent.Close(); _ = peer.Close(); <-done })
	return &daemon.Client{Conn: parent}
}
func TestMigrationSocketVisibilitySequentialRepliesKeepAbsoluteDeadline(t *testing.T) {
	var ids []int
	c := migrationVisibilityPeer(t, func(peer net.Conn) {
		reader := bufio.NewReader(peer)
		for i := 0; i < 2; i++ {
			raw, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var request struct {
				ID int `json:"id"`
			}
			if json.Unmarshal(raw, &request) != nil {
				return
			}
			ids = append(ids, request.ID)
			reply := []byte(`{"jsonrpc":"2.0","id":4,"error":{"data":{"error_code":"repo_loading"}}}` + "\n")
			if i == 1 {
				reply = migrationProbeReply(request.ID)
			}
			if _, err := peer.Write(reply); err != nil {
				return
			}
		}
	})
	conn := &migrationDeadlineConn{Conn: c.Conn}
	c.Conn = conn
	deadline := time.Now().Add(time.Second)
	require.NoError(t, migrationSocketAwaitProbe(c, deadline))
	require.Equal(t, []int{4, 5}, ids)
	require.Equal(t, []time.Time{deadline}, conn.deadlines)
}
func TestMigrationSocketVisibilityRejectsOverdueWrongIDAndDisconnect(t *testing.T) {
	for _, name := range []string{"nonresponse", "overdue-partial", "wrong-id", "disconnect"} {
		t.Run(name, func(t *testing.T) {
			c := migrationVisibilityPeer(t, func(peer net.Conn) {
				reader := bufio.NewReader(peer)
				if _, err := reader.ReadBytes('\n'); err != nil {
					return
				}
				switch name {
				case "nonresponse":
					_, _ = io.Copy(io.Discard, peer)
				case "overdue-partial":
					reply := migrationProbeReply(4)
					_, _ = peer.Write(reply[:15])
					_, _ = io.Copy(io.Discard, peer)
				case "wrong-id":
					_, _ = peer.Write(migrationProbeReply(5))
				case "disconnect":
					return
				}
			})
			err := migrationSocketAwaitProbe(c, time.Now().Add(50*time.Millisecond))
			require.Error(t, err)
		})
	}
}

// A deadline does not revoke bytes already returned to a buffered reader.
// Exercise a complete response whose read/decode finishes after the budget.
type migrationDelayedReadConn struct {
	net.Conn
	delay time.Duration
}

func (c *migrationDelayedReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		time.Sleep(c.delay)
	}
	return n, err
}
func TestMigrationSocketVisibilityRejectsBufferedCompleteAfterDeadline(t *testing.T) {
	c := migrationVisibilityPeer(t, func(peer net.Conn) {
		reader := bufio.NewReader(peer)
		if _, err := reader.ReadBytes('\n'); err != nil {
			return
		}
		_, _ = peer.Write(migrationProbeReply(4))
	})
	c.Conn = &migrationDelayedReadConn{Conn: c.Conn, delay: 50 * time.Millisecond}
	require.Error(t, migrationSocketAwaitProbe(c, time.Now().Add(20*time.Millisecond)))
}
