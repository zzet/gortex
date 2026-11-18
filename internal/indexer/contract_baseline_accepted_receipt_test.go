package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
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

// Binary and minified skips are successful reads: foreground indexing records
// a receipt for their skip node, so reconstruction must accept the same result
// instead of failing the whole repository baseline on every repair pass.
func TestContractBaselineAcceptedReceiptAcceptsContentSkips(t *testing.T) {
	ctx := context.Background()
	reg := parser.NewRegistry()
	reg.Register(languages.NewMarkdownExtractor())
	reg.Register(languages.NewCSSExtractor())
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("fixture")
	defer idx.Close()
	cases := []struct {
		name, path, language string
		src                  []byte
		reason               func(lang string, src []byte) string
		skip                 func(rel, lang, reason string) *parser.ExtractionResult
	}{
		{
			// Box-drawing characters mangled into control bytes and NULs.
			name: "binary", path: "docs/README.md", language: "markdown",
			src:    []byte("# Tool\n\n```\nsermon-ngx/\n\x1c\x00\x00 src/\n\x02   \x1c\x00\x00 clients/\n```\n"),
			reason: func(_ string, src []byte) string { return binaryArtifactReason(src) },
			skip:   binarySkipResult,
		},
		{
			name: "minified", path: "static/bootstrap.min.css", language: "css",
			src:    []byte(".a{color:red}" + strings.Repeat(".b{margin:0;padding:0;border:0}", 400)),
			reason: minifiedArtifactReason,
			skip:   minifiedSkipResult,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := ContractFollowupFile{RepoPrefix: "fixture", Path: "fixture/" + tc.path, Language: tc.language}
			reason := tc.reason(tc.language, tc.src)
			require.NotEmpty(t, reason, "fixture must trip the %s skip", tc.name)
			skip := tc.skip(tc.path, tc.language, reason)
			want, err := idx.collectContractBoundaryReceipt(ctx, file.Path, file.Language, tc.src, skip)
			require.NoError(t, err)
			got, err := collectContractBaselineAcceptedReceipt(ctx, idx, file, ContractAcceptedSource{Bytes: tc.src, SourceFingerprint: contractInputHash(tc.src)})
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}
