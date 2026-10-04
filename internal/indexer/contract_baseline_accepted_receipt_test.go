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
	// Accepted foreground extraction applies this policy after the raw
	// extractor. Compare the same accepted facts, including constant metadata.
	stampParseErrors(parsed)
	normalizeExtractionMetadata(parsed, src)
	want, err := idx.collectContractBoundaryReceipt(ctx, file.Path, file.Language, src, parsed)
	require.NoError(t, err)
	require.NotEmpty(t, want.Groups, "constant-derived endpoint must be reconstructed")
	require.NotContains(t, want.HandlerInputs, "fixture/routes.go::route", "source attribution to a constant must not require function body facts")
	require.NotEmpty(t, want.ProducedInputs[contractBoundaryLookupKey("fixture", "constant_name", "route")], "route constant must retain dependency invalidation evidence")
	// Selected durable evidence may have been enriched/reordered. These rows
	// must not be substituted for the raw accepted parse during reconstruction.
	core.AddBatch([]*graph.Node{{ID: "fixture/routes.go::route", Name: "route", Kind: graph.KindFunction, FilePath: file.Path, RepoPrefix: "fixture", StartLine: 2, EndLine: 2}}, nil)
	got, err := collectContractBaselineAcceptedReceipt(ctx, idx, file, ContractAcceptedSource{Bytes: src, SourceFingerprint: contractInputHash(src)})
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, graph.KindFunction, core.GetNode("fixture/routes.go::route").Kind, "background reconstruction must not rewrite core evidence")
}
