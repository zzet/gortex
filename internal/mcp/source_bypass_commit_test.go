package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestPropagatedSourceWriteHasCommitReceipt(t *testing.T) {
	srv, _, root := setupDeleteServer(t)
	path := filepath.Join(root, "caller.go")
	ctx, note := withMutationCommitNote(t.Context())
	applied, failed, err := srv.applyRemoveLinePatches(ctx, []callerPatch{{
		File: "caller.go", Line: 4, CurrentText: "Target()", Action: "remove_line", abs: path,
	}})
	require.NoError(t, err)
	require.Empty(t, failed)
	require.Len(t, applied, 1)
	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(current), "Target()", "prove that a real source write landed")
	verdict := note.verdict()
	require.Equal(t, mutationDiskCommitted, verdict.Status, "a source writer must publish the same disk receipt as the ordinary edit path")
	require.Len(t, verdict.Snapshots, 1)
	require.Equal(t, "caller.go", verdict.Snapshots[0].Path)
	require.True(t, verdict.Snapshots[0].GraphRecorded)
}

func TestOverlayDiskMergeUsesSourceCommitAuthority(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprintf("deleted_%t/cancelled_%t", deleted, cancelled), func(t *testing.T) {
				srv, root := setupTestServer(t)
				path := filepath.Join(root, "main.go")
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				ctx, note := withMutationCommitNote(t.Context())
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				if cancelled {
					cancel()
				}
				err = srv.diskWriteForMerge(ctx)(daemon.OverlayFile{Path: "main.go", Content: "package main\nfunc Merged() {}\n", Deleted: deleted, BaseSHA: gitBlobSHA(before)})
				if cancelled {
					require.Error(t, err)
					got, readErr := os.ReadFile(path)
					require.NoError(t, readErr)
					require.Equal(t, before, got, "refused merge must preserve bytes")
					return
				}
				require.NoError(t, err)
				if deleted {
					_, statErr := os.Stat(path)
					require.ErrorIs(t, statErr, os.ErrNotExist)
				} else {
					got, readErr := os.ReadFile(path)
					require.NoError(t, readErr)
					require.Contains(t, string(got), "func Merged()")
				}
				require.Equal(t, mutationDiskCommitted, note.verdict().Status)
				require.Len(t, note.verdict().Snapshots, 1)
			})
		}
	}
}

func TestMoveAndInlineSourceWritesHavePhysicalReceipts(t *testing.T) {
	for _, tool := range []string{"move_symbol", "inline_symbol"} {
		t.Run(tool, func(t *testing.T) {
			srv, root := setupMoveInlineRepo(t, map[string]string{
				"pkg/accessor.go": "package pkg\nfunc Value() int { return 7 }\n",
				"pkg/caller.go":   "package pkg\nfunc Use() int { return Value() }\n",
				"pkg/target.go":   "package pkg\nfunc Existing() {}\n",
			})
			ctx, note := withMutationCommitNote(t.Context())
			req := editFileRequest(map[string]any{"id": "pkg/accessor.go::Value", "target_file": "pkg/target.go"})
			var result *mcp.CallToolResult
			var err error
			if tool == "move_symbol" {
				result, err = srv.handleMoveSymbol(ctx, req)
			} else {
				result, err = srv.handleInlineSymbol(ctx, req)
			}
			require.NoError(t, err)
			require.False(t, result.IsError, "%v", result.Content)
			accessor, err := os.ReadFile(filepath.Join(root, "pkg/accessor.go"))
			require.NoError(t, err)
			require.NotContains(t, string(accessor), "func Value()", "prove a real multi-file source operation landed")
			require.Equal(t, mutationDiskCommitted, note.verdict().Status)
			paths := make([]string, 0, len(note.verdict().Snapshots))
			for _, snapshot := range note.verdict().Snapshots {
				paths = append(paths, snapshot.Path)
				require.True(t, snapshot.GraphRecorded)
			}
			want := []string{"pkg/accessor.go", "pkg/caller.go"}
			if tool == "move_symbol" {
				want = append(want, "pkg/target.go")
			}
			require.ElementsMatch(t, want, paths, "every actual source effect must own exactly one physical receipt")
		})
	}
}

func TestPlannedSourceAdmissionPreservesChangedAndNewTargets(t *testing.T) {
	for _, kind := range []string{"changed", "new_target", "cancelled", "held"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			first, second := filepath.Join(root, "first.go"), filepath.Join(root, "second.go")
			require.NoError(t, os.WriteFile(first, []byte("first"), 0o644))
			require.NoError(t, os.WriteFile(second, []byte("before"), 0o644))
			plans := []plannedSource{{path: first, before: []byte("first"), existed: true}, {path: second, before: []byte("before"), existed: kind != "new_target"}}
			if kind == "changed" {
				require.NoError(t, os.WriteFile(second, []byte("concurrent"), 0o644))
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			ctx, release, err := lockPlannedSources(ctx, plans)
			if kind == "held" {
				require.NoError(t, err)
				require.True(t, mutationPathAdmitted(ctx, pathkey.MutationKey(first)))
				require.True(t, mutationPathAdmitted(ctx, pathkey.MutationKey(second)))
				release()
				require.False(t, mutationPathAdmitted(ctx, pathkey.MutationKey(first)))
			} else {
				require.Error(t, err)
				require.Nil(t, release)
			}
			got, readErr := os.ReadFile(first)
			require.NoError(t, readErr)
			require.Equal(t, "first", string(got), "failure on another planned path must precede every write")
		})
	}
}
