package mcp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// Exercise the same grouped physical reader as pending-text search. The actual
// registered peer edits another declaration after the first read. A coordinated
// tool write must follow the coherent byte capture, not split its two hashes.
// Uncoordinated filesystem writes remain covered by the root's refusal controls.
func TestPendingTextReadCapturesBeforeConcurrentToolWrite(t *testing.T) {
	s, backend, base, path, original := snapshotFacadeFixture(t)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(before), "func TargetBefore(")
	require.Contains(t, string(before), "func Target(")
	require.Nil(t, backend.GetNode("main.go::Target"))

	const peerID = "pending-text-peer"
	require.NoError(t, s.ConnectSession(peerID, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(peerID) })
	peer := s.WithClientSession(WithSessionID(t.Context(), peerID), peerID)
	s.NoteSessionToolPolicy(peerID, FacadeSurfaceVersion, "hide")
	require.NotNil(t, s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load","version":"1"}}}`)))
	s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	peer, cancelPeer := context.WithTimeout(peer, 5*time.Second)
	defer cancelPeer()
	reader, err := openPendingTextRootForScan(filepath.Dir(path))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.close(context.Background()) })
	ctx, cancel := context.WithTimeout(base, 150*time.Millisecond)
	defer cancel()
	originalDeadline, _ := ctx.Deadline()

	type answer struct {
		result *mcplib.CallToolResult
		err    error
	}
	acquired, allowWrite := make(chan struct{}), make(chan struct{})
	permit := sync.OnceFunc(func() { close(allowWrite) })
	done := make(chan answer, 1)
	var worker sync.WaitGroup
	t.Cleanup(func() { permit(); cancelPeer(); worker.Wait() })
	var completed *answer
	var barrierErr error
	queued := false
	raw, evidence, readErr := reader.read(ctx, filepath.Base(path), func() {
		worker.Add(1)
		go func() {
			defer worker.Done()
			owned, release, err := acquireMutationPath(peer, path)
			if err != nil {
				done <- answer{err: err}
				return
			}
			defer release()
			close(acquired)
			select {
			case <-allowWrite:
			case <-peer.Done():
				done <- answer{err: peer.Err()}
				return
			}
			result, err := externalVersionRegisteredCall(s, owned, 30, "edit", snapshotEditArgs(path, "NeighborBefore", "Neighbor"))
			done <- answer{result: result, err: err}
		}()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-acquired:
				// On the old reader no token protects the read. Let the real
				// write complete so its strict second hash witnesses the gap.
				permit()
				select {
				case value := <-done:
					completed = &value
				case <-peer.Done():
					barrierErr = peer.Err()
				}
				return
			case <-tick.C:
				if mutationRefs(path) == 2 {
					queued = true
					permit()
					return
				}
			case <-ctx.Done():
				barrierErr = ctx.Err()
				permit()
				return
			}
		}
	}, 1<<20, make([]byte, 32<<10))
	closeErr := reader.close(ctx)
	if completed == nil {
		select {
		case value := <-done:
			completed = &value
		case <-peer.Done():
			t.Fatal("registered writer did not join: ", peer.Err())
		}
	}
	worker.Wait()
	after, diskErr := os.ReadFile(path)
	deadline, _ := ctx.Deadline()
	t.Logf("writer_queued=%v read_error=%v group_close=%v original_context=%v remaining=%s physical_before=%x physical_after=%x", queued, readErr, closeErr, ctx.Err(), time.Until(originalDeadline), sha256.Sum256(before), sha256.Sum256(after))
	require.NoError(t, barrierErr)
	require.NoError(t, completed.err)
	require.NotNil(t, completed.result)
	require.False(t, completed.result.IsError, resultText(completed.result))
	ack := extractTextResult(t, completed.result)
	require.Equal(t, "local", ack["ack_mode"])
	require.Equal(t, true, ack["readback_exact"])
	require.NoError(t, diskErr)
	require.NotEqual(t, before, after)
	require.Contains(t, string(after), "func Neighbor(")
	require.Contains(t, string(after), "func Target(")
	require.NotSame(t, original, liveEntry(pendingFileProjections, path))
	require.NoError(t, ctx.Err(), "the original read allowance must remain live")
	require.Equal(t, originalDeadline, deadline)
	require.NoError(t, readErr, "a later tool write must not split the pending text byte capture")
	require.NoError(t, closeErr)
	require.True(t, queued, "the actual same-path writer must wait only for byte capture")
	require.Equal(t, before, raw)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(raw)), evidence.contentSHA256)
	newRows, err := s.scanSourceSearchText(ctx, s.indexer, path, "main.go", raw, "func Target(", nil, 10000)
	require.NoError(t, err)
	require.Len(t, newRows, 1)
	require.Equal(t, "main.go", newRows[0].Path)
	require.Equal(t, 3, newRows[0].Line)
	oldRows, err := s.scanSourceSearchText(ctx, s.indexer, path, "main.go", raw, "func TargetBefore(", nil, 10000)
	require.NoError(t, err)
	require.Empty(t, oldRows, "complete old-text absence is proven by the same captured bytes")
	require.Zero(t, mutationRefs(path))
}
