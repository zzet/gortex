package languages

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

var updateGoScratchGolden = flag.Bool("update-go-scratch-golden", false, "update the Go extraction scratch-lifetime semantic golden")

// goScratchSemantics preserves every graph field and scalar sidecar while
// ignoring only the order in which independent extraction passes emit rows.
func goScratchSemantics(t *testing.T, result *parser.ExtractionResult) []byte {
	t.Helper()
	var rows []string
	appendRow := func(kind string, value any) {
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		rows = append(rows, kind+" "+string(encoded))
	}
	for _, node := range result.Nodes {
		appendRow("node", node)
	}
	for _, edge := range result.Edges {
		// The public edge JSON omits internal confidence and metadata;
		// lifetime parity must preserve those semantic fields too.
		appendRow("edge", struct {
			*graph.Edge
			Confidence float64        `json:"confidence"`
			Meta       map[string]any `json:"meta"`
		}{edge, edge.Confidence, edge.Meta})
	}
	for _, constant := range result.ConstValues {
		appendRow("constant", constant)
	}
	slices.Sort(rows)
	return []byte(strings.Join(rows, "\n") + "\n")
}

func TestGoExtractionScratchLifetimeParity(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "go_scratch_lifetime.go.txt"))
	require.NoError(t, err)
	goldenPath := filepath.Join("testdata", "go_scratch_lifetime.golden")
	extractor := NewGoExtractor()
	for iteration := range 3 {
		result, err := extractor.Extract("parity.go", source)
		require.NoError(t, err)
		func() {
			defer result.ReleaseTree()
			got := goScratchSemantics(t, result)
			for _, marker := range []string{
				"custom_buyers", "value_ref_candidate", "callback_candidate",
				"generic_arg", "goframe-route", "grpc_register_impl", "temporal.stub",
			} {
				require.Contains(t, string(got), marker, "fixture must exercise the pass and deferred call paths")
			}
			if *updateGoScratchGolden && iteration == 0 {
				require.NoError(t, os.WriteFile(goldenPath, got, 0o644))
				t.Logf("semantic golden SHA256 %x", sha256.Sum256(got))
			}
			want, err := os.ReadFile(goldenPath)
			require.NoError(t, err)
			require.Equal(t, string(want), string(got), "full graph and scalar sidecars must match the pre-scope control")

			// The retained tree must survive scratch reuse after extraction.
			// Walk every token, then compare graph values again; no graph or
			// deferred record may depend on a rewound navigation wrapper.
			require.NotNil(t, result.Tree)
			root := result.Tree.Tree().RootNode()
			require.Equal(t, "source_file", root.Type())
			root.WithScratch(func() {
				walkNodes(root, func(node *sitter.Node) {
					require.NotEmpty(t, node.Type())
				})
			})
			require.Equal(t, string(want), string(goScratchSemantics(t, result)))
			require.Equal(t, "source_file", root.Type())
		}()
	}
}
