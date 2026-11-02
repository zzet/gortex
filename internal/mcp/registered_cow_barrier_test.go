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

// Initially retain the old barrier's capture-only behavior as a regression
// witness. The diagnostic control exposes a real early completion it ignores.
func awaitRegisteredCOWMetadata(ctx context.Context, captured <-chan *projectionEntry, answers <-chan registeredCOWReply, want int, release func()) ([]registeredCOWReply, error) {
	defer release()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for range want {
		select {
		case <-captured:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, errors.New("owned registered reads did not reach the common metadata barrier")
		}
	}
	return nil, nil
}

func TestRegisteredCOWBarrierReportsEarlyRefusalAndReleases(t *testing.T) {
	// A real registered SDK call with original caller cancellation cannot reach
	// the metadata hook. Its result must be visible before waiting for arrivals.
	s, _, _ := canonicalPromotionReadFixture(t)
	ctx := guardE2ESession(t, s, "early-cow-refusal")
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	result, err := registeredCOWCall(s, ctx, 80, "get_file_summary", map[string]any{"path": "main.go", "format": "json"})
	require.True(t, err != nil || result != nil && result.IsError, "the controlled request cannot produce fresh rows")
	answers := make(chan registeredCOWReply, 1)
	answers <- registeredCOWReply{tool: "get_file_summary", result: result, err: err, remaining: -time.Millisecond}
	released := false
	_, failure := awaitRegisteredCOWMetadata(t.Context(), make(chan *projectionEntry), answers, 4, func() { released = true })
	require.True(t, released, "every diagnostic exit releases the owned barrier")
	require.ErrorContains(t, failure, "get_file_summary returned before metadata", "the first public failure must not be replaced by a later barrier timeout")
	require.ErrorContains(t, failure, "remaining_original_budget=-1ms")
}
