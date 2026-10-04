package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

func TestPrepareSourceDeclarationBuiltinDecoding(t *testing.T) {
	const source = "package current\nfunc CurrentDeclaration() {}\n"
	for _, tt := range []struct {
		name string
		raw  []byte
	}{
		{"UTF8", []byte(source)},
		{"UTF8_BOM", append([]byte{0xef, 0xbb, 0xbf}, []byte(source)...)},
		{"UTF16_LE", utf16LEWithBOM(t, source)},
		{"UTF16_BE", utf16BEWithBOM(t, source)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			idx := newTestIndexer(graph.New())
			extractor, prepared, supported, err := idx.PrepareSourceDeclaration("/checkout/current.go", "repo/current.go", tt.raw)
			require.NoError(t, err)
			require.True(t, supported)
			require.Equal(t, source, string(prepared))
			result, err := extractor.Extract("repo/current.go", prepared)
			require.NoError(t, err)
			var names []string
			for _, node := range result.Nodes {
				names = append(names, node.Name)
			}
			require.Contains(t, names, "CurrentDeclaration")
		})
	}
}

func TestSourceDeclarationRequiresGraphForConfiguredTransform(t *testing.T) {
	idx := &Indexer{transforms: newTransformPipeline([]config.TransformRule{{Extensions: []string{".go"}, Command: []string{"gortex-do-not-execute"}}}, nil, nil)}
	_, _, supported, err := idx.PrepareSourceDeclaration("/checkout/current.go", "repo/current.go", []byte("package current\n"))
	require.False(t, supported)
	require.ErrorIs(t, err, ErrSourceDeclarationGraphRequired)
}
