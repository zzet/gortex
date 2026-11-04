package daemon

import (
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionLiveCWD(t *testing.T) {
	s := &Session{CWD: "/launch"}
	assert.Equal(t, "/launch", s.EffectiveCWD(), "no report yet: the handshake CWD")

	s.SetLiveCWD("relative/path")
	s.SetLiveCWD("")
	assert.Equal(t, "/launch", s.EffectiveCWD(), "unusable reports are ignored")

	s.SetLiveCWD("/launch/.worktrees/feature/")
	assert.Equal(t, "/launch/.worktrees/feature", s.EffectiveCWD())
	assert.Equal(t, "/launch", s.CWD, "the handshake CWD is never rewritten")

	var nilSession *Session
	nilSession.SetLiveCWD("/x")
	assert.Empty(t, nilSession.EffectiveCWD())
}

// TestSessionLiveCWD_ReconnectResumes pins why the live CWD is held apart
// from CWD: a proxy reconnects with its original handshake, and Register only
// resumes the logical session when that handshake CWD still matches.
func TestSessionLiveCWD_ReconnectResumes(t *testing.T) {
	r := NewSessionRegistry()
	h := Handshake{Mode: ModeMCP, PID: 4242, LogicalSessionID: "logical-live-cwd", CWD: "/launch"}

	c1, c1Peer := net.Pipe()
	defer c1.Close()
	defer c1Peer.Close()
	first := r.Register(c1, h)
	first.SetLiveCWD("/launch/.worktrees/feature")

	c2, c2Peer := net.Pipe()
	defer c2.Close()
	defer c2Peer.Close()
	second := r.Register(c2, h)

	require.Same(t, first, second, "reconnect must resume the same session")
	assert.Equal(t, "/launch/.worktrees/feature", second.EffectiveCWD())
}

// TestReplayIdentityIgnoresMetaCWD: the proxy's cwd stamp is routing, not
// request content, so a retry re-stamped after the host moved still replays.
func TestReplayIdentityIgnoresMetaCWD(t *testing.T) {
	base := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"edit","arguments":{"path":"a.go"}}}`
	atMain := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"edit","arguments":{"path":"a.go"},"_meta":{"cwd":"/repo"}}}`
	atWorktree := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"edit","arguments":{"path":"a.go"},"_meta":{"cwd":"/repo/.worktrees/x"}}}`
	keepsOtherMeta := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"edit","arguments":{"path":"a.go"},"_meta":{"cwd":"/repo","progressToken":1}}}`
	otherArgs := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"edit","arguments":{"path":"b.go"},"_meta":{"cwd":"/repo"}}}`

	digest := func(frame string) [32]byte {
		t.Helper()
		id, ok := parseMCPReplayIdentity([]byte(frame))
		require.True(t, ok)
		return id.digest
	}
	assert.Equal(t, digest(base), digest(atMain))
	assert.Equal(t, digest(atMain), digest(atWorktree))
	assert.NotEqual(t, digest(base), digest(keepsOtherMeta), "other _meta keys still count")
	assert.NotEqual(t, digest(atMain), digest(otherArgs), "arguments still count")
}

func TestSessionLiveCWD_ConcurrentAccess(t *testing.T) {
	s := &Session{CWD: "/launch"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.SetLiveCWD("/a") }()
		go func() { defer wg.Done(); _ = s.EffectiveCWD() }()
	}
	wg.Wait()
	assert.Equal(t, "/a", s.EffectiveCWD())
}
