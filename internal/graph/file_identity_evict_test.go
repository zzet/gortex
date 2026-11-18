package graph_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The alias bucket also contains shared coverage targets. Removing that bucket
// wholesale would destroy the other file's perfectly valid ownership edges.
func TestFileIdentityEvictionPreservesSharedAliasTargets(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite", "delta"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store
			switch backend {
			case "memory":
				store = graph.New()
			case "sqlite":
				disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, disk.Close()) })
				store = disk
			case "delta":
				store = graph.New()
			}
			canonical, legacy, other := "r/sub/file.py", `r/sub\file.py`, "r/other.py"
			nodes := []*graph.Node{
				{ID: canonical, Kind: graph.KindFile, FilePath: canonical},
				{ID: canonical + "::helper", Kind: graph.KindFunction, Name: "helper", FilePath: canonical},
				{ID: legacy, Kind: graph.KindFile, FilePath: legacy},
				{ID: legacy + "::helper", Kind: graph.KindFunction, Name: "helper", FilePath: legacy},
				{ID: other, Kind: graph.KindFile, FilePath: other},
				{ID: other + "::caller", Kind: graph.KindFunction, Name: "caller", FilePath: other},
			}
			var edges []*graph.Edge
			for _, target := range []struct {
				id   string
				kind graph.NodeKind
				edge graph.EdgeKind
			}{
				{"r/license::MIT", graph.KindLicense, graph.EdgeLicensedAs},
				{"r/team::core", graph.KindTeam, graph.EdgeOwns},
				{"r/module::pkg", graph.KindModule, graph.EdgeImports},
			} {
				nodes = append(nodes, &graph.Node{ID: target.id, Kind: target.kind, FilePath: legacy})
				edges = append(edges, &graph.Edge{From: other, To: target.id, Kind: target.edge, FilePath: other})
			}
			edges = append(edges, &graph.Edge{From: other + "::caller", To: legacy + "::helper", Kind: graph.EdgeCalls, FilePath: other})
			store.AddBatch(nodes, edges)
			if backend == "delta" {
				store = graph.NewDeltaWriter(store, nil)
			}
			n, e := store.(graph.FileIdentityBatchEvicter).EvictFilesWithNodeIDs([]string{canonical}, []string{legacy, legacy + "::helper"})
			require.Equal(t, 4, n)
			require.Equal(t, 1, e)
			for _, id := range []string{canonical, canonical + "::helper", legacy, legacy + "::helper"} {
				require.Nil(t, store.GetNode(id), id)
			}
			for _, id := range []string{other, other + "::caller", "r/license::MIT", "r/team::core", "r/module::pkg"} {
				require.NotNil(t, store.GetNode(id), id)
			}
			require.Len(t, store.GetOutEdges(other), 3)
			require.Empty(t, store.GetOutEdges(other+"::caller"))
			require.Len(t, store.GetFileNodes(legacy), 3, "shared nodes stay at their existing scalar spelling")
		})
	}
}
