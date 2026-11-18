package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestStructuralEOFKeepsRecordedBuiltinArguments(t *testing.T) {
	const source = `package fixture
func BuiltinEdges(xs []int32) {
	ys := make([]int32, len(xs))
	_ = append(ys, int32(len(xs)))
	_ = float64(len(ys))
}
`
	const name = "builtins.go"
	const owner = "fixture/" + name
	warm := newContractJobFixture(t, map[string]string{name: source})
	finalSource := source + "\n// ordinary structural EOF save\n"
	cold := newContractJobFixture(t, map[string]string{name: finalSource})
	// Select the ordinary PRIMARY fallback, not resolution-preserving force
	// mode: the real evict/raw-add/resolve path must rebuild these references.
	rawEdgeProofRemoveMarker(warm, owner)
	path := filepath.Join(warm.root, name)
	writeFile(t, path, finalSource)
	next := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, next, next))
	result, err := warm.mi.IncrementalReindexRepo("fixture", []string{path})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.Zero(t, result.DerivedInvalidation.MetadataOnlyFiles)
	rows := func(f *contractJobFixture) []*graph.Edge {
		var edges []*graph.Edge
		for _, edge := range f.store.AllEdges() {
			if edge.FilePath == owner && edge.Kind == graph.EdgeArgOf {
				edges = append(edges, edge)
			}
		}
		return edges
	}
	want := rows(cold)
	require.NotEmpty(t, want)
	targets := map[string]bool{}
	for _, edge := range want {
		targets[edge.To] = true
		require.False(t, strings.HasPrefix(edge.To, "unresolved::"), "fixture must exercise fully attributed builtin arguments")
	}
	for _, target := range []string{"fixture::builtin::go::make", "fixture::builtin::go::append", "fixture::builtin::go::type::int32", "fixture::builtin::go::type::float64"} {
		require.True(t, targets[target], "cold fixture must contain "+target)
	}
	require.Equal(t, builderRenderEdges(want), builderRenderEdges(rows(warm)), "all argument edge identities and payload fields must equal independent completed cold indexing")
}
