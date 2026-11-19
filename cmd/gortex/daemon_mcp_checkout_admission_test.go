package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/toolref"
)

func TestDispatcher_CheckoutAdmissionFailurePreservesClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		code      string
		retryable bool
	}{
		{"busy", fmt.Errorf("observe checkout: %w", indexer.ErrCheckoutMutationBusy), "view_building", true},
		{"deadline", fmt.Errorf("catalog admission: %w", context.DeadlineExceeded), "view_building", true},
		{"sqlite_busy", checkoutAdmissionSQLiteError(5), "view_building", true},
		{"sqlite_locked_sharedcache", checkoutAdmissionSQLiteError(6 | (1 << 8)), "view_building", true},
		{"sqlite_ioerr", checkoutAdmissionSQLiteError(10), "checkout_inaccessible", false},
		{"committed_busy", checkoutAdmissionCommittedError{checkoutAdmissionSQLiteError(5)}, "checkout_inaccessible", false},
		{"inaccessible", errors.New("Git metadata cannot be read"), "checkout_inaccessible", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := trackedPathMCPSetup(t, t.TempDir())
			sess := &daemon.Session{ID: "checkout-admission", CWD: t.TempDir()}
			d.checkoutCWDProbe = func(_ context.Context, cwd string) (bool, error) {
				require.Equal(t, sess.CWD, cwd)
				// Even a positive proof does not authorize access on error.
				return true, tc.err
			}
			for _, frame := range []string{
				`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"search","arguments":{"query":"private"}}}`,
				`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"workspace_admin","arguments":{"operation":"track"}}}`,
				`{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"gortex://graph"}}`,
			} {
				reply, err := d.Dispatch(context.Background(), sess, []byte(frame))
				require.NoError(t, err)
				var parsed map[string]any
				require.NoError(t, json.Unmarshal(reply, &parsed))
				assert.EqualValues(t, 7, parsed["id"])
				assert.NotContains(t, parsed, "result", "failed admission cannot expose graph data")
				failure, ok := parsed["error"].(map[string]any)
				require.True(t, ok, "wire error missing: %s", reply)
				data, ok := failure["data"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.code, data["error_code"])
				assert.Equal(t, tc.retryable, data["retryable"])
				assert.Equal(t, sess.CWD, data["path"])
				assert.Contains(t, failure["message"], tc.err.Error(), "preserve the cause")
				assert.NotContains(t, string(reply), "gortex track")
				assert.NotContains(t, string(reply), "repo_not_tracked")
			}
			reply, err := d.Dispatch(context.Background(), sess, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`))
			require.NoError(t, err)
			var init struct {
				Result struct {
					Instructions string `json:"instructions"`
				} `json:"result"`
			}
			require.NoError(t, json.Unmarshal(reply, &init))
			if tc.retryable {
				assert.True(t, strings.HasPrefix(init.Result.Instructions, "Gortex is LOADING checkout "), init.Result.Instructions)
			} else {
				assert.True(t, strings.HasPrefix(init.Result.Instructions, "Gortex checkout discovery is unavailable"), init.Result.Instructions)
				assert.Contains(t, init.Result.Instructions, "No graph access has been granted")
			}
			_, logged := d.loggedUntracked.Load(sess.ID)
			assert.False(t, logged, "transient discovery is not an untracked session")
		})
	}
}

// Hooks and agents key on this refusal to tell one pending worktree from an
// unavailable daemon.
func TestCheckoutAdmissionErrorPendingDiscoveryGolden(t *testing.T) {
	err := fmt.Errorf("observe checkout: %w", &indexer.CheckoutDiscoveryPendingError{Root: "/repos/app-wt", Family: "family-0123", Reason: "family_capacity"})
	got := checkoutAdmissionError("/repos/app-wt/internal", []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call"}`), err)
	const want = `{"error":{"code":-32000,"data":{"error_code":"view_building","family":"family-0123","path":"/repos/app-wt","reason":"family_capacity","retryable":true,"state":"discovery_pending"},` +
		`"message":"checkout discovery unavailable: observe checkout: indexer: checkout mutation lane is busy; retry: checkout discovery is pending for /repos/app-wt (family family-0123, reason family_capacity)"},"id":7,"jsonrpc":"2.0"}`
	require.Equal(t, want, string(got))

	// A refusal that names no checkout root keeps the session's path.
	got = checkoutAdmissionError("/repos/app-wt/internal", []byte(`{"jsonrpc":"2.0","id":8,"method":"tools/call"}`), &indexer.CheckoutDiscoveryPendingError{Reason: "in_progress"})
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(got, &parsed))
	require.Equal(t, "/repos/app-wt/internal", errorData(t, parsed)["path"], "%s", got)
}

type checkoutAdmissionSQLiteError int

func (e checkoutAdmissionSQLiteError) Error() string { return fmt.Sprintf("SQLite error %d", e) }
func (e checkoutAdmissionSQLiteError) Code() int     { return int(e) }

type checkoutAdmissionCommittedError struct{ error }

func (e checkoutAdmissionCommittedError) Committed() bool { return true }
func (e checkoutAdmissionCommittedError) Unwrap() error   { return e.error }

func TestDispatcher_CheckoutAdmissionBusyHandshakeAndSameSessionRetry(t *testing.T) {
	d, _ := trackedPathMCPSetup(t, t.TempDir())
	sess := &daemon.Session{ID: "checkout-admission-retry", CWD: t.TempDir()}
	busy := true
	d.checkoutCWDProbe = func(_ context.Context, _ string) (bool, error) {
		if busy {
			return false, indexer.ErrCheckoutMutationBusy
		}
		return true, nil
	}
	dispatch := func(frame string) map[string]any {
		t.Helper()
		reply, err := d.Dispatch(context.Background(), sess, []byte(frame))
		require.NoError(t, err)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal(reply, &parsed))
		return parsed
	}
	init := dispatch(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`)
	assert.NotContains(t, init, "error", "busy discovery must not break the connection")
	result, ok := init["result"].(map[string]any)
	require.True(t, ok)
	instructions, ok := result["instructions"].(string)
	require.True(t, ok)
	// The session reads these once and keeps them after admission, so they
	// must carry the ordinary instructions, not a "no access" verdict.
	assert.True(t, strings.HasPrefix(instructions, gortexmcp.ServerInstructionsCheckoutLoading(sess.CWD, "view_building")), instructions)
	assert.Contains(t, instructions, toolref.TransportClause(), "ordinary instructions follow the preamble")
	assert.NotContains(t, instructions, "No graph access")
	assert.NotContains(t, instructions, "gortex track")

	listed := dispatch(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	assert.NotContains(t, listed, "error")
	result, ok = listed["result"].(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, result["tools"], "stable tool metadata remains discoverable")

	const call = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"capabilities","arguments":{}}}`
	blocked := dispatch(call)
	require.Contains(t, blocked, "error")
	busy = false
	reachable, err := d.cwdReachableChecked(context.Background(), sess.CWD)
	require.NoError(t, err)
	require.True(t, reachable)
	retried := dispatch(call)
	assert.NotContains(t, retried, "error", "the same session must retry without reconnect or tracking")
	assert.Contains(t, retried, "result")
	_, logged := d.loggedUntracked.Load(sess.ID)
	assert.False(t, logged, "busy state must not poison the untracked-session cache")
}

func BenchmarkCheckoutAdmissionError(b *testing.B) {
	frame := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"search"}}`)
	b.ReportAllocs()
	for b.Loop() {
		_ = checkoutAdmissionError("/checkout", frame, indexer.ErrCheckoutMutationBusy)
	}
}

// The daemon rewrite must keep the base the session's own initialize chose:
// a legacy client told the facade workflow names tools it does not have.
func TestDispatcher_CheckoutAdmissionBusyKeepsSessionBase(t *testing.T) {
	d, _ := trackedPathMCPSetup(t, t.TempDir())
	d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, indexer.ErrCheckoutMutationBusy }
	frames := map[string]string{
		"initialize":      `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`,
		"server/discover": `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"antigravity-client","version":"v1.0.0"}}}}`,
	}
	for _, surface := range []struct{ spec, base string }{
		{"", "When Gortex can serve the step"},
		{"core", "Gortex is a code-intelligence graph server"},
	} {
		for method, frame := range frames {
			t.Run(surface.spec+"/"+method, func(t *testing.T) {
				sess := &daemon.Session{ID: "busy-base-" + surface.spec + method, CWD: t.TempDir(), ToolSpec: surface.spec}
				d.SessionStarted(sess, func([]byte) error { return nil })
				t.Cleanup(func() { d.SessionEnded(sess) })
				reply, err := d.Dispatch(context.Background(), sess, []byte(frame))
				require.NoError(t, err)
				var parsed struct {
					Result struct {
						Instructions string `json:"instructions"`
					} `json:"result"`
				}
				require.NoError(t, json.Unmarshal(reply, &parsed), "%s", reply)
				want := gortexmcp.ServerInstructionsCheckoutLoading(sess.CWD, "view_building") + "\n\n" + surface.base
				assert.True(t, strings.HasPrefix(parsed.Result.Instructions, want), "%s", parsed.Result.Instructions)
			})
		}
	}
}
