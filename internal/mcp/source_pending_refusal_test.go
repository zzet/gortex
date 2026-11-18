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

// The original private allowance can expire before extractor admission or
// during extraction. Both real deferrals must preserve the same typed refusal;
// neither may certify canonical absence or serve old source ranges.
func TestRegisteredSourceMissAfterRealProjectionDeferralIsTyped(t *testing.T) {
	for _, mode := range []string{"ordinary_admission", "spent_before_admission", "cancelled_caller"} {
		t.Run(mode, func(t *testing.T) { registeredSourceProjectionDeferral(t, mode) })
	}
}

func registeredSourceProjectionDeferral(t *testing.T, mode string) {
	t.Helper()
	selectedProjectionRegistry(t)
	e := &pendingBudgetExtractor{base: languages.NewGoExtractor(), entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	reg := parser.NewRegistry()
	reg.Register(e)
	s, root := setupTestServerWithRegistry(t, reg)
	s.SetWatcher(mutationTestWatcher{})
	const session = "source-pending-facade"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	base := s.WithClientSession(WithSessionID(t.Context(), session), session)
	s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load","version":"1"}}}`)))
	s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	path := filepath.Join(root, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	after := append(append([]byte(nil), before...), []byte("\nfunc SourceBeyondDeadline() {}\n")...)
	require.NoError(t, os.WriteFile(path, after, 0o600))
	require.Nil(t, s.engine.GetSymbol("main.go::SourceBeyondDeadline"))
	e.hold.Store(true)
	release := sync.OnceFunc(func() { close(e.release) })
	caller, cancel := context.WithTimeout(base, 3*time.Second)
	if mode != "ordinary_admission" {
		// Spend an actual production-sized allowance before dispatch. This
		// controls the valid no-extractor branch without backdating a clock,
		// extending the product budget or manufacturing deferral facts.
		deadline := time.Now().Add(projectedReadBudget)
		caller = context.WithValue(caller, projectedSourceDeadlineKey{}, deadline)
		timer := time.NewTimer(time.Until(deadline))
		<-timer.C
		if mode == "cancelled_caller" {
			cancel()
		}
	}
	type answer struct {
		result   *mcplib.CallToolResult
		err      error
		returned time.Time
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
		done <- answer{result: result, err: callErr, returned: time.Now()}
	}()
	var got answer
	select {
	case got = <-done:
		joined = true
	case <-time.After(3 * time.Second):
		t.Fatal("owned registered source read did not return")
	}
	if mode == "cancelled_caller" {
		require.ErrorIs(t, caller.Err(), context.Canceled)
		require.True(t, got.err != nil || got.result != nil && got.result.IsError)
		if got.result != nil {
			require.NotContains(t, resultText(got.result), "projection_deferred", "a cancelled caller has no live private-budget refusal")
		}
		require.True(t, e.hold.Load(), "cancelled dispatch must not admit extraction")
		return
	}
	require.NoError(t, got.err)
	require.NoError(t, caller.Err())
	require.NotNil(t, observed)
	require.Equal(t, "external_write", requestDeferredProjectionFiles(observed)["main.go"])
	require.Empty(t, requestProjectionLayers(observed))
	deadline, ok := observed.Value(projectedSourceDeadlineKey{}).(time.Time)
	require.True(t, ok)
	require.False(t, got.returned.Before(deadline), "real private allowance must have expired before the registered response returned")
	stage := "before_extractor_admission"
	select {
	case <-e.entered:
		stage = "extracting"
	default:
		require.True(t, e.hold.Load(), "no-entry branch needs positive evidence extraction was not admitted")
	}
	if mode == "spent_before_admission" {
		require.Equal(t, "before_extractor_admission", stage)
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, after, raw)
	require.Nil(t, s.engine.GetSymbol("main.go::SourceBeyondDeadline"), "read must not publish canonical state")
	t.Logf("REAL_SOURCE_DEFERRAL_VERIFIED stage=%s caller_live=true original_private_allowance_expired=true physical_target_present=true canonical_target_absent=true result=%s", stage, resultText(got.result))
	requireReadFreshnessContract(t, got.result, "file_view_pending")
	var body StructuredError
	require.NoError(t, json.Unmarshal([]byte(resultText(got.result)), &body))
	require.Equal(t, "projection_deferred", body.Data["reason"])
	require.NotContains(t, resultText(got.result), "func SourceBeyondDeadline", "refusal carries no source payload")
}

// Negative marker controls exercise classification only. The positive contract
// above requires an actual unavailable projection and an actual registered wire.
func TestSourcePendingRefusalRejectsUnprovenOrUnauthorizedMisses(t *testing.T) {
	s, root := setupTestServer(t)
	for _, name := range []string{"canonical_miss", "wrong_file", "unknown_reason", "store_busy_only", "cancelled", "expired_caller", "wrong_tool", "compressed", "malformed_options", "outside_root", "non_normalized", "out_of_scope", "complete_selected_layer"} {
		t.Run(name, func(t *testing.T) {
			ctx := withRequestProjectionFacts(t.Context())
			id := "main.go::Absent"
			req := mcplib.CallToolRequest{Params: mcplib.CallToolParams{Name: "get_symbol_source", Arguments: map[string]any{"id": id}}}
			switch name {
			case "canonical_miss":
			case "wrong_file":
				markRequestProjectionDeferred(ctx, "other.go", "external_write")
			case "unknown_reason":
				markRequestProjectionDeferred(ctx, "main.go", "unknown")
			case "store_busy_only":
				ctx = markRequestStoreBusy(ctx, "main.go")
			default:
				markRequestProjectionDeferred(ctx, "main.go", "external_write")
			}
			switch name {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "expired_caller":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			case "wrong_tool":
				req.Params.Name = "get_file_summary"
			case "compressed":
				req.Params.Arguments.(map[string]any)["compress_bodies"] = true
			case "malformed_options":
				req.Params.Arguments.(map[string]any)["options"] = "invalid"
			case "outside_root":
				id = filepath.Join(filepath.Dir(root), "outside.go") + "::Absent"
				markRequestProjectionDeferred(ctx, filepath.Join(filepath.Dir(root), "outside.go"), "external_write")
			case "non_normalized":
				id = "nested/../main.go::Absent"
				markRequestProjectionDeferred(ctx, "nested/../main.go", "external_write")
			case "out_of_scope":
				session := s.sessionFor(ctx)
				session.mu.Lock()
				session.scopeResolved = true
				session.scopeBound = true
				session.scopeWorkspaceID = "other-workspace"
				session.mu.Unlock()
				defer func() { session.mu.Lock(); session.resetScopeLocked(); session.mu.Unlock() }()
			case "complete_selected_layer":
				file := filepath.Join(root, "main.go")
				raw, err := os.ReadFile(file)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(file, append(raw, []byte("\nfunc CompleteSelectedLayer() {}\n")...), 0o600))
				projected, err := s.ensureProjected(ctx, []string{"main.go"})
				require.NoError(t, err)
				require.NotEmpty(t, requestProjectionLayers(projected))
				ctx = projected
			}
			require.Nil(t, s.pendingSourceMiss(ctx, req, id), "only an exact authorized unavailable-file observation may become a typed source refusal")
		})
	}
	// Real canonical absence remains the ordinary not-found response.
	result, err := s.handleGetSymbolSource(t.Context(), mcplib.CallToolRequest{Params: mcplib.CallToolParams{Name: "get_symbol_source", Arguments: map[string]any{"id": "main.go::Absent"}}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.False(t, metadataTransientRefusal(result))
	require.Contains(t, resultText(result), "symbol not found")
}

func TestSourcePendingRefusalPreservesCheckedRemovalAndSuccess(t *testing.T) {
	s, _, ctx, _, _ := snapshotFacadeFixture(t)
	for _, tc := range []struct {
		id      string
		refused bool
	}{{"main.go::TargetBefore", true}, {"main.go::Target", false}} {
		result := snapshotRegisteredCall(t, s, ctx, 95, "read", map[string]any{"operation": "source", "target": map[string]any{"symbol": tc.id}, "output": map[string]any{"format": "json", "max_bytes": 0}})
		require.Equal(t, tc.refused, result.IsError, resultText(result))
		require.False(t, metadataTransientRefusal(result))
		if tc.refused {
			require.Contains(t, resultText(result), "removed by pending edit")
		} else {
			require.Contains(t, resultText(result), "func Target()")
		}
	}
}
