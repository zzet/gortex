package indexer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A legitimate incremental dependency refresh must not introduce ownership or
// metadata that the same accepted manifest's completed cold pass omitted.
func TestContractDependencyColdIncrementalOwnerPayloadParity(t *testing.T) {
	for _, modulesEnabled := range []bool{true, false} {
		name := "with_manifest_owner"
		if !modulesEnabled {
			name = "without_manifest_owner"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "go.mod"), "module example.com/fixture\n\ngo 1.22\n\nrequire github.com/gin-gonic/gin v1.9.1\n")
			writeFile(t, filepath.Join(root, "keep.go"), "package fixture\nfunc Keep() int { return 1 }\n")
			removed := filepath.Join(root, "removed.go")
			writeFile(t, removed, "package fixture\nfunc Removed() int { return 2 }\n")
			idx, store := newSQLiteIndexer(t)
			defer idx.Close()
			idx.config.Coverage.Modules.Enabled = &modulesEnabled
			idx.SetRepoPrefix("fixture")
			_, err := idx.Index(root)
			require.NoError(t, err)

			const dependencyID = "dep::github.com/gin-gonic/gin"
			snapshot := func(reader graph.Reader) string {
				t.Helper()
				node := reader.GetNode(dependencyID)
				require.NotNil(t, node)
				require.Equal(t, graph.KindContract, node.Kind)
				require.Equal(t, modulesEnabled, node.Meta["contract_owner_record"])
				meta, ok := node.Meta["contract_meta"].(map[string]any)
				require.True(t, ok, "completed pass must persist full dependency metadata")
				require.Equal(t, "github.com/gin-gonic/gin", meta["module"])
				require.Equal(t, "v1.9.1", meta["version"])
				var owners []*graph.Edge
				for _, edge := range reader.GetInEdges(dependencyID) {
					if edge.Kind == graph.EdgeConsumes {
						owners = append(owners, edge)
					}
				}
				if modulesEnabled {
					require.Len(t, owners, 1)
					require.Equal(t, "fixture/go.mod", owners[0].From)
					require.Equal(t, "fixture/go.mod", owners[0].FilePath)
					require.Equal(t, graph.KindFile, reader.GetNode(owners[0].From).Kind)
					require.Equal(t, "dependency", owners[0].Meta["contract_owner_type"])
				} else {
					require.Empty(t, owners, "dependency analysis must not fabricate a manifest owner")
				}
				data, err := json.Marshal(struct {
					Node   *graph.Node
					Owners []*graph.Edge
				}{node, owners})
				require.NoError(t, err)
				return string(data)
			}
			cold := snapshot(store)
			require.NoError(t, os.Remove(removed))
			_, err = idx.IncrementalReindexPaths(root, []string{removed})
			require.NoError(t, err)
			require.Equal(t, cold, snapshot(store), "unrelated deletion must retain the complete manifest dependency payload")

			clean, cleanStore := newSQLiteIndexer(t)
			defer clean.Close()
			clean.config.Coverage.Modules.Enabled = &modulesEnabled
			clean.SetRepoPrefix("fixture")
			_, err = clean.Index(root)
			require.NoError(t, err)
			require.Equal(t, cold, snapshot(cleanStore), "independent full index must match the incremental result")
		})
	}
}
