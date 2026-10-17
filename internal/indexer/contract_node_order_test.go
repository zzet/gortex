package indexer

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// A contract ID shared by several records (one environment variable read in
// two files) is one node, and which record's file, line and metadata it
// carries must not depend on the order the registry listed the records in:
// the registry is a map filled by parallel extraction, so a whole index listed
// them differently from run to run (14–23 contract nodes differed between two
// whole indexes of one tree).
func TestContractNodeIsTheFirstRecordWhateverTheListingOrder(t *testing.T) {
	record := func(file string, line int) contracts.Contract {
		return contracts.Contract{
			ID: "env::TERM", Type: contracts.ContractEnv, Role: contracts.RoleConsumer,
			FilePath: file, Line: line, RepoPrefix: builderRepoPrefix, Confidence: 0.9,
			Meta: map[string]any{"var": "TERM"},
		}
	}
	other := contracts.Contract{
		ID: "env::HOME", Type: contracts.ContractEnv, Role: contracts.RoleConsumer,
		FilePath: builderRepoPrefix + "/z.go", Line: 3, RepoPrefix: builderRepoPrefix,
	}
	orders := [][]contracts.Contract{
		{record(builderRepoPrefix+"/progress/mode.go", 48), record(builderRepoPrefix+"/progress/glyphs.go", 246), other},
		{other, record(builderRepoPrefix+"/progress/glyphs.go", 246), record(builderRepoPrefix+"/progress/mode.go", 48)},
	}
	var first []*graph.Node
	for i, all := range orders {
		nodes, edges, _ := contractGraphRows(graph.New(), all, true)
		byID := map[string]int{}
		for _, n := range nodes {
			byID[n.ID]++
		}
		if byID["env::TERM"] != 1 || byID["env::HOME"] != 1 {
			t.Fatalf("order %d: one node per contract ID, got %v", i, byID)
		}
		if len(edges) != 0 {
			t.Fatalf("order %d: no admitted owner, want no ownership edges, got %d", i, len(edges))
		}
		for _, n := range nodes {
			if n.ID == "env::TERM" && n.FilePath != builderRepoPrefix+"/progress/glyphs.go" {
				t.Fatalf("order %d: env::TERM carries %s, want the record that sorts first (glyphs.go)", i, n.FilePath)
			}
		}
		if i == 0 {
			first = nodes
			continue
		}
		if !reflect.DeepEqual(builderRenderNodes(nodes), builderRenderNodes(first)) {
			t.Fatalf("the listing order changed the contract nodes:\n%v\n%v", builderRenderNodes(first), builderRenderNodes(nodes))
		}
	}
}
