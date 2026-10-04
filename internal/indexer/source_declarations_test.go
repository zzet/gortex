package indexer

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"testing"
)

func TestSourceDeclarationRequiresGraphForConfiguredTransform(t *testing.T) {
	idx := &Indexer{transforms: newTransformPipeline([]config.TransformRule{{Extensions: []string{".go"}, Command: []string{"gortex-do-not-execute"}}}, nil, nil)}
	_, _, supported, err := idx.PrepareSourceDeclaration("/checkout/current.go", "repo/current.go", []byte("package current\n"))
	require.False(t, supported)
	require.ErrorIs(t, err, ErrSourceDeclarationGraphRequired)
}
