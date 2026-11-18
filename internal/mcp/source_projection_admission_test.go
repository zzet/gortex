package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestToolCommitPublishesExactProjectionBeforeGraphCompletion(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	done := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(done) })
	s.SetWatcher(mutationTestWatcher{done: done})
	s.mutationReindexWait = time.Nanosecond
	path := filepath.Join(root, "main.go")
	before := parser.ParseCount()
	result := callTool(t, s, "edit_file", map[string]any{"path": path, "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc ProjectedAtCommit() {}", "format": "json"})
	parses := parser.ParseCount() - before
	require.False(t, result.IsError, "%v", result.Content)
	payload := extractTextResult(t, result)
	require.Equal(t, true, payload["reindex_pending"])
	require.Equal(t, map[string]any{"file": "fresh"}, payload["tier_state"], "the physical projection supplies file exactness, but an unstamped ticket cannot fabricate graph tier evidence")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "ProjectedAtCommit")
	entry, err := pendingFileProjections.lookup(path)
	require.NoError(t, err)
	require.NotNil(t, entry, "physical commit must admit its exact file projection before a passive graph wait can end")
	require.Positive(t, entry.commitSeq)
	require.EqualValues(t, 1, parses, "a clean committed candidate shares one native parse with its syntax gate")
	require.NotNil(t, graph.NewOverlaidView(s.graph, entry.layer).GetNode("main.go::ProjectedAtCommit"))
	require.Nil(t, s.graph.GetNode("main.go::ProjectedAtCommit"), "file projection is not graph publication")
	requireMutationFileViewState(t, payload, "projected")
	require.Equal(t, true, payload["readback_exact"])
}

type sourceProjectionBlockingExtractor struct {
	base                       parser.Extractor
	active                     atomic.Bool
	once                       sync.Once
	entered, release, finished chan struct{}
}

func (e *sourceProjectionBlockingExtractor) Language() string     { return "go" }
func (e *sourceProjectionBlockingExtractor) Extensions() []string { return []string{".go"} }
func (e *sourceProjectionBlockingExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	if e.active.Load() {
		e.once.Do(func() { close(e.entered) })
		<-e.release
		defer close(e.finished)
	}
	return e.base.Extract(path, raw)
}

func TestMutationCommitRechecksCancellationAndIdentityAfterProjection(t *testing.T) {
	for _, mode := range []string{"cancel", "retarget"} {
		t.Run(mode, func(t *testing.T) {
			blocker := &sourceProjectionBlockingExtractor{base: languages.NewGoExtractor(), entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
			registry := parser.NewRegistry()
			registry.Register(blocker)
			s, root := setupTestServerWithRegistry(t, registry)
			var unblock sync.Once
			t.Cleanup(func() { unblock.Do(func() { close(blocker.release) }) })
			path := filepath.Join(root, "main.go")
			var alias, other string
			if mode == "retarget" {
				original := filepath.Join(root, "original")
				other = filepath.Join(root, "other")
				alias = filepath.Join(root, "selected")
				for _, dir := range []string{original, other} {
					require.NoError(t, os.MkdirAll(dir, 0o755))
					require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package p\nfunc Untouched() {}\n"), 0o644))
				}
				if err := os.Symlink(original, alias); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				path = filepath.Join(alias, "main.go")
			}
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, release, err := acquireMutationPath(parent, path)
			require.NoError(t, err)
			defer release()
			blocker.active.Store(true)
			done := make(chan error, 1)
			go func() {
				_, err := s.commitFileMutation(ctx, "write_file", "", "", "main.go", path, []byte("package p\nfunc BlockedProjection() {}\n"), 0o644)
				done <- err
			}()
			select {
			case <-blocker.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("candidate projection did not enter")
			}
			if mode == "cancel" {
				cancel()
			} else {
				require.NoError(t, os.Remove(alias))
				require.NoError(t, os.Symlink(other, alias))
			}
			unblock.Do(func() { close(blocker.release) })
			select {
			case err := <-done:
				require.ErrorIs(t, err, errMutationNotApplied)
			case <-time.After(2 * time.Second):
				t.Fatal("refused source commit did not return")
			}
			select {
			case <-blocker.finished:
			case <-time.After(2 * time.Second):
				t.Fatal("owned projection extraction did not finish")
			}
			current, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, before, current, "no bytes may change after cancellation or held-path identity loss during preparation")
		})
	}
}

func TestRejectedProjectionCannotEscapeThroughMutationLedger(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	pendingFileProjections.enabled = false
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	s.SetWatcher(mutationTestWatcher{})
	result := callTool(t, s, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() { _ = 2 }", "format": "json"})
	require.False(t, result.IsError)
	payload := extractTextResult(t, result)
	requireMutationFileViewState(t, payload, "unavailable")
	require.Equal(t, false, payload["readback_exact"])
	require.Equal(t, "disabled", payload["file_reason"])
	require.Empty(t, pendingFileProjections.snap.Load().entries)
	record, ok := s.mutationCommits.byReceipt(payload["mutation_receipt"].(string))
	require.True(t, ok)
	require.Nil(t, record.fileProjection.Value(), "a refused graph must never be retained by a physical receipt")
}

func TestPreparedProjectionEachPhysicalCommitHasImmutableEntry(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	ctx, release, err := acquireMutationPath(t.Context(), path)
	require.NoError(t, err)
	defer release()
	raw := []byte("package main\nfunc SameBytes() {}\n")
	prepared, _ := s.buildFileProjection(ctx, path, raw)
	require.NotNil(t, prepared.entry, prepared.refusal)
	operation := sourceMutationOperation{kind: sourceWrite, projection: prepared}
	_, err = s.commitFileMutation(ctx, "write_file", "", "", "main.go", path, raw, 0o644, operation)
	require.NoError(t, err)
	first := liveEntry(pendingFileProjections, path)
	require.NotNil(t, first)
	sequence, disk := first.commitSeq, first.disk
	_, err = s.commitFileMutation(ctx, "write_file", "", "", "main.go", path, raw, 0o644, operation)
	require.NoError(t, err)
	second := liveEntry(pendingFileProjections, path)
	require.NotSame(t, first, second)
	require.Greater(t, second.commitSeq, sequence)
	require.Equal(t, sequence, first.commitSeq, "old pinned snapshots must remain immutable")
	require.True(t, first.disk.matches(disk))
	require.Zero(t, prepared.entry.commitSeq, "a reusable private preparation never becomes a published mutable entry")
}
