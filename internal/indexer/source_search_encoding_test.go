package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestSourceSearchDecodingPreservesAssetsAndConfiguredTransforms(t *testing.T) {
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	reg.Register(languages.NewImageAssetExtractor())
	idx := &Indexer{transforms: newTransformPipeline([]config.TransformRule{{Extensions: []string{".go"}, Command: []string{"gortex-do-not-execute"}}}, reg, nil)}
	const source = "package current\n// source marker\n"
	raw := utf16LEWithBOM(t, source)
	require.Equal(t, source, string(idx.DecodeSourceSearchText("current.go", raw)))
	require.Equal(t, raw, idx.DecodeSourceSearchText("asset.png", raw))
	binary := []byte{'a', 0, 'b', 1, 'c', 0, 'd', 2}
	require.Equal(t, binary, idx.DecodeSourceSearchText("binary.go", binary))
	require.Equal(t, []byte(source), idx.DecodeSourceSearchText("current.go", []byte(source)))
	require.Equal(t, utf16LEWithBOM(t, source), raw, "decoding must not alter accepted raw evidence")
}
