package mcp

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

type compressionSelectedBase struct {
	graph.Reader
	afterNodes    func()
	revisionCalls int
	unknown       bool
}

func (r *compressionSelectedBase) ReadConstantValueProjectionContext(ctx context.Context, ids []string, keys []graph.ConstantFileKey) (graph.ConstantValueProjection, error) {
	return r.Reader.(graph.ConstantValueProjectionReader).ReadConstantValueProjectionContext(ctx, ids, keys)
}
func (r *compressionSelectedBase) LayerContractFileNodesContext(ctx context.Context, repo string, paths []string) (map[string][]*graph.Node, error) {
	rows, err := graph.ContractFileNodesContext(ctx, r.Reader, repo, paths)
	if r.afterNodes != nil {
		r.afterNodes()
	}
	return rows, err
}
func (r *compressionSelectedBase) SearchMutationRevision() (uint64, bool) {
	r.revisionCalls++
	if r.unknown {
		return 0, false
	}
	return r.Reader.(interface{ SearchMutationRevision() (uint64, bool) }).SearchMutationRevision()
}

func TestReadFileCompressionSelectedGen0MutationCannotHideBehindLayerProof(t *testing.T) {
	for _, mode := range []string{"quiet", "gen0_node_aba", "unknown_base"} {
		t.Run(mode, func(t *testing.T) {
			selectedProjectionRegistry(t)
			stack := newViewStack(t)
			path := filepath.Join(stack.worktreeRoot, "unowned.go")
			content := []byte("package repo\n\nfunc Canonical() {}\n")
			require.NoError(t, os.WriteFile(path, content, 0600))
			base := stack.store.AtGeneration(0)
			node := viewRepoNode("repo/unowned.go::Canonical", "Canonical", graph.KindFunction, "repo/unowned.go", 3)
			base.AddNode(node)
			hash := blake3.Sum256(content)
			require.NoError(t, base.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "repo/unowned.go", ContentHash: hex.EncodeToString(hash[:]), Size: len(content), NodeCount: 1}}))
			ctx, view := routedEnrichmentView(t, stack)
			for _, source := range view.materialized.GenerationSources() {
				require.NotZero(t, source.GenerationID, "the actual leased source list omits shared gen0")
			}
			wrapped := &compressionSelectedBase{Reader: view.reader, unknown: mode == "unknown_base"}
			view.reader = wrapped
			hooked := false
			if mode == "gen0_node_aba" {
				wrapped.afterNodes = func() {
					hooked = true
					before := base.MutationRevision()
					changed := *node
					changed.StartLine += 100
					base.AddNode(&changed)
					base.AddNode(node)
					require.Greater(t, base.MutationRevision(), before)
				}
			}
			_, ok := stack.srv.readFileCompressionSymbols(ctx, path, "repo/unowned.go", content)
			if mode == "quiet" {
				require.True(t, ok, "healthy selected compression must still read unowned gen0 nodes")
			} else {
				require.False(t, ok, "unchanged file hashes/nonzero layers cannot witness mutable gen0")
			}
			if mode == "gen0_node_aba" {
				require.True(t, hooked, "the real selected full-node read reached gen0 before the interleaving")
			}
			require.Positive(t, wrapped.revisionCalls, "the composed reader's actual base witness must be consulted")
		})
	}
}
