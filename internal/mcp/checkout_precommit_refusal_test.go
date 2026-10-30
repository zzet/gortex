package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func heldPrecommitFixture(t *testing.T) *realCheckoutMutationFixture {
	t.Helper()
	gate := indexer.NewViewBuildGate()
	gate.Open()
	f := newRealCheckoutMutationFixtureWithSetup(t, nil, func(l *indexer.CheckoutLifecycle, _ string) {
		l.SetBuildGate(gate)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// Keep background publication out of the assertion window, using the
	// existing test build gate. Admission and Prepare still sample real Git.
	release, err := gate.Acquire(ctx, indexer.ViewBuildInteractive)
	require.NoError(t, err)
	t.Cleanup(release)
	return f
}

func movePrecommitHEAD(t *testing.T, f *realCheckoutMutationFixture) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.primary, "independent-motion.txt"), []byte("B\n"), 0o644))
	checkoutMutationGit(t, f.primary, "add", "independent-motion.txt")
	checkoutMutationGit(t, f.primary, "commit", "-m", "independent tree B")
	next := checkoutMutationGit(t, f.primary, "rev-parse", "HEAD")
	checkoutMutationGit(t, f.worktree, "reset", "--soft", next)
}

func TestCheckoutPrecommitRefusalRealSingleFileHandlers(t *testing.T) {
	for _, tool := range []string{"edit_file", "write_file"} {
		t.Run(tool, func(t *testing.T) {
			f := heldPrecommitFixture(t)
			beforeIndex := checkoutMutationGit(t, f.worktree, "ls-files", "--stage")
			f.srv.mutationPreCommitHook = func(*mutationCommitRecord) { movePrecommitHEAD(t, f) }
			var result *mcplib.CallToolResult
			if tool == "edit_file" {
				// No byte-admission guard: exercise the graph-routed lease.
				result = f.edit(t, f.worktree, map[string]any{
					"path": "edit.go", "old_string": "func New() {}", "new_string": "func Changed() {}",
				})
			} else {
				// Whole-file write takes the source-only byte-admission path.
				ctx := WithSessionCWD(WithSessionID(t.Context(), "precommit-byte-write"), f.worktree)
				var err error
				result, err = f.srv.wrapToolHandler(f.srv.handleWriteFile)(ctx, writeFileRequest(map[string]any{
					"path": "edit.go", "content": "package repo\n\nfunc Changed() {}\n",
				}))
				require.NoError(t, err)
			}
			require.True(t, result.IsError, resultText(result))
			text := resultText(result)
			require.Contains(t, text, "checkout HEAD changed")
			require.Contains(t, text, "disk_status=not_applied, receipt=commit-")
			require.Contains(t, text, "nothing was written and the file is unchanged")
			require.Contains(t, text, "Native read/edit is allowed for this checkout until a fresh view is ready")
			require.NotContains(t, text, "mutation was not admitted", "do not relabel a post-admission veto")
			after, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, primitiveWorktreeSource, string(after))
			require.Equal(t, beforeIndex, checkoutMutationGit(t, f.worktree, "ls-files", "--stage"))
			state, _ := f.srv.lifecycle.CheckoutMutationState(t.Context(), f.checkoutID, f.worktree)
			require.Equal(t, "read_only", state)
			_, wrongRoot := f.srv.lifecycle.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.primary)
			require.False(t, wrongRoot)
		})
	}
}

func TestCheckoutPrecommitRefusalExcludesMultiFileAndAppliedScopes(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		singleFile bool
		priorWrite bool
	}{
		{"batch_reuses_single_file", "edit_file", false, false},
		{"already_applied_another_file", "edit_file", true, true},
		{"batch_edit", "batch_edit", true, false},
		{"rename", "rename_symbol", true, false},
		{"scaffold", "scaffold", true, false},
		{"safe_delete", "safe_delete_symbol", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := heldPrecommitFixture(t)
			route, found, err := f.store.Catalog().GetCheckoutRoute(t.Context(), f.checkoutID)
			require.NoError(t, err)
			require.True(t, found)
			lease, err := f.srv.lifecycle.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, route.RouteEpoch)
			require.NoError(t, err)
			defer lease.Close()
			ctx := withCheckoutMutation(t.Context(), lease, f.worktree)
			state := checkoutMutationFromContext(ctx)
			state.singleFile = tc.singleFile
			if tc.priorWrite {
				state.committedPath = filepath.Join(f.worktree, "already-written.txt")
				require.NoError(t, os.WriteFile(state.committedPath, []byte("applied earlier\n"), 0o644))
			}
			f.srv.mutationPreCommitHook = func(*mutationCommitRecord) { movePrecommitHEAD(t, f) }
			record, err := f.srv.commitFileMutation(ctx, tc.tool, "", "", "edit.go", filepath.Join(f.worktree, "edit.go"), []byte("package repo\n"), 0o644)
			require.ErrorIs(t, err, indexer.ErrCheckoutMutationStale)
			require.ErrorIs(t, err, errMutationNotApplied)
			require.Equal(t, mutationDiskNotApplied, record.diskStatus())
			require.NotContains(t, mutationNotAppliedMessage("edit", record, err), "Native read/edit is allowed")
			_, marked := f.srv.lifecycle.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree)
			require.False(t, marked, "one refused file must not grant whole-operation fallback")
			after, readErr := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
			require.NoError(t, readErr)
			require.Equal(t, primitiveWorktreeSource, string(after))
			if tc.priorWrite {
				prior, readErr := os.ReadFile(state.committedPath)
				require.NoError(t, readErr)
				require.Equal(t, "applied earlier\n", string(prior))
			}
		})
	}
}

func TestCheckoutPrecommitTopLevelScopeRequiresKnownSingleFileTool(t *testing.T) {
	for _, tool := range []string{"edit_file", "write_file", "edit_symbol"} {
		require.True(t, singleFileCheckoutMutationTool(tool), tool)
	}
	for _, tool := range []string{"", "unknown_source_tool", "batch_edit", "rename_symbol", "scaffold", "safe_delete_symbol"} {
		t.Run(tool, func(t *testing.T) {
			ctx := withCheckoutMutation(t.Context(), &primitiveCheckoutMutation{}, t.TempDir())
			checkoutMutationFromContext(ctx).singleFile = singleFileCheckoutMutationTool(tool)
			// Even reusing a single-file primitive cannot widen an unknown or
			// multi-file top-level operation into a no-bytes permission proof.
			require.False(t, singleFilePrecommitRefusal(ctx, "edit_file"))
		})
	}
}

func TestCheckoutPrecommitRefusalRejectsUnprovenErrors(t *testing.T) {
	f := heldPrecommitFixture(t)
	for _, cause := range []error{
		indexer.ErrCheckoutMutationStale, indexer.ErrCheckoutMutationBusy, indexer.ErrCheckoutMutationPending,
		context.Canceled, context.DeadlineExceeded, errors.New("source I/O failure"),
		errors.Join(indexer.ErrCheckoutMutationStale, context.Canceled),
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			lease := &primitiveCheckoutMutation{prepareErr: cause}
			ctx := withCheckoutMutation(t.Context(), lease, f.worktree)
			checkoutMutationFromContext(ctx).singleFile = true
			record, err := f.srv.commitFileMutation(ctx, "edit_file", "", "", "edit.go", filepath.Join(f.worktree, "edit.go"), []byte("package repo\n"), 0o644)
			require.ErrorIs(t, err, cause)
			require.Equal(t, mutationDiskNotApplied, record.diskStatus())
			require.NotContains(t, mutationNotAppliedMessage("edit", record, err), "Native read/edit is allowed")
			_, marked := f.srv.lifecycle.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree)
			require.False(t, marked)
		})
	}
}

func TestCheckoutPrecommitRefusalDoesNotExtendToEncodingIOOrPostwrite(t *testing.T) {
	f := heldPrecommitFixture(t)
	t.Run("encoding_before_prepare", func(t *testing.T) {
		path := filepath.Join(f.worktree, "utf16.go")
		before := utf16LEWithBOM(t, "package repo\nfunc Legacy() {}\n")
		require.NoError(t, os.WriteFile(path, before, 0o644))
		lease := &primitiveCheckoutMutation{prepareErr: indexer.ErrCheckoutMutationStale}
		ctx := withCheckoutMutation(t.Context(), lease, f.worktree)
		checkoutMutationFromContext(ctx).singleFile = true
		record, err := f.srv.commitFileMutation(ctx, "write_file", "", "", "utf16.go", path, []byte("package repo\n"), 0o644)
		require.ErrorContains(t, err, "UTF-16")
		require.Zero(t, lease.prepared, "encoding refusal must not borrow a lease refusal")
		require.Equal(t, mutationDiskNotApplied, record.diskStatus())
		after, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		require.Equal(t, before, after)
	})
	t.Run("io_after_prepare", func(t *testing.T) {
		path := filepath.Join(f.worktree, "directory.go")
		require.NoError(t, os.Mkdir(path, 0o755))
		lease := &primitiveCheckoutMutation{}
		ctx := withCheckoutMutation(t.Context(), lease, f.worktree)
		checkoutMutationFromContext(ctx).singleFile = true
		record, err := f.srv.commitFileMutation(ctx, "write_file", "", "", "directory.go", path, []byte("package repo\n"), 0o644)
		require.Error(t, err)
		require.Equal(t, 1, lease.prepared)
		require.Equal(t, mutationDiskFailed, record.diskStatus())
		var marked *nativeCheckoutPrecommitRefusal
		require.False(t, errors.As(err, &marked))
	})
	t.Run("stale_refresh_after_write", func(t *testing.T) {
		path := filepath.Join(f.worktree, "postwrite.go")
		lease := &primitiveCheckoutMutation{refreshErr: indexer.ErrCheckoutMutationStale}
		ctx := withCheckoutMutation(t.Context(), lease, f.worktree)
		state := checkoutMutationFromContext(ctx)
		state.singleFile = true
		data := []byte("package repo\n")
		record, err := f.srv.commitFileMutation(ctx, "write_file", "", "", "postwrite.go", path, data, 0o644)
		require.NoError(t, err)
		require.Equal(t, mutationDiskCommitted, record.diskStatus())
		outcome := f.srv.refreshCheckoutMutation(ctx, path, state)
		require.ErrorIs(t, outcome.Err, indexer.ErrCheckoutMutationStale)
		require.False(t, singleFilePrecommitRefusal(ctx, "write_file"))
		after, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		require.Equal(t, data, after)
	})
	_, marked := f.srv.lifecycle.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree)
	require.False(t, marked, "encoding, disk failure and postwrite refresh must not establish a no-bytes refusal")
}
