package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
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
			_, logged := d.loggedUntracked.Load(sess.ID)
			assert.False(t, logged, "transient discovery is not an untracked session")
		})
	}
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
	assert.Contains(t, instructions, "view_building")
	assert.Contains(t, instructions, "No graph access has been granted")
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
