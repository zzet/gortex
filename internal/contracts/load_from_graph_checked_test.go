package contracts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCheckedContractRegistryPreservesOwnersSelectorsAndEmptyNamespace(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				store = openContractLoaderSQLite(t)
			}
			a := ownerLoaderRecord("repo-a", "workspace", "provider", "repo-a/handler.go", "repo-a/handler.go::serve")
			b := ownerLoaderRecord("repo-a", "workspace", "consumer", "repo-a/client.go", "repo-a/client.go::call")
			other := ownerLoaderRecord("repo-b", "other", "provider", "repo-b/handler.go", "repo-b/handler.go::serve")
			other.ID = a.ID
			b.ID = a.ID
			var sources []*graph.Node
			var edges []*graph.Edge
			for _, c := range []contracts.Contract{a, b, other} {
				n, e := ownerLoaderSourceAndEdge(c, true)
				sources = append(sources, n)
				edges = append(edges, e)
			}
			sources = append(sources, ownerLoaderNode(other, "contract_owner_record"))
			store.AddBatch(sources, edges)
			reg, err := contracts.LoadRegistryFromGraphChecked(context.Background(), store, contracts.RegistryLoadOptions{RepoPrefix: "repo-a"})
			require.NoError(t, err)
			require.NotNil(t, reg)
			require.ElementsMatch(t, []contracts.Contract{a, b}, reg.ByID(a.ID))
			reg, err = contracts.LoadRegistryFromGraphChecked(context.Background(), store, contracts.RegistryLoadOptions{RepoPrefix: "repo-a", FilePaths: []string{a.FilePath}})
			require.NoError(t, err)
			require.Equal(t, []contracts.Contract{a}, reg.ByID(a.ID))
			reg, err = contracts.LoadRegistryFromGraphChecked(context.Background(), store, contracts.RegistryLoadOptions{RepoPrefix: "repo-a", FilePaths: []string{a.FilePath}, ExpandFileIDs: true})
			require.NoError(t, err)
			require.ElementsMatch(t, []contracts.Contract{a, b}, reg.ByID(a.ID))
			reg, err = contracts.LoadRegistryFromGraphChecked(context.Background(), store, contracts.RegistryLoadOptions{RepoPrefix: "repo-a", ContractIDs: []string{a.ID}, Limit: 1})
			require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
			require.Nil(t, reg)
			scalar := ownerLoaderRecord("", "standalone", "provider", "legacy.env", "")
			scalar.ID = "env::STANDALONE"
			store.AddNode(ownerLoaderNode(scalar, ""))
			reg, err = contracts.LoadRegistryFromGraphChecked(context.Background(), store, contracts.RegistryLoadOptions{})
			require.NoError(t, err)
			require.NotNil(t, reg)
			require.Equal(t, []contracts.Contract{scalar}, reg.ByID(scalar.ID))
			require.Len(t, reg.AllIDs(), 1)
		})
	}
}

func TestCheckedContractRegistrySelectedRemovalAndOwnerPayload(t *testing.T) {
	base := graph.New()
	a := ownerLoaderRecord("repo", "workspace", "provider", "repo/a.go", "repo/a.go::serve")
	b := ownerLoaderRecord("repo", "workspace", "consumer", "repo/b.go", "repo/b.go::call")
	b.ID = a.ID
	an, ae := ownerLoaderSourceAndEdge(a, true)
	bn, be := ownerLoaderSourceAndEdge(b, true)
	base.AddBatch([]*graph.Node{an, bn, ownerLoaderNode(a, "contract_owner_record")}, []*graph.Edge{ae, be})
	layer := graph.NewOverlayLayer()
	layer.MarkFile(an.FilePath, true)
	selected := graph.NewOverlaidView(base, layer)
	reg, err := contracts.LoadRegistryFromGraphChecked(context.Background(), selected, contracts.RegistryLoadOptions{RepoPrefix: "repo"})
	require.NoError(t, err)
	require.Equal(t, []contracts.Contract{b}, reg.ByID(a.ID))
	primary, err := contracts.LoadRegistryFromGraphChecked(context.Background(), base, contracts.RegistryLoadOptions{RepoPrefix: "repo"})
	require.NoError(t, err)
	require.ElementsMatch(t, []contracts.Contract{a, b}, primary.ByID(a.ID))
	// Changing one owner's response metadata must not inherit last-writer
	// canonical payload into either the selected or surviving sibling owner.
	changed := b
	changed.Meta = map[string]any{"response_type": "repo/shared.go::Changed"}
	newNode, newEdge := ownerLoaderSourceAndEdge(changed, true)
	edit := graph.NewOverlayLayer()
	edit.MarkFile(bn.FilePath, false)
	edit.AddNode(newNode.FilePath, newNode)
	edit.AddEdge(newEdge)
	reg, err = contracts.LoadRegistryFromGraphChecked(context.Background(), graph.NewOverlaidView(base, edit), contracts.RegistryLoadOptions{RepoPrefix: "repo"})
	require.NoError(t, err)
	require.ElementsMatch(t, []contracts.Contract{a, changed}, reg.ByID(a.ID))
}

type failedCheckedRegistryReader struct {
	graph.Reader
	failure error
	cancel  context.CancelFunc
}

func (r failedCheckedRegistryReader) LoadContractRepoProjectionContext(context.Context, string) (graph.ContractFileProjection, error) {
	if r.cancel != nil {
		r.cancel()
	}
	return graph.ContractFileProjection{ScalarNodes: []*graph.Node{{ID: "partial", Kind: graph.KindContract}}}, r.failure
}
func TestCheckedContractRegistryNeverReturnsPartialOrCanceledResult(t *testing.T) {
	for _, failure := range []error{errors.New("read failed"), graph.ErrContractProjectionStale, graph.ErrContractProjectionIncomplete, graph.ErrContractProjectionLimit} {
		reg, err := contracts.LoadRegistryFromGraphChecked(context.Background(), failedCheckedRegistryReader{Reader: graph.New(), failure: failure}, contracts.RegistryLoadOptions{})
		require.ErrorIs(t, err, failure)
		require.Nil(t, reg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reg, err := contracts.LoadRegistryFromGraphChecked(ctx, failedCheckedRegistryReader{Reader: graph.New(), cancel: cancel}, contracts.RegistryLoadOptions{})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, reg)
	reg, err = contracts.LoadRegistryFromGraphChecked(context.Background(), struct{ graph.Reader }{graph.New()}, contracts.RegistryLoadOptions{})
	require.ErrorIs(t, err, graph.ErrContractProjectionUnsupported)
	require.Nil(t, reg)
}

// A sparse physical SQLite generation inherits source/canonical nodes from its
// selected lower layer. The adapter models generation serving without requiring
// graphview catalog publication in this contracts-package test.
type sparseSQLContractLayer struct {
	*graph.OverlayLayer
	handle *store_sqlite.Store
}

func (l *sparseSQLContractLayer) LayerContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	return l.handle.LayerContractRepoProjectionContext(ctx, repo)
}
func (l *sparseSQLContractLayer) LayerContractFileProjectionContext(ctx context.Context, repo string, files []string) (graph.ContractFileProjection, error) {
	return l.handle.LayerContractFileProjectionContext(ctx, repo, files)
}
func (l *sparseSQLContractLayer) LayerContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	return l.handle.LayerContractIDProjectionContext(ctx, ids)
}
func TestCheckedContractRegistryEdgeOnlyPhysicalGeneration(t *testing.T) {
	store := openContractLoaderSQLite(t)
	c := ownerLoaderRecord("repo", "workspace", "provider", "repo/a.go", "repo/a.go::serve")
	source, owner := ownerLoaderSourceAndEdge(c, true)
	store.AddBatch([]*graph.Node{source, ownerLoaderNode(c, "contract_owner_record")}, nil)
	handle := store.AtGeneration(1)
	// Legacy route edges have no durable owner namespace and must resolve their
	// inherited source during composed reads rather than fail in physical scope.
	route := &graph.Edge{From: source.ID, To: c.ID, Kind: graph.EdgeHandlesRoute, FilePath: c.FilePath}
	foreign := &graph.Edge{From: "foreign-missing", To: "foreign-contract", Kind: graph.EdgeProvides, FilePath: "foreign/a.go", Meta: map[string]any{"contract_owner_repo_prefix": "foreign"}}
	handle.AddBatch(nil, []*graph.Edge{owner, route, foreign})
	selected := graph.NewOverlaidViewWithLayer(store, &sparseSQLContractLayer{OverlayLayer: graph.NewOverlayLayer(), handle: handle})
	reg, err := contracts.LoadRegistryFromGraphChecked(context.Background(), selected, contracts.RegistryLoadOptions{RepoPrefix: "repo"})
	require.NoError(t, err)
	require.Equal(t, []contracts.Contract{c}, reg.ByID(c.ID))
	handle.AddEdge(&graph.Edge{From: "selected-missing", To: c.ID, Kind: graph.EdgeConsumes, FilePath: "repo/missing.go", Meta: map[string]any{"contract_owner_repo_prefix": "repo"}})
	reg, err = contracts.LoadRegistryFromGraphChecked(context.Background(), selected, contracts.RegistryLoadOptions{RepoPrefix: "repo"})
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.Nil(t, reg)
}
