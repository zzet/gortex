package mcp

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

type projectionRetirementSignalStore struct {
	*graph.Graph
	once         sync.Once
	metadataRead chan struct{}
}

func (s *projectionRetirementSignalStore) FileMetasByPaths(prefix string, paths []string) (map[string]graph.FileMetaRow, error) {
	rows, err := s.Graph.FileMetasByPaths(prefix, paths)
	s.once.Do(func() { close(s.metadataRead) })
	return rows, err
}

func TestCanonicalPublicationRetirementRequiresActualBackendContent(t *testing.T) {
	for _, published := range []bool{true, false} {
		name := "receipt_only"
		if published {
			name = "actual_publication"
		}
		t.Run(name, func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			registry := pendingFileProjections
			t.Cleanup(func() { pendingFileProjections = previous })
			fixture := newProjectionFixture(t)
			entry := fixture.edit("a.go", "package p\n\nfunc Published() {}\n")
			require.NoError(t, registry.publish(entry))
			backend := &projectionRetirementSignalStore{Graph: fixture.g, metadataRead: make(chan struct{})}
			s := newMutationServer()
			s.graph = backend
			s.indexer = fixture.idx
			done := make(chan indexer.MutationResult, 1)
			t.Cleanup(func() { close(done) })
			s.SetWatcher(mutationTestWatcher{done: done})
			ctx, release, err := acquireMutationPath(t.Context(), entry.absPath)
			require.NoError(t, err)
			admission := s.admitSourceMutations(ctx, entry.absPath)
			require.NotNil(t, admission.receipt)
			release()
			if published {
				require.NoError(t, fixture.idx.IndexFile(filepath.Join(fixture.dir, "a.go")))
				require.NotNil(t, fixture.g.GetNode("a.go::Published"), "prove actual graph publication independently of the completion ticket")
			} else {
				require.NotNil(t, fixture.g.GetNode("a.go::Old"), "the success notification alone cannot become content evidence")
			}
			done <- indexer.MutationResult{RequestedGeneration: admission.receipt.generation, AppliedGeneration: admission.receipt.generation, Reindexed: true}
			select {
			case <-admission.receipt.done:
			case <-time.After(3 * time.Second):
				t.Fatal("owned ticket did not complete")
			}
			select {
			case <-backend.metadataRead:
			case <-time.After(3 * time.Second):
				t.Fatal("canonical publication never verified the pending projection against its backend")
			}
			if published {
				require.Eventually(t, func() bool { return liveEntry(registry, entry.absPath) == nil }, 3*time.Second, time.Millisecond)
			} else {
				require.Same(t, entry, liveEntry(registry, entry.absPath))
				require.Zero(t, registry.retired.Load())
			}
		})
	}
}

type projectionRetirementCoreOnlyStore struct{ graph.Store }

func TestCanonicalRetirementKeepsEntryWithoutMetadataCapability(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	fixture := newProjectionFixture(t)
	entry := fixture.edit("a.go", "package p\n\nfunc Published() {}\n")
	require.NoError(t, pendingFileProjections.publish(entry))
	s := newMutationServer()
	s.graph = &projectionRetirementCoreOnlyStore{Store: fixture.g}
	require.NotImplements(t, (*graph.FileMetaPathReader)(nil), s.graph)
	work := s.canonicalProjectionRetirement(entry.absPath)
	require.Nil(t, work, "a core wrapper must never be unwrapped to another metadata authority")
	work.run()
	require.Same(t, entry, liveEntry(pendingFileProjections, entry.absPath))
}

func TestInlinePublicationRetiresVerifiedProjection(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	registry := pendingFileProjections
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	result := callTool(t, s, "edit_file", map[string]any{"path": path, "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc InlinePublished() {}", "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	response := extractTextResult(t, result)
	require.Equal(t, true, response["reindexed"], "prove real embedded publication before checking cache retirement")
	require.NotNil(t, s.graph.GetNode("main.go::InlinePublished"))
	require.Positive(t, registry.published.Load(), "the source commit must have admitted its projection first")
	require.Nil(t, liveEntry(registry, path), "synchronous canonical publication must retire content-verified file data too")
	require.Positive(t, registry.retired.Load())
}
