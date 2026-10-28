package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestMCPDeferredCollisionOutcome(t *testing.T) {
	s, mi := newWorktreeMCPServer(t)
	owner, loser := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	for _, root := range []string{owner, loser} {
		require.NoError(t, os.MkdirAll(root, 0o755))
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	mi.SetOnRepoTracked(func(_, root string) {
		if root == loser && calls.Add(1) == 1 {
			close(entered)
			<-release
		}
	})
	// Always release the real producer before fixture cleanup, including when
	// an assertion fails before the owner is installed.
	var released atomic.Bool
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	t.Cleanup(func() {
		unblock()
		s.trackInFlight.Range(func(_, value any) bool {
			select {
			case <-value.(*repositoryTrackJob).done:
			case <-time.After(10 * time.Second):
				t.Error("detached track did not finish after its barrier was released")
			}
			return true
		})
	})
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": loser}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(trackAcceptedHeadroom+time.Millisecond))
	defer cancel()
	first, err := s.handleTrackRepository(ctx, req)
	require.NoError(t, err)
	require.False(t, first.IsError)
	require.Contains(t, extractTextFromContent(t, first.Content), "accepted")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("detached track did not reach its real pre-registration barrier")
	}
	_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: owner})
	require.NoError(t, err)
	unblock()
	key := trackRequestKey(loser, config.RepoEntry{Path: loser})
	value, ok := s.trackInFlight.Load(key)
	require.True(t, ok)
	job := value.(*repositoryTrackJob)
	select {
	case <-job.done:
	case <-time.After(10 * time.Second):
		t.Fatal("detached track did not physically finish")
	}
	second, err := s.handleTrackRepository(t.Context(), req)
	require.NoError(t, err)
	require.True(t, second.IsError)
	payload := second.StructuredContent.(map[string]any)
	require.Equal(t, "prefix_collision", payload["error_code"])
	require.Equal(t, "src", payload["prefix"])
	require.Equal(t, owner, payload["owner_path"])
	require.Equal(t, "present", payload["owner_state"])
	require.Equal(t, int32(1), calls.Load(), "reading a stored collision starts no new track")
	// A remediation request has a different identity and must not replay the
	// previous collision for the same physical root.
	req.Params.Arguments = map[string]any{"path": loser, "name": "other-src"}
	third, err := s.handleTrackRepository(t.Context(), req)
	require.NoError(t, err)
	require.False(t, third.IsError, "%+v", third.Content)
	require.NotNil(t, mi.GetMetadata("other-src"))
	require.Equal(t, int32(2), calls.Load())
}

func TestTrackOutcomeExpiryNeverEvictsARunningProducer(t *testing.T) {
	s, mi := newWorktreeMCPServer(t)
	entry := config.RepoEntry{Path: t.TempDir()}
	key := trackRequestKey(entry.Path, entry)
	running := &repositoryTrackJob{done: make(chan struct{})}
	s.trackInFlight.Store(key, running)
	s.expireTrackOutcomes(time.Now().Add(time.Hour))
	value, found := s.trackInFlight.Load(key)
	require.True(t, found)
	require.Same(t, running, value)
	running.completed = time.Now().Add(-trackOutcomeRetention)
	running.witness = mi.CaptureTrackOutcome("", "")
	close(running.done)
	s.expireTrackOutcomes(time.Now())
	_, found = s.trackInFlight.Load(key)
	require.False(t, found)
}

func TestMCPConfiguredCollisionPrecedesAlreadyConfigured(t *testing.T) {
	s, mi := newWorktreeMCPServer(t)
	owner, loser := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	for _, root := range []string{owner, loser} {
		require.NoError(t, os.MkdirAll(root, 0o755))
	}
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: owner})
	require.NoError(t, err)
	mi.ExpectRepos([]config.RepoEntry{{Path: owner}, {Path: loser}})
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": loser}
	result, err := s.handleTrackRepository(t.Context(), req)
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Equal(t, "prefix_collision", result.StructuredContent.(map[string]any)["error_code"])
	require.NotContains(t, extractTextFromContent(t, result.Content), "already_configured")
}
