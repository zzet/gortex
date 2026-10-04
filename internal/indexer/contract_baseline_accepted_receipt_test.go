package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractBaselineAcceptedReceiptMatchesRawParserFacts(t *testing.T) {
	ctx := context.Background()
	core := graph.New()
	idx := newTestIndexer(core)
	idx.SetRepoPrefix("fixture")
	defer idx.Close()
	src := []byte("package fixture\nconst route = \"/constant-route\"\nfunc register(router *Router) { router.GET(route, serve) }\nfunc serve() {}\n")
	file := ContractFollowupFile{RepoPrefix: "fixture", Path: "fixture/routes.go", Language: "go"}
	parsed, err := idx.ExtractBuffer(file.Language, "routes.go", src)
	require.NoError(t, err)
	defer parsed.ReleaseTree()
	want, err := idx.collectContractBoundaryReceipt(ctx, file.Path, file.Language, src, parsed)
	require.NoError(t, err)
	require.NotEmpty(t, want.Groups, "constant-derived endpoint must be reconstructed")
	// Selected durable evidence may have been enriched/reordered. These rows
	// must not be substituted for the raw accepted parse during reconstruction.
	core.AddBatch([]*graph.Node{{ID: "fixture/routes.go::route", Name: "route", Kind: graph.KindFunction, FilePath: file.Path, RepoPrefix: "fixture", StartLine: 2, EndLine: 2}}, nil)
	got, err := collectContractBaselineAcceptedReceipt(ctx, idx, file, ContractAcceptedSource{Bytes: src, SourceFingerprint: contractInputHash(src)})
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, graph.KindFunction, core.GetNode("fixture/routes.go::route").Kind, "background reconstruction must not rewrite core evidence")
}
