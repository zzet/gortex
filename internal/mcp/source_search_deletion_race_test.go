package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestSourceSearchSnapshotRetriesWrappedDeletion(t *testing.T) {
	for _, disappearOnRead := range []int{1, 2} {
		t.Run(fmt.Sprintf("read_%d", disappearOnRead), func(t *testing.T) {
			stack := newViewStack(t)
			dir := filepath.Join(stack.worktreeRoot, "retry")
			require.NoError(t, os.MkdirAll(dir, 0755))
			survivor, deleted := filepath.Join(dir, "a.go"), filepath.Join(dir, "z.go")
			before := []byte("package retry\nfunc BeforeNeedle() {}\n")
			after := []byte("package retry\nfunc SurvivingNeedle() {}\n")
			require.NoError(t, os.WriteFile(survivor, before, 0644))
			require.NoError(t, os.WriteFile(deleted, []byte("package retry\nfunc DeletedNeedle() {}\n"), 0644))
			view := &requestView{sourceScope: "declarations", sourceRepoPrefix: "repo", viewRoot: stack.worktreeRoot}
			ctx := withRequestView(t.Context(), view)
			reads, survivorReads := 0, 0
			var missing error
			read := func(path string, maxBytes int64) ([]byte, physicalReadEvidence, error) {
				if path == survivor {
					survivorReads++
				}
				if path == deleted {
					reads++
					if reads == disappearOnRead {
						// The initial Stat or the second inventory has already
						// admitted this path. Delete before the real reader's Lstat.
						require.NoError(t, os.WriteFile(survivor, after, 0644))
						require.NoError(t, os.Remove(deleted))
					}
				}
				content, evidence, err := readPhysicalFileEvidenceBounded(path, maxBytes)
				if path == deleted && reads == disappearOnRead {
					missing = err
				}
				return content, evidence, err
			}
			files, err := stack.srv.sourceSearchSnapshotWithReader(ctx, view, []string{"retry"}, ResolvedScope{}, read)
			require.NoError(t, err)
			require.ErrorIs(t, missing, os.ErrNotExist)
			require.False(t, os.IsNotExist(missing), "the real reader wraps the PathError")
			require.Len(t, files, 1)
			require.Equal(t, "retry/a.go", files[0].path)
			require.Equal(t, after, files[0].content, "retry must resample earlier files, not omit only the disappeared path")
			require.GreaterOrEqual(t, survivorReads, 3)
			var req mcplib.CallToolRequest
			req.Params.Arguments = map[string]any{"query": "Needle", "path": "retry"}
			res, err := stack.srv.handleSourceSearchSymbols(ctx, req, view, "Needle", fieldQuery{}, ResolvedScope{})
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
			require.Contains(t, viewResultText(t, res), "repo/retry/a.go::SurvivingNeedle")
			require.NotContains(t, viewResultText(t, res), "DeletedNeedle")
			require.NotContains(t, viewResultText(t, res), "BeforeNeedle")
		})
	}
}

func TestSourceSearchSnapshotDoesNotRetryOtherReadFailures(t *testing.T) {
	for _, failure := range []string{"permission", "nonregular", "escape"} {
		t.Run(failure, func(t *testing.T) {
			srv, root := setupTestServer(t)
			path := filepath.Join(root, "target.go")
			require.NoError(t, os.WriteFile(path, []byte("package target\n"), 0644))
			outside := filepath.Join(t.TempDir(), "outside.go")
			require.NoError(t, os.WriteFile(outside, []byte("package outside\nfunc PrivateNeedle() {}\n"), 0644))
			if failure == "escape" {
				probe := filepath.Join(root, "symlink-probe")
				if err := os.Symlink(outside, probe); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				require.NoError(t, os.Remove(probe))
			}
			view := &requestView{sourceScope: "declarations", viewRoot: root}
			reads := 0
			read := func(abs string, maxBytes int64) ([]byte, physicalReadEvidence, error) {
				reads++
				switch failure {
				case "permission":
					return nil, physicalReadEvidence{}, fmt.Errorf("physical read: %w", &os.PathError{Op: "open", Path: abs, Err: os.ErrPermission})
				case "nonregular":
					require.NoError(t, os.Remove(abs))
					require.NoError(t, os.Mkdir(abs, 0755))
				case "escape":
					require.NoError(t, os.Remove(abs))
					require.NoError(t, os.Symlink(outside, abs))
				}
				return readPhysicalFileEvidenceBounded(abs, maxBytes)
			}
			files, err := srv.sourceSearchSnapshotWithReader(withRequestView(t.Context(), view), view, []string{"target.go"}, ResolvedScope{}, read)
			require.Error(t, err)
			require.Nil(t, files)
			require.Equal(t, 1, reads, "non-ENOENT and confinement failures must not become retry success")
			require.NotErrorIs(t, err, errSourceSnapshotMoved)
			if failure == "permission" {
				require.ErrorIs(t, err, os.ErrPermission)
			}
		})
	}
}

func TestSourceSearchSnapshotDeletionRetriesKeepOriginalDeadline(t *testing.T) {
	t.Run("expired_original_deadline", func(t *testing.T) {
		srv, root := setupTestServer(t)
		deadline := time.Now().Add(-time.Second)
		view := &requestView{sourceScope: "declarations", viewRoot: root, freshness: &requestFreshnessOutcome{deadline: deadline}}
		read := func(string, int64) ([]byte, physicalReadEvidence, error) {
			t.Fatal("expired original budget was replaced before a physical read")
			return nil, physicalReadEvidence{}, nil
		}
		files, err := srv.sourceSearchSnapshotWithReader(withRequestView(t.Context(), view), view, nil, ResolvedScope{}, read)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Nil(t, files)
		require.Equal(t, deadline, view.freshness.deadline)
	})
	t.Run("canceled_deletion_retry", func(t *testing.T) {
		srv, root := setupTestServer(t)
		path := filepath.Join(root, "target.go")
		content := []byte("package target\nfunc MovingNeedle() {}\n")
		require.NoError(t, os.WriteFile(path, content, 0644))
		view := &requestView{sourceScope: "declarations", viewRoot: root}
		ctx, cancel := context.WithCancel(withRequestView(t.Context(), view))
		defer cancel()
		reads := 0
		read := func(abs string, maxBytes int64) ([]byte, physicalReadEvidence, error) {
			reads++
			require.NoError(t, os.Remove(abs))
			bytes, evidence, err := readPhysicalFileEvidenceBounded(abs, maxBytes)
			require.True(t, errors.Is(err, os.ErrNotExist))
			require.NoError(t, os.WriteFile(abs, content, 0644))
			cancel()
			return bytes, evidence, err
		}
		files, err := srv.sourceSearchSnapshotWithReader(ctx, view, []string{"target.go"}, ResolvedScope{}, read)
		require.ErrorIs(t, err, context.Canceled, "wrapped deletion must reach the cancelable resample wait")
		require.Nil(t, files)
		require.Equal(t, 1, reads)
	})
}
