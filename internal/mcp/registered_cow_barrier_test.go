package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

type registeredCOWReply struct {
	tool      string
	result    *mcplib.CallToolResult
	err       error
	remaining time.Duration
}

// Worker calls return errors to the owning test instead of Goexit-ing before
// publishing an answer. This is the same registered SDK path and authorization.
func registeredCOWCall(s *Server, ctx context.Context, id int, name string, args map[string]any) (*mcplib.CallToolResult, error) {
	if !s.IsToolEnabledForSession(ctx, name) {
		return nil, fmt.Errorf("session tool disabled: %s", name)
	}
	s.EnsureToolPromotedForSession(ctx, name)
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		return nil, err
	}
	reply := s.MCPServer().HandleMessage(WithAuthorizedToolCall(ctx, name), frame)
	if reply == nil {
		return nil, errors.New("registered SDK returned no response")
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Error  any                    `json:"error"`
		Result *mcplib.CallToolResult `json:"result"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("registered protocol error: %v", envelope.Error)
	}
	if envelope.Result == nil {
		return nil, errors.New("registered SDK result absent")
	}
	return envelope.Result, nil
}

// A completed public refusal is the first failing stage, not evidence that
// more callbacks will arrive. Always release held test work before reporting it.
func awaitRegisteredCOWMetadata(ctx context.Context, captured <-chan *projectionEntry, answers <-chan registeredCOWReply, want int, release func(), onCapture func(*projectionEntry)) ([]registeredCOWReply, error) {
	defer release()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	early := []registeredCOWReply{}
	count := 0
	for count < want {
		select {
		case entry := <-captured:
			count++
			if onCapture != nil {
				onCapture(entry)
			}
		case reply := <-answers:
			early = append(early, reply)
			if reply.tool == "read_file" && reply.err == nil {
				continue
			}
			text := "no tool result"
			if reply.result != nil {
				text = resultText(reply.result)
			}
			return early, fmt.Errorf("%s returned before metadata barrier: arrivals=%d/%d remaining_original_budget=%s protocol_error=%v result=%s", reply.tool, count, want, reply.remaining, reply.err, text)
		case <-ctx.Done():
			return early, ctx.Err()
		case <-timer.C:
			return early, fmt.Errorf("owned registered metadata arrivals=%d/%d; no early public answer observed", count, want)
		}
	}
	return early, nil
}

func TestRegisteredCOWBarrierReportsEarlyRefusalAndReleases(t *testing.T) {
	// A real registered SDK call with original caller cancellation cannot reach
	// the metadata hook. Its result must be visible before waiting for arrivals.
	s, _, _ := canonicalPromotionReadFixture(t)
	const session = "early-cow-refusal"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"registered-cow-negative","version":"1"}}}`)))
	s.NoteSessionToolPolicy(session, "all", "defer")
	require.True(t, s.IsToolEnabledForSession(ctx, "get_file_summary"))
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	result, err := registeredCOWCall(s, ctx, 80, "get_file_summary", map[string]any{"path": "main.go", "format": "json"})
	require.True(t, err != nil || result != nil && result.IsError, "the controlled request cannot produce fresh rows")
	answers := make(chan registeredCOWReply, 1)
	answers <- registeredCOWReply{tool: "get_file_summary", result: result, err: err, remaining: -time.Millisecond}
	released := false
	_, failure := awaitRegisteredCOWMetadata(t.Context(), make(chan *projectionEntry), answers, 4, func() { released = true }, nil)
	require.True(t, released, "every diagnostic exit releases the owned barrier")
	require.ErrorContains(t, failure, "get_file_summary returned before metadata", "the first public failure must not be replaced by a later barrier timeout")
	require.ErrorContains(t, failure, "remaining_original_budget=-1ms")
}

func TestRegisteredCOWBarrierKeepsCompletedRawReadAndCapturedOwners(t *testing.T) {
	captures := make(chan *projectionEntry, 4)
	owner := &projectionEntry{}
	for range 4 {
		captures <- owner
	}
	answers := make(chan registeredCOWReply, 1)
	answers <- registeredCOWReply{tool: "read_file", result: mcplib.NewToolResultText("actual current raw bytes")}
	released := false
	seen := 0
	early, err := awaitRegisteredCOWMetadata(t.Context(), captures, answers, 4, func() { released = true }, func(entry *projectionEntry) { require.Same(t, owner, entry); seen++ })
	require.NoError(t, err)
	require.True(t, released)
	require.Equal(t, 4, seen)
	// Whichever select branch ran first, the raw answer is preserved exactly once.
	if len(early) == 0 {
		early = append(early, <-answers)
	}
	require.Len(t, early, 1)
	require.Equal(t, "read_file", early[0].tool)
}

func TestRegisteredCOWBarrierCallerCancellationReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	released := false
	_, err := awaitRegisteredCOWMetadata(ctx, make(chan *projectionEntry), make(chan registeredCOWReply), 4, func() { released = true }, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, released)
}
