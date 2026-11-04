package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

func frameMetaCWD(t *testing.T, frame []byte) (string, bool) {
	t.Helper()
	var msg struct {
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	require.NoError(t, json.Unmarshal(frame, &msg))
	cwd, ok := msg.Params.Meta["cwd"].(string)
	return cwd, ok
}

func TestStampClientCWD(t *testing.T) {
	const cwd = "/work/repo/.worktrees/feature"

	t.Run("tools/call gains _meta.cwd and keeps its arguments and framing", func(t *testing.T) {
		in := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"edit","arguments":{"path":"a.go"}}}` + "\n")
		out := stampClientCWD(in, cwd)

		require.Equal(t, byte('\n'), out[len(out)-1], "line framing must survive")
		got, ok := frameMetaCWD(t, out)
		require.True(t, ok)
		assert.Equal(t, cwd, got)

		var msg struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(out, &msg))
		assert.Equal(t, 3, msg.ID)
		assert.Equal(t, "tools/call", msg.Method)
		assert.Equal(t, "edit", msg.Params.Name)
		assert.Equal(t, map[string]any{"path": "a.go"}, msg.Params.Arguments)
	})

	t.Run("existing _meta keys are preserved", func(t *testing.T) {
		in := []byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"x","_meta":{"progressToken":9}}}`)
		out := stampClientCWD(in, cwd)
		var msg struct {
			Params struct {
				Meta map[string]any `json:"_meta"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(out, &msg))
		assert.EqualValues(t, 9, msg.Params.Meta["progressToken"])
		assert.Equal(t, cwd, msg.Params.Meta["cwd"])
	})

	t.Run("tools/call without params gains params._meta.cwd", func(t *testing.T) {
		out := stampClientCWD([]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call"}`), cwd)
		got, ok := frameMetaCWD(t, out)
		require.True(t, ok)
		assert.Equal(t, cwd, got)
	})

	unchanged := map[string]string{
		"client-supplied cwd wins": `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"x","_meta":{"cwd":"/elsewhere"}}}`,
		"other methods":            `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`,
		"notifications":            `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		"array params":             `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":[1,2]}`,
		"non-object _meta":         `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"_meta":"x"}}`,
		"malformed JSON":           `{"jsonrpc":"2.0","id":10,"method":"tools/call"`,
	}
	for name, frame := range unchanged {
		t.Run(name+" is left untouched", func(t *testing.T) {
			in := []byte(frame + "\n")
			assert.Equal(t, in, stampClientCWD(in, cwd))
		})
	}

	t.Run("empty cwd is a no-op", func(t *testing.T) {
		in := []byte(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{}}`)
		assert.Equal(t, in, stampClientCWD(in, ""))
	})
}

func TestLiveClientCWD(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, dir, liveClientCWD(func() string { return dir + "/." }))
	assert.Empty(t, liveClientCWD(nil))
	assert.Empty(t, liveClientCWD(func() string { return "" }))
	assert.Empty(t, liveClientCWD(func() string { return "relative/dir" }))
	assert.Empty(t, liveClientCWD(func() string { return "/" }), "a root says nothing about the project")
}

// TestDispatcher_LiveClientCWDOverridesHandshake pins the daemon half: a
// tools/call stamped with `_meta.cwd` resolves against that directory, and
// later frames without the stamp keep it, while the handshake CWD — which
// reconnect resume matches on — is never rewritten.
func TestDispatcher_LiveClientCWDOverridesHandshake(t *testing.T) {
	tracked := t.TempDir()
	untracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	sess := &daemon.Session{ID: "sess_live_cwd", CWD: tracked}
	stamped := stampClientCWD([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`), untracked)

	reply, err := d.Dispatch(context.Background(), sess, stamped)
	require.NoError(t, err)
	assertNotTrackedAt(t, reply, untracked)
	assert.Equal(t, tracked, sess.CWD, "handshake CWD must stay put for reconnect resume")
	assert.Equal(t, untracked, sess.EffectiveCWD())

	// The next call carries no stamp (a non-Linux proxy, or an older one):
	// the session stays where the client last said it was.
	reply, err = d.Dispatch(context.Background(), sess, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`))
	require.NoError(t, err)
	assertNotTrackedAt(t, reply, untracked)
}

func TestDispatcher_LiveClientCWDReturnsToTrackedRepo(t *testing.T) {
	tracked := t.TempDir()
	untracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	// Launched outside any tracked repo, then moved into one.
	sess := &daemon.Session{ID: "sess_live_cwd_back", CWD: untracked}
	stamped := stampClientCWD([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`), tracked)

	reply, err := d.Dispatch(context.Background(), sess, stamped)
	require.NoError(t, err)
	require.NotNil(t, reply)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			assert.NotEqual(t, "repo_not_tracked", data["error_code"], "live CWD in a tracked repo was refused: %v", parsed)
		}
	}
}

// TestRequestCWD_FrameStampWins: a frame resolves against its own stamp even
// when another frame on the session recorded a different live CWD after it
// arrived — the concurrency case where two stamped calls interleave.
func TestRequestCWD_FrameStampWins(t *testing.T) {
	sess := &daemon.Session{CWD: "/launch"}
	frameA := stampClientCWD([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`), "/launch")
	sess.SetLiveCWD("/launch/.worktrees/b") // frame B landed in between

	assert.Equal(t, "/launch", requestCWD(sess, frameA))
	assert.Equal(t, "/launch/.worktrees/b", requestCWD(sess, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)),
		"an unstamped frame uses the session's live CWD")
	assert.Equal(t, "/launch/.worktrees/b", requestCWD(sess, []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"_meta":{"cwd":"relative"}}}`)),
		"an invalid stamp is ignored")
}

func assertNotTrackedAt(t *testing.T, reply []byte, path string) {
	t.Helper()
	require.NotNil(t, reply)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	errObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "expected an error reply: %v", parsed)
	data, ok := errObj["data"].(map[string]any)
	require.True(t, ok, "expected error.data: %v", parsed)
	assert.Equal(t, "repo_not_tracked", data["error_code"])
	assert.Equal(t, path, data["path"], "the refusal must name the live CWD, not the handshake CWD")
}
