package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractFollowupProjectedHeaderUsesCapturedAuthority(t *testing.T) {
	for _, mode := range []string{"missing_selected", "selected", "wrong_kind", "wrong_path", "unadmitted", "read_error", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			idx, store := newSQLiteIndexer(t)
			idx.SetRepoPrefix("fixture")
			src := generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n")
			result, recognized := generatedTreeSitterParserProjection("src/parser.c", "c", src)
			require.True(t, recognized)
			idx.applyRepoPrefix(result.Nodes, result.Edges)
			path := "fixture/tree_sitter/parser.h"
			if mode == "unadmitted" {
				path = "Trellis/tree_sitter/parser.h"
			}
			header := &graph.Node{ID: path, FilePath: path, RepoPrefix: "fixture", Kind: graph.KindFile}
			if mode == "unadmitted" {
				header.RepoPrefix = "Trellis"
			}
			require.NoError(t, store.AddBatchChecked([]*graph.Node{header}, nil))
			_, selected, err := store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "fixture-selected", LayerID: "selected", GenerationKind: "dirty", ConfigHash: "config", ExtractorVersions: `{"c":"1"}`, ResolverVersion: "test", CreatedAt: 1})
			require.NoError(t, err)
			if mode == "selected" || mode == "wrong_kind" || mode == "wrong_path" {
				copy := *header
				if mode == "wrong_kind" {
					copy.Kind = graph.KindVariable
				}
				if mode == "wrong_path" {
					copy.FilePath = "fixture/not-the-header.h"
				}
				require.NoError(t, selected.AddBatchChecked([]*graph.Node{&copy}, nil))
			}
			for _, edge := range result.Edges {
				if edge.Kind == graph.EdgeImports {
					edge.To = path
				}
			}
			var reader graph.Reader = selected
			if mode == "read_error" {
				reader = contractCapturedFailingProjectionReader{Reader: selected, err: errors.New("selected header unavailable")}
			}
			core := &contractCapturedCore{Reader: store, readers: map[string]graph.Reader{"fixture": reader}}
			ctx := t.Context()
			if mode == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			proof := idx.contractGeneratedCoreProof(ctx, ContractFollowupFile{Path: "fixture/src/parser.c", RepoPrefix: "fixture", Language: "c"}, src, result.Nodes, result.Edges, core)
			require.Equal(t, mode == "selected", proof, "broad primary header cannot certify absent, invalid, or unadmitted selected evidence")
		})
	}
}

type contractCapturedFailingProjectionReader struct {
	graph.Reader
	err error
}

func (r contractCapturedFailingProjectionReader) LayerContractFileProjectionContext(context.Context, string, []string) (graph.ContractFileProjection, error) {
	return graph.ContractFileProjection{}, r.err
}
func (r contractCapturedFailingProjectionReader) LayerContractIDProjectionContext(context.Context, []string) (graph.ContractFileProjection, error) {
	return graph.ContractFileProjection{}, r.err
}
