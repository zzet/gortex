package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// A real current-file extraction exhausts the original private allowance. The
// requested new definition exists on disk but is absent from canonical state;
// an unavailable view must not certify its absence or serve old source ranges.
func TestRegisteredSourceMissAfterRealProjectionDeferralIsTyped(t *testing.T) {
	selectedProjectionRegistry(t)
	e := &pendingBudgetExtractor{base: languages.NewGoExtractor(), entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	reg := parser.NewRegistry()
	reg.Register(e)
	s, root := setupTestServerWithRegistry(t, reg)
	s.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(root, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	after := append(append([]byte(nil), before...), []byte("\nfunc SourceBeyondDeadline() {}\n")...)
	require.NoError(t, os.WriteFile(path, after, 0o600))
	require.Nil(t, s.engine.GetSymbol("main.go::SourceBeyondDeadline"))
	e.hold.Store(true)
	release := sync.OnceFunc(func() { close(e.release) })
	caller, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	type answer struct {
		result *mcplib.CallToolResult
		err    error
	}
	done := make(chan answer, 1)
	joined := false
	leaf, ok := s.facades.legacy("get_symbol_source")
	require.True(t, ok)
	var observed context.Context
	s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		result, callErr := leaf.handler(ctx, req)
		observed = ctx
		return result, callErr
	})
	t.Cleanup(func() {
		cancel()
		release()
		if !joined {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("owned registered source read did not join")
			}
		}
		select {
		case <-e.entered:
			select {
			case <-e.finished:
			case <-time.After(3 * time.Second):
				t.Error("owned extraction did not join")
			}
		default:
		}
	})
	go func() {
		result, callErr := externalVersionRegisteredCall(s, caller, 91, "read", map[string]any{"operation": "source", "target": map[string]any{"symbol": "main.go::SourceBeyondDeadline"}, "output": map[string]any{"format": "json", "max_bytes": 0}})
		done <- answer{result, callErr}
	}()
	select {
	case <-e.entered:
	case <-caller.Done():
		t.Fatal("actual source projection never entered extractor")
	}
	var got answer
	select {
	case got = <-done:
		joined = true
	case <-caller.Done():
		t.Fatal("source read waited beyond original projection allowance")
	}
	require.NoError(t, got.err)
	require.NoError(t, caller.Err())
	require.NotNil(t, observed)
	require.Equal(t, "external_write", requestDeferredProjectionFiles(observed)["main.go"])
	require.Empty(t, requestProjectionLayers(observed))
	deadline, ok := observed.Value(projectedSourceDeadlineKey{}).(time.Time)
	require.True(t, ok)
	require.False(t, time.Now().Before(deadline), "real private allowance must have expired")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, after, raw)
	require.Nil(t, s.engine.GetSymbol("main.go::SourceBeyondDeadline"), "read must not publish canonical state")
	requireReadFreshnessContract(t, got.result, "file_view_pending")
	var body StructuredError
	require.NoError(t, json.Unmarshal([]byte(resultText(got.result)), &body))
	require.Equal(t, "projection_deferred", body.Data["reason"])
	require.NotContains(t, resultText(got.result), "func SourceBeyondDeadline", "refusal carries no source payload")
	t.Log("REAL_SOURCE_DEFERRAL_VERIFIED caller_live=true original_private_allowance_expired=true physical_target_present=true canonical_target_absent=true")
}
