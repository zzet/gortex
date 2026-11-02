package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type compressionCheckedStore struct {
	graph.Store
	checked    graph.ConstantValueProjectionReader
	nodes      graph.OverlayLayerContractFileNodesReader
	afterNodes func()
}

func (s *compressionCheckedStore) ReadConstantValueProjectionContext(ctx context.Context, ids []string, keys []graph.ConstantFileKey) (graph.ConstantValueProjection, error) {
	return s.checked.ReadConstantValueProjectionContext(ctx, ids, keys)
}
func (s *compressionCheckedStore) LayerContractFileNodesContext(ctx context.Context, repo string, paths []string) (map[string][]*graph.Node, error) {
	rows, err := s.nodes.LayerContractFileNodesContext(ctx, repo, paths)
	if s.afterNodes != nil {
		s.afterNodes()
	}
	return rows, err
}

type compressionRevisionStore struct{ *compressionCheckedStore }

func (s *compressionRevisionStore) MutationRevision() uint64 {
	return s.Store.(interface{ MutationRevision() uint64 }).MutationRevision()
}

func TestReadFileCompressionBindsCanonicalBytesAndStableNodes(t *testing.T) {
	selectedProjectionRegistry(t)
	s, root := setupCompressTestServer(t)
	path := filepath.Join(root, "service.go")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	nodes, ok := s.readFileCompressionSymbols(t.Context(), path, "service.go", content)
	require.True(t, ok, "a clean indexed file needs no cache entry to compress")
	require.NotEmpty(t, nodes)
	_, ok = s.readFileCompressionSymbols(t.Context(), path, "service.go", append([]byte("\n"), content...))
	require.False(t, ok, "old canonical ranges cannot bind a shifted current render")
	require.Empty(t, pendingFileProjections.snap.Load().entries, "compression is not a projection publisher")
}

func TestReadFileCompressionRejectsHashABAAndNewPendingOwner(t *testing.T) {
	for _, mode := range []string{"hash_aba", "same_bytes_new_owner", "pending_owner_aba", "unknown_revision"} {
		t.Run(mode, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, root := setupCompressTestServer(t)
			path := filepath.Join(root, "service.go")
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			base := s.graph
			wrapped := &compressionCheckedStore{Store: base, checked: base.(graph.ConstantValueProjectionReader), nodes: base.(graph.OverlayLayerContractFileNodesReader)}
			var published *projectionEntry
			if mode == "hash_aba" {
				wrapped.afterNodes = func() {
					nodes := base.GetFileNodes("service.go")
					require.NotEmpty(t, nodes)
					original := nodes[0]
					changed := *original
					changed.StartLine += 100
					base.AddNode(&changed)
					base.AddNode(original)
				}
			} else if mode == "same_bytes_new_owner" || mode == "pending_owner_aba" {
				wrapped.afterNodes = func() {
					stamp, statErr := statDiskStamp(path)
					require.NoError(t, statErr)
					published = syntheticProjectionEntry(path, "service.go", "new-owner", 1, stamp, content)
					require.NoError(t, pendingFileProjections.publishExpected(published, nil))
					if mode == "pending_owner_aba" {
						pendingFileProjections.drop(published)
					}
				}
			}
			if mode == "unknown_revision" {
				s.graph = wrapped
			} else {
				s.graph = &compressionRevisionStore{wrapped}
			}
			_, ok := s.readFileCompressionSymbols(t.Context(), path, "service.go", content)
			require.False(t, ok, "matching source hashes cannot conceal changed/unknown selected ownership")
			if published != nil && mode != "pending_owner_aba" {
				require.Same(t, published, pendingFileProjections.snap.Load().entries[published.key])
				require.False(t, published.suspect.Load(), "refusing old ranges cannot poison a newer entry")
			}
		})
	}
}
