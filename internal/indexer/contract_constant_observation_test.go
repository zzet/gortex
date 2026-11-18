package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func TestColdContractsResolveCrossFileConstantAfterParseSidecars(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "constant.go"), "package fixture\n\nconst Route = \"/api/items\"\n")
	// Larger files dispatch first. With one worker this consumer necessarily
	// attempts lookup before the constant file has been parsed or flushed.
	writeFile(t, filepath.Join(root, "routes.go"), "package fixture\n\nfunc setup(r Router) { r.GET(Route, handler) }\nfunc handler() {}\n")
	idx := newTestIndexer(graph.New())
	idx.config.Workers = 1
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.NotNil(t, idx.ContractRegistry())
	var paths []string
	for _, record := range idx.ContractRegistry().ByRepo("fixture") {
		if record.Type == contracts.ContractHTTP && record.Role == contracts.RoleProvider {
			path, _ := record.Meta["path"].(string)
			paths = append(paths, path)
		}
	}
	require.Contains(t, paths, "/api/items", "the oracle requires an actual const-based route, not two empty outputs")
	idx.contractCacheMu.RLock()
	cached := idx.contractCache["fixture/routes.go"]
	idx.contractCacheMu.RUnlock()
	require.NotNil(t, cached)
	require.NotEmpty(t, cached.contracts, "the early empty cache must be replaced after constants become complete")
}

func TestColdContractRefreshUsesAcceptedSource(t *testing.T) {
	root := t.TempDir()
	// Disk moved after parsing. The refresh must remain paired with the old
	// accepted graph/source, rather than accepting the newer disk route.
	writeFile(t, filepath.Join(root, "routes.go"), "package fixture\nfunc setup(r Router) { r.GET(\"/new\", handler) }\n")
	g := graph.New()
	g.AddBatch([]*graph.Node{{ID: "file", Kind: graph.KindFile, FilePath: "fixture/routes.go", RepoPrefix: "fixture"}}, nil)
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	reg := contracts.NewRegistry()
	accepted := []byte("package fixture\nfunc setup(r Router) { r.GET(\"/accepted\", handler) }\n")
	err := idx.refreshColdConstantContractFiles(context.Background(), reg,
		[]coldConstantContractInput{{path: "fixture/routes.go", language: "go", source: accepted, mtime: 42}},
		map[string][]contracts.Extractor{"go": {&contracts.HTTPExtractor{}}})
	require.NoError(t, err)
	records := reg.ByRepo("fixture")
	require.Len(t, records, 1)
	require.Equal(t, "/accepted", records[0].Meta["path"])
	require.Equal(t, int64(42), idx.contractCache["fixture/routes.go"].mtimeNano)
}

func TestColdContractRefreshRejectsCanceledOrMissingGraph(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	input := []coldConstantContractInput{{path: "fixture/routes.go", language: "go", source: []byte("package fixture")}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reg := contracts.NewRegistry()
	require.ErrorIs(t, idx.refreshColdConstantContractFiles(ctx, reg, input, nil), context.Canceled)
	require.ErrorContains(t, idx.refreshColdConstantContractFiles(context.Background(), reg, input, nil), "lost its graph rows")
	require.Empty(t, reg.ByRepo("fixture"))
	require.Empty(t, idx.contractCache)
}

func TestColdContractConstantObservationIncludesUnresolvedLookups(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	for _, test := range []struct {
		name, argument string
		attempted      bool
	}{
		{"literal", `"/api/items"`, false},
		{"unresolved_constant", "Route", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempted := false
			src := []byte("package fixture\nfunc setup(r Router) { r.GET(" + test.argument + ", handler) }\n")
			idx.runContractExtractorsForFileObserved("fixture/routes.go", src, nil, nil,
				[]contracts.Extractor{&contracts.HTTPExtractor{}}, nil, &attempted)
			require.Equal(t, test.attempted, attempted)
		})
	}
}
