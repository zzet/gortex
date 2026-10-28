package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

// TestTrackRepositoryDetachesFirstIndexFromRequestLifetime pins #326.
//
// The first index of a fresh repo ran inside the MCP request context and
// registration only landed *after* it returned, so a request that hit the
// daemon's 60s lifetime cancelled the index and left the repo untracked —
// a retry then repeated the cycle. MCP-driven tracking therefore only ever
// worked for repos that fully indexed inside the deadline.
//
// A already-cancelled request context reproduces that deterministically: with
// the index bound to it, TrackRepoCtx fails immediately and nothing registers.
func TestTrackRepositoryDetachesFirstIndexFromRequestLifetime(t *testing.T) {
	srv, mi := newWorktreeMCPServer(t)
	require.Empty(t, mi.AllMetadata(), "fixture must start with no tracked repo")

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib.go"),
		[]byte("package lib\n\nfunc Tracked() {}\n"), 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the daemon ended the request lifetime

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": repo}

	res, err := srv.handleTrackRepository(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError,
		"a spent request context must not abort tracking: %+v", res.Content)

	var payload struct {
		Status string `json:"status"`
		Prefix string `json:"prefix"`
	}
	require.NoError(t, json.Unmarshal(
		[]byte(extractTextFromContent(t, res.Content)), &payload))
	require.Equal(t, "tracked", payload.Status)
	require.NotEmpty(t, payload.Prefix)

	// The detached index must have registered the repo, so a later call finds
	// it instead of starting over.
	require.Len(t, mi.AllMetadata(), 1,
		"the first index must register the repo even though the request was cancelled")

	second, err := srv.handleTrackRepository(context.Background(), req)
	require.NoError(t, err)
	require.False(t, second.IsError)
	// The property is that the second call finds the repo instead of starting
	// over. Whether it reports the index as finished or still running depends
	// on whether the detached first index has completed yet — which is exactly
	// what "detached" leaves unspecified, so pinning one of the two wordings
	// makes this assertion a race against the machine rather than a check on
	// the handler.
	secondText := extractTextFromContent(t, second.Content)
	require.True(t,
		strings.Contains(secondText, "already tracked") ||
			strings.Contains(secondText, "already running"),
		"the second call must recognise the repo rather than start over: %s", secondText)
}

// TestTrackRepositoryAnswersBeforeTheRequestDeadline pins the second half of
// #326: rather than letting the daemon abort the request, the handler answers
// `accepted` while the index keeps running server-side. A deadline that has
// already passed leaves no room to wait, so the answer must come back
// immediately and must not be an error.
func TestTrackRepositoryAnswersBeforeTheRequestDeadline(t *testing.T) {
	srv, mi := newWorktreeMCPServer(t)

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib.go"),
		[]byte("package lib\n\nfunc Deadlined() {}\n"), 0o644))

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": repo}

	res, err := srv.handleTrackRepository(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError,
		"an expired deadline must yield an answer, not a failure: %+v", res.Content)

	var payload struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(
		[]byte(extractTextFromContent(t, res.Content)), &payload))
	// Either the index beat the (already spent) timer or the handler answered
	// `accepted`; both are correct, and neither may be an error or a hang.
	require.Contains(t, []string{"accepted", "tracked"}, payload.Status)

	// Whichever branch answered, the work must still land — and this test must
	// not return while the detached goroutine is still writing (it persists
	// config and re-runs analysis after registering), or it would race the
	// tests that follow. A second track call reports "already tracked" only
	// once that goroutine released its in-flight guard, so polling it is both
	// the assertion and the barrier.
	require.Eventually(t, func() bool {
		follow, followErr := srv.handleTrackRepository(context.Background(), req)
		return followErr == nil && follow != nil && !follow.IsError &&
			strings.Contains(extractTextFromContent(t, follow.Content), "already tracked")
	}, 30*time.Second, 25*time.Millisecond,
		"the detached first index must register the repo and finish cleanly")
	require.Len(t, mi.AllMetadata(), 1)
}

func TestTrackRepositoryPrefixCollisionNamesMCPParameter(t *testing.T) {
	srv, mi := newWorktreeMCPServer(t)
	first, second := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	for _, path := range []string{first, second} {
		require.NoError(t, os.MkdirAll(path, 0o755))
	}
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
	require.NoError(t, err)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": second}
	res, err := srv.handleTrackRepository(t.Context(), req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	text := extractTextFromContent(t, res.Content)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &payload))
	require.Contains(t, payload["message"], "name parameter")
	require.NotContains(t, payload["message"], "gortex track --name")
	require.Equal(t, "prefix_collision", payload["error_code"])
	require.Equal(t, first, payload["owner_path"], "compare decoded paths, including Windows JSON escapes")
	require.NoError(t, os.RemoveAll(first))
	res, err = srv.handleTrackRepository(t.Context(), req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	text = extractTextFromContent(t, res.Content)
	require.Contains(t, text, "no longer exists")
	require.Contains(t, text, "untrack_repository")
}

func TestTrackRepositoryConfiguredNameChangeRequiresConfigEdit(t *testing.T) {
	srv, mi := newWorktreeMCPServer(t)
	root := t.TempDir()
	require.NoError(t, srv.configManager.Global().AddRepo(config.RepoEntry{Path: root, Name: "src"}))
	require.NoError(t, srv.configManager.Global().Save())
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": root, "name": "other-src"}
	res, err := srv.handleTrackRepository(t.Context(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "%+v", res.Content)
	require.Contains(t, extractTextFromContent(t, res.Content), "already configured; set name: other-src")
	require.Empty(t, mi.AllMetadata(), "refusal must precede indexing")
	require.Equal(t, "src", srv.configManager.Global().FindRepoByPath(root).Name)
}
